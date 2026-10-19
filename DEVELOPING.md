# statfs-guard 开发说明

给接手继续开发的人（或 AI 编码助手）看的工作手册。
`README.md` 讲「这是什么、为什么」；本文件讲「怎么改、改哪里会炸、怎么验」。

> **先读这 30 秒**：这个项目的正确性不靠测试保证（目前没有测试），靠三条承重不变量。
> 其中第一条一旦被改反，**不会报错，会静默失效**——监控看起来全绿，实际什么都没在测。
> 改 `internal/agent/agent.go` 或 `internal/cephfs/` 之前，务必先看第 3 节。

---

## 1. 环境与构建

### 1.1 工具链

| 项 | 要求 | 实测 |
|---|---|---|
| Go | `go.mod` 声明 `go 1.24.0`；controller-runtime v0.21 要求 ≥ 1.24 | 1.25 / 1.26 都验过 |
| Python | 仅 `hack/bundle.py` 用，标准库即可，不需要 pyyaml | 3.x 均可 |
| kubectl | `make install` / `make reports` 用 | 任意能访问集群的机器 |

依赖版本已锁（不要随意升）：

```
k8s.io/api            v0.33.13
k8s.io/apimachinery   v0.33.13
k8s.io/client-go      v0.33.13
sigs.k8s.io/controller-runtime  v0.21.0
github.com/prometheus/client_golang v1.22.0
github.com/go-logr/logr v1.4.3
module: github.com/zzhkhyl/statfs-guard-operator
```

**不要升到 k8s.io/apimachinery v0.37 之类**：会要求 go ≥ 1.26，本机工具链跟不上，
且 controller-runtime 需同步升级，属于无收益的风险。

### 1.2 两个必须显式设的环境变量

有些内网环境会把默认 `GOPROXY` 指向一个不可用的私有源（返回 502）。
遇到这种情况时，所有 go 命令都要覆盖：

```bash
export GOPROXY=https://goproxy.cn,direct GOSUMDB=off GOTOOLCHAIN=local
```

- `GOTOOLCHAIN=local`：阻止 Go 为了满足 `go` 指令去自动下载新工具链（会失败并卡很久）。
- 这两个变量已经写进 `Makefile` 的 `GOFLAGS_PIN`，所以 `make *` 系列不需要额外设；
  直接用 `go` 的场合才需要。

### 1.3 常用命令

```bash
make verify             # CI 跑的全套：gofmt / vet / 双平台 build / test / bundle 一致性
make tidy vet build     # 依赖 + 静态检查 + 交叉编译出 dist/
make build              # dist/{manager,agent,prober}，linux/amd64
make bundle             # config/**.yaml -> deploy/install.yaml + deploy/monitoring.yaml
make install            # = bundle + kubectl apply -f deploy/install.yaml
make install-monitoring # 只装监控（VMPodScrape/VMServiceScrape/VMRule，需 vmoperator）
make uninstall          # 卸载（含 CRD）
make reports            # kubectl get cephfsstatfsreports -o wide
```

**Windows 本机注意**：`make bundle` 调的是 `python3`。本机若没有这个命令名，
用自己 Python 解释器的路径直接跑：

```bash
python hack/bundle.py     # 或 <你的 python.exe 绝对路径> hack/bundle.py
```

编译产物是 `linux/amd64` 静态二进制（`CGO_ENABLED=0`），可以直接 `scp` 到节点上跑。

---

## 2. 代码地图

14 个 Go 文件，约 2350 行。按「改动会牵动什么」排列：

### 2.1 技术地基（改这里风险最高）

