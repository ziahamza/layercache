//go:build !linux && !darwin

package cli

import "errors"

func readSecretFile(string) (string, error) {
	return "", errors.New("protected secret files are unavailable on this platform")
}
