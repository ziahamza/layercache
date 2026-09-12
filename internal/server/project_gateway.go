package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/config"
)

// ProjectGatewayConfig is an explicit project allowlist, not dynamic discovery
// from untrusted request fields. Each project retains its own signing secret,
// persistence, quota and membership policy. Origin is the shared HTTPS origin.
type ProjectGatewayConfig struct {
	Origin           string
	Projects         map[string]config.Config
	RegistryURL      string
	RegistryUsername string
	RegistryPassword string
}

type ProjectGateway struct {
	projects map[string]*Server
	aliases  map[string]*Server
	registry *httputil.ReverseProxy
}

var projectAlias = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func NewProjectGateway(ctx context.Context, cfg ProjectGatewayConfig) (_ *ProjectGateway, err error) {
	origin, err := url.Parse(cfg.Origin)
	if err != nil || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" ||
		(origin.Scheme != "https" && !(origin.Scheme == "http" && isGatewayLoopback(origin.Hostname()))) {
		return nil, errors.New("gateway origin must be an HTTPS origin, or loopback HTTP, without a path")
	}
	if len(cfg.Projects) == 0 || len(cfg.Projects) > 128 {
		return nil, errors.New("gateway requires 1 to 128 projects")
	}
	gateway := &ProjectGateway{projects: map[string]*Server{}, aliases: map[string]*Server{}}
	defer func() {
		if err != nil {
			_ = gateway.Close()
		}
	}()
	secrets := map[string]bool{}
	for alias, project := range cfg.Projects {
		if !projectAlias.MatchString(alias) || project.Role != "team" {
			return nil, errors.New("gateway projects require valid aliases and Team Cache configurations")
		}
		if _, exists := gateway.projects[project.ProjectID]; exists || secrets[project.LocalToken] {
			return nil, errors.New("gateway projects must have distinct identities and signing secrets")
		}
		project.ActionsArchiveBaseURL = cfg.Origin
		if err := project.Validate(); err != nil {
			return nil, fmt.Errorf("invalid project %s configuration: %w", alias, err)
		}
		instance, openErr := New(ctx, project)
		if openErr != nil {
			return nil, fmt.Errorf("open project %s: %w", alias, openErr)
		}
		gateway.projects[project.ProjectID], gateway.aliases[alias] = instance, instance
		secrets[project.LocalToken] = true
	}
	if cfg.RegistryURL != "" {
		target, parseErr := url.Parse(cfg.RegistryURL)
		if parseErr != nil || target.Host == "" || target.User != nil || target.Path != "" || target.RawQuery != "" || target.Fragment != "" ||
			(target.Scheme != "https" && !(target.Scheme == "http" && isGatewayLoopback(target.Hostname()))) || cfg.RegistryUsername == "" || cfg.RegistryPassword == "" {
			return nil, errors.New("registry requires an HTTPS or loopback HTTP origin and protected backend credentials")
		}
		gateway.registry = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = target.Host
				pr.Out.Header.Del("Forwarded")
				pr.Out.Header.Del("X-Forwarded-For")
				pr.Out.Header.Del("X-Forwarded-Host")
				pr.Out.Header.Del("X-Forwarded-Proto")
				pr.Out.Header.Del("X-LayerCache-Project")
				pr.Out.SetBasicAuth(cfg.RegistryUsername, cfg.RegistryPassword)
			},
			ModifyResponse: func(response *http.Response) error {
				// Do not leak internal credentials, auth realms or upload origins.
				response.Header.Del("WWW-Authenticate")
				if location := response.Header.Get("Location"); location != "" {
					parsed, err := url.Parse(location)
					if err != nil || parsed.User != nil || parsed.Fragment != "" || (parsed.IsAbs() && (parsed.Scheme != target.Scheme || parsed.Host != target.Host)) || (!parsed.IsAbs() && parsed.Host != "") {
						return errors.New("invalid registry redirect")
					}
					alias := strings.Split(strings.TrimPrefix(response.Request.URL.Path, "/v2/"), "/")[0]
					if parsed.RawPath != "" || !strings.HasPrefix(parsed.Path, "/v2/"+alias+"/") || !canonicalGatewayPath(parsed.Path) {
						return errors.New("registry redirect leaves project namespace")
					}
					parsed.Scheme, parsed.Host = "", ""
					response.Header.Set("Location", parsed.String())
				}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				http.Error(w, "registry unavailable", http.StatusBadGateway)
			},
			ErrorLog: log.New(io.Discard, "", 0),
		}
	} else if cfg.RegistryUsername != "" || cfg.RegistryPassword != "" {
		return nil, errors.New("registry credentials require a registry origin")
	}
	return gateway, nil
}

