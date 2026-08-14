/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package virtconfig

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"

	k8sv1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/cache"

	clonev1 "kubevirt.io/api/clone/v1beta1"
	v1 "kubevirt.io/api/core/v1"
	exportv1 "kubevirt.io/api/export/v1beta1"
	instancetypev1 "kubevirt.io/api/instancetype/v1beta1"
	snapshotv1 "kubevirt.io/api/snapshot/v1beta1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/pointer"
)

const (
	NodeDrainTaintDefaultKey = "kubevirt.io/drain"
)

type ConfigModifiedFn func()

// NewClusterConfig is a wrapper of NewClusterConfigWithCPUArch with default cpuArch.
func NewClusterConfig(crdInformer cache.SharedIndexInformer,
	kubeVirtInformer cache.SharedIndexInformer,
	namespace string) (*ClusterConfig, error) {
	return NewClusterConfigWithCPUArch(
		crdInformer,
		kubeVirtInformer,
		namespace,
		runtime.GOARCH,
	)
}

// NewClusterConfigWithCPUArch represents the `kubevirt-config` config map. It can be used to live-update
// values if the config changes. The config update works like this:
// 1. Check if the config exists. If it does not exist, return the default config
// 2. Check if the config got updated. If so, try to parse and return it
// 3. In case of errors or no updates (resource version stays the same), it returns the values from the last good config
func NewClusterConfigWithCPUArch(crdInformer cache.SharedIndexInformer,
	kubeVirtInformer cache.SharedInformer,
	namespace, cpuArch string) (*ClusterConfig, error) {

	defaultConfig := defaultClusterConfig(cpuArch)

	c := &ClusterConfig{
		crdStore:        crdInformer.GetStore(),
		kubeVirtStore:   kubeVirtInformer.GetStore(),
		cpuArch:         cpuArch,
		lock:            &sync.Mutex{},
		namespace:       namespace,
		lastValidConfig: defaultConfig,
		defaultConfig:   defaultConfig,
	}

	_, err := crdInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.crdAddedDeleted,
		DeleteFunc: c.crdAddedDeleted,
		UpdateFunc: c.crdUpdated,
	})
	if err != nil {
		return nil, err
	}

	_, err = kubeVirtInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.configAddedDeleted,
		UpdateFunc: c.configUpdated,
	})
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *ClusterConfig) configAddedDeleted(_ interface{}) {
	go c.GetConfig()
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.configModifiedCallback != nil {
		for _, callback := range c.configModifiedCallback {
			go callback()
		}
	}
}

func (c *ClusterConfig) configUpdated(_, _ interface{}) {
	go c.GetConfig()
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.configModifiedCallback != nil {
		for _, callback := range c.configModifiedCallback {
			go callback()
		}
	}
}

func isDataVolumeCrd(crd *extv1.CustomResourceDefinition) bool {
	return crd.Spec.Names.Kind == "DataVolume"
}

func isDataSourceCrd(crd *extv1.CustomResourceDefinition) bool {
	return crd.Spec.Names.Kind == "DataSource"
}

func isServiceMonitor(crd *extv1.CustomResourceDefinition) bool {
	return crd.Spec.Names.Kind == "ServiceMonitor"
}

func isPrometheusRules(crd *extv1.CustomResourceDefinition) bool {
	return crd.Spec.Names.Kind == "PrometheusRule"
}

// [升级兼容] 判断 CRD 是否属于 snapshot.kubevirt.io 组
func isSnapshotCrd(crd *extv1.CustomResourceDefinition) bool {
	return crd.Spec.Group == "snapshot.kubevirt.io"
}

// [升级兼容] 判断 CRD 是否属于 export.kubevirt.io 组
func isExportCrd(crd *extv1.CustomResourceDefinition) bool {
	return crd.Spec.Group == "export.kubevirt.io"
}

// [升级兼容] 判断 CRD 是否属于 clone.kubevirt.io 组
func isCloneCrd(crd *extv1.CustomResourceDefinition) bool {
	return crd.Spec.Group == "clone.kubevirt.io"
}

// [升级兼容] 判断 CRD 是否属于 instancetype.kubevirt.io 组
func isInstancetypeCrd(crd *extv1.CustomResourceDefinition) bool {
	return crd.Spec.Group == "instancetype.kubevirt.io"
}

