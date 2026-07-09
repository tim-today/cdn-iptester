package config

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server  ServerConfig   `yaml:"server" json:"server"`
	Worker  WorkerConfig   `yaml:"worker" json:"worker"`
	Probe   ProbeConfig    `yaml:"probe" json:"probe"`
	Sources []SourceConfig `yaml:"sources" json:"sources"`
	Output  OutputConfig   `yaml:"output" json:"output"`
	Clash   ClashConfig    `yaml:"clash" json:"clash"`
}

type ServerConfig struct {
	Listen string `yaml:"listen" json:"listen"`
}

type WorkerConfig struct {
	BaseURL   string `yaml:"base_url" json:"base_url"`
	Password  string `yaml:"password" json:"password"`
	UserAgent string `yaml:"user_agent" json:"user_agent"`
}

type ProbeConfig struct {
	Target            TargetConfig    `yaml:"target" json:"target"`
	Preflight         PreflightConfig `yaml:"preflight" json:"preflight"`
	IPv4              bool            `yaml:"ipv4" json:"ipv4"`
	IPv6              bool            `yaml:"ipv6" json:"ipv6"`
	Countries         []string        `yaml:"countries,flow" json:"countries"`
	RequireGeoIPMatch bool            `yaml:"require_geoip_match" json:"require_geoip_match"`
	GeoIPDBPath       string          `yaml:"geoip_db_path" json:"geoip_db_path"`
	Ports             []int           `yaml:"ports,flow" json:"ports"`
	CandidateLimit    int             `yaml:"candidate_limit" json:"candidate_limit"`
	Keep              int             `yaml:"keep" json:"keep"`
	TimeoutMS         int             `yaml:"timeout_ms" json:"timeout_ms"`
	Concurrency       int             `yaml:"concurrency" json:"concurrency"`
	PerCIDR24Limit    int             `yaml:"per_cidr24_limit" json:"per_cidr24_limit"`
}

type TargetConfig struct {
	Mode           string `yaml:"mode" json:"mode"`
	URL            string `yaml:"url" json:"url"`
	Host           string `yaml:"host" json:"host"`
	SNI            string `yaml:"sni" json:"sni"`
	Method         string `yaml:"method" json:"method"`
	ExpectedStatus []int  `yaml:"expected_status,flow" json:"expected_status"`
}

type PreflightConfig struct {
	Enabled             bool  `yaml:"enabled" json:"enabled"`
	DisableSampleProbe  bool  `yaml:"disable_sample_probe" json:"disable_sample_probe"`
	SampleSize          int   `yaml:"sample_size" json:"sample_size"`
	BlockOnLowLatency   bool  `yaml:"block_on_low_latency" json:"block_on_low_latency"`
	LowLatencyThreshold int64 `yaml:"low_latency_threshold_ms" json:"low_latency_threshold_ms"`
}

type SourceConfig struct {
	Type   string `yaml:"type" json:"type"`
	Name   string `yaml:"name" json:"name"`
	URL    string `yaml:"url" json:"url"`
	Path   string `yaml:"path" json:"path"`
	Weight int    `yaml:"weight" json:"weight"`
}

type OutputConfig struct {
	Path         string `yaml:"path" json:"path"`
	RemarkPrefix string `yaml:"remark_prefix" json:"remark_prefix"`
	DryRun       bool   `yaml:"dry_run" json:"dry_run"`
}

type AutoProxyIPConfig struct {
	Enabled           bool                    `yaml:"enabled" json:"enabled"`
	Country           string                  `yaml:"country" json:"country"`
	Limit             int                     `yaml:"limit" json:"limit"`
	MaxCandidates     int                     `yaml:"max_candidates" json:"max_candidates"`
	SourceURL         string                  `yaml:"source_url" json:"source_url"`
	CheckAPI          string                  `yaml:"check_api" json:"check_api"`
	Concurrency       int                     `yaml:"concurrency" json:"concurrency"`
	RequireGeoIPMatch bool                    `yaml:"require_geoip_match" json:"require_geoip_match"`
	GeoIPDBPath       string                  `yaml:"geoip_db_path" json:"geoip_db_path"`
	WorkerVerify      AutoProxyIPVerifyConfig `yaml:"worker_verify" json:"worker_verify"`
}

type AutoProxyIPVerifyConfig struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	URL       string `yaml:"url" json:"url"`
	MaxChecks int    `yaml:"max_checks" json:"max_checks"`
}

