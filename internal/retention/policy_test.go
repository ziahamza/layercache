package retention_test

import (
	"testing"
	"time"

	"github.com/layercache/layercache/internal/retention"
)

func TestImpactKeepsOlderExpensiveArtifactAndFallsBackForUnknownTiming(t *testing.T) {
	now := time.Unix(1000, 0)
	candidates := []retention.Candidate{
		{ID: "expensive", Bytes: 100, ProducerDurationMS: 60000, DurationKnown: true, LastAccess: now.Add(-time.Hour)},
		{ID: "cheap", Bytes: 100, ProducerDurationMS: 100, DurationKnown: true, LastAccess: now},
	}
	if index, policy := retention.Choose(retention.Impact, candidates, now); index != 1 || policy != retention.Impact {
		t.Fatalf("impact victim = %d (%s)", index, policy)
	}
	candidates[1].DurationKnown = false
	if index, policy := retention.Choose(retention.Impact, candidates, now); index != 0 || policy != retention.LRU {
		t.Fatalf("unknown cost must select ordinary LRU, got %d (%s)", index, policy)
	}
}

func TestImpactUsesReclaimableBytesBoundedReadsAndAging(t *testing.T) {
	now := time.Unix(10000000, 0)
	base := retention.Candidate{ID: "small", Bytes: 100, ProducerDurationMS: 1000, DurationKnown: true, LastAccess: now}
	large := base
	large.ID, large.Bytes = "large", 1000
	if index, _ := retention.Choose(retention.Impact, []retention.Candidate{base, large}, now); index != 1 {
		t.Fatal("equal-cost larger artifact must be evicted first")
	}
	popular := base
	popular.ID, popular.AccessCount = "popular", 32
	if index, _ := retention.Choose(retention.Impact, []retention.Candidate{base, popular}, now); index != 0 {
		t.Fatal("observed reuse must improve retention value")
	}
	popular.LastAccess = now.Add(-10 * retention.HalfLife)
	popular.AccessCount = 1 << 62
	if index, _ := retention.Choose(retention.Impact, []retention.Candidate{base, popular}, now); index != 1 {
		t.Fatal("bounded popularity must decay after inactivity")
	}
}

func TestKnownZeroCostIsNotUnknownAndTiesAreStable(t *testing.T) {
	now := time.Now()
	candidates := []retention.Candidate{
		{ID: "b", DurationKnown: true, LastAccess: now},
		{ID: "a", DurationKnown: true, LastAccess: now},
	}
	if index, policy := retention.Choose(retention.Impact, candidates, now); index != 1 || policy != retention.Impact {
		t.Fatalf("deterministic zero-cost victim = %d (%s)", index, policy)
	}
	if index, _ := retention.Choose(retention.Impact, nil, now); index != -1 {
		t.Fatalf("empty candidate index = %d", index)
	}
}

func TestProducerDurationCannotBuyUnboundedRetention(t *testing.T) {
	now := time.Now()
	candidates := []retention.Candidate{
		{ID: "ancient", Bytes: 1, DurationKnown: true, ProducerDurationMS: 1 << 62, LastAccess: now.Add(-20 * retention.HalfLife)},
		{ID: "recent", Bytes: 1, DurationKnown: true, ProducerDurationMS: 1000, LastAccess: now},
	}
	if index, _ := retention.Choose(retention.Impact, candidates, now); index != 0 {
		t.Fatal("extreme producer duration defeated the documented cap and aging")
	}
}
