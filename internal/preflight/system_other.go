//go:build !windows

package preflight

func detectSystemProxy() systemProxyStatus {
	return systemProxyStatus{Message: "当前平台暂未实现系统代理读取，仅使用抽样测速判断"}
}
