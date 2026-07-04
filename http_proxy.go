package loggingproxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/google/uuid"
	golangproxy "golang.org/x/net/proxy"
)

type HTTPProxyAuthConfig struct {
	Username string
	Password string
}

type HTTPProxyOptions struct {
	Logger                    Logger
	MITM                      bool
	MITMCA                    *MITMCA
	MITMIncludeHosts          []string
	MITMExcludeHosts          []string
	LoggingExcludeURLPrefixes []string
	UpstreamTLSConfig         *tls.Config
	ClientProxy               HTTPClientProxyConfig
	Auth                      HTTPProxyAuthConfig
	Verbose                   bool
}

type HTTPProxyServer struct {
	proxy                     *goproxy.ProxyHttpServer
	logger                    Logger
	authenticator             *httpProxyAuthenticator
	mitmEnabled               bool
	mitmCA                    *MITMCA
	mitmInclude               *mitmExcludeMatcher
	mitmExclude               *mitmExcludeMatcher
	loggingExcludeURLPrefixes *urlPrefixMatcher
}

type httpProxyAuthenticator struct {
	username string
	password string
}

type httpProxyRequestState struct {
	metadata    RequestMetadata
	requestTime time.Time
}

type teeReadCloser struct {
	source          io.ReadCloser
	writer          *io.PipeWriter
	once            sync.Once
	loggingDisabled bool
}

type contextDialerFunc func(context.Context, string, string) (net.Conn, error)

func (f contextDialerFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

func (f contextDialerFunc) Dial(network, addr string) (net.Conn, error) {
	return f(context.Background(), network, addr)
}

func NewHTTPProxyServer(options HTTPProxyOptions) (*HTTPProxyServer, error) {
	logger := options.Logger
	if logger == nil {
		logger = &NoOpLogger{}
	}

	authenticator, err := newHTTPProxyAuthenticator(options.Auth)
	if err != nil {
		return nil, err
	}

	transport, err := newHTTPTransport(options.ClientProxy)
	if err != nil {
		return nil, err
	}
	// Keep the upstream transport from adding implicit Accept-Encoding: gzip and
	// auto-decompressing responses. If the client explicitly asks for compressed
	// data, those compressed bytes should be proxied through unchanged; logging
	// can decompress its tee'd copy separately.
	transport.DisableCompression = true
	if options.UpstreamTLSConfig != nil {
		transport.TLSClientConfig = options.UpstreamTLSConfig.Clone()
	}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}

	mitmInclude, err := newMITMIncludeMatcher(options.MITMIncludeHosts)
	if err != nil {
		return nil, err
	}
	mitmExclude, err := newMITMExcludeMatcher(options.MITMExcludeHosts)
	if err != nil {
		return nil, err
	}
	loggingExcludeURLPrefixes, err := newURLPrefixMatcher(options.LoggingExcludeURLPrefixes)
	if err != nil {
		return nil, err
	}

	proxy := goproxy.NewProxyHttpServer()
	proxy.Tr = transport
	proxy.ConnectDial = nil
	proxy.ConnectDialWithReq = nil
	if transport.Proxy != nil {
		proxy.ConnectDialWithReq = newConnectDialWithHTTPClientProxy(proxy, transport, transport.Proxy)
	}
	// Preserve client Accept-Encoding so compressed request/response streams are
	// proxied through unchanged. The logging path only sees a tee'd copy.
	proxy.KeepAcceptEncoding = true
	proxy.KeepHeader = false
	proxy.Verbose = options.Verbose
	if options.Verbose {
		proxy.Logger = log.Default()
	} else {
		proxy.Logger = log.New(io.Discard, "", 0)
	}

	server := &HTTPProxyServer{
		proxy:                     proxy,
		logger:                    logger,
		authenticator:             authenticator,
		mitmEnabled:               options.MITM,
		mitmCA:                    options.MITMCA,
		mitmInclude:               mitmInclude,
		mitmExclude:               mitmExclude,
		loggingExcludeURLPrefixes: loggingExcludeURLPrefixes,
	}

	if server.authenticator != nil {
		proxy.OnRequest().HandleConnect(goproxy.FuncHttpsHandler(server.handleConnectAuth))
		proxy.OnRequest().DoFunc(server.handleRequestAuth)
	}

	if options.MITM {
		if options.MITMCA == nil {
			return nil, fmt.Errorf("MITM mode requires a MITM CA")
		}
		mitmAction := &goproxy.ConnectAction{Action: goproxy.ConnectMitm, TLSConfig: options.MITMCA.TLSConfigForHost()}
		proxy.OnRequest().HandleConnect(goproxy.FuncHttpsHandler(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
			if !server.shouldMITMHost(host) {
				server.logHTTPProxyConnect(host, ctx)
				return goproxy.OkConnect, host
			}
			return mitmAction, host
		}))
	}

	proxy.OnRequest().DoFunc(server.handleRequest)
	proxy.OnResponse().DoFunc(server.handleResponse)

	return server, nil
}

