# KubeVirt containerDisk 替代自研 Job 转换 ISO 方案分析（2026-08-19）

> 本文档基于 `v1.6.6`（commit `51395ba146`）源码，分析"用 containerDisk 替代生产环境自研的 Job 转换 ISO→qcow2 + PV 挂载为 cdrom 方案"的可行性、与原生设计哲学的差距，并附全部 v1.6.6 代码位置。
>
> **背景**：生产定制代码中，虚拟机操作系统镜像是这么处理的——virt-launcher 启动时另起一个 Job，Job 容器内专门把原始 `.iso` 文件转化为 qcow2 的 `disk.img`，然后把存放转化后文件的 PV 挂给 virt-launcher 作为 cdrom。
>
> **结论先行**：可以。containerDisk 更简单、更贴近原生设计哲学，且彻底规避了"PV 残留 / SAN 挂载 busy"这类自研方案长期踩的坑。唯一前提是明确 **disk.img 的语义**：一次性安装/引导介质 → 直接上 containerDisk；需要持久保留的系统盘 → 用 DataVolume/CDI（同样替代自研 Job，且原生）。

---

## 0. TL;DR

| # | 结论 |
|---|------|
| 1 | containerDisk 是 kubevirt 原生"把容器镜像当虚拟磁盘"的机制，专用于**一次性/可丢弃启动介质**（ISO 光盘、live CD、kernel/initrd）。 |
| 2 | 其核心是 **bind-mount 镜像根文件系统里的磁盘文件**直接给 QEMU（`getContainerDiskPath` → `ParentPathForRootMount` → `GetImage`），**不需要任何格式转换**；QEMU 原生可引导 `.iso`。 |
| 3 | 需要可写时由 virt-launcher 在 emptyDir 上建 **COW 覆盖层**（`CreateEphemeralImages`），基座保持只读，VMI 删除即丢——这是容器语义，**非持久化**。 |
| 4 | 自研 Job+PV 方案的可比开销：额外对象（Job/PVC/PV）、生命周期编排、iso→qcow2 转换、PV 清理与残留风险（正是本仓库此前《升降级删除 VM-PV 残留根因分析》的坑域）。containerDisk 全部免除。 |
| 5 | 若 disk.img 是**需要持久保留的系统盘**，containerDisk 不合适；原生替代是 **DataVolume/CDI**（vendor 内已含 `kubevirt.io/containerized-data-importer-api`），CDI 负责导入 ISO→PVC，同样免自研。 |

---

## 1. containerDisk 的使用场景

containerDisk（`ContainerDiskSource`）用于**一次性/无状态启动介质**：

- 启动光盘（ISO）镜像
- 无状态系统盘（live CD / 云镜像）
- kernel/initrd 引导（`kernelBoot.container`）

它**不承诺持久化**：磁盘基座只读，virt-launcher 用 COW 覆盖层承载写入，VMI 终止即丢。这与"镜像分发、镜像拉取、格式免转换"的容器哲学一致。

---

## 2. containerDisk 工作机制（v1.6.6 代码路径）

### 2.1 声明（VMI Spec）

`ContainerDiskSource` 字段（image / imagePullSecret / path / imagePullPolicy）：
`staging/src/kubevirt.io/api/core/v1/types.go`（`pkg/container-disk` 卷处理见下）。

磁盘文件在镜像内**约定放 `/disk`**，或由 `path` 指定任意位置：

```go
// pkg/os/disk/validation.go:12 （v1.6.6）
DiskSourceFallbackPath = "/disk"
```

### 2.2 virt-controller 渲染 launcher pod（sidecar/init 容器）

`pkg/virt-controller/services/template.go`：
- `:497` `containerdisk.GenerateContainers(...)` — 每个 containerDisk 卷生成同名 sidecar 容器
- `:586` `containerdisk.GenerateInitContainers(...)` — 同时生成 init 容器做**预拉取**（`:585` 注释："this causes containerDisks to be pre-pulled before virt-launcher starts"）

容器规格由 `pkg/container-disk/container-disk.go` 生成：
- `:204` `GenerateInitContainers` / `:208` `GenerateContainers`
- `:245` `generateContainersHelper` / `:260` `generateContainerFromVolume`
- sidecar 命令 `/usr/bin/container-disk --copy-path <dir>`，init 命令 `--no-op`

