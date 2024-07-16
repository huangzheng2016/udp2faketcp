package udp2faketcp

import (
	"flag"
	"log"
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
}

func debugLogln(args ...interface{}) {
	if DEBUG {
		log.Println(args...)
	}
}

func debugLogf(format string, args ...interface{}) {
	if DEBUG {
		log.Printf(format, args...)
	}
}

func ctlInit() {
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
	flag.IntVar(&o.MTU, "mtu", 1408, "MTU: default 1408")
	flag.IntVar(&o.MTU, "m", 1408, "MTU: default 1408")
	flag.Parse()
	UDP_TTL = time.Duration(o.TTL) * time.Second
	MAX_PACKET_LEN = o.MTU
	DEBUG = o.Debug
	if o.Help || (o.Server == false && o.Client == false) || o.ListenAddr == "" || o.RemoteAddr == "" {
		flag.Usage()
	} else if o.Server {
		Server(o.ListenAddr, o.RemoteAddr)
	} else if o.Client {
		Client(o.ListenAddr, o.RemoteAddr)
	} else {
		flag.Usage()
	}
}
