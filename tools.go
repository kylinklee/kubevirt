//go:build tools

// [Go 1.22兼容] Go 1.24 使用 go.mod 中的 tool 指令声明工具依赖，
// Go 1.22 不支持 tool 指令，改用 tools.go + build tags 方式声明。
// 此文件不会被编译进任何二进制，仅用于将工具依赖纳入 go.mod 管理。

package kubevirt

import (
	_ "github.com/onsi/ginkgo/v2/ginkgo"
	_ "github.com/wadey/gocovmerge"
	_ "mvdan.cc/sh/v3/cmd/shfmt"
)
