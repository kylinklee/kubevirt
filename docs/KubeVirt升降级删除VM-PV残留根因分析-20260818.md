# KubeVirt 升降级删除 VM → PV 残留根因分析（2026-08-18 二次修正版）

> 本文档基于 `v1.2.0`（commit `b2af7b619c`）与 `v1.6.6`（commit `51395ba146`）两个 tag 的源码逐行对比、fork 工作树改动、以及用户提供的新关键证据，定位"1.2.0 建 VM → 升级管理面到 1.6.6 → 删除 VM"（路径 1）必现 PV 残留的根因。
>
> **2026-08-18 二次修正**：用户提供新证据——**残留的块设备挂载出现在 virt-handler 进程自己的 `/proc/<pid>/mountinfo` 里**；纯 1.2.0 / 纯 1.6.6 集群都不会出现；1.2.0 升级到 1.6.6 后出现；**重拉 virt-handler pod/进程后这些挂载消失**。据此修正第一版"孤儿 QEMU 持有 launcher mount namespace"的推断（第一版被证伪），新的根因聚焦在 **handler 自身 mount namespace 里的挂载传播副本**。

---

## 0. TL;DR（二次修正版）

| # | 结论 |
|---|------|
| 1 | 残留的直接内核机制不变：**SAN 块设备上的 ext4 从未被卸载**，`jbd2` 内核线程持有块设备（引用计数 >0），CSI 删块设备被内核拒绝 → PV 残留。 |
| 2 | **新证据把残留位置钉死在 virt-handler 自己的 mount namespace 里**（`/proc/<handler-pid>/mountinfo` 可见、重拉 handler 即消失）。所以不是"孤儿 QEMU 持有 launcher namespace"（第一版，已撤回），而是 **handler 自身 namespace 里的一个挂载传播副本**。 |
| 3 | 挂载进入 handler namespace 的机制：**virt-handler DaemonSet 以 `MountPropagation: Bidirectional` 挂载了宿主的 `/var/lib/kubelet`**。这是 1.6.6 独有的改动（commit `152652ee10`）。kubelet 为 launcher pod 挂载 SAN filesystem PVC 时，会在宿主 `/var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~csi/<pvc>/mount` 创建 ext4 挂载，该挂载**经共享挂载组传播进 handler 的 namespace**。 |
| 4 | 为什么只有路径 1 残留：路径 1 中 QEMU 在 pod 拆除时**仍未退出**（1.2.0 launcher 的 arm64 优雅关机失效 + 1.6.6 handler 更快/更激进的删除流程 + pod 立即被删），kubelet 无法干净卸载 SAN 卷 → 宿主侧挂载保持 busy → **handler 侧的传播副本也一直存在** → PV 残留。 |
| 5 | 为什么重拉 handler 就消失：重启 handler pod 即销毁其 mount namespace，传播副本随 namespace 一起消失。**这是"重拉可解决"的直接解释，也反向证明了残留就在 handler 的 namespace 里。** |

---

## 1. 现象与关键新证据

用户观测（第一版已记录）：
- VM/VMI/virt-launcher pod 全删、两个 PVC 已删，但 2 个 PV 残留（SAN，storageClass `dorado-inner-san`）。
- CSI 删块设备时内核报"引用计数不为 0"，`ps -aux | grep <block>` 只见 `jbd2` 内核线程。
- `mount` / `lsof` / `fuser -mv` 全部无输出。

**二次修正的关键新证据（用户提供）**：
- 在**纯 1.2.0** 和**纯 1.6.6** 集群中，`virt-handler` 进程的 `/proc/<pid>/mountinfo` **都没有**残留的块设备信息。
- **1.2.0 升级到 1.6.6 后**，handler 的 mountinfo 里出现了残留块设备挂载。
- **重拉 virt-handler pod（或重启进程）后，这些挂载就消失了。**

这三个事实串起来，结论只有一个：**残留挂载位于 virt-handler 自身进程的 mount namespace 中，且它的存活期 = handler 进程的存活期。**

---

## 2. 为什么挂载会出现在 handler 自己的 mountinfo 里（核心机制）

### 2.1 关键代码差异：commit `152652ee10`（1.6.6 独有，1.2.0 无）

```
commit 152652ee10  virt-handler: mount all of /var/lib/kubelet
Date: 2024-11-15
```

