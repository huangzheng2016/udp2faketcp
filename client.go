package udp2faketcp

import (
	"crypto/rand"
	"errors"
	"io"
	"log"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/huangzheng2016/udp2faketcp/tcpraw"
)

const (
	heartbeatInterval = time.Second
	// deadTimeout is how long a flow may be silent (no frames, not even
	// heartbeats) before it is declared dead and replaced.
	deadTimeout   = 5 * heartbeatInterval
	maxSessions   = 1024
	redialBackoff = 5 * time.Second
)

var errSessionClosed = errors.New("session closed")

var udpSessions sync.Map // udp source addr -> *clientSession
var sessionLock sync.Mutex
var sessionCount atomic.Int64

// clientFlow is one fake-TCP connection of a session.
type clientFlow struct {
	sess     *clientSession
	conn     *tcpraw.TCPConn
	hmacSeq  atomic.Uint64
	replay   replayWindow
	gotReply atomic.Bool
	dead     atomic.Bool
	done     chan struct{}
	echoTS   atomic.Int64 // last heartbeat timestamp received from the server
	rtt      rttEstimator
	tx       atomic.Uint64
	rx       atomic.Uint64
}

// clientSession groups the flows carrying datagrams of one UDP source.
// Outgoing datagrams are striped across the flows with a session-level
// sequence number; incoming ones pass through the reorder buffer.
type clientSession struct {
	id      [handshakeLen]byte
	remote  string
	udpConn *net.UDPConn
	udpAddr *net.UDPAddr
	tcpAddr *net.TCPAddr

	streamSeq atomic.Uint64
	rr        atomic.Uint64
	reorder   reorderBuffer
	lastData  atomic.Int64

	flowsMu sync.RWMutex
	flows   []*clientFlow

	done      chan struct{}
	closeOnce sync.Once
}

// send writes a control frame (handshake/heartbeat) on this flow.
func (f *clientFlow) send(typ byte, payload []byte) {
	frame := encodeFrame(make([]byte, 0, frameHeadLen+len(payload)), AUTH_KEY, f.hmacSeq.Add(1), 0, typ, payload)
	f.conn.SetWriteDeadline(time.Now().Add(UDP_TTL))
	if _, err := f.conn.WriteTo(frame, f.sess.tcpAddr); err != nil {
		debugLogln("Error writing to RAWTCP:", err)
		f.die()
	}
}

func (f *clientFlow) die() {
	if f.dead.Swap(true) {
		return
	}
	close(f.done)
	f.conn.Close() // sends RST, so the server tears the flow down immediately
	f.sess.removeFlow(f)
}

// handleRemote forwards frames arriving on this flow back to the UDP client.
func (f *clientFlow) handleRemote() {
	buffer := make([]byte, MAX_PACKET_LEN)
	for {
		f.conn.SetReadDeadline(time.Now().Add(deadTimeout))
		length, _, err := f.conn.ReadFrom(buffer)
		if err != nil {
			if err != io.EOF {
				debugLogln("Error reading from RAWTCP:", err)
			}
			break
		}
		typ, streamSeq, hmacSeq, payload, ok := decodeFrame(AUTH_KEY, buffer[:length])
		if !ok {
			debugLogln("Invalid frame from RAWTCP")
			continue
		}
		f.gotReply.Store(true)
		f.rx.Add(1)
		switch typ {
		case frameData:
			if AUTH_KEY != nil && !f.replay.check(hmacSeq) {
				debugLogln("Replayed frame dropped")
				continue
			}
			f.sess.lastData.Store(time.Now().UnixNano())
			f.sess.deliver(f.sess.reorder.push(streamSeq, payload))
		case frameHeartbeat:
			if ts, _, ok := parseHeartbeat(payload); ok {
				f.echoTS.Store(ts)
				// answer promptly so the peer's RTT estimate is not
				// quantized by the heartbeat period
				f.send(framePong, buildHeartbeat(&f.echoTS))
			}
			// nudge the reorder buffer so a stalled tail is not held back
			f.sess.deliver(f.sess.reorder.expire())
		case framePong:
			if _, echo, ok := parseHeartbeat(payload); ok && echo > 0 {
				f.rtt.add(time.Now().UnixNano() - echo)
			}
		default:
			debugLogln("Unknown frame type:", typ)
		}
	}
	f.die()
}

