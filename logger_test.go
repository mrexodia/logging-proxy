package loggingproxy

import (
	"io"
	"sync/atomic"
	"testing"
	"time"
)

type disabledTestLogger struct {
	calls atomic.Int32
}

func (logger *disabledTestLogger) CaptureEnabled() bool {
	return false
}

func (logger *disabledTestLogger) LogRequest(_ RequestMetadata, _ time.Time, stream io.ReadCloser) {
	logger.calls.Add(1)
	defer stream.Close()
	_, _ = io.Copy(io.Discard, stream)
}

func (logger *disabledTestLogger) LogResponse(_ RequestMetadata, _ time.Time, stream io.ReadCloser) {
	logger.calls.Add(1)
	defer stream.Close()
	_, _ = io.Copy(io.Discard, stream)
}

type defaultEnabledTestLogger struct{}

func (defaultEnabledTestLogger) LogRequest(_ RequestMetadata, _ time.Time, stream io.ReadCloser) {
	_ = stream.Close()
}

func (defaultEnabledTestLogger) LogResponse(_ RequestMetadata, _ time.Time, stream io.ReadCloser) {
	_ = stream.Close()
}

func TestLoggerCaptureEnabled(t *testing.T) {
	if loggerCaptureEnabled(nil) {
		t.Fatal("nil logger should disable capture")
	}
	if loggerCaptureEnabled(&NoOpLogger{}) {
		t.Fatal("NoOpLogger should disable capture")
	}
	if loggerCaptureEnabled(&disabledTestLogger{}) {
		t.Fatal("CaptureController should be able to disable capture")
	}
	if !loggerCaptureEnabled(defaultEnabledTestLogger{}) {
		t.Fatal("custom loggers should remain capture-enabled by default")
	}
}