| 文件 | 内容 | 注意 |
|---|---|---|
| `internal/cephfs/rstat_linux.go` | **真原语**。`GetRStat()` 读 `ceph.dir.*` vxattr、`Statfs()` 调 statfs(2)、`IsCephFS()` 判 magic | 整个项目的技术前提都在这里。任何改动都要在真机 A/B 验证 |
| `internal/cephfs/types.go` | `RStat` / `Usage` 结构、`NoQuota = -1` | 纯类型 |
| `internal/cephfs/rstat_other.go` | 非 Linux 的 stub（`//go:build !linux`） | 存在的唯一理由：让开发机（Windows）也能 `go build ./...`。改 linux 侧签名要同步改它 |

### 2.2 agent 侧（DaemonSet）

| 文件 | 关键函数 | 说明 |
|---|---|---|
| `internal/agent/agent.go` | `Discover()` | 用 `filepath.Glob(<pods-dir>/*/volumes/kubernetes.io~csi/*/mount)` 扫卷，PV 名 = 路径倒数第二段。**不需要 API 权限** |
| | `Probe()` | 一次探测 = 一次修复。**顺序是承重的**，见 3.1 |
| | `observe()` | episode 记账 + 写指标。改名/改语义要同步 `sweep()` 的清理逻辑 |
| | `sweep()` | 每周期全量发现 + 探测 + **清理消失的卷的 series** |
| | `Handler()` | HTTP API：`/healthz` `/metrics` `/v1/probe[?pv=]` `/v1/volumes` |
| `cmd/agent/main.go` | flag 解析 | 改 flag → 同步 `config/agent/daemonset.yaml` 的 args + README 参数表 |

### 2.3 manager 侧（Deployment）

| 文件 | 关键函数 | 说明 |
|---|---|---|
| `internal/controller/report_controller.go` | `Reconcile()` | 按 PV 名 key，一个 PV 一个 `CephFSStatfsReport` |
| | `inspect()` | 收集真值 + kubelet 读数 → 定相位。**相位判据见 3.3** |
| | `probeNodes()` | 多节点逐个探测，取 `|drift|` 最大的那个 |
| | `applyStatus()` | 折叠状态 + 导出指标 + 相位切换时发事件 |
| | `announce()` | 事件发射 + **冷却节流** |
| | `exportMetrics()` | **相位标签归零在这里**，见 3.4 |
| | `forgetMetrics()` | 删卷时清 series |
| `internal/kube/agentclient.go` | `AgentPodOn()` / `Probe()` / `Volumes()` | 走 **API server pod proxy**（`pods/<name>:9101/proxy/...`），不依赖 Pod 网络 |
| | `IsCephFSVolume()` / `SubvolumePath()` | 认 `cephfs.csi.ceph.com`；subvolumePath 从 CSI 属性取 |
| `internal/metrics/metrics.go` | — | 两组指标声明。**改指标名 = 三处同步** |
| `api/v1alpha1/statfsreport_types.go` | — | CRD 类型 + Phase 常量 + printcolumn 注解 |

### 2.4 清单与脚本

| 文件 | 说明 |
|---|---|
| `config/crd/*.yaml` | CRD（**手写**，不是 controller-gen 生成） |
| `config/rbac/rbac.yaml` | Namespace + 两个 SA + ClusterRole（agent **零权限**） |
| `config/agent/daemonset.yaml` | DaemonSet，hostPath 只读，`runAsUser: 0` |
| `config/manager/deployment.yaml` | Deployment + metrics Service |
| `config/prometheus/*.yaml` | VMPodScrape / VMServiceScrape / VMRule（**VictoriaMetrics operator 形式**，见 7.1） |
| `hack/bundle.py` | 合成**两份**产物：`deploy/install.yaml`（crd→rbac→agent→manager）与 `deploy/monitoring.yaml`（prometheus） |
| `Dockerfile` | 多阶段；三个二进制打进同一个镜像，靠 `command:` 选入口。镜像里的 `USER nonroot` 只对 manager 生效，见 7.1 |
| `Makefile` | 见 1.3。`REGISTRY`/`IMAGE`/`GOPROXY`/`GOSUMDB`/`GOTOOLCHAIN` 都是 `?=` 可覆盖 |
| `docs/DEPLOY.md` | 面向部署者的安装/验证/权限说明 |
| `.github/workflows/ci.yaml` | CI：`verify` 任务 + `image` 任务（推 GHCR，PR 只构建不推） |

