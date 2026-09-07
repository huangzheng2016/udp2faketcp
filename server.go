package udp2faketcp

import (
	"io"
	"log"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/huangzheng2016/udp2faketcp/tcpraw"
)

const maxFlows = maxSessions * 32

var flowByAddr sync.Map  // tcp addr -> *serverFlow
var sessionByID sync.Map // session id -> *serverSession
var serverLock sync.Mutex
var serverSessionCount atomic.Int64
var serverFlowCount atomic.Int64
var backendUDPAddr *net.UDPAddr

// serverFlow is one fake-TCP connection of a session, seen from the server.
type serverFlow struct {
	conn    *tcpraw.TCPConn // the shared listener
	tcpAddr net.Addr
	sess    atomic.Pointer[serverSession]
	hmacSeq atomic.Uint64
	replay  replayWindow
	authed  atomic.Bool
	dead    atomic.Bool
	done    chan struct{}
	echoTS  atomic.Int64 // last heartbeat timestamp received from the client
	lastRx  atomic.Int64 // last frame received on this flow
	rtt     rttEstimator
	tx      atomic.Uint64
	rx      atomic.Uint64
}

// serverSession merges all flows of one client session: inbound datagrams
// pass through the reorder buffer, outbound ones are striped across flows.
type serverSession struct {
	id   [handshakeLen]byte
	conn *tcpraw.TCPConn
	udp  *net.UDPConn

	streamSeq atomic.Uint64
	rr        atomic.Uint64
	reorder   reorderBuffer
	lastData  atomic.Int64

	flowsMu sync.RWMutex
	flows   []*serverFlow

	done      chan struct{}
	closeOnce sync.Once
}

// send writes a control frame (heartbeat) on this flow.
func (f *serverFlow) send(typ byte, payload []byte) {
	frame := encodeFrame(make([]byte, 0, frameHeadLen+len(payload)), AUTH_KEY, f.hmacSeq.Add(1), 0, typ, payload)
	f.conn.SetWriteDeadline(time.Now().Add(UDP_TTL))
	if _, err := f.conn.WriteTo(frame, f.tcpAddr); err != nil {
		debugLogln("Error writing to RAWTCP:", err)
		f.die()
	}
}

func (f *serverFlow) die() {
	if f.dead.Swap(true) {
		return
	}
	close(f.done)
	f.conn.CloseFlow(f.tcpAddr) // sends RST, so the client tears the flow down immediately
	flowByAddr.CompareAndDelete(f.tcpAddr.String(), f)
	serverFlowCount.Add(-1)
	if s := f.sess.Load(); s != nil {
		s.removeFlow(f)
	}
}

// watchdog keeps the NAT/conntrack state of this flow alive and declares
// the flow dead when the client goes silent.
func (f *serverFlow) watchdog() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, f.lastRx.Load())) > deadTimeout {
				debugLogln("Flow dead timeout:", f.tcpAddr.String())
				f.die()
				return
			}
			if f.sess.Load() != nil {
				f.send(frameHeartbeat, buildHeartbeat(&f.echoTS))
			}
		}
	}
}

func (s *serverSession) addFlow(f *serverFlow) {
	s.flowsMu.Lock()
	s.flows = append(append([]*serverFlow{}, s.flows...), f)
	s.flowsMu.Unlock()
}

func (s *serverSession) removeFlow(f *serverFlow) {
	s.flowsMu.Lock()
	flows := make([]*serverFlow, 0, len(s.flows))
	for _, x := range s.flows {
		if x != f {
			flows = append(flows, x)
		}
	}
	s.flows = flows
	s.flowsMu.Unlock()
}

func (s *serverSession) pickFlow() *serverFlow {
	s.flowsMu.RLock()
	defer s.flowsMu.RUnlock()
	if len(s.flows) == 0 {
		return nil
	}
	return s.flows[int(s.rr.Add(1))%len(s.flows)]
}

func (s *serverSession) deliver(payloads [][]byte) {
	for _, p := range payloads {
		if _, err := s.udp.Write(p); err != nil {
			debugLogln("Error writing to UDP:", err)
			s.close()
			return
		}
	}
}

// handleBackend stripes replies from the backend UDP service across the
// session's flows. There is deliberately no read deadline: a quiet backend
// does not mean a dead tunnel. The read unblocks when close() closes udp.
func (s *serverSession) handleBackend() {
	budget := payloadBudget()
	buffer := make([]byte, budget+1) // see client.go for the extra byte
	frameBuf := make([]byte, 0, MAX_PACKET_LEN)
	for {
		length, err := s.udp.Read(buffer)
		if err != nil {
			if err != io.EOF {
				debugLogln("Error reading from UDP:", err)
			}
			break
		}
		if length > budget {
			mtuWarn()
			continue
		}
		s.lastData.Store(time.Now().UnixNano())
		f := s.pickFlow()
		if f == nil {
			continue // no flow right now; drop, UDP tolerates
		}
		frame := encodeFrame(frameBuf[:0], AUTH_KEY, f.hmacSeq.Add(1), s.streamSeq.Add(1), frameData, buffer[:length])
		s.conn.SetWriteDeadline(time.Now().Add(UDP_TTL))
		if _, err := s.conn.WriteTo(frame, f.tcpAddr); err != nil {
			debugLogln("Error writing to RAWTCP:", err)
			f.die()
			continue
		}
		f.tx.Add(1)
	}
	s.close()
}

