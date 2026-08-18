# KubeVirt 升降级删除 VM → PV 残留（jbd2 占用块设备）根因分析（2026-08-18）

> 本文档基于 `v1.2.0`（commit `b2af7b619c`）与 `v1.6.6`（commit `51395ba146`）两个 tag 的源码逐行对比，以及 4 条升降级路径的实验结果（只有路径 1 必现 PV 残留），定位删除 VM 后块设备清理不掉的根因。
>
> 前置背景：管理平面（virt-api / virt-controller / virt-handler）与 workload（virt-launcher / qemu）可分别升降级；`workloadUpdateMethods: []` 为空，workload 在升级后不自动重启，因此"升级管理平面到 1.6.6 但 workload 仍为 1.2.0"是真实存在的组合。被删 VM：`27f237e6b7f96587b6202ff3607ad88a`（arm64，96Gi，2Mi 大页，45 CPU，DedicatedCPUPlacement + IsolateEmulatorThread + NUMA，2 个 filesystem 型 PVC：`bootdisk-*` / `sysdisk-*`，storageClass `dorado-inner-san`，SAN 后端）。

---

## 0. TL;DR

| # | 结论 |
|---|------|
| 1 | 残留的直接内核机制：**两个 SAN 块设备上的 ext4 文件系统从未被卸载**，ext4 日志内核线程 `jbd2` 一直持有块设备（引用计数 >0），CSI 删块设备被内核拒绝 → PV 残留。 |
| 2 | `mount`/`lsof`/`fuser` 都查不到的原因：**该 ext4 挂载存在于一个"宿主 mount namespace 之外"的 namespace 中**——即 virt-launcher pod 的 mount namespace，它被一个**删除 pod 后仍存活（被孤儿化）的 QEMU 进程**持有。宿主工具只看得见宿主 namespace 的挂载。 |
| 3 | 根因（版本不对称）：**1.2.0 launcher 的优雅关机信号在 arm64 上是无效的**。1.6.6 handler 请求优雅关机 → 1.2.0 launcher 发出 `virDomainShutdownFlags(DOMAIN_SHUTDOWN_ACPI_POWER_BTN)`，在 arm64 上不生效，guest 永不掉电；而 1.6.6 launcher 用的是 `DOMAIN_SHUTDOWN_DEFAULT`（commit `d555b25e26` 专门为此修复）。 |
| 4 | 只有路径 1 必现，是因为**路径 2/3 的 launcher 都是 1.6.6（DEFAULT 信号有效），路径 4 虽同为 1.2.0 launcher + ACPI 无效信号，但 1.2.0 handler 的删除流程在 pod 被彻底拆除前把 QEMU 强杀掉了**（见 §6 的诚实讨论）。 |
| 5 | 修复方向：要么把 workload 一并升级到 1.6.6（DEFAULT 信号生效）；要么保证"删除 VM 前 guest 已被真正关机/强杀、QEMU 已退出、mount namespace 已释放"，再删 PVC/PV。 |

---

## 1. 现象与 4 条路径的 2×2 矩阵

| # | 创建时版本 | 删除时 handler/controller | 删除时 launcher | 结果 |
|---|-----------|--------------------------|-----------------|------|
| 1 | 1.2.0 | 1.6.6（管理面已升级） | **1.2.0**（workload 未重启） | **必现 PV 残留** |
| 2 | 1.2.0（后整体升到 1.6.6） | 1.6.6 | **1.6.6** | OK |
| 3 | 1.6.6（后降级） | 1.2.0 | **1.6.6** | OK |
| 4 | 1.6.6（后整体降到 1.2.0） | 1.2.0 | **1.2.0** | OK |

**关键点：路径 1 与路径 4 的 launcher 都是 1.2.0，唯一区别是"删除时谁在管理"（1.6.6 handler vs 1.2.0 handler）。** 因此根因必须是"1.6.6 管理面 + 1.2.0 launcher"这一未经测试的跨版本组合下的确定性行为差异。

---

## 2. 残留的直接内核机制：为什么 `mount`/`lsof`/`fuser` 都查不到