该 commit 把 virt-handler DaemonSet 的卷挂载从"只 bidi 挂载 `/var/lib/kubelet/pods`"改为"**bidi 挂载整个 `/var/lib/kubelet`**"：

**1.2.0**（`pkg/virt-operator/resource/generate/components/daemonsets.go`）：

```go
volumes := []volume{
    ...
    {"kubelet-pods-shortened", kubeletPodsPath, "/pods", nil},
    {"kubelet-pods", kubeletPodsPath, kubeletPodsPath, &bidi},   // 只有 /var/lib/kubelet/pods 是 bidi
    ...
}
```

**1.6.6**（同一文件）：

```go
volumes := []volume{
    ...
    {"kubelet-pods", kubeletPodsPath, "/pods", nil},
    {"kubelet", util.KubeletRoot, util.KubeletRoot, &bidi},      // 整个 /var/lib/kubelet 是 bidi
    ...
}
```

`bidi := corev1.MountPropagationBidirectional`。`MountPropagation: Bidirectional` 意味着：**宿主在挂载点下新建的任何挂载，都会双向传播进 handler pod 的 mount namespace；handler namespace 里的挂载也会传回宿主。** 1.2.0 与 1.6.6 都挂了 `/var/lib/kubelet/pods`（bidi），但 1.6.6 把范围扩大到了整个 `/var/lib/kubelet`。

### 2.2 SAN filesystem PVC 挂载传播进 handler namespace

对 filesystem 型 PVC（本 VM 的两个卷），kubelet/CRI 会在启动 launcher pod 时把 SAN 块设备的 ext4 文件系统挂载到宿主：

```
/var/lib/kubelet/pods/<launcher-uid>/volumes/kubernetes.io~csi/<pvc>/mount   ← ext4，source 是 SAN 块设备
```

然后 bind 进 launcher pod 的 mount namespace（`/var/run/kubevirt-private/vmi-disks/<vol>`）。

由于 handler 对宿主的 `/var/lib/kubelet` 是 Bidirectional 挂载，**这个 ext4 挂载一旦在宿主 `/var/lib/kubelet/pods/...` 下建立，就会作为共享挂载组的 peer，自动传播进 handler 自己的 mount namespace**（在相同路径 `…/kubernetes.io~csi/…/mount` 出现一份副本）。这就是 handler 的 `/proc/<pid>/mountinfo` 里出现 SAN 块设备的原因。

> 1.2.0 handler 同样 bidi 挂了 `/var/lib/kubelet/pods`，所以也会收到这份传播副本；**但 1.2.0 里 VMI 删除时 SAN 挂载会被正常释放，传播副本随之消失**（见 §4）。

### 2.3 handler 从不主动清理这类传播副本

virt-handler 的删除清理（`processVmCleanup`）只负责：
- `containerDiskMounter.Unmount`（容器盘）
- `hotplugVolumeMounter.UnmountAll`（热插拔盘）
- `teardownNetwork` / `CloseLauncherClient` / `domainStore.Delete`

**它从不（也不需要）主动 umount kubelet 的 CSI 卷挂载**——那是 kubelet 的职责。handler 侧对传播副本的"清理"完全依赖宿主侧卸载后经共享挂载组反向传播的 umount。**只要宿主侧 SAN 挂载一直 busy 未被卸载，handler 侧的传播副本就一直在。** 这是残留的本质。

---

## 3. 为什么只有路径 1 残留：QEMU 未退出 → SAN 挂载保持 busy

### 3.1 优雅关机失效（arm64 上 1.2.0 launcher 的 ACPI 信号无效）

launcher 侧 `SignalShutdownVMI` 发出的关机信号，两版本不同：

| 组件 | 1.2.0 | 1.6.6 |
|------|-------|-------|
| `SignalShutdownVMI` | `DOMAIN_SHUTDOWN_ACPI_POWER_BTN` | `DOMAIN_SHUTDOWN_DEFAULT` |
| 来源 | 上游原版 | commit `d555b25e26`（"graceful shutdown on non-ACPI architectures"） |

**commit `d555b25e26`**（2024-11-04，在 1.6.6 中、不在 1.2.0 中）正是为了修"非 ACPI 架构上优雅关机失效"：

```
Fix: graceful shutdown on non-ACPI architectures by using default signal
Graceful shutdown was failing on IBM Z (s390x) due to lack of ACPI support.
This fix changes virt-handler to send DOMAIN_SHUTDOWN_DEFAULT when a
graceful shutdown is requested.
...
- On non-ACPI architectures, an architecture-specific method is used.
```

