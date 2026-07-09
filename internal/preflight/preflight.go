package preflight

import (
	"context"
	"fmt"
	"net"
	"sort"
	"time"

	"github.com/grootpxw/edgetunnel-bestsub/internal/config"
	"github.com/grootpxw/edgetunnel-bestsub/internal/probe"
	"github.com/grootpxw/edgetunnel-bestsub/internal/source"
)

type Check struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type Report struct {
	OK          bool           `json:"ok"`
	Blocked     bool           `json:"blocked"`
	Checks      []Check        `json:"checks"`
	Sample      []probe.Result `json:"sample,omitempty"`
	Advice      []string       `json:"advice"`
	GeneratedAt time.Time      `json:"generated_at"`
}

func Run(ctx context.Context, cfg config.Config) Report {
	report := Report{OK: true, GeneratedAt: time.Now()}

	systemProxy := detectSystemProxy()
	if systemProxy.Enabled {
		report.add(false, "block", "系统代理", systemProxy.Message)
		report.Blocked = true
		report.Advice = append(report.Advice, "关闭 Windows 系统代理或 PAC 自动配置后再测速。")
	} else if systemProxy.Message != "" {
		report.add(true, "info", "系统代理", systemProxy.Message)
	} else {
		report.add(true, "info", "系统代理", "未检测到系统代理")
	}

	ipv6Status := checkIPv6Connectivity()
	if !ipv6Status.OK && cfg.Probe.IPv6 {
		report.add(false, "warn", "IPv6 网络", ipv6Status.Message)
		report.Advice = append(report.Advice, "本地网络似乎不支持 IPv6，建议在配置中关闭 IPv4: false 或开启双栈测试。")
	} else if cfg.Probe.IPv6 {
		report.add(true, "info", "IPv6 网络", ipv6Status.Message)
	}

	if cfg.Probe.Preflight.DisableSampleProbe {
		report.add(true, "info", "抽样测速", "已在配置中跳过")
	} else {
		sample := sampleProbe(ctx, cfg)
		report.Sample = sample
		successful := successfulResults(sample)
		if len(successful) == 0 {
			report.add(false, "warn", "抽样测速", "抽样未得到可用结果，可能是网络异常或测速目标不支持当前端口")
		} else {
			median := medianTotal(successful)
			if median > 0 && median < cfg.Probe.Preflight.LowLatencyThreshold {
				report.add(false, "block", "异常低延迟", fmt.Sprintf("抽样成功结果中位延迟为 %dms，低于阈值 %dms", median, cfg.Probe.Preflight.LowLatencyThreshold))
				if cfg.Probe.Preflight.BlockOnLowLatency {
					report.Blocked = true
				}
				report.Advice = append(report.Advice, "延迟异常低通常说明测速流量走了本机代理、透明代理或 TUN。")
			} else {
				report.add(true, "info", "抽样测速", fmt.Sprintf("抽样成功 %d/%d，中位延迟 %dms", len(successful), len(sample), median))
			}
		}
	}

	report.OK = !report.Blocked
	if report.Blocked {
		report.Advice = append(report.Advice, "关闭代理软件、系统代理和路由器透明代理后再运行测速。")
	}
	return report
}

func (r *Report) add(ok bool, severity string, name string, message string) {
	r.Checks = append(r.Checks, Check{Name: name, OK: ok, Severity: severity, Message: message})
}

func sampleProbe(ctx context.Context, cfg config.Config) []probe.Result {
	sampleSize := cfg.Probe.Preflight.SampleSize
	if sampleSize <= 0 {
		sampleSize = 8
	}
	cfg.Probe.CandidateLimit = sampleSize
	if cfg.Probe.Concurrency > sampleSize {
		cfg.Probe.Concurrency = sampleSize
	}
	candidates, err := source.Load(ctx, cfg)
	if err != nil {
		return nil
	}
	if len(candidates) > sampleSize {
		candidates = candidates[:sampleSize]
	}
	return probe.Run(ctx, cfg, candidates)
}

func successfulResults(results []probe.Result) []probe.Result {
	out := make([]probe.Result, 0, len(results))
	for _, result := range results {
		if result.Success {
			out = append(out, result)
		}
	}
	return out
}
func checkIPv6Connectivity() struct {
	OK      bool
	Message string
} {
	// Try to dial Cloudflare IPv6 DNS
	timeout := 2 * time.Second
	conn, err := net.DialTimeout("tcp", "[2606:4700:4700::1111]:80", timeout)
	if err != nil {
		return struct {
			OK      bool
			Message string
		}{OK: false, Message: "本地网络 IPv6 路由不可达: " + err.Error()}
	}
	conn.Close()
	return struct {
		OK      bool
		Message string
	}{OK: true, Message: "IPv6 连接正常"}
}

func medianTotal(results []probe.Result) int64 {
	if len(results) == 0 {
		return 0
	}
	values := make([]int64, 0, len(results))
	for _, result := range results {
		values = append(values, result.TotalMS)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values[len(values)/2]
}