type ClashConfig struct {
	AutoProxyIP AutoProxyIPConfig `yaml:"proxyip_auto" json:"proxyip_auto"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	cfg.ApplyDefaults()
	cfg.ResolvePaths(resolveBaseDir(path))
	return cfg, cfg.Validate()
}

func (c *Config) Save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := yaml.NewEncoder(f)
	enc.SetIndent(4)
	return enc.Encode(c)
}

func (c *Config) ApplyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:8788"
	}
	if c.Worker.UserAgent == "" {
		c.Worker.UserAgent = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
	}
	if c.Probe.Target.Method == "" {
		c.Probe.Target.Method = "HEAD"
	}
	if len(c.Probe.Target.ExpectedStatus) == 0 {
		c.Probe.Target.ExpectedStatus = []int{200, 204, 301, 302, 403, 404}
	}
	if c.Probe.Preflight.SampleSize <= 0 {
		c.Probe.Preflight.SampleSize = 8
	}
	if c.Probe.Preflight.LowLatencyThreshold <= 0 {
		c.Probe.Preflight.LowLatencyThreshold = 20
	}
	// If neither is explicitly set, enable both
	if !c.Probe.IPv4 && !c.Probe.IPv6 {
		c.Probe.IPv4 = true
		c.Probe.IPv6 = true
	}
	if len(c.Probe.Ports) == 0 {
		c.Probe.Ports = []int{443, 2053, 2083, 2087, 2096, 8443}
	}
	if c.Probe.CandidateLimit <= 0 {
		c.Probe.CandidateLimit = 600
	}
	if c.Probe.Keep <= 0 {
		c.Probe.Keep = 30
	}
	if c.Probe.TimeoutMS <= 0 {
		c.Probe.TimeoutMS = 2500
	}
	if c.Probe.Concurrency <= 0 {
		c.Probe.Concurrency = 120
	}
	if c.Probe.PerCIDR24Limit <= 0 {
		c.Probe.PerCIDR24Limit = 2
	}
	if c.Probe.RequireGeoIPMatch && c.Probe.GeoIPDBPath == "" {
		c.Probe.GeoIPDBPath = "GeoLite2-Country.mmdb"
	}
	if c.Output.Path == "" {
		c.Output.Path = "CDN-IPtester.txt"
	}
	if c.Output.RemarkPrefix == "" {
		c.Output.RemarkPrefix = "IP 官方优选"
	}
	if c.Clash.AutoProxyIP.Limit <= 0 {
		c.Clash.AutoProxyIP.Limit = 8
	}
	if c.Clash.AutoProxyIP.MaxCandidates <= 0 {
		c.Clash.AutoProxyIP.MaxCandidates = 50
	}
	if c.Clash.AutoProxyIP.SourceURL == "" {
		c.Clash.AutoProxyIP.SourceURL = "https://zip.cm.edu.kg/all.txt"
	}
	if c.Clash.AutoProxyIP.CheckAPI == "" {
		c.Clash.AutoProxyIP.CheckAPI = "https://api.090227.xyz/check?proxyip=%s"
	}
	if c.Clash.AutoProxyIP.Concurrency <= 0 {
		c.Clash.AutoProxyIP.Concurrency = 20
	}
	if c.Clash.AutoProxyIP.GeoIPDBPath == "" {
		c.Clash.AutoProxyIP.GeoIPDBPath = c.Probe.GeoIPDBPath
	}
	if c.Clash.AutoProxyIP.WorkerVerify.URL == "" {
		c.Clash.AutoProxyIP.WorkerVerify.URL = "https://cloudflare.com/cdn-cgi/trace"
	}
	if c.Clash.AutoProxyIP.WorkerVerify.MaxChecks <= 0 {
		c.Clash.AutoProxyIP.WorkerVerify.MaxChecks = c.Clash.AutoProxyIP.Limit * 3
	}
}

func (c *Config) ResolvePaths(baseDir string) {
	if baseDir == "" {
		return
	}
	if c.Output.Path != "" && !filepath.IsAbs(c.Output.Path) {
		c.Output.Path = filepath.Join(baseDir, c.Output.Path)
	}
	if c.Probe.GeoIPDBPath != "" && !filepath.IsAbs(c.Probe.GeoIPDBPath) {
		c.Probe.GeoIPDBPath = filepath.Join(baseDir, c.Probe.GeoIPDBPath)
	}
	if c.Clash.AutoProxyIP.GeoIPDBPath != "" && !filepath.IsAbs(c.Clash.AutoProxyIP.GeoIPDBPath) {
		c.Clash.AutoProxyIP.GeoIPDBPath = filepath.Join(baseDir, c.Clash.AutoProxyIP.GeoIPDBPath)
	}
	for i := range c.Sources {
		if c.Sources[i].Path != "" && !filepath.IsAbs(c.Sources[i].Path) {
			c.Sources[i].Path = filepath.Join(baseDir, c.Sources[i].Path)
		}
	}
}

func resolveBaseDir(configPath string) string {
	configDir := filepath.Dir(configPath)
	if filepath.Base(configDir) == "configs" {
		return filepath.Dir(configDir)
	}
	return configDir
}

func (c Config) Validate() error {
	if c.Probe.Target.URL == "" {
		return errors.New("probe.target.url is required")
	}
	if c.Probe.Target.Host == "" {
		return errors.New("probe.target.host is required")
	}
	if c.Probe.Target.SNI == "" {
		return errors.New("probe.target.sni is required")
	}
	if len(c.Sources) == 0 {
		return errors.New("at least one source is required")
	}
	return nil
}

func (c Config) Timeout() time.Duration {
	return time.Duration(c.Probe.TimeoutMS) * time.Millisecond
}
