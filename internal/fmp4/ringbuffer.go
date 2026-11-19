package fmp4

import (
	"sync"
	"time"
)

// GOPRingBuffer keeps recent GOPs (always aligned to a leading video keyframe)
// bounded by both pre-roll duration and byte budget.
type GOPRingBuffer struct {
	mu          sync.Mutex
	maxDuration time.Duration
	maxBytes    int64
	gops        [][]Fragment
	totalBytes  int64
}

// NewGOPRingBuffer creates a ring buffer retaining at least maxDuration of recent GOPs
// while staying within maxBytes.
func NewGOPRingBuffer(maxDuration time.Duration, maxBytes int64) *GOPRingBuffer {
	if maxBytes <= 0 {
		maxBytes = 24 * 1024 * 1024
	}
	return &GOPRingBuffer{
		maxDuration: maxDuration,
		maxBytes:    maxBytes,
	}
}

// Push appends a fragment to the ring buffer and evicts stale GOPs.
func (b *GOPRingBuffer) Push(f Fragment) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if f.ReceivedAt.IsZero() {
		f.ReceivedAt = time.Now()
	}

	if f.IsKeyframe {
		b.gops = append(b.gops, []Fragment{f})
	} else {
		if len(b.gops) == 0 {
			// Wait until we have seen an IDR keyframe so snapshots always start cleanly.
			return
		}
		last := len(b.gops) - 1
		b.gops[last] = append(b.gops[last], f)
	}
	b.totalBytes += f.ByteSize()

	for len(b.gops) > 1 {
		nextStart := b.gops[1][0].ReceivedAt
		overDur := b.maxDuration <= 0 || f.ReceivedAt.Sub(nextStart) >= b.maxDuration
		overBytes := b.totalBytes > b.maxBytes
		if !overDur && !overBytes {
			break
		}
		for _, oldFrag := range b.gops[0] {
			b.totalBytes -= oldFrag.ByteSize()
		}
		b.gops[0] = nil
		b.gops = b.gops[1:]
	}

	// Guard against a broken stream never sending another keyframe.
	if len(b.gops) == 1 && b.totalBytes > b.maxBytes {
		b.gops = nil
		b.totalBytes = 0
	}
}

// Snapshot returns a copy of all currently buffered fragments starting at a keyframe.
func (b *GOPRingBuffer) Snapshot() []Fragment {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.gops) == 0 {
		return nil
	}
	total := 0
	for _, g := range b.gops {
		total += len(g)
	}
	out := make([]Fragment, 0, total)
	for _, g := range b.gops {
		out = append(out, g...)
	}
	return out
}

// Reset clears all buffered GOPs (e.g., upon stream reconnect).
func (b *GOPRingBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gops = nil
	b.totalBytes = 0
}

// Stats returns the current buffered GOP count and byte size.
func (b *GOPRingBuffer) Stats() (gops int, bytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.gops), b.totalBytes
}
