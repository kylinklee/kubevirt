// [K8s 1.31兼容] 此文件中的 mock 辅助代码依赖 generated_mock_kubevirt.go（已跳过编译），
// 生产用的 NewMinimal* 等函数已拆分到 kubevirt_helpers.go。
//go:build ignore

package kubecli


// GetMockKubevirtClientFromClientConfig, MockKubevirtClientInstance are used to create a mechanism
// (1) Override the GetKubevirtClientFromClientConfig() closure with GetMockKubevirtClientFromClientConfig() or
//     GetInvalidKubevirtClientFromClientConfig()
// (2) Then create the instance of the client, and assign into MockKubevirtClientInstance before the tests start:
//     kubecli.MockKubevirtClientInstance = kubecli.NewMockKubevirtClient(ctrl)
// MockKubevirtClientInstance is a reference to the kubevirt client that could be manipulated by the test code
var MockKubevirtClientInstance *MockKubevirtClient
// GetMockKubevirtClientFromClientConfig is an entry point for testing, could be used to override GetKubevirtClientFromClientConfig
func GetMockKubevirtClientFromClientConfig(cmdConfig clientcmd.ClientConfig) (KubevirtClient, error) {
	return MockKubevirtClientInstance, nil
// GetInvalidKubevirtClientFromClientConfig is an entry point for testing case where client should be invalid
func GetInvalidKubevirtClientFromClientConfig(cmdConfig clientcmd.ClientConfig) (KubevirtClient, error) {