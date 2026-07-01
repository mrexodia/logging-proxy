package loggingproxy

import (
	"fmt"
	"net"
	"strings"
)

type mitmExcludeMatcher struct {
	matchAll   bool
	exactHosts map[string]struct{}
	cidrs      []*net.IPNet
	suffixes   []string
}

func newMITMExcludeMatcher(patterns []string) (*mitmExcludeMatcher, error) {
	matcher := &mitmExcludeMatcher{
		exactHosts: map[string]struct{}{},
	}

	for _, rawPattern := range patterns {
		pattern := normalizeMITMHost(rawPattern)
		if pattern == "" {
			continue
		}
		if pattern == "*" {
			matcher.matchAll = true
			continue
		}
		if strings.Contains(pattern, "/") {
			_, cidr, err := net.ParseCIDR(pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid MITM exclude host %q: %w", rawPattern, err)
			}
			matcher.cidrs = append(matcher.cidrs, cidr)
			continue
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := strings.TrimPrefix(pattern, "*")
			if suffix == "." {
				return nil, fmt.Errorf("invalid MITM exclude host %q", rawPattern)
			}
			matcher.suffixes = append(matcher.suffixes, suffix)
			continue
		}

		matcher.exactHosts[pattern] = struct{}{}
	}

	return matcher, nil
}

func (m *mitmExcludeMatcher) Match(host string) bool {
	if m == nil {
		return false
	}
	if m.matchAll {
		return true
	}

	host = normalizeMITMHost(host)
	if host == "" {
		return false
	}

	if _, ok := m.exactHosts[host]; ok {
		return true
	}

	if ip := net.ParseIP(host); ip != nil {
		for _, cidr := range m.cidrs {
			if cidr.Contains(ip) {
				return true
			}
		}
	}

	for _, suffix := range m.suffixes {
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return true
		}
	}

	return false
}

func normalizeMITMHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}

	if splitHost, _, err := net.SplitHostPort(host); err == nil {
		host = splitHost
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}
