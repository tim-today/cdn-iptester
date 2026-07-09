package probe

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/geoip2-golang"

	"github.com/grootpxw/edgetunnel-bestsub/internal/config"
	"github.com/grootpxw/edgetunnel-bestsub/internal/source"
)

type Result struct {
	IP           string `json:"ip"`
	Port         int    `json:"port"`
	Source       string `json:"source"`
	Remark       string `json:"remark"`
	Colo         string `json:"colo"`
	CountryCode  string `json:"country_code"`
	CountryName  string `json:"country_name"`
	StatusCode   int    `json:"status_code"`
	TCPMS        int64  `json:"tcp_ms"`
	TLSMS        int64  `json:"tls_ms"`
	HTTPMS       int64  `json:"http_ms"`
	TotalMS      int64  `json:"total_ms"`
	Success      bool   `json:"success"`
	Error        string `json:"error,omitempty"`
	SourceWeight int    `json:"source_weight"`
	Attempts     int    `json:"attempts,omitempty"`
	Successes    int    `json:"successes,omitempty"`
	SuccessRate  int    `json:"success_rate,omitempty"`
}

func Run(ctx context.Context, cfg config.Config, candidates []source.Candidate) []Result {
	if len(candidates) == 0 {
		return nil
	}

	var geoIPDB *geoip2.Reader
	if cfg.Probe.RequireGeoIPMatch {
		db, err := geoip2.Open(cfg.Probe.GeoIPDBPath)
		if err != nil {
			log.Printf("Failed to open GeoIP DB at %s: %v", cfg.Probe.GeoIPDBPath, err)
		} else {
			geoIPDB = db
			defer geoIPDB.Close()
		}
	}

	workerCount := cfg.Probe.Concurrency
	if workerCount <= 0 {
		workerCount = 100
	}
	jobs := make(chan source.Candidate)
	results := make(chan Result, len(candidates))

	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for candidate := range jobs {
				results <- One(ctx, cfg, candidate, geoIPDB)
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, candidate := range candidates {
			select {
			case <-ctx.Done():
				return
			case jobs <- candidate:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make([]Result, 0, len(candidates))
	for result := range results {
		out = append(out, result)
	}
	Sort(out)
	return out
}

func One(ctx context.Context, cfg config.Config, candidate source.Candidate, geoIPDB *geoip2.Reader) (result Result) {
	result = Result{
		IP:           candidate.IP,
		Port:         candidate.Port,
		Source:       candidate.Source,
		Remark:       candidate.Remark,
		SourceWeight: candidate.Weight,
	}
	defer func() {
		result.Attempts = 1
		if result.Success {
			result.Successes = 1
			result.SuccessRate = 100
		}
	}()

	targetURL, err := url.Parse(cfg.Probe.Target.URL)
	if err != nil {
		result.Error = err.Error()
		return
	}
	path := targetURL.RequestURI()
	if path == "" {
		path = "/"
	}
	method := strings.ToUpper(cfg.Probe.Target.Method)
	if method == "" {
		method = http.MethodHead
	}

	timeout := cfg.Timeout()
	dialer := &net.Dialer{Timeout: timeout}
	address := net.JoinHostPort(candidate.IP, fmt.Sprintf("%d", candidate.Port))

	startTotal := time.Now()
	startTCP := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		result.TotalMS = elapsedMS(startTotal)
		result.Error = err.Error()
		return
	}
	result.TCPMS = elapsedMS(startTCP)
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		result.Error = err.Error()
		return
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: cfg.Probe.Target.SNI,
		MinVersion: tls.VersionTLS12,
	})
	startTLS := time.Now()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		result.TotalMS = elapsedMS(startTotal)
		result.Error = err.Error()
		return
	}
	result.TLSMS = elapsedMS(startTLS)
	defer tlsConn.Close()

	startHTTP := time.Now()
	request := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\nConnection: close\r\nAccept: */*\r\n\r\n",
		method, path, cfg.Probe.Target.Host, cfg.Worker.UserAgent)
	if _, err := tlsConn.Write([]byte(request)); err != nil {
		result.TotalMS = elapsedMS(startTotal)
		result.Error = err.Error()
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		result.TotalMS = elapsedMS(startTotal)
		result.Error = err.Error()
		return
	}
	defer resp.Body.Close()

	result.HTTPMS = elapsedMS(startHTTP)
	result.TotalMS = elapsedMS(startTotal)
	result.StatusCode = resp.StatusCode
	result.Colo = parseColo(resp.Header.Get("Cf-Ray"))
	result.CountryCode, result.CountryName = coloCountry(result.Colo)
	result.Success = statusOK(resp.StatusCode, cfg.Probe.Target.ExpectedStatus)
	if !result.Success {
		result.Error = resp.Status
	} else if geoIPDB != nil && result.CountryCode != "" {
		ip := net.ParseIP(result.IP)
		if ip != nil {
			record, err := geoIPDB.Country(ip)
			if err == nil && record.Country.IsoCode != "" {
				if !strings.EqualFold(record.Country.IsoCode, result.CountryCode) {
					result.Success = false
					result.Error = fmt.Sprintf("GeoIP mismatch: %s != %s", record.Country.IsoCode, result.CountryCode)
				}
			}
		}
	}
	return
}

func Sort(results []Result) {
	sort.SliceStable(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.Success != b.Success {
			return a.Success
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
}

func Keep(results []Result, keep int, per24Limit int, countries []string) []Result {
	if keep <= 0 {
		keep = 30
	}
	if per24Limit <= 0 {
		per24Limit = 2
	}
	seen24 := map[string]int{}
	out := make([]Result, 0, keep)
	for _, result := range results {
		if !result.Success {
			continue
		}
		if !countryAllowed(result.CountryCode, countries) {
			continue
		}
		key := cidrKey(result.IP)
		if key != "" && seen24[key] >= per24Limit {
			continue
		}
		if key != "" {
			seen24[key]++
		}
		out = append(out, result)
		if len(out) >= keep {
			break
		}
	}
	return out
}

func FormatADD(results []Result, remarkPrefix string) string {
	lines := make([]string, 0, len(results))
	for _, result := range results {
		remark := strings.TrimSpace(result.Remark)
		if remark == "" {
			var parts []string
			if result.CountryCode != "" {
				flag := getFlagEmoji(result.CountryCode)
				name := result.CountryName
				if name == "" {
					name = result.CountryCode
				}
				// Minimalist format: 🇯🇵 日本 (JP)
				parts = append(parts, fmt.Sprintf("%s %s (%s)", flag, name, result.CountryCode))
			} else if result.Colo != "" {
				parts = append(parts, result.Colo)
			}
			remark = strings.Join(parts, " ")
		}
		address := result.IP
		if strings.Contains(address, ":") {
			address = "[" + address + "]"
		}
		lines = append(lines, fmt.Sprintf("%s:%d#%s", address, result.Port, remark))
	}
	return strings.Join(lines, "\n")
}

func getFlagEmoji(countryCode string) string {
	if len(countryCode) != 2 {
		return ""
	}
	code := strings.ToUpper(countryCode)
	// Base for regional indicator symbols (U+1F1E6)
	const base = 0x1F1E6 - 'A'
	return string(rune(code[0])+base) + string(rune(code[1])+base)
}

func statusOK(status int, expected []int) bool {
	for _, code := range expected {
		if status == code {
			return true
		}
	}
	return false
}

func parseColo(cfRay string) string {
	if cfRay == "" {
		return ""
	}
	_, colo, ok := strings.Cut(cfRay, "-")
	if !ok {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(colo))
}

func countryAllowed(code string, countries []string) bool {
	if len(countries) == 0 {
		return true
	}
	for _, country := range countries {
		if strings.EqualFold(strings.TrimSpace(country), code) {
			return true
		}
	}
	return false
}

func coloCountry(colo string) (string, string) {
	code := coloCountryMap[strings.ToUpper(strings.TrimSpace(colo))]
	if code == "" {
		return "", ""
	}
	return code, countryNameMap[code]
}

var coloCountryMap = map[string]string{
	// Asia-Pacific
	"NRT": "JP", "KIX": "JP", "FUK": "JP", "OKA": "JP", "HND": "JP", "CTS": "JP", "NGO": "JP", "ITM": "JP",
	"SIN": "SG",
	"HKG": "HK",
	"TPE": "TW", "KHH": "TW",
	"ICN": "KR", "GMP": "KR", "PUS": "KR",
	"BKK": "TH", "DMK": "TH", "HKT": "TH",
	"KUL": "MY", "BKI": "MY", "PEN": "MY",
	"MNL": "PH", "CEB": "PH", "DVO": "PH",
	"CGK": "ID", "DPS": "ID", "SUB": "ID",
	"HAN": "VN", "SGN": "VN", "DAD": "VN",
	"SYD": "AU", "MEL": "AU", "BNE": "AU", "PER": "AU", "ADL": "AU", "CBR": "AU",
	"AKL": "NZ", "WLG": "NZ", "CHC": "NZ",
	"BOM": "IN", "DEL": "IN", "MAA": "IN", "BLR": "IN", "HYD": "IN", "CCU": "IN",
	"SZX": "CN", "CAN": "CN", "PVG": "CN", "SHA": "CN", "PEK": "CN", "PKX": "CN", "HGH": "CN", "SIA": "CN", "CKG": "CN", "TNA": "CN", "CGO": "CN", "CSX": "CN", "WUH": "CN",

	// North America
	"LAX": "US", "SJC": "US", "SFO": "US", "SEA": "US", "PDX": "US", "HNL": "US",
	"DFW": "US", "IAH": "US", "ORD": "US", "ATL": "US", "MIA": "US", "ORL": "US",
	"IAD": "US", "EWR": "US", "JFK": "US", "BOS": "US", "DEN": "US",
	"PHX": "US", "LAS": "US", "MCI": "US", "MSP": "US", "DTW": "US",
	"YVR": "CA", "YYZ": "CA", "YUL": "CA", "YYC": "CA", "YWG": "CA",

	// Europe
	"LHR": "GB", "MAN": "GB", "EDI": "GB",
	"FRA": "DE", "MUC": "DE", "DUS": "DE", "HAM": "DE", "BER": "DE",
	"CDG": "FR", "MRS": "FR",
	"AMS": "NL",
	"MAD": "ES", "BCN": "ES",
	"MIL": "IT", "FCO": "IT",
	"ZRH": "CH",
	"VIE": "AT",
	"WAW": "PL",
	"ARN": "SE",
	"CPH": "DK",
	"OSL": "NO",
	"HEL": "FI",
	"PRG": "CZ",
	"BRU": "BE",
	"LIS": "PT",
	"DUB": "IE",

	// Other common regions
	"GRU": "BR", "GIG": "BR",
	"MEX": "MX",
	"SCL": "CL",
	"EZE": "AR",
	"JNB": "ZA", "CPT": "ZA",
	"DXB": "AE",
	"DOH": "QA",
	"TLV": "IL",
}

var countryNameMap = map[string]string{
	"JP": "日本",
	"SG": "新加坡",
	"HK": "香港",
	"TW": "台湾",
	"KR": "韩国",
	"US": "美国",
	"CA": "加拿大",
	"GB": "英国",
	"DE": "德国",
	"FR": "法国",
	"NL": "荷兰",
	"ES": "西班牙",
	"IT": "意大利",
	"AU": "澳大利亚",
	"NZ": "新西兰",
	"TH": "泰国",
	"MY": "马来西亚",
	"PH": "菲律宾",
	"ID": "印尼",
	"VN": "越南",
	"IN": "印度",
	"BR": "巴西",
	"MX": "墨西哥",
	"ZA": "南非",
	"AE": "阿联酋",
}

func cidrKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	if addr.Is4() {
		b := addr.As4()
		return fmt.Sprintf("%d.%d.%d.0/24", b[0], b[1], b[2])
	}
	// For IPv6, we use /64 as the key for deduplication
	b := addr.As16()
	return fmt.Sprintf("%02x%02x:%02x%02x:%02x%02x:%02x%02x::/64",
		b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7])
}

func elapsedMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
