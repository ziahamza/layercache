package portal

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/config"
)

type portalQA struct {
	t              *testing.T
	p              *Portal
	cfg            Config
	github         *httptest.Server
	mu             sync.Mutex
	challenge      string
	admin          bool
	revoked        bool
	exchanges      int
	tokenExpiresIn int64
}

func signPortalOIDCTestToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": "portal-qa"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	message := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(message))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return message + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestPortalManagedOIDCRejectsRecycledRepositoryName(t *testing.T) {
	q := newPortalQA(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "portal-qa", "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
		}}})
	}))
	defer issuer.Close()
	q.cfg.ProjectTemplate.GitHubOIDCIssuer = issuer.URL
	q.p.cfg.ProjectTemplate.GitHubOIDCIssuer = issuer.URL
	alice := q.login("alice")
	team := q.team(alice, "Team")
	project := q.project(alice, team, "acme/alpha")
	if id, err := q.p.store.RepositoryID(context.Background(), project.ID); err != nil || id != "123" {
		t.Fatalf("provisioned repository ID = %q, %v", id, err)
	}
	exchange := func(repositoryID string, includeID bool) int {
		t.Helper()
		now := time.Now().UTC()
		claims := map[string]any{
			"iss": issuer.URL, "aud": "layercache:" + project.ID,
			"iat": now.Add(-time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix(),
			"sub": "repo:acme/alpha:ref:refs/heads/main", "repository": "acme/alpha",
			"ref": "refs/heads/main", "sha": strings.Repeat("a", 40),
			"event_name": "push", "run_id": "123", "run_attempt": "1", "check_run_id": "456",
		}
		if includeID {
			claims["repository_id"] = repositoryID
		}
		body, err := json.Marshal(map[string]string{
			"project": project.ID, "compatibility": "linux-amd64-schema1",
			"idToken": signPortalOIDCTestToken(t, key, claims),
		})
		if err != nil {
			t.Fatal(err)
		}
		return q.request("POST", "/v1/auth/github-oidc/exchange", string(body), nil, nil).Code
	}
	if code := exchange("123", true); code != http.StatusOK {
		t.Fatalf("original repository OIDC exchange = %d, want 200", code)
	}
	// A new repository at the same owner/name carries a different immutable ID.
	for _, tc := range []struct {
		id      string
		include bool
	}{{"456", true}, {"", false}, {"not-a-number", true}} {
		if code := exchange(tc.id, tc.include); code != http.StatusUnauthorized {
			t.Fatalf("recycled or missing repository ID %q exchange = %d, want 401", tc.id, code)
		}
	}
	if _, err := q.p.store.db.Exec(`UPDATE lc_portal_projects SET repository_id='' WHERE id=$1`, project.ID); err != nil {
		t.Fatal(err)
	}
	if code := exchange("123", true); code != http.StatusUnauthorized {
		t.Fatalf("legacy blank repository ID exchange = %d, want 401", code)
	}
}

type browserQA struct {
	cookie *http.Cookie
	csrf   string
}