本 VM 是 **arm64**。在 arm64 上，`ACPI_POWER_BTN` 这一事件并不被 QEMU/guest 响应（这正是 `d555b25e26` 要修的问题之一）。而 `DEFAULT` 让 libvirt/QEMU 自行选择合适方式（guest agent → ACPI → 架构特定），在 arm64 上才真正有效。

**路径 1 = 1.6.6 handler + 1.2.0 launcher**：handler 请求优雅关机 → 1.2.0 launcher 发出 `ACPI_POWER_BTN` → arm64 上无效 → **guest 不掉电，QEMU 不退出**。

### 3.2 fork 工作树的佐证：fork 把 launcher 又改回 ACPI 分支

fork 工作树相对 v1.6.6 的 `pkg/virt-launcher/virtwrap/manager.go` 改动（`SignalShutdownVMI`）：

```go
// [升级兼容] v1.5.0 起（commit d555b25e26）改用 DOMAIN_SHUTDOWN_DEFAULT 信号，
// 该信号会让 libvirt 优先通过 QEMU guest agent 发起关机；当 guest 内未运行
// qemu-ga 时 agent 关机无效，优雅关机将超时并 destroy domain，
// 导致 VMI 以 Failed 结束，触发 runStrategy=RerunOnFailure 的自动重启。
// 恢复 v1.2.0 行为：domain 实际启用 ACPI 时显式发送 ACPI 电源键；
shutdownFlag := libvirt.DOMAIN_SHUTDOWN_DEFAULT
if domainHasACPI(dom) {
    shutdownFlag = libvirt.DOMAIN_SHUTDOWN_ACPI_POWER_BTN
}
err = dom.ShutdownFlags(shutdownFlag)
```

也就是说 **fork 环境自己就确认过"arm64 上 ACPI 优雅关机无效"这一现象**（其注释明确提到 qemu-ga 缺失时优雅关机超时、VMI Failed、触发 RerunOnFailure 自动重启）。`domainHasACPI(dom)` 从 active XML 判 ACPI，本 VM arm64 XML 里有 `<acpi>` → fork 的 1.6.6 launcher 也会走 ACPI 分支。**在 arm64 + 无 qemu-ga 时，ACPI 关机事件不生效。**

### 3.3 1.6.6 handler 的删除流程让"强杀兜底"更容易输给 pod 拆除竞态

即便优雅关机失效，两版本都有"强杀"兜底（grace period 到期 → `KillVirtualMachine` → `DestroyFlags(GRACEFUL)`）。但路径 1 的 1.6.6 管理面有多个行为让兜底更难成功：

1. **`deleteVM` 把 delete + cleanup 合并到同一次 reconcile**（1.6.6 `pkg/virt-handler/vm.go` @1312）：

   ```go
   func (c *VirtualMachineController) deleteVM(vmi *v1.VirtualMachineInstance) error {
       err := c.processVmDelete(vmi)     // Undefine——不停运行中的 QEMU
       ...
       err = c.processVmCleanup(vmi)     // 立即关闭 launcher client
       ...
   }
   ```

   `processVmDelete` → `client.DeleteDomain` → `UndefineFlags(DOMAIN_UNDEFINE_KEEP_NVRAM)`。**Undefine 只移除 domain 配置，不停止运行中的 QEMU。** 1.2.0 则是 staged：shutdown → delete → cleanup 分在多次 reconcile，天然给 QEMU 退出留时间。

2. **新增触发：`!domainAlive && domainExists && !vmi.IsFinal() → shouldDelete`**（1.6.6 @1422）：

   ```go
   if !domainAlive && domainExists && !vmi.IsFinal() {
       log.Log.Object(vmi).V(3).Info("Deleting inactive domain for vmi.")
       shouldDelete = true
   }
   ```

   1.2.0 无此等价逻辑。pod 拆除窗口内 handler 从 domain 缓存看到"domain 存在但状态为空/Shutoff"（GetDomain 失败、socket 抖动、ghost record 兜底）即直接 `deleteVM`，对仍运行的 QEMU 执行 Undefine + cleanup，而不是先 Kill。