// [升级兼容] 判断 CRD 是否 served 指定版本
// v1.2.0 集群中 snapshot/export/clone CRD 只 serve v1alpha1，
// 而 v1.6.6 的 informer 使用 v1beta1 REST client。跳版本升级时
// 必须确认 CRD 已 serve v1beta1 才能安全创建对应 informer。
func crdServesVersion(crd *extv1.CustomResourceDefinition, version string) bool {
	for _, v := range crd.Spec.Versions {
		if v.Name == version && v.Served {
			return true
		}
	}
	return false
}

func (c *ClusterConfig) crdAddedDeleted(obj interface{}) {
	go c.GetConfig()
	crd := obj.(*extv1.CustomResourceDefinition)
	if !isDataVolumeCrd(crd) && !isDataSourceCrd(crd) &&
		!isServiceMonitor(crd) && !isPrometheusRules(crd) &&
		// [升级兼容] 跳版本升级时 snapshot/export/clone CRD 的出现/消失
		// 需要触发 virt-controller/virt-api 重新初始化以切换真实 informer
		!isSnapshotCrd(crd) && !isExportCrd(crd) && !isCloneCrd(crd) &&
		// [升级兼容] instancetype CRD 同样影响 instancetype informer 的真实/dummy 切换
		!isInstancetypeCrd(crd) {
		return
	}

	c.lock.Lock()
	defer c.lock.Unlock()
	if c.configModifiedCallback != nil {
		for _, callback := range c.configModifiedCallback {
			go callback()

		}
	}
}

func (c *ClusterConfig) crdUpdated(_, cur interface{}) {
	c.crdAddedDeleted(cur)
}