### 2.5 「改 A 必须同步改 B」对照表

这张表是接手时最省时间的东西：

| 你改了 | 必须同步 |
|---|---|
| `internal/metrics/*.go` 的指标名 | ① `README.md` 指标表 ② `config/prometheus/prometheusrule.yaml` 的 expr ③ 上游 Grafana 面板 |
| 任何 `cmd/*/main.go` 的 flag | ① 对应 `config/*/*.yaml` 的 args ② `README.md` 参数表 |
| `api/v1alpha1/*_types.go` 的**字段** | ① `zz_generated.deepcopy.go`（**手写，不会自动生成**）② `config/crd/*.yaml` 的 schema |
| `api/v1alpha1/*_types.go` 的 Phase 常量 | ① `forgetMetrics()` 里的 phase 列表 ② README 的 Phase 表 |
| `config/**` 任意清单 | 跑 `make bundle` 重生成 `deploy/install.yaml` **与** `deploy/monitoring.yaml`（不手抄） |
| `go.mod` 的 module 路径 | 所有 import 前缀（6 个 Go 文件：`internal/{agent,controller,kube}`、`cmd/{agent,manager,prober}`） |
| `api/v1alpha1` 的 `GroupName` | `config/crd/*.yaml` 的文件名 + `metadata.name` + `spec.group`、`config/rbac/rbac.yaml` 的 `apiGroups`、`cmd/manager/main.go` 的 `LeaderElectionID`、`Makefile` 的 uninstall |
| `config/agent/daemonset.yaml` 或 `config/manager/deployment.yaml` 的 `image` | CI 推的 tag 策略（见 `.github/workflows/ci.yaml`）与 `docs/DEPLOY.md` 第二节 |
| `config/rbac/rbac.yaml` 的 `rules` | 整份 RBAC 被内嵌进 `deploy/install.yaml`，改完必须 `make bundle`。其中 `coordination.k8s.io/leases` 是选主的必需项（审查-1），不要顺手删掉 |
| `internal/cephfs/` 的函数签名 | `internal/cephfs/rstat_other.go` 的 stub |

---

## 3. 承重不变量（改代码前必读）

### 3.1 ⚠️ statfs(2) 必须先于 rbytes 读

位置：`internal/agent/agent.go` → `Probe()` 内的 goroutine：

```go
usage, usErr := cephfs.Statfs(path)   // ① 先取 kubelet 同源的读数
stat := cephfs.GetRStat(path)          // ② 再读真值——这一读顺手把缓存修了
```

**为什么承重**：读 `ceph.dir.rbytes` 会强制走 MDS 往返并回填 `i_rbytes`，即读它就会修好它。
如果顺序反过来，`Statfs()` 读到的永远是已经被修好的值，`Drift` 恒为 0。

**失效模式：静默。** 不会编译错、不会 panic，只会「所有卷都健康」——
监控看起来完美，实际检测能力归零。这是本项目最危险的改动。

**顺带**：`Discover()` 里的 `IsCephFS()` 内部也调了一次 `statfs(2)`，但那不触发 MDS 往返
（statfs 从不往返），所以它在 `Probe()` 之前调用**不破坏**检测语义。不要因为这个就以为顺序无所谓。

### 3.2 一次成功探测 = 一次修复

读真值和修缓存是同一个系统调用，所以 `Drift != 0` 记录的是**刚刚被抓住的一次谎报**，
不是一个持续存在的故障。三条推论：

- **auto-heal 下「有偏差」是常态，不是事件** → 事件必须节流（`EventCooldown`，默认 30m）。
  见 `announce()`。
