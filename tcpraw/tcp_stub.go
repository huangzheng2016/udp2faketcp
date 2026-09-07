//go:build !linux

package tcpraw

import (
	"errors"
	"net"
)

type TCPConn struct{ *net.UDPConn }

func Dial(network, address string) (*TCPConn, error) {
	return nil, errors.New("os not supported")
}

func Listen(network, address string) (*TCPConn, error) {
	return nil, errors.New("os not supported")
}

func (conn *TCPConn) CloseFlow(addr net.Addr) error {
	return errors.New("os not supported")
}

func (conn *TCPConn) Events() <-chan net.Addr {
	return nil
}
