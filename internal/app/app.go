package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/grootpxw/edgetunnel-bestsub/internal/config"
	"github.com/grootpxw/edgetunnel-bestsub/internal/preflight"
	"github.com/grootpxw/edgetunnel-bestsub/internal/probe"
	"github.com/grootpxw/edgetunnel-bestsub/internal/proxyip"
	"github.com/grootpxw/edgetunnel-bestsub/internal/source"
	"github.com/grootpxw/edgetunnel-bestsub/internal/worker"
)

type RunResult struct {
	StartedAt    time.Time         `json:"started_at"`
	FinishedAt   time.Time         `json:"finished_at"`
	Mode         string            `json:"mode"`
	Candidates   int               `json:"candidates"`
	Results      []probe.Result    `json:"results"`
	Top          []probe.Result    `json:"top"`
	ADDText      string            `json:"add_text"`
	OutputPath   string            `json:"output_path"`
	Pushed       bool              `json:"pushed"`
	PushError    string            `json:"push_error,omitempty"`
	Preflight    *preflight.Report `json:"preflight,omitempty"`
	AutoProxyIPs string            `json:"auto_proxy_ips,omitempty"`
}

func RunOnce(ctx context.Context, cfg config.Config, push bool) (RunResult, error) {
	return RunOnceMode(ctx, cfg, push, "quick")
}

func RunOnceMode(ctx context.Context, cfg config.Config, push bool, mode string) (RunResult, error) {
	mode = normalizeMode(mode)
	cfg = applyMode(cfg, mode)
	start := time.Now()
	run := RunResult{StartedAt: start, Mode: mode}

	// 测速前预检 Worker 登录，避免测完才发现密码错误
	// 只要配置了密码就验证
	needWorker := cfg.Worker.Password != ""
	var workerClient *worker.Client
	if needWorker {
		c, err := worker.New(cfg)
		if err != nil {
			return RunResult{}, fmt.Errorf("Worker 连接失败: %w", err)
		}
		if err := c.Login(ctx); err != nil {
			return RunResult{}, fmt.Errorf("login: %w", err)
		}
		log.Printf("[preflight] Worker 登录验证通过")
		workerClient = c
	}

	if cfg.Probe.Preflight.Enabled {
		report := preflight.Run(ctx, cfg)
		run.Preflight = &report
		if report.Blocked {
			run.FinishedAt = time.Now()
			return run, nil
		}
	}
	candidates, err := source.Load(ctx, cfg)
	if err != nil {
		return RunResult{}, err
	}
	results := probe.Run(ctx, cfg, candidates)
	if mode == "stable" {
		results = rerunStable(ctx, cfg, results)
	}
	top := probe.Keep(results, cfg.Probe.Keep, cfg.Probe.PerCIDR24Limit, cfg.Probe.Countries)
	addText := probe.FormatADD(top, cfg.Output.RemarkPrefix)

	if err := writeOutput(cfg.Output.Path, addText); err != nil {
		return RunResult{}, err
	}

	run.FinishedAt = time.Now()
	run.Candidates = len(candidates)
	run.Results = results
	run.Top = top
	run.ADDText = addText
	run.OutputPath = cfg.Output.Path

	if push && !cfg.Output.DryRun {
		if err := workerClient.PushADD(ctx, addText); err != nil {
			run.PushError = err.Error()
			return run, nil
		}
		run.Pushed = true
	}
	return run, nil
}

// FetchProxyIPOnly 单独执行反代 IP 优选流程，不触发入口 IP 测速。
func FetchProxyIPOnly(ctx context.Context, cfg config.Config) (string, error) {
	if !cfg.Clash.AutoProxyIP.Enabled {
		return "", fmt.Errorf("proxyip_auto 未启用")
	}

	// 如果 worker_verify 开启，先验证登录
	var workerClient *worker.Client
	if cfg.Clash.AutoProxyIP.WorkerVerify.Enabled {
		c, err := worker.New(cfg)
		if err != nil {
			return "", fmt.Errorf("Worker 连接失败: %w", err)
		}
		if err := c.Login(ctx); err != nil {
			return "", fmt.Errorf("login: %w", err)
		}
		workerClient = c
	}

	fetchOpts := proxyip.Options{
		Country:           cfg.Clash.AutoProxyIP.Country,
		Limit:             cfg.Clash.AutoProxyIP.Limit,
		MaxCandidates:     cfg.Clash.AutoProxyIP.MaxCandidates,
		SourceURL:         cfg.Clash.AutoProxyIP.SourceURL,
		CheckAPI:          cfg.Clash.AutoProxyIP.CheckAPI,
		Concurrency:       cfg.Clash.AutoProxyIP.Concurrency,
		RequireGeoIPMatch: cfg.Clash.AutoProxyIP.RequireGeoIPMatch,
		GeoIPDBPath:       cfg.Clash.AutoProxyIP.GeoIPDBPath,
		WorkerVerify: proxyip.WorkerVerifyOptions{
			Enabled:   cfg.Clash.AutoProxyIP.WorkerVerify.Enabled,
			URL:       cfg.Clash.AutoProxyIP.WorkerVerify.URL,
			MaxChecks: cfg.Clash.AutoProxyIP.WorkerVerify.MaxChecks,
		},
		WorkerBaseURL:  cfg.Worker.BaseURL,
		WorkerPassword: cfg.Worker.Password,
		UserAgent:      cfg.Worker.UserAgent,
	}
	if workerClient != nil {
		fetchOpts.WorkerHTTPClient = workerClient.HTTPClient()
	}

	fetchedIPs, err := proxyip.FetchAndCheck(ctx, fetchOpts)
	if err != nil {
		return "", err
	}
	if len(fetchedIPs) == 0 {
		return "", fmt.Errorf("未找到符合条件的反代 IP")
	}
	return strings.Join(fetchedIPs, ","), nil
}

