# statfs-guard

Kubernetes operator that detects and repairs the CephFS `statfs` staleness that
makes `kubelet_volume_stats_used_bytes` — and therefore every 「卷使用率」面板与「磁盘写满」告警 — silently wrong.

面向问题：**CephFS 卷的「已用容量」读数会长期停在旧值，最高可低报 100%，导致这个卷的写满告警等于不存在。**

---

## 一、它修的是什么（一句话）

`statfs(2)`（也就是 `df` 与 kubelet 采集的数据源）在 CephFS 挂载根设有 quota 时，
直接读客户端本地缓存的 mount-root inode 字段 `ci->i_rbytes`，**整条路径不做任何 MDS 往返**：

```c
/* fs/ceph/quota.c :: ceph_quota_update_statfs()  —— 上游 master 至今如此 */
spin_lock(&ci->i_ceph_lock);
if (ci->i_max_bytes) {
        total = ci->i_max_bytes >> CEPH_BLOCK_SHIFT;
        used  = ci->i_rbytes    >> CEPH_BLOCK_SHIFT;   /* ← 纯缓存读取，无重校验 */
        free  = total > used ? total - used : 0;
}
spin_unlock(&ci->i_ceph_lock);
```

`i_rbytes` 只有两个写入点：收到带 rstat 的 MDS 回复（`ceph_fill_inode()`），
或 MDS 主动推 `CEPH_MSG_CLIENT_QUOTA`（`ceph_handle_quota()`，推不推由 `CInode::check_rstats()` 决定）。
**推送没到，缓存就无限期陈旧。**

而读 `ceph.dir.rbytes` 这个 vxattr 会走 `__ceph_getxattr()` → `CEPH_STAT_RSTAT` →
**强制路由到 auth MDS 并回填 `i_rbytes`**。

## 二、于是「监测」与「修复」是同一个系统调用

这是整个设计的地基：

- 要拿到真值，只能读 `ceph.dir.rbytes`；
- 读了它，顺手就把被污染的那个客户端修好了；
- 修好之后，kubelet 下一个采集周期（≤60s）报的就是对的了。

所以 statfs-guard 不是「发现偏差 → 再做点别的」，
而是**用一个只读的 xattr 读取，同时完成 取真值 / 记录失真 / 纠正缓存**三件事。
没有任何写入：不动配额、不动挂载、不 evict 客户端，**没有可以让数据受损的路径**。

### 现场实测

> 下面这份回显是**示例数据**（名字、路径、数字都做了替换），只用来展示形态；你自己集群上的数值会不同。

`example-ns/example-pvc`（node-1，真值 ≈ 40 GiB）：

```console
$ df -h /var/lib/kubelet/pods/<pod-uid>/volumes/kubernetes.io~csi/<pv-name>/mount
100G   30G   70G   30%    ← 读之前

$ statfs-guard-prober --format text <同一个路径>
/var/lib/kubelet/.../<pv-name>/mount   rbytes=42949672960   rfiles=245559   quota=107374182400

$ df -h <同一个路径>
100G   40G   60G   40%    ← 读之后，同一秒

# kubelet 侧
T+0s    kubelet_volume_stats_used_bytes = 32212254720   (30 GiB)  ← 陈旧的读数
T+60s   kubelet_volume_stats_used_bytes = 42949672960   (40 GiB)  ← 下一个采集周期自动纠正
```

同一时刻在**别的客户端**（Ceph 主机上 `getfattr`、或新建一个同卷临时挂载）读 rbytes，
**刷不动** node-1 —— 缓存是 per-node 的（CephFS 一个节点一个 superblock），
所以修复动作必须落在**持有该挂载的那个节点**上。这决定了本 operator 的形态：
一个 DaemonSet + 一个集群侧 controller，而不是一个中心化的 job。

### 这不是个例：一个节点上的 CephFS 卷，首探几乎全中

把 `agent` 直接跑在一个持有 CephFS 挂载的节点上（`--probe-interval=30m`），第一次 sweep 的形态：

> 下表同样是**示例数据**：PV 名与数字都做过替换。

| PV | 真值 `rbytes` | kubelet 侧 `statfs` | 偏差 | 相对误差 |
|---|---|---|---|---|
| `pvc-0001…` | 5,000,000,000 | 4,960,000,000 | +38 MB | 0.8% |
| `pvc-0002…` | 67,000,000 | 29,000,000 | +38 MB | **56%** |
| `pvc-0003…` | 40,000,000,000 | 39,995,000,000 | +5 MB | 0.01% |
| `pvc-0004…` | 142,000,000 | 138,000,000 | +4 MB | 2.8% |
| `pvc-0005…` | 600,000 | **0** | +600 KB | **100%** |
| `pvc-0006…` | 12,000 | **0** | +12 KB | **100%** |
| `pvc-0007…` | 12,000 | **0** | +12 KB | **100%** |
| `pvc-0008…` | 58,000 | 4,200,000 | **−4.1 MB** | 反向（缓存偏高） |
| `pvc-0009…` | 0 | 0 | 0 | 空卷，正常 |