func newConnectDialWithHTTPClientProxy(proxy *goproxy.ProxyHttpServer, transport *http.Transport, proxyFunc func(*http.Request) (*url.URL, error)) func(*http.Request, string, string) (net.Conn, error) {
	return func(request *http.Request, network, addr string) (net.Conn, error) {
		proxyRequest := requestForConnectProxyLookup(request, addr)
		proxyURL, err := proxyFunc(proxyRequest)
		if err != nil {
			return nil, err
		}
		if proxyURL == nil {
			return dialDirect(transport, request, network, addr)
		}

		if isSOCKSProxyURL(proxyURL) {
			return dialSOCKSProxy(transport, request, proxyURL, network, addr)
		}

		dial := proxy.NewConnectDialToProxyWithHandler(proxyURL.String(), proxyConnectRequestHandler(proxyURL))
		if dial == nil {
			return nil, fmt.Errorf("unsupported HTTP client proxy scheme %q for CONNECT", proxyURL.Scheme)
		}
		return dial(network, addr)
	}
}

func requestForConnectProxyLookup(original *http.Request, addr string) *http.Request {
	proxyURL := &url.URL{Scheme: "https", Host: addr}
	if original == nil {
		return &http.Request{URL: proxyURL}
	}

	clone := original.Clone(original.Context())
	clone.URL = proxyURL
	return clone
}

func dialDirect(transport *http.Transport, request *http.Request, network, addr string) (net.Conn, error) {
	ctx := context.Background()
	if request != nil {
		ctx = request.Context()
	}
	return dialDirectContext(transport, ctx, network, addr)
}

func dialDirectContext(transport *http.Transport, ctx context.Context, network, addr string) (net.Conn, error) {
	if transport != nil && transport.DialContext != nil {
		return transport.DialContext(ctx, network, addr)
	}
	if transport != nil && transport.Dial != nil {
		return transport.Dial(network, addr)
	}

	var dialer net.Dialer
	return dialer.DialContext(ctx, network, addr)
}

func dialSOCKSProxy(transport *http.Transport, request *http.Request, proxyURL *url.URL, network, addr string) (net.Conn, error) {
	ctx := context.Background()
	if request != nil {
		ctx = request.Context()
	}

	forward := contextDialerFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialDirectContext(transport, ctx, network, addr)
	})

	dialer, err := golangproxy.SOCKS5("tcp", proxyURL.Host, socksProxyAuth(proxyURL), forward)
	if err != nil {
		return nil, err
	}
	if contextDialer, ok := dialer.(golangproxy.ContextDialer); ok {
		return contextDialer.DialContext(ctx, network, addr)
	}
	return dialer.Dial(network, addr)
}

func isSOCKSProxyURL(proxyURL *url.URL) bool {
	if proxyURL == nil {
		return false
	}
	scheme := strings.ToLower(proxyURL.Scheme)
	return scheme == "socks5" || scheme == "socks5h"
}

