package loggingproxy

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/google/uuid"
)

type HTTPProxyOptions struct {
	Logger            Logger
	MITM              bool
	MITMCertificate   *tls.Certificate
	UpstreamTLSConfig *tls.Config
	Verbose           bool
}

type HTTPProxyServer struct {
	proxy       *goproxy.ProxyHttpServer
	logger      Logger
	mitmEnabled bool
}

type httpProxyRequestState struct {
	metadata    RequestMetadata
	requestTime time.Time
}

type memoryCertStore struct {
	mu    sync.Mutex
	certs map[string]*tls.Certificate
}

type teeReadCloser struct {
	io.Reader
	source io.Closer
	writer io.Closer
	once   sync.Once
}

func NewHTTPProxyServer(options HTTPProxyOptions) (*HTTPProxyServer, error) {
	logger := options.Logger
	if logger == nil {
		logger = &NoOpLogger{}
	}

	transport := newDirectTransport()
	transport.DisableCompression = true
	if options.UpstreamTLSConfig != nil {
		transport.TLSClientConfig = options.UpstreamTLSConfig.Clone()
	}

	proxy := goproxy.NewProxyHttpServer()
	proxy.Tr = transport
	proxy.ConnectDial = nil
	proxy.ConnectDialWithReq = nil
	proxy.KeepAcceptEncoding = true
	proxy.KeepHeader = false
	proxy.Verbose = options.Verbose
	if options.Verbose {
		proxy.Logger = log.Default()
	} else {
		proxy.Logger = log.New(io.Discard, "", 0)
	}

	server := &HTTPProxyServer{
		proxy:       proxy,
		logger:      logger,
		mitmEnabled: options.MITM,
	}

	if options.MITM {
		if options.MITMCertificate == nil {
			return nil, fmt.Errorf("MITM mode requires a CA certificate")
		}
		if options.MITMCertificate.Leaf == nil {
			if len(options.MITMCertificate.Certificate) == 0 {
				return nil, fmt.Errorf("MITM CA certificate chain is empty")
			}
			leaf, err := x509.ParseCertificate(options.MITMCertificate.Certificate[0])
			if err != nil {
				return nil, fmt.Errorf("failed to parse MITM CA leaf certificate: %w", err)
			}
			options.MITMCertificate.Leaf = leaf
		}
		proxy.CertStore = &memoryCertStore{certs: map[string]*tls.Certificate{}}
		mitmAction := &goproxy.ConnectAction{Action: goproxy.ConnectMitm, TLSConfig: goproxy.TLSConfigFromCA(options.MITMCertificate)}
		proxy.OnRequest().HandleConnect(goproxy.FuncHttpsHandler(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
			return mitmAction, host
		}))
	}

	proxy.OnRequest().DoFunc(server.handleRequest)
	proxy.OnResponse().DoFunc(server.handleResponse)

	return server, nil
}

func (s *HTTPProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.proxy.ServeHTTP(w, r)
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
	metadata.ResponseContentEncoding = responseContentEncoding

	response.Body = wrapBodyForLogging(response.Body, func(body io.ReadCloser) {
		s.logHTTPProxyResponse(metadata, responseTime, response.Proto, response.Status, responseHeaders, responseContentEncoding, body)
	})

	return response
}

func (s *HTTPProxyServer) logHTTPProxyRequest(metadata RequestMetadata, timestamp time.Time, method, target, proto string, headers http.Header, contentEncoding string, body io.ReadCloser) {
	defer body.Close()

	var headerBuf bytes.Buffer
	fmt.Fprintf(&headerBuf, "%s %s %s\r\n", method, target, proto)
	for name, values := range headers {
		if strings.EqualFold(name, "Host") || strings.EqualFold(name, "Content-Encoding") {
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
		Reader: io.TeeReader(body, writer),
		source: body,
		writer: writer,
	}
	go logFunc(reader)
	return wrapped
}

func (t *teeReadCloser) Close() error {
	var err error
	t.once.Do(func() {
		err = errors.Join(t.source.Close(), t.writer.Close())
	})
	return err
}

func (s *memoryCertStore) Fetch(hostname string, gen func() (*tls.Certificate, error)) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cert, ok := s.certs[hostname]; ok {
		return cert, nil
	}

	cert, err := gen()
	if err != nil {
		return nil, err
	}
	s.certs[hostname] = cert
	return cert, nil
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	clone := *u
	return &clone
}
