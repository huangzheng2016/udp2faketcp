package udp2faketcp

import (
	"sync/atomic"
	"time"
)

type rttEstimator struct {
	srtt   atomic.Int64
	rttvar atomic.Int64
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

func (r *rttEstimator) timeout() time.Duration {
	s := r.srtt.Load()
	if s == 0 {
		return 0
	}
	return time.Duration(s + 4*r.rttvar.Load())
}