func defaultClusterConfig(cpuArch string) *v1.KubeVirtConfiguration {
	parallelOutboundMigrationsPerNodeDefault := ParallelOutboundMigrationsPerNodeDefault
	parallelMigrationsPerClusterDefault := ParallelMigrationsPerClusterDefault
	bandwidthPerMigrationDefault := resource.MustParse(BandwidthPerMigrationDefault)
	nodeDrainTaintDefaultKey := NodeDrainTaintDefaultKey
	allowAutoConverge := MigrationAllowAutoConverge
	allowPostCopy := MigrationAllowPostCopy
	defaultUnsafeMigrationOverride := DefaultUnsafeMigrationOverride
	progressTimeout := MigrationProgressTimeout
	completionTimeoutPerGiB := MigrationCompletionTimeoutPerGiB
	cpuRequestDefault := resource.MustParse(DefaultCPURequest)
	nodeSelectorsDefault, _ := parseNodeSelectors(DefaultNodeSelectors)
	defaultNetworkInterface := DefaultNetworkInterface
	defaultMemBalloonStatsPeriod := DefaultMemBalloonStatsPeriod
	SmbiosDefaultConfig := &v1.SMBiosConfiguration{
		Family:       SmbiosConfigDefaultFamily,
		Manufacturer: SmbiosConfigDefaultManufacturer,
		Product:      SmbiosConfigDefaultProduct,
	}
	supportedQEMUGuestAgentVersions := strings.Split(strings.TrimRight(SupportedGuestAgentVersions, ","), ",")
	defaultDiskVerification := &v1.DiskVerification{
		MemoryLimit: resource.NewQuantity(DefaultDiskVerificationMemoryLimitBytes, resource.BinarySI),
	}
	defaultEvictionStrategy := v1.EvictionStrategyNone

	return &v1.KubeVirtConfiguration{
		ImagePullPolicy: DefaultImagePullPolicy,
		DeveloperConfiguration: &v1.DeveloperConfiguration{
			UseEmulation:           DefaultAllowEmulation,
			MemoryOvercommit:       DefaultMemoryOvercommit,
			LessPVCSpaceToleration: DefaultLessPVCSpaceToleration,
			MinimumReservePVCBytes: DefaultMinimumReservePVCBytes,
			NodeSelectors:          nodeSelectorsDefault,
			CPUAllocationRatio:     DefaultCPUAllocationRatio,
			DiskVerification:       defaultDiskVerification,
			LogVerbosity: &v1.LogVerbosity{
				VirtAPI:        DefaultVirtAPILogVerbosity,
				VirtOperator:   DefaultVirtOperatorLogVerbosity,
				VirtController: DefaultVirtControllerLogVerbosity,
				VirtHandler:    DefaultVirtHandlerLogVerbosity,
				VirtLauncher:   DefaultVirtLauncherLogVerbosity,
			},
		},
		EvictionStrategy: &defaultEvictionStrategy,
		MigrationConfiguration: &v1.MigrationConfiguration{
			ParallelMigrationsPerCluster:      &parallelMigrationsPerClusterDefault,
			ParallelOutboundMigrationsPerNode: &parallelOutboundMigrationsPerNodeDefault,
			NodeDrainTaintKey:                 &nodeDrainTaintDefaultKey,
			BandwidthPerMigration:             &bandwidthPerMigrationDefault,
			ProgressTimeout:                   &progressTimeout,
			CompletionTimeoutPerGiB:           &completionTimeoutPerGiB,
			UnsafeMigrationOverride:           &defaultUnsafeMigrationOverride,
			AllowAutoConverge:                 &allowAutoConverge,
			AllowPostCopy:                     &allowPostCopy,
		},
		CPURequest: &cpuRequestDefault,
		NetworkConfiguration: &v1.NetworkConfiguration{
			NetworkInterface:                  defaultNetworkInterface,
			DeprecatedPermitSlirpInterface:    pointer.P(DefaultPermitSlirpInterface),
			PermitBridgeInterfaceOnPodNetwork: pointer.P(DefaultPermitBridgeInterfaceOnPodNetwork),
		},
		SMBIOSConfig:                SmbiosDefaultConfig,
		SELinuxLauncherType:         DefaultSELinuxLauncherType,
		SupportedGuestAgentVersions: supportedQEMUGuestAgentVersions,
		MemBalloonStatsPeriod:       &defaultMemBalloonStatsPeriod,
		APIConfiguration: &v1.ReloadableComponentConfiguration{
			RestClient: &v1.RESTClientConfiguration{RateLimiter: &v1.RateLimiter{TokenBucketRateLimiter: &v1.TokenBucketRateLimiter{
				QPS:   DefaultVirtAPIQPS,
				Burst: DefaultVirtAPIBurst,
			}}},
		},
		ControllerConfiguration: &v1.ReloadableComponentConfiguration{
			RestClient: &v1.RESTClientConfiguration{RateLimiter: &v1.RateLimiter{TokenBucketRateLimiter: &v1.TokenBucketRateLimiter{
				QPS:   DefaultVirtControllerQPS,
				Burst: DefaultVirtControllerBurst,
			}}},
		},
		HandlerConfiguration: &v1.ReloadableComponentConfiguration{
			RestClient: &v1.RESTClientConfiguration{RateLimiter: &v1.RateLimiter{TokenBucketRateLimiter: &v1.TokenBucketRateLimiter{
				QPS:   DefaultVirtHandlerQPS,
				Burst: DefaultVirtHandlerBurst,
			}}},
		},
		WebhookConfiguration: &v1.ReloadableComponentConfiguration{
			RestClient: &v1.RESTClientConfiguration{RateLimiter: &v1.RateLimiter{TokenBucketRateLimiter: &v1.TokenBucketRateLimiter{
				QPS:   DefaultVirtWebhookClientQPS,
				Burst: DefaultVirtWebhookClientBurst,
			}}},
		},
		ArchitectureConfiguration: &v1.ArchConfiguration{
			Amd64: &v1.ArchSpecificConfiguration{
				OVMFPath:         DefaultARCHOVMFPath,
				EmulatedMachines: strings.Split(DefaultAMD64EmulatedMachines, ","),
				MachineType:      DefaultAMD64MachineType,
			},
			Arm64: &v1.ArchSpecificConfiguration{
				OVMFPath:         DefaultAARCH64OVMFPath,
				EmulatedMachines: strings.Split(DefaultAARCH64EmulatedMachines, ","),
				MachineType:      DefaultAARCH64MachineType,
			},
			Ppc64le: &v1.ArchSpecificConfiguration{
				OVMFPath:         DefaultARCHOVMFPath,
				EmulatedMachines: strings.Split(DefaultPPC64LEEmulatedMachines, ","),
				MachineType:      DefaultPPC64LEMachineType,
			},
			DefaultArchitecture: runtime.GOARCH,
		},
		LiveUpdateConfiguration: &v1.LiveUpdateConfiguration{
			MaxHotplugRatio: DefaultMaxHotplugRatio,
		},
		VMRolloutStrategy: pointer.P(DefaultVMRolloutStrategy),
	}
}

type ClusterConfig struct {
	crdStore                         cache.Store
	kubeVirtStore                    cache.Store
	namespace                        string
	cpuArch                          string
	lock                             *sync.Mutex
	lastValidConfig                  *v1.KubeVirtConfiguration
	defaultConfig                    *v1.KubeVirtConfiguration
	lastInvalidConfigResourceVersion string
	lastValidConfigResourceVersion   string
	configModifiedCallback           []ConfigModifiedFn
}

