package loggingproxy

import "net/http"

func newDirectTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		clone := transport.Clone()
		clone.Proxy = nil
		return clone
	}

	return &http.Transport{Proxy: nil}
}

func newDirectHTTPClient() *http.Client {
	return &http.Client{Transport: newDirectTransport()}
}
