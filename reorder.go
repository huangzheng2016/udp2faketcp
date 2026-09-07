package udp2faketcp

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	// reorderWindow is the maximum number of out-of-order datagrams one
	// session buffers before declaring the gap lost. It must cover the
	// session's bandwidth-delay product: pps x inter-flow skew. 1024
	// holds ~100Mbps x 80ms or ~40Mbps x 200ms; memory is bounded to
	// window x MTU per session.
	reorderWindow = 1024
	// reorderMaxDelay is the default gap-wait before one is skipped, used
	// until heartbeat RTT estimates take over. Trades latency for
	// losslessness. 100ms covers typical ECMP path skew during the first
	// seconds of a session, before adaptation kicks in.
	reorderMaxDelay = 100 * time.Millisecond
	// bounds for the adaptive gap-wait derived from flow RTTs
	reorderMinDelay    = 5 * time.Millisecond
	reorderMaxDelayCap = 500 * time.Millisecond
)

type reorderEntry struct {
	payload []byte
	ts      time.Time
}

// reorderBuffer reassembles datagrams that were striped across multiple
// flows back into their original order. It is safe for concurrent use:
// the flows of a session push from different goroutines.
type reorderBuffer struct {
	mu       sync.Mutex
	init     bool
	expect   uint64
	buf      map[uint64]reorderEntry
	ooo      uint64       // datagrams buffered out-of-order
	late     uint64       // datagrams arrived too late to deliver
	maxDelay atomic.Int64 // ns; 0 means reorderMaxDelay
}

// stats returns tuning counters (out-of-order buffered, arrived too late).
func (r *reorderBuffer) stats() (ooo, late uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ooo, r.late
}

// setMaxDelay tunes how long a buffered datagram may wait for a gap to
// fill. Sessions update it from heartbeat RTT estimates.
func (r *reorderBuffer) setMaxDelay(d time.Duration) {
	r.maxDelay.Store(int64(d))
}

func (r *reorderBuffer) delayBudget() time.Duration {
	if d := r.maxDelay.Load(); d > 0 {
		return time.Duration(d)
	}
	return reorderMaxDelay
}

// push feeds one datagram and returns everything that became deliverable,
// in order. The payload is copied only when it has to be buffered.
func (r *reorderBuffer) push(seq uint64, payload []byte) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.init {
		r.init, r.expect, r.buf = true, seq, make(map[uint64]reorderEntry)
	}
	if seq < r.expect {
		r.late++
		return nil // too late, counted as lost already
	}
	var out [][]byte
	if seq == r.expect {
		out = append(out, payload)
		r.expect++
	} else {
		r.ooo++
		cp := make([]byte, len(payload))
		copy(cp, payload)
		r.buf[seq] = reorderEntry{cp, time.Now()}
	}
	return append(out, r.drainLocked()...)
}

// expire skips gaps whose successors have waited longer than reorderMaxDelay.
// Call it when traffic may be stalled (e.g. on heartbeat), so the last
// datagrams of a burst are not held back forever.
func (r *reorderBuffer) expire() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.init {
		return nil
	}
	return r.drainLocked()
}

// drainLocked delivers buffered datagrams that follow expect directly, then
// skips gaps when the buffer is full or the oldest entry has gone stale.
func (r *reorderBuffer) drainLocked() [][]byte {
	var out [][]byte
	for len(r.buf) > 0 {
		if e, ok := r.buf[r.expect]; ok {
			delete(r.buf, r.expect)
			out = append(out, e.payload)
			r.expect++
			continue
		}
		var minSeq uint64
		var minTS time.Time
		first := true
		for s, e := range r.buf {
			if first || s < minSeq {
				minSeq, minTS, first = s, e.ts, false
			}
		}
		if len(r.buf) < reorderWindow && time.Since(minTS) < r.delayBudget() {
			break // give the gap a chance to fill
		}
		r.expect = minSeq // declare the gap lost
	}
	return out
}