用户观测到：VM/VMI/launcher pod 全删、两个 PVC 已删，但 PV 残留；CSI 删块设备时内核报"引用计数不为 0"；`ps -aux | grep <block-name>` 只看到 `jbd2` 内核线程；`mount`/`lsof`/`fuser -mv` 全部无输出。

### 2.1 jbd2 = ext4 文件系统仍挂载的直接证据

`jbd2/<major>-<minor>-<dev>` 是 **ext4 日志线程**。只要一个 ext4 文件系统处于"已挂载"状态，内核就会维持一个 `jbd2` 内核线程持有该块设备的打开引用。**块设备引用计数不为 0 ⇒ 该 SAN 块设备上的 ext4 从删 pod 到删 PVC/PV 全程都没有被真正卸载。**

### 2.2 为什么宿主工具看不见这个挂载

`mount` 默认只列**当前进程所在 mount namespace** 的挂载树；`lsof`/`fuser -mv` 只在**宿主 PID namespace 可见的进程**里找持有者。当某挂载存在于**另一个 mount namespace** 中，且持有该 namespace 的进程不在宿主 namespace 的可见进程集合（或已被重新 init 收养为孤儿）时，三者都查不到。

**结论：这个 ext4 挂载活在 virt-launcher pod 的 mount namespace 里，而这个 namespace 被一个删除 pod 后仍存活的进程（QEMU）持有。** 只要 namespace 还活着，其中的挂载就不会被拆除，`jbd2` 就继续占住块设备。

### 2.3 谁挂载了这两个 filesystem PVC

两个卷是非热插拔 PVC，由 **kubelet/CRI（iSulad）在 pod 启动时**把 SAN 块设备的 ext4 文件系统挂进 virt-launcher pod 的 mount namespace（`/var/run/kubevirt-private/vmi-disks/<vol>/disk.img` 最终指向该挂载）。**virt-handler 不负责卸载非热插拔卷**——`processVmCleanup` 只调 `containerDiskMounter.Unmount`（容器盘）和 `hotplugVolumeMounter.UnmountAll`（热插拔盘，本 VM 无）。卸载这些 filesystem PVC 是 kubelet 在"pod 彻底终止后"做的事。**一旦 pod 的 mount namespace 被某个存活进程吊住，kubelet 的卸载就永远等不到"namespace 空"，PV/PVC 删除与块设备回收就会撞上 jbd2。**

所以整条链是：

```
guest 未真正关机 → QEMU 未退出 → QEMU 持有 pod 的 mount namespace
→ SAN ext4 挂载仍存在 → jbd2 持有块设备 → CSI 删块设备被内核拒绝 → PV 残留
```

---

## 3. 核心代码差异：优雅关机信号在 1.2.0 与 1.6.6 之间的一次不兼容修改

这是两个版本在"launcher 侧优雅关机"上的唯一实质差异，也是路径 2/3 不残留、路径 1 残留的分水岭。

### 3.1 commit `d555b25e26`（2024-11-04，在 1.6.6 中、不在 1.2.0 中）

```
Fix: graceful shutdown on non-ACPI architectures by using default signal

Graceful shutdown was failing on IBM Z (s390x) due to lack of ACPI support.
This fix changes virt-handler to send DOMAIN_SHUTDOWN_DEFAULT when a
graceful shutdown is requested.
...
- On non-ACPI architectures, an architecture-specific method is used.
```

改动两处：

**launcher 侧**（`pkg/virt-launcher/virtwrap/manager.go` `SignalShutdownVMI`）：

```diff
-		err = dom.ShutdownFlags(libvirt.DOMAIN_SHUTDOWN_ACPI_POWER_BTN)
+		err = dom.ShutdownFlags(libvirt.DOMAIN_SHUTDOWN_DEFAULT)
```

**handler 侧**（`pkg/virt-handler/vm.go` `helperVmShutdown`）：

```diff
-	// Only attempt to gracefully shutdown if the domain has the ACPI feature enabled
-	if isACPIEnabled(vmi, domain) && tryGracefully {
+	if domainHasGracePeriod(domain) && tryGracefully {
```

### 3.2 两版本实测行为对照

