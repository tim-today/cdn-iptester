package proxyip

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/geoip2-golang"
)

type checkAPIResponse struct {
	Candidate    string `json:"candidate"`
	Success      bool   `json:"success"`
	ResponseTime int    `json:"responseTime"`
}

type ProxyIPResult struct {
	IP           string
	ResponseTime int
}

type Options struct {
	Country           string
	Limit             int
	MaxCandidates     int
	SourceURL         string
	CheckAPI          string
	Concurrency       int
	RequireGeoIPMatch bool
	GeoIPDBPath       string
	WorkerVerify      WorkerVerifyOptions
	WorkerBaseURL     string
	WorkerPassword    string
	UserAgent         string
	// WorkerHTTPClient 如果不为 nil，则复用该已登录的 client，跳过内部登录。
	WorkerHTTPClient *http.Client
}

type WorkerVerifyOptions struct {
	Enabled   bool
	URL       string
	MaxChecks int
}

type workerProxyIPTestResponse struct {
	Success      bool   `json:"success"`
	ProxyIP      string `json:"proxyip"`
	URL          string `json:"url"`
	IP           string `json:"ip"`
	Country      string `json:"country"`
	Colo         string `json:"colo"`
	Status       int    `json:"status"`
	ResponseTime int    `json:"responseTime"`
	Error        string `json:"error"`
}

// FetchAndCheck fetches proxy IPs, filters by country, verifies them, and returns up to limit IPs.
func FetchAndCheck(ctx context.Context, opts Options) ([]string, error) {
	if opts.Country == "" {
		return nil, nil
	}
	if opts.Limit <= 0 {
		opts.Limit = 8
	}
	if opts.MaxCandidates <= 0 {
		opts.MaxCandidates = 50
	}
	if opts.SourceURL == "" {
		opts.SourceURL = "https://zip.cm.edu.kg/all.txt"
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "bestsub-go/0.1"
	}

	country := strings.ToUpper(opts.Country)
	log.Printf("Starting proxy IP selection for country: %s", country)

	// 1. Fetch all IPs
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.SourceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", opts.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch all.txt: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("failed to fetch all.txt: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read all.txt: %w", err)
	}

	// 2. Filter by country
	candidates := parseAllTXT(string(body), country)

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no proxy IPs found for country %s", country)
	}
	log.Printf("Found %d candidate proxy IPs for country %s", len(candidates), country)

	// 3. GeoIP filter (local DB, no network cost) — before latency check to reduce candidates
	if opts.RequireGeoIPMatch {
		filtered, err := filterGeoIPCandidates(candidates, opts.GeoIPDBPath, country)
		if err != nil {
			return nil, err
		}
		candidates = filtered
		if len(candidates) == 0 {
			return nil, fmt.Errorf("no proxy IPs left after GeoIP country filter")
		}
		log.Printf("GeoIP filter kept %d candidates", len(candidates))
	}

	if opts.WorkerVerify.Enabled {
		validResults, err := checkWorkerCandidates(ctx, opts, candidates, country)
		if err != nil {
			return nil, err
		}
		if len(validResults) == 0 {
			return nil, fmt.Errorf("no valid proxy IPs after Worker exit country check")
		}
		sort.SliceStable(validResults, func(i, j int) bool {
			return validResults[i].ResponseTime < validResults[j].ResponseTime
		})

		finalIPs := make([]string, 0, opts.Limit)
		for i := 0; i < len(validResults) && i < opts.Limit; i++ {
			finalIPs = append(finalIPs, validResults[i].IP)
		}
		log.Printf("Successfully fetched and checked %d proxy IPs", len(finalIPs))
		return finalIPs, nil
	}

	// 4. Check latency concurrently through the legacy check API.
	validResults := checkLatency(ctx, client, candidates, opts.CheckAPI, opts.Concurrency)

	if len(validResults) == 0 {
		return nil, fmt.Errorf("no valid proxy IPs after latency check")
	}
	sort.SliceStable(validResults, func(i, j int) bool {
		return validResults[i].ResponseTime < validResults[j].ResponseTime
	})

	// 5. Pick top limit
	var finalIPs []string
	for i := 0; i < len(validResults) && i < opts.Limit; i++ {
		finalIPs = append(finalIPs, validResults[i].IP)
	}

	log.Printf("Successfully fetched and checked %d proxy IPs", len(finalIPs))
	return finalIPs, nil
}