- `cephfs_guard_volume_stale_seconds` 在 auto-heal 打开时**恒为 ~0**，这是正确行为不是没数据。
- 要判断「监控盲了多久」，看 `cephfs_guard_heal_total` / `cephfs_node_drift_episodes_total`。

### 3.3 「活跃 episode」判据 = `|drift| > tolerance`，不是 `StaleSeconds > 0`

位置：`report_controller.go` → `inspect()`：

```go
liveEpisode := abs64(s.Drift) > tolerance
```

**为什么不能写 `s.StaleSeconds > 0`**：episode 的第一次观测时，
`episodeStart[pv]` 刚刚被写入，`StaleSeconds` 恰好是 0（`time.Since(now)`），
于是首探会被误判成「已收敛」。这个 bug 在设计评审阶段被现场实测抓出来过。

### 3.4 相位型 info 指标必须显式置 0

位置：`exportMetrics()` 末尾 + `forgetMetrics()`。

`cephfs_guard_volume_phase{pv,...,phase}` 是「哪个相位 = 1」的 info 型指标。
相位切换时如果把新相位设 1 而不管旧的，**旧相位的 series 会永远停在 1**，
`count by (phase)(... == 1)` 就会把两个相位都算上。
所以切换时（或换节点时）要先把 `previous.Phase` 那个 label 组合显式 `Set(0)`；
卷被删除时要把全部相位（含节点标签）`Delete` 掉。

### 3.5 缓存是 per-node 的

CephFS 一个节点一个 superblock，缓存不会跨节点共享。
由此三个形态结论（不要试图「优化」掉）：

1. 修复动作必须落在**持有该挂载的那个节点**上 → 所以 agent 是 DaemonSet 而不是中心化 Job。
   从别的客户端（Ceph 主机 `getfattr`、新建临时挂载）读 rbytes，**刷不动**目标节点。
2. 一个卷可能同时挂在多节点上，各节点偏差可以不同 → 逐节点探测，报**最差**的那个。
   见 `probeNodes()`。
3. 不能用 `ceph tell mds ... client evict` 代替（内核被 evict 会 EIO，可能打断业务）。

### 3.6 agent 零 API 权限，且 `/v1/probe?pv=` 必须校验 PV 名

agent 的 SA 没有任何 RBAC 绑定。它靠 kubelet 自己的目录布局推导卷归属：

```
/var/lib/kubelet/pods/<podUID>/volumes/kubernetes.io~csi/<pvName>/mount
```

`/v1/probe?pv=<pv>` 会先用 `pv` 去 `a.paths`（发现结果）里查路径，查不到返回 404，
**绝不接受任意路径参数**。这是刻意的：否则这个端点就是一个「以 root 读节点上任意文件」的原语。
新增 HTTP 端点时，保持「输入是 PV 名，不是路径」这条线。

### 3.7 `IsCephFS()` 的 magic 是 0x00c36400

```go
return uint32(st.Type) == 0x00c36400   // CEPH_SUPER_MAGIC
```

写错的后果同样是**静默**：所有卷被跳过 → `Discover()` 返回空 → 报告为空 → 看起来「没有异常」。
所以有一条低成本的健康检查：在有 CephFS 卷的节点上，`/v1/volumes` 不应为空
（`cephfs_node_mounts` 同理）。建议把它做成启动后的自检日志或告警。

---

## 4. 改完代码的验收清单（DoD）

改完必须全部过一遍。先跑一次 `make verify` 就能覆盖 1–5（CI 跑的就是它）；
6 之后要看改动范围。

```bash
cd statfs-guard-operator
export GOPROXY=https://goproxy.cn,direct GOSUMDB=off GOTOOLCHAIN=local

# 1. 格式
gofmt -l .                          # 必须无输出

# 2. 静态检查（注意要带 GOOS，否则 linux-only 文件不参与）
GOOS=linux GOARCH=amd64 go vet ./...     # 必须 0

# 3. 编译（两个平台都要过：linux 是目标，windows 是开发机自检 stub）
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...

# 4. 单测
go test ./... -count=1

# 5. 清单一致（重生成两份产物）
python3 hack/bundle.py
```

