package server

import (
	"context"
	"strings"

	"github.com/layercache/layercache/internal/access"
)

// A signed token cannot retain privileges removed from a GitHub member after
// issuance. Database failures deny access; token capabilities remain an upper bound.
func (server *Server) currentMembershipAllows(ctx context.Context, claims access.Claims, required access.Capability) bool {
	if server.projectAuthority != nil {
		if strings.HasPrefix(claims.Subject, "runtime-admin:") || strings.HasPrefix(claims.Subject, "github-actions:") {
			return true
		}
		role, err := server.projectAuthority.MemberRole(ctx, server.config.ProjectID, claims.Subject)
		capabilities, ok := capabilitiesForRole(role)
		return err == nil && ok && (access.Claims{Capabilities: capabilities}).Allows(required)
	}
	if server.config.Role == "local" || !strings.HasPrefix(claims.Subject, "github:") {
		return true
	}
	var role string
	if server.cloudStore != nil {
		var err error
		role, err = server.cloudStore.MemberRole(ctx, server.config.ProjectID, claims.Subject)
		if err != nil {
			return false
		}
	} else {
		role = server.config.TeamMembers[strings.TrimPrefix(claims.Subject, "github:")]
	}
	capabilities, ok := capabilitiesForRole(role)
	return ok && (access.Claims{Capabilities: capabilities}).Allows(required)
}
