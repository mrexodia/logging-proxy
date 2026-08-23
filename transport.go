package loggingproxy

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/http/httpproxy"
)

// HTTPClientProxyEnvironment contains the effective process proxy settings.
type HTTPClientProxyEnvironment struct {
	HTTPProxy  string
	HTTPSProxy string
	NoProxy    string
}

// ReadHTTPClientProxyEnvironment returns the effective process proxy settings.
// ALL_PROXY/all_proxy fills either scheme-specific proxy when it is unset.
func ReadHTTPClientProxyEnvironment() HTTPClientProxyEnvironment {
	config := proxyConfigFromEnvironment()
	return HTTPClientProxyEnvironment{
		HTTPProxy:  config.HTTPProxy,
		HTTPSProxy: config.HTTPSProxy,
		NoProxy:    config.NoProxy,
	}
}

// HTTPClientProxyConfig configures the upstream proxy used by outbound HTTP clients.
type HTTPClientProxyConfig struct {
	// ProxyURL forces all outbound HTTP client traffic through this proxy.
	// If set, ProxyFromEnvironment is ignored.
	ProxyURL string

	// ProxyFromEnvironment enables HTTP_PROXY, HTTPS_PROXY, ALL_PROXY, and NO_PROXY lookup.
	// Nil defaults to true.
	ProxyFromEnvironment *bool
}

func newHTTPTransport(proxyConfig HTTPClientProxyConfig) (*http.Transport, error) {
	transport := cloneDefaultTransport()

	proxyFunc, err := proxyConfig.proxyFunc()
	if err != nil {
		return nil, err
	}
	transport.Proxy = proxyFunc

	return transport, nil
}

func newHTTPClient(proxyConfig HTTPClientProxyConfig) (*http.Client, error) {
	transport, err := newHTTPTransport(proxyConfig)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport}, nil
}

func newDirectTransport() *http.Transport {
	transport := cloneDefaultTransport()
	transport.Proxy = nil
	return transport
}

func newDirectHTTPClient() *http.Client {
	return &http.Client{Transport: newDirectTransport()}
}

func cloneDefaultTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		clone := transport.Clone()
		clone.Proxy = nil
		return clone
	}

	return &http.Transport{Proxy: nil}
}

func (config HTTPClientProxyConfig) proxyFunc() (func(*http.Request) (*url.URL, error), error) {
	if strings.TrimSpace(config.ProxyURL) != "" {
		proxyURL, err := parseProxyURL(config.ProxyURL)
		if err != nil {
			return nil, err
		}
		return http.ProxyURL(proxyURL), nil
	}

	if config.proxyFromEnvironmentEnabled() {
		return proxyFromEnvironment, nil
	}

	return nil, nil
}

func (config HTTPClientProxyConfig) proxyFromEnvironmentEnabled() bool {
	return config.ProxyFromEnvironment == nil || *config.ProxyFromEnvironment
}

func ParseHTTPClientProxyURL(rawProxyURL string) (*url.URL, error) {
	return parseProxyURL(rawProxyURL)
}

func parseProxyURL(rawProxyURL string) (*url.URL, error) {
	rawProxyURL = strings.TrimSpace(rawProxyURL)
	if rawProxyURL == "" {
		return nil, nil
	}

	proxyURL, err := url.Parse(rawProxyURL)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		proxyURLWithScheme, schemeErr := url.Parse("http://" + rawProxyURL)
		if schemeErr == nil && proxyURLWithScheme.Scheme != "" && proxyURLWithScheme.Host != "" {
			proxyURL = proxyURLWithScheme
			err = nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("invalid HTTP client proxy URL %q: %w", rawProxyURL, err)
	}
	if proxyURL == nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, fmt.Errorf("invalid HTTP client proxy URL %q: expected absolute URL or host:port", rawProxyURL)
	}

	switch strings.ToLower(proxyURL.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return proxyURL, nil
	default:
		return nil, fmt.Errorf("invalid HTTP client proxy URL %q: unsupported scheme %q", rawProxyURL, proxyURL.Scheme)
	}
}

func proxyConfigFromEnvironment() *httpproxy.Config {
	config := httpproxy.FromEnvironment()
	allProxy := firstEnvironmentValue("ALL_PROXY", "all_proxy")
	if config.HTTPProxy == "" {
		config.HTTPProxy = allProxy
	}
	if config.HTTPSProxy == "" {
		config.HTTPSProxy = allProxy
	}
	return config
}

func firstEnvironmentValue(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func proxyFromEnvironment(request *http.Request) (*url.URL, error) {
	if request == nil || request.URL == nil {
		return nil, nil
	}
	return proxyConfigFromEnvironment().ProxyFunc()(request.URL)
}