| 组件 | 1.2.0 | 1.6.6 |
|------|-------|-------|
| launcher `SignalShutdownVMI` 发出的关机信号 | `DOMAIN_SHUTDOWN_ACPI_POWER_BTN` | `DOMAIN_SHUTDOWN_DEFAULT` |
| handler `processVmShutdown` / `helperVmShutdown` 的"是否走优雅关机"门槛 | `isACPIEnabled(vmi, domain)` | `domainHasGracePeriod(domain)` |

- `isACPIEnabled` 与 `domainHasGracePeriod` 两版本**函数体完全一致**（`pkg/virt-handler/vm.go`，1.2.0 @3404/3410，1.6.6 @2279/2285）。
- `SetDefaults_FeatureState`（`staging/src/kubevirt.io/api/core/v1/defaults.go`）在 `ACPI.Enabled == nil` 时默认置 `true` → **arm64 的 domain XML 里也有 `<acpi>`** → 1.2.0 的 `isACPIEnabled` 在本 VM 上返回 true。因此**两条路径在 handler 侧都会进入优雅关机流程**，差异完全在 launcher 发出的信号上。
- domain 的 `GracePeriod.DeletionGracePeriodSeconds` 在 domain 创建时写入（`manager.go`：`GracePeriodMetadata{DeletionGracePeriodSeconds: converter.GracePeriodSeconds(vmi)}`），本 VM `TerminationGracePeriodSeconds` 未设 → 默认 30s（`DefaultGracePeriodSeconds = 30`）。`domainHasGracePeriod` 恒 true。

### 3.3 结论

在 arm64（且 guest 无 qemu-guest-agent 或 ACPI 关机事件不生效）的环境下：

- **1.2.0 launcher** 的 `ACPI_POWER_BTN` 是**无效信号**——这正是 `d555b25e26` 要修的那类问题（"graceful shutdown was failing on non-ACPI architectures"）。
- **1.6.6 launcher** 的 `DEFAULT` 让 libvirt/QEMU 自己选合适的方式（guest agent / ACPI / 架构特定方法），在路径 2/3 中真实生效。

因此：**路径 1 中 1.6.6 handler 请求优雅关机，1.2.0 launcher 发出一个不生效的 ACPI 关机事件，guest 永远不会掉电，QEMU 永远不退出。**

---

## 4. 1.6.6 handler 的删除路径比 1.2.0 更快/更激进（次级差异）

即便优雅关机失效，理论上还有"强杀"兜底。两版本都会在优雅关机 grace period（30s）到期后调用 `KillVirtualMachine`（`DestroyFlags(GRACEFUL)` → SIGTERM QEMU）。但在路径 1 里，1.6.6 handler 的行为让这条兜底更容易输给 pod 拆除竞态：

### 4.1 `deleteVM` = delete + cleanup 合并到同一次 reconcile（1.6.6 独有）

`pkg/virt-handler/vm.go`（1.6.6 @1312）：

```go
func (c *VirtualMachineController) deleteVM(vmi *v1.VirtualMachineInstance) error {
	err := c.processVmDelete(vmi)     // Undefine（不停止运行中的 QEMU！）
	if err != nil { return err }
	err = c.processVmCleanup(vmi)     // 立即清理本地状态 / 关闭 launcher client
	...
}
```

`processVmDelete` → `client.DeleteDomain(vmi)` → `UndefineFlags(DOMAIN_UNDEFINE_KEEP_NVRAM)`。**对运行中的 domain，Undefine 只是"去掉持久配置、使其变成 transient"，并不会停止 QEMU。** 1.2.0 则是 staged：`shouldDelete → processVmDelete`、`shouldCleanUp → processVmCleanup` 分开在不同的 reconcile 里，天然给 QEMU 退出留了时间窗口。

### 4.2 1.6.6 新增触发条件：`!domainAlive && domainExists && !vmi.IsFinal() → shouldDelete`

`pkg/virt-handler/vm.go`（1.6.6 @1422）：

```go
if !domainAlive && domainExists && !vmi.IsFinal() {
	log.Log.Object(vmi).V(3).Info("Deleting inactive domain for vmi.")
	shouldDelete = true
}
```

