package cli

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/config"
)

func TestLocalCLIControlDoesNotSendCredentialsThroughHTTPProxy(t *testing.T) {
	// net/http caches proxy environment once. Use a new process so other tests'
	// requests cannot accidentally make this regression pass.
	if os.Getenv("LAYERCACHE_PROXY_TEST_CHILD") != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(binary, "-test.run=^TestLocalCLIControlDoesNotSendCredentialsThroughHTTPProxy$", "-test.count=1")
		child.Env = append(os.Environ(), "LAYERCACHE_PROXY_TEST_CHILD=1")
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("isolated proxy regression: %v\n%s", err, output)
		}
		return
	}
	proxied := make(chan bool, 32)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied <- r.Header.Get("Authorization") != ""
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	configPath, _ := setupLocalConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cfg.LocalToken {
			t.Error("local API did not receive its credential")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"maxBytes":4096,"usedBytes":0,"running":true}`))
	}))
	defer endpoint.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(endpoint.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"0.0.0.0", "", "::ffff:0.0.0.0"} {
		t.Run("listen_"+host, func(t *testing.T) {
			cfg.Role, cfg.Listen = "team", net.JoinHostPort(host, port)
			if err := config.Save(configPath, cfg); err != nil {
				t.Fatal(err)
			}
			output, _, commandErr := callCLI("cache", "quota", "--config", configPath, "--json")
			select {
			case credential := <-proxied:
				t.Fatalf("local CLI used HTTP_PROXY; credential forwarded = %v", credential)
			default:
			}
			if commandErr != nil || !strings.Contains(output, `"maxBytes":4096`) {
				t.Fatalf("local quota request: %v, %s", commandErr, output)
			}
			for _, command := range [][]string{{"status"}, {"gc"}, {"report", "--run", "proxy-regression"}} {
				arguments := append(command, "--config", configPath, "--json")
				if _, _, err := callCLI(arguments...); err != nil {
					t.Fatalf("%s: %v", command[0], err)
				}
			}
			select {
			case credential := <-proxied:
				t.Fatalf("local lifecycle/report used HTTP_PROXY; credential forwarded = %v", credential)
			default:
			}
		})
	}
	t.Run("IPv6 unspecified bind", func(t *testing.T) {
		listener, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			t.Skipf("IPv6 loopback unavailable: %v", err)
		}
		ipv6 := httptest.NewUnstartedServer(endpoint.Config.Handler)
		ipv6.Listener = listener
		ipv6.Start()
		defer ipv6.Close()
		_, port, err := net.SplitHostPort(listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		cfg.Listen = net.JoinHostPort("::", port)
		if err := config.Save(configPath, cfg); err != nil {
			t.Fatal(err)
		}
		if _, _, err := callCLI("cache", "quota", "--config", configPath, "--json"); err != nil {
			t.Fatal(err)
		}
		select {
		case credential := <-proxied:
			t.Fatalf("IPv6 local control used HTTP_PROXY; credential forwarded = %v", credential)
		default:
		}
	})
	// Explicit remote endpoints must retain the engineer's configured proxy.
	cfg.PublicURL = "http://public-cache.example.invalid"
	_, _, _ = callPublicBuild(context.Background(), cfg, http.MethodGet, "/v1/public-builds", "remote-fixture-token", nil)
	select {
	case <-proxied:
	default:
		t.Fatal("explicit remote Public Cache request ignored HTTP_PROXY")
	}
}
