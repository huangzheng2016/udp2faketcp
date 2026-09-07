package udp2faketcp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"
)

// Wire protocol: every fake-TCP payload is a frame.
//
//	without -k: [1B type][8B streamSeq BE][payload]
//	with    -k: [1B type][8B streamSeq BE][8B hmacSeq BE][16B HMAC][payload]
//
// HMAC = HMAC-SHA256(key, hmacSeq || type || streamSeq || payload)[:16]
//
// Frame types: 0 = data, 1 = heartbeat, 2 = handshake, 3 = pong.
// streamSeq numbers data frames of one session across all its flows for
// reordering; control frames carry 0. hmacSeq is a per-flow counter for
// authentication and anti-replay. A handshake payload is the 16-byte
// session ID shared by all flows of the session. Heartbeats are answered
// with a pong so each side can measure the per-flow RTT from the echo.
// Both ends must run the same version and the same -k. The server adapts
// to the client's flow count via the session ID, so -f is client-only.
const (
	frameData = iota
	frameHeartbeat
	frameHandshake
	framePong
)

const (
	frameSeqLen   = 8
	streamSeqLen  = 8
	frameTagLen   = 16
	frameFixedLen = 1 + streamSeqLen
	frameHeadLen  = frameFixedLen + frameSeqLen + frameTagLen
	handshakeLen  = 16
)

func deriveKey(pass string) []byte {
	sum := sha256.Sum256([]byte(pass))
	return sum[:]
}

const heartbeatPayloadLen = 16

// buildHeartbeat returns a heartbeat payload carrying the local clock and
// the echo of the last timestamp received from the peer, allowing the peer
// to measure the round-trip time of this flow.
func buildHeartbeat(echoTS *atomic.Int64) []byte {
	p := make([]byte, heartbeatPayloadLen)
	binary.BigEndian.PutUint64(p, uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint64(p[8:], uint64(echoTS.Load()))
	return p
}

// parseHeartbeat splits a heartbeat payload into the peer's timestamp and
// the echoed local timestamp (0 when the peer has nothing to echo yet).
func parseHeartbeat(p []byte) (ts, echo int64, ok bool) {
	if len(p) < heartbeatPayloadLen {
		return 0, 0, false
	}
	ts = int64(binary.BigEndian.Uint64(p))
	echo = int64(binary.BigEndian.Uint64(p[8:]))
	return ts, echo, true
}

// payloadBudget is the maximum UDP payload that fits into one frame of
// MAX_PACKET_LEN bytes.
func payloadBudget() int {
	budget := MAX_PACKET_LEN - frameFixedLen
	if AUTH_KEY != nil {
		budget -= frameSeqLen + frameTagLen
	}
	return budget
}

// encodeFrame appends a frame carrying payload to buf and returns the result.
func encodeFrame(buf, key []byte, hmacSeq uint64, streamSeq uint64, typ byte, payload []byte) []byte {
	var ssb [streamSeqLen]byte
	binary.BigEndian.PutUint64(ssb[:], streamSeq)
	buf = append(buf, typ)
	buf = append(buf, ssb[:]...)
	if key == nil {
		return append(buf, payload...)
	}
	var hsb [frameSeqLen]byte
	binary.BigEndian.PutUint64(hsb[:], hmacSeq)
	buf = append(buf, hsb[:]...)
	tagOff := len(buf)
	buf = append(buf, make([]byte, frameTagLen)...)
	buf = append(buf, payload...)
	mac := hmac.New(sha256.New, key)
	mac.Write(hsb[:])
	mac.Write([]byte{typ})
	mac.Write(ssb[:])
	mac.Write(payload)
	copy(buf[tagOff:], mac.Sum(nil)[:frameTagLen])
	return buf
}

// decodeFrame validates and splits a frame built by encodeFrame.
func decodeFrame(key, frame []byte) (typ byte, streamSeq uint64, hmacSeq uint64, payload []byte, ok bool) {
	if len(frame) < frameFixedLen {
		return 0, 0, 0, nil, false
	}
	typ = frame[0]
	streamSeq = binary.BigEndian.Uint64(frame[1:])
	if key == nil {
		return typ, streamSeq, 0, frame[frameFixedLen:], true
	}
	if len(frame) < frameHeadLen {
		return 0, 0, 0, nil, false
	}
	hmacSeq = binary.BigEndian.Uint64(frame[frameFixedLen:])
	mac := hmac.New(sha256.New, key)
	mac.Write(frame[frameFixedLen : frameFixedLen+frameSeqLen])
	mac.Write(frame[:frameFixedLen])
	mac.Write(frame[frameHeadLen:])
	if !hmac.Equal(mac.Sum(nil)[:frameTagLen], frame[frameFixedLen+frameSeqLen:frameHeadLen]) {
		return 0, 0, 0, nil, false
	}
	return typ, streamSeq, hmacSeq, frame[frameHeadLen:], true
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
