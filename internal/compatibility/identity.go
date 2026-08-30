// Package compatibility validates the opaque identity that separates native
// build outputs. Layer Cache does not infer toolchains from this value; callers
// are responsible for declaring every output-affecting ABI fact they need.
package compatibility

import (
	"errors"
	"fmt"
)

const (
	// Header carries a compatibility selector on authenticated cache requests.
	Header = "X-LayerCache-Compatibility"

	// MaxLength keeps request metadata and cache index keys bounded.
	MaxLength = 256
)

// Validate accepts a lowercase, ASCII, delimiter-safe compatibility identity.
// The restricted alphabet gives every logical value one wire representation
// and prevents ambiguous multi-value HTTP header encodings.
func Validate(identity string) error {
	if identity == "" {
		return errors.New("compatibility identity is required")
	}
	if len(identity) > MaxLength {
		return fmt.Errorf("compatibility identity cannot exceed %d bytes", MaxLength)
	}
	for index, char := range []byte(identity) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			continue
		}
		if index > 0 && (char == '-' || char == '_' || char == '.' || char == ':' || char == '+' || char == '@') {
			continue
		}
		return errors.New("compatibility identity must use lowercase ASCII letters, digits, and canonical -_.:+@ delimiters")
	}
	return nil
}
