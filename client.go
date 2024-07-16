package udp2faketcp

import (
	"github.com/xtaci/tcpraw"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

var tcpConnections sync.Map
var tcpLock sync.Mutex

func Client(localAddr string, remoteAddr string) {
	udpAddr, err := net.ResolveUDPAddr("udp4", localAddr)
	if err != nil {
		log.Println("Error resolving UDP:", err)
		return
	}
	tcpAddr, err := net.ResolveTCPAddr("tcp4", remoteAddr)
	if err != nil {
		log.Println("Error dialing TCP:", err)
		return
	}
	udpConn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		log.Println("Error listen UDP:", err)
		return
	}
	defer udpConn.Close()
	for {
		buffer := make([]byte, MAX_PACKET_LEN)
		length, addr, err := udpConn.ReadFromUDP(buffer)
		if err != nil {
			debugLogln("Error reading from UDP:", err)
			continue
		}
		go func() {
			if _, exists := tcpConnections.Load(addr.String()); !exists {
				tcpLock.Lock()
				defer tcpLock.Unlock()
				if _, exists := tcpConnections.Load(addr.String()); !exists {
					log.Printf("New UDP client: %s", addr.String())
					tcpConn, err := tcpraw.Dial("tcp4", remoteAddr)
					if err != nil {
						debugLogln("Error dialing TCP:", err)
						return
					}
					tcpConn.SetDeadline(time.Now().Add(UDP_TTL))
					if err != nil {
						debugLogln("Error set deadline:", err)
						return
					}
					tcpConnections.Store(addr.String(), tcpConn)
					go handleClientConnection(tcpConn, udpConn, addr)
				}
			}
			if val, ok := tcpConnections.Load(addr.String()); ok {
				tcpConn := val.(*tcpraw.TCPConn)
				_, err = tcpConn.WriteTo(buffer[:length], tcpAddr)
				if err != nil {
					debugLogln("Error writing to TCP:", err)
					tcpConn.Close()
					tcpConnections.Delete(addr.String())
					return
				}
				debugLogln("Wrote", length, "bytes from", addr.String())
			}
		}()

	}
}
func handleClientConnection(conn *tcpraw.TCPConn, udpConn *net.UDPConn, udpAddr *net.UDPAddr) {
	defer conn.Close()
	buffer := make([]byte, MAX_PACKET_LEN)
	for {
		err := conn.SetDeadline(time.Now().Add(UDP_TTL))
		if err != nil {
			debugLogln("Error setting read deadline:", err)
			break
		}
		length, _, err := conn.ReadFrom(buffer)
		if err != nil {
			if err != io.EOF {
				debugLogln("Error reading from RAWTCP:", err)
			}
			break
		}
		_, err = udpConn.WriteToUDP(buffer[:length], udpAddr)
		if err != nil {
			debugLogln("Error writing to UDP:", err)
			break
		}
	}
	tcpConnections.Delete(udpAddr.String())
}