func isGatewayLoopback(host string) bool {
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}
func canonicalGatewayPath(value string) bool {
	return !strings.ContainsAny(value, "\\\x00\r\n") && (value == "/" || path.Clean(value) == strings.TrimSuffix(value, "/"))
}

func (gateway *ProjectGateway) Close() error {
	var errs []error
	for _, project := range gateway.projects {
		errs = append(errs, project.Close())
	}
	return errors.Join(errs...)
}

func (gateway *ProjectGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if !canonicalGatewayPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v2/") {
		gateway.serveRegistry(w, r)
		return
	}
	project, ok := gateway.routeProject(w, r)
	if !ok {
		return
	}
	project.Handler().ServeHTTP(w, r)
}

func (gateway *ProjectGateway) routeProject(w http.ResponseWriter, r *http.Request) (*Server, bool) {
	selected := ""
	selectProject := func(value string) bool {
		if value == "" || (selected != "" && selected != value) {
			return false
		}
		selected = value
		return true
	}
	deny := func() (*Server, bool) { http.NotFound(w, r); return nil, false }
	for _, key := range []string{"teamId", "teamSlug", "team", "slug"} {
		if values, present := r.URL.Query()[key]; present && (len(values) != 1 || !selectProject(values[0])) {
			return deny()
		}
	}
	if values := r.Header.Values("X-LayerCache-Project"); len(values) > 0 && (len(values) != 1 || !selectProject(values[0])) {
		return deny()
	}
	if len(r.Header.Values("Authorization")) > 1 {
		return deny()
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer lc2.") {
		parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
		if len(parts) != 3 || len(auth) > 32<<10 {
			return deny()
		}
		value, err := routingProject(parts[1])
		if err != nil || !selectProject(value) {
			return deny()
		}
	}
	if r.URL.Path == "/v1/auth/github/exchange" || r.URL.Path == "/v1/auth/github-oidc/exchange" {
		if r.Method != http.MethodPost {
			return deny()
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "invalid exchange request", 400)
			return nil, false
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var input struct {
			Project string `json:"project"`
		}
		if json.Unmarshal(body, &input) != nil || !selectProject(input.Project) {
			return deny()
		}
	}
	if values, present := r.URL.Query()["authority"]; present {
		if len(values) != 1 {
			return deny()
		}
		value, err := routingProject(values[0])
		if err != nil || !selectProject(value) {
			return deny()
		}
	}
	project := gateway.projects[selected]
	if project == nil {
		return deny()
	}
	// All decoded fields above are routing hints ONLY. The selected server
	// verifies the bearer signature or signed archive URL before any data access.
	return project, true
}

func routingProject(encoded string) (string, error) {
	if len(encoded) > 24<<10 {
		return "", errors.New("routing hint too large")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	var hint struct {
		Project string `json:"project"`
	}
	if err := json.Unmarshal(data, &hint); err != nil || hint.Project == "" {
		return "", errors.New("invalid project hint")
	}
	return hint.Project, nil
}

func (gateway *ProjectGateway) serveRegistry(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	deny := func() {
		w.Header().Set("WWW-Authenticate", `Basic realm="Layer Cache"`)
		http.Error(w, "unauthorized", 401)
	}
	alias, password, ok := r.BasicAuth()
	project := gateway.aliases[alias]
	if !ok || project == nil || len(password) > 32<<10 || len(r.Header.Values("Authorization")) != 1 || r.URL.RawPath != "" {
		deny()
		return
	}
	probe := r.Clone(r.Context())
	probe.Header.Set("Authorization", "Bearer "+password)
	claims, ok := project.authenticateRequest(probe, "buildkit")
	required := access.CapabilityRead
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		required = access.CapabilityWrite
	case http.MethodDelete:
		required = access.CapabilityAdmin
	default:
		deny()
		return
	}
	if !ok || !claims.Allows(required) || (claims.Integration != "" && claims.Integration != "buildkit") || !project.authorizesSelectors(probe, claims) || !project.currentMembershipAllows(r.Context(), claims, required) {
		deny()
		return
	}
	if values := r.Header.Values("X-LayerCache-Project"); len(values) > 0 && (len(values) != 1 || values[0] != claims.Project) {
		deny()
		return
	}
	if r.URL.Path == "/v2/" {
		if required != access.CapabilityRead || r.URL.RawQuery != "" {
			deny()
			return
		}
		writeJSON(w, 200, map[string]any{})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v2/"+alias+"/") {
		deny()
		return
	}
	// No cross-project blob mounts, global catalog, or cross-project listing.
	if from, present := r.URL.Query()["from"]; present {
		if len(from) != 1 || !strings.HasPrefix(from[0], alias+"/") || !canonicalGatewayPath("/"+from[0]) {
			deny()
			return
		}
	}
	if gateway.registry == nil {
		http.Error(w, "registry unavailable", 503)
		return
	}
	gateway.registry.ServeHTTP(w, r)
}
