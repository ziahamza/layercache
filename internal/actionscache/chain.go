package actionscache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

const cacheChainTransferChunkSize = 4 << 20

// CacheChain composes Local, Team, and optionally verified Public Caches
// while preserving the v1 StorageIndex contract. Remote bytes become visible
// through this index only after a complete Local Cache commit.
type CacheChain struct {
	local  StorageIndex
	team   StorageIndex
	public StorageIndex

	mu           sync.Mutex
	reservations map[int64]ReserveRequest
	warming      map[cacheChainIdentity]*warmCall
}

type cacheChainIdentity struct {
	repository    string
	compatibility string
	ref           string
	key           string
	version       string
}

type warmCall struct {
	done  chan struct{}
	entry Entry
	err   error
}

type publicEntryRevalidator interface {
	RevalidateEntry(context.Context, *PublicEntryMetadata) (*PublicEntryMetadata, error)
}

type entryInvalidator interface {
	InvalidateEntry(context.Context, int64) error
}

type publicEntryMetadataUpdater interface {
	UpdatePublicEntryMetadata(context.Context, int64, *PublicEntryMetadata) error
}

type reservationAborter interface {
	Abort(context.Context, int64) error
}

type entryReleaser interface {
	ReleaseEntry(context.Context, int64) error
}

func NewCacheChain(local, team StorageIndex) (*CacheChain, error) {
	if local == nil {
		return nil, errors.New("Local Cache storage is required")
	}
	if team == nil {
		return nil, errors.New("Team Cache storage is required")
	}
	return &CacheChain{
		local: local, team: team, reservations: make(map[int64]ReserveRequest), warming: make(map[cacheChainIdentity]*warmCall),
	}, nil
}

// NewCacheChainWithPublic adds a read-only, verified Public Cache after the
// Local and Team Caches. Writes deliberately retain NewCacheChain semantics:
// commit locally and publish best-effort to Team Cache only.
func NewCacheChainWithPublic(local, team, public StorageIndex) (*CacheChain, error) {
	if local == nil {
		return nil, errors.New("Local Cache storage is required")
	}
	if public == nil {
		return nil, errors.New("Public Cache storage is required")
	}
	if _, ok := local.(entryInvalidator); !ok {
		return nil, errors.New("Local Cache storage must support Public Cache invalidation")
	}
	if _, ok := local.(publicEntryMetadataUpdater); !ok {
		return nil, errors.New("Local Cache storage must persist Public Cache trust metadata")
	}
	if _, ok := public.(publicEntryRevalidator); !ok {
		return nil, errors.New("Public Cache storage must support warmed-entry revalidation")
	}
	return &CacheChain{
		local: local, team: team, public: public,
		reservations: make(map[int64]ReserveRequest), warming: make(map[cacheChainIdentity]*warmCall),
	}, nil
}