6. **改了 `config/**`** → 确认 `deploy/install.yaml` **与** `deploy/monitoring.yaml` 都已重生成，
   且每个文档都有 `apiVersion` + `kind`。CI 用 `python3 hack/bundle.py && git diff --exit-code -- deploy/`
   卡住「改了 config 忘了重生成」。
7. **改了 CRD 字段** → 手工同步 `zz_generated.deepcopy.go` 与 `config/crd/*.yaml`。
   （没有引入 controller-gen；如果打算引入，见 7.2）
8. **改了指标名/标签** → 同步 README 表 + `config/prometheus/prometheusrule.yaml`。
9. **动了 `internal/cephfs/` 或 `internal/agent/`** → 必须做第 5 节的真机验证，不能只靠编译过。
10. **改了 `config/prometheus/**`** → 这些是 VictoriaMetrics operator 的 CRD，CI 只校验它们被正确打包，
    **不校验** CRD schema 是否合法。要验证得在装了 vmoperator 的集群上跑
    `kubectl apply --dry-run=server -f deploy/monitoring.yaml`。

---

## 5. 现场验证手册

编译通过 ≠ 能用。这个项目的正确性只有真机能证。以下是已验证有效的套路。

### 5.1 前置

需要一台**持有 CephFS 挂载的节点**（`kubectl get pod -o wide` 找一个上面跑着 CephFS 卷 Pod 的节点即可）。
`prober` 和 `agent` 都是静态二进制，`sftp` 上传后直接跑，不需要容器运行时。

### 5.2 用 prober 做 A/B（验证「读即修复」）

```bash
P=/var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~csi/<pv>/mount
df -h "$P"                              # 读之前，应该是旧值
/tmp/statfs-guard-prober --format text "$P"   # 这一读就是修复动作
df -h "$P"                              # 同一秒，应该已经变准
```

**成功的标志**：两次 `df` 数值不同。若相同，说明该卷当前本来就是准的（正常），
换一个偏差大的卷再试。

### 5.3 用 agent 做全节点普查

```bash
nohup /tmp/statfs-guard-agent \
  --pods-dir=/var/lib/kubelet/pods \
  --probe-interval=30m \
  --listen=127.0.0.1:9101 \
  --log-level=info > /tmp/agent.log 2>&1 &

sleep 6
curl -s http://127.0.0.1:9101/v1/volumes   # 本节点能看到的 PV -> 路径
curl -s http://127.0.0.1:9101/v1/probe     # 所有样本（缓存）
curl -s http://127.0.0.1:9101/metrics | grep '^cephfs_node_'

pkill -f statfs-guard-agent                # 验完就停
```

**判读**：现场首探时，一个节点上的 CephFS 卷**几乎全部**报出偏差是预期结果
（详见 README 第二节的示例表）。CephFS 上 statfs「总是有点偏」，
幅度 ≈ 距 MDS 上次推送期间的写入量。所以：

- 偏差小（MB 级）= 正常，不代表有故障；
- 偏差大（GB 级，或相对误差接近 100%）= 就是本项目要抓的那种；
- 偏差可以为负（statfs 报得比真值高），所以判据用 `|drift|`。

### 5.4 验证 kubelet 侧是否跟上

读真值之后，kubelet 需要**下一个采集周期（≤60s）**才反映出来：

```bash
curl -s 'http://<你的 VictoriaMetrics / Prometheus 地址>/api/v1/query?query=kubelet_volume_stats_used_bytes{namespace="<ns>",persistentvolumeclaim="<pvc>"}'
```

隔一分钟再查一次，看数值是否跳到真值附近。这就是「修复生效」的端到端证据。

---

## 6. 本机工具链陷阱

这些是在这台机器上实际踩过的，写下来省得重踩。