func newPortalQA(t *testing.T) *portalQA {
	t.Helper()
	q := &portalQA{t: t, admin: true}
	q.github = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q.mu.Lock()
		defer q.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/token":
			q.exchanges++
			_ = r.ParseForm()
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("client_secret") != "oauth-secret" || r.Form.Get("redirect_uri") != "https://cloud.example/auth/callback" || base64.RawURLEncoding.EncodeToString(sum[:]) != q.challenge {
				http.Error(w, "bad PKCE exchange", 400)
				return
			}
			response := map[string]any{"access_token": "github-" + r.Form.Get("code")}
			if q.tokenExpiresIn > 0 {
				response["expires_in"] = q.tokenExpiresIn
			}
			_ = json.NewEncoder(w).Encode(response)
		case r.URL.Path == "/user":
			if q.revoked {
				http.Error(w, "revoked", 401)
				return
			}
			token := r.Header.Get("Authorization")
			id, login := 1, "alice"
			if token == "Bearer github-bob" {
				id, login = 2, "bob"
			} else if token != "Bearer github-alice" {
				http.Error(w, "bad token", 401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "login": login})
		case strings.HasPrefix(r.URL.Path, "/repos/"):
			if q.revoked {
				http.Error(w, "revoked", 401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 123, "full_name": strings.TrimPrefix(r.URL.Path, "/repos/"), "default_branch": "main", "permissions": map[string]bool{"admin": q.admin}})
		case r.URL.Path == "/users/bob":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2, "login": "bob"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(q.github.Close)
	template, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	template.MaxBytes = 8 << 20
	template.MinFreeBytes = 0
	q.cfg = Config{Origin: "https://cloud.example", DataDir: t.TempDir(), SessionKey: []byte(strings.Repeat("k", 32)), GitHubClientID: "oauth-id", GitHubClientSecret: "oauth-secret", TeamCreatorIDs: []string{"1"}, GitHubAPIURL: q.github.URL, GitHubAuthorizeURL: q.github.URL + "/authorize", GitHubTokenURL: q.github.URL + "/token", ProjectTemplate: template}
	q.p, err = New(context.Background(), q.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.p.Close() })
	return q
}
func (q *portalQA) request(method, path, body string, b *browserQA, headers map[string]string) *httptest.ResponseRecorder {
	q.t.Helper()
	r := httptest.NewRequest(method, q.cfg.Origin+path, strings.NewReader(body))
	if b != nil {
		r.AddCookie(b.cookie)
		r.Header.Set("Origin", q.cfg.Origin)
		r.Header.Set("X-CSRF-Token", b.csrf)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	q.p.ServeHTTP(w, r)
	return w
}
func requireStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d want %d: %s", w.Code, status, w.Body.String())
	}
}
func decodeQA[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func (q *portalQA) begin() (*http.Cookie, url.Values) {
	q.t.Helper()
	w := q.request("GET", "/auth/github", "", nil, nil)
	requireStatus(q.t, w, 302)
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		q.t.Fatal(err)
	}
	values := u.Query()
	if values.Get("code_challenge_method") != "S256" {
		q.t.Fatal("missing PKCE")
	}
	q.mu.Lock()
	q.challenge = values.Get("code_challenge")
	q.mu.Unlock()
	return w.Result().Cookies()[0], values
}
func (q *portalQA) login(user string) *browserQA {
	q.t.Helper()
	cookie, query := q.begin()
	w := q.request("GET", "/auth/callback?state="+query.Get("state")+"&code="+user, "", &browserQA{cookie: cookie}, nil)
	requireStatus(q.t, w, 303)
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if strings.HasSuffix(c.Name, "_session") {
			session = c
		}
	}
	if session == nil || !session.Secure || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Path != "/" {
		q.t.Fatal("missing hardened session cookie")
	}
	b := &browserQA{cookie: session}
	w = q.request("GET", "/api/session", "", b, nil)
	requireStatus(q.t, w, 200)
	data := decodeQA[map[string]json.RawMessage](q.t, w)
	if err := json.Unmarshal(data["csrfToken"], &b.csrf); err != nil {
		q.t.Fatal(err)
	}
	return b
}
func (q *portalQA) team(b *browserQA, name string) Team {
	w := q.request("POST", "/api/teams", fmt.Sprintf(`{"name":%q}`, name), b, nil)
	requireStatus(q.t, w, 201)
	return decodeQA[Team](q.t, w)
}
func (q *portalQA) project(b *browserQA, team Team, repo string) Project {
	w := q.request("POST", "/api/teams/"+team.ID+"/projects", fmt.Sprintf(`{"name":"Build","repository":%q}`, repo), b, nil)
	requireStatus(q.t, w, 201)
	return decodeQA[Project](q.t, w)
}
func (q *portalQA) capability(project Project, user string) string {
	w := q.request("POST", "/v1/auth/github/exchange", fmt.Sprintf(`{"project":%q,"githubToken":%q,"compatibility":"linux-amd64-schema1","repository":%q,"ref":"refs/heads/main","defaultRef":"refs/heads/main"}`, project.ID, "github-"+user, project.Repository), nil, nil)
	requireStatus(q.t, w, 200)
	return decodeQA[struct {
		Token string `json:"token"`
	}](q.t, w).Token
}

