package udp2faketcp

import (
	"bytes"
	"testing"
)

func TestFrameNoKey(t *testing.T) {
	payload := []byte("hello")
	frame := encodeFrame(nil, nil, 0, frameData, payload)
	if len(frame) != 1+len(payload) {
		t.Fatalf("frame length = %d, want %d", len(frame), 1+len(payload))
	}
	typ, _, got, ok := decodeFrame(nil, frame)
	if !ok || typ != frameData || !bytes.Equal(got, payload) {
		t.Fatalf("decode = (%d, %q, %v)", typ, got, ok)
	}
}

func TestFrameWithKey(t *testing.T) {
	key := deriveKey("secret")
	payload := []byte("hello")
	frame := encodeFrame(nil, key, 42, frameData, payload)
	if len(frame) != frameHeadLen+len(payload) {
		t.Fatalf("frame length = %d, want %d", len(frame), frameHeadLen+len(payload))
	}
	typ, seq, got, ok := decodeFrame(key, frame)
	if !ok || typ != frameData || seq != 42 || !bytes.Equal(got, payload) {
		t.Fatalf("decode = (%d, %d, %q, %v)", typ, seq, got, ok)
	}
}

func TestFrameTamper(t *testing.T) {
	key := deriveKey("secret")
	frame := encodeFrame(nil, key, 1, frameData, []byte("hello"))
	frame[frameHeadLen] ^= 0xff // flip a payload bit
	if _, _, _, ok := decodeFrame(key, frame); ok {
		t.Fatal("tampered frame accepted")
	}
	if _, _, _, ok := decodeFrame(deriveKey("other"), encodeFrame(nil, key, 1, frameData, []byte("hi"))); ok {
		t.Fatal("frame accepted under wrong key")
	}
	if _, _, _, ok := decodeFrame(key, []byte{0, 1, 2}); ok {
		t.Fatal("short frame accepted")
	}
}

func TestFrameKeyMismatch(t *testing.T) {
	// a keyed receiver must reject unkeyed frames and vice versa
	frame := encodeFrame(nil, nil, 0, frameData, []byte("hello"))
	if _, _, _, ok := decodeFrame(deriveKey("k"), frame); ok {
		t.Fatal("unkeyed frame accepted by keyed receiver")
	}
}

func TestReplayWindow(t *testing.T) {
	var w replayWindow
	if !w.check(100) {
		t.Fatal("first frame rejected")
	}
	if w.check(100) {
		t.Fatal("duplicate frame accepted")
	}
	if !w.check(101) || !w.check(105) {
		t.Fatal("fresh frames rejected")
	}
	if !w.check(103) {
		t.Fatal("out-of-order frame within window rejected")
	}
	if w.check(103) {
		t.Fatal("replayed out-of-order frame accepted")
	}
	// jump far enough that every prior seq falls out of the window
	if !w.check(100 + 2*replayWindowBits) {
		t.Fatal("frame beyond window rejected")
	}
	if w.check(100) {
		t.Fatal("ancient frame accepted")
	}
	if w.check(105) {
		t.Fatal("evicted frame accepted")
	}
	if !w.check(100 + 2*replayWindowBits + 1) {
		t.Fatal("fresh frame after window jump rejected")
	}
}