三个结论，都影响设计：

1. **CephFS 上的 statfs 基本总是偏的**，只是幅度取决于「距 MDS 上次推送过了多久」。
   所以「偏差」不是一个非 0 即故障的布尔量，容差是必须的（默认 `max(64MiB, 5%)`），
   否则每个周期都会报一遍。
2. **偏差可以双向**（`pvc-0008…` 是 statfs 比真值**高** 4 MB）—— 所以判定用 `|drift|`。
3. 真出事的那种（有卷被连续报 0 达数月、有卷低报数 GB）幅度是 GB 级，
   64 MiB 容差足以抓住，而小卷的几百 KB 误差对容量决策没有意义。

## 三、架构

```
                      ┌──────────────────────────────────────────┐
                      │  manager (Deployment, leader-elected)    │
                      │  · 每个 CephFS PV 一个 CephFSStatfsReport │
                      │  · 对账 真值 vs kubelet 读数              │
                      │  · 导出 cephfs_guard_* 指标 + 事件        │
                      └───────────────┬──────────────────────────┘
                                      │ pods/proxy  (无需 Pod 网络可达)
                     ┌────────────────┴─────────────────┐
                     ▼                                  ▼
      ┌──────────────────────────┐        ┌──────────────────────────┐
      │ agent (DaemonSet) node-1 │        │ agent (DaemonSet) node-2 │
      │ hostPath: kubelet/pods RO│        │  ...                     │
      │ 每 5min 每个卷:           │        │                          │
      │  ① statfs(2) → 报数       │        │                          │
      │  ② rbytes   → 真值 + 修复 │        │                          │
      │ :9101 /metrics /v1/probe │        │                          │
      └──────────────────────────┘        └──────────────────────────┘
                     │                                  │
                     └──────────► Prometheus ◄──────────┘
                         cephfs_node_* / cephfs_guard_*
```

**① 必须先 `statfs` 再读 `rbytes`** —— 顺序是承重的。读 rbytes 会顺手刷新缓存，
先测 statfs 才测得出偏差；反过来永远读到已经修好的值，检测形同虚设。

**② 卷的归属节点从 kubelet 自己的目录布局得出**，agent 不需要任何 API 权限：
`/var/lib/kubelet/pods/<podUID>/volumes/kubernetes.io~csi/<pvName>/mount`。

**③ 多节点同时挂载一个卷时逐个探测，报最差的那个客户端** —— 因为缓存是 per-node 的。

## 四、三个二进制

| 二进制 | 角色 | 说明 |
|---|---|---|
| `manager` | 集群侧 controller | CRD 状态机、指标、事件、告警 |
| `agent` | 每节点 DaemonSet | 发现卷、取真值、修缓存、暴露指标 |
| `prober` | 一次性探针 | 同一套原语，`kubectl debug` 里手验用 |

```console
$ statfs-guard-prober --format text /var/lib/kubelet/pods/*/volumes/kubernetes.io~csi/*/mount
/var/lib/kubelet/.../<pv-name>/mount   rbytes=42949672960   rfiles=245559   quota=107374182400
statfs-guard-prober: 1/1 path(s) read
```

## 五、装

镜像由 CI 自动构建推送到 GHCR（`ghcr.io/zzhkhyl/statfs-guard-operator`），正常不需要自己 build：

```bash
make install                         # = make bundle + kubectl apply -f deploy/install.yaml
kubectl -n statfs-guard get pods -o wide
kubectl get cfsr -o wide
```

监控清单（VictoriaMetrics operator 的 CRD）是**可选的独立一步**，因为集群没装 vmoperator 时它会整份 apply 失败：

```bash
make install-monitoring
```

想自己 build 镜像：

```bash
make image push                      # REGISTRY / IMAGE 可用环境变量覆盖
# 改了 config/agent/daemonset.yaml 或 config/manager/deployment.yaml 的 image 之后，记得 make bundle
```

卸载：`make uninstall`。

> **完整的部署说明**（含私有 GHCR 包的拉取配置、逐项验证步骤、agent 为什么需要 root、参数怎么改）见 **[`docs/DEPLOY.md`](docs/DEPLOY.md)**。

### 观测

```console
$ kubectl get cfsr -o wide
NAME     NAMESPACE    PVC    NODE     TRUE          REPORTED      DRIFT      PHASE
pvc-0001 example-ns   data   node-1   42949672960   42945413120   4259840    Healed
pvc-0002 example-ns   logs   node-2   10737418240   10737418240   0          OK
```

