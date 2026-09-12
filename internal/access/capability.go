package access

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

type Capability string

const (
	CapabilityRead  Capability = "read"
	CapabilityWrite Capability = "write"
	CapabilityAdmin Capability = "admin"
)

type Claims struct {
	Subject       string       `json:"sub"`
	RunID         string       `json:"runId,omitempty"`
	WorkspaceID   string       `json:"workspaceId,omitempty"`
	Project       string       `json:"project"`
	Integration   string       `json:"integration,omitempty"`
	Compatibility string       `json:"compatibility,omitempty"`
	Repository    string       `json:"repository,omitempty"`
	Ref           string       `json:"ref,omitempty"`
	DefaultRef    string       `json:"defaultRef,omitempty"`
	SourceCommit  string       `json:"sourceCommit,omitempty"`
	RecipeDigest  string       `json:"recipeDigest,omitempty"`
	Target        string       `json:"target,omitempty"`
	Platform      string       `json:"platform,omitempty"`
	Toolchain     string       `json:"toolchain,omitempty"`
	Builder       string       `json:"builder,omitempty"`
	Capabilities  []Capability `json:"capabilities"`
	ExpiresAt     time.Time    `json:"expiresAt"`
}

func (claims Claims) Allows(required Capability) bool {
	for _, capability := range claims.Capabilities {
		if capability == CapabilityAdmin || capability == required || capability == CapabilityWrite && required == CapabilityRead {
			return true
		}
	}
	return false
}

func MintCapabilityToken(secret string, claims Claims, now time.Time) (string, error) {
	claims, err := normalizeClaims(claims, now)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signature := capabilitySignature(secret, encoded)
	if len(signature) == 0 {
		return "", ErrInvalidToken
	}
	return "lc2." + encoded + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func ParseCapabilityToken(secret, token string, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "lc2" || secret == "" {
		return Claims{}, ErrInvalidToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(signature, capabilitySignature(secret, parts[1])) {
		return Claims{}, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) == 0 || len(payload) > 16<<10 {
		return Claims{}, ErrInvalidToken
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var claims Claims
	if err := decoder.Decode(&claims); err != nil {
		return Claims{}, ErrInvalidToken
	}
	if !claims.ExpiresAt.After(now) {
		return Claims{}, ErrExpiredToken
	}
	claims, err = normalizeClaims(claims, now)
	if err != nil {
		return Claims{}, err
	}
	return claims, nil
}

func normalizeClaims(claims Claims, now time.Time) (Claims, error) {
	claims.Subject = strings.TrimSpace(claims.Subject)
	claims.Project = strings.TrimSpace(claims.Project)
	claims.Integration = strings.TrimSpace(claims.Integration)
	claims.Compatibility = strings.TrimSpace(claims.Compatibility)
	claims.Repository = strings.TrimSpace(claims.Repository)
	claims.Ref = strings.TrimSpace(claims.Ref)
	claims.DefaultRef = strings.TrimSpace(claims.DefaultRef)
	claims.SourceCommit = strings.TrimSpace(claims.SourceCommit)
	claims.RecipeDigest = strings.TrimSpace(claims.RecipeDigest)
	claims.Target = strings.TrimSpace(claims.Target)
	claims.Platform = strings.TrimSpace(claims.Platform)
	claims.Toolchain = strings.TrimSpace(claims.Toolchain)
	claims.Builder = strings.TrimSpace(claims.Builder)
	claims.RunID = strings.TrimSpace(claims.RunID)
	claims.WorkspaceID = strings.TrimSpace(claims.WorkspaceID)
	if claims.Subject == "" || claims.Project == "" || claims.ExpiresAt.IsZero() || !claims.ExpiresAt.After(now) {
		return Claims{}, ErrInvalidToken
	}
	if claims.Integration != "" && claims.Integration != "turbo" && claims.Integration != "actions" && claims.Integration != "buildkit" {
		return Claims{}, ErrInvalidToken
	}
	for _, identity := range []string{claims.RunID, claims.WorkspaceID} {
		if len(identity) > 512 || strings.ContainsAny(identity, "\x00\r\n") {
			return Claims{}, ErrInvalidToken
		}
	}
	if len(claims.Target) > 512 || strings.ContainsAny(claims.Target, "\x00\r\n") {
		return Claims{}, ErrInvalidToken
	}
	if claims.ExpiresAt.Sub(now) > 24*time.Hour {
		return Claims{}, errors.New("capability token lifetime cannot exceed 24 hours")
	}
	if len(claims.Capabilities) == 0 || len(claims.Capabilities) > 3 {
		return Claims{}, ErrInvalidToken
	}
	seen := make(map[Capability]struct{}, len(claims.Capabilities))
	for _, capability := range claims.Capabilities {
		if capability != CapabilityRead && capability != CapabilityWrite && capability != CapabilityAdmin {
			return Claims{}, ErrInvalidToken
		}
		seen[capability] = struct{}{}
	}
	claims.Capabilities = claims.Capabilities[:0]
	for capability := range seen {
		claims.Capabilities = append(claims.Capabilities, capability)
	}
	sort.Slice(claims.Capabilities, func(i, j int) bool { return claims.Capabilities[i] < claims.Capabilities[j] })
	return claims, nil
}

func capabilitySignature(secret, payload string) []byte {
	if secret == "" {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("layercache-capability-v2\x00"))
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil)
}