func TestPortalTeamCreationAdmissionAndInvitation(t *testing.T) {
	q := newPortalQA(t)
	alice, bob := q.login("alice"), q.login("bob")
	for _, tc := range []struct {
		browser *browserQA
		allowed bool
	}{{alice, true}, {bob, false}} {
		response := q.request("GET", "/api/session", "", tc.browser, nil)
		requireStatus(t, response, 200)
		session := decodeQA[struct {
			CanCreateTeam bool `json:"canCreateTeam"`
		}](t, response)
		if session.CanCreateTeam != tc.allowed {
			t.Fatalf("team creation advertised as %v, want %v", session.CanCreateTeam, tc.allowed)
		}
	}
	response := q.request("POST", "/api/teams", `{"name":"Unauthorized"}`, bob, nil)
	requireStatus(t, response, 403)
	if !strings.Contains(response.Body.String(), "invited beta creators") {
		t.Fatal("team admission denial did not explain beta access")
	}
	team := q.team(alice, "Beta team")
	requireStatus(t, q.request("POST", "/api/teams/"+team.ID+"/invitations", `{"login":"bob","role":"reader"}`, alice, nil), 201)
	response = q.request("GET", "/api/session", "", bob, nil)
	requireStatus(t, response, 200)
	workspace := decodeQA[struct {
		Invitations []Invitation `json:"invitations"`
		Teams       []Team       `json:"teams"`
	}](t, response)
	if len(workspace.Invitations) != 1 || len(workspace.Teams) != 0 {
		t.Fatalf("expected pending invitation for non-creator, got %+v", workspace)
	}
	requireStatus(t, q.request("POST", "/api/invitations/"+workspace.Invitations[0].ID+"/accept", "", bob, nil), 204)
	response = q.request("GET", "/api/session", "", bob, nil)
	requireStatus(t, response, 200)
	workspace = decodeQA[struct {
		Invitations []Invitation `json:"invitations"`
		Teams       []Team       `json:"teams"`
	}](t, response)
	if len(workspace.Invitations) != 0 || len(workspace.Teams) != 1 || workspace.Teams[0].ID != team.ID {
		t.Fatalf("non-creator could not use invited team: %+v", workspace)
	}
	requireStatus(t, q.request("POST", "/api/teams", `{"name":"Still unauthorized"}`, bob, nil), 403)
	if err := q.p.Close(); err != nil {
		t.Fatal(err)
	}
	q.cfg.TeamCreatorIDs = []string{"2"}
	var err error
	q.p, err = New(context.Background(), q.cfg)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, q.request("POST", "/api/teams", `{"name":"Former creator"}`, alice, nil), 403)
	q.team(bob, "New creator")
}