`PHASE` 取值：

| Phase | 含义 |
|---|---|
| `OK` | 两个数一致，kubelet 读数可信 |
| `Healing` | 抓到失真，**本次读取已顺手修掉了**，等 kubelet 下个周期跟上（60s 后复查） |
| `Healed` | 曾经抓到过失真、现已一致 —— 这个卷的历史读数不可作准 |
| `Drifted` | 抓到失真但 `--auto-heal=false`（只观测不修） |
| `Error` | agent 不可达 / 卷未挂载 / 读取被拒 |
| `Skipped` | 被 `--storage-classes` / `--exclude-namespaces` 排除，或未被任何 Pod 挂载 |

## 六、参数

`manager`

| 参数 | 默认 | 说明 |
|---|---|---|
| `--drift-min-bytes` | `67108864` (64MiB) | 绝对容差 |
| `--drift-min-ratio` | `0.05` | 相对容差（取两者较大值） |
| `--auto-heal` | `true` | 是否自动修；关掉则只观测（注意：读真值本身就会修） |
| `--resync-interval` | `5m` | 正常卷复查周期 |
| `--verify-interval` | `60s` | 有开放 episode 的卷复查周期 |
| `--storage-classes` | 空=全部 | 只处理这些 StorageClass |
| `--exclude-namespaces` | 空 | 跳过这些 namespace |
| `--agent-namespace` / `--agent-selector` / `--agent-port` | `statfs-guard` / `app.kubernetes.io/name=statfs-guard-agent` / `9101` | agent 定位 |
| `--leader-elect` | `true` | 多副本时只一个活跃 |

`agent`

| 参数 | 默认 | 说明 |
|---|---|---|
| `--probe-interval` | `5m` | 每卷探测周期。**每次探测 = 一次 MDS statx**，不要调太激进 |
| `--probe-timeout` | `30s` | 单卷超时 |
| `--pods-dir` | `/var/lib/kubelet/pods` | hostPath 挂载点 |
| `--listen` | `:9101` | HTTP API |

## 七、指标

agent（`cephfs_node_*`，标签 `pv,node`）：

| 指标 | 说明 |
|---|---|
| `cephfs_node_volume_true_used_bytes` | `ceph.dir.rbytes`，真值 |
| `cephfs_node_volume_true_files` | `ceph.dir.rfiles` |
| `cephfs_node_volume_quota_bytes` | `ceph.quota.max_bytes` |
| `cephfs_node_volume_statfs_used_bytes` | 本地 `statfs(2)` 读数（kubelet 同源） |
| `cephfs_node_volume_drift_bytes` | 同一节点同一时刻两者的差，隔离出「缓存陈旧」本身 |
| `cephfs_node_drift_episodes_total` | 抓到的失真 episode 次数 |
| `cephfs_node_reported_stale_seconds` | 本次失真已持续多久 |
| `cephfs_node_revalidate_total{pv,node,result}` | 刷新次数（每次成功探测 = 1 次刷新） |
| `cephfs_node_probe_duration_seconds` | 探测延迟（MDS 往返） |
| `cephfs_node_mounts` | 本节点可见 CephFS 卷数 |

manager（`cephfs_guard_*`，标签含 `pvc_namespace,pvc,storageclass`）：

| 指标 | 说明 |
|---|---|
| `cephfs_guard_volume_true_used_bytes` | **告警应该用这个** |
| `cephfs_guard_volume_reported_used_bytes` | kubelet 报的（不可信那个） |
| `cephfs_guard_volume_drift_bytes` / `_ratio` | 偏差 |
| `cephfs_guard_volume_quota_bytes` / `_used_ratio` | 真实充满度 |
| `cephfs_guard_volume_phase{pv,...,phase}` | 状态机当前相位（切换时旧相位会被置 0） |
| `cephfs_guard_volume_stale_seconds` | 当前开放 episode 已持续多久 |
| `cephfs_guard_heal_total` | 修复次数（`issued` / `converged`） |
| `cephfs_guard_agent_up` | agent 是否应答 |

按相位统计全局：

```promql
count by (phase) (cephfs_guard_volume_phase == 1)
```

## 八、告警

`config/prometheus/prometheusrule.yaml` 里五条，核心两条。
（这份清单是 **VMRule**，即 VictoriaMetrics operator 形式，由 `make install-monitoring` 安装，
不在主体 `deploy/install.yaml` 里，见 [`docs/DEPLOY.md`](docs/DEPLOY.md) 第四节。）

```promql
# 写满告警。用真值，不用 kubelet —— 这条规则以前根本没法写。
cephfs_guard_volume_used_ratio > 0.85

# kubelet 在低报。operator 已经修了；持续不收敛说明刷新没生效，要人工看。
cephfs_guard_volume_drift_bytes != 0
```

