package artifact

import (
	"errors"
	"math"
)

var ErrStagingQuota = stagingQuotaError{}

type stagingQuotaError struct{}

func (stagingQuotaError) Error() string { return "Local Cache transient staging limit exceeded" }
func (stagingQuotaError) Unwrap() error { return ErrQuota }

// StagingLease accounts for temporary bytes until their files are committed
// or removed. Release is safe to call more than once.
type StagingLease struct {
	store    *Store
	bytes    int64
	limit    int64
	upload   bool
	released bool
}

// ReserveUploadStaging admits retained integration upload bytes. Across all
// such leases, at most MaxBytes may be retained while a runtime is active.
func (store *Store) ReserveUploadStaging(bytes int64) (*StagingLease, error) {
	return store.reserveStaging(bytes, true)
}

// ReserveVerificationStaging admits a known-size temporary archive that will
// be verified before it can enter the Local Cache. Verification staging shares
// the total transient limit but does not consume the retained-upload limit.
func (store *Store) ReserveVerificationStaging(bytes int64) (*StagingLease, error) {
	return store.reserveStaging(bytes, false)
}

func (store *Store) reserveStaging(bytes int64, upload bool) (*StagingLease, error) {
	if bytes < 0 || bytes > store.maxBytes {
		return nil, ErrStagingQuota
	}
	lease := &StagingLease{store: store, limit: store.maxBytes, upload: upload}
	if err := lease.grow(bytes); err != nil {
		return nil, err
	}
	return lease, nil
}

func (store *Store) beginArtifactStaging() *StagingLease {
	return &StagingLease{store: store, limit: store.maxBytes}
}

func (lease *StagingLease) grow(bytes int64) error {
	if bytes < 0 {
		return ErrStagingQuota
	}
	store := lease.store
	store.stagingMu.Lock()
	defer store.stagingMu.Unlock()
	if lease.released {
		return errors.New("Local Cache staging lease is already released")
	}
	if exceedsLimit(lease.bytes, bytes, lease.limit) ||
		exceedsLimit(store.stagingBytes, bytes, store.transientStagingLimit()) ||
		lease.upload && exceedsLimit(store.uploadBytes, bytes, store.maxBytes) {
		return ErrStagingQuota
	}
	lease.bytes += bytes
	store.stagingBytes += bytes
	if lease.upload {
		store.uploadBytes += bytes
	}
	return nil
}

func (lease *StagingLease) shrink(bytes int64) {
	if bytes <= 0 {
		return
	}
	store := lease.store
	store.stagingMu.Lock()
	defer store.stagingMu.Unlock()
	if lease.released {
		return
	}
	if bytes > lease.bytes {
		bytes = lease.bytes
	}
	lease.bytes -= bytes
	store.stagingBytes -= bytes
	if lease.upload {
		store.uploadBytes -= bytes
	}
}

func (lease *StagingLease) Release() {
	if lease == nil || lease.store == nil {
		return
	}
	store := lease.store
	store.stagingMu.Lock()
	defer store.stagingMu.Unlock()
	if lease.released {
		return
	}
	store.stagingBytes -= lease.bytes
	if lease.upload {
		store.uploadBytes -= lease.bytes
	}
	lease.bytes = 0
	lease.released = true
}

// MaxBytes is the configured committed Local Cache byte limit. Retained
// integration uploads are also bounded to this value.
func (store *Store) MaxBytes() int64 { return store.maxBytes }

func (store *Store) transientStagingLimit() int64 {
	if store.maxBytes > math.MaxInt64/2 {
		return math.MaxInt64
	}
	return store.maxBytes * 2
}

func exceedsLimit(current, added, limit int64) bool {
	return current < 0 || added < 0 || current > limit || added > limit-current
}
