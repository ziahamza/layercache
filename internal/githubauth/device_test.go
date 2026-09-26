package githubauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeviceClientBeginPollAndRefresh(t *testing.T) {
	var polls, refreshes atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json" ||
			r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
			t.Errorf("invalid OAuth request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Has("client_secret") || r.Form.Get("client_id") != "fixture-client" {
			t.Error("device flow sent a client secret or wrong client ID")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/device/code":
			if scopes := strings.Fields(r.Form.Get("scope")); len(scopes) != 2 ||
				!slices.Contains(scopes, "read:user") || !slices.Contains(scopes, "offline_access") {
				t.Errorf("unexpected device scopes: %q", scopes)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "fixture-device", "user_code": "ABCD-EFGH",
				"verification_uri": "https://github.com/login/device", "expires_in": 60, "interval": 1,
			})
		case "/oauth/access_token":
			switch r.Form.Get("grant_type") {
			case deviceGrantType:
				if r.Form.Get("device_code") != "fixture-device" || r.Form.Has("scope") {
					t.Error("invalid device-code poll parameters")
				}
				switch polls.Add(1) {
				case 1:
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
				case 2:
					// GitHub returns the new minimum interval, not an amount to add.
					_ = json.NewEncoder(w).Encode(map[string]any{"error": "slow_down", "interval": 6})
				case 3:
					_ = json.NewEncoder(w).Encode(map[string]any{
						"access_token": "access-one", "refresh_token": "refresh-one",
						"expires_in": 28800, "refresh_token_expires_in": 15897600,
						"token_type": "bearer", "scope": "read:user",
					})
				default:
					t.Error("polled after authorization completed")
					w.WriteHeader(http.StatusBadRequest)
				}
			case "refresh_token":
				refreshes.Add(1)
				if r.Form.Get("refresh_token") != "refresh-one" || r.Form.Has("scope") {
					t.Error("invalid refresh parameters")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "access-two", "refresh_token": "refresh-two",
					"expires_in": 28800, "refresh_token_expires_in": 15897600,
					"token_type": "bearer", "scope": "read:user",
				})
			default:
				t.Errorf("unexpected grant type: %q", r.Form.Get("grant_type"))
				w.WriteHeader(http.StatusBadRequest)
			}
		default:
			t.Errorf("unexpected OAuth path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer provider.Close()

	client := DeviceClient{
		ClientID: "fixture-client", DeviceEndpoint: provider.URL + "/device/code",
		TokenEndpoint: provider.URL + "/oauth/access_token", HTTPClient: provider.Client(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	device, err := client.Begin(ctx)
	if err != nil || device.DeviceCode != "fixture-device" || device.UserCode != "ABCD-EFGH" || device.Interval != 1 {
		t.Fatalf("begin device authorization: %+v, %v", device, err)
	}
	session, err := client.Poll(ctx, device)
	if err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 3 || session.AccessToken != "access-one" || session.RefreshToken != "refresh-one" ||
		session.ClientID != "fixture-client" || session.TokenEndpoint != client.TokenEndpoint || session.ObtainedAt.IsZero() ||
		session.AccessTokenNeedsRefresh(session.ObtainedAt, 2*time.Minute) ||
		!session.AccessTokenNeedsRefresh(session.ObtainedAt.Add(28700*time.Second), 2*time.Minute) {
		t.Fatalf("wrong device session: %+v (polls=%d)", session, polls.Load())
	}
	// Refresh uses the identity stored with the original session, as a later CLI
	// invocation need not receive the OAuth client ID again.
	refreshed, err := (DeviceClient{HTTPClient: provider.Client()}).Refresh(ctx, session)
	if err != nil || refreshes.Load() != 1 || refreshed.AccessToken != "access-two" ||
		refreshed.RefreshToken != "refresh-two" || refreshed.ClientID != "fixture-client" ||
		refreshed.TokenEndpoint != client.TokenEndpoint {
		t.Fatalf("refresh rotation: %+v, %v (refreshes=%d)", refreshed, err, refreshes.Load())
	}
}

func TestDeviceClientPollDenial(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
	}))
	defer provider.Close()
	client := DeviceClient{ClientID: "fixture-client", TokenEndpoint: provider.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := client.Poll(ctx, DeviceCode{DeviceCode: "fixture-device", ExpiresIn: 30, Interval: 1})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("expected actionable denial, got %v", err)
	}
}

func TestDeviceClientRejectsInvalidEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"http://example.com/device/code", "http://127.0.0.1:8080/device/code?secret=1",
		"http://user:password@127.0.0.1:8080/device/code", "http://127.0.0.1:8080/device/code#secret",
	} {
		t.Run(url.QueryEscape(endpoint), func(t *testing.T) {
			client := DeviceClient{ClientID: "fixture-client", DeviceEndpoint: endpoint}
			if _, err := client.Begin(context.Background()); err == nil {
				t.Fatal("unsafe OAuth endpoint accepted")
			}
		})
	}
}

func TestDeviceClientRejectsCrossOriginRedirect(t *testing.T) {
	var destinationReached atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationReached.Store(true)
	}))
	defer destination.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/receive", http.StatusFound)
	}))
	defer provider.Close()

	client := DeviceClient{ClientID: "fixture-client", DeviceEndpoint: provider.URL, HTTPClient: provider.Client()}
	if _, err := client.Begin(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing OAuth redirect") {
		t.Fatalf("cross-origin redirect accepted: %v", err)
	}
	if destinationReached.Load() {
		t.Fatal("OAuth client followed a redirect to another origin")
	}
}

func TestNextDevicePollInterval(t *testing.T) {
	for _, test := range []struct {
		name      string
		current   time.Duration
		suggested int64
		want      time.Duration
	}{
		{"GitHub returns new interval", 5 * time.Second, 10, 10 * time.Second},
		{"missing interval adds five seconds", 5 * time.Second, 0, 10 * time.Second},
		{"longer server interval wins", 5 * time.Second, 20, 20 * time.Second},
		{"stale interval still backs off", 10 * time.Second, 10, 15 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nextDevicePollInterval(test.current, test.suggested); got != test.want {
				t.Fatalf("next interval = %s, want %s", got, test.want)
			}
		})
	}
}
