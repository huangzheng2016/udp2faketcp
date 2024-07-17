package udp2faketcp

import (
	"github.com/xtaci/tcpraw"
	"io"
	"log"
	"net"
	"runtime"
	"sync"
	"time"
)

var tcpConnections sync.Map
var tcpLock sync.Mutex

func Client(localAddr string, remoteAddr string) {
	udpAddr, err := net.ResolveUDPAddr("udp", localAddr)
	if err != nil {
		log.Println("Error resolving UDP:", err)
		return
	}
	tcpAddr, err := net.ResolveTCPAddr("tcp", remoteAddr)
	if err != nil {
		log.Println("Error dialing TCP:", err)
		return
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Println("Error listen UDP:", err)
		return
	}
	defer udpConn.Close()

	cpuCores := runtime.NumCPU()
	for i := 0; i < cpuCores; i++ {
		go func() {
			for {
				buffer := make([]byte, MAX_PACKET_LEN)
				length, addr, err := udpConn.ReadFromUDP(buffer)
				if err != nil {
					debugLogln("Error reading from UDP:", err)
					continue
				}
				var val any
				var exists bool
				if val, exists = tcpConnections.Load(addr.String()); !exists {
					tcpLock.Lock()
					if val, exists = tcpConnections.Load(addr.String()); !exists {
						log.Printf("New UDP client: %s", addr.String())
						tcpConn, err := tcpraw.Dial("tcp", remoteAddr)
						if err != nil {
							debugLogln("Error dialing TCP:", err)
							tcpLock.Unlock()
							continue
						}
						tcpConn.SetDeadline(time.Now().Add(UDP_TTL))
						if err != nil {
							debugLogln("Error set deadline:", err)
							tcpLock.Unlock()
							continue
						}
						tcpConnections.Store(addr.String(), tcpConn)
						tcpLock.Unlock()
						go handleClientConnection(tcpConn, udpConn, addr)
					} else {
						tcpLock.Unlock()
					}
				}
				if val != nil {
					tcpConn := val.(*tcpraw.TCPConn)
					_, err = tcpConn.WriteTo(buffer[:length], tcpAddr)
					if err != nil {
						debugLogln("Error writing to TCP:", err)
						tcpConn.Close()
						tcpConnections.Delete(addr.String())
						continue
					}
					debugLogln("Wrote", length, "bytes from", addr.String())
				}
			}
		}()
	}
	select {}
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
