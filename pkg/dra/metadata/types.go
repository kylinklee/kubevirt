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

package metadata

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// [K8s 1.31兼容] 以下三个类型原本从 k8s.io/api/resource/v1 导入（K8s 1.33+ GA），
// K8s 1.31 中 resource/v1 不存在（只有 v1alpha3），因此在本地重新定义，
// 字段结构与上游 resource/v1 保持一致。

// QualifiedName 是设备属性的限定名称（原 resource/v1 类型，K8s 1.31 不可用）
type QualifiedName string

// DeviceAttribute 表示单个设备属性（原 resource/v1 类型，K8s 1.31 不可用）
type DeviceAttribute struct {
	StringValue *string `json:"stringValue,omitempty"`
	BoolValue   *bool   `json:"boolValue,omitempty"`
	IntValue    *int64  `json:"intValue,omitempty"`
}

// NetworkDeviceData 包含网络设备元数据（原 resource/v1 类型，K8s 1.31 不可用）
type NetworkDeviceData struct {
	IPAddresses []string `json:"ipAddresses,omitempty"`
}

// DeviceMetadata contains metadata about devices allocated to a ResourceClaim.
// It is serialized to versioned JSON files that can be mounted into containers.
// These types mirror the KEP-5304 v1alpha1 API in k8s.io/dynamic-resource-allocation.
type DeviceMetadata struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// PodClaimName is only present for template-generated claims.
	// For pre-existing claims (resourceClaimName), this field is absent.
	PodClaimName *string                 `json:"podClaimName,omitempty"`
	Requests     []DeviceMetadataRequest `json:"requests,omitempty"`
}

// DeviceMetadataRequest contains metadata for a single request within a ResourceClaim.
type DeviceMetadataRequest struct {
	Name    string   `json:"name"`
	Devices []Device `json:"devices,omitempty"`
}

// Device contains metadata about a single allocated device.
type Device struct {
	Driver      string                          `json:"driver"`
	Pool        string                          `json:"pool"`
	Name        string                          `json:"name"`
	Attributes  map[QualifiedName]DeviceAttribute `json:"attributes,omitempty"`
	NetworkData *NetworkDeviceData              `json:"networkData,omitempty"`
}

// Well-known attribute keys for device identification
const (
	// PCIBusIDAttribute is the standard attribute for PCI device address (passthrough GPUs)
	PCIBusIDAttribute = QualifiedName("resource.kubernetes.io/pciBusID")
	// MDevUUIDAttribute is the attribute for mediated device UUID (vGPUs)
	// Note: This is not yet standardized under resource.kubernetes.io
	MDevUUIDAttribute = QualifiedName("mdevUUID")
)