func (storage *CacheChain) Lookup(ctx context.Context, request LookupRequest) (LookupResult, error) {
	localResult, localErr := storage.local.Lookup(ctx, request)
	localFound := localErr == nil
	if localErr != nil && !errors.Is(localErr, ErrNotFound) {
		return LookupResult{}, fmt.Errorf("lookup Local Cache: %w", localErr)
	}
	if localFound && localResult.Entry.Public != nil {
		refreshed, err := storage.revalidateWarmedPublic(ctx, localResult.Entry)
		if err != nil {
			if invalidateErr := storage.invalidateLocalEntry(ctx, localResult.Entry.ID); invalidateErr != nil {
				return LookupResult{}, fmt.Errorf("invalidate untrusted warmed Public Cache entry: %w", invalidateErr)
			}
			localFound = false
		} else {
			localResult.Entry.Public = refreshed
			localResult.Entry.Origin = SourcePublicCache
		}
	}
	if localFound {
		localResult.Source = SourceLocalCache
		if isBestPossibleLookup(localResult, request) {
			return localResult, nil
		}
	}

	var teamResult LookupResult
	teamErr := error(ErrNotFound)
	teamFound := false
	if storage.team != nil {
		teamResult, teamErr = storage.team.Lookup(ctx, request)
		teamFound = teamErr == nil
		if teamFound {
			teamResult.Source = SourceTeamCache
			if (!localFound || lookupIsBetter(teamResult, localResult, request)) && isBestPossibleLookup(teamResult, request) {
				warmedEntry, err := storage.warmLocalOnce(ctx, request, teamResult, storage.team, "Team Cache")
				if err != nil {
					if localFound {
						return localResult, nil
					}
					return LookupResult{}, remoteFailureMiss(ctx, "warm Local Cache from Team Cache", err)
				}
				teamResult.Entry = warmedEntry
				return teamResult, nil
			}
		} else if teamErr != nil && !errors.Is(teamErr, ErrNotFound) && storage.public == nil {
			if localFound {
				return localResult, nil
			}
			return LookupResult{}, remoteFailureMiss(ctx, "lookup Team Cache", teamErr)
		}
	}

	best := LookupResult{}
	bestStorage := StorageIndex(nil)
	bestName := ""
	found := false
	if localFound {
		best, bestStorage, bestName, found = localResult, storage.local, "Local Cache", true
	}
	if teamFound && (!found || lookupIsBetter(teamResult, best, request)) {
		best, bestStorage, bestName, found = teamResult, storage.team, "Team Cache", true
	}

	var publicErr error
	if storage.public != nil {
		publicResult, err := storage.public.Lookup(ctx, request)
		if err == nil {
			publicResult.Source = SourcePublicCache
			if !found || lookupIsBetter(publicResult, best, request) {
				best, bestStorage, bestName, found = publicResult, storage.public, "Public Cache", true
			}
		} else if !errors.Is(err, ErrNotFound) {
			publicErr = err
		}
	}
	if !found {
		if teamErr != nil && !errors.Is(teamErr, ErrNotFound) {
			return LookupResult{}, remoteFailureMiss(ctx, "lookup Team Cache", teamErr)
		}
		if publicErr != nil {
			return LookupResult{}, remoteFailureMiss(ctx, "lookup Public Cache", publicErr)
		}
		return LookupResult{}, ErrNotFound
	}
	if bestStorage == storage.local {
		return best, nil
	}
	warmedEntry, err := storage.warmLocalOnce(ctx, request, best, bestStorage, bestName)
	if err != nil {
		if localFound && best.Source != SourceLocalCache {
			return localResult, nil
		}
		return LookupResult{}, remoteFailureMiss(ctx, "warm Local Cache from "+bestName, err)
	}
	best.Entry = warmedEntry
	return best, nil
}

func remoteFailureMiss(ctx context.Context, operation string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return errors.Join(ErrNotFound, fmt.Errorf("%s: %w", operation, err))
}

func (storage *CacheChain) warmLocalOnce(
	ctx context.Context,
	request LookupRequest,
	remote LookupResult,
	remoteStorage StorageIndex,
	remoteName string,
) (Entry, error) {
	identity := cacheChainIdentity{
		repository: request.Scope.Repository, compatibility: request.Scope.Compatibility,
		ref: remote.Entry.Ref, key: remote.Entry.Key, version: remote.Entry.Version,
	}
	storage.mu.Lock()
	if running := storage.warming[identity]; running != nil {
		storage.mu.Unlock()
		select {
		case <-ctx.Done():
			return Entry{}, ctx.Err()
		case <-running.done:
			return running.entry, running.err
		}
	}
	call := &warmCall{done: make(chan struct{})}
	storage.warming[identity] = call
	storage.mu.Unlock()

	call.entry, call.err = storage.warmLocal(ctx, request, remote, remoteStorage, remoteName)
	storage.mu.Lock()
	delete(storage.warming, identity)
	close(call.done)
	storage.mu.Unlock()
	return call.entry, call.err
}