其余：`CephFSStatfsDriftPersists`（修了不收敛）、`CephFSStatfsGuardTruthUnavailable`（真值取不到）、
`CephFSStatfsGuardAgentDown`（agent 挂了，读数会重新变错）。

## 九、边界与不做的事

**它修的是「读数」，不是内核。** 底层是内核 CephFS 客户端 statfs 无重校验 + MDS 推送不可靠的组合缺陷
（上游同类 tracker：#78671 未修；libcephfs 同类修复 #72355 只进了 squid 19.2.4，reef 无 backport）。
升级任何组件都修不了这个读数问题，所以本 operator **不去改 Ceph/K8s 的行为**，
而是把「取真值」这件事变成一个持续运行的、自愈的动作。

**明确不做：**

- 不写数据、不改配额、不 remount、不用 `ceph tell mds ... client evict`（内核被 evict 会 EIO，可能打断业务）。
- 不替代 `kubelet_volume_stats_used_bytes` 的存在，但**明确告诉监控别信它**。
- 不处理非 CephFS 卷（只认 `cephfs.csi.ceph.com`）。

**已知限制：**

1. 每个卷每次探测 = 一次 MDS statx。几十个 PV、5 分钟周期可以忽略；卷多时请调大 `--probe-interval`。
2. agent 需要 `runAsUser: 0` 才能穿透 kubelet 目录并读 xattr；它只读、且 hostPath 是 `readOnly: true`。
3. `cephfs_guard_volume_stale_seconds` 在 auto-heal 打开时**恒为 ~0**（因为每次探测顺手就修好了）——
   这是正确行为，不是没数据。要判断「监控到底盲了多久」，看 `cephfs_guard_heal_total` 与
   `cephfs_node_drift_episodes_total`；要判断**当前是否有人看不住这个卷**，看 `_probe_success` 与
   `cephfs_guard_agent_up`。
4. 「episode 计数」存在 agent 进程内存里，agent 重启后从 0 重算。
5. 若某卷从未被任何 Pod 挂载（没有 kubelet 读数可对账），会被标成 `Skipped`。
6. 默认容差是 `max(64MiB, 5%)`，刻意偏保守：CephFS 上「有偏差」是常态，
   容差是为「会不会误导容量决策」服务的，不是为「有没有偏差」服务的。
   想更敏感就调小 `--drift-min-bytes`（例如 `1048576`），代价是事件变多。

## 十、目录

```
cmd/manager        集群侧 controller
cmd/agent          每节点 DaemonSet
cmd/prober         一次性探针
internal/cephfs    statfs(2) + ceph.dir.* rstat 的原语（含非 Linux stub）
internal/agent     卷发现、探测、episode 记账、HTTP API
internal/controller  CephFSStatfsReport 状态机
internal/kube      PV/PVC/Pod → 节点解析、agent 客户端
internal/metrics   两组 Prometheus 指标
api/v1alpha1       CRD 类型（含手写 deepcopy）
config/            CRD / RBAC / DaemonSet / Deployment / VMPodScrape / VMServiceScrape / VMRule
deploy/            生成物：install.yaml（主体）+ monitoring.yaml（可选监控）
hack/bundle.py     把 config/ 合成上面两份（不手抄）
docs/DEPLOY.md     部署说明
.github/workflows  CI：验收 + GHCR 镜像构建
```

## 十一、开发

**接手继续开发请读 [`DEVELOPING.md`](DEVELOPING.md)** —— 含代码地图、
「改 A 必须同步改 B」对照表、承重不变量（改反会静默失效的那几条）、
验收清单、真机验证套路、本机工具链陷阱，以及已知不一致与待办。

```bash
make verify         # CI 跑的就是这个：gofmt / vet / 双平台编译 / 单测 / 清单一致性
make test
make build          # dist/{manager,agent,prober} (linux/amd64)
```

CI 在 [`.github/workflows/ci.yaml`](.github/workflows/ci.yaml)：`verify` 任务做上面那些；
`image` 任务用 buildx 构建 linux/amd64 镜像，`main` 推送与 `v*` tag 推送时发布到
`ghcr.io/zzhkhyl/statfs-guard-operator`，Pull Request 只构建不推送。

> 依赖版本锁在 k8s.io/{api,client-go,apimachinery} v0.33.x + controller-runtime v0.21（要求 go ≥ 1.24）。
> `Makefile` 把 `GOPROXY` / `GOSUMDB` / `GOTOOLCHAIN` 都做成可覆盖变量（`?=`），
> 所以公网环境与需要走镜像源的环境都不必改文件。

本项目以 **MIT** 许可发布，见 [`LICENSE`](LICENSE)。
