package udp2faketcp

import (
	"testing"
	"time"
)

func TestReorderInOrder(t *testing.T) {
	var r reorderBuffer
	if out := r.push(1, []byte("a")); len(out) != 1 || string(out[0]) != "a" {
		t.Fatalf("got %v", out)
	}
	if out := r.push(2, []byte("b")); len(out) != 1 || string(out[0]) != "b" {
		t.Fatalf("got %v", out)
	}
}

func TestReorderOutOfOrder(t *testing.T) {
	var r reorderBuffer
	r.push(1, []byte("a"))
	if out := r.push(3, []byte("c")); len(out) != 0 {
		t.Fatalf("gap frame should be buffered: %v", out)
	}
	out := r.push(2, []byte("b"))
	if len(out) != 2 || string(out[0]) != "b" || string(out[1]) != "c" {
		t.Fatalf("gap filled, expect [b c], got %v", out)
	}
}

func TestReorderLateDrop(t *testing.T) {
	var r reorderBuffer
	r.push(5, []byte("e"))
	if out := r.push(4, []byte("d")); len(out) != 0 {
		t.Fatalf("late frame should be dropped: %v", out)
	}
	if out := r.push(5, []byte("e2")); len(out) != 0 {
		t.Fatalf("duplicate frame should be dropped: %v", out)
	}
}

func TestReorderGapSkipOnFull(t *testing.T) {
	var r reorderBuffer
	r.push(1, []byte("a"))

	for i := 0; i < reorderWindow-1; i++ {
		if out := r.push(uint64(3+i), []byte("x")); len(out) != 0 {
			t.Fatalf("push %d should still be buffered, got %v", 3+i, out)
		}
	}

	out := r.push(uint64(3+reorderWindow-1), []byte("y"))
	if len(out) != reorderWindow || string(out[len(out)-1]) != "y" {
		t.Fatalf("full buffer should skip the gap and drain, got %d frames", len(out))
	}
	if r.expect != uint64(3+reorderWindow) {
		t.Fatalf("expect = %d", r.expect)
	}
}

func TestReorderExpire(t *testing.T) {
	var r reorderBuffer
	r.push(1, []byte("a"))
	r.push(3, []byte("c"))
	if out := r.expire(); len(out) != 0 {
		t.Fatalf("should still wait: %v", out)
	}

	r.mu.Lock()
	e := r.buf[3]
	e.ts = time.Now().Add(-2 * reorderMaxDelay)
	r.buf[3] = e
	r.mu.Unlock()
	out := r.expire()
	if len(out) != 1 || string(out[0]) != "c" {
		t.Fatalf("gap should be skipped after expiry, got %v", out)
	}
	if r.expect != 4 {
		t.Fatalf("expect = %d, want 4", r.expect)
	}
}

func TestReorderBufferedPayloadCopied(t *testing.T) {
	var r reorderBuffer
	src := []byte("c")
	r.push(1, []byte("a"))
	r.push(3, src)
	src[0] = 'X'
	out := r.push(2, []byte("b"))
	if len(out) != 2 || string(out[1]) != "c" {
		t.Fatalf("buffered payload was not copied: %v", out)
	}
}
