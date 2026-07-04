package loggingproxy

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type urlPrefixMatcher struct {
	muxes       map[string]*http.ServeMux
	registered  map[string]struct{}
	queryAll    map[string]bool
	queryPrefix map[string][]string
}

type urlPrefixMatchHandler struct{}

func (urlPrefixMatchHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

func newURLPrefixMatcher(rawPrefixes []string) (*urlPrefixMatcher, error) {
	matcher := &urlPrefixMatcher{
		muxes:       map[string]*http.ServeMux{},
		registered:  map[string]struct{}{},
		queryAll:    map[string]bool{},
		queryPrefix: map[string][]string{},
	}
	for _, rawPrefix := range rawPrefixes {
		rawPrefix = strings.TrimSpace(rawPrefix)
		if rawPrefix == "" {
			continue
		}

		parsed, err := url.Parse(rawPrefix)
		if err != nil {
			return nil, fmt.Errorf("invalid logging exclude URL prefix %q: %w", rawPrefix, err)
		}
		if parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("invalid logging exclude URL prefix %q: expected absolute URL", rawPrefix)
		}

		scheme := strings.ToLower(parsed.Scheme)
		if scheme != "http" && scheme != "https" {
			return nil, fmt.Errorf("invalid logging exclude URL prefix %q: unsupported scheme %q", rawPrefix, parsed.Scheme)
		}

		path := parsed.Path
		if path == "" {
			path = "/"
		}
		pattern := path
		if strings.HasSuffix(path, "/") {
			pattern += "{path...}"
		}

		muxKey := urlPrefixMuxKey(parsed)
		mux := matcher.muxes[muxKey]
		if mux == nil {
			mux = http.NewServeMux()
			matcher.muxes[muxKey] = mux
		}

		registrationKey := muxKey + "\x00" + pattern
		if _, ok := matcher.registered[registrationKey]; !ok {
			if err := registerURLPrefixPattern(mux, pattern); err != nil {
				return nil, fmt.Errorf("invalid logging exclude URL prefix %q: %w", rawPrefix, err)
			}
			matcher.registered[registrationKey] = struct{}{}
		}

		if parsed.RawQuery == "" {
			matcher.queryAll[registrationKey] = true
		} else {
			matcher.queryPrefix[registrationKey] = append(matcher.queryPrefix[registrationKey], parsed.RawQuery)
		}
	}
	return matcher, nil
}

func registerURLPrefixPattern(mux *http.ServeMux, pattern string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("invalid mux pattern %q: %v", pattern, recovered)
		}
	}()
	mux.Handle(pattern, urlPrefixMatchHandler{})
	return nil
}

func (m *urlPrefixMatcher) Empty() bool {
	return m == nil || len(m.muxes) == 0
}

func (m *urlPrefixMatcher) Match(target *url.URL) bool {
	if m == nil || target == nil {
		return false
	}

	mux := m.muxes[urlPrefixMuxKey(target)]
	if mux == nil {
		return false
	}

	request := &http.Request{
		Method: http.MethodGet,
		URL:    target,
		Host:   target.Host,
	}
	handler, pattern := mux.Handler(request)
	if _, ok := handler.(urlPrefixMatchHandler); !ok {
		return false
	}

	registrationKey := urlPrefixMuxKey(target) + "\x00" + pattern
	if m.queryAll[registrationKey] {
		return true
	}
	for _, queryPrefix := range m.queryPrefix[registrationKey] {
		if strings.HasPrefix(target.RawQuery, queryPrefix) {
			return true
		}
	}
	return false
}

func urlPrefixMuxKey(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "\x00" + normalizeURLHost(u)
}

func normalizeURLHost(u *url.URL) string {
	if u == nil {
		return ""
	}

	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	port := u.Port()
	if port == "" || (strings.EqualFold(u.Scheme, "https") && port == "443") || (strings.EqualFold(u.Scheme, "http") && port == "80") {
		return host
	}
	return net.JoinHostPort(host, port)
}
