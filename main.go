package udp2faketcp

import (
	"time"
)

var DEBUG = false
var MAX_PACKET_LEN = 1408
var UDP_TTL = 180 * time.Second

func main() {
	ctlInit()
}
