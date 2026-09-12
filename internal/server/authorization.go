package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/compatibility"
)

const publicBuildStatusProbePath = "/v1/public-builds/public-build-0"

func (server *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if server.actionsHandler != nil && server.actionsHandler.AuthorizesArchiveDownload(request) {
			next.ServeHTTP(writer, request)
			return
		}

		required, integration := requiredRequestCapability(request)
		claims, ok := server.authenticateRequest(request, integration)
		if !ok || !claims.Allows(required) || !server.authorizesSelectors(request, claims) {
			server.writeAuthorizationFailure(writer, request)
			return
		}
		if claims.Integration != "" && claims.Integration != integration {
			server.writeAuthorizationFailure(writer, request)
			return
		}

		ctx := access.WithClaims(request.Context(), claims)
		if integration == "actions" {
			if claims.Compatibility == "" {
				writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "send exactly one compatibility selector"})
				return
			}
			authority, valid := actionsAuthority(claims)
			if !valid {
				server.writeAuthorizationFailure(writer, request)
				return
			}
			ctx = actionscache.WithRequestAuthority(ctx, authority)
		}
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func (server *Server) authenticateRequest(request *http.Request, integration string) (access.Claims, bool) {
	values := request.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return access.Claims{}, false
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return access.Claims{}, false
	}
	now := time.Now().UTC()
	if subtle.ConstantTimeCompare([]byte(token), []byte(server.config.LocalToken)) == 1 {
		return server.localAdministratorClaims(request, integration, now), true
	}
	claims, err := access.ParseCapabilityToken(server.config.LocalToken, token, now)
	if err == nil && claims.Project == server.config.ProjectID {
		return claims, true
	}
	// Preserve already-issued v1 Workspace tokens only on the loopback/local
	// runtime. Team and Public services never accept their unscoped authority.
	if server.config.Role == "local" {
		runID, legacyErr := access.ParseWorkspaceToken(server.config.LocalToken, token, now)
		if legacyErr == nil {
			claims := server.localAdministratorClaims(request, integration, now)
			claims.Subject = server.config.InstallationID
			claims.RunID = runID
			claims.Capabilities = []access.Capability{access.CapabilityWrite}
			return claims, true
		}
	}
	return access.Claims{}, false
}

func (server *Server) localAdministratorClaims(request *http.Request, integration string, now time.Time) access.Claims {
	compatibilityID := server.config.CompatibilityID
	if server.config.Role == "team" {
		compatibilityID = ""
	}
	if selected, ok := singleCompatibilitySelector(request); ok && compatibility.Validate(selected) == nil {
		compatibilityID = selected
	}
	return access.Claims{
		Subject:       "runtime-admin:" + server.config.InstallationID,
		Project:       server.config.ProjectID,
		Integration:   integration,
		Compatibility: compatibilityID,
		Repository:    strings.ToLower(server.config.ActionsRepository),
		Ref:           server.config.ActionsRef,
		DefaultRef:    server.config.ActionsDefaultRef,
		Capabilities:  []access.Capability{access.CapabilityAdmin},
		ExpiresAt:     now.Add(24 * time.Hour),
	}
}

func requiredRequestCapability(request *http.Request) (access.Capability, string) {
	path := request.URL.Path
	integration := ""
	switch {
	case strings.HasPrefix(path, "/v8/"):
		integration = "turbo"
	case strings.HasPrefix(path, "/_apis/artifactcache/") || strings.HasPrefix(path, "/_layercache/compatibility/"):
		integration = "actions"
	case strings.HasPrefix(path, "/v1/buildkit/promotion-leases/"):
		integration = "buildkit"
	}
	if path == "/v1/gc" || path == "/v1/public/revoke" || strings.HasPrefix(path, "/v1/cache/") || strings.HasPrefix(path, "/v1/audit") || strings.HasPrefix(path, "/v1/members") {
		return access.CapabilityAdmin, integration
	}
	if path == publicBuildStatusProbePath {
		// Public Build submission requires write authority. The reserved GET
		// probe is intentionally non-mutating while checking that same authority.
		return access.CapabilityWrite, integration
	}
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		return access.CapabilityRead, integration
	}
	return access.CapabilityWrite, integration
}

