package skirk

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const defaultGoogleIPListFile = "assets/ip-list.txt"

type cachedListStore struct {
	mu         sync.RWMutex
	path       string
	entries    []string
	loadedPath string
	loadedAt   time.Time
	rotation   atomic.Uint64
}

var cache_list = &cachedListStore{path: defaultGoogleIPListPath()}

type googleIPCandidate struct {
	ip      string
	latency time.Duration
}

func defaultGoogleIP() string {
	return cache_list.Path()
}

func DefaultGoogleIP() string {
	return defaultGoogleIP()
}

func defaultGoogleIPListPath() string {
	if value := strings.TrimSpace(os.Getenv("SKIRK_GOOGLE_IP_LIST")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("SKIRK_CACHED_LIST")); value != "" {
		return value
	}
	return defaultGoogleIPListFile
}

func (c *cachedListStore) Path() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if trimmed := strings.TrimSpace(c.path); trimmed != "" {
		return trimmed
	}
	return defaultGoogleIPListPath()
}

func (c *cachedListStore) SetPath(spec string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		trimmed = defaultGoogleIPListPath()
	}
	if c.path != trimmed {
		c.path = trimmed
		c.entries = nil
		c.loadedPath = ""
		c.loadedAt = time.Time{}
		c.rotation.Store(0)
	}
}

func (c *cachedListStore) SetEntries(entries []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = normalizeGoogleIPEntries(entries)
	c.loadedPath = c.path
	c.loadedAt = time.Now().UTC()
	c.rotation.Store(0)
}

func (c *cachedListStore) Entries(spec string, limit int) ([]string, error) {
	ips, err := c.resolve(spec, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ips))
	copy(out, ips)
	return out, nil
}

func resolveGoogleIPSpec(spec string) (string, error) {
	ips, err := resolveGoogleIPList(spec, 1)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", errors.New("Google edge IP did not produce any IP")
	}
	return ips[0], nil
}

func resolveGoogleIPList(spec string, limit int) ([]string, error) {
	return cache_list.Entries(spec, limit)
}

func (c *cachedListStore) resolve(spec string, limit int) ([]string, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		trimmed = c.Path()
	}
	if limit <= 0 {
		limit = 12
	}

	if ip := net.ParseIP(trimmed); ip != nil {
		return []string{ip.String()}, nil
	}

	c.mu.RLock()
	cachedPath := c.loadedPath
	cachedEntries := append([]string(nil), c.entries...)
	c.mu.RUnlock()

	if cachedPath == trimmed && len(cachedEntries) > 0 {
		if limit < len(cachedEntries) {
			return append([]string(nil), cachedEntries[:limit]...), nil
		}
		return cachedEntries, nil
	}

	data, path, err := readGoogleIPList(trimmed)
	if err != nil {
		return nil, err
	}
	raw := normalizeGoogleIPEntries(parseGoogleIPsFromList(string(data)))
	if len(raw) == 0 {
		return nil, fmt.Errorf("google IP list %q did not contain a valid IP address", path)
	}

	ordered := prioritizeGoogleIPsByLatency(raw)

	c.mu.Lock()
	c.path = trimmed
	c.entries = append([]string(nil), ordered...)
	c.loadedPath = trimmed
	c.loadedAt = time.Now().UTC()
	c.mu.Unlock()

	if limit < len(ordered) {
		return append([]string(nil), ordered[:limit]...), nil
	}
	return ordered, nil
}

func normalizeGoogleIPEntries(entries []string) []string {
	seen := make(map[string]struct{}, len(entries))
	normalized := make([]string, 0, len(entries))
	for _, entry := range entries {
		ip := net.ParseIP(strings.TrimSpace(entry))
		if ip == nil {
			continue
		}
		canonical := ip.String()
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		normalized = append(normalized, canonical)
	}
	return normalized
}

func parseGoogleIPsFromList(text string) []string {
	ips := make([]string, 0, 32)
	seen := make(map[string]struct{}, 32)
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
			if line == "" {
				continue
			}
		}
		fields := strings.FieldsFunc(line, func(r rune) bool {
			switch r {
			case ',', ';', '	', ' ':
				return true
			default:
				return false
			}
		})
		candidates := append([]string(nil), fields...)
		candidates = append(candidates, strings.Trim(line, `"'`))
		for _, field := range candidates {
			field = strings.TrimSpace(strings.Trim(field, `"'`))
			if field == "" {
				continue
			}
			ip := net.ParseIP(field)
			if ip == nil {
				continue
			}
			canonical := ip.String()
			if _, ok := seen[canonical]; ok {
				continue
			}
			seen[canonical] = struct{}{}
			ips = append(ips, canonical)
		}
	}
	return ips
}

func prioritizeGoogleIPsByLatency(ips []string) []string {
	if len(ips) <= 1 {
		return ips
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SKIRK_GOOGLE_IP_PROBE_DISABLE")), "1") {
		return ips
	}
	probeTimeout := 450 * time.Millisecond
	if raw := strings.TrimSpace(os.Getenv("SKIRK_GOOGLE_IP_PROBE_TIMEOUT_MS")); raw != "" {
		if ms, err := time.ParseDuration(raw + "ms"); err == nil && ms >= 100*time.Millisecond && ms <= 5*time.Second {
			probeTimeout = ms
		}
	}
	probeCount := len(ips)
	if probeCount > 6 {
		probeCount = 6
	}
	candidates := make([]googleIPCandidate, 0, probeCount)
	for _, ip := range ips[:probeCount] {
		candidates = append(candidates, googleIPCandidate{
			ip:      ip,
			latency: measureGoogleIPLatency(ip, probeTimeout),
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].latency < candidates[j].latency
	})
	ordered := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ordered = append(ordered, candidate.ip)
	}
	for _, ip := range ips[probeCount:] {
		ordered = append(ordered, ip)
	}
	return ordered
}

func measureGoogleIPLatency(ip string, timeout time.Duration) time.Duration {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "443"), timeout)
	if err != nil {
		return timeout + (10 * time.Second)
	}
	_ = conn.Close()
	latency := time.Since(start)
	if latency <= 0 {
		return time.Millisecond
	}
	return latency
}

func readGoogleIPList(spec string) ([]byte, string, error) {
	if data, err := os.ReadFile(spec); err == nil {
		return data, spec, nil
	}
	if filepath.IsAbs(spec) {
		return nil, spec, fmt.Errorf("could not read Google IP list %q", spec)
	}

	candidates := []string{spec}
	if cwd, err := os.Getwd(); err == nil {
		for dir := cwd; ; dir = filepath.Dir(dir) {
			candidates = append(candidates, filepath.Join(dir, spec))
			candidates = append(candidates, filepath.Join(dir, "resources", spec))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		for dir := filepath.Dir(exe); ; dir = filepath.Dir(dir) {
			candidates = append(candidates, filepath.Join(dir, spec))
			candidates = append(candidates, filepath.Join(dir, "resources", spec))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
		}
	}

	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		if data, err := os.ReadFile(candidate); err == nil {
			return data, candidate, nil
		}
	}
	return nil, spec, fmt.Errorf("could not read Google IP list %q", spec)
}
