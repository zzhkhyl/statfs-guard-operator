# 部署 statfs-guard

面向要把它装到 Kubernetes 集群上的人。

安装形态是「一个 CRD + 一个 DaemonSet + 一个 Deployment」，**没有 webhook、没有对外端口、不改 Ceph 也不改 kubelet 的任何配置**。
运行时的全部写操作只有两类：写 `CephFSStatfsReport` 的 status，和发 Kubernetes Event。

背景与原理见 [`../README.md`](../README.md)；改代码前必读 [`../DEVELOPING.md`](../DEVELOPING.md)。

---

## 1. 前置条件

| 项 | 要求 |
|---|---|
| Kubernetes | 清单是 `apiextensions.k8s.io/v1` + `apps/v1`，1.24+ 都可用 |
| 存储 | 存在 `cephfs.csi.ceph.com` 的 PV，且这些子卷已在节点上被内核 CephFS 客户端挂载 |
| 镜像 | 节点能拉取 `ghcr.io` 上的镜像 |
| 工具 | `kubectl`。不需要 kustomize、不需要 helm |

agent 会读节点的 `/var/lib/kubelet/pods`（**只读** hostPath），所以它必须能落在持有 CephFS 挂载的节点上。清单已带 `tolerations: [{operator: Exists}]`，会调度到包括控制面在内的所有节点。

---

## 2. 拿到镜像

镜像由 GitHub Actions 自动构建并推送到 GHCR：

```text
ghcr.io/zzhkhyl/statfs-guard-operator:latest
```

tag 规则：

| 触发 | 产生的 tag |
|---|---|
| push 到 `main` | `latest`、`main`、`sha-<短 sha>` |
| push tag `v1.2.3` | `1.2.3`、`1.2`、`sha-<短 sha>` |
| Pull Request | 只构建校验，**不推送** |

仓库里两份工作负载清单写的是 `:latest`。想钉版本就改这两处的 `image:`（`config/agent/daemonset.yaml`、`config/manager/deployment.yaml`），然后重生成清单：

```bash
make bundle
```

> `deploy/install.yaml` 与 `deploy/monitoring.yaml` 是 `hack/bundle.py` 生成的，**不要手改**，否则下次 `make bundle` 会覆盖掉。

### 2.1 让集群能拉到镜像

GHCR 的包默认是**私有**的，公开仓库也一样。二选一：

**方案 A：把包改成 public（推荐，最省事）**

GitHub → 右上角头像 → `Your packages` → `statfs-guard-operator` → `Package settings` → `Change visibility` → `Public`。

**方案 B：保持私有，配 imagePullSecret**

```bash
kubectl create namespace statfs-guard --dry-run=client -o yaml | kubectl apply -f -

kubectl -n statfs-guard create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io \
  --docker-username=<你的 GitHub 用户名> \
  --docker-password=<一个有 read:packages 权限的 PAT>
```

然后在 `config/agent/daemonset.yaml` 与 `config/manager/deployment.yaml` 的 `spec.template.spec` 下加：

```yaml
      imagePullSecrets:
        - name: ghcr-pull
```

再 `make bundle` 重生成。

> 顺序提示：`make install` 会先 apply 出 `statfs-guard` namespace，所以 secret 要么先按上面第一条命令建好 namespace 再建，要么在 `make install` 之后补建再 `kubectl -n statfs-guard rollout restart` 两个工作负载。

---

## 3. 安装

```bash
make install
# = make bundle + kubectl apply -f deploy/install.yaml

kubectl -n statfs-guard get pods -o wide
```

装出来的东西：

| 对象 | 作用 |
|---|---|
| CRD `cephfsstatfsreports.storage.statfsguard.io` | 每个 CephFS PV 一个报告对象，短名 `cfsr` |
| Namespace `statfs-guard` | |
| ServiceAccount `statfs-guard-agent` | **没有任何 RBAC 绑定**，agent 不需要 API 权限 |
| SA + ClusterRole + ClusterRoleBinding `statfs-guard-manager` | 读 PV/PVC/Pod、走 `pods/proxy`、写报告与事件 |
| DaemonSet `statfs-guard-agent` | 每节点一个，探测即修复，监听 `:9101` |
| Deployment `statfs-guard-manager` | 集群侧对账、导出指标、发事件，`leader-elect=true` 单活跃 |
| Service `statfs-guard-manager-metrics` | headless，给抓取用 |

---

## 4. 监控（可选，且是独立一步）

`config/prometheus/` 下是 **VictoriaMetrics operator** 的 CRD：`VMPodScrape`（agent）、`VMServiceScrape`（manager）、`VMRule`（五条告警）。

它们**不在** `deploy/install.yaml` 里。原因：集群没装 vmoperator 时，apply 会因为找不到 CRD 而让整份清单失败——包括本来完全独立可用的 CRD/RBAC/工作负载。所以监控被拆成单独一份：

```bash
# 先确认集群里有这些 CRD
kubectl get crd | grep operator.victoriametrics.com

make install-monitoring
```

如果集群里是 **VictoriaMetrics 单机**（`vmsingle`，没有 operator），上面这些 CRD 用不了。两个替代办法：

1. 直接把 DaemonSet 上已经打好的注解当成服务发现来源——agent 的 Pod 模板里已有：
   ```yaml
   prometheus.io/scrape: "true"
   prometheus.io/port: "9101"
   ```
   （manager 侧没有这对注解，它的指标在 `8080`，需要单独配一个静态 job 或加同样的注解。）
2. 在 VM 的 `-promscrape.config` 里加静态 job。

> 注意：本仓库的 VMPodScrape **故意不做** `node` 标签的 relabel。agent 导出的每条 series 本来就自带 `node`（值来自 `os.Hostname()`）；再 relabel 一个 `node` 只会在 hostname 与 node 名不完全一致时多出 `exported_node`。如果你的环境里 hostname ≠ node 名，请先把这一点想清楚再动。

