package udp2faketcp

import (
	"time"
)

var DEBUG = false
var MAX_PACKET_LEN = 1440
var UDP_TTL = 180 * time.Second
var SOCK_BUF = 4 << 20
var AUTH_KEY []byte
var FLOWS = 1
