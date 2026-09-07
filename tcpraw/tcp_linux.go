//go:build linux

// Package tcpraw is vendored from github.com/xtaci/tcpraw (MIT License,
// see LICENSE in this directory) and maintained locally by udp2faketcp.
//
// Local changes:
//   - cleaner() actually loops (upstream cleaned the flow table only once)
//   - chMessage is buffered to decouple packet capture from readers
//   - RST is sent on Close/CloseFlow and reported via Events on receipt
//   - Dial/WriteTo wait briefly for the flow sequence numbers to be learned,
//     so the first packets carry plausible seq/ack values
package tcpraw

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

var (
	errTimeout = errors.New("timeout")
	expire     = time.Minute
)

// a message from NIC
type message struct {
	bts  []byte
	addr net.Addr
}

// a tcp flow information of a connection pair
type tcpFlow struct {
	conn         *net.TCPConn               // the related system TCP connection of this flow
	handle       *net.IPConn                // the handle to send packets
	seq          uint32                     // TCP sequence number
	ack          uint32                     // TCP acknowledge number
	networkLayer gopacket.SerializableLayer // network layer header for tx
	ts           time.Time                  // last packet incoming time
	buf          gopacket.SerializeBuffer   // a buffer for write
	tcpHeader    layers.TCP
}

// TCPConn defines a TCP-packet oriented connection
type TCPConn struct {
	die     chan struct{}
	dieOnce sync.Once

	// the main golang sockets
	tcpconn  *net.TCPConn     // from net.Dial
	listener *net.TCPListener // from net.Listen

	// handles
	handles []*net.IPConn

	// packets captured from all related NICs will be delivered to this channel
	chMessage chan message

	// addresses of flows that sent us a RST
	chEvent chan net.Addr

	// all TCP flows
	flowTable map[string]*tcpFlow
	flowsLock sync.Mutex

	// iptables
	iptables *iptables.IPTables
	iprule   []string

	ip6tables *iptables.IPTables
	ip6rule   []string

	// deadlines
	readDeadline  atomic.Value
	writeDeadline atomic.Value

	// serialization
	opts gopacket.SerializeOptions
}

// lockflow locks the flow table and apply function `f` to the entry, and create one if not exist
func (conn *TCPConn) lockflow(addr net.Addr, f func(e *tcpFlow)) {
	key := addr.String()
	conn.flowsLock.Lock()
	e := conn.flowTable[key]
	if e == nil { // entry first visit
		e = new(tcpFlow)
		e.ts = time.Now()
		e.buf = gopacket.NewSerializeBuffer()
	}
	f(e)
	conn.flowTable[key] = e
	conn.flowsLock.Unlock()
}

// removeFlow deletes the flow, closes its kernel connection and notifies Events.
func (conn *TCPConn) removeFlow(addr net.Addr) {
	key := addr.String()
	conn.flowsLock.Lock()
	e := conn.flowTable[key]
	if e != nil {
		delete(conn.flowTable, key)
	}
	conn.flowsLock.Unlock()

	if e != nil && e.conn != nil {
		setTTL(e.conn, 64)
		e.conn.Close()
		select {
		case conn.chEvent <- addr:
		default:
		}
	}
}

// waitFlow blocks until the flow's seq/ack numbers have been learned from
// captured handshake packets, or the timeout expires. Without this the first
// outbound packets may carry zeros, which stateful middleboxes can drop.
func (conn *TCPConn) waitFlow(addr net.Addr, timeout time.Duration) {
	key := addr.String()
	deadline := time.Now().Add(timeout)
	for {
		conn.flowsLock.Lock()
		e := conn.flowTable[key]
		learned := e != nil && e.seq != 0 && e.ack != 0
		conn.flowsLock.Unlock()
		if learned || time.Now().After(deadline) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// clean expired flows
func (conn *TCPConn) cleaner() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-conn.die:
			return
		case <-ticker.C:
			conn.flowsLock.Lock()
			for k, v := range conn.flowTable {
				if time.Since(v.ts) > expire {
					if v.conn != nil {
						setTTL(v.conn, 64)
						v.conn.Close()
					}
					delete(conn.flowTable, k)
				}
			}
			conn.flowsLock.Unlock()
		}
	}
}

