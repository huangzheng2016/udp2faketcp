package udp2faketcp

import (
	"github.com/xtaci/tcpraw"
	"io"
	"log"
	"net"
	"sync"
)

var udpConnections sync.Map
var udpLock sync.Mutex

func Server(localAddr string, remoteAddr string) {
	conn, err := tcpraw.Listen("tcp4", localAddr)
	if err != nil {
		log.Println("Error dialing TCP:", err)
		return
	}
	defer conn.Close()
	for {
		buffer := make([]byte, MAX_PACKET_LEN)
		length, tcpAddr, err := conn.ReadFrom(buffer)
		if err != nil {
			debugLogln("Error reading from RAWTCP:", err)
			continue
		}
		go func() {
			debugLogln("Read from RAWTCP:", length, tcpAddr.String(), string(buffer[:length]))
			if _, exists := udpConnections.Load(tcpAddr.String()); !exists {
				udpLock.Lock()
				defer udpLock.Unlock()
				if _, exists := udpConnections.Load(tcpAddr.String()); !exists {
					log.Println("New TCP client:", tcpAddr.String())
					udpAddr, err := net.ResolveUDPAddr("udp4", remoteAddr)
					if err != nil {
						debugLogln("Error resolving UDP address:", err)
						return
					}
					udpConn, err := net.DialUDP("udp4", nil, udpAddr)
					if err != nil {
						debugLogln("Error dialing TCP:", err)
						return
					}
					udpConnections.Store(tcpAddr.String(), udpConn)
					go handleServerConnection(udpConn, conn, tcpAddr)
				}
			}
			if val, ok := udpConnections.Load(tcpAddr.String()); ok {
				tcpConn := val.(*net.UDPConn)
				_, err = tcpConn.Write(buffer[:length])
				if err != nil {
					debugLogln("Error writing to TCP:", err)
					tcpConn.Close()
					udpConnections.Delete(tcpAddr.String())
					return
				}
				debugLogln("Wrote", length, "bytes to", tcpAddr.String())
			}
		}()

	}
}
func handleServerConnection(udpConn *net.UDPConn, conn *tcpraw.TCPConn, tcpAddr net.Addr) {
	defer udpConn.Close()
	buffer := make([]byte, MAX_PACKET_LEN)
	for {
		length, err := udpConn.Read(buffer)
		if err != nil {
			if err != io.EOF {
				debugLogln("Error reading from UDP:", err)
			}
			break
		}
		_, err = conn.WriteTo(buffer[:length], tcpAddr)
		if err != nil {
			debugLogln("Error writing to RAWTCP:", err)
			break
		}
	}
	udpConnections.Delete(tcpAddr.String())
}