func (c *ClusterConfig) SetConfigModifiedCallback(cb ConfigModifiedFn) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.configModifiedCallback = append(c.configModifiedCallback, cb)
	for _, callback := range c.configModifiedCallback {
		go callback()
	}
}

func setConfigFromKubeVirt(config *v1.KubeVirtConfiguration, kv *v1.KubeVirt) error {
	kvConfig := &kv.Spec.Configuration
	overrides, err := json.Marshal(kvConfig)
	if err != nil {
		return err
	}

	err = json.Unmarshal(overrides, &config)
	if err != nil {
		return err
	}

	if config.ArchitectureConfiguration == nil {
		config.ArchitectureConfiguration = &v1.ArchConfiguration{}
	}
	// set default architecture from status of CR
	config.ArchitectureConfiguration.DefaultArchitecture = kv.Status.DefaultArchitecture

	return validateConfig(config)
}

// getConfig returns the latest valid parsed config map result, or updates it
// if a newer version is available.
// XXX Rework this, to happen mostly in informer callbacks.
// This will also allow us then to react to config changes and e.g. restart some controllers
func (c *ClusterConfig) GetConfig() (config *v1.KubeVirtConfiguration) {
	c.lock.Lock()
	defer c.lock.Unlock()

	kv := c.GetConfigFromKubeVirtCR()
	if kv == nil {
		return c.lastValidConfig
	}

	resourceVersion := kv.ResourceVersion

	// if there is a configuration config map present we should use its configuration
	// and ignore configuration in kubevirt
	if c.lastValidConfigResourceVersion == resourceVersion ||
		c.lastInvalidConfigResourceVersion == resourceVersion {
		return c.lastValidConfig
	}

	config = defaultClusterConfig(c.cpuArch)
	err := setConfigFromKubeVirt(config, kv)
	if err != nil {
		c.lastInvalidConfigResourceVersion = resourceVersion
		log.DefaultLogger().Reason(err).Errorf("Invalid cluster config using KubeVirt resource version '%s', falling back to last good resource version '%s'", resourceVersion, c.lastValidConfigResourceVersion)
		return c.lastValidConfig
	}

	log.DefaultLogger().Infof("Updating cluster config from KubeVirt to resource version '%s'", resourceVersion)
	c.lastValidConfigResourceVersion = resourceVersion
	c.lastValidConfig = config
	return c.lastValidConfig
}

func (c *ClusterConfig) GetConfigFromKubeVirtCR() *v1.KubeVirt {
	objects := c.kubeVirtStore.List()
	var kubeVirtName string
	for _, obj := range objects {
		if kv, ok := obj.(*v1.KubeVirt); ok && kv.DeletionTimestamp == nil {
			if kv.Status.Phase != "" {
				kubeVirtName = kv.Name
			}
		}
	}

	if kubeVirtName == "" {
		return nil
	}

	if obj, exists, err := c.kubeVirtStore.GetByKey(c.namespace + "/" + kubeVirtName); err != nil {
		log.DefaultLogger().Reason(err).Errorf("Error loading the cluster config from KubeVirt cache, falling back to last good resource version '%s'", c.lastValidConfigResourceVersion)
		return nil
	} else if !exists {
		// this path should not be possible
		return nil
	} else {
		return obj.(*v1.KubeVirt)
	}
}

func (c *ClusterConfig) HasDataSourceAPI() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	objects := c.crdStore.List()
	for _, obj := range objects {
		if crd, ok := obj.(*extv1.CustomResourceDefinition); ok && crd.DeletionTimestamp == nil {
			if isDataSourceCrd(crd) {
				return true
			}
		}
	}
	return false
}

// [升级兼容] 检查 snapshot.kubevirt.io CRD 是否存在且 serve v1beta1
// 升级过程中 snapshot CRD 可能只有 v1alpha1（v1.2.0 集群），
// v1.6.6 的 informer 需要 v1beta1，virt-api/virt-controller 据此决定
// 是否创建真实 informer
func (c *ClusterConfig) HasSnapshotAPI() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	objects := c.crdStore.List()
	for _, obj := range objects {
		if crd, ok := obj.(*extv1.CustomResourceDefinition); ok && crd.DeletionTimestamp == nil {
			if isSnapshotCrd(crd) && crdServesVersion(crd, snapshotv1.SchemeGroupVersion.Version) {
				return true
			}
		}
	}
	return false
}