// captureFlow capture every inbound packets based on rules of BPF
func (conn *TCPConn) captureFlow(handle *net.IPConn, port int) {
	buf := make([]byte, 2048)
	opt := gopacket.DecodeOptions{NoCopy: true, Lazy: true}
	for {
		n, addr, err := handle.ReadFromIP(buf)
		if err != nil {
			return
		}

		// try decoding TCP frame from buf[:n]
		packet := gopacket.NewPacket(buf[:n], layers.LayerTypeTCP, opt)
		transport := packet.TransportLayer()
		tcp, ok := transport.(*layers.TCP)
		if !ok {
			continue
		}

		// port filtering
		if int(tcp.DstPort) != port {
			continue
		}

		// address building
		var src net.TCPAddr
		src.IP = addr.IP
		src.Port = int(tcp.SrcPort)

		var orphan bool
		// flow maintaince
		conn.lockflow(&src, func(e *tcpFlow) {
			if e.conn == nil { // make sure it's related to net.TCPConn
				orphan = true // mark as orphan if it's not related net.TCPConn
			}

			// to keep track of TCP header related to this source
			e.ts = time.Now()
			if tcp.ACK {
				e.seq = tcp.Ack
			}
			if tcp.SYN {
				e.ack = tcp.Seq + 1
			}
			if tcp.PSH {
				if e.ack == tcp.Seq {
					e.ack = tcp.Seq + uint32(len(tcp.Payload))
				}
			}
			e.handle = handle
		})

		if orphan {
			continue
		}

		switch {
		case tcp.RST:
			// the peer (or a middlebox) tore the connection down
			conn.removeFlow(&src)
		case tcp.PSH:
			// push data if it's not orphan
			payload := make([]byte, len(tcp.Payload))
			copy(payload, tcp.Payload)
			select {
			case conn.chMessage <- message{payload, &src}:
			case <-conn.die:
				return
			}
		}
	}
}

// ReadFrom implements the PacketConn ReadFrom method.
func (conn *TCPConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	var timer *time.Timer
	var deadline <-chan time.Time
	if d, ok := conn.readDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer = time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case <-deadline:
		return 0, nil, errTimeout
	case <-conn.die:
		return 0, nil, io.EOF
	case packet := <-conn.chMessage:
		n = copy(p, packet.bts)
		return n, packet.addr, nil
	}
}

// writeFlowPacket builds and sends a single TCP packet for the flow.
// A RST packet carries no payload and does not advance the flow seq.
func (conn *TCPConn) writeFlowPacket(e *tcpFlow, raddr *net.TCPAddr, p []byte, rst bool) error {
	var lport int
	if conn.tcpconn != nil {
		lport = conn.tcpconn.LocalAddr().(*net.TCPAddr).Port
	} else {
		lport = conn.listener.Addr().(*net.TCPAddr).Port
	}

	// build tcp header with local and remote port
	e.tcpHeader.SrcPort = layers.TCPPort(lport)
	e.tcpHeader.DstPort = layers.TCPPort(raddr.Port)
	binary.Read(rand.Reader, binary.LittleEndian, &e.tcpHeader.Window)
	e.tcpHeader.Window |= 0x8000 // make sure it's larger than 32768
	e.tcpHeader.Ack = e.ack
	e.tcpHeader.Seq = e.seq
	e.tcpHeader.PSH = !rst
	e.tcpHeader.ACK = true
	e.tcpHeader.RST = rst

	// build IP header with src & dst ip for TCP checksum
	if raddr.IP.To4() != nil {
		ip := &layers.IPv4{
			Protocol: layers.IPProtocolTCP,
			SrcIP:    e.handle.LocalAddr().(*net.IPAddr).IP.To4(),
			DstIP:    raddr.IP.To4(),
		}
		e.tcpHeader.SetNetworkLayerForChecksum(ip)
	} else {
		ip := &layers.IPv6{
			NextHeader: layers.IPProtocolTCP,
			SrcIP:      e.handle.LocalAddr().(*net.IPAddr).IP.To16(),
			DstIP:      raddr.IP.To16(),
		}
		e.tcpHeader.SetNetworkLayerForChecksum(ip)
	}

	e.buf.Clear()
	var err error
	if rst {
		err = gopacket.SerializeLayers(e.buf, conn.opts, &e.tcpHeader)
	} else {
		err = gopacket.SerializeLayers(e.buf, conn.opts, &e.tcpHeader, gopacket.Payload(p))
	}
	if err != nil {
		return err
	}
	if conn.tcpconn != nil {
		_, err = e.handle.Write(e.buf.Bytes())
	} else {
		_, err = e.handle.WriteToIP(e.buf.Bytes(), &net.IPAddr{IP: raddr.IP})
	}
	return err
}