func checkLatency(ctx context.Context, client *http.Client, candidates []string, checkAPI string, concurrency int) []ProxyIPResult {
	var wg sync.WaitGroup
	resultsChan := make(chan ProxyIPResult, len(candidates))
	if concurrency <= 0 {
		concurrency = 20
	}
	sem := make(chan struct{}, concurrency)

	for _, ip := range candidates {
		wg.Add(1)
		go func(targetIP string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			checkURL := fmt.Sprintf(checkAPI, url.QueryEscape(targetIP))
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
			if err != nil {
				return
			}
			cResp, cErr := client.Do(req)
			if cErr != nil {
				return
			}
			defer cResp.Body.Close()

			if cResp.StatusCode != http.StatusOK {
				return
			}

			cBody, cErr := io.ReadAll(cResp.Body)
			if cErr != nil {
				return
			}

			var checkData checkAPIResponse
			if err := json.Unmarshal(cBody, &checkData); err == nil && checkData.Success {
				resultsChan <- ProxyIPResult{
					IP:           targetIP,
					ResponseTime: checkData.ResponseTime,
				}
			}
		}(ip)
	}

	wg.Wait()
	close(resultsChan)

	var validResults []ProxyIPResult
	for res := range resultsChan {
		validResults = append(validResults, res)
	}
	return validResults
}

func filterGeoIPCountry(results []ProxyIPResult, dbPath string, country string) ([]ProxyIPResult, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("geoip_db_path is required when require_geoip_match is true")
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("GeoIP DB not available at %s: %w", dbPath, err)
	}
	db, err := geoip2.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	out := make([]ProxyIPResult, 0, len(results))
	for _, result := range results {
		addr, err := proxyIPAddr(result.IP)
		if err != nil {
			continue
		}
		record, err := db.Country(net.ParseIP(addr.String()))
		if err == nil && strings.EqualFold(record.Country.IsoCode, country) {
			out = append(out, result)
		}
	}
	log.Printf("GeoIP country check kept %d/%d proxy IPs", len(out), len(results))
	return out, nil
}

// verifyExitIPGeoIP 验证出口 IP 的 GeoIP 注册归属地是否匹配目标国家
func verifyExitIPGeoIP(addr netip.Addr, dbPath string, country string) bool {
	if dbPath == "" {
		return true // 没有数据库则跳过验证
	}
	db, err := geoip2.Open(dbPath)
	if err != nil {
		return true // 打不开数据库则跳过
	}
	defer db.Close()
	record, err := db.Country(net.ParseIP(addr.String()))
	if err != nil {
		return false
	}
	return strings.EqualFold(record.Country.IsoCode, country)
}

// filterGeoIPCandidates 在延迟检测前用本地 GeoIP 数据库过滤候选 IP
func filterGeoIPCandidates(candidates []string, dbPath string, country string) ([]string, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("geoip_db_path is required when require_geoip_match is true")
	}
	db, err := geoip2.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open GeoIP DB: %w", err)
	}
	defer db.Close()

	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		addr, err := proxyIPAddr(candidate)
		if err != nil {
			continue
		}
		record, err := db.Country(net.ParseIP(addr.String()))
		if err == nil && strings.EqualFold(record.Country.IsoCode, country) {
			out = append(out, candidate)
		}
	}
	return out, nil
}

type workerCandidateCheck struct {
	Result ProxyIPResult
	Valid  bool
}

