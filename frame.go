package udp2faketcp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"sync"
)

// Wire protocol: every fake-TCP payload is a frame.
//
//	without -k: [1B type][payload]
//	with    -k: [1B type][8B seq BE][16B HMAC-SHA256(key, seq||type||payload)][payload]
//
// Frame types: 0 = data, 1 = heartbeat, 2 = handshake.
// Both ends must run the same version and the same key.
const (
	frameData = iota
	frameHeartbeat
	frameHandshake
)

const (
	frameSeqLen  = 8
	frameTagLen  = 16
	frameHeadLen = 1 + frameSeqLen + frameTagLen
	handshakeLen = 16
)

func deriveKey(pass string) []byte {
	sum := sha256.Sum256([]byte(pass))
	return sum[:]
}

// payloadBudget is the maximum UDP payload that fits into one frame of
// MAX_PACKET_LEN bytes.
func payloadBudget() int {
	budget := MAX_PACKET_LEN - 1
	if AUTH_KEY != nil {
		budget -= frameSeqLen + frameTagLen
	}
	return budget
}

// encodeFrame appends a frame carrying payload to buf and returns the result.
func encodeFrame(buf, key []byte, seq uint64, typ byte, payload []byte) []byte {
	if key == nil {
		buf = append(buf, typ)
		return append(buf, payload...)
	}
	var seqb [frameSeqLen]byte
	binary.BigEndian.PutUint64(seqb[:], seq)
	buf = append(buf, typ)
	buf = append(buf, seqb[:]...)
	tagOff := len(buf)
	buf = append(buf, make([]byte, frameTagLen)...)
	buf = append(buf, payload...)
	mac := hmac.New(sha256.New, key)
	mac.Write(seqb[:])
	mac.Write([]byte{typ})
	mac.Write(payload)
	copy(buf[tagOff:], mac.Sum(nil)[:frameTagLen])
	return buf
}

// decodeFrame validates and splits a frame built by encodeFrame.
func decodeFrame(key, frame []byte) (typ byte, seq uint64, payload []byte, ok bool) {
	if len(frame) == 0 {
		return 0, 0, nil, false
	}
	if key == nil {
		return frame[0], 0, frame[1:], true
	}
	if len(frame) < frameHeadLen {
		return 0, 0, nil, false
	}
	typ = frame[0]
	seq = binary.BigEndian.Uint64(frame[1:])
	mac := hmac.New(sha256.New, key)
	mac.Write(frame[1 : 1+frameSeqLen])
	mac.Write(frame[:1])
	mac.Write(frame[frameHeadLen:])
	if !hmac.Equal(mac.Sum(nil)[:frameTagLen], frame[1+frameSeqLen:frameHeadLen]) {
		return 0, 0, nil, false
	}
	return typ, seq, frame[frameHeadLen:], true
}

const replayWindowBits = 4096

// replayWindow tracks received frame sequence numbers and rejects duplicates
// and frames too old to still be plausible.
type replayWindow struct {
	mu     sync.Mutex
	init   bool
	max    uint64
	bitmap [replayWindowBits / 64]uint64
}

// check reports whether seq is fresh, and marks it as seen.
func (w *replayWindow) check(seq uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.init {
		w.init, w.max = true, seq
		w.bitmap[0] = 1
		return true
	}
	if seq > w.max {
		if shift := seq - w.max; shift >= replayWindowBits {
			clear(w.bitmap[:])
		} else {
			words, bits := int(shift/64), uint(shift%64)
			for i := len(w.bitmap) - 1; i >= 0; i-- {
				src := i - words
				var v uint64
				if src >= 0 {
					v = w.bitmap[src] << bits
					if bits != 0 && src-1 >= 0 {
						v |= w.bitmap[src-1] >> (64 - bits)
					}
				}
				w.bitmap[i] = v
			}
		}
		w.bitmap[0] |= 1
		w.max = seq
		return true
	}
	diff := w.max - seq
	if diff >= replayWindowBits {
		return false
	}
	mask := uint64(1) << (diff % 64)
	idx := diff / 64
	if w.bitmap[idx]&mask != 0 {
		return false
	}
	w.bitmap[idx] |= mask
	return true
}
