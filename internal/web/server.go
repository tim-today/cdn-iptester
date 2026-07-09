package web

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grootpxw/edgetunnel-bestsub/internal/app"
	"github.com/grootpxw/edgetunnel-bestsub/internal/config"
	"github.com/grootpxw/edgetunnel-bestsub/internal/preflight"
	"github.com/grootpxw/edgetunnel-bestsub/internal/probe"
	"github.com/grootpxw/edgetunnel-bestsub/internal/worker"
)

func init() {
	// 强制纠正 Windows 下可能错误的 MIME 类型
	mime.AddExtensionType(".js", "application/javascript; charset=utf-8")
	mime.AddExtensionType(".css", "text/css; charset=utf-8")
	mime.AddExtensionType(".woff2", "font/woff2")
}

//go:embed static
var staticFS embed.FS

type Server struct {
	configPath string
	cfg        config.Config
	mu         sync.Mutex
	running    bool
	last       *app.RunResult
	lastError  string
}

func New(configPath string, cfg config.Config) *Server {
	s := &Server{configPath: configPath, cfg: cfg}
	s.loadADDFromDisk()
	return s
}

// loadADDFromDisk 启动时从本地 ADD.txt 恢复上次结果，避免重启后必须重新测速。
func (s *Server) loadADDFromDisk() {
	path := s.cfg.Output.Path
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var results []probe.Result
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		result := parseADDLine(line)
		if result.IP != "" && result.Port > 0 {
			results = append(results, result)
		}
	}
	if len(results) == 0 {
		return
	}
	addText := probe.FormatADD(results, s.cfg.Output.RemarkPrefix)
	s.last = &app.RunResult{
		Top:        results,
		ADDText:    addText,
		OutputPath: path,
	}
	// 恢复 PROXYIP.txt
	proxyIPPath := proxyIPFilePath(path)
	if data, err := os.ReadFile(proxyIPPath); err == nil {
		content := strings.TrimSpace(string(data))
		if content != "" {
			s.last.AutoProxyIPs = content
		}
	}
	log.Printf("[startup] 从 %s 恢复了 %d 条历史结果", path, len(results))
	if s.last.AutoProxyIPs != "" {
		log.Printf("[startup] 从 %s 恢复了反代 IP: %s", proxyIPPath, s.last.AutoProxyIPs)
	}
}

// proxyIPFilePath 根据结果路径推导反代 IP 结果路径
func proxyIPFilePath(addPath string) string {
	dir := ""
	if idx := strings.LastIndexAny(addPath, "/\\"); idx >= 0 {
		dir = addPath[:idx+1]
	}
	return dir + "PROXYIP-tester.txt"
}

func (s *Server) saveProxyIPToDisk(content string) {
	path := proxyIPFilePath(s.cfg.Output.Path)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		log.Printf("[proxyip] 保存 %s 失败: %v", path, err)
	} else {
		log.Printf("[proxyip] 已保存反代 IP 到 %s", path)
	}
}

