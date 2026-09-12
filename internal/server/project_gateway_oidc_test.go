package server

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
)

func TestProjectGatewayOIDCSharedNamespaceScope(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{"keys": []map[string]string{{"kid": "gateway", "kty": "RSA", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	}))
	defer issuer.Close()
	gateway, projects := gatewayFixture(t, nil)
	for _, project := range gateway.projects {
		project.config.GitHubOIDCIssuer = issuer.URL
	}
	now := time.Now().UTC()
	for _, integration := range []string{"turbo", "buildkit"} {
		for _, event := range []string{"push", "workflow_dispatch", "schedule", "pull_request", "pull_request_target", "workflow_run", ""} {
			for _, ref := range []string{"refs/heads/main", "refs/heads/feature", "refs/pull/1/merge"} {
				idToken := signOIDCTestToken(t, key, "gateway", map[string]any{
					"iss": issuer.URL, "aud": "layercache:" + projects["alpha"].ProjectID,
					"iat": now.Add(-time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Minute).Unix(),
					"sub": "repo:acme/alpha:ref:" + ref, "repository": "acme/alpha", "ref": ref, "sha": strings.Repeat("a", 40),
					"event_name":   event,
					"workflow_ref": "acme/alpha/.github/workflows/ci.yml@" + ref, "run_id": "123", "run_attempt": "1", "check_run_id": "456",
				})
				body, _ := json.Marshal(map[string]string{"project": projects["alpha"].ProjectID, "integration": integration, "compatibility": "linux-amd64-schema1", "idToken": idToken})
				response := gatewayRequest(gateway, "POST", "/v1/auth/github-oidc/exchange", "", string(body), nil)
				if response.Code != 200 {
					t.Fatalf("OIDC exchange: %d %s", response.Code, response.Body.String())
				}
				var result struct {
					Token string `json:"token"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				claims, err := access.ParseCapabilityToken(projects["alpha"].LocalToken, result.Token, now)
				wantWrite := ref == "refs/heads/main" && (event == "push" || event == "workflow_dispatch" || event == "schedule")
				if err != nil || claims.Integration != integration || claims.Allows(access.CapabilityWrite) != wantWrite {
					t.Fatalf("wrong registry authority: %+v %v", claims, err)
				}
				if integration == "turbo" {
					put := gatewayRequest(gateway, "PUT", "/v8/artifacts/scope-check", "Bearer "+result.Token, "payload", nil)
					if !wantWrite && put.Code != http.StatusUnauthorized {
						t.Fatalf("%s %s obtained Turbo write access: %d", event, ref, put.Code)
					}
					if wantWrite && put.Code != http.StatusOK && put.Code != http.StatusCreated {
						t.Fatalf("trusted Turbo write failed: %d %s", put.Code, put.Body.String())
					}
				}
				body, _ = json.Marshal(map[string]string{"project": projects["beta"].ProjectID, "integration": integration, "compatibility": "linux-amd64-schema1", "idToken": idToken})
				if response := gatewayRequest(gateway, "POST", "/v1/auth/github-oidc/exchange", "", string(body), nil); response.Code != 401 {
					t.Fatalf("cross-project OIDC exchange accepted: %d", response.Code)
				}
			}
		}
	}
}
