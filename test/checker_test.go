package main

import (
	"fmt"
	"github.com/huangzheng2016/udp2faketcp"
	"net"
	"testing"
	"time"
)

func listen(addr string) {
	ln, _ := net.Listen("tcp", addr)
	conn, _ := ln.Accept()
	defer conn.Close()
	for {
		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		if err != nil {
			fmt.Println("Error reading:", err.Error())
			return
		}
		fmt.Println("Received:", string(buf[:n]))
	}
}
func send(addr string) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Println("Error dialing:", err.Error())
		return
	}
	defer conn.Close()
	message := "Hello, World!"
	for {
		_, err := conn.Write([]byte(message))
		if err != nil {
			fmt.Println("Error write:", err.Error())
			return
		}
		fmt.Println("Send Message:", message)
		time.Sleep(1 * time.Second)
	}
}
func ulisten(addr string) {
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		fmt.Println("Error resolving:", err.Error())
		return
	}
	conn, _ := net.ListenUDP("udp", uaddr)
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
		conn.WriteToUDP([]byte(message), udpAddr)
		if err != nil {
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
				continue
			}
			fmt.Println("Read reply:", string(buf[:n]))
		}
	}()
	for {
		_, err := conn.Write([]byte(message))
		if err != nil {
			fmt.Println("Error write:", err.Error())
			return
		}
		fmt.Println("Send Message:", message)
		time.Sleep(1 * time.Second)
	}
}
func Test_udp(t *testing.T) {
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

func Test_server(T *testing.T) {
	udp2faketcp.Server("0.0.0.0:3435", "127.0.0.1:3433")
}

func Test_client(T *testing.T) {
	udp2faketcp.Client("0.0.0.0:3434", "127.0.0.1:3435")
}