// parseADDLine 解析 ADD.txt 的一行，格式: IP:端口#备注 或 [IPv6]:端口#备注
func parseADDLine(line string) probe.Result {
	address, remark, _ := strings.Cut(line, "#")
	address = strings.TrimSpace(address)
	remark = strings.TrimSpace(remark)

	var ip string
	var port int

	if strings.HasPrefix(address, "[") {
		// IPv6: [addr]:port
		end := strings.LastIndex(address, "]:")
		if end < 0 {
			return probe.Result{}
		}
		ip = address[1:end]
		p, err := strconv.Atoi(address[end+2:])
		if err != nil || p <= 0 {
			return probe.Result{}
		}
		port = p
	} else {
		// IPv4: addr:port
		host, portStr, ok := strings.Cut(address, ":")
		if !ok {
			return probe.Result{}
		}
		p, err := strconv.Atoi(portStr)
		if err != nil || p <= 0 {
			return probe.Result{}
		}
		ip = host
		port = p
	}

	// 验证 IP 合法性
	if _, err := netip.ParseAddr(ip); err != nil {
		return probe.Result{}
	}

	return probe.Result{
		IP:      ip,
		Port:    port,
		Remark:  remark,
		Success: true,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/results/latest", s.handleLatest)
	mux.HandleFunc("/api/results/cdn-iptester.txt", s.handleADD)
	mux.HandleFunc("/api/ips/sources", s.handleIPSources)
	mux.HandleFunc("/api/ips/sources/save", s.handleIPSourcesSave)
	mux.HandleFunc("/api/ips/update", s.handleIPUpdate)
	mux.HandleFunc("/api/ips/preview", s.handleIPPreview)
	mux.HandleFunc("/api/config/update", s.handleConfigUpdate)
	mux.HandleFunc("/api/preflight", s.handlePreflight)
	mux.HandleFunc("/api/probe/run", s.handleRun)
	mux.HandleFunc("/api/proxyip/fetch", s.handleProxyIPFetch)
	mux.HandleFunc("/api/worker/push", s.handlePush)
	mux.HandleFunc("/api/worker/proxyip", s.handleProxyIPPush)
	mux.HandleFunc("/", s.handleIndex)

	mux.Handle("/static/", http.FileServer(http.FS(staticFS)))
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"config_path": s.configPath,
		"config":      s.cfg,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, map[string]any{
		"running":    s.running,
		"last_error": s.lastError,
		"has_result": s.last != nil,
		"last_candidates": func() int {
			if s.last == nil {
				return 0
			}
			return s.last.Candidates
		}(),
		"last_success": func() int {
			if s.last == nil {
				return 0
			}
			count := 0
			for _, r := range s.last.Results {
				if r.Success {
					count++
				}
			}
			return count
		}(),
	})
}

func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		http.Error(w, "no result yet", http.StatusNotFound)
		return
	}
	writeJSON(w, s.last)
}

func (s *Server) handleADD(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		http.Error(w, "no result yet", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s.last.ADDText))
}

type IPSource struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	UpdateTime string `json:"update_time"`
	Exists     bool   `json:"exists"`
}

type IPSourceConfig struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

func (s *Server) getIPSourcePath(id string) string {
	baseDir := filepath.Dir(s.configPath)
	return filepath.Join(baseDir, "ip_sources", id+".txt")
}

func (s *Server) getIPSourcesConfigPath() string {
	baseDir := filepath.Dir(s.configPath)
	return filepath.Join(baseDir, "ip_sources.json")
}

func (s *Server) loadIPSourcesConfig() ([]IPSourceConfig, error) {
	path := s.getIPSourcesConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			defaults := []IPSourceConfig{
				{ID: "cf-v4", Name: "官方v4", URL: "https://www.cloudflare.com/ips-v4"},
				{ID: "cf-v4-backup", Name: "官方v4备用", URL: "https://cf.090227.xyz/ips-v4"},
				{ID: "cm-best", Name: "CM优选", URL: "https://raw.githubusercontent.com/cmliu/cmliu/main/CF-CIDR.txt"},
				{ID: "as13335-v4", Name: "AS1335-V4", URL: "https://raw.githubusercontent.com/ipverse/asn-ip/master/as/13335/ipv4-aggregated.txt"},
				{ID: "as209242-v4", Name: "AS209242-v4", URL: "https://raw.githubusercontent.com/ipverse/asn-ip/master/as/209242/ipv4-aggregated.txt"},
			}
			err = s.saveIPSourcesConfig(defaults)
			return defaults, err
		}
		return nil, err
	}
	var list []IPSourceConfig
	err = json.Unmarshal(data, &list)
	return list, err
}

func (s *Server) saveIPSourcesConfig(list []IPSourceConfig) error {
	path := s.getIPSourcesConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}



