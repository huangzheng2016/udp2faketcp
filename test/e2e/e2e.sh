#!/usr/bin/env bash
# End-to-end tunnel test in Docker: iperf over udp2faketcp.
#
# Usage:
#   test/e2e/e2e.sh build            build binary + image (run first / after code changes)
#   test/e2e/e2e.sh basic FLOWS [KEY] [SECS]   throughput without any shaping
#   test/e2e/e2e.sh limit FLOWS [KEY] [SECS]   per-flow 10mbit shaping (needs FLOWS <= 8)
#   test/e2e/e2e.sh clean
set -euo pipefail
cd "$(dirname "$0")/../.."

IMG=udp2faketcp-e2e
NET=u2f-e2e
PORT_RANGE="20000 20015"
LIMIT=10mbit

build() {
	GOOS=linux GOARCH="$(go env GOARCH)" CGO_ENABLED=0 go build -o test/e2e/udp2faketcp ./cmd/udp2faketcp
	docker build -q -t "$IMG" test/e2e >/dev/null
}

start_server() { # KEY
	docker run -d --name u2f-server --network "$NET" --cap-add NET_ADMIN --cap-add NET_RAW \
		--sysctl net.core.rmem_max=67108864 --sysctl net.core.wmem_max=67108864 \
		"$IMG" sh -c "iperf -s -u -p 5201 >/tmp/iperf-server.log 2>&1 & exec udp2faketcp -s -l 0.0.0.0:12345 -r 127.0.0.1:5201 -d $1" >/dev/null
	SERVER_IP=$(docker inspect -f "{{index .NetworkSettings.Networks \"$NET\" \"IPAddress\"}}" u2f-server)
}

start_client() { # FLOWS KEY
	docker run -d --name u2f-client --network "$NET" --cap-add NET_ADMIN --cap-add NET_RAW \
		--sysctl net.core.rmem_max=67108864 --sysctl net.core.wmem_max=67108864 \
		"$IMG" sh -c "exec udp2faketcp -c -l 0.0.0.0:51821 -r $SERVER_IP:12345 -d -f $1 $2" >/dev/null
}

run_iperf() { # SECS RATE
	docker exec u2f-client iperf -c 127.0.0.1 -B 127.0.0.1:19999 -u -p 51821 -b "$2" -t "$1" -l 1300 2>&1 | grep -A3 'Server Report' || true
	echo "--- client tunnel stats ---"
	{ docker logs u2f-client 2>&1; docker exec u2f-client cat /tmp/tunnel.log 2>/dev/null; } | grep 'session ' | tail -3 || true
	echo "--- server tunnel stats ---"
	docker logs u2f-server 2>&1 | grep 'session ' | tail -3 || true
}

clean() {
	docker rm -f u2f-server u2f-client >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
}

setup_limit() { # per-source-port shaping on the client side
	# htb with explicit burst; each source port gets its own $LIMIT class,
	# unmatched traffic goes to the unshaped default class
	docker exec u2f-client sh -c "
		tc qdisc add dev eth0 root handle 1: htb default 999
		tc class add dev eth0 parent 1: classid 1:999 htb rate 1000mbit
		i=0
		for p in \$(seq 20000 20015); do
			i=\$((i+1))
			tc class add dev eth0 parent 1: classid 1:\$i htb rate $LIMIT burst 128kbit cburst 128kbit
			tc filter add dev eth0 parent 1: protocol ip u32 match ip sport \$p 0xffff classid 1:\$i
		done"
}

setup_skew() { # like setup_limit, plus 50ms extra delay on half the ports
	setup_limit
	docker exec u2f-client sh -c "
		i=4
		for p in \$(seq 20004 20007); do
			i=\$((i+1))
			tc qdisc add dev eth0 parent 1:\$i handle \${i}0: netem delay 50ms
		done"
}

case "${1:-}" in
build)
	build
	;;
basic)
	clean
	docker network create "$NET" >/dev/null
	start_server "${3:-}"
	start_client "$2" "${3:-}"
	sleep 6
	run_iperf "${4:-10}" 1G
	;;
limit)
	clean
	docker network create "$NET" >/dev/null
	# container first, shaping before the tunnel starts dialing
	docker run -d --name u2f-client --network "$NET" --cap-add NET_ADMIN --cap-add NET_RAW \
		--sysctl net.core.rmem_max=67108864 --sysctl net.core.wmem_max=67108864 \
		--sysctl net.ipv4.ip_local_port_range="$PORT_RANGE" \
		"$IMG" sleep infinity >/dev/null
	setup_limit
	start_server "${3:-}"
	docker exec -d u2f-client sh -c "exec udp2faketcp -c -l 0.0.0.0:51821 -r $SERVER_IP:12345 -d -f $2 ${3:-} >>/tmp/tunnel.log 2>&1"
	sleep 6
	run_iperf "${4:-10}" "${5:-90M}"
	;;
skew)
	clean
	docker network create "$NET" >/dev/null
	docker run -d --name u2f-client --network "$NET" --cap-add NET_ADMIN --cap-add NET_RAW \
		--sysctl net.core.rmem_max=67108864 --sysctl net.core.wmem_max=67108864 \
		--sysctl net.ipv4.ip_local_port_range="$PORT_RANGE" \
		"$IMG" sleep infinity >/dev/null
	setup_skew
	start_server "${3:-}"
	docker exec -d u2f-client sh -c "exec udp2faketcp -c -l 0.0.0.0:51821 -r $SERVER_IP:12345 -d -f $2 ${3:-} >>/tmp/tunnel.log 2>&1"
	sleep 6
	run_iperf "${4:-12}" "${5:-70M}"
	;;
clean)
	clean
	;;
*)
	grep '^#' "$0" | head -8
	exit 2
	;;
esac