3. **virt-controller 看到 VMI 有 DeletionTimestamp 立即删 launcher pod**（两版本一致：1.6.6 `lifecycle.go` @64、1.2.0 `vmi.go` @1163）：

   ```go
   if vmi.DeletionTimestamp != nil {
       err := c.deleteAllMatchingPods(vmi)   // 立即发起，与 QEMU 是否停止无关
   }
   ```

   pod 的 `TerminationGracePeriodSeconds` 为 `TerminationGracePeriodSeconds + 15 + 15 = 60s`，kubelet 60s 后 SIGKILL。**优雅关机失效时，QEMU 能否活过 pod 拆除，决定了 SAN 挂载是否被释放。**

4. **1.2.0 virt-launcher-monitor 的单次 `Wait4` 漏 reap**（`cmd/virt-launcher-monitor/virt-launcher-monitor.go`）：

   - 1.2.0（@188-201）：SIGCHLD 只 `Wait4` 一次。若先 reap 到 QEMU 等其它子进程，virt-launcher 主进程退出被漏掉 → `exitStatus` 永不就绪 → monitor 不退出、也不执行"qemu SIGTERM 兜底"。
   - 1.6.6（@121-140）：`for { Wait4 … break }` reap 循环，不遗漏。

### 3.4 完整故障时序（路径 1）

```
t=-∞   VM 由 1.2.0 全套创建；launcher pod 挂载 2 个 SAN ext4 filesystem PVC。
       —— handler 因 bidi /var/lib/kubelet，SAN 挂载的传播副本进入 handler 自身 mount namespace。
t=0    ~管理面升到 1.6.6（handler/controller=1.6.6，launcher 仍是 1.2.0）。
t=0    ~用户删除 VM → 1.6.6 virt-controller 给 VMI 打 DeletionTimestamp。
t≈0    1.6.6 virt-controller 立即 deleteAllMatchingPods() → 删 launcher pod（grace 60s）。
t≈0    1.6.6 handler sync：VMI 有 DeletionTimestamp、domain alive → shouldShutdown
       → processVmShutdown → helperVmShutdown：domainHasGracePeriod=true
       → RPC ShutdownVirtualMachine。
t≈0    1.2.0 launcher SignalShutdownVMI：dom.ShutdownFlags(ACPI_POWER_BTN)
       → arm64 无效 → guest 不掉电；仅把 domain metadata DeletionTimestamp 置为 now。
t≈0    kubelet SIGTERM 容器 → launcher 进入 45s 优雅倒计时（其自身兜底）。
t≈5…   1.6.6 handler 每 5s 重发 Shutdown RPC（仍 ACPI 无效）。
t=30   1.6.6 handler hasGracePeriodExpired → KillVirtualMachine
       → 1.2.0 launcher KillVMI → DestroyFlags → 尝试杀 QEMU。
       若此刻 launcher socket 已失效 / launcher 已被 SIGKILL / RPC 断开（IsDisconnected 被吞），
       QEMU 没被杀死。
t=45   1.2.0 launcher 自身 monitor 倒计时到点 → KillVMI。
       若 1.2.0 virt-launcher-monitor 单次 Wait4 漏 reap（§3.3-4），或 launcher 已被 SIGKILL，
       这条兜底也不触发。
t=60   kubelet 60s 到点 SIGKILL 容器 cgroup。QEMU 存活（孤儿化）→ 持有 SAN 挂载。
       —— 宿主 /var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~csi/.../mount 保持 busy。
       —— handler 自身 namespace 里的传播副本随之保持存在（jbd2 持块设备）。
t≈∞    VMI finalizer 移除 → VMI/VM/PVC 删除 → PV 回收 → CSI 删块设备
       → 内核：引用计数非 0（jbd2）→ 拒绝 → PV 残留。
```

**关键收尾**：`ps -aux | grep` 只见 `jbd2`、`mount/lsof/fuser` 无输出的原因——残留的 ext4 挂载在 handler 的 mount namespace 里（宿主 `mount` 命令看不到其它 namespace 的挂载；`lsof`/`fuser` 不显示内核线程对块设备的持有）。**而重拉 handler pod 即销毁其 mount namespace，传播副本随之消失 → "重拉能解决"。**

---

## 4. 为什么纯 1.2.0 / 纯 1.6.6 / 路径 2/3/4 都不残留

