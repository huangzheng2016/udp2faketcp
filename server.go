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

var udpConnections sync.Map
var udpLock sync.Mutex

func Server(localAddr string, remoteAddr string) {

	udpAddr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		debugLogln("Error resolving UDP address:", err)
		return
	}

	conn, err := tcpraw.Listen("tcp", localAddr)
	if err != nil {
		log.Println("Error dialing TCP:", err)
		return
	}
	defer conn.Close()

	cpuCores := runtime.NumCPU()
	for i := 0; i < cpuCores; i++ {
		go func() {
			for {
				buffer := make([]byte, MAX_PACKET_LEN)
				length, tcpAddr, err := conn.ReadFrom(buffer)
				if err != nil {
					debugLogln("Error reading from RAWTCP:", err)
					continue
				}
				debugLogln("Read from RAWTCP:", length, tcpAddr.String())
				var val any
				var exists bool
				if val, exists = udpConnections.Load(tcpAddr.String()); !exists {
					udpLock.Lock()
					if val, exists = udpConnections.Load(tcpAddr.String()); !exists {
						log.Println("New TCP client:", tcpAddr.String())
						udpConn, err := net.DialUDP("udp", nil, udpAddr)
						if err != nil {
							debugLogln("Error dialing TCP:", err)
							udpLock.Unlock()
							continue
						}
						udpConnections.Store(tcpAddr.String(), udpConn)
						udpLock.Unlock()
						go handleServerConnection(udpConn, conn, tcpAddr)
					} else {
						udpLock.Unlock()
					}
				}
				if val != nil {
					tcpConn := val.(*net.UDPConn)
					_, err = tcpConn.Write(buffer[:length])
					if err != nil {
						debugLogln("Error writing to TCP:", err)
						tcpConn.Close()
						udpConnections.Delete(tcpAddr.String())
						continue
					}
					debugLogln("Wrote", length, "bytes to", tcpAddr.String())
				}
			}
		}()
	}
	select {}
}
func handleServerConnection(udpConn *net.UDPConn, conn *tcpraw.TCPConn, tcpAddr net.Addr) {
	defer udpConn.Close()
	buffer := make([]byte, MAX_PACKET_LEN)
	for {
		err := udpConn.SetDeadline(time.Now().Add(UDP_TTL))
		if err != nil {
			debugLogln("Error setting read deadline:", err)
			break
		}
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