| 现象 | 原因 | 对策 |
|---|---|---|
| `go build ./... \| head` 被 SIGTERM | 管道下游提前关闭 | 重定向到 `build.log` 再 `cat` |
| `cmd \| tail` 报 command not found | 该环境的 Bash 管道接 coreutils 有问题 | 只用 `&&` 串联，判成败看**产物内容** |
| PowerShell 命令正常但无 stdout 回显 | 该工具在此环境不回显 | 写文件，再用 Read 读 |
| Netcatty 安全策略拒绝执行 | 命令里含 `$(` 或反引号 | 改用 `python3 -c`，或拆成两步 |
| 多条 SSH 命令想并行 | Netcatty 命令是串行的 | 顺序发；长驻进程用 `nohup ... &` 后再另起一条命令查 |
| `go mod tidy` 卡很久后失败 | 默认 GOPROXY 502 / GOTOOLCHAIN 自动下载 | 见 1.2 |
| 节点上没有 `getfattr` / `python3` | 精简镜像 | 用本项目自带的静态 Go 二进制（`syscall.Getxattr`，仅标准库） |

---

## 7. 已知不一致与待办

上一轮清理解决了一批，剩下的是真正还需要人做决定的。

### 7.1 已解决

| 原编号 | 原事项 | 处理方式 |
|---|---|---|
| 7.1 | `Dockerfile` 的 `USER nonroot` 与 DaemonSet 的 `runAsUser: 0` 读起来矛盾 | 行为本来就是对的（container 级 `securityContext` 覆盖镜像 `USER`）。已在 `Dockerfile` 补齐注释，说明 agent 由 DaemonSet 覆盖为 root 及其理由 |
| 7.2 | 监控清单是 `monitoring.coreos.com` 的 CRD，而 VictoriaMetrics 默认不认 | 已 1:1 换成 `VMPodScrape` / `VMServiceScrape` / `VMRule`，并拆到独立产物 `deploy/monitoring.yaml`（`make install-monitoring`），未装 vmoperator 的集群不会再被整份 apply 拖垮 |
| 7.3 | PodMonitor 的 relabel 可能与指标自带的 `node` 标签打架 | 已**直接去掉**那段 relabel：agent 导出的每条 series 本来就自带 `node`（`os.Hostname()`），再 relabel 一个 `node` 只会多出 `exported_node` |
| 7.4a | `ConsecutiveDrift` 定义了但从未读写 | 已删除（`api/v1alpha1/statfsreport_types.go` + `config/crd/*.yaml`）。**没有**实现防抖，以免改动相位状态机、触碰 3.3 |
| 7.4b | 没有任何测试文件，`make test` 空跑 | 已补 `internal/controller/report_controller_test.go` 与 `internal/agent/agent_test.go`（纯函数、零新依赖），并接进 `make verify` 与 CI |
| 7.4c | `agentclient.go` 注释引用不存在的 `docs/` | 注释改为指向 `README.md`；同时新增了 `docs/DEPLOY.md` |
| 7.4e | `VolumeUsedRatio` 在配额被移除后残留 | `exportMetrics()` 中配额 `<= 0` 时改为 `DeleteLabelValues`，避免 `used_ratio > 0.85` 继续命中旧值 |
| 7.4f | 镜像地址是内网占位 | 已改为 `ghcr.io/zzhkhyl/statfs-guard-operator:latest`，由 CI 构建推送 |
| 审查-1 | ClusterRole 缺 `coordination.k8s.io/leases`，选主必然失败 | 已补规则。controller-runtime v0.21 固定用 `LeasesResourceLock`，而此前全仓没有任何 leases 规则：部署后 manager 只会反复刷 `is forbidden`，一条 report 都不产生，Pod 却是 Running |
| 审查-2 | 卷换节点/换存储类后，旧 label 组合的 8 个 gauge 永久残留 | `exportMetrics` 先用 `volumeSeries()` 删旧组合再写新值；触发条件抽成纯函数 `nodeChanged`，首次观测（`previous.Node == ""`）不算变更 |
| 审查-3 | agent 侧删卷后 3 组 series 不清理 | `sweep()` 的 Gone 循环补齐 `NodeDriftEpisodesTotal` / `NodeEpisodeSeconds` / `NodeRevalidateTotal`；counter 需按 `ok`、`error` 两个取值分别删 |
| 审查-4 | agent 的 HTTP server 没有任何超时 | 补 `ReadHeaderTimeout` / `ReadTimeout` / `IdleTimeout`；`WriteTimeout = probeTimeout + 30s`，因为 `/v1/probe` 是同步探测，写死会在调大 `--probe-timeout` 后截断响应 |
| 审查-5 | `Discover()` 返回 0 个卷时完全静默（3.7 的静默失效模式） | 每段「空期」输出一次 Warn、恢复时输出一次 Info，避免每 5 分钟刷屏 |
| 审查-6 | `initialSweep` 可能在 informer 就绪前 list，拿到空结果 | 初版用 `Cache.WaitForCacheSync`，但它只等**已启动**的 informer：informer 还没起来时直接返回 true，扫描仍拿到空列表（实测 20 个 PV 报 `cephfs_pvs: 0`）。现改为 `Reconciler.APIReader`（`mgr.GetAPIReader()`）直连 API server 做一次性 List，彻底不依赖 informer 状态 |
| 审查-7 | manager 的 `cephfs_guard_*` 指标一条都没导出 | `promauto` 注册到 prometheus **默认**注册表，而 controller-runtime 的 metrics server 服务的是 `sigs.k8s.io/controller-runtime/pkg/metrics.Registry`，两者不通。`:8080/metrics` 只有 controller-runtime 自己的指标，所有 `cephfs_guard_*` 告警永远匹配不到东西。现新增 `metrics.Register(ctrlmetrics.Registry)` 显式注册，并加单测锁住 |
| 审查-8 | `CephFSKubeletFigureUnreliable` 用 `drift_bytes != 0` | 与容差设计矛盾：实测 20 个卷里 19 个 drift 非 0（中位数 67KB，最大 1MB），全部远低于 64MiB 容差；装上 vmoperator 后会长期对 19 个卷告警，把真正要看的 `CephFSStatfsDriftPersists` 淹没。改为 `abs(drift) > 64MiB or abs(drift_ratio) > 0.05`，与 manager 的 `max(64MiB, 5%)` 对齐 |
| 审查-9 | `initialSweep` 的日志无法证明扫描真的读到了 PV | `cephfs_pvs` 记的是 `created`，空缓存与「20 个 PV 都已有报告」都输出 `0`，故障时无法区分，静默回归也看不出来。日志改为 `listed/cephfs_pvs/created/other_pvs` 四个数 |

