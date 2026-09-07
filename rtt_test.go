package udp2faketcp

import (
	"testing"
	"time"
)

func TestRTTEstimator(t *testing.T) {
	var r rttEstimator
	if r.timeout() != 0 {
		t.Fatal("timeout without samples should be 0")
	}
	r.add(int64(100 * time.Millisecond))
	if d := r.timeout(); d != 300*time.Millisecond { // srtt + 4*(srtt/2)
		t.Fatalf("first sample: timeout = %v", d)
	}
	for i := 0; i < 20; i++ {
		r.add(int64(100 * time.Millisecond))
	}
	if d := r.timeout(); d < 100*time.Millisecond || d > 200*time.Millisecond {
		t.Fatalf("converged timeout = %v", d)
	}
	r.add(int64(300 * time.Millisecond))
	if d := r.timeout(); d <= 100*time.Millisecond {
		t.Fatalf("spike not reflected: %v", d)
	}
}
