package source

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/grootpxw/edgetunnel-bestsub/internal/config"
)

type Candidate struct {
	IP     string `json:"ip"`
	Port   int    `json:"port"`
	Remark string `json:"remark"`
	Source string `json:"source"`
	Weight int    `json:"weight"`
}

func (c Candidate) Address() string {
	return netip.AddrPortFrom(netip.MustParseAddr(c.IP), uint16(c.Port)).String()
}

func Load(ctx context.Context, cfg config.Config) ([]Candidate, error) {
	var tokens []sourceToken
	for _, src := range cfg.Sources {
		text, err := readSource(ctx, src)
		if err != nil {
			continue
		}
		for _, token := range splitTokens(text) {
			tokens = append(tokens, sourceToken{Value: token, Source: src.Name, Weight: src.Weight})
		}
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("no candidates loaded from sources")
	}

	var cidrs []sourceToken
	var direct []Candidate
	for _, token := range tokens {
		candidates, isCIDR := parseToken(token, cfg)
		if isCIDR {
			cidrs = append(cidrs, token)
			continue
		}
		direct = append(direct, candidates...)
	}

	limit := cfg.Probe.CandidateLimit
	if limit < len(direct) {
		limit = len(direct)
	}
	randomBudget := max(0, limit-len(direct))
	randoms := generateFromCIDRs(cidrs, cfg, randomBudget)

	candidates := dedupe(append(direct, randoms...))
	if len(candidates) == 0 {
		return nil, fmt.Errorf("sources were loaded, but no valid IP candidates were parsed")
	}
	return candidates, nil
}

type sourceToken struct {
	Value  string
	Source string
	Weight int
}

func readSource(ctx context.Context, src config.SourceConfig) (string, error) {
	switch strings.ToLower(src.Type) {
	case "remote", "remote_cidr":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
		if err != nil {
			return "", err
		}
		client := &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				Proxy: nil,
			},
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("%s returned %s", src.URL, resp.Status)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		return string(data), err
	case "file":
		data, err := os.ReadFile(src.Path)
		return string(data), err
	default:
		return "", fmt.Errorf("unsupported source type %q", src.Type)
	}
}

var tokenSplit = regexp.MustCompile(`[,\s"']+`)

func splitTokens(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "#") {
			out = append(out, line)
			continue
		}
		for _, part := range tokenSplit.Split(line, -1) {
			part = strings.TrimSpace(part)
			if part == "" || strings.HasPrefix(part, "#") {
				continue
			}
			out = append(out, part)
		}
	}
	return out
}

func parseToken(token sourceToken, cfg config.Config) ([]Candidate, bool) {
	value, remark := splitRemark(token.Value)
	if prefix, err := netip.ParsePrefix(value); err == nil {
		is4 := prefix.Addr().Is4()
		if is4 && !cfg.Probe.IPv4 {
			return nil, false
		}
		if !is4 && !cfg.Probe.IPv6 {
			return nil, false
		}
		return nil, true
	}

	if ap, err := parseAddrPort(value); err == nil {
		is4 := ap.Addr().Is4()
		if is4 && !cfg.Probe.IPv4 {
			return nil, false
		}
		if !is4 && !cfg.Probe.IPv6 {
			return nil, false
		}
		return []Candidate{{
			IP:     ap.Addr().String(),
			Port:   int(ap.Port()),
			Remark: remark,
			Source: token.Source,
			Weight: token.Weight,
		}}, false
	}
	if addr, err := netip.ParseAddr(value); err == nil {
		is4 := addr.Is4()
		if is4 && !cfg.Probe.IPv4 {
			return nil, false
		}
		if !is4 && !cfg.Probe.IPv6 {
			return nil, false
		}
		out := make([]Candidate, 0, len(cfg.Probe.Ports))
		for _, port := range cfg.Probe.Ports {
			out = append(out, Candidate{
				IP:     addr.String(),
				Port:   port,
				Remark: remark,
				Source: token.Source,
				Weight: token.Weight,
			})
		}
		return out, false
	}
	return nil, false
}