### 7.2 仍未解决

| # | 事项 | 说明 |
|---|---|---|
| a | CRD 与 deepcopy 都是手写的 | 引入 controller-gen 需同步改 `Makefile`（现在没有 `manifests`/`generate` 目标）。手写 schema 没有任何自动校验，改 CRD 字段时最容易漏 |
| b | `prober` 不在镜像 ENTRYPOINT 里，但已在镜像内 | 可以用 `kubectl debug` / `kubectl exec` 直接调 `/usr/local/bin/prober`，这是刻意留的 |
| c | **operator 尚未装到任何集群** | 只在节点上跑过 agent 二进制（只读）。CRD/operator 是集群级变更 |
| d | 指标名没有测试保护 | `internal/metrics` 改名会静默打断告警，目前只能靠 2.5 节的对照表人工同步 |
| e | CI 不校验 `config/prometheus/**` 的 CRD schema | 只校验打包结果。要在装了 vmoperator 的集群上 `kubectl apply --dry-run=server` 才能确认真能 apply |
| f | `_probe_success` 语义在两处略有差别 | agent 侧是「rstat 读成功」，manager 侧是「相位不是 Error」；做告警时别混用 |

---

## 8. 给 AI 编码助手（CodeBuddy）的接手提示

在这个仓库里开新会话时，把下面这段作为上下文注入，能显著减少它走弯路：

