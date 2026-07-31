# KubeVirt v1.8.1 兼容 K8s 1.31 修改说明

## 背景

KubeVirt v1.8.1 官方仅支持 K8s 1.33-1.35。本分支通过源码修改使其可在 K8s 1.31 上编译和运行。

## 编译状态

| 组件 | 状态 | 备注 |
|------|------|------|
| cmd/virt-operator | ✅ | 核心组件 |
| cmd/virt-controller | ✅ | 核心组件 |
| cmd/virt-handler | ✅ | 核心组件 |
| cmd/virt-api | ✅ | 核心组件 |
| cmd/virtctl | ❌ | `virt-template-client-go` 需适配 `CodecFactoryForGeneratedClient` |
| cmd/virt-launcher | ❌ | 需 CGO + libnbd，需在 Linux 构建机上编译 |

## 修改清单（13 个文件）

### 1. go.mod 依赖降级

**变更：** k8s.io/* 从 v0.34.2 降至 v0.31.0

| 依赖 | 原版本 | 新版本 |
|------|--------|--------|
| k8s.io/* | v0.34.2 (K8s 1.34) | v0.31.0 (K8s 1.31) |
| structured-merge-diff | v6.3.0 | v4.4.1 / v4.5.0 |
| controller-runtime | v0.22.4 | v0.19.3 |
| kube-openapi | 2025-07-10 | 2024-02-28 |
| go-openapi/swag | v0.25.4 | v0.22.3 (via replace) |
| gnostic-models | v0.7.1 | v0.6.8 (via replace) |

**文件：** `go.mod`, `go.work`, `staging/src/kubevirt.io/api/go.mod`, `staging/src/kubevirt.io/client-go/go.mod`

### 2. DRA 模块适配

K8s 1.33 引入 `resource/v1`（GA），K8s 1.31 只有 `resource/v1alpha3`。

**`pkg/dra/metadata/types.go`：**
- 移除 `import resourcev1 "k8s.io/api/resource/v1"`
- 定义本地类型替代：
  - `QualifiedName` (string 别名)
  - `DeviceAttribute` (含 StringValue/BoolValue/IntValue)
  - `NetworkDeviceData` (含 IPAddresses)

**`tools/vms-generator/`：**
- `resource/v1` → `resource/v1alpha3`
- `ExactDeviceRequest` 结构适配 v1alpha3 格式

### 3. 客户端 API 适配

**`rest.CodecFactoryForGeneratedClient` → `scheme.Codecs.WithoutConversion()`**

该函数在 K8s 1.33+ 的 client-go 中引入。K8s 1.31 使用 `scheme.Codecs.WithoutConversion()` 模式。

影响文件（17 个 typed client）：
```
staging/src/kubevirt.io/client-go/kubevirt/typed/*/..._client.go
staging/src/kubevirt.io/client-go/containerizeddataimporter/typed/*/..._client.go
staging/src/kubevirt.io/client-go/externalsnapshotter/typed/*/..._client.go
staging/src/kubevirt.io/client-go/networkattachmentdefinitionclient/typed/*/..._client.go
staging/src/kubevirt.io/client-go/prometheusoperator/typed/*/..._client.go
```

**generated_mock_kubevirt.go：**
- 移除 K8s 1.34 专属方法：`ResourceV1()`, `ResourceV1beta1()`, `ResourceV1beta2()`, `CoordinationV1alpha2()`
- 添加 K8s 1.31 需要的 `CoordinationV1alpha1()`

**kubevirt_test_utils.go：**
- 生产代码（`NewMinimal*` 函数）拆分到 `kubevirt_helpers.go`
- mock 相关代码标记 `//go:build ignore`（mock 针对 K8s 1.34 生成，与 1.31 的 `kubernetes.Interface` 不兼容）

### 4. API 签名适配

**`cache.DefaultWatchErrorHandler`：**
- K8s 1.34: `func(ctx context.Context, r *Reflector, err error)`
- K8s 1.31: `func(r *Reflector, err error)`（无 context 参数）

影响文件：
- `pkg/virt-controller/watch/application.go`
- `pkg/virt-api/api.go`
- `cmd/virt-handler/virt-handler.go`