func (storage *CacheChain) Reserve(ctx context.Context, request ReserveRequest) (Reservation, error) {
	reservation, err := storage.local.Reserve(ctx, request)
	if err != nil {
		return Reservation{}, err
	}
	storage.mu.Lock()
	storage.reservations[reservation.ID] = cloneReserveRequest(request)
	storage.mu.Unlock()
	return reservation, nil
}

func (storage *CacheChain) Upload(ctx context.Context, request UploadRequest) error {
	return storage.local.Upload(ctx, request)
}

func (storage *CacheChain) Commit(ctx context.Context, request CommitRequest) (Entry, error) {
	entry, err := storage.local.Commit(ctx, request)
	if err != nil {
		return Entry{}, err
	}
	storage.mu.Lock()
	reservation, found := storage.reservations[request.ReservationID]
	delete(storage.reservations, request.ReservationID)
	storage.mu.Unlock()
	if found {
		storage.publishTeamBestEffort(ctx, reservation, entry)
	}
	return entry, nil
}

func (storage *CacheChain) Open(ctx context.Context, request OpenRequest) (Archive, error) {
	return storage.local.Open(ctx, request)
}

func (storage *CacheChain) warmLocal(
	ctx context.Context,
	request LookupRequest,
	remote LookupResult,
	remoteStorage StorageIndex,
	remoteName string,
) (_ Entry, returnErr error) {
	archive, err := remoteStorage.Open(ctx, OpenRequest{Scope: request.Scope, ID: remote.Entry.ID})
	if err != nil {
		return Entry{}, err
	}
	archiveOpen := true
	defer func() {
		if archiveOpen {
			returnErr = errors.Join(returnErr, archive.Body.Close())
		}
	}()

	warmScope := request.Scope
	warmScope.Ref = remote.Entry.Ref
	expectedSize := archive.Entry.Size
	var cacheSize *int64
	if expectedSize >= 0 {
		cacheSize = &expectedSize
	}
	reservation, err := storage.local.Reserve(ctx, ReserveRequest{
		Scope: warmScope, Key: remote.Entry.Key, Version: remote.Entry.Version, CacheSize: cacheSize,
	})
	if errors.Is(err, ErrAlreadyExists) {
		return storage.existingLocalEntry(ctx, warmScope, remote.Entry)
	}
	if err != nil {
		return Entry{}, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if aborter, ok := storage.local.(reservationAborter); ok {
			returnErr = errors.Join(returnErr, aborter.Abort(ctx, reservation.ID))
		}
	}()

	size, err := uploadArchive(ctx, storage.local, reservation.ID, &warmScope, archive.Body, expectedSize)
	if err != nil {
		return Entry{}, err
	}
	if err := archive.Body.Close(); err != nil {
		return Entry{}, err
	}
	archiveOpen = false
	if releaser, ok := remoteStorage.(entryReleaser); ok {
		if err := releaser.ReleaseEntry(ctx, remote.Entry.ID); err != nil {
			return Entry{}, fmt.Errorf("release %s staging: %w", remoteName, err)
		}
	}
	entry, err := storage.local.Commit(ctx, CommitRequest{
		ReservationID: reservation.ID, Scope: &warmScope, Size: size, Origin: remote.Source,
		Public: clonePublicEntryMetadata(remote.Entry.Public),
	})
	if err != nil {
		return Entry{}, err
	}
	committed = true
	return entry, nil
}

func (storage *CacheChain) revalidateWarmedPublic(
	ctx context.Context,
	entry Entry,
) (*PublicEntryMetadata, error) {
	revalidator, ok := storage.public.(publicEntryRevalidator)
	if !ok {
		return nil, errors.New("Public Cache revalidator is unavailable")
	}
	refreshed, err := revalidator.RevalidateEntry(ctx, entry.Public)
	if err != nil {
		return nil, err
	}
	updater := storage.local.(publicEntryMetadataUpdater)
	if err := updater.UpdatePublicEntryMetadata(ctx, entry.ID, refreshed); err != nil {
		return nil, err
	}
	return refreshed, nil
}

