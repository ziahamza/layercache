package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDashboardAuthenticatesBeforeReadingProjects(t *testing.T) {
	reads := 0
	handler := New("127.0.0.1:7438", "session-secret", func(_ context.Context, period time.Duration) Snapshot {
		reads++
		if period != 7*24*time.Hour {
			t.Fatalf("period = %v", period)
		}
		return Snapshot{Projects: []Project{{ID: "github.com/acme/private"}}}
	})
	for _, test := range []struct {
		name, path, host, token, method string
		want                            int
	}{
		{"no session", "/api/snapshot", "127.0.0.1:7438", "", "GET", 401},
		{"wrong session", "/api/snapshot", "127.0.0.1:7438", "wrong", "GET", 401},
		{"rebinding", "/api/snapshot", "attacker.test:7438", "session-secret", "GET", 403},
		{"mutation", "/api/snapshot", "127.0.0.1:7438", "session-secret", "POST", 405},
		{"unbounded period", "/api/snapshot?period=365d", "127.0.0.1:7438", "session-secret", "GET", 400},
		{"unknown route", "/api/config", "127.0.0.1:7438", "session-secret", "GET", 404},
		{"authorized", "/api/snapshot?period=7d", "127.0.0.1:7438", "session-secret", "GET", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			request.Host = test.host
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status %d, want %d", response.Code, test.want)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("snapshot may be cached")
			}
			if test.want != 200 && strings.Contains(response.Body.String(), "acme/private") {
				t.Fatal("project leaked")
			}
		})
	}
	if reads != 1 {
		t.Fatalf("project reads = %d", reads)
	}
}

func TestDashboardShipsShellWithoutCredentials(t *testing.T) {
	handler := New("127.0.0.1:7438", "secret-value", nil)
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = "127.0.0.1:7438"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("asset %s unavailable", path)
		}
		if strings.Contains(w.Body.String(), "secret-value") {
			t.Fatal("credential in static asset")
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatal("missing frame policy")
		}
	}
}

func TestDashboardExpiredSnapshotIsNotSuccessfulJSON(t *testing.T) {
	handler := New("127.0.0.1:7438", "session", func(context.Context, time.Duration) Snapshot { return Snapshot{} })
	r := httptest.NewRequest("GET", "/api/snapshot", nil)
	r.Host = "127.0.0.1:7438"
	r.Header.Set("Authorization", "Bearer session")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("expired snapshot status = %d, want 504", w.Code)
	}
}