func (s *Server) handleIPSources(w http.ResponseWriter, r *http.Request) {
	cfgList, err := s.loadIPSourcesConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var list []IPSource
	for _, src := range cfgList {
		path := s.getIPSourcePath(src.ID)
		info, err := os.Stat(path)
		exists := false
		updateTime := "从未更新"
		if err == nil && !info.IsDir() {
			exists = true
			updateTime = info.ModTime().Format("2006-01-02 15:04:05")
		}
		list = append(list, IPSource{
			ID:         src.ID,
			Name:       src.Name,
			URL:        src.URL,
			UpdateTime: updateTime,
			Exists:     exists,
		})
	}
	writeJSON(w, list)
}

func (s *Server) handleIPSourcesSave(w http.ResponseWriter, r *http.Request) {
	var list []IPSourceConfig
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	oldList, err := s.loadIPSourcesConfig()
	if err == nil {
		newIds := make(map[string]bool)
		for _, item := range list {
			newIds[item.ID] = true
		}
		for _, oldItem := range oldList {
			if !newIds[oldItem.ID] {
				path := s.getIPSourcePath(oldItem.ID)
				_ = os.Remove(path)
			}
		}
	}

	if err := s.saveIPSourcesConfig(list); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{"success": true})
}

func (s *Server) handleIPUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	cfgList, err := s.loadIPSourcesConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var target *IPSourceConfig
	for _, src := range cfgList {
		if src.ID == id {
			target = &src
			break
		}
	}
	if target == nil {
		http.Error(w, "invalid source id", http.StatusBadRequest)
		return
	}

	path := s.getIPSourcePath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// 代理处理逻辑：使用用户本机的 127.0.0.18:7897 服务
	// 为避免代理不在线时 client.Do 经历漫长的 TCP 握手超时等待，此处前置 200ms 的快速 TCP 探针
	proxyURL, err := url.Parse("http://127.0.0.18:7897")
	proxyAlive := false
	if err == nil {
		conn, dialErr := net.DialTimeout("tcp", proxyURL.Host, 200*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			proxyAlive = true
		}
	}

	var transport *http.Transport
	if proxyAlive {
		transport = &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		}
		log.Printf("[ip_update] 代理探测在线，将通过代理下载: %s", target.URL)
	} else {
		transport = &http.Transport{}
		log.Printf("[ip_update] 代理探测不在线或不可达，直接发起直连下载: %s", target.URL)
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   35 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp, err := client.Do(req)
	// 仅当使用了代理且失败时，才尝试一次直连重试
	if err != nil && proxyAlive {
		log.Printf("[ip_update] 代理请求失败，尝试直连重试: %v", err)
		directClient := &http.Client{
			Timeout: 30 * time.Second,
		}
		resp, err = directClient.Do(req)
	}

	if err != nil {
		http.Error(w, "下载失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "下载返回状态码异常: "+resp.Status, http.StatusInternalServerError)
		return
	}

	out, err := os.Create(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{"success": true})
}

func (s *Server) handleIPPreview(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	path := s.getIPSourcePath(id)
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "文件不存在或无法读取，请先更新此 IP 段", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		http.Error(w, "probe is already running", http.StatusConflict)
		return
	}
	s.running = true
	s.lastError = ""
	s.mu.Unlock()

	push := r.URL.Query().Get("push") == "1" || r.URL.Query().Get("push") == "true"
	cfg := s.cfg
	if countries := parseCountries(r.URL.Query().Get("countries")); len(countries) > 0 {
		cfg.Probe.Countries = countries
	}

	// 动态解析 sources query 并替换 cfg.Sources
	sourcesParam := r.URL.Query().Get("sources")
	if sourcesParam != "" {
		var nextSources []config.SourceConfig
		for _, id := range strings.Split(sourcesParam, ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			path := s.getIPSourcePath(id)
			if _, err := os.Stat(path); err == nil {
				nextSources = append(nextSources, config.SourceConfig{
					Type:   "file",
					Name:   id,
					Path:   path,
					Weight: 100,
				})
			}
		}
		if len(nextSources) > 0 {
			cfg.Sources = nextSources
		}
	}

	mode := r.URL.Query().Get("mode")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		result, err := app.RunOnceMode(ctx, cfg, push, mode)
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			s.lastError = err.Error()
		} else {
			if result.AutoProxyIPs == "" && s.last != nil {
				result.AutoProxyIPs = s.last.AutoProxyIPs
			}
			s.last = &result
			if result.AutoProxyIPs != "" {
				s.saveProxyIPToDisk(result.AutoProxyIPs)
			}
		}
		s.running = false
	}()

	writeJSON(w, map[string]any{"started": true})
}

