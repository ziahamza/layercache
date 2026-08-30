package actionscache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

type MemoryStorage struct {
	mu           sync.Mutex
	nextID       int64
	nextSequence uint64
	entries      []*memoryEntry
	reservations map[int64]*memoryReservation
	occupied     map[memoryIdentity]struct{}
}

type memoryIdentity struct {
	repository    string
	compatibility string
	ref           string
	key           string
	version       string
}

type memoryEntry struct {
	Entry
	repository    string
	compatibility string
	sequence      uint64
	body          []byte
}

type memoryReservation struct {
	id           int64
	identity     memoryIdentity
	expectedSize *int64
	chunks       map[int64][]byte
}

func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		reservations: make(map[int64]*memoryReservation),
		occupied:     make(map[memoryIdentity]struct{}),
	}
}

func (storage *MemoryStorage) Lookup(_ context.Context, request LookupRequest) (LookupResult, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()

	refs := []struct {
		value string
		scope RefScope
	}{{value: request.Scope.Ref, scope: RefScopeCurrent}}
	if request.Scope.DefaultRef != request.Scope.Ref {
		refs = append(refs, struct {
			value string
			scope RefScope
		}{value: request.Scope.DefaultRef, scope: RefScopeDefault})
	}

	for _, ref := range refs {
		for _, requestedKey := range request.Keys {
			if entry := storage.findNewest(request, ref.value, requestedKey, true); entry != nil {
				return LookupResult{
					Entry:        cloneEntry(entry.Entry),
					Match:        MatchExact,
					RequestedKey: requestedKey,
					RefScope:     ref.scope,
					Source:       SourceLocalCache,
				}, nil
			}
			if entry := storage.findNewest(request, ref.value, requestedKey, false); entry != nil {
				return LookupResult{
					Entry:        cloneEntry(entry.Entry),
					Match:        MatchPrefix,
					RequestedKey: requestedKey,
					RefScope:     ref.scope,
					Source:       SourceLocalCache,
				}, nil
			}
		}
	}
	return LookupResult{}, ErrNotFound
}

func (storage *MemoryStorage) Reserve(_ context.Context, request ReserveRequest) (Reservation, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()

	identity := memoryIdentity{
		repository:    request.Scope.Repository,
		compatibility: request.Scope.Compatibility,
		ref:           request.Scope.Ref,
		key:           request.Key,
		version:       request.Version,
	}
	if _, found := storage.occupied[identity]; found {
		return Reservation{}, ErrAlreadyExists
	}
	storage.nextID++
	id := storage.nextID
	var expectedSize *int64
	if request.CacheSize != nil {
		size := *request.CacheSize
		expectedSize = &size
	}
	storage.reservations[id] = &memoryReservation{
		id:           id,
		identity:     identity,
		expectedSize: expectedSize,
		chunks:       make(map[int64][]byte),
	}
	storage.occupied[identity] = struct{}{}
	return Reservation{ID: id}, nil
}

// Abort releases an incomplete in-memory reservation used by an interrupted
// Local Cache warm.
func (storage *MemoryStorage) Abort(_ context.Context, reservationID int64) error {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	reservation, found := storage.reservations[reservationID]
	if !found {
		return ErrNotFound
	}
	delete(storage.reservations, reservationID)
	delete(storage.occupied, reservation.identity)
	return nil
}

func (storage *MemoryStorage) Upload(_ context.Context, request UploadRequest) error {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return err
	}
	if request.Start < 0 || request.End < request.Start || int64(len(body)) != request.End-request.Start+1 {
		return ErrInvalidUpload
	}

	storage.mu.Lock()
	defer storage.mu.Unlock()
	reservation, found := storage.reservations[request.ReservationID]
	if !found {
		return ErrNotFound
	}
	if !reservationScopeMatches(request.Scope, reservation.identity.repository,
		reservation.identity.compatibility, reservation.identity.ref) {
		return ErrNotFound
	}
	if reservation.expectedSize != nil && request.End >= *reservation.expectedSize {
		return ErrInvalidUpload
	}
	if existing, found := reservation.chunks[request.Start]; found {
		if bytes.Equal(existing, body) {
			return nil
		}
		return ErrInvalidUpload
	}
	reservation.chunks[request.Start] = append([]byte(nil), body...)
	return nil
}