func normalizeMode(mode string) string {
	switch mode {
	case "stable":
		return "stable"
	default:
		return "quick"
	}
}

func applyMode(cfg config.Config, mode string) config.Config {
	if mode == "stable" {
		if cfg.Probe.CandidateLimit < 1800 {
			cfg.Probe.CandidateLimit = 1800
		}
		if cfg.Probe.TimeoutMS < 4000 {
			cfg.Probe.TimeoutMS = 4000
		}
		if cfg.Probe.Concurrency > 100 {
			cfg.Probe.Concurrency = 100
		}
		return cfg
	}
	if cfg.Probe.CandidateLimit > 600 {
		cfg.Probe.CandidateLimit = 600
	}
	if cfg.Probe.TimeoutMS > 2500 {
		cfg.Probe.TimeoutMS = 2500
	}
	return cfg
}

func rerunStable(ctx context.Context, cfg config.Config, first []probe.Result) []probe.Result {
	successful := make([]probe.Result, 0, len(first))
	for _, result := range first {
		if result.Success {
			successful = append(successful, result)
		}
	}
	probe.Sort(successful)
	if len(successful) > 100 {
		successful = successful[:100]
	}
	candidates := make([]source.Candidate, 0, len(successful))
	for _, result := range successful {
		candidates = append(candidates, source.Candidate{
			IP:     result.IP,
			Port:   result.Port,
			Remark: result.Remark,
			Source: result.Source,
			Weight: result.SourceWeight,
		})
	}
	rounds := [][]probe.Result{successful}
	const extraRounds = 5
	for i := 0; i < extraRounds; i++ {
		select {
		case <-ctx.Done():
			return mergeStable(rounds)
		default:
		}
		if i > 0 {
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return mergeStable(rounds)
			case <-timer.C:
			}
		}
		rounds = append(rounds, probe.Run(ctx, cfg, candidates))
	}
	return mergeStable(rounds)
}

func mergeStable(rounds [][]probe.Result) []probe.Result {
	type acc struct {
		best       probe.Result
		attempts   int
		successes  int
		totalTotal int64
		totalHTTP  int64
		totalTCP   int64
		totalTLS   int64
		minTotal   int64
		maxTotal   int64
		weight     int
	}
	items := map[string]*acc{}
	add := func(result probe.Result) {
		key := fmt.Sprintf("%s:%d", result.IP, result.Port)
		current := items[key]
		if current == nil {
			copy := result
			items[key] = &acc{best: copy, weight: result.SourceWeight}
			current = items[key]
		}
		current.attempts++
		if result.SourceWeight > current.weight {
			current.weight = result.SourceWeight
		}
		if result.Success {
			current.successes++
			current.totalTotal += result.TotalMS
			current.totalHTTP += result.HTTPMS
			current.totalTCP += result.TCPMS
			current.totalTLS += result.TLSMS
			if current.minTotal == 0 || result.TotalMS < current.minTotal {
				current.minTotal = result.TotalMS
			}
			if result.TotalMS > current.maxTotal {
				current.maxTotal = result.TotalMS
			}
			if !current.best.Success || result.TotalMS < current.best.TotalMS {
				current.best = result
			}
		}
	}
	for _, round := range rounds {
		for _, result := range round {
			add(result)
		}
	}
	out := make([]probe.Result, 0, len(items))
	for _, item := range items {
		result := item.best
		result.Attempts = item.attempts
		result.Successes = item.successes
		if item.attempts > 0 {
			result.SuccessRate = item.successes * 100 / item.attempts
		}
		result.Success = item.successes > 0
		if item.successes > 0 {
			count := int64(item.successes)
			result.TotalMS = item.totalTotal / count
			result.HTTPMS = item.totalHTTP / count
			result.TCPMS = item.totalTCP / count
			result.TLSMS = item.totalTLS / count
		}
		result.SourceWeight = item.weight
		out = append(out, result)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		spread := func(result probe.Result) int64 {
			item := items[fmt.Sprintf("%s:%d", result.IP, result.Port)]
			if item == nil || item.successes <= 1 {
				return 1<<63 - 1
			}
			return item.maxTotal - item.minTotal
		}
		if a.Success != b.Success {
			return a.Success
		}
		if a.SuccessRate != b.SuccessRate {
			return a.SuccessRate > b.SuccessRate
		}
		if a.Successes != b.Successes {
			return a.Successes > b.Successes
		}
		aSpread := spread(a)
		bSpread := spread(b)
		if aSpread != bSpread {
			return aSpread < bSpread
		}
		if a.TotalMS != b.TotalMS {
			return a.TotalMS < b.TotalMS
		}
		if a.HTTPMS != b.HTTPMS {
			return a.HTTPMS < b.HTTPMS
		}
		if a.SourceWeight != b.SourceWeight {
			return a.SourceWeight > b.SourceWeight
		}
		return a.Port < b.Port
	})
	return out
}

func writeOutput(path string, body string) error {
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(body), 0644)
}