// WriteTo implements the PacketConn WriteTo method.
func (conn *TCPConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	var deadline <-chan time.Time
	if d, ok := conn.writeDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case <-deadline:
		return 0, errTimeout
	case <-conn.die:
		return 0, io.EOF
	default:
		raddr, err := net.ResolveTCPAddr("tcp", addr.String())
		if err != nil {
			return 0, err
		}

		// the final ACK of the kernel handshake may not have been captured
		// yet; wait briefly so the first packets carry a plausible seq
		conn.flowsLock.Lock()
		e := conn.flowTable[addr.String()]
		needWait := e != nil && e.conn != nil && (e.seq == 0 || e.ack == 0)
		conn.flowsLock.Unlock()
		if needWait {
			conn.waitFlow(addr, 50*time.Millisecond)
		}

		conn.lockflow(addr, func(e *tcpFlow) {
			// if the flow doesn't have handle , assume this packet has lost, without notification
			if e.handle == nil {
				n = len(p)
				return
			}
			if err = conn.writeFlowPacket(e, raddr, p, false); err != nil {
				return
			}
			// increase seq in flow
			e.seq += uint32(len(p))
			n = len(p)
		})
	}
	return
}

// CloseFlow sends a RST to the given peer, closes its kernel connection and
// removes the flow. It lets the peer and middleboxes tear down immediately
// instead of waiting for a timeout.
func (conn *TCPConn) CloseFlow(addr net.Addr) error {
	key := addr.String()
	conn.flowsLock.Lock()
	defer conn.flowsLock.Unlock()

	e := conn.flowTable[key]
	if e == nil {
		return nil
	}
	delete(conn.flowTable, key)

	if e.handle != nil {
		if raddr, err := net.ResolveTCPAddr("tcp", key); err == nil {
			conn.writeFlowPacket(e, raddr, nil, true) // best effort
		}
	}
	if e.conn != nil {
		setTTL(e.conn, 64)
		e.conn.Close()
	}
	return nil
}

// Events returns a channel reporting the addresses of flows that sent a RST,
// i.e. connections the peer wants torn down immediately.
func (conn *TCPConn) Events() <-chan net.Addr {
	return conn.chEvent
}

// Close closes the connection.
func (conn *TCPConn) Close() error {
	var err error
	conn.dieOnce.Do(func() {
		// signal closing
		close(conn.die)

		// RST every known flow, so peers and middleboxes can tear down now
		conn.flowsLock.Lock()
		for key, e := range conn.flowTable {
			if e.handle != nil {
				if raddr, rerr := net.ResolveTCPAddr("tcp", key); rerr == nil {
					conn.writeFlowPacket(e, raddr, nil, true) // best effort
				}
			}
			if e.conn != nil {
				setTTL(e.conn, 64)
				e.conn.Close()
			}
			delete(conn.flowTable, key)
		}
		conn.flowsLock.Unlock()

		if conn.listener != nil {
			err = conn.listener.Close() // server
		}

		// close handles
		for k := range conn.handles {
			conn.handles[k].Close()
		}

		// delete iptable
		if conn.iptables != nil {
			conn.iptables.Delete("filter", "OUTPUT", conn.iprule...)
		}
		if conn.ip6tables != nil {
			conn.ip6tables.Delete("filter", "OUTPUT", conn.ip6rule...)
		}
	})
	return err
}

// LocalAddr returns the local network address.
func (conn *TCPConn) LocalAddr() net.Addr {
	if conn.tcpconn != nil {
		return conn.tcpconn.LocalAddr()
	} else if conn.listener != nil {
		return conn.listener.Addr()
	}
	return nil
}