func (storage *CacheChain) invalidateLocalEntry(ctx context.Context, id int64) error {
	invalidator, ok := storage.local.(entryInvalidator)
	if !ok {
		return errors.New("Local Cache invalidator is unavailable")
	}
	err := invalidator.InvalidateEntry(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (storage *CacheChain) existingLocalEntry(ctx context.Context, scope Scope, remote Entry) (Entry, error) {
	scope.DefaultRef = scope.Ref
	result, err := storage.local.Lookup(ctx, LookupRequest{Scope: scope, Keys: []string{remote.Key}, Version: remote.Version})
	if err != nil {
		return Entry{}, err
	}
	if result.Match != MatchExact || result.Entry.Key != remote.Key {
		return Entry{}, ErrAlreadyExists
	}
	return result.Entry, nil
}

func (storage *CacheChain) publishTeamBestEffort(ctx context.Context, reservation ReserveRequest, entry Entry) {
	if storage.team == nil {
		return
	}
	archive, err := storage.local.Open(ctx, OpenRequest{Scope: reservation.Scope, ID: entry.ID})
	if err != nil {
		return
	}
	defer archive.Body.Close()
	size := entry.Size
	teamReservation, err := storage.team.Reserve(ctx, ReserveRequest{
		Scope: reservation.Scope, Key: reservation.Key, Version: reservation.Version, CacheSize: &size,
	})
	if err != nil {
		return
	}
	if _, err := uploadArchive(ctx, storage.team, teamReservation.ID, &reservation.Scope, archive.Body, size); err != nil {
		return
	}
	_, _ = storage.team.Commit(ctx, CommitRequest{ReservationID: teamReservation.ID, Scope: &reservation.Scope, Size: size})
}

func uploadArchive(
	ctx context.Context,
	destination StorageIndex,
	reservationID int64,
	scope *Scope,
	source io.Reader,
	expectedSize int64,
) (int64, error) {
	buffer := make([]byte, cacheChainTransferChunkSize)
	var offset int64
	for {
		count, err := io.ReadFull(source, buffer)
		if count > 0 {
			chunk := buffer[:count]
			if uploadErr := destination.Upload(ctx, UploadRequest{
				ReservationID: reservationID,
				Scope:         scope,
				Start:         offset,
				End:           offset + int64(count) - 1,
				Body:          bytes.NewReader(chunk),
			}); uploadErr != nil {
				return offset, uploadErr
			}
			offset += int64(count)
		}
		switch err {
		case nil:
			continue
		case io.EOF, io.ErrUnexpectedEOF:
			if expectedSize >= 0 && offset != expectedSize {
				return offset, ErrIncompleteUpload
			}
			return offset, nil
		default:
			return offset, err
		}
	}
}

func isBestPossibleLookup(result LookupResult, request LookupRequest) bool {
	return len(request.Keys) > 0 && result.RefScope == RefScopeCurrent &&
		result.RequestedKey == request.Keys[0] && result.Match == MatchExact
}

func lookupIsBetter(candidate, incumbent LookupResult, request LookupRequest) bool {
	candidateRank := lookupRank(candidate, request)
	incumbentRank := lookupRank(incumbent, request)
	for index := range candidateRank {
		if candidateRank[index] != incumbentRank[index] {
			return candidateRank[index] < incumbentRank[index]
		}
	}
	return false
}

func lookupRank(result LookupResult, request LookupRequest) [3]int {
	refRank := 2
	if result.RefScope == RefScopeCurrent {
		refRank = 0
	} else if result.RefScope == RefScopeDefault {
		refRank = 1
	}
	keyRank := len(request.Keys) + 1
	for index, key := range request.Keys {
		if result.RequestedKey == key {
			keyRank = index
			break
		}
	}
	matchRank := 2
	if result.Match == MatchExact {
		matchRank = 0
	} else if result.Match == MatchPrefix {
		matchRank = 1
	}
	return [3]int{refRank, keyRank, matchRank}
}

var _ StorageIndex = (*CacheChain)(nil)