`domainAlive = domainExists && status ∉ {Shutoff, Crashed, ""}`。这条在 1.2.0 中没有等价逻辑（已 grep 确认）。一旦 1.6.6 handler 从 domain 缓存看到"domain 存在但状态为空/Shutoff"（pod 拆除窗口内 GetDomain 失败、socket 抖动、ghost record 兜底等都可能产生这种瞬时状态），就会直接走 `deleteVM` → **对可能仍在运行的 QEMU 执行 Undefine + cleanup**，而不是先尝试 Kill。

### 4.3 grace period 时钟来源不同（1.6.6 读 VMI，1.2.0 只读 domain metadata）

1.6.6 `hasGracePeriodExpired(terminationGracePeriod *int64, dom)`（@1559）：**优先读 `vmi.Spec.TerminationGracePeriodSeconds`**，为空才回退 domain metadata；1.2.0（@462）只读 domain metadata。本 VM 两者都是 30s，数值相同，但 1.6.6 版本在 VMI 侧时钟可用时更"确定"，30s 一到必触发 Kill。

### 4.4 virt-controller 一看到 VMI 有 DeletionTimestamp 就立刻删 launcher pod

两版本一致（1.6.6 `pkg/virt-controller/watch/vmi/lifecycle.go` @64、1.2.0 `vmi.go` @1163）：

```go
if vmi.DeletionTimestamp != nil {
	err := c.deleteAllMatchingPods(vmi)   // 默认 DeleteOptions，走 pod 的 TerminationGracePeriodSeconds
	...
}
```

**pod 删除动作与 QEMU 是否已停止无关，立即发起。** pod 的 `TerminationGracePeriodSeconds` 由模板生成（两版本都是 `gracePeriodKillAfter = TerminationGracePeriodSeconds + 15 + 15 = 60s`），kubelet 在 60s 后 SIGKILL 容器。**优雅关机失效时，QEMU 能否活着撑到 60s SIGKILL 之后，决定了 mount namespace 是否残留。**

---

## 5. 路径 1 的完整故障时序

```
t=-∞   VM 由 1.2.0 全套创建；launcher pod 挂载 2 个 SAN ext4 filesystem PVC。
t=0    ～管理面升到 1.6.6（handler/controller=1.6.6，launcher 仍是 1.2.0）。
t=0    ～用户删除 VM → VM controller 给 VMI 打 DeletionTimestamp。
t≈0    1.6.6 virt-controller 立刻 deleteAllMatchingPods() → 删除 launcher pod（grace 60s）。
t≈0    1.6.6 handler sync：VMI 有 DeletionTimestamp、domain alive → shouldShutdown
       → processVmShutdown → helperVmShutdown：domainHasGracePeriod=true
       → shutdownVMI → RPC ShutdownVirtualMachine。
t≈0    1.2.0 launcher SignalShutdownVMI：dom.ShutdownFlags(ACPI_POWER_BTN)
       → arm64 无效 → guest 不掉电；仅把 domain metadata DeletionTimestamp 置为 now。
       （launcher 侧 ProcessMonitor 在 pod 被 SIGTERM 后也进入 45s 优雅倒计时，
         到点会 KillVMI —— 但这条兜底同样受下面竞态影响）
t≈0    kubelet SIGTERM 所有容器 → virt-launcher-monitor(1.2.0) 转发 SIGTERM 给 virt-launcher
       → launcher 开始 45s 倒计时。
t≈5…  1.6.6 handler 每 5s 重发 Shutdown RPC（仍 ACPI 无效）。
t=30   1.6.6 handler hasGracePeriodExpired → KillVirtualMachine
       → 1.2.0 launcher KillVMI → DestroyFlags(GRACEFUL/SIGTERM) → 尝试杀 QEMU。
       若此刻 launcher socket 已随 pod 拆除失效 / launcher 已被 SIGKILL，
       RPC 失败（IsDisconnected 被吞掉），QEMU 没被杀死。
t=45   1.2.0 launcher 自身 monitor 倒计时到点 → KillVMI。
       若 virt-launcher-monitor 的 1.2.0 单次 Wait4 漏 reap（见 §5.1），
       或 launcher 已被 SIGKILL，这条兜底也不触发。
t=60   kubelet 60s 到点 SIGKILL 容器 cgroup。
       —— 但 QEMU 若已被孤儿化（如 IsolateEmulatorThread 的 housekeeping cgroup
          逃逸、或 transient domain 无配置无人可管），则 QEMU 存活，
          pod 的 mount namespace 存活，SAN ext4 挂载存活。
t≈∞    VMI finalizer 在 allPodsDeleted 后移除 → VMI/VM 删除；PVC 删除 → PV 回收
       → CSI 删块设备 → 内核：块设备引用计数非 0（jbd2 持有）→ 拒绝 → PV 残留。
```

