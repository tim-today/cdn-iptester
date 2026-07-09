package desktop

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/grootpxw/edgetunnel-bestsub/internal/app"
	"github.com/grootpxw/edgetunnel-bestsub/internal/config"
	"github.com/grootpxw/edgetunnel-bestsub/internal/geoipdata"
	"github.com/grootpxw/edgetunnel-bestsub/internal/web"
)

const appName = "CDN-IPtester"

const defaultConfigTemplate = `server:
    listen: "127.0.0.1:8788"

worker:
    base_url: "https://your-worker.workers.dev"
    password: "your_password"
    user_agent: "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"

probe:
    target:
        mode: "worker"
        url: "https://your-worker.workers.dev/robots.txt"
        host: "your-worker.workers.dev"
        sni: "your-worker.workers.dev"
        method: "HEAD"
        expected_status: [200, 204, 301, 302, 403, 404]
    preflight:
        enabled: true
        disable_sample_probe: true
        sample_size: 8
        block_on_low_latency: true
        low_latency_threshold_ms: 20
    ipv4: true
    ipv6: false
    countries: []
    require_geoip_match: false
    geoip_db_path: ""
    ports: [443, 2053, 2083, 2087, 2096, 8443]
    candidate_limit: 600
    keep: 30
    timeout_ms: 2500
    concurrency: 120
    per_cidr24_limit: 2

sources:
    - type: "remote"
      name: "cf-ipv4"
      url: "https://www.cloudflare.com/ips-v4"
      path: ""
      weight: 130
    - type: "remote"
      name: "cmliu-cf-cidr"
      url: "https://raw.githubusercontent.com/cmliu/cmliu/main/CF-CIDR.txt"
      path: ""
      weight: 120
    - type: "remote"
      name: "xiu2-ipv4"
      url: "https://raw.githubusercontent.com/XIU2/CloudflareSpeedTest/master/ip.txt"
      path: ""
      weight: 100
    - type: "file"
      name: "local-seeds"
      url: ""
      path: "seeds.txt"
      weight: 200

output:
    path: "CDN-IPtester.txt"
    remark_prefix: "IP 官方优选"
    dry_run: false

clash:
    proxyip_auto:
        enabled: true
        country: "JP"
        limit: 8
        max_candidates: 50
        source_url: "https://zip.cm.edu.kg/all.txt"
        check_api: "https://api.090227.xyz/check?proxyip=%s"
        concurrency: 2
        require_geoip_match: false
        geoip_db_path: "GeoLite2-Country.mmdb"
        worker_verify:
            enabled: true
            url: "https://cloudflare.com/cdn-cgi/trace"
            max_checks: 24
`

func Main() {
	configPath := flag.String("config", defaultConfigPath(), "config file path")
	run := flag.Bool("run", false, "run probe once and exit")
	push := flag.Bool("push", false, "push ADD.txt to worker after probing; ignored when output.dry_run=true")
	webOnly := flag.Bool("web", false, "start http server only, without desktop window")
	jsonOut := flag.Bool("json", false, "print JSON result when used with -run")
	flag.Parse()

	resolvedConfigPath, err := resolveConfigPath(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		waitBeforeExit()
		os.Exit(1)
	}

	cfg, err := config.Load(resolvedConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败：%v\n", err)
		waitBeforeExit()
		os.Exit(1)
	}
	if err := ensureBundledGeoIPDB(resolvedConfigPath); err != nil {
		log.Printf("释放内置 GeoIP 数据库失败: %v", err)
	}

	if *run {
		if err := runOnce(cfg, *push, *jsonOut); err != nil {
			log.Fatal(err)
		}
		return
	}

	if *webOnly {
		startHTTPOnly(resolvedConfigPath, cfg)
		return
	}

	startDesktopTool(resolvedConfigPath, cfg)
}

func ensureBundledGeoIPDB(configPath string) error {
	path, err := geoipdata.EnsureCountryDB(filepath.Dir(configPath))
	if err == nil {
		log.Printf("[startup] GeoIP 数据库就绪: %s", path)
	}
	return err
}