func socksProxyAuth(proxyURL *url.URL) *golangproxy.Auth {
	if proxyURL == nil || proxyURL.User == nil {
		return nil
	}

	password, _ := proxyURL.User.Password()
	return &golangproxy.Auth{
		User:     proxyURL.User.Username(),
		Password: password,
	}
}

func proxyConnectRequestHandler(proxyURL *url.URL) func(*http.Request) {
	if proxyURL == nil || proxyURL.User == nil {
		return nil
	}

	username := proxyURL.User.Username()
	password, _ := proxyURL.User.Password()
	token := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return func(request *http.Request) {
		request.Header.Set("Proxy-Authorization", "Basic "+token)
	}
}

func (s *HTTPProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.mitmCA != nil && r.URL != nil && r.URL.Path == "/crl" && !r.URL.IsAbs() {
		s.mitmCA.ServeCRL(w, r)
		return
	}
	s.proxy.ServeHTTP(w, r)
}

func (s *HTTPProxyServer) shouldMITMHost(host string) bool {
	if s.mitmExclude.Match(host) {
		return false
	}
	if !s.mitmInclude.Empty() && !s.mitmInclude.Match(host) {
		return false
	}
	return true
}

func (s *HTTPProxyServer) shouldLogHost(host string) bool {
	if s.mitmExclude.Match(host) {
		return false
	}
	if !s.mitmInclude.Empty() && !s.mitmInclude.Match(host) {
		return false
	}
	return true
}

func (s *HTTPProxyServer) shouldLogURL(targetURL *url.URL) bool {
	if targetURL == nil {
		return false
	}
	if !s.shouldLogHost(targetURL.Host) {
		return false
	}
	if s.loggingExcludeURLPrefixes.Match(targetURL) {
		return false
	}
	return true
}

func newHTTPProxyAuthenticator(config HTTPProxyAuthConfig) (*httpProxyAuthenticator, error) {
	if config.Username == "" && config.Password == "" {
		return nil, nil
	}
	if config.Username == "" || config.Password == "" {
		return nil, fmt.Errorf("proxy authentication requires both username and password")
	}
	return &httpProxyAuthenticator{username: config.Username, password: config.Password}, nil
}

func (a *httpProxyAuthenticator) Valid(header string) bool {
	if a == nil {
		return true
	}

	username, password, ok := parseProxyBasicAuth(header)
	if !ok {
		return false
	}
	usernameOK := subtle.ConstantTimeCompare([]byte(username), []byte(a.username)) == 1
	passwordOK := subtle.ConstantTimeCompare([]byte(password), []byte(a.password)) == 1
	return usernameOK && passwordOK
}

func parseProxyBasicAuth(header string) (string, string, bool) {
	const prefix = "Basic "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", false
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	return username, password, ok
}

func (s *HTTPProxyServer) handleRequestAuth(request *http.Request, _ *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	if s.authenticator == nil {
		return request, nil
	}
	if request != nil && s.authenticator.Valid(request.Header.Get("Proxy-Authorization")) {
		request.Header.Del("Proxy-Authorization")
		return request, nil
	}
	return request, proxyAuthRequiredResponse(request)
}

func (s *HTTPProxyServer) handleConnectAuth(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
	if s.authenticator == nil {
		return nil, host
	}

	var request *http.Request
	if ctx != nil {
		request = ctx.Req
	}
	if request != nil && s.authenticator.Valid(request.Header.Get("Proxy-Authorization")) {
		request.Header.Del("Proxy-Authorization")
		return nil, host
	}
	if ctx != nil {
		ctx.Resp = proxyAuthRequiredResponse(request)
	}
	return goproxy.RejectConnect, host
}