func checkWorkerCandidates(ctx context.Context, opts Options, candidates []string, country string) ([]ProxyIPResult, error) {
	if opts.WorkerBaseURL == "" {
		return nil, fmt.Errorf("worker.base_url is required when worker_verify.enabled is true")
	}
	if opts.Limit <= 0 {
		opts.Limit = 8
	}
	if opts.MaxCandidates <= 0 {
		opts.MaxCandidates = 50
	}

	sampled := sampleCandidates(candidates, opts.MaxCandidates)
	if len(sampled) == 0 {
		return nil, fmt.Errorf("no proxy IPs left after sampling")
	}

	workerCount := opts.Concurrency
	if workerCount <= 0 {
		workerCount = 20
	}
	if workerCount > len(sampled) {
		workerCount = len(sampled)
	}
	log.Printf("Worker verify sampling %d/%d candidates, concurrency=%d", len(sampled), len(candidates), workerCount)

	var client *http.Client
	if opts.WorkerHTTPClient != nil {
		client = opts.WorkerHTTPClient
	} else {
		if opts.WorkerPassword == "" {
			return nil, fmt.Errorf("worker.password is required when worker_verify.enabled is true")
		}
		c, err := newWorkerHTTPClient(opts)
		if err != nil {
			return nil, err
		}
		if err := workerLogin(ctx, c, opts); err != nil {
			return nil, err
		}
		client = c
	}

	checkCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan string)
	results := make(chan workerCandidateCheck, len(sampled))

	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for candidate := range jobs {
				select {
				case <-checkCtx.Done():
					return
				default:
				}
				results <- checkWorkerCandidate(checkCtx, client, opts, candidate, country)
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, candidate := range sampled {
			select {
			case <-checkCtx.Done():
				return
			case jobs <- candidate:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	valid := make([]ProxyIPResult, 0, opts.Limit)
	checked := 0
	for check := range results {
		checked++
		if check.Valid && len(valid) < opts.Limit {
			valid = append(valid, check.Result)
			log.Printf("Worker verify accepted %s (%d/%d)", check.Result.IP, len(valid), opts.Limit)
			if len(valid) >= opts.Limit {
				log.Printf("Worker verify early stopped: reached limit %d after checking %d/%d sampled candidates", opts.Limit, checked, len(sampled))
				cancel()
			}
		}
		if checked%10 == 0 || checked == len(sampled) || len(valid) >= opts.Limit {
			log.Printf("Worker verify progress: checked=%d/%d valid=%d", checked, len(sampled), len(valid))
		}
	}

	log.Printf("Worker exit country check kept %d/%d proxy IPs", len(valid), checked)
	return valid, nil
}

func sampleCandidates(candidates []string, maxCandidates int) []string {
	sampled := append([]string(nil), candidates...)
	rand.Shuffle(len(sampled), func(i, j int) {
		sampled[i], sampled[j] = sampled[j], sampled[i]
	})
	if maxCandidates > 0 && len(sampled) > maxCandidates {
		sampled = sampled[:maxCandidates]
	}
	return sampled
}

func checkWorkerCandidate(ctx context.Context, client *http.Client, opts Options, candidate string, country string) workerCandidateCheck {
	test, err := workerProxyIPTest(ctx, client, opts, candidate)
	if err != nil {
		if ctx.Err() != nil {
			return workerCandidateCheck{}
		}
		log.Printf("Worker proxyip test failed for %s: %v", candidate, err)
		return workerCandidateCheck{}
	}
	if !test.Success {
		log.Printf("Worker proxyip test rejected %s: success=false error=%s", candidate, test.Error)
		return workerCandidateCheck{}
	}
	if !strings.EqualFold(test.Country, country) {
		log.Printf("Worker proxyip test rejected %s: colo country=%s, want %s", candidate, test.Country, country)
		return workerCandidateCheck{}
	}
	if test.IP == "" {
		log.Printf("Worker proxyip test rejected %s: no exit IP returned", candidate)
		return workerCandidateCheck{}
	}
	exitAddr, parseErr := netip.ParseAddr(test.IP)
	if parseErr != nil {
		log.Printf("Worker proxyip test rejected %s: invalid exit IP %s", candidate, test.IP)
		return workerCandidateCheck{}
	}
	if !verifyExitIPGeoIP(exitAddr, opts.GeoIPDBPath, country) {
		log.Printf("Worker proxyip test rejected %s: exit IP %s not registered in %s", candidate, test.IP, country)
		return workerCandidateCheck{}
	}
	return workerCandidateCheck{
		Result: ProxyIPResult{
			IP:           candidate,
			ResponseTime: test.ResponseTime,
		},
		Valid: true,
	}
}

func filterWorkerExitCountry(ctx context.Context, opts Options, results []ProxyIPResult, country string) ([]ProxyIPResult, error) {
	if opts.WorkerBaseURL == "" {
		return nil, fmt.Errorf("worker.base_url is required when worker_verify.enabled is true")
	}
	limit := opts.WorkerVerify.MaxChecks
	if limit <= 0 || limit > len(results) {
		limit = len(results)
	}

	var client *http.Client
	if opts.WorkerHTTPClient != nil {
		// 复用外部已登录的 client
		client = opts.WorkerHTTPClient
	} else {
		if opts.WorkerPassword == "" {
			return nil, fmt.Errorf("worker.password is required when worker_verify.enabled is true")
		}
		c, err := newWorkerHTTPClient(opts)
		if err != nil {
			return nil, err
		}
		if err := workerLogin(ctx, c, opts); err != nil {
			return nil, err
		}
		client = c
	}

	out := make([]ProxyIPResult, 0, opts.Limit)
	for i := 0; i < limit; i++ {
		result := results[i]
		test, err := workerProxyIPTest(ctx, client, opts, result.IP)
		if err != nil {
			log.Printf("Worker proxyip test failed for %s: %v", result.IP, err)
			continue
		}
		if !test.Success {
			log.Printf("Worker proxyip test rejected %s: success=false error=%s", result.IP, test.Error)
			continue
		}
		// 验证机房位置匹配目标国家
		if !strings.EqualFold(test.Country, country) {
			log.Printf("Worker proxyip test rejected %s: colo country=%s, want %s", result.IP, test.Country, country)
			continue
		}
		// 用本地 GeoIP 数据库验证出口 IP 的注册归属地
		if test.IP == "" {
			log.Printf("Worker proxyip test rejected %s: no exit IP returned", result.IP)
			continue
		}
		exitAddr, parseErr := netip.ParseAddr(test.IP)
		if parseErr != nil {
			log.Printf("Worker proxyip test rejected %s: invalid exit IP %s", result.IP, test.IP)
			continue
		}
		if !verifyExitIPGeoIP(exitAddr, opts.GeoIPDBPath, country) {
			log.Printf("Worker proxyip test rejected %s: exit IP %s not registered in %s", result.IP, test.IP, country)
			continue
		}
		if test.ResponseTime > 0 {
			result.ResponseTime = test.ResponseTime
		}
		out = append(out, result)
		if len(out) >= opts.Limit {
			break
		}
	}
	log.Printf("Worker exit country check kept %d/%d proxy IPs", len(out), limit)
	return out, nil
}

func newWorkerHTTPClient(opts Options) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout: 20 * time.Second,
		Jar:     jar,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}, nil
}