```text
项目：statfs-guard，Kubernetes operator，修复 CephFS 的 statfs 读数陈旧问题。
仓库：github.com/zzhkhyl/statfs-guard-operator（Go module 同此路径）。
CRD：group storage.statfsguard.io，kind CephFSStatfsReport（短名 cfsr）。
语言 Go 1.24+，controller-runtime v0.21，k8s.io/* v0.33.13。
本轮开发范围：<写清楚要改什么，以及期望的验证方式>。

必读，按顺序：
1. DEVELOPING.md（尤其是第 3 节「承重不变量」与第 4 节「验收清单」）
2. internal/agent/agent.go 的 Probe() —— statfs 必须先于 rbytes 读，改反会静默失效
3. internal/controller/report_controller.go 的 inspect() / exportMetrics()

约定（不要违反）：
- 构建必须带 GOPROXY=https://goproxy.cn,direct GOSUMDB=off GOTOOLCHAIN=local
- 验证用 make verify；它等于：gofmt -l 为空 → GOOS=linux go vet ./... 为 0
  → linux+windows 双平台 go build ./... → go test ./... → hack/bundle.py
- 动了 config/** 必须跑 python3 hack/bundle.py，重生成 deploy/install.yaml 与
  deploy/monitoring.yaml，不要手改生成物
- 改指标名要同步 README 指标表 + config/prometheus/prometheusrule.yaml（VMRule）
- 改 CRD 字段要同步手写 zz_generated.deepcopy.go + config/crd/*.yaml
- 改 module 路径或 GroupName 的影响面见第 2.5 节对照表
- 纯函数有单元测试（internal/controller、internal/agent）；运行时的正确性仍然只靠第 5 节真机验证
- 修复动作是「读一个只读 xattr」，不要引入任何写操作、不要动配额、不要 remount、不要 client evict
```

如果任务要动 `internal/cephfs/` 或 agent 的探测顺序，**要求它先复述第 3.1 节的顺序约束和失效模式**，
再让它改。这个改动没有编译期保护。

---

## 9. 一页速查

```
架构      manager(Deployment, 集群侧对账/指标/告警)
          agent(DaemonSet, 每节点探测 = 修复)
          prober(一次性 CLI, 同一套原语)

地基      读 ceph.dir.rbytes  -> 强制 MDS 往返 -> 回填 i_rbytes -> 下一个 statfs 就对了
          所以「检测」与「修复」是同一个只读调用

铁律      ① statfs 先于 rbytes（改反 = 静默失效）
          ② auto-heal 下 drift != 0 是常态 → 事件要冷却
          ③ 活跃判据用 |drift| > tolerance，不用 StaleSeconds > 0
          ④ 相位型指标切相位/换节点要显式置 0；删卷要清 series
          ⑤ 缓存 per-node → 修复必须在持有挂载的节点上做
          ⑥ agent 零 API 权限；/v1/probe 只收 PV 名，不收路径

容差      max(64MiB, 5%)。CephFS 上「有点偏」是常态，容差是为容量决策服务的

验证      make verify（gofmt / vet / 双平台 build / go test / bundle 重生成）/ 真机 A-B 读前读后 df
```

---

## 10. 相关文档

| 文件 | 内容 |
|---|---|
| `README.md` | 问题背景、现场实测（示例数据）、架构、三个二进制、安装、参数、指标、告警、边界 |
| `docs/DEPLOY.md` | 面向部署者：镜像与拉取配置、安装与卸载、逐项验证、agent 权限说明、调参 |
| `LICENSE` | MIT |
| `.github/workflows/ci.yaml` | CI：`verify` 验收任务 + `image` 构建推送 GHCR |