func proxyAuthRequiredResponse(request *http.Request) *http.Response {
	body := "proxy authentication required\n"
	response := &http.Response{
		Request:       request,
		Header:        make(http.Header),
		StatusCode:    http.StatusProxyAuthRequired,
		Status:        fmt.Sprintf("%d %s", http.StatusProxyAuthRequired, http.StatusText(http.StatusProxyAuthRequired)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		ContentLength: int64(len(body)),
		Body:          io.NopCloser(strings.NewReader(body)),
	}
	response.Header.Set("Content-Type", goproxy.ContentTypeText)
	response.Header.Set("Proxy-Authenticate", `Basic realm="logging-proxy"`)
	response.Header.Set("Connection", "close")
	return response
}

func (s *HTTPProxyServer) logHTTPProxyConnect(host string, ctx *goproxy.ProxyCtx) {
	connectLogger, ok := s.logger.(ConnectLogger)
	if !ok {
		return
	}

	timestamp := time.Now()
	method := http.MethodConnect
	target := host
	if ctx != nil && ctx.Req != nil {
		if ctx.Req.Method != "" {
			method = ctx.Req.Method
		}
		if ctx.Req.Host != "" {
			target = ctx.Req.Host
		}
		if ctx.Req.URL != nil && ctx.Req.URL.Host != "" {
			target = ctx.Req.URL.Host
		}
	}

	metadata := RequestMetadata{
		ID:               uuid.New().String(),
		Pattern:          "HTTP_PROXY_CONNECT",
		Method:           method,
		SourceURL:        target,
		DestinationURL:   target,
		RequestStartedAt: timestamp,
	}
	connectLogger.LogConnect(metadata, timestamp)
}

func (s *HTTPProxyServer) handleRequest(request *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	if request == nil || request.URL == nil {
		return request, nil
	}

	requestTime := time.Now()
	targetURL := cloneURL(request.URL)
	requestContentEncoding := request.Header.Get("Content-Encoding")
	pattern := "HTTP_PROXY"
	if s.mitmEnabled && strings.EqualFold(targetURL.Scheme, "https") {
		pattern = "HTTP_PROXY_MITM"
	} else if strings.EqualFold(targetURL.Scheme, "https") {
		pattern = "HTTP_PROXY_HTTPS"
	}

	if !s.shouldLogURL(targetURL) {
		ctx.UserData = nil
		return request, nil
	}

	metadata := RequestMetadata{
		ID:                     uuid.New().String(),
		Pattern:                pattern,
		Method:                 request.Method,
		SourceURL:              targetURL.String(),
		DestinationURL:         targetURL.String(),
		RequestContentEncoding: requestContentEncoding,
	}
	ctx.UserData = &httpProxyRequestState{metadata: metadata, requestTime: requestTime}

	requestHeaders := request.Header.Clone()
	request.Body = wrapBodyForLogging(request.Body, func(body io.ReadCloser) {
		s.logHTTPProxyRequest(metadata, requestTime, request.Method, targetURL.String(), request.Proto, requestHeaders, requestContentEncoding, body)
	})

	return request, nil
}

func (s *HTTPProxyServer) handleResponse(response *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
	if response == nil {
		return response
	}

	state, ok := ctx.UserData.(*httpProxyRequestState)
	if !ok || state == nil {
		return response
	}

	metadata := state.metadata
	responseTime := time.Now()
	responseHeaders := response.Header.Clone()
	responseContentEncoding := responseHeaders.Get("Content-Encoding")
	upstreamProto := response.Proto
	metadata.ResponseContentEncoding = responseContentEncoding

	// goproxy's MITM path serializes the upstream *http.Response with
	// response.Write(client). If the upstream connection used HTTP/2, leaving
	// response.Proto as HTTP/2.0 would write an invalid HTTP/2 status line (and
	// even Transfer-Encoding: chunked) onto the MITM client's HTTP/1.x TLS stream.
	// Preserve the upstream protocol in the log, but make the response written to
	// the client match the protocol used by that client request.
	if ctx != nil && ctx.Req != nil && strings.EqualFold(metadata.Pattern, "HTTP_PROXY_MITM") {
		response.Proto = ctx.Req.Proto
		response.ProtoMajor = ctx.Req.ProtoMajor
		response.ProtoMinor = ctx.Req.ProtoMinor
	}

	response.Body = wrapBodyForLogging(response.Body, func(body io.ReadCloser) {
		s.logHTTPProxyResponse(metadata, responseTime, upstreamProto, response.Status, responseHeaders, responseContentEncoding, body)
	})

	return response
}

func (s *HTTPProxyServer) logHTTPProxyRequest(metadata RequestMetadata, timestamp time.Time, method, target, proto string, headers http.Header, contentEncoding string, body io.ReadCloser) {
	defer body.Close()

	var headerBuf bytes.Buffer
	fmt.Fprintf(&headerBuf, "%s %s %s\r\n", method, target, proto)
	for name, values := range headers {
		if shouldSkipLoggedRequestHeader(name) {
			continue
		}
		for _, value := range values {
			fmt.Fprintf(&headerBuf, "%s: %s\r\n", name, value)
		}
	}
	headerBuf.WriteString("\r\n")

	var bodyReader io.Reader = body
	if contentEncoding != "" {
		decompressed, err := decompressReader(body, contentEncoding)
		if err != nil {
			fmt.Fprintf(&headerBuf, "X-Decompression-Error: %v\r\n", err)
		} else {
			defer decompressed.Close()
			bodyReader = decompressed
		}
	}

	s.logger.LogRequest(metadata, timestamp, &readCloser{
		Reader: io.MultiReader(&headerBuf, bodyReader),
		Closer: io.NopCloser(nil),
	})
}

func (s *HTTPProxyServer) logHTTPProxyResponse(metadata RequestMetadata, timestamp time.Time, proto, status string, headers http.Header, contentEncoding string, body io.ReadCloser) {
	defer body.Close()

	var headerBuf bytes.Buffer
	fmt.Fprintf(&headerBuf, "%s %s\r\n", proto, status)
	for name, values := range headers {
		if strings.EqualFold(name, "Content-Encoding") {
			continue
		}
		for _, value := range values {
			fmt.Fprintf(&headerBuf, "%s: %s\r\n", name, value)
		}
	}
	headerBuf.WriteString("\r\n")

	var bodyReader io.Reader = body
	if contentEncoding != "" {
		decompressed, err := decompressReader(body, contentEncoding)
		if err != nil {
			fmt.Fprintf(&headerBuf, "X-Decompression-Error: %v\r\n", err)
		} else {
			defer decompressed.Close()
			bodyReader = decompressed
		}
	}

	s.logger.LogResponse(metadata, timestamp, &readCloser{
		Reader: io.MultiReader(&headerBuf, bodyReader),
		Closer: io.NopCloser(nil),
	})
}

func wrapBodyForLogging(body io.ReadCloser, logFunc func(io.ReadCloser)) io.ReadCloser {
	if body == nil {
		body = http.NoBody
	}

	reader, writer := io.Pipe()
	wrapped := &teeReadCloser{
		source: body,
		writer: writer,
	}
	go logFunc(reader)
	return wrapped
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.source.Read(p)
	if n > 0 && !t.loggingDisabled {
		if _, writeErr := t.writer.Write(p[:n]); writeErr != nil {
			// Logging is best-effort. If the log reader exits early (for example
			// because decompression detected a truncated gzip stream after a client
			// cancel), do not turn that side-channel failure into a proxied stream
			// read error.
			t.loggingDisabled = true
			_ = t.writer.CloseWithError(writeErr)
		}
	}
	return n, err
}

func (t *teeReadCloser) Close() error {
	var err error
	t.once.Do(func() {
		err = errors.Join(t.source.Close(), t.writer.Close())
	})
	return err
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	clone := *u
	return &clone
}
