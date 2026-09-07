package udp2faketcp

import (
	"flag"
	"log"
	"os"
	"sync/atomic"
	"time"
)

type Options struct {
	Help       bool
	Debug      bool
	Server     bool
	Client     bool
	ListenAddr string
	RemoteAddr string
	TTL        int64
	MTU        int
	SockBuf    int
	Key        string
}

func debugLogln(args ...interface{}) {
	if DEBUG {
		log.Println(args...)
	}
}

func setBuffers(c interface {
	SetReadBuffer(int) error
	SetWriteBuffer(int) error
}) {
	if SOCK_BUF <= 0 {
		return
	}
	if err := c.SetReadBuffer(SOCK_BUF); err != nil {
		debugLogln("Error setting read buffer (check net.core.rmem_max):", err)
	}
	if err := c.SetWriteBuffer(SOCK_BUF); err != nil {
		debugLogln("Error setting write buffer (check net.core.wmem_max):", err)
	}
}

var mtuWarned atomic.Int64

func mtuWarn() {
	now := time.Now().Unix()
	if last := mtuWarned.Load(); now-last >= 60 && mtuWarned.CompareAndSwap(last, now) {
		log.Printf("Received datagram filling the %d-byte budget; consider lowering the upper-layer MTU", payloadBudget())
	}
}

func CliInit() {
	o := Options{}
	flag.BoolVar(&o.Help, "h", false, "Print help")
	flag.BoolVar(&o.Help, "help", false, "Print help")
	flag.BoolVar(&o.Debug, "debug", false, "Enable debug")
	flag.BoolVar(&o.Debug, "d", false, "Enable debug")
	flag.BoolVar(&o.Server, "server", false, "Start as server")
	flag.BoolVar(&o.Server, "s", false, "Start as server")
	flag.BoolVar(&o.Client, "client", false, "Start as client")
	flag.BoolVar(&o.Client, "c", false, "Start as client")
	flag.StringVar(&o.ListenAddr, "listen", "", "Listen address")
	flag.StringVar(&o.ListenAddr, "l", "", "Listen address")
	flag.StringVar(&o.RemoteAddr, "remote", "", "Remote address")
	flag.StringVar(&o.RemoteAddr, "r", "", "Remote address")
	flag.Int64Var(&o.TTL, "ttl", 180, "TTL: default 180 (seconds)")
	flag.Int64Var(&o.TTL, "t", 180, "TTL: default 180 (seconds)")
	flag.IntVar(&o.MTU, "mtu", 1440, "MTU: default 1440")
	flag.IntVar(&o.MTU, "m", 1440, "MTU: default 1440")
	flag.IntVar(&o.SockBuf, "sockbuf", 4, "Socket buffer size in MB (0 = kernel default)")
	flag.IntVar(&o.SockBuf, "b", 4, "Socket buffer size in MB (0 = kernel default)")
	flag.StringVar(&o.Key, "key", "", "Shared key for HMAC authentication")
	flag.StringVar(&o.Key, "k", "", "Shared key for HMAC authentication")
	flag.Parse()

	if o.Help {
		flag.Usage()
		os.Exit(0)
	}
	if o.Server == o.Client || o.ListenAddr == "" || o.RemoteAddr == "" {
		flag.Usage()
		os.Exit(2)
	}
	if o.TTL <= 0 {
		log.Fatalf("Invalid TTL: %d", o.TTL)
	}
	if o.MTU <= 0 || o.MTU > 65535 {
		log.Fatalf("Invalid MTU: %d", o.MTU)
	}
	if o.SockBuf < 0 || o.SockBuf > 256 {
		log.Fatalf("Invalid socket buffer size: %d MB", o.SockBuf)
	}

	UDP_TTL = time.Duration(o.TTL) * time.Second
	MAX_PACKET_LEN = o.MTU
	SOCK_BUF = o.SockBuf << 20
	DEBUG = o.Debug
	if o.Key != "" {
		AUTH_KEY = deriveKey(o.Key)
	}

	if o.Server {
		Server(o.ListenAddr, o.RemoteAddr)
	} else {
		Client(o.ListenAddr, o.RemoteAddr)
	}
}
