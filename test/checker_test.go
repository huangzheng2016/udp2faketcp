package main

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/huangzheng2016/udp2faketcp"
)

func manualTest(t *testing.T) {
	t.Helper()
	if os.Getenv("UDP2FAKETCP_MANUAL") == "" {
		t.Skip("manual test, set UDP2FAKETCP_MANUAL=1 to run")
	}
}

func ulisten(addr string) {
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		fmt.Println("Error resolving:", err.Error())
		return
	}
	conn, err := net.ListenUDP("udp", uaddr)
	if err != nil {
		fmt.Println("Error listening:", err.Error())
		return
	}
	defer conn.Close()
	message := "Hi"
	for {
		buf := make([]byte, 1024)
		n, udpAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Println("Error reading:", err.Error())
			return
		}
		fmt.Println("Received:", string(buf[:n]))
		if _, err := conn.WriteToUDP([]byte(message), udpAddr); err != nil {
			fmt.Println("Error reply:", err.Error())
			return
		}
	}
}

func usend(addr string) {
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		fmt.Println("Error resolving:", err.Error())
		return
	}
	conn, err := net.DialUDP("udp", nil, uaddr)
	if err != nil {
		fmt.Println("Error dialing:", err.Error())
		return
	}
	defer conn.Close()
	message := "Hello, World!"
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			fmt.Println("Read reply:", string(buf[:n]))
		}
	}()
	for {
		if _, err := conn.Write([]byte(message)); err != nil {
			fmt.Println("Error write:", err.Error())
			return
		}
		fmt.Println("Send Message:", message)
		time.Sleep(1 * time.Second)
	}
}

func Test_udp(t *testing.T) {
	manualTest(t)
	go func() {
		for {
			ulisten("0.0.0.0:3433")
		}
	}()
	go func() {
		for {
			usend("127.0.0.1:3434")
			time.Sleep(1 * time.Second)
		}
	}()
	select {}
}

func Test_server(t *testing.T) {
	manualTest(t)
	udp2faketcp.Server("0.0.0.0:3435", "127.0.0.1:3433")
}

func Test_client(t *testing.T) {
	manualTest(t)
	udp2faketcp.Client("0.0.0.0:3434", "127.0.0.1:3435")
}