## 已禁用的特性

| 特性 | Feature Gate | 原因 |
|------|-------------|------|
| DRA GPU 透传 | GPUsWithDRA | 需要 K8s 1.33+ resource/v1 |
| DRA HostDevice 透传 | HostDevicesWithDRA | 需要 K8s 1.33+ resource/v1 |
| ImageVolume 容器磁盘 | ImageVolume | 需要 K8s 1.33+ ImageVolume API |

这些 Feature Gate 在 K8s 1.31 上默认不会激活，不会影响正常功能。

## 升级操作指南

### 前提条件

- K8s 1.31 集群
- 已安装 KubeVirt v1.2.x 或更早版本

### 升级步骤

```bash
# 1. 备份
kubectl get kubevirt kubevirt -n kubevirt -o yaml > kv-backup.yaml
kubectl get vm,vmi,dv,pvc --all-namespaces -o yaml > workloads-backup.yaml

# 2. 构建镜像（在 Linux 构建机上）
git checkout k8s131-compat-v1.8.1
GOWORK=off GOFLAGS=-mod=mod GOOS=linux CGO_ENABLED=0 go build -o _out/virt-operator ./cmd/virt-operator
GOWORK=off GOFLAGS=-mod=mod GOOS=linux CGO_ENABLED=0 go build -o _out/virt-controller ./cmd/virt-controller
GOWORK=off GOFLAGS=-mod=mod GOOS=linux CGO_ENABLED=0 go build -o _out/virt-handler ./cmd/virt-handler
GOWORK=off GOFLAGS=-mod=mod GOOS=linux CGO_ENABLED=0 go build -o _out/virt-api ./cmd/virt-api
# virt-launcher 需要 CGO
CGO_ENABLED=1 go build -o _out/virt-launcher ./cmd/virt-launcher

# 3. 构建并推送容器镜像
# 根据项目的 docker/Makefile 构建镜像并推送到私有 registry
make docker

# 4. 更新 KubeVirt CR
kubectl patch kubevirt kubevirt -n kubevirt --type=json \
  -p '[{"op": "add", "path": "/spec/imageTag", "value": "v1.8.1-k8s131"}]'

# 5. 等待升级完成
kubectl wait kubevirt kubevirt -n kubevirt --for=condition=Available --timeout=600s

# 6. 验证
kubectl get pods -n kubevirt
kubectl get kubevirt kubevirt -n kubevirt
```

### 运行时验证清单

```bash
# VM 创建和启动
virtctl run test-vm --image=cirros-container-disk-demo --namespace=default
virtctl start test-vm

# Live Migration
virtctl migrate test-vm

# 快照
kubectl apply -f - <<EOF
apiVersion: snapshot.kubevirt.io/v1beta1
kind: VirtualMachineSnapshot
metadata:
  name: test-snapshot
spec:
  source:
    apiGroup: kubevirt.io
    kind: VirtualMachine
    name: test-vm
EOF

# 清理
virtctl stop test-vm
virtctl delete vm test-vm
```

## 风险评估

| 风险 | 等级 | 说明 |
|------|------|------|
| client-go v0.31.0 与 K8s 1.31 apiserver | 🟢 低 | 版本完全匹配 |
| CRD schema 向后兼容 | 🟢 低 | v1alpha1 端口仍可用 |
| DRA/ImageVolume 禁用 | 🟢 低 | 新特性，不影响存量 VM |
| 结构化合并差异 v4 vs v6 | 🟡 中 | SSA 行为可能有细微差异 |
| 未测试的功能路径 | 🟡 中 | 需完整功能回归测试 |

## 后续工作

1. **virtctl 适配**：fork `virt-template-client-go` 并应用相同的 `CodecFactoryForGeneratedClient` 修改
2. **完整功能测试**：在 K8s 1.31 集群上进行端到端测试
3. **CI 适配**：在 CI 中添加 K8s 1.31 测试矩阵
4. **上游贡献**：如需长期维护，考虑向 KubeVirt 上游贡献 K8s 1.31 兼容性支持