func TestPortalOAuthStatePKCEReplayAndSessionCSRF(t *testing.T) {
	q := newPortalQA(t)
	cookie, query := q.begin()
	state := query.Get("state")
	for _, path := range []string{"/auth/callback?state=" + state + "&code=alice", "/auth/callback?state=" + state + "&state=" + state + "&code=alice"} {
		requireStatus(t, q.request("GET", path, "", nil, nil), 403)
	}
	requireStatus(t, q.request("GET", "/auth/callback?state="+strings.Repeat("0", 48)+"&code=alice", "", &browserQA{cookie: cookie}, nil), 403)
	w := q.request("GET", "/auth/callback?state="+state+"&code=alice", "", &browserQA{cookie: cookie}, nil)
	requireStatus(t, w, 303)
	requireStatus(t, q.request("GET", "/auth/callback?state="+state+"&code=alice", "", &browserQA{cookie: cookie}, nil), 403)
	q.mu.Lock()
	exchanges := q.exchanges
	q.mu.Unlock()
	if exchanges != 1 {
		t.Fatalf("OAuth replay reached exchange %d times", exchanges)
	}
	b := q.login("alice")
	for _, headers := range []map[string]string{{"Origin": "https://attacker.example"}, {"Origin": ""}, {"X-CSRF-Token": ""}, {"X-CSRF-Token": "incorrect"}} {
		requireStatus(t, q.request("POST", "/api/teams", `{"name":"Unauthorized"}`, b, headers), 403)
	}
	requireStatus(t, q.request("GET", "/api/session", "", nil, nil), 401)
	q.team(b, "Allowed")
	requireStatus(t, q.request("POST", "/api/logout", "", b, nil), 204)
	requireStatus(t, q.request("GET", "/api/session", "", b, nil), 401)
}

func TestPortalBrowserSessionEndsBeforeExpiringGitHubToken(t *testing.T) {
	q := newPortalQA(t)
	q.tokenExpiresIn = 3600
	cookie, query := q.begin()
	response := q.request("GET", "/auth/callback?state="+query.Get("state")+"&code=alice", "", &browserQA{cookie: cookie}, nil)
	requireStatus(t, response, 303)
	for _, issued := range response.Result().Cookies() {
		if strings.HasSuffix(issued.Name, "_session") {
			if issued.MaxAge <= 0 || issued.MaxAge >= 3600 {
				t.Fatalf("browser session cookie lifetime = %d seconds; provider token expires in 3600 seconds", issued.MaxAge)
			}
			requireStatus(t, q.request("GET", "/api/session", "", &browserQA{cookie: issued}, nil), 200)
			return
		}
	}
	t.Fatal("OAuth callback did not issue a browser session cookie")
}

