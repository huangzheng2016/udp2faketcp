package udp2faketcp

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	reorderWindow = 1024

	reorderMaxDelay = 100 * time.Millisecond

	reorderMinDelay    = 5 * time.Millisecond
	reorderMaxDelayCap = 500 * time.Millisecond
)

type reorderEntry struct {
	payload []byte
	ts      time.Time
}

type reorderBuffer struct {
	mu       sync.Mutex
	init     bool
	expect   uint64
	buf      map[uint64]reorderEntry
	ooo      uint64
	late     uint64
	maxDelay atomic.Int64
}

func (r *reorderBuffer) stats() (ooo, late uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ooo, r.late
}

func (r *reorderBuffer) setMaxDelay(d time.Duration) {
	r.maxDelay.Store(int64(d))
}

func (r *reorderBuffer) delayBudget() time.Duration {
	if d := r.maxDelay.Load(); d > 0 {
		return time.Duration(d)
	}
	return reorderMaxDelay
}

func (r *reorderBuffer) push(seq uint64, payload []byte) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.init {
		r.init, r.expect, r.buf = true, seq, make(map[uint64]reorderEntry)
	}
	if seq < r.expect {
		r.late++
		return nil
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

func (r *reorderBuffer) expire() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.init {
		return nil
	}
	return r.drainLocked()
}

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
			break
		}
		r.expect = minSeq
	}
	return out
}