func (storage *MemoryStorage) Commit(_ context.Context, request CommitRequest) (Entry, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()

	reservation, found := storage.reservations[request.ReservationID]
	if !found {
		return Entry{}, ErrNotFound
	}
	if !reservationScopeMatches(request.Scope, reservation.identity.repository,
		reservation.identity.compatibility, reservation.identity.ref) {
		return Entry{}, ErrNotFound
	}
	if request.Size < 0 || reservation.expectedSize != nil && request.Size != *reservation.expectedSize {
		return Entry{}, ErrInvalidUpload
	}
	starts := make([]int64, 0, len(reservation.chunks))
	for start := range reservation.chunks {
		starts = append(starts, start)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	var body []byte
	var offset int64
	for _, start := range starts {
		if start != offset {
			return Entry{}, ErrIncompleteUpload
		}
		chunk := reservation.chunks[start]
		body = append(body, chunk...)
		offset += int64(len(chunk))
	}
	if offset != request.Size {
		return Entry{}, ErrIncompleteUpload
	}
	origin, publicMetadata, err := normalizedCommitTrust(request)
	if err != nil {
		return Entry{}, err
	}
	if publicMetadata != nil {
		digest := sha256.Sum256(body)
		if hex.EncodeToString(digest[:]) != publicMetadata.Digest || int64(len(body)) != publicMetadata.Size {
			return Entry{}, ErrInvalidUpload
		}
	}

	storage.nextSequence++
	entry := &memoryEntry{
		Entry: Entry{
			ID:        reservation.id,
			Key:       reservation.identity.key,
			Version:   reservation.identity.version,
			Ref:       reservation.identity.ref,
			Size:      request.Size,
			CreatedAt: time.Now().UTC(),
			Origin:    origin,
			Public:    publicMetadata,
		},
		repository:    reservation.identity.repository,
		compatibility: reservation.identity.compatibility,
		sequence:      storage.nextSequence,
		body:          body,
	}
	storage.entries = append(storage.entries, entry)
	delete(storage.reservations, request.ReservationID)
	return cloneEntry(entry.Entry), nil
}

func (storage *MemoryStorage) Open(_ context.Context, request OpenRequest) (Archive, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	for _, entry := range storage.entries {
		if entry.ID == request.ID &&
			entry.repository == request.Scope.Repository &&
			entry.compatibility == request.Scope.Compatibility &&
			(entry.Ref == request.Scope.Ref || entry.Ref == request.Scope.DefaultRef) {
			return Archive{
				Entry: cloneEntry(entry.Entry),
				Body:  io.NopCloser(bytes.NewReader(append([]byte(nil), entry.body...))),
			}, nil
		}
	}
	return Archive{}, ErrNotFound
}

func (storage *MemoryStorage) InvalidateEntry(_ context.Context, id int64) error {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	for index, entry := range storage.entries {
		if entry.ID != id {
			continue
		}
		identity := memoryIdentity{
			repository: entry.repository, compatibility: entry.compatibility,
			ref: entry.Ref, key: entry.Key, version: entry.Version,
		}
		storage.entries = append(storage.entries[:index], storage.entries[index+1:]...)
		delete(storage.occupied, identity)
		return nil
	}
	return ErrNotFound
}

func (storage *MemoryStorage) UpdatePublicEntryMetadata(_ context.Context, id int64, metadata *PublicEntryMetadata) error {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	for _, entry := range storage.entries {
		if entry.ID == id {
			entry.Public = clonePublicEntryMetadata(metadata)
			entry.Origin = SourcePublicCache
			return nil
		}
	}
	return ErrNotFound
}

func (storage *MemoryStorage) findNewest(request LookupRequest, ref, key string, exact bool) *memoryEntry {
	var newest *memoryEntry
	for _, entry := range storage.entries {
		if entry.repository != request.Scope.Repository ||
			entry.compatibility != request.Scope.Compatibility ||
			entry.Ref != ref || entry.Version != request.Version {
			continue
		}
		matches := entry.Key == key
		if !exact {
			matches = entry.Key != key && strings.HasPrefix(entry.Key, key)
		}
		if matches && (newest == nil || entry.sequence > newest.sequence) {
			newest = entry
		}
	}
	return newest
}

var _ StorageIndex = (*MemoryStorage)(nil)