func resolveConfigPath(path string) (string, error) {
	if path == defaultConfigPath() {
		return ensureDefaultConfig(path)
	}
	candidates := []string{path}
	if !filepath.IsAbs(path) {
		if exeDir, err := executableDir(); err == nil {
			candidates = append(candidates, filepath.Join(exeDir, path))
		}
	}
	for _, candidate := range candidates {
		if fileExists(candidate) {
			return candidate, nil
		}
	}

	if filepath.Clean(path) == filepath.Clean(defaultConfigPath()) {
		for _, examplePath := range candidateExamplePaths() {
			if fileExists(examplePath) {
				fmt.Fprintf(os.Stderr, "未找到 %s，已临时使用 %s。\n", path, examplePath)
				fmt.Fprintln(os.Stderr, "建议复制示例配置为 configs/config.yaml，并填写你的 Worker 信息。")
				return examplePath, nil
			}
		}
	}
	return "", fmt.Errorf("未找到配置文件：%s\n请复制 configs/config.example.yaml 为 configs/config.yaml 后再运行。", path)
}

func defaultConfigPath() string {
	if appDir, err := appConfigDir(); err == nil {
		return filepath.Join(appDir, "config.yaml")
	}
	return filepath.Join("configs", "config.yaml")
}

func candidateExamplePaths() []string {
	out := []string{filepath.Join("configs", "config.example.yaml")}
	if appDir, err := appConfigDir(); err == nil {
		out = append(out, filepath.Join(appDir, "config.example.yaml"))
	}
	if exeDir, err := executableDir(); err == nil {
		out = append(out, filepath.Join(exeDir, "configs", "config.example.yaml"))
	}
	return out
}

func appConfigDir() (string, error) {
	baseDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(baseDir, appName), nil
}

func ensureDefaultConfig(path string) (string, error) {
	if fileExists(path) {
		return path, nil
	}
	configDir := filepath.Dir(path)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(defaultConfigTemplate), 0644); err != nil {
		return "", err
	}
	seedsPath := filepath.Join(configDir, "seeds.txt")
	if !fileExists(seedsPath) {
		_ = os.WriteFile(seedsPath, []byte(strings.TrimSpace("# 每行一个 IP、IP:端口 或 CIDR\n")), 0644)
	}
	fmt.Fprintf(os.Stderr, "首次运行已创建默认配置：%s\n", path)
	return path, nil
}

func executableDir() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err == nil {
		path = realPath
	}
	return filepath.Dir(path), nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func waitBeforeExit() {
	if len(os.Args) > 1 {
		return
	}
	fmt.Fprintln(os.Stderr, "按回车键退出...")
	_, _ = fmt.Fscanln(os.Stdin)
}

func runOnce(cfg config.Config, push bool, jsonOut bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	result, err := app.RunOnce(ctx, cfg, push)
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	if result.Preflight != nil && result.Preflight.Blocked {
		fmt.Println("preflight: blocked")
		for _, check := range result.Preflight.Checks {
			fmt.Printf("- [%s] %s: %s\n", check.Severity, check.Name, check.Message)
		}
		return nil
	}
	fmt.Printf("candidates: %d\n", result.Candidates)
	fmt.Printf("top: %d\n", len(result.Top))
	fmt.Printf("output: %s\n", result.OutputPath)
	if result.Pushed {
		fmt.Println("pushed: true")
	}
	if result.PushError != "" {
		fmt.Printf("push_error: %s\n", result.PushError)
	}
	return nil
}

func startDesktopTool(configPath string, cfg config.Config) {
	server := web.New(configPath, cfg)
	err := wails.Run(&options.App{
		Title:             "CDN-IPtester",
		Width:             980,
		Height:            640,
		MinWidth:          900,
		MinHeight:         600,
		HideWindowOnClose: false,
		AssetServer: &assetserver.Options{
			Handler: server.Handler(),
		},
		Mac: &mac.Options{
			TitleBar: mac.TitleBarDefault(),
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			DisableWindowIcon:    false,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}

func startHTTPOnly(configPath string, cfg config.Config) {
	server := web.New(configPath, cfg)
	httpServer := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("debug http ui: http://%s", cfg.Server.Listen)
	log.Fatal(httpServer.ListenAndServe())
}