---

## 5. 验证装对了

### 5.1 看报告表

```bash
kubectl get cfsr -o wide
```

期望看到每个 CephFS PV 一行：

```text
NAMESPACE      PVC                NODE     TRUE          REPORTED      DRIFT       PHASE
example-ns     example-pvc        node-1   10737418240   10733158400   4259840     Healed
```

`PHASE` 的含义：

| Phase | 含义 |
|---|---|
| `OK` | 两个数一致，kubelet 读数可信 |
| `Healing` | 抓到失真，**本次读取已顺手修掉**，等 kubelet 下个采集周期（≤60s）跟上 |
| `Healed` | 曾经抓到过失真、现已一致 —— 这个卷的历史读数不可作准 |
| `Drifted` | 抓到失真但 `--auto-heal=false`（只观测不修） |
| `Error` | agent 不可达 / 卷未挂载 / 读取被拒 |
| `Skipped` | 被 `--storage-classes` / `--exclude-namespaces` 排除，或没有被任何 Pod 挂载 |

刚装完时出现大量 `Healing` 是**正常且预期**的：CephFS 上的 statfs 几乎总是偏的（见 README 第二节），第一次探测会把攒了很久的偏差一次性抓出来。

### 5.2 确认 agent 真的看见了卷（最重要的一条自检）

CephFS 的 superblock magic 写错、hostPath 挂错目录，都会让 `Discover()` 返回空，
表现是「报告全绿、其实什么都没测」——**这个失效模式是静默的**。

```bash
POD=$(kubectl -n statfs-guard get pod -l app.kubernetes.io/name=statfs-guard-agent \
        --field-selector spec.nodeName=<某个有 CephFS 卷的节点> -o name | head -1)
kubectl -n statfs-guard get --raw \
  "/api/v1/namespaces/statfs-guard/pods/${POD#pod/}:9101/proxy/v1/volumes"
```

期望返回一个 `PV名 -> 挂载路径` 的 JSON。返回 `{}` 的处理办法：

- 该节点上**真的没有** CephFS 卷 → 正常；
- 该节点上**有** CephFS 卷却是 `{}` → 排查 `IsCephFS()` 的 magic 判定与 hostPath 挂载路径，别放过，此时监控等于没开。

### 5.3 看 manager 指标

```bash
kubectl -n statfs-guard port-forward svc/statfs-guard-manager-metrics 8080:8080 &
curl -s localhost:8080/metrics | grep '^cephfs_guard_'

# 按相位统计全局
curl -s localhost:8080/metrics | grep 'cephfs_guard_volume_phase'
```

### 5.4 端到端确认修复生效

如果集群里有 VictoriaMetrics / Prometheus，查一下 kubelet 侧的读数：

```promql
kubelet_volume_stats_used_bytes{persistentvolumeclaim="<你的 pvc>"}
```

对比 `cephfs_guard_volume_true_used_bytes`。第一次 agent 探测之后，kubelet 需要**下一个采集周期（≤60s）**才反映出来。

### 5.5 排障：Pod 都是 Running，但一条报告都没有

先看 manager 的日志：

```bash
kubectl -n statfs-guard logs deploy/statfs-guard-manager | grep -iE 'lease|forbidden|leader'
```

如果出现

```text
leases.coordination.k8s.io "statfs-guard.storage.statfsguard.io" is forbidden:
User "system:serviceaccount:statfs-guard:statfs-guard-manager" cannot create resource "leases"
```

说明生效的 ClusterRole 里没有 `coordination.k8s.io/leases`。manager 以 `--leader-elect=true` 启动，
拿不到租约就**永远不会 reconcile**，但进程本身是健康的——所以这个问题特别容易被当成「装好了」。

处理：确认 `deploy/install.yaml` 是重新生成过的（`make bundle`），然后

```bash
kubectl apply -f deploy/install.yaml
kubectl -n statfs-guard rollout restart deploy/statfs-guard-manager
```

---

## 6. 权限说明：为什么 agent 是 root

`Dockerfile` 最后是 `USER nonroot`，但 `config/agent/daemonset.yaml` 里 agent 容器显式写了 `runAsUser: 0`。这不是矛盾：

- container 级 `securityContext` **覆盖**镜像里的 `USER`；
- agent 需要 root 才能穿透 kubelet 的卷目录并读 `ceph.dir.*` xattr；
- 它读的 hostPath 是 `readOnly: true`，`capabilities.drop: ["ALL"]`，`readOnlyRootFilesystem: true`，不写任何东西。

manager 走镜像的 `USER nonroot`（65532），满足 `runAsNonRoot: true`。

---

## 7. 卸载

```bash
make uninstall
```

它按顺序做三件事：删监控清单、删主体清单、删 CRD。
**删 CRD 会连带删掉所有 `CephFSStatfsReport` 对象。**

agent 停掉之后，节点的 CephFS 客户端缓存会重新开始变陈旧——也就是 kubelet 的卷用量读数会重新变得不可信。

---

## 8. 调参

参数表见 README 第六节。部署态要改参数，改 `config/manager/deployment.yaml` / `config/agent/daemonset.yaml` 的 `args`，然后：

```bash
make bundle && kubectl apply -f deploy/install.yaml
```

两个最常被问到的：

- `--probe-interval`（agent，默认 `5m`）：每次探测 = 每个卷一次 MDS statx。卷很多时把它调大，别调小。
- `--drift-min-bytes`（manager，默认 `64Mi`）：容差。CephFS 上「有偏差」是常态，容差是为**容量决策**服务的。想更敏感就调小（例如 `1048576`），代价是事件变多。
