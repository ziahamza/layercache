// Package portal owns self-serve teams and projects. Cache byte protocols remain
// in server; this module supplies their durable membership authority.
package portal

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/dashboard"
	"github.com/layercache/layercache/internal/githubauth"
	"github.com/layercache/layercache/internal/measurement"
	"github.com/layercache/layercache/internal/server"
)

//go:embed assets/*
var assets embed.FS

type Config struct {
	Origin             string
	DataDir            string
	PostgresURL        string
	GitHubClientID     string
	GitHubClientSecret string
	SessionKey         []byte
	GitHubAPIURL       string
	GitHubAuthorizeURL string
	GitHubTokenURL     string
	ProjectTemplate    config.Config
	StoragePool        server.StoragePoolConfig
}
type Portal struct {
	cfg       Config
	store     *Store
	gateway   *server.ProjectGateway
	aead      cipher.AEAD
	client    *http.Client
	mux       *http.ServeMux
	authority string
	secure    bool
}

func ValidateOrigin(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "::1" || u.Hostname() == "localhost"))) {
		return errors.New("cloud origin must be HTTPS, or loopback HTTP, without a path")
	}
	return nil
}
func New(ctx context.Context, cfg Config) (*Portal, error) {
	if err := ValidateOrigin(cfg.Origin); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(cfg.DataDir) || len(cfg.SessionKey) != 32 || cfg.GitHubClientID == "" || cfg.GitHubClientSecret == "" {
		return nil, errors.New("cloud requires an absolute data directory, 32-byte session key and GitHub OAuth credentials")
	}
	if cfg.GitHubAPIURL == "" {
		cfg.GitHubAPIURL = githubauth.DefaultAPIURL
	}
	if cfg.GitHubAuthorizeURL == "" {
		cfg.GitHubAuthorizeURL = "https://github.com/login/oauth/authorize"
	}
	if cfg.GitHubTokenURL == "" {
		cfg.GitHubTokenURL = githubauth.DefaultTokenEndpoint
	}
	for _, endpoint := range []string{cfg.GitHubAPIURL, cfg.GitHubAuthorizeURL, cfg.GitHubTokenURL} {
		u, err := url.Parse(endpoint)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, ErrInvalid
		}
		origin := u.Scheme + "://" + u.Host
		if err := ValidateOrigin(origin); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, errors.New("create cloud data directory")
	}
	if cfg.StoragePool.Path != "" {
		root, rootErr := filepath.EvalSymlinks(cfg.StoragePool.Path)
		data, dataErr := filepath.EvalSymlinks(cfg.DataDir)
		relative, relativeErr := filepath.Rel(root, data)
		if rootErr != nil || dataErr != nil || relativeErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return nil, errors.New("cloud data must be inside its bounded storage pool")
		}
	}
	block, err := aes.NewCipher(cfg.SessionKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	store, err := OpenStore(ctx, cfg.PostgresURL, filepath.Join(cfg.DataDir, "portal.db"))
	if err != nil {
		return nil, err
	}
	origin, _ := url.Parse(cfg.Origin)
	p := &Portal{cfg: cfg, store: store, aead: aead, authority: origin.Host, secure: origin.Scheme == "https", mux: http.NewServeMux(), client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	p.gateway, err = server.NewProjectGateway(ctx, server.ProjectGatewayConfig{Origin: cfg.Origin, Authority: store, StoragePool: cfg.StoragePool})
	if err != nil {
		store.Close()
		return nil, err
	}
	projects, err := store.AllProjects(ctx)
	if err != nil {
		p.Close()
		return nil, err
	}
	for _, project := range projects {
		if err = p.ensureProject(ctx, project); err != nil {
			p.Close()
			return nil, errors.New("restore managed project runtime")
		}
	}
	p.routes()
	return p, nil
}
func (p *Portal) Close() error { return errors.Join(p.gateway.Close(), p.store.Close()) }
func (p *Portal) ensureProject(ctx context.Context, project Project) error {
	cfg := p.cfg.ProjectTemplate
	cfg.Role, cfg.ProjectID, cfg.LocalToken = "team", project.ID, project.Secret
	cfg.DataDir = filepath.Join(p.cfg.DataDir, "projects", project.ID)
	cfg.ActionsRepository, cfg.ActionsDefaultRef, cfg.ActionsRef = project.Repository, project.DefaultRef, project.DefaultRef
	cfg.GitHubAPIURL = p.cfg.GitHubAPIURL
	cfg.TeamMembers = nil
	cfg.TeamURL, cfg.TeamToken, cfg.PublicURL, cfg.PublicAccessToken = "", "", "", ""
	cfg.GitHubCredentialAccount, cfg.GitHubCLIPath = "", ""
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return errors.New("create project cache directory")
	}
	return p.gateway.AddProject(ctx, project.ID, cfg)
}
func (p *Portal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if r.Host != p.authority {
		http.Error(w, "invalid cloud host", 403)
		return
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/v8/") || strings.HasPrefix(path, "/_apis/artifactcache/") || strings.HasPrefix(path, "/_layercache/compatibility/") || path == "/v1/status" || strings.HasPrefix(path, "/v1/reports") || path == "/v1/auth/github/exchange" || path == "/v1/auth/github-oidc/exchange" {
		p.gateway.ServeHTTP(w, r)
		return
	}
	p.mux.ServeHTTP(w, r)
}
func (p *Portal) routes() {
	files, _ := fs.Sub(assets, "assets")
	static := http.FileServer(http.FS(files))
	p.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { static.ServeHTTP(w, r) })
	for _, path := range []string{"/app.js", "/style.css"} {
		p.mux.Handle("GET "+path, static)
	}
	p.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, map[string]string{"status": "ok"}) })
	p.mux.HandleFunc("GET /api/public-config", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, map[string]string{"githubClientId": p.cfg.GitHubClientID})
	})
	p.mux.HandleFunc("GET /auth/github", p.beginLogin)
	p.mux.HandleFunc("GET /auth/callback", p.finishLogin)
	p.mux.HandleFunc("GET /api/cli/projects", p.cliProjects)
	p.mux.Handle("GET /api/session", p.auth(p.sessionInfo))
	p.mux.Handle("POST /api/logout", p.auth(p.logout))
	p.mux.Handle("POST /api/teams", p.auth(p.createTeam))
	p.mux.Handle("POST /api/teams/{team}/projects", p.auth(p.createProject))
	p.mux.Handle("GET /api/teams/{team}/members", p.auth(p.members))
	p.mux.Handle("PUT /api/teams/{team}/members/{user}", p.auth(p.changeMember))
	p.mux.Handle("DELETE /api/teams/{team}/members/{user}", p.auth(p.changeMember))
	p.mux.Handle("POST /api/teams/{team}/invitations", p.auth(p.invite))
	p.mux.Handle("POST /api/invitations/{invite}/accept", p.auth(p.accept))
	p.mux.Handle("GET /api/projects/{project}/snapshot", p.auth(p.projectSnapshot))
}
func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, err error) {
	status, message := 500, "cloud request failed"
	switch {
	case errors.Is(err, ErrDenied):
		status, message = 403, "access denied"
	case errors.Is(err, ErrInvalid):
		status, message = 400, "invalid input"
	case errors.Is(err, ErrConflict):
		status, message = 409, err.Error()
	case errors.Is(err, ErrLimit):
		status, message = 409, err.Error()
	}
	jsonResponse(w, status, map[string]string{"error": message})
}
func readJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return ErrInvalid
	}
	return nil
}
func (p *Portal) cookieName(suffix string) string {
	if p.secure {
		return "__Host-layercache_" + suffix
	}
	return "layercache_" + suffix
}
func (p *Portal) cookie(w http.ResponseWriter, suffix, value string, seconds int) {
	http.SetCookie(w, &http.Cookie{Name: p.cookieName(suffix), Value: value, Path: "/", MaxAge: seconds, HttpOnly: true, Secure: p.secure, SameSite: http.SameSiteLaxMode})
}
func (p *Portal) auth(next func(http.ResponseWriter, *http.Request, Session)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(p.cookieName("session"))
		if err != nil || len(cookie.Value) != 48 {
			jsonResponse(w, 401, map[string]string{"error": "sign in required"})
			return
		}
		session, err := p.store.Session(r.Context(), cookie.Value)
		if err != nil {
			jsonResponse(w, 401, map[string]string{"error": "sign in required"})
			return
		}
		if r.Method != "GET" && (r.Header.Get("Origin") != p.cfg.Origin || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRF)) != 1) {
			fail(w, ErrDenied)
			return
		}
		next(w, r, session)
	})
}
func (p *Portal) encrypt(value string) string {
	nonce := make([]byte, p.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic("system random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(p.aead.Seal(nonce, nonce, []byte(value), []byte(p.cfg.Origin)))
}
func (p *Portal) decrypt(value string) (string, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(data) < p.aead.NonceSize() {
		return "", ErrDenied
	}
	decoded, err := p.aead.Open(nil, data[:p.aead.NonceSize()], data[p.aead.NonceSize():], []byte(p.cfg.Origin))
	return string(decoded), err
}
func (p *Portal) beginLogin(w http.ResponseWriter, r *http.Request) {
	state, verifier := randomID(""), randomID("")
	if err := p.store.SaveOAuth(r.Context(), state, verifier); err != nil {
		fail(w, err)
		return
	}
	p.cookie(w, "oauth", state, 600)
	hash := sha256.Sum256([]byte(verifier))
	u, _ := url.Parse(p.cfg.GitHubAuthorizeURL)
	query := url.Values{"client_id": {p.cfg.GitHubClientID}, "redirect_uri": {p.cfg.Origin + "/auth/callback"}, "scope": {"read:user repo"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}}
	u.RawQuery = query.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}
func (p *Portal) finishLogin(w http.ResponseWriter, r *http.Request) {
	states, codes := r.URL.Query()["state"], r.URL.Query()["code"]
	cookie, err := r.Cookie(p.cookieName("oauth"))
	p.cookie(w, "oauth", "", -1)
	if err != nil || len(states) != 1 || len(codes) != 1 || len(codes[0]) > 1024 || len(states[0]) != 48 || subtle.ConstantTimeCompare([]byte(states[0]), []byte(cookie.Value)) != 1 {
		fail(w, ErrDenied)
		return
	}
	verifier, err := p.store.ConsumeOAuth(r.Context(), states[0])
	if err != nil {
		fail(w, ErrDenied)
		return
	}
	form := url.Values{"client_id": {p.cfg.GitHubClientID}, "client_secret": {p.cfg.GitHubClientSecret}, "code": {codes[0]}, "redirect_uri": {p.cfg.Origin + "/auth/callback"}, "code_verifier": {verifier}}
	request, err := http.NewRequestWithContext(r.Context(), "POST", p.cfg.GitHubTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		fail(w, err)
		return
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		fail(w, ErrDenied)
		return
	}
	defer response.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&token) != nil || token.AccessToken == "" {
		fail(w, ErrDenied)
		return
	}
	identity, err := githubauth.VerifyUser(r.Context(), p.cfg.GitHubAPIURL, token.AccessToken)
	if err != nil {
		fail(w, ErrDenied)
		return
	}
	user := User{ID: userID(identity.ID), Login: identity.Login}
	if err = p.store.UpsertUser(r.Context(), user); err != nil {
		fail(w, err)
		return
	}
	sessionToken := randomID("")
	session := Session{User: user, CSRF: randomID(""), EncryptedToken: p.encrypt(token.AccessToken), ExpiresAt: time.Now().Add(12 * time.Hour).Unix()}
	if err = p.store.SaveSession(r.Context(), sessionToken, session); err != nil {
		fail(w, err)
		return
	}
	p.cookie(w, "session", sessionToken, 12*3600)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (p *Portal) sessionInfo(w http.ResponseWriter, r *http.Request, session Session) {
	teams, err := p.store.Teams(r.Context(), session.User.ID)
	if err != nil {
		fail(w, err)
		return
	}
	projects, err := p.store.Projects(r.Context(), session.User.ID)
	if err != nil {
		fail(w, err)
		return
	}
	invitations, err := p.store.Invitations(r.Context(), session.User.ID)
	if err != nil {
		fail(w, err)
		return
	}
	jsonResponse(w, 200, map[string]any{"user": session.User, "csrfToken": session.CSRF, "teams": teams, "projects": projects, "invitations": invitations, "cloudOrigin": p.cfg.Origin})
}
func (p *Portal) cliProjects(w http.ResponseWriter, r *http.Request) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || len(values[0]) > 16<<10 {
		fail(w, ErrDenied)
		return
	}
	identity, err := githubauth.VerifyUser(r.Context(), p.cfg.GitHubAPIURL, strings.TrimPrefix(values[0], "Bearer "))
	if err != nil {
		fail(w, ErrDenied)
		return
	}
	user := User{ID: userID(identity.ID), Login: identity.Login}
	if err = p.store.UpsertUser(r.Context(), user); err != nil {
		fail(w, err)
		return
	}
	projects, err := p.store.Projects(r.Context(), user.ID)
	if err != nil {
		fail(w, err)
		return
	}
	teams, err := p.store.Teams(r.Context(), user.ID)
	if err != nil {
		fail(w, err)
		return
	}
	jsonResponse(w, 200, map[string]any{"user": user, "teams": teams, "projects": projects})
}
func (p *Portal) logout(w http.ResponseWriter, r *http.Request, session Session) {
	cookie, _ := r.Cookie(p.cookieName("session"))
	if err := p.store.DeleteSession(r.Context(), cookie.Value); err != nil {
		fail(w, err)
		return
	}
	p.cookie(w, "session", "", -1)
	w.WriteHeader(204)
}
func (p *Portal) createTeam(w http.ResponseWriter, r *http.Request, session Session) {
	var input struct {
		Name string `json:"name"`
	}
	if err := readJSON(w, r, &input); err != nil {
		fail(w, err)
		return
	}
	team, err := p.store.CreateTeam(r.Context(), session.User.ID, strings.TrimSpace(input.Name))
	if err != nil {
		fail(w, err)
		return
	}
	jsonResponse(w, 201, team)
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9_.-]{1,100}$`)
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

func (p *Portal) githubGET(ctx context.Context, token, path string, result any) error {
	request, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(p.cfg.GitHubAPIURL, "/")+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := p.client.Do(request)
	if err != nil {
		return ErrDenied
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result) != nil {
		return ErrDenied
	}
	return nil
}
func (p *Portal) createProject(w http.ResponseWriter, r *http.Request, session Session) {
	var input struct {
		Name       string `json:"name"`
		Repository string `json:"repository"`
	}
	if err := readJSON(w, r, &input); err != nil {
		fail(w, err)
		return
	}
	input.Repository = strings.ToLower(strings.TrimSpace(input.Repository))
	if !repositoryPattern.MatchString(input.Repository) || strings.Contains(input.Repository, "..") {
		fail(w, ErrInvalid)
		return
	}
	token, err := p.decrypt(session.EncryptedToken)
	if err != nil {
		fail(w, ErrDenied)
		return
	}
	var repository struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Permissions   struct {
			Admin bool `json:"admin"`
		} `json:"permissions"`
	}
	if err = p.githubGET(r.Context(), token, "/repos/"+input.Repository, &repository); err != nil || !repository.Permissions.Admin || !strings.EqualFold(repository.FullName, input.Repository) || repository.DefaultBranch == "" || strings.ContainsAny(repository.DefaultBranch, "\x00\r\n") {
		jsonResponse(w, 403, map[string]string{"error": "GitHub repository administrator access is required; sign in again if access changed"})
		return
	}
	project, err := p.store.CreateProject(r.Context(), session.User.ID, r.PathValue("team"), strings.TrimSpace(input.Name), input.Repository, "refs/heads/"+repository.DefaultBranch)
	if err != nil {
		fail(w, err)
		return
	}
	if err = p.ensureProject(r.Context(), project); err != nil {
		jsonResponse(w, 503, map[string]string{"error": "project saved; cache provisioning will retry when you open it"})
		return
	}
	jsonResponse(w, 201, project)
}
func (p *Portal) members(w http.ResponseWriter, r *http.Request, session Session) {
	members, err := p.store.Members(r.Context(), session.User.ID, r.PathValue("team"))
	if err != nil {
		fail(w, err)
		return
	}
	jsonResponse(w, 200, members)
}
func (p *Portal) changeMember(w http.ResponseWriter, r *http.Request, session Session) {
	var input struct {
		Role string `json:"role"`
	}
	if r.Method != "DELETE" {
		if err := readJSON(w, r, &input); err != nil {
			fail(w, err)
			return
		}
		if !validRole(input.Role) {
			fail(w, ErrInvalid)
			return
		}
	}
	if err := p.store.ChangeMember(r.Context(), session.User.ID, r.PathValue("team"), r.PathValue("user"), input.Role); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}
func (p *Portal) invite(w http.ResponseWriter, r *http.Request, session Session) {
	var input struct {
		Login string `json:"login"`
		Role  string `json:"role"`
	}
	if err := readJSON(w, r, &input); err != nil {
		fail(w, err)
		return
	}
	if !loginPattern.MatchString(input.Login) || !validRole(input.Role) {
		fail(w, ErrInvalid)
		return
	}
	token, err := p.decrypt(session.EncryptedToken)
	if err != nil {
		fail(w, ErrDenied)
		return
	}
	var target githubauth.User
	if err = p.githubGET(r.Context(), token, "/users/"+url.PathEscape(input.Login), &target); err != nil || target.ID <= 0 {
		fail(w, ErrDenied)
		return
	}
	invitation, err := p.store.Invite(r.Context(), session.User.ID, r.PathValue("team"), strconv.FormatInt(target.ID, 10), input.Role)
	if err != nil {
		fail(w, err)
		return
	}
	jsonResponse(w, 201, invitation)
}
func (p *Portal) accept(w http.ResponseWriter, r *http.Request, session Session) {
	if err := p.store.Accept(r.Context(), session.User.ID, r.PathValue("invite")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

type capture struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (c *capture) Header() http.Header { return c.header }
func (c *capture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}
func (c *capture) Write(data []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	if c.body.Len()+len(data) > 1<<20 {
		return 0, errors.New("snapshot exceeds limit")
	}
	return c.body.Write(data)
}
func (p *Portal) projectSnapshot(w http.ResponseWriter, r *http.Request, session Session) {
	projects, err := p.store.Projects(r.Context(), session.User.ID)
	if err != nil {
		fail(w, err)
		return
	}
	var selected *Project
	for i := range projects {
		if projects[i].ID == r.PathValue("project") {
			selected = &projects[i]
			break
		}
	}
	if selected == nil {
		fail(w, ErrDenied)
		return
	}
	if err = p.ensureProject(r.Context(), *selected); err != nil {
		jsonResponse(w, 503, map[string]string{"error": "project cache is unavailable"})
		return
	}
	now := time.Now().UTC()
	token, err := access.MintCapabilityToken(selected.Secret, access.Claims{Subject: "github-id:" + session.User.ID, Project: selected.ID, Capabilities: []access.Capability{access.CapabilityRead}, ExpiresAt: now.Add(time.Minute)}, now)
	if err != nil {
		fail(w, err)
		return
	}
	read := func(path string, target any) bool {
		req, _ := http.NewRequestWithContext(r.Context(), "GET", p.cfg.Origin+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		out := &capture{header: http.Header{}}
		p.gateway.ServeHTTP(out, req)
		return out.status == 200 && json.Unmarshal(out.body.Bytes(), target) == nil
	}
	cache := dashboard.Cache{State: "unavailable", ReportState: "unavailable"}
	var status dashboard.Status
	if read("/v1/status", &status) {
		cache.State = "connected"
		cache.Status = &status
	}
	period := 24 * time.Hour
	if r.URL.Query().Get("period") == "7d" {
		period = 7 * 24 * time.Hour
	} else if value := r.URL.Query().Get("period"); value != "" && value != "24h" {
		fail(w, ErrInvalid)
		return
	}
	var report measurement.PeriodReport
	query := url.Values{"from": {now.Add(-period).Format(time.RFC3339Nano)}, "to": {now.Format(time.RFC3339Nano)}}
	if read("/v1/reports?"+query.Encode(), &report) {
		cache.Report = &report
		cache.ReportState = "connected"
	}
	jsonResponse(w, 200, map[string]any{"project": selected, "cache": cache, "updatedAt": now})
}
