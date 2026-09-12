//go:build !unix

package buildkit

import (
	"context"
	"errors"
)

func acquireFilePromotionLock(context.Context, string, string) (func(), error) {
	return nil, errors.New("cross-process Team Cache promotion locks require a Unix host")
}
