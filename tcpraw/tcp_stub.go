//go:build !linux

package tcpraw

import (
	"errors"
	"net"
)

type TCPConn struct{ *net.UDPConn }

// Dial connects to the remote TCP port,
// and returns a single packet-oriented connection
func Dial(network, address string) (*TCPConn, error) {
	return nil, errors.New("os not supported")
}

func Listen(network, address string) (*TCPConn, error) {
	return nil, errors.New("os not supported")
}

// CloseFlow is not supported on this platform.
func (conn *TCPConn) CloseFlow(addr net.Addr) error {
	return errors.New("os not supported")
}

// Events returns nil on this platform.
func (conn *TCPConn) Events() <-chan net.Addr {
	return nil
}