func (server *Server) authorizesSelectors(request *http.Request, claims access.Claims) bool {
	if claims.Project != server.config.ProjectID {
		return false
	}
	headerSelectors := request.Header.Values(compatibility.Header)
	pathSelector := compatibilityPathSelector(request)
	if len(headerSelectors) > 1 || len(headerSelectors) == 1 && pathSelector != "" && headerSelectors[0] != pathSelector {
		return false
	}
	if claims.Compatibility != "" {
		if err := compatibility.Validate(claims.Compatibility); err != nil {
			return false
		}
		if selected, present := singleCompatibilitySelector(request); present && selected != claims.Compatibility {
			return false
		}
	}
	for _, key := range []string{"teamId", "teamSlug", "team", "slug"} {
		values, present := request.URL.Query()[key]
		if !present {
			continue
		}
		if len(values) != 1 || values[0] != claims.Project {
			return false
		}
	}
	return true
}

func singleCompatibilitySelector(request *http.Request) (string, bool) {
	header := request.Header.Values(compatibility.Header)
	if len(header) > 1 {
		return "", false
	}
	path := compatibilityPathSelector(request)
	if len(header) == 1 && path != "" && header[0] != path {
		return "", false
	}
	if len(header) == 1 {
		return header[0], true
	}
	if path != "" {
		return path, true
	}
	return "", false
}

func compatibilityPathSelector(request *http.Request) string {
	if selected := request.PathValue("compatibility"); selected != "" {
		return selected
	}
	const prefix = "/_layercache/compatibility/"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		return ""
	}
	selected, _, found := strings.Cut(strings.TrimPrefix(request.URL.Path, prefix), "/")
	if !found {
		return ""
	}
	return selected
}

func actionsAuthority(claims access.Claims) (actionscache.RequestAuthority, bool) {
	authority := actionscache.RequestAuthority{
		Project:    claims.Project,
		Repository: strings.ToLower(claims.Repository), Ref: claims.Ref, DefaultRef: claims.DefaultRef,
		Compatibility: claims.Compatibility, SourceCommit: claims.SourceCommit,
		RecipeDigest: claims.RecipeDigest, Target: claims.Target, Platform: claims.Platform,
		Toolchain: claims.Toolchain, Builder: claims.Builder,
		RunID: claims.RunID, WorkspaceID: claims.WorkspaceID,
	}
	if authority.Repository == "" || authority.Ref == "" || authority.DefaultRef == "" || authority.Compatibility == "" {
		return actionscache.RequestAuthority{}, false
	}
	return authority, true
}

func (server *Server) writeAuthorizationFailure(writer http.ResponseWriter, request *http.Request) {
	server.recordAuthorizationDenial(request)
	path := request.URL.Path
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		switch {
		case path == publicBuildStatusProbePath:
			// This reserved impossible ID is the CLI's non-mutating capability
			// probe. Unlike ordinary lookups, its response may disclose whether
			// the presented credential itself is valid without disclosing data.
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		case strings.HasPrefix(path, "/v8/artifacts/"):
			writer.WriteHeader(http.StatusNotFound)
			return
		case strings.HasSuffix(path, "/cache") &&
			(strings.HasPrefix(path, "/_apis/artifactcache/") || strings.HasPrefix(path, "/_layercache/compatibility/")):
			writer.WriteHeader(http.StatusNoContent)
			return
		case strings.Contains(path, "/_apis/artifactcache/caches/"):
			writer.WriteHeader(http.StatusNotFound)
			return
		case strings.HasPrefix(path, "/v1/public-builds/"):
			writer.WriteHeader(http.StatusNotFound)
			return
		}
	}
	writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}
