package actionscache

import (
	"net/url"
	"testing"
	"time"
)

func TestAbandonedChainReservationMetadataStaysBounded(t *testing.T) {
	now := time.Now().UTC()
	chain := &CacheChain{reservations: make(map[int64]cacheChainReservation)}
	request := ReserveRequest{Scope: Scope{Repository: "acme/widgets"}, Key: "key", Version: "v1"}
	for id := int64(1); id <= maxTrackedCacheChainReservations+8; id++ {
		chain.rememberReservation(id, request, now.Add(time.Duration(id)))
	}
	if len(chain.reservations) != maxTrackedCacheChainReservations {
		t.Fatalf("tracked chain reservations = %d, want %d", len(chain.reservations), maxTrackedCacheChainReservations)
	}
	if _, found := chain.reservations[1]; found {
		t.Fatal("oldest abandoned chain reservation was not evicted")
	}
	chain.reservations[-1] = cacheChainReservation{expires: now.Add(-time.Second)}
	chain.pruneReservationsLocked(now)
	if _, found := chain.reservations[-1]; found {
		t.Fatal("expired chain reservation was not removed")
	}
}

func TestRemoteMetadataStaysBoundedAndExpiresIndependently(t *testing.T) {
	now := time.Now().UTC()
	storage := &RemoteStorage{
		locations:    make(map[remoteEntryIdentity]remoteLocationMetadata),
		entries:      make(map[remoteEntryIdentity]remoteEntryMetadata),
		scopes:       make(map[remoteEntryIdentity]time.Time),
		reservations: make(map[int64]remoteReservationMetadata),
	}
	location := &url.URL{Scheme: "https", Host: "cache.example.test", Path: "/archive"}
	for id := int64(1); id <= maxRemoteEntryMetadata+8; id++ {
		identity := remoteEntryIdentity{id: id, scope: Scope{Repository: "acme/widgets", Ref: "refs/heads/main"}}
		storage.rememberEntryLocked(identity, Entry{ID: id}, location, remoteLookupMetadataTTL, now.Add(time.Duration(id)))
	}
	if len(storage.scopes) != maxRemoteEntryMetadata || len(storage.entries) != maxRemoteEntryMetadata ||
		len(storage.locations) != maxRemoteEntryMetadata {
		t.Fatalf("remote metadata sizes = scopes %d, entries %d, locations %d", len(storage.scopes), len(storage.entries), len(storage.locations))
	}
	if _, found := storage.scopes[remoteEntryIdentity{id: 1, scope: Scope{Repository: "acme/widgets", Ref: "refs/heads/main"}}]; found {
		t.Fatal("oldest remote lookup metadata was not evicted")
	}

	expiring := remoteEntryIdentity{id: -1, scope: Scope{Repository: "acme/widgets", Ref: "refs/heads/main"}}
	storage.scopes[expiring] = now.Add(time.Hour)
	storage.entries[expiring] = remoteEntryMetadata{entry: Entry{ID: -1}, expires: now.Add(-time.Second)}
	storage.locations[expiring] = remoteLocationMetadata{location: location, expires: now.Add(-time.Second)}
	storage.reservations[-1] = remoteReservationMetadata{expires: now.Add(-time.Second)}
	storage.pruneMetadataLocked(now)
	if _, found := storage.entries[expiring]; found {
		t.Fatal("expired remote entry metadata was not removed")
	}
	if _, found := storage.locations[expiring]; found {
		t.Fatal("expired remote lookup location was not removed")
	}
	if _, found := storage.reservations[-1]; found {
		t.Fatal("expired remote reservation metadata was not removed")
	}
	if _, found := storage.scopes[expiring]; !found {
		t.Fatal("scope tombstone expired with one-shot lookup metadata")
	}
	storage.pruneMetadataLocked(now.Add(2 * time.Hour))
	if _, found := storage.scopes[expiring]; found {
		t.Fatal("expired remote scope tombstone was not removed")
	}
}