### 5.1 1.2.0 virt-launcher-monitor 的单次 `Wait4`（可能漏 reap，1.6.6 已修）

`cmd/virt-launcher-monitor/virt-launcher-monitor.go`：

- 1.2.0（@188-201）：SIGCHLD 时**只做一次** `syscall.Wait4(-1, ..., WNOHANG, nil)`。若先 reap 到的是 QEMU 等其它子进程，virt-launcher 主进程的退出被漏掉 → `exitCode := <-exitStatus` 永远阻塞 → monitor 作为 pod PID 1 不退出、也不执行后面的"qemu SIGTERM 兜底"，直到 60s 被 SIGKILL。
- 1.6.6（@121-140）：改为 `for { Wait4 ... break }` 的 reap 循环，不会漏。

这条是"1.2.0 launcher 在 pod 拆除窗口内对 QEMU 的最后兜底（launcher 自身 monitor 45s 杀 QEMU + monitor 的 qemu-SIGTERM）容易集体失效"的放大器，进一步解释为什么路径 1 的 QEMU 能活过 pod 拆除。

---

## 6. 为什么路径 2/3 不残留，路径 4 也不残留（及诚实的不确定点）

| 路径 | launcher 关机信号 | 为什么 guest 最终停掉 / QEMU 退出 |
|------|------------------|----------------------------------|
| 2 | 1.6.6 `DEFAULT` | 信号在 arm64 上真实生效 → guest 关机 → domain Shutoff → QEMU 正常退出 → 挂载释放。 |
| 3 | 1.6.6 `DEFAULT` | 同上（handler 是 1.2.0，但其 `isACPIEnabled` 在本 VM 上也为 true，一样会走优雅关机 → launcher 发 DEFAULT → 生效）。 |
| 4 | 1.2.0 `ACPI_POWER_BTN` | 与路径 1 **信号相同**（同样无效），但删除方是 1.2.0 handler：staged 流程（shutdown→delete→cleanup 分多次 reconcile）+ `closeLauncherClient` 的 watchdog/legacy-socket 清理，使 pod 拆除前 QEMU 已被 30s Kill 或 45s 兜底干掉，且未孤儿化。 |

**诚实声明（未 100% 静态锁定）**：路径 4 与路径 1 的 launcher 完全一样、ACPI 无效信号也一样，为何路径 4 能"强杀成功/不孤儿化"、路径 1 会"强杀失败/孤儿化"，在纯静态代码层面无法 100% 唯一确定。最可能的解释是 1.6.6 handler 的 `deleteVM` 合并流程 + 新增 `!domainAlive && domainExists` 触发，使"Undefine/cleanup 先于 QEMU 停止"更容易发生；但也存在其它竞争因素（iSulad 对 pod cgroup 的 SIGKILL 是否覆盖 IsolateEmulatorThread 的 housekeeping 子 cgroup；launcher socket 何时失效等）。

**最终确认判据（需要路径 1 的运行时日志）**：
1. virt-handler 日志应出现 `Signaled graceful shutdown for ...`（第一次 Shutdown RPC）且**之后没有** `Grace period expired, killing ...` 后面的 QEMU 成功退出事件；或出现 `IsDisconnected` 的 kill 失败。
2. virt-launcher（1.2.0）日志应有 `Signaled graceful shutdown`（ACPI 分支），但**没有** `Domain stopped` / qemu pid gone。
3. 删除后节点上 `ps -aux | grep qemu` 应能找到孤儿 QEMU（其命令行里含两个磁盘的 `disk.img` 路径）——这是"mount namespace 被 QEMU 持有"的直接证据。
4. `lsns -t mnt` / `ls /proc/<qemu-pid>/ns/mnt` 能定位该 namespace；`nsenter -t <qemu-pid> -m mount | grep vmi-disks` 应能看到两个 ext4 挂载。

