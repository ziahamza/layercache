package remote

import (
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	DefaultMetadataTimeout = 2 * time.Second
	DefaultTransferIdle    = 30 * time.Second
)

var ErrTransferIdle = errors.New("remote cache transfer made no progress before the idle deadline")

// Timeouts separates the bounded metadata phase from the progress-based body
// phase. A transfer may run for any total duration while bytes keep moving.
type Timeouts struct {
	Metadata     time.Duration
	TransferIdle time.Duration
}

func normalizeTimeouts(timeouts Timeouts) (Timeouts, error) {
	if timeouts.Metadata == 0 {
		timeouts.Metadata = DefaultMetadataTimeout
	}
	if timeouts.TransferIdle == 0 {
		timeouts.TransferIdle = DefaultTransferIdle
	}
	if timeouts.Metadata < 0 || timeouts.TransferIdle < 0 {
		return Timeouts{}, errors.New("remote cache timeouts must be positive")
	}
	return timeouts, nil
}

func transportWithMetadataTimeout(timeout time.Duration) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	transport.DialContext = dialer.DialContext
	transport.TLSHandshakeTimeout = timeout
	transport.ResponseHeaderTimeout = timeout
	return transport
}

type idleReadCloser struct {
	body    io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
	mu      sync.Mutex
	expired bool
	closed  bool
}

func withIdleReadTimeout(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	reader := &idleReadCloser{body: body, timeout: timeout}
	reader.timer = time.AfterFunc(timeout, reader.expire)
	return reader
}

func (reader *idleReadCloser) expire() {
	reader.mu.Lock()
	if reader.closed {
		reader.mu.Unlock()
		return
	}
	reader.expired = true
	reader.mu.Unlock()
	_ = reader.body.Close()
}

func (reader *idleReadCloser) Read(buffer []byte) (int, error) {
	read, err := reader.body.Read(buffer)
	reader.mu.Lock()
	expired := reader.expired
	if read > 0 && !expired && !reader.closed {
		reader.timer.Reset(reader.timeout)
	}
	reader.mu.Unlock()
	if expired && read == 0 {
		return 0, ErrTransferIdle
	}
	return read, err
}

func (reader *idleReadCloser) Close() error {
	reader.mu.Lock()
	reader.closed = true
	reader.timer.Stop()
	reader.mu.Unlock()
	return reader.body.Close()
}

type progressReader struct {
	reader  io.Reader
	timeout time.Duration
	timer   *time.Timer
	mu      sync.Mutex
	expired bool
	closed  bool
}

func newProgressReader(reader io.Reader, timeout time.Duration, cancel func()) *progressReader {
	progress := &progressReader{reader: reader, timeout: timeout}
	progress.timer = time.AfterFunc(timeout, func() {
		progress.mu.Lock()
		if progress.closed {
			progress.mu.Unlock()
			return
		}
		progress.expired = true
		progress.mu.Unlock()
		cancel()
	})
	return progress
}

func (reader *progressReader) Read(buffer []byte) (int, error) {
	read, err := reader.reader.Read(buffer)
	reader.mu.Lock()
	if read > 0 && !reader.expired && !reader.closed {
		reader.timer.Reset(reader.timeout)
	}
	reader.mu.Unlock()
	return read, err
}

func (reader *progressReader) stop() bool {
	reader.mu.Lock()
	reader.closed = true
	reader.timer.Stop()
	expired := reader.expired
	reader.mu.Unlock()
	return expired
}