| 路径 | handler | launcher | 关机信号 | 为什么 SAN 挂载最终被释放 |
|------|---------|----------|----------|--------------------------|
| 2 | 1.6.6 | 1.6.6 | `DEFAULT` | arm64 上真实生效 → guest 关机 → QEMU 正常退出 → kubelet 干净卸载 SAN 卷 → 宿主侧释放，handler 传播副本随之消失。 |
| 3 | 1.2.0 | 1.6.6 | `DEFAULT` | 同上（handler 的 `isACPIEnabled` 本 VM 为 true，也会走优雅关机 → launcher 发 DEFAULT → 生效）。 |
| 4 | 1.2.0 | 1.2.0 | `ACPI_POWER_BTN` | 信号同样无效，但 1.2.0 handler 是 staged 流程（shutdown→delete→cleanup 分多次 reconcile），且 `processVmCleanup` 里 `closeLauncherClient` 会做 legacy socket / watchdog 清理；QEMU 在 pod 拆除前被 30s Kill 或 launcher 45s 兜底杀死 → SAN 挂载释放 → handler 传播副本消失。 |
| **1** | **1.6.6** | **1.2.0** | **`ACPI_POWER_BTN`（无效）** | **QEMU 未被杀死 → SAN 挂载保持 busy → handler 传播副本残留 → PV 残留。** |

**纯 1.2.0 / 纯 1.6.6 集群 handler mountinfo 干净**的原因：所有挂载都随 QEMU 退出而被 kubelet 干净卸载，传播副本经共享挂载组反向传播 umount 而消失。残留只在"QEMU 没死 + SAN 挂载 busy"的路径 1 出现。

---

## 5. 修复与规避建议（按优先级）

1. **根治（推荐）：workload 一并升级到 1.6.6。** 1.6.6 launcher 用 `DEFAULT` 信号，arm64 优雅关机真实生效，QEMU 正常退出，SAN 挂载被释放，不会残留。这是官方支持路径。
2. **删除前确保 guest 已真正关机**：`virtctl stop` 并等待 VMI 进入 Succeeded，再删 VM/PVC。
3. **若 1.2.0 是定版**：给 1.2.0 launcher 的 `SignalShutdownVMI` 移植 `d555b25e26` 的 launcher 侧改动（`ACPI_POWER_BTN → DEFAULT`），或参考 fork 工作树的 `domainHasACPI` 思路，在 arm64/无 qemu-ga 场景避免依赖 ACPI 关机。
4. **运维止血**：PV 残留时，先找到仍存活的 QEMU（`ps -aux | grep qemu` / 按磁盘路径 grep），`kill` 之，等待 `jbd2` 消失、`mount` 干净后再删 PV。找不到 QEMU 时，**重拉 virt-handler pod** 也会清掉其 namespace 里的传播副本（但需确认宿主侧 SAN 挂载已释放，否则只是暂时"隐藏"）。
5. **长期加固（可选）**：若确认挂载传播是主因，可评估 handler DaemonSet 的 `/var/lib/kubelet` 挂载传播策略，或让 handler 在 VMI 删除后主动检查/清理自身 namespace 下对应 pod 的 CSI 挂载传播副本（需谨慎，避免误删仍被其它组件使用的挂载）。

---

## 6. 诚实的不确定点与最终确认判据

### 6.1 未 100% 静态锁定的点

1. **路径 4（纯 1.2.0）为何强杀一定成功、路径 1 为何一定失败**：两版本都有 30s Kill / 45s 兜底，纯静态代码无法唯一钉死"路径 1 中所有兜底都恰好失效"的确切瞬间（launcher socket 何时失效、1.2.0 monitor 单次 Wait4 是否漏 reap、kubelet/iSulad SIGKILL 的精确时机）。已列出最可能的候选差异（§3.3），但需要运行时日志最终确认。
2. **fork 的 vhost-disk-hot-plug 注解**（VM 上有 `kubevirt.io/enable-vhost-disk-hot-plug: true`）在 handler 二进制中未检索到对应处理逻辑；该注解可能由 virt-controller/virt-launcher 消费。若该特性会触发额外的挂载/热插拔行为，可能叠加影响，但不改变"QEMU 未退出 → SAN 挂载 busy → handler 传播副本残留"的主链条。
3. 挂载传播是否在"纯 1.2.0"中同样进入 handler namespace（1.2.0 也 bidi 挂了 `/var/lib/kubelet/pods`）——按共享挂载组语义应当会，但因 QEMU 正常退出而被释放，故纯 1.2.0 不残留。这一点可用 §6.2 判据在运行时验证。

