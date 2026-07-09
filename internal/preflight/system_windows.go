package preflight

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

func detectSystemProxy() systemProxyStatus {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return systemProxyStatus{Message: "无法读取 Windows 系统代理设置: " + err.Error()}
	}
	defer key.Close()

	proxyEnable, _, _ := key.GetIntegerValue("ProxyEnable")
	proxyServer, _, _ := key.GetStringValue("ProxyServer")
	autoConfigURL, _, _ := key.GetStringValue("AutoConfigURL")

	var parts []string
	if proxyEnable != 0 {
		server := strings.TrimSpace(proxyServer)
		if server == "" {
			server = "已启用但未读取到服务器地址"
		}
		parts = append(parts, "手动代理="+server)
	}
	if strings.TrimSpace(autoConfigURL) != "" {
		parts = append(parts, "PAC="+autoConfigURL)
	}

	if len(parts) > 0 {
		return systemProxyStatus{
			Enabled: true,
			Message: "检测到 Windows 系统代理开启: " + strings.Join(parts, "; "),
		}
	}
	return systemProxyStatus{Message: fmt.Sprintf("Windows 系统代理未开启")}
}