// SetDeadline implements the Conn SetDeadline method.
func (conn *TCPConn) SetDeadline(t time.Time) error {
	if err := conn.SetReadDeadline(t); err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

// SetReadDeadline implements the Conn SetReadDeadline method.
func (conn *TCPConn) SetReadDeadline(t time.Time) error {
	conn.readDeadline.Store(t)
	return nil
}

// SetWriteDeadline implements the Conn SetWriteDeadline method.
func (conn *TCPConn) SetWriteDeadline(t time.Time) error {
	conn.writeDeadline.Store(t)
	return nil
}

// SetDSCP sets the 6bit DSCP field in IPv4 header, or 8bit Traffic Class in IPv6 header.
func (conn *TCPConn) SetDSCP(dscp int) error {
	for k := range conn.handles {
		if err := setDSCP(conn.handles[k], dscp); err != nil {
			return err
		}
	}
	return nil
}

// SetReadBuffer sets the size of the operating system's receive buffer associated with the connection.
func (conn *TCPConn) SetReadBuffer(bytes int) error {
	var err error
	for k := range conn.handles {
		if err := conn.handles[k].SetReadBuffer(bytes); err != nil {
			return err
		}
	}
	return err
}

// SetWriteBuffer sets the size of the operating system's transmit buffer associated with the connection.
func (conn *TCPConn) SetWriteBuffer(bytes int) error {
	var err error
	for k := range conn.handles {
		if err := conn.handles[k].SetWriteBuffer(bytes); err != nil {
			return err
		}
	}
	return err
}

// Dial connects to the remote TCP port,
// and returns a single packet-oriented connection
func Dial(network, address string) (*TCPConn, error) {
	// remote address resolve
	raddr, err := net.ResolveTCPAddr(network, address)
	if err != nil {
		return nil, err
	}

	// AF_INET
	handle, err := net.DialIP("ip:tcp", nil, &net.IPAddr{IP: raddr.IP})
	if err != nil {
		return nil, err
	}

	// create an established tcp connection
	// will hack this tcp connection for packet transmission
	tcpconn, err := net.DialTCP(network, nil, raddr)
	if err != nil {
		return nil, err
	}

	// fields
	conn := new(TCPConn)
	conn.die = make(chan struct{})
	conn.flowTable = make(map[string]*tcpFlow)
	conn.tcpconn = tcpconn
	conn.chMessage = make(chan message, 512)
	conn.chEvent = make(chan net.Addr, 64)
	conn.lockflow(tcpconn.RemoteAddr(), func(e *tcpFlow) { e.conn = tcpconn })
	conn.handles = append(conn.handles, handle)
	conn.opts = gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}
	go conn.captureFlow(handle, tcpconn.LocalAddr().(*net.TCPAddr).Port)
	go conn.cleaner()

	// the SYN-ACK may not have been captured yet; wait for the flow's
	// seq/ack to be learned before handing the connection to the caller
	conn.waitFlow(tcpconn.RemoteAddr(), 200*time.Millisecond)

	// iptables
	err = setTTL(tcpconn, 1)
	if err != nil {
		return nil, err
	}

	if ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4); err == nil {
		rule := []string{"-m", "ttl", "--ttl-eq", "1", "-p", "tcp", "-d", raddr.IP.String(), "--dport", fmt.Sprint(raddr.Port), "-j", "DROP"}
		if exists, err := ipt.Exists("filter", "OUTPUT", rule...); err == nil {
			if !exists {
				if err = ipt.Append("filter", "OUTPUT", rule...); err == nil {
					conn.iprule = rule
					conn.iptables = ipt
				}
			}
		}
	}
	if ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv6); err == nil {
		rule := []string{"-m", "hl", "--hl-eq", "1", "-p", "tcp", "-d", raddr.IP.String(), "--dport", fmt.Sprint(raddr.Port), "-j", "DROP"}
		if exists, err := ipt.Exists("filter", "OUTPUT", rule...); err == nil {
			if !exists {
				if err = ipt.Append("filter", "OUTPUT", rule...); err == nil {
					conn.ip6rule = rule
					conn.ip6tables = ipt
				}
			}
		}
	}

	// discard everything
	go io.Copy(io.Discard, tcpconn)

	return conn, nil
}

