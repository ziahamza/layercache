//go:build !linux && !darwin

package cli

import (
	"context"
	"errors"
)

func lockConfiguration(context.Context, string) (func(), error) {
	return nil, errors.New("safe configuration locking is unavailable on this platform")
}
