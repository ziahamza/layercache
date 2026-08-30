package cli

import (
	"net/http"
	"testing"
)

func TestCLIHTTPClientHasBoundedConnectionAndResponseTimes(t *testing.T) {
	t.Parallel()

	client := newCLIHTTPClient(controlRequestTimeout)
	if client.Timeout != controlRequestTimeout {
		t.Fatalf("client timeout = %s, want %s", client.Timeout, controlRequestTimeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.DialContext == nil || transport.ResponseHeaderTimeout <= 0 || transport.TLSHandshakeTimeout <= 0 {
		t.Fatalf("CLI transport has unbounded phase: %+v", transport)
	}
}
