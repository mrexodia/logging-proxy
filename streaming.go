package loggingproxy

import (
	"errors"
	"io"
	"net/http"
)

// copyProxyResponse flushes headers before waiting for the first body byte and
// flushes every forwarded read, including small SSE events and fixed-length
// streamed responses. WriteHeader and io.Copy alone leave data in net/http's
// buffers. This does not preserve upstream HTTP chunk or TCP packet boundaries.
func copyProxyResponse(w http.ResponseWriter, body io.Reader) (int64, error) {
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			// Embedded handlers can supply writers without flushing support.
			// Keep forwarding the body rather than silently dropping it.
			return copyProxyStream(w, body)
		}
		return 0, err
	}
	return copyProxyStream(responseFlushWriter{writer: w, controller: controller}, body)
}

type responseFlushWriter struct {
	writer     io.Writer
	controller *http.ResponseController
}

func (w responseFlushWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err == nil && n > 0 {
		err = w.controller.Flush()
	}
	return n, err
}
