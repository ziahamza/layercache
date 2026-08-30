package cli

import (
	"net"
	"net/http"
	"time"
)

const (
	controlRequestTimeout = 30 * time.Second
	publishRequestTimeout = 30 * time.Minute
)

func newCLIHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialer.DialContext
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.TLSHandshakeTimeout = 5 * time.Second
	return &http.Client{Transport: transport, Timeout: timeout}
}
