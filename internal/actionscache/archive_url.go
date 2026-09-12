package actionscache

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	archiveExpiryQuery    = "expires"
	archiveSignatureQuery = "signature"
	archiveAuthorityQuery = "authority"
	archiveOutcomeQuery   = "outcome"
)

// ArchiveURLSigner signs the canonical path-and-authority resource and expiry
// of a v1 archive URL. The stock actions/cache v1 client downloads
// archiveLocation without its API bearer token, so archive authority must
// travel in the URL and expire quickly.
type ArchiveURLSigner interface {
	SignArchiveURL(resource string, expiresAt time.Time) (string, error)
	VerifyArchiveURL(resource string, expiresAt time.Time, signature string) error
}

type hmacArchiveURLSigner struct {
	secret []byte
}

func NewHMACArchiveURLSigner(token string) (ArchiveURLSigner, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("archive URL signing token is required")
	}
	return newHMACArchiveURLSigner([]byte(token)), nil
}

func newHMACArchiveURLSigner(secret []byte) ArchiveURLSigner {
	return &hmacArchiveURLSigner{secret: append([]byte(nil), secret...)}
}

func (signer *hmacArchiveURLSigner) SignArchiveURL(resource string, expiresAt time.Time) (string, error) {
	mac := hmac.New(sha256.New, signer.secret)
	_, _ = mac.Write(archiveSignaturePayload(resource, expiresAt))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (signer *hmacArchiveURLSigner) VerifyArchiveURL(resource string, expiresAt time.Time, signature string) error {
	provided, err := hex.DecodeString(signature)
	if err != nil {
		return errors.New("archive URL signature is invalid")
	}
	mac := hmac.New(sha256.New, signer.secret)
	_, _ = mac.Write(archiveSignaturePayload(resource, expiresAt))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return errors.New("archive URL signature is invalid")
	}
	return nil
}

func archiveSignaturePayload(resource string, expiresAt time.Time) []byte {
	return []byte("GET\n" + resource + "\n" + strconv.FormatInt(expiresAt.UnixMilli(), 10))
}
