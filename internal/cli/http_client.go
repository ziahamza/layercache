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
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// CLI requests carry administrator, GitHub, OIDC, or project
			// credentials. Never replay them through an HTTP redirect.
			return http.ErrUseLastResponse
		},
	}
}

// A bind address identifies the local daemon, not a proxyable remote service.
// Unspecified binds need a concrete loopback destination on the same family.
func localRuntimeURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err == nil {
		address := net.ParseIP(host)
		if host == "" || address != nil && address.IsUnspecified() {
			host = "127.0.0.1"
			if address != nil && address.To4() == nil {
				host = "::1"
			}
			listen = net.JoinHostPort(host, port)
		}
	}
	return "http://" + listen
}

func newLocalCLIHTTPClient(timeout time.Duration) *http.Client {
	client := newCLIHTTPClient(timeout)
	client.Transport.(*http.Transport).Proxy = nil
	return client
}