// watchdog keeps the flow alive with heartbeats and retransmits the
// handshake until the server confirms it.
func (f *clientFlow) watchdog() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-f.sess.done:
			return
		case <-f.conn.Events():
			debugLogln("Flow reset by server:", f.sess.udpAddr.String())
			f.die()
			return
		case <-ticker.C:
			if f.gotReply.Load() {
				f.send(frameHeartbeat, buildHeartbeat(&f.echoTS))
			} else {
				f.send(frameHandshake, f.sess.id[:])
			}
		}
	}
}

func (s *clientSession) deliver(payloads [][]byte) {
	for _, p := range payloads {
		if _, err := s.udpConn.WriteToUDP(p, s.udpAddr); err != nil {
			debugLogln("Error writing to UDP:", err)
			s.close()
			return
		}
	}
}

// pickFlow round-robins over the flows, preferring ones whose handshake
// the server has already confirmed.
func (s *clientSession) pickFlow() *clientFlow {
	s.flowsMu.RLock()
	defer s.flowsMu.RUnlock()
	n := len(s.flows)
	if n == 0 {
		return nil
	}
	var first *clientFlow
	for i := 0; i < n; i++ {
		f := s.flows[int(s.rr.Add(1))%n]
		if first == nil {
			first = f
		}
		if f.gotReply.Load() {
			return f
		}
	}
	return first
}

// removeFlow drops f from the flow list (copy-on-write, senders hold RLock).
func (s *clientSession) removeFlow(f *clientFlow) {
	s.flowsMu.Lock()
	flows := make([]*clientFlow, 0, len(s.flows))
	for _, x := range s.flows {
		if x != f {
			flows = append(flows, x)
		}
	}
	s.flows = flows
	s.flowsMu.Unlock()
}

func (s *clientSession) addFlow() error {
	select {
	case <-s.done:
		return errSessionClosed
	default:
	}
	conn, err := tcpraw.Dial("tcp", s.remote)
	if err != nil {
		return err
	}
	select {
	case <-s.done:
		conn.Close()
		return errSessionClosed
	default:
	}
	setBuffers(conn)
	f := &clientFlow{sess: s, conn: conn, done: make(chan struct{})}
	s.flowsMu.Lock()
	s.flows = append(append([]*clientFlow{}, s.flows...), f)
	s.flowsMu.Unlock()
	go f.handleRemote()
	go f.watchdog()
	f.send(frameHandshake, s.id[:])
	return nil
}

// fillFlows dials flows until the session reaches FLOWS, with backoff on
// failure. Runs once at session creation so all flows come up at once;
// maintain() only replaces flows that die later.
func (s *clientSession) fillFlows() {
	for {
		s.flowsMu.RLock()
		n := len(s.flows)
		s.flowsMu.RUnlock()
		if n >= FLOWS {
			return
		}
		if err := s.addFlow(); err != nil {
			if errors.Is(err, errSessionClosed) {
				return
			}
			debugLogln("Error dialing flow:", err)
			select {
			case <-s.done:
				return
			case <-time.After(redialBackoff):
			}
		}
	}
}

// maintain keeps the flow count at FLOWS, reaps idle sessions and dumps
// per-flow statistics for tuning.
func (s *clientSession) maintain() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	backoff := time.Time{}
	statsTick := 0
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, s.lastData.Load())) > UDP_TTL {
				debugLogln("Session idle timeout:", s.udpAddr.String())
				s.close()
				return
			}
			s.flowsMu.RLock()
			n := len(s.flows)
			s.flowsMu.RUnlock()
			if n < FLOWS && time.Now().After(backoff) {
				if err := s.addFlow(); err != nil && !errors.Is(err, errSessionClosed) {
					debugLogln("Error dialing flow:", err)
					backoff = time.Now().Add(redialBackoff)
				}
			}
			statsTick++
			if statsTick >= 10 {
				statsTick = 0
				s.logStats()
			}
			s.tuneReorder()
		}
	}
}

// tuneReorder adapts the reorder gap-wait to the worst flow RTT estimate.
func (s *clientSession) tuneReorder() {
	var max time.Duration
	s.flowsMu.RLock()
	for _, f := range s.flows {
		if d := f.rtt.timeout(); d > max {
			max = d
		}
	}
	s.flowsMu.RUnlock()
	if max <= 0 {
		return // no estimate yet, keep the default
	}
	// safety margin: the gap-wait must comfortably exceed the inter-flow
	// skew, not sit exactly at it (MLVPN uses the same x2.2 factor)
	max = max * 11 / 5
	if max < reorderMinDelay {
		max = reorderMinDelay
	}
	if max > reorderMaxDelayCap {
		max = reorderMaxDelayCap
	}
	s.reorder.setMaxDelay(max)
}

