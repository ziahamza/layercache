// Package dashboard serves the CLI-connected dashboard. The browser receives
// projections of cache data, never runtime or Team Cache credentials.
package dashboard

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"time"

	"github.com/layercache/layercache/internal/measurement"
)

//go:embed assets/*
var assets embed.FS

type Status struct {
	ProjectID      string `json:"projectId"`
	UsageBytes     int64  `json:"usageBytes"`
	MaxBytes       *int64 `json:"maxBytes"`
	Artifacts      int64  `json:"artifacts"`
	EvictionPolicy string `json:"evictionPolicy"`
}

type Cache struct {
	State       string                    `json:"state"`
	Status      *Status                   `json:"status,omitempty"`
	Report      *measurement.PeriodReport `json:"report,omitempty"`
	ReportState string                    `json:"reportState"`
}

type Project struct {
	ID    string `json:"id"`
	Local Cache  `json:"local"`
	Team  Cache  `json:"team"`
}

type Snapshot struct {
	Projects  []Project `json:"projects"`
	UpdatedAt time.Time `json:"updatedAt"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
}

// New serves only fixed read routes. A fresh process token authenticates browser
// requests; the exact authority check also rejects DNS rebinding.
func New(authority, token string, snapshot func(context.Context, time.Duration) Snapshot) http.Handler {
	files, _ := fs.Sub(assets, "assets")
	static := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if token == "" || r.Host != authority {
			http.Error(w, "invalid dashboard host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "read-only dashboard", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/api/snapshot" {
			if len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
				http.Error(w, "open the dashboard link printed by the CLI", http.StatusUnauthorized)
				return
			}
			period := 24 * time.Hour
			switch r.URL.Query().Get("period") {
			case "", "24h":
			case "7d":
				period = 7 * 24 * time.Hour
			default:
				http.Error(w, "invalid period", http.StatusBadRequest)
				return
			}
			result := snapshot(r.Context(), period)
			if result.Projects == nil {
				http.Error(w, "dashboard snapshot timed out; refresh to try again", http.StatusGatewayTimeout)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(result)
			return
		}
		switch r.URL.Path {
		case "/", "/app.js", "/style.css":
			static.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}
