package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/grootpxw/edgetunnel-bestsub/internal/config"
)

type Client struct {
	baseURL   string
	password  string
	userAgent string
	http      *http.Client
}

func New(cfg config.Config) (*Client, error) {
	if cfg.Worker.BaseURL == "" {
		return nil, fmt.Errorf("worker.base_url is required")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL:   strings.TrimRight(cfg.Worker.BaseURL, "/"),
		password:  cfg.Worker.Password,
		userAgent: cfg.Worker.UserAgent,
		http: &http.Client{
			Timeout: 20 * time.Second,
			Jar:     jar,
			Transport: &http.Transport{
				Proxy: nil,
			},
		},
	}, nil
}

func (c *Client) Login(ctx context.Context) error {
	if c.password == "" {
		return fmt.Errorf("worker.password is required")
	}
	form := url.Values{}
	form.Set("password", c.password)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("login returned %s", resp.Status)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if !strings.Contains(string(body), "success") {
		return fmt.Errorf("Worker 登录失败（密码可能错误）")
	}
	return nil
}

func (c *Client) PushADD(ctx context.Context, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/admin/ADD.txt", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=UTF-8")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("push returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	if !strings.Contains(string(responseBody), "success") {
		return fmt.Errorf("push did not return success: %s", strings.TrimSpace(string(responseBody)))
	}
	return nil
}

// HTTPClient 返回内部已登录的 http.Client（带 cookie），供其他模块复用会话。
func (c *Client) HTTPClient() *http.Client {
	return c.http
}

// BaseURL 返回 Worker 的基础 URL。
func (c *Client) BaseURL() string {
	return c.baseURL
}

func (c *Client) PushProxyIP(ctx context.Context, body string) error {
	type pushResult struct {
		name string
		err  error
	}

	results := make(chan pushResult, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		results <- pushResult{name: "config.json", err: c.pushProxyIPConfig(ctx, body)}
	}()
	go func() {
		defer wg.Done()
		results <- pushResult{name: "PROXYIP.txt", err: c.pushProxyIPText(ctx, body)}
	}()

	wg.Wait()
	close(results)

	var errs []string
	for result := range results {
		if result.err == nil {
			return nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", result.name, result.err))
	}
	return fmt.Errorf("push proxyip failed: %s", strings.Join(errs, "; "))
}

func (c *Client) pushProxyIPText(ctx context.Context, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/admin/PROXYIP.txt", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=UTF-8")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("push proxyip returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	if !strings.Contains(string(responseBody), "success") {
		return fmt.Errorf("push proxyip did not return success: %s", strings.TrimSpace(string(responseBody)))
	}
	return nil
}

func (c *Client) pushProxyIPConfig(ctx context.Context, body string) error {
	cfg, err := c.FetchConfig(ctx)
	if err != nil {
		return err
	}
	updateProxyIPConfig(cfg, body)

	payload, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/admin/config.json", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("push returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	if !strings.Contains(string(responseBody), "success") {
		return fmt.Errorf("push did not return success: %s", strings.TrimSpace(string(responseBody)))
	}
	return nil
}

func (c *Client) FetchConfig(ctx context.Context) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/admin/config.json", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch config returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}

	var cfg map[string]any
	if err := json.Unmarshal(responseBody, &cfg); err != nil {
		return nil, fmt.Errorf("decode config.json: %w", err)
	}
	return cfg, nil
}

func updateProxyIPConfig(cfg map[string]any, proxyIPs string) {
	reverseProxy, ok := cfg["反代"].(map[string]any)
	if !ok || reverseProxy == nil {
		reverseProxy = map[string]any{}
		cfg["反代"] = reverseProxy
	}
	reverseProxy["PROXYIP"] = strings.TrimSpace(proxyIPs)
}