func splitRemark(value string) (string, string) {
	before, after, ok := strings.Cut(value, "#")
	if !ok {
		return value, ""
	}
	return before, after
}

func parseAddrPort(value string) (netip.AddrPort, error) {
	if ap, err := netip.ParseAddrPort(value); err == nil {
		return ap, nil
	}
	host, portText, ok := strings.Cut(value, ":")
	if !ok || strings.Contains(host, ":") {
		return netip.AddrPort{}, fmt.Errorf("not addr:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return netip.AddrPort{}, fmt.Errorf("invalid port")
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(addr, uint16(port)), nil
}

func generateFromCIDRs(tokens []sourceToken, cfg config.Config, budget int) []Candidate {
	if budget <= 0 || len(tokens) == 0 {
		return nil
	}
	out := make([]Candidate, 0, budget)
	for len(out) < budget {
		hasMatch := false
		for _, token := range tokens {
			if len(out) >= budget {
				break
			}
			prefix, err := netip.ParsePrefix(token.Value)
			if err != nil {
				continue
			}
			is4 := prefix.Addr().Is4()
			if is4 && !cfg.Probe.IPv4 {
				continue
			}
			if !is4 && !cfg.Probe.IPv6 {
				continue
			}
			hasMatch = true

			var addr netip.Addr
			if is4 {
				addr, err = randomIPv4(prefix)
			} else {
				addr, err = randomIPv6(prefix)
			}
			if err != nil {
				continue
			}
			port := cfg.Probe.Ports[randomInt(len(cfg.Probe.Ports))]
			out = append(out, Candidate{
				IP:     addr.String(),
				Port:   port,
				Source: token.Source,
				Weight: token.Weight,
			})
		}
		if !hasMatch {
			break
		}
	}
	return out
}

func randomIPv4(prefix netip.Prefix) (netip.Addr, error) {
	addr := prefix.Masked().Addr()
	bits := prefix.Bits()
	if bits < 0 || bits > 32 {
		return netip.Addr{}, fmt.Errorf("invalid ipv4 prefix")
	}
	base := binary.BigEndian.Uint32(addr.AsSlice())
	hostBits := 32 - bits
	var offset uint32
	if hostBits > 0 {
		maxOffset := uint64(1) << hostBits
		offset = uint32(randomUint64(maxOffset))
	}
	mask := ^uint32(0)
	if hostBits >= 32 {
		mask = 0
	} else {
		mask <<= hostBits
	}
	ip := (base & mask) + offset
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], ip)
	return netip.AddrFrom4(b), nil
}

func randomIPv6(prefix netip.Prefix) (netip.Addr, error) {
	addr := prefix.Masked().Addr()
	bits := prefix.Bits()
	if bits < 0 || bits > 128 {
		return netip.Addr{}, fmt.Errorf("invalid ipv6 prefix")
	}

	slice := addr.As16()
	startByte := bits / 8
	for i := startByte; i < 16; i++ {
		r := byte(randomInt(256))
		if i == startByte {
			mask := byte(0xFF) >> (bits % 8)
			slice[i] |= (r & mask)
		} else {
			slice[i] = r
		}
	}

	return netip.AddrFrom16(slice), nil
}

func randomInt(n int) int {
	if n <= 1 {
		return 0
	}
	return int(randomUint64(uint64(n)))
}

func randomUint64(max uint64) uint64 {
	if max <= 1 {
		return 0
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano()) % max
	}
	return binary.BigEndian.Uint64(b[:]) % max
}

func dedupe(in []Candidate) []Candidate {
	seen := map[string]struct{}{}
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		if c.IP == "" || c.Port == 0 {
			continue
		}
		key := fmt.Sprintf("%s:%d", c.IP, c.Port)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, c)
	}
	return out
}