// maintain reaps idle sessions and dumps per-flow statistics for tuning.
func (s *serverSession) maintain() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	statsTick := 0
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, s.lastData.Load())) > UDP_TTL {
				debugLogln("Session idle timeout")
				s.close()
				return
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
func (s *serverSession) tuneReorder() {
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

func (s *serverSession) logStats() {
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
	log.Printf("session flows=%d tx=%v rx=%v rtt=%v reorder_ooo=%d reorder_late=%d",
		len(tx), tx, rx, rtt, ooo, late)
}

func (s *serverSession) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.flowsMu.Lock()
		flows := s.flows
		s.flows = nil
		s.flowsMu.Unlock()
		for _, f := range flows {
			f.die()
		}
		s.udp.Close()
		sessionByID.CompareAndDelete(string(s.id[:]), s)
		serverSessionCount.Add(-1)
	})
}

// attachSession links a freshly handshaked flow to its session, creating
// the session (and its backend UDP connection) on first sight.
func attachSession(f *serverFlow, sid [handshakeLen]byte) *serverSession {
	serverLock.Lock()
	defer serverLock.Unlock()
	if s := f.sess.Load(); s != nil {
		return s
	}
	key := string(sid[:])
	if val, ok := sessionByID.Load(key); ok {
		sess := val.(*serverSession)
		sess.addFlow(f)
		f.sess.Store(sess)
		return sess
	}
	if serverSessionCount.Load() >= maxSessions {
		return nil
	}
	udpConn, err := net.DialUDP("udp", nil, backendUDPAddr)
	if err != nil {
		debugLogln("Error dialing UDP:", err)
		return nil
	}
	setBuffers(udpConn)
	sess := &serverSession{id: sid, udp: udpConn, conn: f.conn, done: make(chan struct{})}
	sess.lastData.Store(time.Now().UnixNano())
	sessionByID.Store(key, sess)
	serverSessionCount.Add(1)
	sess.addFlow(f)
	f.sess.Store(sess)
	log.Println("New session:", f.tcpAddr.String())
	go sess.handleBackend()
	go sess.maintain()
	return sess
}

func Server(localAddr string, remoteAddr string) {
	udpAddr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		log.Println("Error resolving UDP address:", err)
		return
	}
	backendUDPAddr = udpAddr

	conn, err := tcpraw.Listen("tcp", localAddr)
	if err != nil {
		log.Println("Error listening TCP:", err)
		return
	}
	defer conn.Close()
	setBuffers(conn)

	// tear down flows whose fake-TCP connection was reset
	go func() {
		for addr := range conn.Events() {
			debugLogln("Connection reset by client:", addr.String())
			if val, ok := flowByAddr.Load(addr.String()); ok {
				val.(*serverFlow).die()
			}
		}
	}()

	cpuCores := runtime.NumCPU()
	for i := 0; i < cpuCores; i++ {
		go func() {
			buffer := make([]byte, MAX_PACKET_LEN)
			for {
				length, tcpAddr, err := conn.ReadFrom(buffer)
				if err != nil {
					debugLogln("Error reading from RAWTCP:", err)
					continue
				}
				typ, streamSeq, hmacSeq, payload, ok := decodeFrame(AUTH_KEY, buffer[:length])
				if !ok {
					debugLogln("Invalid frame from RAWTCP")
					continue
				}
				key := tcpAddr.String()
				val, exists := flowByAddr.Load(key)
				if !exists {
					if typ != frameHandshake {
						// only handshake frames may create a flow
						debugLogln("Frame before handshake from", key)
						continue
					}
					serverLock.Lock()
					if val, exists = flowByAddr.Load(key); !exists {
						if serverFlowCount.Load() >= maxFlows {
							debugLogln("Too many flows, dropping:", key)
							serverLock.Unlock()
							conn.CloseFlow(tcpAddr)
							continue
						}
						f := &serverFlow{conn: conn, tcpAddr: tcpAddr, done: make(chan struct{})}
						f.lastRx.Store(time.Now().UnixNano())
						flowByAddr.Store(key, f)
						serverFlowCount.Add(1)
						val = f
						serverLock.Unlock()
						log.Println("New TCP client:", key)
						go f.watchdog()
					} else {
						serverLock.Unlock()
					}
				}
				f := val.(*serverFlow)
				f.rx.Add(1)
				f.lastRx.Store(time.Now().UnixNano())
				switch typ {
				case frameHandshake:
					if len(payload) < handshakeLen {
						debugLogln("Short handshake from", key)
						continue
					}
					if AUTH_KEY != nil && !f.authed.Swap(true) {
						log.Println("Client authenticated:", key)
					} else {
						f.authed.Store(true)
					}
					if f.sess.Load() == nil {
						var sid [handshakeLen]byte
						copy(sid[:], payload[:handshakeLen])
						if attachSession(f, sid) == nil {
							debugLogln("Cannot create session for", key)
							f.die()
							continue
						}
					}
					// confirm so the client stops retransmitting the handshake
					f.send(frameHeartbeat, buildHeartbeat(&f.echoTS))
				case frameData:
					sess := f.sess.Load()
					if sess == nil || !f.authed.Load() {
						debugLogln("Data before handshake from", key)
						continue
					}
					if AUTH_KEY != nil && !f.replay.check(hmacSeq) {
						debugLogln("Replayed frame dropped")
						continue
					}
					sess.lastData.Store(time.Now().UnixNano())
					sess.deliver(sess.reorder.push(streamSeq, payload))
				case frameHeartbeat:
					if ts, _, ok := parseHeartbeat(payload); ok {
						f.echoTS.Store(ts)
						// answer promptly, see client.go
						f.send(framePong, buildHeartbeat(&f.echoTS))
					}
					if sess := f.sess.Load(); sess != nil {
						sess.deliver(sess.reorder.expire())
					}
				case framePong:
					if _, echo, ok := parseHeartbeat(payload); ok && echo > 0 {
						f.rtt.add(time.Now().UnixNano() - echo)
					}
				default:
					debugLogln("Unknown frame type:", typ)
				}
			}
		}()
	}
	select {}
}