// [升级兼容] 检查 export.kubevirt.io CRD 是否存在且 serve v1beta1
func (c *ClusterConfig) HasExportAPI() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	objects := c.crdStore.List()
	for _, obj := range objects {
		if crd, ok := obj.(*extv1.CustomResourceDefinition); ok && crd.DeletionTimestamp == nil {
			if isExportCrd(crd) && crdServesVersion(crd, exportv1.SchemeGroupVersion.Version) {
				return true
			}
		}
	}
	return false
}

// [升级兼容] 检查 clone.kubevirt.io CRD 是否存在且 serve v1beta1
func (c *ClusterConfig) HasCloneAPI() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	objects := c.crdStore.List()
	for _, obj := range objects {
		if crd, ok := obj.(*extv1.CustomResourceDefinition); ok && crd.DeletionTimestamp == nil {
			if isCloneCrd(crd) && crdServesVersion(crd, clonev1.SchemeGroupVersion.Version) {
				return true
			}
		}
	}
	return false
}

// [升级兼容] 检查 instancetype.kubevirt.io CRD 是否存在且 serve v1beta1。
// 跳版本升级（v1.2.0→v1.6.6）中间态下该 CRD 可能不存在或只 serve v1alpha1，
// 此时硬编码 v1beta1 的 informer 其 ListWatch 会 404，reflector 卡在重试退避中。
// virt-controller 据此决定 instancetype informer 使用真实实现还是 dummy 实现。
func (c *ClusterConfig) HasInstancetypeAPI() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	objects := c.crdStore.List()
	for _, obj := range objects {
		if crd, ok := obj.(*extv1.CustomResourceDefinition); ok && crd.DeletionTimestamp == nil {
			if isInstancetypeCrd(crd) && crdServesVersion(crd, instancetypev1.SchemeGroupVersion.Version) {
				return true
			}
		}
	}
	return false
}

func (c *ClusterConfig) HasDataVolumeAPI() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	objects := c.crdStore.List()
	for _, obj := range objects {
		if crd, ok := obj.(*extv1.CustomResourceDefinition); ok && crd.DeletionTimestamp == nil {
			if isDataVolumeCrd(crd) {
				return true
			}
		}
	}
	return false
}

func (c *ClusterConfig) HasServiceMonitorAPI() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	objects := c.crdStore.List()
	for _, obj := range objects {
		if crd, ok := obj.(*extv1.CustomResourceDefinition); ok && crd.DeletionTimestamp == nil {
			if isServiceMonitor(crd) {
				return true
			}
		}
	}
	return false
}

func (c *ClusterConfig) HasPrometheusRuleAPI() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	objects := c.crdStore.List()
	for _, obj := range objects {
		if crd, ok := obj.(*extv1.CustomResourceDefinition); ok && crd.DeletionTimestamp == nil {
			if isPrometheusRules(crd) {
				return true
			}
		}
	}
	return false
}

func parseNodeSelectors(str string) (map[string]string, error) {
	nodeSelectors := make(map[string]string)
	for _, s := range strings.Split(strings.TrimSpace(str), "\n") {
		v := strings.Split(s, "=")
		if len(v) != 2 {
			return nil, fmt.Errorf("Invalid node selector: %s", s)
		}
		nodeSelectors[v[0]] = v[1]
	}
	return nodeSelectors, nil
}

func validateConfig(config *v1.KubeVirtConfiguration) error {
	// set image pull policy
	switch config.ImagePullPolicy {
	case "", k8sv1.PullAlways, k8sv1.PullNever, k8sv1.PullIfNotPresent:
		break
	default:
		return fmt.Errorf("invalid dev.imagePullPolicy in config: %v", config.ImagePullPolicy)
	}

	if config.DeveloperConfiguration.MemoryOvercommit <= 0 {
		return fmt.Errorf("invalid memoryOvercommit in ConfigMap: %d", config.DeveloperConfiguration.MemoryOvercommit)
	}

	if config.DeveloperConfiguration.CPUAllocationRatio <= 0 {
		return fmt.Errorf("invalid cpu allocation ratio in ConfigMap: %d", config.DeveloperConfiguration.CPUAllocationRatio)
	}

	if toleration := config.DeveloperConfiguration.LessPVCSpaceToleration; toleration < 0 || toleration > 100 {
		return fmt.Errorf("invalid lessPVCSpaceToleration in ConfigMap: %d", toleration)
	}

	// set default network interface
	switch config.NetworkConfiguration.NetworkInterface {
	case "", string(v1.BridgeInterface), string(v1.DeprecatedSlirpInterface), string(v1.MasqueradeInterface):
		break
	default:
		return fmt.Errorf("invalid default-network-interface in config: %v", config.NetworkConfiguration.NetworkInterface)
	}

	return nil
}
