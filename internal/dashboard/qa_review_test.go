package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDashboardQARejectsAdversarialRequestsWithoutReading(t *testing.T) {
	reads := 0
	handler := New("127.0.0.1:7438", "review-session", func(context.Context, time.Duration) Snapshot { reads++; return Snapshot{} })
	cases := []struct {
		name, target, method, host string
		auth                       []string
		code                       int
	}{
		{"duplicate authorization", "/api/snapshot", "GET", "127.0.0.1:7438", []string{"Bearer review-session", "Bearer review-session"}, 401},
		{"combined authorization", "/api/snapshot", "GET", "127.0.0.1:7438", []string{"Bearer review-session, Bearer review-session"}, 401},
		{"token query", "/api/snapshot?token=review-session", "GET", "127.0.0.1:7438", nil, 401},
		{"token cookie", "/api/snapshot", "GET", "127.0.0.1:7438", nil, 401},
		{"alternate loopback", "/api/snapshot", "GET", "localhost:7438", []string{"Bearer review-session"}, 403},
		{"wrong port", "/api/snapshot", "GET", "127.0.0.1:7439", []string{"Bearer review-session"}, 403},
		{"absolute attacker host", "http://attacker.test/api/snapshot", "GET", "attacker.test", []string{"Bearer review-session"}, 403},
		{"preflight", "/api/snapshot", "OPTIONS", "127.0.0.1:7438", []string{"Bearer review-session"}, 405},
		{"head", "/api/snapshot", "HEAD", "127.0.0.1:7438", []string{"Bearer review-session"}, 405},
		{"traversal", "/assets/../app.js", "GET", "127.0.0.1:7438", nil, 404},
		{"embedded files listing", "/assets/", "GET", "127.0.0.1:7438", nil, 404},
		{"encoded separator", "/api%2fsnapshot", "GET", "127.0.0.1:7438", nil, 401},
		{"trailing route slash", "/api/snapshot/", "GET", "127.0.0.1:7438", []string{"Bearer review-session"}, 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.target, nil)
			r.Host = tc.host
			for _, a := range tc.auth {
				r.Header.Add("Authorization", a)
			}
			r.Header.Set("Origin", "https://attacker.test")
			r.Header.Set("Cookie", "token=review-session")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatalf("got %d, want %d", w.Code, tc.code)
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("cross-origin access allowed")
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("response may be cached")
			}
		})
	}
	if reads != 0 {
		t.Fatalf("unauthorized reads: %d", reads)
	}
}

func TestDashboardQARejectsEmptySessionAndPreservesCancellation(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/snapshot", nil)
	r.Host = "127.0.0.1:7438"
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	New(r.Host, "", nil).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("empty session accepted: %d", w.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = r.WithContext(ctx)
	r.Header.Set("Authorization", "Bearer session")
	called := false
	handler := New(r.Host, "session", func(ctx context.Context, _ time.Duration) Snapshot {
		called = true
		if ctx.Err() != context.Canceled {
			t.Error("cancellation lost")
		}
		return Snapshot{Projects: []Project{{ID: "</script><script>alert(1)</script>"}}}
	})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if !called {
		t.Fatal("callback not called")
	}
	if strings.Contains(w.Body.String(), "<script>") {
		t.Fatal("JSON projection has executable HTML")
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("projection lacks JSON content type")
	}
}
