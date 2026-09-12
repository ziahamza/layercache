package buildkit

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	teamExportEntropyBytes = 16
	teamExportSeparator    = "-u"
	maxTeamExportLabel     = 128 - len(teamExportSeparator) - teamExportEntropyBytes*2
)

// NewTeamExportID adds 128 bits of randomness to a human-readable build label.
// The resulting OCI tag is safe to publish once as an immutable Team Cache ref.
func NewTeamExportID(label string) (string, error) {
	if label == "" {
		return "", errors.New("Team Cache export label is required")
	}
	if len(label) > maxTeamExportLabel || !ociTagPattern.MatchString(label) {
		return "", fmt.Errorf("Team Cache export label %q must be a safe OCI tag of at most %d characters", label, maxTeamExportLabel)
	}
	entropy := make([]byte, teamExportEntropyBytes)
	if _, err := rand.Read(entropy); err != nil {
		return "", fmt.Errorf("create unique Team Cache export identity: %w", err)
	}
	return label + teamExportSeparator + hex.EncodeToString(entropy), nil
}

func validateTeamExportID(identity string) error {
	separator := strings.LastIndex(identity, teamExportSeparator)
	if separator <= 0 || len(identity)-separator-len(teamExportSeparator) != teamExportEntropyBytes*2 {
		return errors.New("Team Cache export ID must be created with buildkit.NewTeamExportID")
	}
	label := identity[:separator]
	entropy := identity[separator+len(teamExportSeparator):]
	if len(label) > maxTeamExportLabel || !ociTagPattern.MatchString(label) {
		return errors.New("Team Cache export ID must contain a safe OCI tag label")
	}
	decoded, err := hex.DecodeString(entropy)
	if err != nil || len(decoded) != teamExportEntropyBytes || entropy != strings.ToLower(entropy) {
		return errors.New("Team Cache export ID has invalid uniqueness entropy")
	}
	return nil
}