func workerLogin(ctx context.Context, client *http.Client, opts Options) error {
	form := url.Values{}
	form.Set("password", opts.WorkerPassword)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(opts.WorkerBaseURL, "/")+"/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", opts.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("login returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if !strings.Contains(string(body), "success") {
		return fmt.Errorf("Worker 登录失败（密码可能错误），响应不含 success 标记")
	}
	return nil
}

func workerProxyIPTest(ctx context.Context, client *http.Client, opts Options, proxyIP string) (workerProxyIPTestResponse, error) {
	endpoint, err := url.Parse(strings.TrimRight(opts.WorkerBaseURL, "/") + "/admin/proxyip-test")
	if err != nil {
		return workerProxyIPTestResponse{}, err
	}
	query := endpoint.Query()
	query.Set("proxyip", proxyIP)
	if opts.WorkerVerify.URL != "" {
		query.Set("url", opts.WorkerVerify.URL)
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return workerProxyIPTestResponse{}, err
	}
	req.Header.Set("User-Agent", opts.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return workerProxyIPTestResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return workerProxyIPTestResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return workerProxyIPTestResponse{}, fmt.Errorf("worker test returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var data workerProxyIPTestResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return workerProxyIPTestResponse{}, err
	}
	return data, nil
}

func proxyIPAddr(value string) (netip.Addr, error) {
	addrPort, err := netip.ParseAddrPort(value)
	if err == nil {
		return addrPort.Addr(), nil
	}
	return netip.ParseAddr(value)
}

func parseAllTXT(text string, country string) []string {
	country = strings.ToUpper(strings.TrimSpace(country))
	seen := map[string]struct{}{}
	var candidates []string

	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		address, code, ok := parseAllTXTLine(line)
		if !ok || !strings.EqualFold(code, country) {
			continue
		}
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		candidates = append(candidates, address)
	}
	return candidates
}

func parseAllTXTLine(line string) (string, string, bool) {
	address, country, ok := strings.Cut(line, "#")
	if !ok {
		return "", "", false
	}
	address = strings.TrimSpace(address)
	country = strings.ToUpper(strings.TrimSpace(country))
	if address == "" || country == "" {
		return "", "", false
	}
	if _, err := netip.ParseAddrPort(address); err != nil {
		return "", "", false
	}
	return address, country, true
}
