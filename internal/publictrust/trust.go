package publictrust

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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
	Integration        string          `json:"integration"`
	Project            string          `json:"project"`
	Compatibility      string          `json:"compatibility"`
	NativeKey          string          `json:"nativeKey"`
	Repository         string          `json:"repository"`
	Commit             string          `json:"commit"`
	RecipeDigest       string          `json:"recipeDigest"`
	Target             string          `json:"target"`
	Platform           string          `json:"platform"`
	Inputs             []DeclaredInput `json:"inputs,omitempty"`
	Toolchain          string          `json:"toolchain"`
	Builder            string          `json:"builder"`
	BuilderImageDigest string          `json:"builderImageDigest"`
	Digest             string          `json:"digest"`
	Size               int64           `json:"size"`
	DurationMS         int64           `json:"durationMs"`
	BuildID            string          `json:"buildId"`
	IssuedAt           time.Time       `json:"issuedAt"`
	ExpiresAt          time.Time       `json:"expiresAt"`
}

type DeclaredInput struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type CacheCoordinate struct {
	Integration   string `json:"integration"`
	Project       string `json:"project"`
	Compatibility string `json:"compatibility"`
	NativeKey     string `json:"nativeKey"`
}

func (coordinate CacheCoordinate) Identity() string {
	canonical := strings.Join([]string{
		coordinate.Integration,
		coordinate.Project,
		coordinate.Compatibility,
		coordinate.NativeKey,
	}, "\x00")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

func (publication Publication) Coordinate() CacheCoordinate {
	return CacheCoordinate{
		Integration:   publication.Integration,
		Project:       publication.Project,
		Compatibility: publication.Compatibility,
		NativeKey:     publication.NativeKey,
	}
}

type Expected struct {
	Integration        string
	Project            string
	Compatibility      string
	NativeKey          string
	Repository         string
	Commit             string
	RecipeDigest       string
	Target             string
	Platform           string
	Inputs             []DeclaredInput
	Toolchain          string
	Builder            string
	BuilderImageDigest string
	BuildID            string
	PublicIdentity     string
	Digest             string
	Size               *int64
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

// CacheIdentity identifies the lookup slot used by a native cache client. It
// deliberately excludes provenance. A lookup slot may become ambiguous when
// trusted builds produce different bytes for it.
func (publication Publication) CacheIdentity() string {
	return publication.Coordinate().Identity()
}

// Identity identifies one complete signed Public Cache publication. Unlike
// CacheIdentity, it binds source, recipe, builder, output bytes, and Public
// Build identity. Clients may compare this value before they mutate a
// Workspace.
func (publication Publication) Identity() string {
	parts := []string{
		publication.Integration,
		publication.Project,
		publication.Compatibility,
		publication.NativeKey,
		publication.Repository,
		publication.Commit,
		publication.RecipeDigest,
		publication.Target,
		publication.Platform,
	}
	for _, input := range publication.Inputs {
		parts = append(parts, input.Name, input.Value)
	}
	parts = append(parts,
		publication.Toolchain,
		publication.Builder,
		publication.BuilderImageDigest,
		publication.BuildID,
		publication.Digest,
		strconv.FormatInt(publication.Size, 10),
	)
	canonical := strings.Join(parts, "\x00")
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
			Name: "layercache-public:" + publication.Identity(),
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
	if signed.Subject[0].Name != "layercache-public:"+publication.Identity() ||
		signed.Subject[0].Digest["sha256"] != publication.Digest {
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
	if !matchesExpected(publication, expected) {
		return Publication{}, ErrIdentity
	}
	if expected.Digest != "" && publication.Digest != strings.TrimPrefix(expected.Digest, "sha256:") {
		return Publication{}, ErrIdentity
	}
	return publication, nil
}

func matchesExpected(publication Publication, expected Expected) bool {
	if expected.Repository != "" && publication.Repository != expected.Repository {
		return false
	}
	if expected.Commit != "" && publication.Commit != expected.Commit {
		return false
	}
	if expected.RecipeDigest != "" && publication.RecipeDigest != expected.RecipeDigest {
		return false
	}
	if expected.Target != "" && publication.Target != expected.Target {
		return false
	}
	if expected.Platform != "" && publication.Platform != expected.Platform {
		return false
	}
	if expected.Inputs != nil && !slices.Equal(publication.Inputs, expected.Inputs) {
		return false
	}
	if expected.Toolchain != "" && publication.Toolchain != expected.Toolchain {
		return false
	}
	if expected.Builder != "" && publication.Builder != expected.Builder {
		return false
	}
	if expected.BuilderImageDigest != "" && publication.BuilderImageDigest != expected.BuilderImageDigest {
		return false
	}
	if expected.BuildID != "" && publication.BuildID != expected.BuildID {
		return false
	}
	if expected.PublicIdentity != "" && publication.Identity() != expected.PublicIdentity {
		return false
	}
	return expected.Size == nil || publication.Size == *expected.Size
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
	if publication.Repository == "" || publication.Commit == "" || publication.RecipeDigest == "" || publication.Target == "" || publication.Platform == "" {
		return errors.New("publication source identity is incomplete")
	}
	if publication.Toolchain == "" || publication.Builder == "" || publication.BuildID == "" {
		return errors.New("publication builder identity is incomplete")
	}
	if !validSHA256Digest(publication.BuilderImageDigest) {
		return errors.New("publication builder image digest must be a lowercase sha256 digest")
	}
	if err := validateDeclaredInputs(publication.Inputs); err != nil {
		return err
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

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	encoded := strings.TrimPrefix(value, "sha256:")
	if encoded != strings.ToLower(encoded) {
		return false
	}
	_, err := hex.DecodeString(encoded)
	return err == nil
}

var declaredInputNamePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

func validateDeclaredInputs(inputs []DeclaredInput) error {
	if len(inputs) > 32 {
		return errors.New("publication has too many declared inputs")
	}
	if !slices.IsSortedFunc(inputs, func(left, right DeclaredInput) int {
		return strings.Compare(left.Name, right.Name)
	}) {
		return errors.New("publication declared inputs are not canonical")
	}
	totalBytes := 0
	for index, input := range inputs {
		if !declaredInputNamePattern.MatchString(input.Name) || index > 0 && inputs[index-1].Name == input.Name {
			return errors.New("publication declared input name is invalid or duplicated")
		}
		if !utf8.ValidString(input.Value) || len(input.Value) > 1024 || strings.IndexFunc(input.Value, func(character rune) bool {
			return character == 0 || character == '\r' || character == '\n' || character < 0x20 || character == 0x7f
		}) >= 0 {
			return errors.New("publication declared input value is invalid")
		}
		totalBytes += len(input.Name) + len(input.Value)
	}
	if totalBytes > 16<<10 {
		return errors.New("publication declared inputs are too large")
	}
	return nil
}

// ValidatePublication checks the complete trusted metadata before a registry
// adapter makes it visible. Filesystem/SQLite and PostgreSQL registries share
// this validation so storage choice cannot change the trust contract.
func ValidatePublication(publication Publication) error {
	return validatePublication(publication)
}

func pae(payloadType string, payload []byte) []byte {
	return []byte("DSSEv1 " + strconv.Itoa(len(payloadType)) + " " + payloadType + " " + strconv.Itoa(len(payload)) + " " + string(payload))
}