func TestPortalRejectsSessionAfterGitHubTokenLifetime(t *testing.T) {
	q := newPortalQA(t)
	q.tokenExpiresIn = 62
	b := q.login("alice")
	time.Sleep(3 * time.Second)
	requireStatus(t, q.request("GET", "/api/session", "", b, nil), 401)
}
func TestPortalProjectPermissionsAndSecretProjection(t *testing.T) {
	q := newPortalQA(t)
	alice := q.login("alice")
	bob := q.login("bob")
	team := q.team(alice, "Team")
	q.mu.Lock()
	q.admin = false
	q.mu.Unlock()
	requireStatus(t, q.request("POST", "/api/teams/"+team.ID+"/projects", `{"name":"Build","repository":"acme/alpha"}`, alice, nil), 403)
	q.mu.Lock()
	q.admin = true
	q.mu.Unlock()
	requireStatus(t, q.request("POST", "/api/teams/"+team.ID+"/projects", `{"name":"Build","repository":"acme/alpha"}`, bob, nil), 403)
	project := q.project(alice, team, "acme/alpha")
	requireStatus(t, q.request("GET", "/api/projects/"+project.ID+"/snapshot", "", bob, nil), 403)
	for _, path := range []string{"/api/session", "/api/projects/" + project.ID + "/snapshot", "/api/cli/projects"} {
		headers := map[string]string{}
		if path == "/api/cli/projects" {
			headers["Authorization"] = "Bearer github-alice"
		}
		w := q.request("GET", path, "", alice, headers)
		requireStatus(t, w, 200)
		for _, secret := range []string{"github-alice", "oauth-secret", "localToken", "encryptedToken", "dataDir"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("%s leaked %s", path, secret)
			}
		}
	}
	w := q.request("GET", "/api/projects/"+project.ID+"/snapshot?period=forever", "", alice, nil)
	requireStatus(t, w, 400)
	q.mu.Lock()
	q.revoked = true
	q.mu.Unlock()
	requireStatus(t, q.request("POST", "/api/teams/"+team.ID+"/projects", `{"name":"Build","repository":"acme/beta"}`, alice, nil), 403)
}
func TestPortalCacheIsolationRevocationAndRestart(t *testing.T) {
	q := newPortalQA(t)
	alice := q.login("alice")
	bob := q.login("bob")
	team := q.team(alice, "Team")
	first := q.project(alice, team, "acme/alpha")
	second := q.project(alice, team, "acme/beta")
	w := q.request("POST", "/api/teams/"+team.ID+"/invitations", `{"login":"bob","role":"writer"}`, alice, nil)
	requireStatus(t, w, 201)
	invite := decodeQA[Invitation](t, w)
	requireStatus(t, q.request("POST", "/api/invitations/"+invite.ID+"/accept", "", alice, nil), 403)
	requireStatus(t, q.request("POST", "/api/invitations/"+invite.ID+"/accept", "", bob, nil), 204)
	tokens := []string{q.capability(first, "bob"), q.capability(second, "alice")}
	for i, token := range tokens {
		headers := map[string]string{"Authorization": "Bearer " + token, "X-LayerCache-Compatibility": "linux-amd64-schema1"}
		requireStatus(t, q.request("PUT", "/v8/artifacts/shared-key", fmt.Sprintf("project-%d", i), nil, headers), 200)
	}
	if err := q.p.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	q.p, err = New(context.Background(), q.cfg)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, q.request("GET", "/api/session", "", bob, nil), 200)
	for i, token := range tokens {
		w := q.request("GET", "/v8/artifacts/shared-key", "", nil, map[string]string{"Authorization": "Bearer " + token, "X-LayerCache-Compatibility": "linux-amd64-schema1"})
		requireStatus(t, w, 200)
		if w.Body.String() != fmt.Sprintf("project-%d", i) {
			t.Fatal("cross-project artifact mixup")
		}
	}

	headers := map[string]string{"Authorization": "Bearer " + tokens[0], "X-LayerCache-Compatibility": "linux-amd64-schema1"}
	requireStatus(t, q.request("GET", "/v8/artifacts/shared-key?teamId="+second.ID, "", nil, headers), 404)
	requireStatus(t, q.request("PUT", "/api/teams/"+team.ID+"/members/2", `{"role":"reader"}`, alice, nil), 204)
	if response := q.request("PUT", "/v8/artifacts/downgraded-writer", "must not write", nil, headers); response.Code < 400 {
		t.Fatal("previously issued writer capability survived downgrade")
	}
	requireStatus(t, q.request("GET", "/v8/artifacts/shared-key", "", nil, headers), 200)
	requireStatus(t, q.request("DELETE", "/api/teams/"+team.ID+"/members/2", "", alice, nil), 204)
	for _, path := range []string{"/v8/artifacts/shared-key", "/v1/status"} {
		w := q.request("GET", path, "", nil, map[string]string{"Authorization": "Bearer " + tokens[0], "X-LayerCache-Compatibility": "linux-amd64-schema1"})
		if w.Code < 400 {
			t.Fatalf("revoked capability accepted at %s", path)
		}
	}
	requireStatus(t, q.request("GET", "/api/projects/"+first.ID+"/snapshot", "", bob, nil), 403)
}
func TestPortalExpiredSessionAndOAuth(t *testing.T) {
	q := newPortalQA(t)
	b := q.login("alice")
	_, err := q.p.store.db.Exec(`UPDATE lc_portal_sessions SET expires_at=$1`, time.Now().Add(-time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, q.request("GET", "/api/session", "", b, nil), 401)
	cookie, query := q.begin()
	_, err = q.p.store.db.Exec(`UPDATE lc_portal_oauth SET expires_at=$1`, time.Now().Add(-time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, q.request("GET", "/auth/callback?state="+query.Get("state")+"&code=alice", "", &browserQA{cookie: cookie}, nil), 403)
}
