package githubauth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DefaultOIDCIssuer = "https://token.actions.githubusercontent.com"

type ActionsIdentity struct {
	Subject         string
	Repository      string
	Ref             string
	Commit          string
	WorkflowRef     string
	JobWorkflowRef  string
	RepositoryOwner string
	RunID           string
	RunAttempt      string
	CheckRunID      string
}

type OIDCVerifier struct {
	Issuer string

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	keysUntil time.Time
}

func (verifier *OIDCVerifier) Verify(ctx context.Context, rawToken, audience string, now time.Time) (ActionsIdentity, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 || audience == "" {
		return ActionsIdentity{}, errors.New("invalid GitHub Actions OIDC token")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(headerBytes) > 8<<10 {
		return ActionsIdentity{}, errors.New("invalid GitHub Actions OIDC header")
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if json.Unmarshal(headerBytes, &header) != nil || header.Algorithm != "RS256" || header.KeyID == "" {
		return ActionsIdentity{}, errors.New("unsupported GitHub Actions OIDC signature")
	}
	key, err := verifier.key(ctx, header.KeyID, now)
	if err != nil {
		return ActionsIdentity{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ActionsIdentity{}, errors.New("invalid GitHub Actions OIDC signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return ActionsIdentity{}, errors.New("invalid GitHub Actions OIDC signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > 64<<10 {
		return ActionsIdentity{}, errors.New("invalid GitHub Actions OIDC claims")
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	var claims map[string]any
	if decoder.Decode(&claims) != nil {
		return ActionsIdentity{}, errors.New("invalid GitHub Actions OIDC claims")
	}
	issuer := verifier.Issuer
	if issuer == "" {
		issuer = DefaultOIDCIssuer
	}
	if stringClaim(claims, "iss") != issuer || !audienceClaim(claims["aud"], audience) {
		return ActionsIdentity{}, errors.New("GitHub Actions OIDC issuer or audience does not match")
	}
	expiresAt, ok := numericDate(claims["exp"])
	if !ok || !now.Before(expiresAt) {
		return ActionsIdentity{}, errors.New("GitHub Actions OIDC token expired")
	}
	if issuedAt, ok := numericDate(claims["iat"]); !ok || issuedAt.After(now.Add(time.Minute)) || now.Sub(issuedAt) > 15*time.Minute {
		return ActionsIdentity{}, errors.New("GitHub Actions OIDC token has an invalid issue time")
	}
	if notBefore, ok := numericDate(claims["nbf"]); ok && now.Add(time.Minute).Before(notBefore) {
		return ActionsIdentity{}, errors.New("GitHub Actions OIDC token is not valid yet")
	}
	identity := ActionsIdentity{
		Subject: stringClaim(claims, "sub"), Repository: stringClaim(claims, "repository"),
		Ref: stringClaim(claims, "ref"), Commit: stringClaim(claims, "sha"),
		WorkflowRef: stringClaim(claims, "workflow_ref"), JobWorkflowRef: stringClaim(claims, "job_workflow_ref"),
		RepositoryOwner: stringClaim(claims, "repository_owner"),
		RunID:           unsignedIntegerClaim(claims, "run_id"),
		RunAttempt:      unsignedIntegerClaim(claims, "run_attempt"),
		CheckRunID:      unsignedIntegerClaim(claims, "check_run_id"),
	}
	if identity.Subject == "" || identity.Repository == "" || identity.Ref == "" || identity.Commit == "" ||
		identity.RunID == "" || identity.RunAttempt == "" || identity.CheckRunID == "" {
		return ActionsIdentity{}, errors.New("GitHub Actions OIDC token lacks required claims")
	}
	return identity, nil
}

func (verifier *OIDCVerifier) key(ctx context.Context, keyID string, now time.Time) (*rsa.PublicKey, error) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if now.Before(verifier.keysUntil) {
		if key := verifier.keys[keyID]; key != nil {
			return key, nil
		}
	}
	keys, err := verifier.fetchKeys(ctx)
	if err != nil {
		return nil, err
	}
	verifier.keys = keys
	verifier.keysUntil = now.Add(5 * time.Minute)
	if key := keys[keyID]; key != nil {
		return key, nil
	}
	return nil, errors.New("GitHub Actions OIDC signing key is unknown")
}

func (verifier *OIDCVerifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	issuer := verifier.Issuer
	if issuer == "" {
		issuer = DefaultOIDCIssuer
	}
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname()))) {
		return nil, errors.New("GitHub Actions OIDC issuer must use HTTPS except on loopback")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("GitHub Actions OIDC issuer cannot contain credentials, query, or fragment")
	}
	origin := parsed.Scheme + "://" + parsed.Host
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(issuer, "/")+"/.well-known/jwks", nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(next *http.Request, redirects []*http.Request) error {
			if len(redirects) >= 3 || next.URL.Scheme+"://"+next.URL.Host != origin {
				return errors.New("refusing OIDC key redirect outside its origin")
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch GitHub Actions OIDC keys: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return nil, fmt.Errorf("fetch GitHub Actions OIDC keys: HTTP %d", response.StatusCode)
	}
	var set struct {
		Keys []struct {
			KeyID     string `json:"kid"`
			KeyType   string `json:"kty"`
			Algorithm string `json:"alg"`
			Use       string `json:"use"`
			Modulus   string `json:"n"`
			Exponent  string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode GitHub Actions OIDC keys: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, jwk := range set.Keys {
		if jwk.KeyID == "" || jwk.KeyType != "RSA" || jwk.Algorithm != "RS256" || jwk.Use != "sig" {
			continue
		}
		modulus, modulusErr := base64.RawURLEncoding.DecodeString(jwk.Modulus)
		exponentBytes, exponentErr := base64.RawURLEncoding.DecodeString(jwk.Exponent)
		if modulusErr != nil || exponentErr != nil || len(modulus) < 256 || len(exponentBytes) == 0 || len(exponentBytes) > 4 {
			continue
		}
		exponent := 0
		for _, value := range exponentBytes {
			exponent = exponent<<8 | int(value)
		}
		if exponent < 3 || exponent%2 == 0 {
			continue
		}
		keys[jwk.KeyID] = &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: exponent}
	}
	if len(keys) == 0 {
		return nil, errors.New("GitHub Actions OIDC key set contained no supported keys")
	}
	return keys, nil
}

func stringClaim(claims map[string]any, name string) string {
	value, _ := claims[name].(string)
	return value
}

func unsignedIntegerClaim(claims map[string]any, name string) string {
	value := stringClaim(claims, name)
	if value == "" || len(value) > 20 || value[0] == '0' && len(value) > 1 {
		return ""
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed == 0 {
		return ""
	}
	return value
}

func numericDate(value any) (time.Time, bool) {
	var seconds int64
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(typed.String(), 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		seconds = parsed
	case float64:
		seconds = int64(typed)
	default:
		return time.Time{}, false
	}
	if seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).UTC(), true
}

func audienceClaim(value any, expected string) bool {
	switch typed := value.(type) {
	case string:
		return typed == expected
	case []any:
		for _, item := range typed {
			if item == expected {
				return true
			}
		}
	}
	return false
}