func (s *Server) handleProxyIPFetch(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		http.Error(w, "probe is already running", http.StatusConflict)
		return
	}
	if !s.cfg.Clash.AutoProxyIP.Enabled {
		s.mu.Unlock()
		http.Error(w, "proxyip_auto 未启用，请在配置中设置 clash.proxyip_auto.enabled: true", http.StatusBadRequest)
		return
	}
	s.running = true
	s.lastError = ""
	cfg := s.cfg
	// 支持前端传入国家覆盖配置
	if country := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("country"))); country != "" {
		cfg.Clash.AutoProxyIP.Country = country
		// 持久化到配置文件
		s.cfg.Clash.AutoProxyIP.Country = country
		_ = s.cfg.Save(s.configPath)
	}
	s.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		result, err := app.FetchProxyIPOnly(ctx, cfg)
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			s.lastError = err.Error()
		} else {
			if s.last == nil {
				s.last = &app.RunResult{}
			}
			s.last.AutoProxyIPs = result
			s.saveProxyIPToDisk(result)
		}
		s.running = false
	}()

	writeJSON(w, map[string]any{"started": true})
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		http.Error(w, "probe is currently running", http.StatusConflict)
		return
	}
	if s.last == nil || s.last.ADDText == "" {
		s.mu.Unlock()
		http.Error(w, "no result to push", http.StatusBadRequest)
		return
	}
	if s.cfg.Worker.Password == "" {
		s.mu.Unlock()
		http.Error(w, "未配置 Worker 密码，请在配置文件中填写 worker.password", http.StatusBadRequest)
		return
	}
	if s.cfg.Output.DryRun {
		s.mu.Unlock()
		http.Error(w, "处于演练模式 (dry_run: true)，推送已跳过。请在配置中将其改为 false 后重启", http.StatusBadRequest)
		return
	}
	last := s.last
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 1*time.Minute)
	defer cancel()

	client, err := worker.New(s.cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := client.Login(ctx); err != nil {
		http.Error(w, "login: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if err := client.PushADD(ctx, last.ADDText); err != nil {
		http.Error(w, "push: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.last.Pushed = true
	s.mu.Unlock()

	writeJSON(w, map[string]any{"success": true})
}

func (s *Server) handleProxyIPPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		http.Error(w, "probe is currently running", http.StatusConflict)
		return
	}
	if s.last == nil || strings.TrimSpace(s.last.AutoProxyIPs) == "" {
		s.mu.Unlock()
		http.Error(w, "no auto proxyip result to push", http.StatusBadRequest)
		return
	}
	if s.cfg.Worker.Password == "" {
		s.mu.Unlock()
		http.Error(w, "未配置 Worker 密码，请在配置文件中填写 worker.password", http.StatusBadRequest)
		return
	}
	proxyIPs := strings.TrimSpace(s.last.AutoProxyIPs)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 1*time.Minute)
	defer cancel()

	client, err := worker.New(s.cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := client.Login(ctx); err != nil {
		http.Error(w, "login: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if err := client.PushProxyIP(ctx, proxyIPs); err != nil {
		http.Error(w, "push proxyip: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"success":  true,
		"proxy_ip": proxyIPs,
	})
}

func parseCountries(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToUpper(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	writeJSON(w, preflight.Run(ctx, s.cfg))
}

func (s *Server) handleConfigUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Countries []string       `json:"countries"`
		Config    *config.Config `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if req.Config != nil {
		next := *req.Config
		next.ApplyDefaults()
		next.ResolvePaths(filepath.Dir(s.configPath))
		if err := next.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.cfg = next
	} else {
		s.cfg.Probe.Countries = req.Countries
	}
	if err := s.cfg.Save(s.configPath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{"success": true})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}