// Listen acts like net.ListenTCP,
// and returns a single packet-oriented connection
func Listen(network, address string) (*TCPConn, error) {
	// fields
	conn := new(TCPConn)
	conn.flowTable = make(map[string]*tcpFlow)
	conn.die = make(chan struct{})
	conn.chMessage = make(chan message, 512)
	conn.chEvent = make(chan net.Addr, 64)
	conn.opts = gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}

	// resolve address
	laddr, err := net.ResolveTCPAddr(network, address)
	if err != nil {
		return nil, err
	}

	// AF_INET
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	if laddr.IP == nil || laddr.IP.IsUnspecified() { // if address is not specified, capture on all ifaces
		var lasterr error
		for _, iface := range ifaces {
			if addrs, err := iface.Addrs(); err == nil {
				for _, addr := range addrs {
					if ipaddr, ok := addr.(*net.IPNet); ok {
						if handle, err := net.ListenIP("ip:tcp", &net.IPAddr{IP: ipaddr.IP}); err == nil {
							conn.handles = append(conn.handles, handle)
							go conn.captureFlow(handle, laddr.Port)
						} else {
							lasterr = err
						}
					}
				}
			}
		}
		if len(conn.handles) == 0 {
			return nil, lasterr
		}
	} else {
		if handle, err := net.ListenIP("ip:tcp", &net.IPAddr{IP: laddr.IP}); err == nil {
			conn.handles = append(conn.handles, handle)
			go conn.captureFlow(handle, laddr.Port)
		} else {
			return nil, err
		}
	}

	// start listening
	l, err := net.ListenTCP(network, laddr)
	if err != nil {
		return nil, err
	}

	conn.listener = l

	// start cleaner
	go conn.cleaner()

	// iptables drop packets marked with TTL = 1
	// TODO: what if iptables is not available, the next hop will send back ICMP Time Exceeded,
	// is this still an acceptable behavior?
	if ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4); err == nil {
		rule := []string{"-m", "ttl", "--ttl-eq", "1", "-p", "tcp", "--sport", fmt.Sprint(laddr.Port), "-j", "DROP"}
		if exists, err := ipt.Exists("filter", "OUTPUT", rule...); err == nil {
			if !exists {
				if err = ipt.Append("filter", "OUTPUT", rule...); err == nil {
					conn.iprule = rule
					conn.iptables = ipt
				}
			}
		}
	}
	if ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv6); err == nil {
		rule := []string{"-m", "hl", "--hl-eq", "1", "-p", "tcp", "--sport", fmt.Sprint(laddr.Port), "-j", "DROP"}
		if exists, err := ipt.Exists("filter", "OUTPUT", rule...); err == nil {
			if !exists {
				if err = ipt.Append("filter", "OUTPUT", rule...); err == nil {
					conn.ip6rule = rule
					conn.ip6tables = ipt
				}
			}
		}
	}

	// discard everything in original connection
	go func() {
		for {
			tcpconn, err := l.AcceptTCP()
			if err != nil {
				return
			}

			// if we cannot set TTL = 1, the only thing reasonable is panic
			if err := setTTL(tcpconn, 1); err != nil {
				panic(err)
			}

			// record net.Conn
			conn.lockflow(tcpconn.RemoteAddr(), func(e *tcpFlow) { e.conn = tcpconn })

			// discard everything
			go io.Copy(io.Discard, tcpconn)
		}
	}()

	return conn, nil
}

// setTTL sets the Time-To-Live field on a given connection
func setTTL(c *net.TCPConn, ttl int) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	addr := c.LocalAddr().(*net.TCPAddr)

	if addr.IP.To4() == nil {
		raw.Control(func(fd uintptr) {
			err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_UNICAST_HOPS, ttl)
		})
	} else {
		raw.Control(func(fd uintptr) {
			err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
		})
	}
	return err
}

// setDSCP sets the 6bit DSCP field in IPv4 header, or 8bit Traffic Class in IPv6 header.
func setDSCP(c *net.IPConn, dscp int) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	addr := c.LocalAddr().(*net.IPAddr)

	if addr.IP.To4() == nil {
		raw.Control(func(fd uintptr) {
			err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_TCLASS, dscp)
		})
	} else {
		raw.Control(func(fd uintptr) {
			err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TOS, dscp<<2)
		})
	}
	return err
}
