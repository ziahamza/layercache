package publictrust

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"
)

const rotationPayloadType = "application/vnd.layercache.trust-rotation+json"

// Rotation authorizes one replacement of the pinned key for a specific
// Public Cache endpoint. It is signed by the currently trusted private key.
type Rotation struct {
	Version     int       `json:"version"`
	Endpoint    string    `json:"endpoint"`
	Sequence    uint64    `json:"sequence"`
	PreviousKey string    `json:"previousKey"`
	NextKey     string    `json:"nextKey"`
	IssuedAt    time.Time `json:"issuedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

func SignRotation(private ed25519.PrivateKey, rotation Rotation) (Envelope, error) {
	if len(private) != ed25519.PrivateKeySize {
		return Envelope{}, ErrSignature
	}
	key := private.Public().(ed25519.PublicKey)
	if err := validateRotation(rotation, key); err != nil {
		return Envelope{}, err
	}
	payload, err := json.Marshal(rotation)
	if err != nil {
		return Envelope{}, err
	}
	digest := sha256.Sum256(key)
	return Envelope{PayloadType: rotationPayloadType, Payload: base64.StdEncoding.EncodeToString(payload),
		Signatures: []Signature{{KeyID: "sha256:" + hex.EncodeToString(digest[:]), Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(private, pae(rotationPayloadType, payload)))}},
	}, nil
}

func VerifyRotation(key ed25519.PublicKey, envelope Envelope, endpoint string, sequence uint64, now time.Time) (Rotation, error) {
	if len(key) != ed25519.PublicKeySize || envelope.PayloadType != rotationPayloadType || len(envelope.Payload) > 64<<10 || len(envelope.Signatures) != 1 {
		return Rotation{}, ErrSignature
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return Rotation{}, ErrSignature
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signatures[0].Sig)
	if err != nil || !ed25519.Verify(key, pae(rotationPayloadType, payload), signature) {
		return Rotation{}, ErrSignature
	}
	var rotation Rotation
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rotation); err != nil {
		return Rotation{}, ErrSignature
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return Rotation{}, ErrSignature
	}
	if err := validateRotation(rotation, key); err != nil {
		return Rotation{}, err
	}
	if strings.TrimRight(endpoint, "/") != strings.TrimRight(rotation.Endpoint, "/") || rotation.Sequence <= sequence {
		return Rotation{}, ErrIdentity
	}
	if now.Before(rotation.IssuedAt) || !now.Before(rotation.ExpiresAt) {
		return Rotation{}, ErrExpired
	}
	return rotation, nil
}

func validateRotation(rotation Rotation, key ed25519.PublicKey) error {
	u, err := url.Parse(rotation.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(rotation.Endpoint) != rotation.Endpoint {
		return errors.New("trust rotation requires an exact HTTPS service endpoint")
	}
	if rotation.Version != 1 || rotation.Sequence == 0 || rotation.IssuedAt.IsZero() || !rotation.ExpiresAt.After(rotation.IssuedAt) || rotation.ExpiresAt.Sub(rotation.IssuedAt) > 30*24*time.Hour {
		return errors.New("invalid trust rotation version, sequence, or validity window")
	}
	previous, err := DecodePublicKey(rotation.PreviousKey)
	if err != nil || !bytes.Equal(previous, key) {
		return ErrIdentity
	}
	next, err := DecodePublicKey(rotation.NextKey)
	if err != nil || bytes.Equal(previous, next) {
		return errors.New("trust rotation must name a different valid Ed25519 key")
	}
	return nil
}