该二进制（`cmd/container-disk-v2alpha/main.c`）**不拷贝、不转换任何数据**，只创建一个 unix socket 作为"镜像根文件系统定位标记"，然后常驻等待：
- `:106-107` `--copy-path` / `--no-op` 选项
- `:151-181` bind/listen unix socket（路径为 `/pods/<uid>/.../container-disks/disk_N.sock`）

### 2.3 virt-handler：定位镜像根文件系统 → bind-mount 进 VM

`pkg/virt-handler/container-disk/mount.go`：
- `:611` `getContainerDiskPath`：
  - `:617` `DetectForSocket(vmi, sock)` — 通过 socket 定位容器进程
  - `:622` `ParentPathForRootMount(...)` — 拿到该镜像容器在宿主机上的 root mount（overlayfs）
  - `:627` `containerdisk.GetImage(mountPoint, volume.ContainerDisk.Path)` — 在镜像 fs 里定位磁盘文件
- `:292` bind-mount 到 launcher 视角路径 `/var/run/kubevirt/container-disks/disk_N.img`
  （`pkg/container-disk/container-disk.go:105` `GetDiskTargetPathFromLauncherView`）
- 挂载目标目录由 `pkg/container-disk/container-disk.go:68` `GetVolumeMountDirOnHost` 计算：
  - `:73` `${podsBaseDir}/${podUID}/volumes/kubernetes.io~empty-dir/container-disks`，`podsBaseDir=/var/lib/kubelet/pods`
  - `:84` `safepath.JoinAndResolveWithRelativeRoot("/", foundBasepath)` — 相对容器根解析

入口：`pkg/virt-handler/vm.go:2020-2029`（`ContainerDisksReady` → `MountAndVerify`）与 `pkg/virt-handler/migration-target.go:627-636`。

### 2.4 可写盘：COW 覆盖层（非持久化）

`pkg/container-disk/container-disk.go:351` `CreateEphemeralImages` → `virt-launcher` 在 emptyDir 上为每个 containerDisk 建 qcow2 覆盖层：
- `pkg/virt-launcher/virtwrap/manager.go:2674` `GetDiskTargetPathFromLauncherView(volumeIndex)` 作为 backing file
- `:1024` `GetDiskInfoWithValidation(...)` 探测磁盘格式

写入全部落在覆盖层，**基座镜像文件从未被写**，VMI 删除即丢。

### 2.5 可选新特性：ImageVolume（连 bind-mount 都省）

kubelet ≥1.28 支持 image volume 时，kubelet 直接把镜像挂给 launcher，virt-handler 不再需要 bind-mount：
- `pkg/virt-handler/container-disk/mount.go:228,365` `ImageVolumeEnabled()` + `needsBindMountFunc`
- `pkg/virt-launcher/virtwrap/manager.go:2759` `getDiskTargetPathFromImageVolumeView`

### 2.6 对照：hotplug 磁盘为什么不用挂载 `/var/lib/kubelet/pods`

`pkg/virt-handler/hotplug-disk/mount.go:44-46` 直接用 HostPID 前缀访问宿主机路径，**与 2.3 的容器内相对 `/` 解析形成对照**——这正是此前修复 commit（`2a65e63da7`/`e39201524a`）里 containerDisk 必须保留完整路径挂载的原因（见前文分析，`daemonsets.go:296` `kubelet-pods-full`）。

---

## 3. 自研 Job+PV 方案 vs containerDisk

自研方案（生产定制代码）：virt-launcher 启动时另起 **Job** → 容器内 **iso→qcow2** → 产物写 **PV** → PV 挂给 virt-launcher 当 **cdrom**。

| 维度 | 自研 Job+PV 方案 | containerDisk（原生） |
|---|---|---|
| 额外对象 | Job、PVC/PV；需自行协调与清理 | 无，仅 VMI spec 一个字段 |
| 格式转换 | 必须 iso→qcow2 | **不需要**；QEMU 直接引导 iso（`GetImage` 原样 bind-mount，见 2.3） |
| 调度顺序 | 需保证 Job 先于 launcher 完成 | init 容器预拉取，kubelet 保证顺序（`template.go:586`） |
| 镜像分发 | 手动搬文件到 PV | 标准镜像仓库 + imagePullSecrets / digest 锁定 / 预拉取 |
| 生命周期清理 | 手动清理 Job/PV；**正是"PV 残留 / SAN busy"问题的坑域** | 无 PV、无残留 |
| 磁盘语义 | 需自行定义 | 明确：只读基座 + COW 覆盖（2.4） |

