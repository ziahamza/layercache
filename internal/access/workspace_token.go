package access

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalidToken = errors.New("workspace token is invalid")
	ErrExpiredToken = errors.New("workspace token is expired")
)

type runIDContextKey struct{}

func MintWorkspaceToken(secret, runID string, now time.Time, ttl time.Duration) (string, error) {
	if secret == "" || runID == "" || len(runID) > 256 || strings.ContainsAny(runID, "\x00\r\n") {
		return "", ErrInvalidToken
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return "", errors.New("workspace token lifetime must be between zero and 24 hours")
	}
	encodedRun := base64.RawURLEncoding.EncodeToString([]byte(runID))
	expires := strconv.FormatInt(now.Add(ttl).Unix(), 10)
	payload := "lc1." + encodedRun + "." + expires
	signature := signToken(secret, payload)
	return payload + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func ParseWorkspaceToken(secret, token string, now time.Time) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != "lc1" || secret == "" {
		return "", ErrInvalidToken
	}
	payload := strings.Join(parts[:3], ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || !hmac.Equal(signature, signToken(secret, payload)) {
		return "", ErrInvalidToken
	}
	runBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(runBytes) == 0 || len(runBytes) > 256 || strings.ContainsAny(string(runBytes), "\x00\r\n") {
		return "", ErrInvalidToken
	}
	expires, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", ErrInvalidToken
	}
	if now.Unix() > expires {
		return "", ErrExpiredToken
	}
	return string(runBytes), nil
}

func WithRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, runIDContextKey{}, runID)
}

func RunID(ctx context.Context) string {
	runID, _ := ctx.Value(runIDContextKey{}).(string)
	return runID
}

func signToken(secret, payload string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil)
}
