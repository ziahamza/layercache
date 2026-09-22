package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/compatibility"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publicbuild"
)

func TestGitHubOIDCCapabilityIsActionsOnly(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const keyID = "actions-oidc-test-key"
	issuer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/.well-known/jwks" {
			http.NotFound(writer, request)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"keys": []map[string]string{{
			"kid": keyID, "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes()),
		}}})
	}))
	t.Cleanup(issuer.Close)

	cfg := config.Config{
		Version: 1, Role: "team", DataDir: t.TempDir(), Listen: "127.0.0.1:7437",
		MaxBytes: 1 << 20, ProjectID: "github.com/acme/widget", LocalToken: "team-token",
		CompatibilityID: "server-host-linux-amd64-schema1", ActionsRepository: "acme/widget",
		ActionsRef: "refs/heads/main", ActionsDefaultRef: "refs/heads/main",
		ActionsPublicBuilder: "layercache-public-builder-v1", BuildkitBuilder: "layercache-test",
		GitHubOIDCIssuer: issuer.URL,
	}
	instance, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	endpoint := httptest.NewServer(instance.Handler())
	t.Cleanup(endpoint.Close)

	now := time.Now().UTC()
	idToken := signOIDCTestToken(t, privateKey, keyID, map[string]any{
		"iss": issuer.URL, "aud": "layercache:" + cfg.ProjectID,
		"iat": now.Add(-time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"sub": "repo:acme/widget:ref:refs/heads/main", "repository": "acme/widget",
		"ref": "refs/heads/main", "sha": strings.Repeat("a", 40),
		"event_name":   "push",
		"workflow_ref": "acme/widget/.github/workflows/public-cache.yml@refs/heads/main",
		"run_id":       "123456789", "run_attempt": "2", "check_run_id": "987654321",
	})
	exchangeBody, err := json.Marshal(map[string]string{
		"project": cfg.ProjectID, "compatibility": "linux-amd64-schema1", "idToken": idToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(endpoint.URL+"/v1/auth/github-oidc/exchange", "application/json", bytes.NewReader(exchangeBody))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("OIDC exchange status = %d, want 200", response.StatusCode)
	}
	var exchanged struct {
		TeamToken string `json:"teamToken"`
	}
	if err := json.NewDecoder(response.Body).Decode(&exchanged); err != nil {
		t.Fatal(err)
	}
	claims, err := access.ParseCapabilityToken(cfg.LocalToken, exchanged.TeamToken, now)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Integration != "actions" {
		t.Fatalf("OIDC capability integration = %q, want actions", claims.Integration)
	}
	if claims.RunID != "github-actions:acme/widget:123456789:2" ||
		claims.WorkspaceID != "github-actions-check:acme/widget:987654321" {
		t.Fatalf("OIDC measurement correlation = run %q workspace %q", claims.RunID, claims.WorkspaceID)
	}
	for _, integration := range []string{"turbo", "buildkit"} {
		body, _ := json.Marshal(map[string]string{"project": cfg.ProjectID, "compatibility": "linux-amd64-schema1", "idToken": idToken, "integration": integration})
		response, err := http.Post(endpoint.URL+"/v1/auth/github-oidc/exchange", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			TeamToken string `json:"teamToken"`
		}
		err = json.NewDecoder(response.Body).Decode(&result)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		issued, err := access.ParseCapabilityToken(cfg.LocalToken, result.TeamToken, now)
		if err != nil {
			t.Fatal(err)
		}
		want := "github-actions:acme/widget:123456789:2"
		if integration == "turbo" {
			want += ":check:987654321"
		}
		if issued.RunID != want {
			t.Fatalf("%s runID=%q want %q", integration, issued.RunID, want)
		}
	}
	const publicTarget = ".github/workflows/public-cache.yml#public-cache"
	publicRecipe, err := publicbuild.MaintainedRecipeDigest(publicbuild.IntegrationActions, publicTarget)
	if err != nil {
		t.Fatal(err)
	}
	instance.config.ActionsPublicRecipeDigest = publicRecipe
	postPublicExchange := func(target string) *http.Response {
		t.Helper()
		body, err := json.Marshal(map[string]string{
			"project": cfg.ProjectID, "compatibility": "linux-amd64-schema1", "idToken": idToken,
			"recipeDigest": publicRecipe, "target": target, "platform": "linux/amd64",
			"toolchain": actionsPublicToolchain, "builder": cfg.ActionsPublicBuilder,
		})
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.Post(endpoint.URL+"/v1/auth/github-oidc/exchange", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	publicResponse := postPublicExchange(publicTarget)
	defer publicResponse.Body.Close()
	if publicResponse.StatusCode != http.StatusOK {
		t.Fatalf("Public OIDC exchange status = %d, want 200", publicResponse.StatusCode)
	}
	var publicExchange struct {
		TeamToken string `json:"teamToken"`
	}
	if err := json.NewDecoder(publicResponse.Body).Decode(&publicExchange); err != nil {
		t.Fatal(err)
	}
	publicClaims, err := access.ParseCapabilityToken(cfg.LocalToken, publicExchange.TeamToken, now)
	if err != nil {
		t.Fatal(err)
	}
	if publicClaims.Target != publicTarget {
		t.Fatalf("Public OIDC target = %q, want %q", publicClaims.Target, publicTarget)
	}
	wrongTarget := postPublicExchange(".github/workflows/other.yml#public-cache")
	defer wrongTarget.Body.Close()
	if wrongTarget.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong signed-workflow target status = %d, want 401", wrongTarget.StatusCode)
	}

	actions, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{
		Endpoint: endpoint.URL, Token: exchanged.TeamToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	size := int64(1)
	if _, err := actions.Reserve(context.Background(), actionscache.ReserveRequest{
		Scope: actionscache.Scope{
			Repository: "acme/widget", Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
			Compatibility: "linux-amd64-schema1", SourceCommit: strings.Repeat("a", 40),
		},
		Key: "oidc-actions-only", Version: "v1", CacheSize: &size,
	}); err != nil {
		t.Fatalf("Actions request with OIDC capability: %v", err)
	}

	for name, operation := range map[string][2]string{
		"Turbo write":              {http.MethodPut, "/v8/artifacts/not-authorized"},
		"administrative operation": {http.MethodPost, "/v1/gc"},
	} {
		t.Run(name, func(t *testing.T) {
			request, err := http.NewRequest(operation[0], endpoint.URL+operation[1], strings.NewReader("x"))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+exchanged.TeamToken)
			request.Header.Set(compatibility.Header, "linux-amd64-schema1")
			denied, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer denied.Body.Close()
			if denied.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", denied.StatusCode)
			}
		})
	}

	called := false
	promotion := instance.requireToken(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		called = true
		writer.WriteHeader(http.StatusNoContent)
	}))
	promotionRequest := httptest.NewRequest(http.MethodPost, "/v1/buildkit/promotion-leases/acquire", strings.NewReader("{}"))
	promotionRequest.Header.Set("Authorization", "Bearer "+exchanged.TeamToken)
	promotionResponse := httptest.NewRecorder()
	promotion.ServeHTTP(promotionResponse, promotionRequest)
	if called || promotionResponse.Code != http.StatusUnauthorized {
		t.Fatalf("BuildKit promotion called = %v, status = %d, want false and 401", called, promotionResponse.Code)
	}

	t.Run("explicit Turbo remains isolated", func(t *testing.T) {
		post := func(extra map[string]any) *http.Response {
			t.Helper()
			body := map[string]any{"project": cfg.ProjectID, "compatibility": "linux-amd64-schema1", "idToken": idToken, "integration": "turbo"}
			for key, value := range extra {
				body[key] = value
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.Post(endpoint.URL+"/v1/auth/github-oidc/exchange", "application/json", bytes.NewReader(encoded))
			if err != nil {
				t.Fatal(err)
			}
			return response
		}
		response := post(map[string]any{"ttlSeconds": 3600})
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("Turbo exchange status = %d", response.StatusCode)
		}
		var result struct {
			TeamToken string `json:"teamToken"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		claims, err := access.ParseCapabilityToken(cfg.LocalToken, result.TeamToken, now)
		if err != nil {
			t.Fatal(err)
		}
		if claims.Integration != "turbo" || claims.Project != cfg.ProjectID || claims.Repository != "acme/widget" ||
			claims.Ref != "refs/heads/main" || claims.SourceCommit != strings.Repeat("a", 40) ||
			claims.RecipeDigest != "" || claims.Target != "" || claims.ExpiresAt.Sub(now) < 59*time.Minute || claims.ExpiresAt.Sub(now) > 61*time.Minute {
			t.Fatalf("unexpected Turbo capability: %+v", claims)
		}
		for _, operation := range []struct {
			method, path string
			want         int
		}{
			{http.MethodPut, "/v8/artifacts/turbo-oidc", http.StatusOK},
			{http.MethodPost, "/_apis/artifactcache/caches", http.StatusUnauthorized},
			{http.MethodPost, "/v1/gc", http.StatusUnauthorized},
		} {
			request, err := http.NewRequest(operation.method, endpoint.URL+operation.path, strings.NewReader("x"))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+result.TeamToken)
			request.Header.Set(compatibility.Header, "linux-amd64-schema1")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != operation.want {
				t.Fatalf("%s status = %d, want %d", operation.path, response.StatusCode, operation.want)
			}
		}
		for _, extra := range []map[string]any{
			{"integration": "all"}, {"integration": "admin"}, {"ttlSeconds": 3601},
			{"ttlSeconds": -1}, {"ttlSeconds": 1.5}, {"ttlSeconds": "3600"},
			{"integration": "actions", "ttlSeconds": 3600}, {"recipeDigest": publicRecipe},
			{"project": "github.com/acme/another"},
		} {
			response := post(extra)
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("invalid exchange %v status = %d", extra, response.StatusCode)
			}
		}
	})
}

func signOIDCTestToken(t *testing.T, privateKey *rsa.PrivateKey, keyID string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": keyID, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}
