package buildkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

var promotionProcessLocks = struct {
	sync.Mutex
	byReference map[string]chan struct{}
}{byReference: make(map[string]chan struct{})}

type PromotionCoordinator interface {
	Acquire(context.Context, string) (PromotionLease, error)
}

type PromotionLease interface {
	Release(context.Context) error
}

// maintainedPromotionLease is implemented by remote leases whose authority
// expires while a registry mutation may still be running. Local test adapters
// and process-only locks do not need to implement it.
type maintainedPromotionLease interface {
	PromotionLease
	Maintain(context.Context) error
}

func (a *Adapter) promote(ctx context.Context, command Command, reference string, stderr io.Writer) error {
	if reference == "" {
		return errors.New("Team Cache promotion reference is missing")
	}
	if a.config.PromotionLockDir == "" {
		return errors.New("Team Cache promotion lock directory is not configured")
	}
	releaseProcess, err := acquireProcessPromotionLock(ctx, reference)
	if err != nil {
		return err
	}
	defer releaseProcess()
	releaseFile, err := acquireFilePromotionLock(ctx, a.config.PromotionLockDir, reference)
	if err != nil {
		return err
	}
	defer releaseFile()
	var lease PromotionLease
	if a.config.PromotionCoordinator != nil {
		lease, err = a.config.PromotionCoordinator.Acquire(ctx, reference)
		if err != nil {
			return fmt.Errorf("acquire Team Cache promotion lease: %w", err)
		}
	}
	promotionContext := ctx
	var stopMaintenance context.CancelCauseFunc
	var maintenanceDone chan error
	if maintained, ok := lease.(maintainedPromotionLease); ok {
		promotionContext, stopMaintenance = context.WithCancelCause(ctx)
		maintenanceDone = make(chan error, 1)
		go func() {
			err := maintained.Maintain(promotionContext)
			if err != nil && promotionContext.Err() == nil {
				stopMaintenance(err)
			}
			maintenanceDone <- err
		}()
	}
	promoteErr := a.runner.Run(promotionContext, command, stderr, stderr)
	var maintenanceErr error
	if stopMaintenance != nil {
		stopMaintenance(context.Canceled)
		maintenanceErr = <-maintenanceDone
		if errors.Is(maintenanceErr, context.Canceled) || errors.Is(maintenanceErr, context.DeadlineExceeded) && ctx.Err() != nil {
			maintenanceErr = nil
		}
		if maintenanceErr != nil {
			maintenanceErr = fmt.Errorf("maintain Team Cache promotion lease: %w", maintenanceErr)
		}
	}
	if lease == nil {
		return errors.Join(promoteErr, maintenanceErr)
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	releaseErr := lease.Release(releaseCtx)
	if releaseErr != nil {
		releaseErr = fmt.Errorf("release Team Cache promotion lease: %w", releaseErr)
	}
	return errors.Join(promoteErr, maintenanceErr, releaseErr)
}

func acquireProcessPromotionLock(ctx context.Context, reference string) (func(), error) {
	promotionProcessLocks.Lock()
	lock, found := promotionProcessLocks.byReference[reference]
	if !found {
		lock = make(chan struct{}, 1)
		promotionProcessLocks.byReference[reference] = lock
	}
	promotionProcessLocks.Unlock()
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