---

## 7. 修复与规避建议

1. **根治：让关机信号有效。** 把 workload 一并升级到 1.6.6（launcher 用 `DEFAULT` 信号），或在删除前确保 guest 已真正关机（`virtctl stop` 并等 VMI Succeeded）。这是官方支持路径。
2. **兜底：删除前先确认 QEMU 已退出。** 在删 VM 前检查节点上无该 VM 的 `qemu-system*` 进程、无残留的 `vmi-disks` 挂载，再删 PVC/PV。
3. **若 1.2.0 是定版：** 给 1.2.0 launcher 的 `SignalShutdownVMI` 打 `DOMAIN_SHUTDOWN_ACPI_POWER_BTN → DOMAIN_SHUTDOWN_DEFAULT` 的等价补丁（即把 `d555b25e26` 的 launcher 侧改动移植回 1.2.0）。该改动很小且对 x86 无副作用（DEFAULT 在 x86 上同样走 ACPI）。
4. **运维止血：** PV 残留时，找到孤儿 QEMU（`ps -aux | grep qemu` / 按磁盘路径 grep），`kill` 该 QEMU，等 `jbd2` 消失、`mount` 干净后再删 PV；找不到孤儿进程时检查 `lsns`/`nsenter`。

---

## 8. 关键源码位置

| 文件（tag） | 内容 |
|------------|------|
| `commit d555b25e26`（1.6.6 含，1.2.0 不含） | handler `helperVmShutdown` 门槛 `isACPIEnabled` → `domainHasGracePeriod`；launcher `SignalShutdownVMI` 信号 `ACPI_POWER_BTN` → `DEFAULT`。 |
| `pkg/virt-launcher/virtwrap/manager.go` `SignalShutdownVMI` | 1.2.0 用 `DOMAIN_SHUTDOWN_ACPI_POWER_BTN`；1.6.6 用 `DOMAIN_SHUTDOWN_DEFAULT`。 |
| `pkg/virt-handler/vm.go` `processVmShutdown` / `helperVmShutdown` / `hasGracePeriodExpired` | 1.2.0 门槛 `isACPIEnabled`；1.6.6 门槛 `domainHasGracePeriod`；1.6.6 grace 时钟优先读 VMI。 |
| `pkg/virt-handler/vm.go` `deleteVM`（1.6.6 @1312） | `processVmDelete`（Undefine，不停运行中 QEMU）+ `processVmCleanup` 合并到同一次 reconcile。 |
| `pkg/virt-handler/vm.go`（1.6.6 @1422） | 新增 `!domainAlive && domainExists && !vmi.IsFinal() → shouldDelete`（1.2.0 无等价）。 |
| `pkg/virt-controller/watch/vmi/lifecycle.go`（1.6.6 @64）/ `vmi.go`（1.2.0 @1163） | VMI 有 DeletionTimestamp 立即 `deleteAllMatchingPods` 删 launcher pod。 |
| `cmd/virt-launcher-monitor/virt-launcher-monitor.go` | 1.2.0 单次 `Wait4` 漏 reap；1.6.6 reap 循环。 |
| `pkg/virt-controller/services/template.go` | 两版本均 `gracePeriodKillAfter = +15+15 = 60s` pod 终止宽限。 |
| `staging/src/kubevirt.io/api/core/v1/defaults.go` | `SetDefaults_FeatureState` 默认 `ACPI.Enabled=true` → arm64 XML 也有 `<acpi>`。 |
| `pkg/virt-handler/cgroup/` | 1.6.6 `NewManagerFromVM(vmi, host)`（新增 host 参数）；IsolateEmulatorThread → housekeeping 子 cgroup。 |

---

*参考：KubeVirt commit [`d555b25e26`](https://github.com/kubevirt/kubevirt/commit/d555b25e26978ad7aad829444ae6761c37000878) "Fix: graceful shutdown on non-ACPI architectures by using default signal"；内核 `jbd2` 线程与块设备引用计数机制（ext4 挂载存活 ⇒ jbd2 持块设备）。*