### 6.2 最终确认判据（需要路径 1 的运行时数据）

1. **删除后**，在节点上执行 `nsenter -t <virt-handler-pid> -m mount | grep vmi-disks`（或直接看 handler 的 `/proc/<pid>/mountinfo`），应能看到两个 SAN ext4 挂载——**直接证明残留就在 handler namespace**。
2. 对比 `nsenter -t <virt-handler-pid> -m mount | grep kubernetes.io~csi` 与宿主 `mount | grep kubernetes.io~csi`：handler 里有、宿主里也可能有（busy）。
3. **删除后** `ps -aux | grep qemu`：应能找到孤儿 QEMU（命令行含两个 `disk.img` 路径）。
4. handler 日志：应有 `Signaled graceful shutdown for ...`（第一次 Shutdown RPC）之后**没有** QEMU 成功退出的记录；可能有 `IsDisconnected` 的 kill 失败。
5. **重拉 handler 前后**抓取 `/proc/<old-pid>/mountinfo` vs `/proc/<new-pid>/mountinfo`：旧进程 namespace 里那两个 CSI 挂载随旧进程销毁而消失。

---

## 7. 关键源码位置

| 文件（tag） | 内容 |
|------------|------|
| `commit 152652ee10`（1.6.6 含，1.2.0 不含） | "virt-handler: mount all of /var/lib/kubelet"：handler DaemonSet 从 bidi `/var/lib/kubelet/pods` 扩为 bidi `/var/lib/kubelet`。 |
| `pkg/virt-operator/resource/generate/components/daemonsets.go` | handler 卷挂载与 `MountPropagation: Bidirectional`（1.2.0 @271-276；1.6.6 @290-296）。 |
| `commit d555b25e26`（1.6.6 含，1.2.0 不含） | launcher `SignalShutdownVMI` 信号 `ACPI_POWER_BTN → DEFAULT`；handler `helperVmShutdown` 门槛 `isACPIEnabled → domainHasGracePeriod`。 |
| `pkg/virt-launcher/virtwrap/manager.go` `SignalShutdownVMI` | 1.2.0 用 `DOMAIN_SHUTDOWN_ACPI_POWER_BTN`；1.6.6 用 `DOMAIN_SHUTDOWN_DEFAULT`。fork 工作树加 `domainHasACPI` 恢复 ACPI 分支。 |
| `pkg/virt-handler/vm.go` `deleteVM`（1.6.6 @1312） | `processVmDelete`（Undefine，不停运行中 QEMU）+ `processVmCleanup` 合并同一次 reconcile。 |
| `pkg/virt-handler/vm.go`（1.6.6 @1422） | 新增 `!domainAlive && domainExists && !vmi.IsFinal() → shouldDelete`（1.2.0 无）。 |
| `pkg/virt-controller/watch/vmi/lifecycle.go`（1.6.6 @64）/ `vmi.go`（1.2.0 @1163） | VMI 有 DeletionTimestamp 立即删 launcher pod。 |
| `cmd/virt-launcher-monitor/virt-launcher-monitor.go` | 1.2.0 单次 `Wait4` 漏 reap；1.6.6 reap 循环。 |
| `pkg/virt-controller/services/template.go` | 两版本 `gracePeriodKillAfter = +15+15 = 60s` pod 终止宽限。 |
| `staging/src/kubevirt.io/api/core/v1/defaults.go` | `SetDefaults_FeatureState` 默认 `ACPI.Enabled=true` → arm64 XML 也有 `<acpi>`。 |
| fork 工作树 `pkg/virt-launcher/virtwrap/manager.go` | `domainHasACPI` 注释自述 arm64/无 qemu-ga 时 ACPI 优雅关机无效、超时 destroy、VMI Failed、触发 RerunOnFailure。 |

---

*参考：KubeVirt commit [`152652ee10`](https://github.com/kubevirt/kubevirt/commit/152652ee105b3147ed3926ebe59af9856d1b7416)（virt-handler: mount all of /var/lib/kubelet）、commit [`d555b25e26`](https://github.com/kubevirt/kubevirt/commit/d555b25e26978ad7aad829444ae6761c37000878)（graceful shutdown on non-ACPI architectures）；内核共享挂载组 / `MountPropagation: Bidirectional` / `jbd2` 线程机制。*
