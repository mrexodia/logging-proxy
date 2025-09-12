package loggingproxy

import (
	"net/http"
	"time"
)

type LoggingClient struct {
	serverURL string
	client    *http.Client
	enabled   bool
}

func NewLoggingClient(serverURL string) *LoggingClient {
	return &LoggingClient{
		serverURL: serverURL,
		client:    &http.Client{Timeout: 30 * time.Second},
		enabled:   true,
	}
}
