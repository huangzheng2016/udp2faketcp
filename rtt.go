package udp2faketcp

import (
	"sync/atomic"
	"time"
)

// rttEstimator tracks the round-trip time of a flow with RFC 6298 style
// smoothing. Samples come from heartbeat echoes, so no clock sync between
// the peers is needed.
type rttEstimator struct {
	srtt   atomic.Int64 // ns
	rttvar atomic.Int64 // ns
}

func (r *rttEstimator) add(sample int64) {
	for {
		s := r.srtt.Load()
		v := r.rttvar.Load()
		var ns, nv int64
		if s == 0 {
			ns, nv = sample, sample/2
		} else {
			d := s - sample
			if d < 0 {
				d = -d
			}
			nv = v*3/4 + d/4
			ns = s*7/8 + sample/8
		}
		if r.srtt.CompareAndSwap(s, ns) {
			r.rttvar.Store(nv)
			return
		}
	}
}

// timeout returns srtt + 4*rttvar, or 0 when no estimate exists yet.
func (r *rttEstimator) timeout() time.Duration {
	s := r.srtt.Load()
	if s == 0 {
		return 0
	}
	return time.Duration(s + 4*r.rttvar.Load())
}
