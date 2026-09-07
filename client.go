package udp2faketcp

import (
	"crypto/rand"
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
	readTimeout       = 15 * time.Second
	maxPeers          = 1024
)

var tcpConnections sync.Map
var tcpLock sync.Mutex
var tcpConnCount atomic.Int64

type clientPeer struct {
	conn      *tcpraw.TCPConn
	udpConn   *net.UDPConn
	udpAddr   *net.UDPAddr
	tcpAddr   *net.TCPAddr
	nonce     []byte
	seq       atomic.Uint64
	replay    replayWindow
	lastData  atomic.Int64
	gotReply  atomic.Bool
	done      chan struct{}
	closeOnce sync.Once
}

func newClientPeer(conn *tcpraw.TCPConn, udpConn *net.UDPConn, udpAddr *net.UDPAddr, tcpAddr *net.TCPAddr) *clientPeer {
	p := &clientPeer{
		conn:    conn,
		udpConn: udpConn,
		udpAddr: udpAddr,
		tcpAddr: tcpAddr,
		nonce:   make([]byte, handshakeLen),
		done:    make(chan struct{}),
	}
	rand.Read(p.nonce)
	p.lastData.Store(time.Now().UnixNano())
	return p
}

func (p *clientPeer) nextSeq() uint64 {
	return p.seq.Add(1)
}

// sendFrame encodes and writes a single frame. Low-frequency control frames
// allocate; the data path uses a per-goroutine scratch buffer instead.
func (p *clientPeer) sendFrame(typ byte, payload []byte) error {
	frame := encodeFrame(make([]byte, 0, frameHeadLen+len(payload)), AUTH_KEY, p.nextSeq(), typ, payload)
	p.conn.SetWriteDeadline(time.Now().Add(UDP_TTL))
	_, err := p.conn.WriteTo(frame, p.tcpAddr)
	return err
}

func (p *clientPeer) close() {
	p.closeOnce.Do(func() {
		close(p.done)
		p.conn.Close() // sends RST, so the server tears down immediately
		tcpConnections.CompareAndDelete(p.udpAddr.String(), p)
		tcpConnCount.Add(-1)
	})
}

// handleRemote forwards frames from the fake-TCP side back to the UDP client.
func (p *clientPeer) handleRemote() {
	defer p.close()
	buffer := make([]byte, MAX_PACKET_LEN)
	for {
		p.conn.SetReadDeadline(time.Now().Add(readTimeout))
		length, _, err := p.conn.ReadFrom(buffer)
		if err != nil {
			if err != io.EOF {
				debugLogln("Error reading from RAWTCP:", err)
			}
			return
		}
		typ, seq, payload, ok := decodeFrame(AUTH_KEY, buffer[:length])
		if !ok {
			debugLogln("Invalid frame from RAWTCP")
			continue
		}
		p.gotReply.Store(true)
		switch typ {
		case frameData:
			if AUTH_KEY != nil && !p.replay.check(seq) {
				debugLogln("Replayed frame dropped")
				continue
			}
			p.lastData.Store(time.Now().UnixNano())
			if _, err := p.udpConn.WriteToUDP(payload, p.udpAddr); err != nil {
				debugLogln("Error writing to UDP:", err)
				return
			}
		case frameHeartbeat, frameHandshake:
			// keepalive only
		default:
			debugLogln("Unknown frame type:", typ)
		}
	}
}

// watchdog sends heartbeats, retransmits the handshake until the server
// confirms it, and reaps the connection when it goes idle.
func (p *clientPeer) watchdog() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-p.conn.Events():
			debugLogln("Connection reset by server:", p.udpAddr.String())
			p.close()
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, p.lastData.Load())) > UDP_TTL {
				debugLogln("Connection idle timeout:", p.udpAddr.String())
				p.close()
				return
			}
			if AUTH_KEY != nil && !p.gotReply.Load() {
				// the first packets of a flow may be lost; retry the
				// handshake until any valid frame comes back
				if err := p.sendFrame(frameHandshake, p.nonce); err != nil {
					debugLogln("Error sending handshake:", err)
					p.close()
					return
				}
			} else if err := p.sendFrame(frameHeartbeat, nil); err != nil {
				debugLogln("Error sending heartbeat:", err)
				p.close()
				return
			}
		}
	}
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
				val, exists := tcpConnections.Load(key)
				if !exists {
					tcpLock.Lock()
					if val, exists = tcpConnections.Load(key); !exists {
						if tcpConnCount.Load() >= maxPeers {
							debugLogln("Too many connections, dropping:", key)
							tcpLock.Unlock()
							continue
						}
						log.Printf("New UDP client: %s", key)
						tcpConn, err := tcpraw.Dial("tcp", remoteAddr)
						if err != nil {
							debugLogln("Error dialing TCP:", err)
							tcpLock.Unlock()
							continue
						}
						setBuffers(tcpConn)
						peer := newClientPeer(tcpConn, udpConn, addr, tcpAddr)
						tcpConnections.Store(key, peer)
						tcpConnCount.Add(1)
						val = peer
						tcpLock.Unlock()
						go peer.handleRemote()
						go peer.watchdog()
						if AUTH_KEY != nil {
							peer.sendFrame(frameHandshake, peer.nonce)
						}
					} else {
						tcpLock.Unlock()
					}
				}
				peer := val.(*clientPeer)
				peer.lastData.Store(time.Now().UnixNano())
				frame := encodeFrame(frameBuf[:0], AUTH_KEY, peer.nextSeq(), frameData, buffer[:length])
				peer.conn.SetWriteDeadline(time.Now().Add(UDP_TTL))
				if _, err = peer.conn.WriteTo(frame, tcpAddr); err != nil {
					debugLogln("Error writing to TCP:", err)
					peer.close()
					continue
				}
				debugLogln("Wrote", length, "bytes from", key)
			}
		}()
	}
	select {}
}
