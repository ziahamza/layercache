// Package retention ranks complete, unpinned cache artifacts. Storage adapters
// own admission, deduplication, transactions, and deletion; this package never
// changes cache visibility or makes a claim about measured compute savings.
package retention

import (
	"fmt"
	"math"
	"time"
)

type Policy string

const (
	LRU    Policy = "lru"
	Impact Policy = "impact"

	MaxAccessCount = int64(32)
	HalfLife       = 7 * 24 * time.Hour
	MaxProducerMS  = int64(24 * time.Hour / time.Millisecond)
)

func Normalize(policy Policy) (Policy, error) {
	if policy == "" {
		return LRU, nil
	}
	if policy != LRU && policy != Impact {
		return "", fmt.Errorf("eviction policy must be lru or impact, got %q", policy)
	}
	return policy, nil
}

// Candidate represents all cache keys sharing one immutable blob. Bytes is the
// size reclaimed when every key in the group is removed, counted exactly once.
// Storage adapters must omit a group if any key is pinned or protected.
type Candidate struct {
	ID                 string
	Bytes              int64
	ProducerDurationMS int64
	DurationKnown      bool
	AccessCount        int64
	LastAccess         time.Time
	CreatedAt          time.Time
}

// Choose returns an eviction candidate and the policy actually used. Unknown
// producer costs fall back to LRU across the candidate set so missing timing
// is never interpreted as free recomputation. The impact score is a gross
// producer-time heuristic, not a predicted hit probability or net ROI metric.
func Choose(policy Policy, candidates []Candidate, now time.Time) (int, Policy) {
	effective := policy
	if effective != Impact {
		effective = LRU
	}
	for _, candidate := range candidates {
		if !candidate.DurationKnown || candidate.ProducerDurationMS < 0 {
			effective = LRU
			break
		}
	}
	victim := -1
	var smallest float64
	for index, candidate := range candidates {
		score := 0.0
		if effective == Impact {
			score = grossValuePerByte(candidate, now)
		}
		if victim < 0 || score < smallest || score == smallest && older(candidate, candidates[victim]) {
			victim, smallest = index, score
		}
	}
	return victim, effective
}

func grossValuePerByte(candidate Candidate, now time.Time) float64 {
	age := now.Sub(candidate.LastAccess)
	if age < 0 {
		age = 0
	}
	reads := min(max(candidate.AccessCount, 0), MaxAccessCount)
	cost := min(max(candidate.ProducerDurationMS, 0), MaxProducerMS)
	return float64(cost) * (1 + math.Log2(1+float64(reads))) *
		math.Exp2(-float64(age)/float64(HalfLife)) / float64(max(candidate.Bytes, 1))
}

func older(left, right Candidate) bool {
	if !left.LastAccess.Equal(right.LastAccess) {
		return left.LastAccess.Before(right.LastAccess)
	}
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.Before(right.CreatedAt)
	}
	return left.ID < right.ID
}