### 关键点：iso→qcow2 转换是不必要的

- QEMU 原生支持 `.iso` 作为 cdrom 块设备，`pkg/os/disk` 中**无任何格式转换逻辑**；`GetImage` 只是把镜像内的文件 bind-mount 出来原样使用。
- 若用户想要 qcow2，直接在镜像内放置 qcow2 文件即可，同样原样 bind-mount。
- 因此"转换出 disk.img 再挂 cdrom"这步自研逻辑，在 containerDisk 下可以整段删除。

---

## 4. 关键 caveat：disk.img 是"安装介质"还是"系统盘"？

| disk.img 的角色 | 建议方案 |
|---|---|
| 一次性安装/引导介质（真正系统装到别的盘） | **containerDisk**，最简，见第 2 节 |
| 需要持久保留的系统盘（iso 转换出的 qcow2 即最终磁盘） | containerDisk **不合适**；原生替代 = **DataVolume/CDI**：CDI 导入 ISO→PVC（自动转换、处理 PVC 生命周期与保留策略） |

DataVolume 在 v1.6.6 已原生支持：
- vendor：`kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1`
- 消费方：`pkg/virt-controller/watch/vm/vm.go:64,88`（`cdiv1.DataVolume`、`CloneAuthFunc`）

---

## 5. 结论

1. containerDisk **可以且应当**替代自研 Job+PV 方案用于引导介质场景：声明式、免转换、免 PV、免清理、贴近原生哲学。
2. 自研方案里最沉重的两件事——**格式转换**与 **PV 生命周期管理**——在原生体系里分别由"QEMU 原生支持 iso / bind-mount 原样使用"和"容器镜像作为只读介质"天然解决。
3. 需要持久化时，迁移到 DataVolume/CDI 而非继续自研。

---

## 附录：v1.6.6 代码位置索引

| 主题 | 文件:行（v1.6.6 = commit `51395ba146`） |
|---|---|
| 镜像内磁盘默认路径 `/disk` | `pkg/os/disk/validation.go:12` |
| `podsBaseDir` / 挂载目标计算 | `pkg/container-disk/container-disk.go:46,68,73,84` |
| 容器根解析（对照 hotplug 的 `/proc/1/root`） | `pkg/container-disk/container-disk.go:84` vs `pkg/virt-handler/hotplug-disk/mount.go:45` |
| 定位镜像 fs 并 bind-mount | `pkg/virt-handler/container-disk/mount.go:611,617,622,627,292` |
| launcher 视角磁盘路径 | `pkg/container-disk/container-disk.go:105` |
| sidecar/init 容器生成 | `pkg/container-disk/container-disk.go:204,208,245,260` |
| launcher pod 渲染入口 | `pkg/virt-controller/services/template.go:497,586` |
| `container-disk` 二进制（仅建 socket，不拷贝不转换） | `cmd/container-disk-v2alpha/main.c:106-107,151-181` |
| COW 覆盖层（可写、非持久） | `pkg/container-disk/container-disk.go:351`；`pkg/virt-launcher/virtwrap/manager.go:1024,2674` |
| ImageVolume 免 bind-mount | `pkg/virt-handler/container-disk/mount.go:228,365`；`pkg/virt-launcher/virtwrap/manager.go:2759` |
| 调用入口 | `pkg/virt-handler/vm.go:2020-2029`；`pkg/virt-handler/migration-target.go:627-636` |
| `kubelet-pods-full` 完整路径挂载（containerDisk 必需） | `pkg/virt-operator/resource/generate/components/daemonsets.go:296`（本分支 `e39201524a`） |
| DataVolume/CDI（持久化替代） | `pkg/virt-controller/watch/vm/vm.go:64,88`；vendor `kubevirt.io/containerized-data-importer-api` |
