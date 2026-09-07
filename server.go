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

var udpConnections sync.Map
var udpLock sync.Mutex
var udpConnCount atomic.Int64

type serverPeer struct {
	conn      *tcpraw.TCPConn // the shared listener
	udp       *net.UDPConn
	tcpAddr   net.Addr
	seq       atomic.Uint64
	replay    replayWindow
	authed    atomic.Bool
	lastData  atomic.Int64
	done      chan struct{}
	closeOnce sync.Once
}

func newServerPeer(conn *tcpraw.TCPConn, udp *net.UDPConn, tcpAddr net.Addr) *serverPeer {
	p := &serverPeer{
		conn:    conn,
		udp:     udp,
		tcpAddr: tcpAddr,
		done:    make(chan struct{}),
	}
	p.authed.Store(AUTH_KEY == nil)
	p.lastData.Store(time.Now().UnixNano())
	return p
}

func (p *serverPeer) nextSeq() uint64 {
	return p.seq.Add(1)
}

func (p *serverPeer) sendFrame(typ byte, payload []byte) error {
	frame := encodeFrame(make([]byte, 0, frameHeadLen+len(payload)), AUTH_KEY, p.nextSeq(), typ, payload)
	p.conn.SetWriteDeadline(time.Now().Add(UDP_TTL))
	_, err := p.conn.WriteTo(frame, p.tcpAddr)
	return err
}

func (p *serverPeer) close() {
	p.closeOnce.Do(func() {
		close(p.done)
		p.conn.CloseFlow(p.tcpAddr) // sends RST, so the client tears down immediately
		p.udp.Close()
		udpConnections.CompareAndDelete(p.tcpAddr.String(), p)
		udpConnCount.Add(-1)
	})
}

// handleBackend forwards replies from the backend UDP service to the client.
func (p *serverPeer) handleBackend() {
	defer p.close()
	budget := payloadBudget()
	buffer := make([]byte, budget+1) // see client.go for the extra byte
	frameBuf := make([]byte, 0, MAX_PACKET_LEN)
	for {
		p.udp.SetReadDeadline(time.Now().Add(readTimeout))
		length, err := p.udp.Read(buffer)
		if err != nil {
			if err != io.EOF {
				debugLogln("Error reading from UDP:", err)
			}
			return
		}
		if length > budget {
			mtuWarn()
			continue
		}
		p.lastData.Store(time.Now().UnixNano())
		frame := encodeFrame(frameBuf[:0], AUTH_KEY, p.nextSeq(), frameData, buffer[:length])
		p.conn.SetWriteDeadline(time.Now().Add(UDP_TTL))
		if _, err := p.conn.WriteTo(frame, p.tcpAddr); err != nil {
			debugLogln("Error writing to RAWTCP:", err)
			return
		}
	}
}

// watchdog sends heartbeats so the client can detect a dead server, and
// reaps the connection when it goes idle.
func (p *serverPeer) watchdog() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, p.lastData.Load())) > UDP_TTL {
				debugLogln("Connection idle timeout:", p.tcpAddr.String())
				p.close()
				return
			}
			if err := p.sendFrame(frameHeartbeat, nil); err != nil {
				debugLogln("Error sending heartbeat:", err)
				p.close()
				return
			}
		}
	}
}

func Server(localAddr string, remoteAddr string) {
	udpAddr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		log.Println("Error resolving UDP address:", err)
		return
	}

	conn, err := tcpraw.Listen("tcp", localAddr)
	if err != nil {
		log.Println("Error listening TCP:", err)
		return
	}
	defer conn.Close()
	setBuffers(conn)

	// tear down peers whose fake-TCP connection was reset
	go func() {
		for addr := range conn.Events() {
			debugLogln("Connection reset by client:", addr.String())
			if val, ok := udpConnections.Load(addr.String()); ok {
				val.(*serverPeer).close()
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
				typ, seq, payload, ok := decodeFrame(AUTH_KEY, buffer[:length])
				if !ok {
					debugLogln("Invalid frame from RAWTCP")
					continue
				}
				key := tcpAddr.String()
				val, exists := udpConnections.Load(key)
				if !exists {
					if AUTH_KEY != nil && typ != frameHandshake {
						// only handshake frames may create a peer
						debugLogln("Frame before handshake from", key)
						continue
					}
					udpLock.Lock()
					if val, exists = udpConnections.Load(key); !exists {
						if udpConnCount.Load() >= maxPeers {
							debugLogln("Too many connections, dropping:", key)
							udpLock.Unlock()
							conn.CloseFlow(tcpAddr)
							continue
						}
						log.Println("New TCP client:", key)
						udpConn, err := net.DialUDP("udp", nil, udpAddr)
						if err != nil {
							debugLogln("Error dialing UDP:", err)
							udpLock.Unlock()
							continue
						}
						setBuffers(udpConn)
						peer := newServerPeer(conn, udpConn, tcpAddr)
						udpConnections.Store(key, peer)
						udpConnCount.Add(1)
						val = peer
						udpLock.Unlock()
						go peer.handleBackend()
						go peer.watchdog()
					} else {
						udpLock.Unlock()
					}
				}
				peer := val.(*serverPeer)
				switch typ {
				case frameHandshake:
					if AUTH_KEY != nil && !peer.authed.Swap(true) {
						// newly authenticated; confirm so the client
						// stops retransmitting the handshake
						peer.sendFrame(frameHeartbeat, nil)
						log.Println("Client authenticated:", key)
					}
				case frameData:
					if !peer.authed.Load() {
						debugLogln("Data before handshake from", key)
						continue
					}
					if AUTH_KEY != nil && !peer.replay.check(seq) {
						debugLogln("Replayed frame dropped")
						continue
					}
					peer.lastData.Store(time.Now().UnixNano())
					if _, err := peer.udp.Write(payload); err != nil {
						debugLogln("Error writing to UDP:", err)
						peer.close()
						continue
					}
					debugLogln("Wrote", len(payload), "bytes to", key)
				case frameHeartbeat:
					// keepalive only
				default:
					debugLogln("Unknown frame type:", typ)
				}
			}
		}()
	}
	select {}
}