func (s *clientSession) logStats() {
	if !DEBUG {
		return
	}
	s.flowsMu.RLock()
	tx := make([]uint64, 0, len(s.flows))
	rx := make([]uint64, 0, len(s.flows))
	rtt := make([]time.Duration, 0, len(s.flows))
	for _, f := range s.flows {
		tx = append(tx, f.tx.Load())
		rx = append(rx, f.rx.Load())
		rtt = append(rtt, f.rtt.timeout().Round(time.Millisecond))
	}
	s.flowsMu.RUnlock()
	ooo, late := s.reorder.stats()
	log.Printf("session %s: flows=%d tx=%v rx=%v rtt=%v reorder_ooo=%d reorder_late=%d",
		s.udpAddr.String(), len(tx), tx, rx, rtt, ooo, late)
}

func (s *clientSession) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.flowsMu.Lock()
		flows := s.flows
		s.flows = nil
		s.flowsMu.Unlock()
		for _, f := range flows {
			f.die()
		}
		udpSessions.CompareAndDelete(s.udpAddr.String(), s)
		sessionCount.Add(-1)
	})
}

func Client(localAddr string, remoteAddr string) {
	udpAddr, err := net.ResolveUDPAddr("udp", localAddr)
	if err != nil {
		log.Println("Error resolving UDP address:", err)
		return
	}
	tcpAddr, err := net.ResolveTCPAddr("tcp", remoteAddr)
	if err != nil {
		log.Println("Error resolving TCP address:", err)
		return
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Println("Error listening UDP:", err)
		return
	}
	defer udpConn.Close()
	setBuffers(udpConn)

	budget := payloadBudget()
	cpuCores := runtime.NumCPU()
	for i := 0; i < cpuCores; i++ {
		go func() {
			// one extra byte so truncated datagrams can be told apart
			// from ones that exactly fill the budget
			buffer := make([]byte, budget+1)
			frameBuf := make([]byte, 0, MAX_PACKET_LEN)
			for {
				length, addr, err := udpConn.ReadFromUDP(buffer)
				if err != nil {
					debugLogln("Error reading from UDP:", err)
					continue
				}
				if length > budget {
					mtuWarn()
					continue
				}
				key := addr.String()
				val, exists := udpSessions.Load(key)
				if !exists {
					sessionLock.Lock()
					if val, exists = udpSessions.Load(key); !exists {
						if sessionCount.Load() >= maxSessions {
							debugLogln("Too many sessions, dropping:", key)
							sessionLock.Unlock()
							continue
						}
						log.Printf("New UDP client: %s", key)
						sess := &clientSession{
							remote:  remoteAddr,
							udpConn: udpConn,
							udpAddr: addr,
							tcpAddr: tcpAddr,
							done:    make(chan struct{}),
						}
						rand.Read(sess.id[:])
						sess.lastData.Store(time.Now().UnixNano())
						udpSessions.Store(key, sess)
						sessionCount.Add(1)
						val = sess
						sessionLock.Unlock()
						go sess.maintain()
						// the first flow comes up inline, the rest concurrently
						if err := sess.addFlow(); err != nil {
							debugLogln("Error dialing TCP:", err)
						}
						go sess.fillFlows()
					} else {
						sessionLock.Unlock()
					}
				}
				sess := val.(*clientSession)
				sess.lastData.Store(time.Now().UnixNano())
				f := sess.pickFlow()
				if f == nil {
					continue // flows are (re)establishing; drop, UDP tolerates
				}
				frame := encodeFrame(frameBuf[:0], AUTH_KEY, f.hmacSeq.Add(1), sess.streamSeq.Add(1), frameData, buffer[:length])
				f.conn.SetWriteDeadline(time.Now().Add(UDP_TTL))
				if _, err = f.conn.WriteTo(frame, tcpAddr); err != nil {
					debugLogln("Error writing to TCP:", err)
					f.die()
					continue
				}
				f.tx.Add(1)
				debugLogln("Wrote", length, "bytes from", key)
			}
		}()
	}
	select {}
}
