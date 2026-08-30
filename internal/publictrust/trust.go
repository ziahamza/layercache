package publictrust

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	payloadType   = "application/vnd.in-toto+json"
	statementType = "https://in-toto.io/Statement/v1"
	predicateType = "https://layercache.dev/attestation/public-cache/v1"
)

var (
	ErrSignature = errors.New("public publication signature is invalid")
	ErrIdentity  = errors.New("public publication identity does not match")
	ErrExpired   = errors.New("public publication verification lease expired")
	ErrAmbiguous = errors.New("public publication is ambiguous")
	ErrRevoked   = errors.New("public publication is revoked")
	ErrNotFound  = errors.New("public publication not found")
)

type Publication struct {
	Integration   string    `json:"integration"`
	Project       string    `json:"project"`
	Compatibility string    `json:"compatibility"`
	NativeKey     string    `json:"nativeKey"`
	Repository    string    `json:"repository"`
	Commit        string    `json:"commit"`
	RecipeDigest  string    `json:"recipeDigest"`
	Platform      string    `json:"platform"`
	Toolchain     string    `json:"toolchain"`
	Builder       string    `json:"builder"`
	Digest        string    `json:"digest"`
	Size          int64     `json:"size"`
	DurationMS    int64     `json:"durationMs"`
	BuildID       string    `json:"buildId"`
	IssuedAt      time.Time `json:"issuedAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

type Expected struct {
	Integration   string
	Project       string
	Compatibility string
	NativeKey     string
	Digest        string
}

type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

type Signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

type statement struct {
	Type          string      `json:"_type"`
	Subject       []subject   `json:"subject"`
	PredicateType string      `json:"predicateType"`
	Predicate     Publication `json:"predicate"`
}

type subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

func (publication Publication) Identity() string {
	canonical := strings.Join([]string{
		publication.Integration,
		publication.Project,
		publication.Compatibility,
		publication.NativeKey,
	}, "\x00")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

func Sign(privateKey ed25519.PrivateKey, publication Publication) (Envelope, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return Envelope{}, errors.New("invalid Ed25519 private key")
	}
	if err := validatePublication(publication); err != nil {
		return Envelope{}, err
	}
	statement := statement{
		Type: statementType,
		Subject: []subject{{
			Name: publication.Integration + ":" + publication.NativeKey,
			Digest: map[string]string{
				"sha256": publication.Digest,
			},
		}},
		PredicateType: predicateType,
		Predicate:     publication,
	}
	payload, err := json.Marshal(statement)
	if err != nil {
		return Envelope{}, fmt.Errorf("encode in-toto statement: %w", err)
	}
	signature := ed25519.Sign(privateKey, pae(payloadType, payload))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	keyHash := sha256.Sum256(publicKey)
	return Envelope{
		PayloadType: payloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
		Signatures: []Signature{{
			KeyID: "sha256:" + hex.EncodeToString(keyHash[:]),
			Sig:   base64.StdEncoding.EncodeToString(signature),
		}},
	}, nil
}

func Verify(publicKey ed25519.PublicKey, envelope Envelope, expected Expected, now time.Time) (Publication, error) {
	if len(publicKey) != ed25519.PublicKeySize || envelope.PayloadType != payloadType || len(envelope.Signatures) == 0 {
		return Publication{}, ErrSignature
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return Publication{}, ErrSignature
	}
	valid := false
	for _, candidate := range envelope.Signatures {
		signature, err := base64.StdEncoding.DecodeString(candidate.Sig)
		if err == nil && ed25519.Verify(publicKey, pae(envelope.PayloadType, payload), signature) {
			valid = true
			break
		}
	}
	if !valid {
		return Publication{}, ErrSignature
	}
	var signed statement
	if err := json.Unmarshal(payload, &signed); err != nil {
		return Publication{}, ErrSignature
	}
	if signed.Type != statementType || signed.PredicateType != predicateType || len(signed.Subject) != 1 {
		return Publication{}, ErrSignature
	}
	publication := signed.Predicate
	if err := validatePublication(publication); err != nil {
		return Publication{}, ErrSignature
	}
	if signed.Subject[0].Digest["sha256"] != publication.Digest {
		return Publication{}, ErrSignature
	}
	if now.Before(publication.IssuedAt) || now.After(publication.ExpiresAt) {
		return Publication{}, ErrExpired
	}
	if publication.Integration != expected.Integration ||
		publication.Project != expected.Project ||
		publication.Compatibility != expected.Compatibility ||
		publication.NativeKey != expected.NativeKey {
		return Publication{}, ErrIdentity
	}
	if expected.Digest != "" && publication.Digest != strings.TrimPrefix(expected.Digest, "sha256:") {
		return Publication{}, ErrIdentity
	}
	return publication, nil
}

func EncodePublicKey(key ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(key)
}

func DecodePublicKey(encoded string) (ed25519.PublicKey, error) {
	bytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(bytes) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key")
	}
	return ed25519.PublicKey(bytes), nil
}

func EncodePrivateKey(key ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(key)
}

func DecodePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	bytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(bytes) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key")
	}
	return ed25519.PrivateKey(bytes), nil
}

func validatePublication(publication Publication) error {
	if publication.Integration == "" || publication.Project == "" || publication.Compatibility == "" || publication.NativeKey == "" {
		return errors.New("publication cache identity is incomplete")
	}
	if publication.Repository == "" || publication.Commit == "" || publication.RecipeDigest == "" || publication.Platform == "" {
		return errors.New("publication source identity is incomplete")
	}
	if publication.Toolchain == "" || publication.Builder == "" || publication.BuildID == "" {
		return errors.New("publication builder identity is incomplete")
	}
	if len(publication.Digest) != sha256.Size*2 {
		return errors.New("publication digest must be a SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(publication.Digest); err != nil {
		return errors.New("publication digest must be a SHA-256 hex digest")
	}
	if publication.Size < 0 || publication.DurationMS < 0 {
		return errors.New("publication size and duration cannot be negative")
	}
	if publication.IssuedAt.IsZero() || !publication.ExpiresAt.After(publication.IssuedAt) {
		return errors.New("publication validity is invalid")
	}
	return nil
}

func pae(payloadType string, payload []byte) []byte {
	return []byte("DSSEv1 " + strconv.Itoa(len(payloadType)) + " " + payloadType + " " + strconv.Itoa(len(payload)) + " " + string(payload))
}
