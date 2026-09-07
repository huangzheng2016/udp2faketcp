# udp2faketcp
A raw tunnel sending packets through faketcp

Support linux only because of the rawtcp with cgo disabled

```shell
Usage:
  udp2faketcp [flags]

Flags:
  -s, --server         Start as server
  -c, --client         Start as client
  -d, --debug          Enable debug
  -h, --help           Print help
  -l, --listen [host:port]  Listen address
  -r, --remote [host:port]  Remote address
  -t, --ttl [int]        TTL: default 180 (seconds)
  -m, --mtu [int]        MTU: default 1440
  -b, --sockbuf [int]    Socket buffer size in MB: default 4 (0 = kernel default)
  -f, --flows [int]      Parallel fake-TCP flows per session: default 4 (1-32)
  -k, --key [string]     Shared key for HMAC authentication
```

# Protocol

Every fake-TCP payload is a small frame: a 1-byte type (data / heartbeat / handshake / pong), an 8-byte
session sequence number, and the payload. Both sides exchange a heartbeat every second (answered with a pong,
which doubles as the per-flow RTT measurement); a connection with no data for `-ttl` seconds is torn down
with a RST, so the peer and middleboxes free their state immediately instead of timing out.

With `-k`, each frame additionally carries an 8-byte per-flow sequence number and a 16-byte truncated
HMAC-SHA256 tag (key derived from the passphrase with SHA-256). The server only forwards traffic from clients
that completed a handshake frame, so the tunnel is no longer an open relay, and a 4096-bit replay window
rejects replayed frames. Note: `-k` authenticates packets, it does not encrypt them.

**Both ends must run the same version, the same `-k` and the same `-f`.**

# Multi-flow aggregation

ISPs and middleboxes often rate-limit a single TCP 4-tuple. With `-f N` (default 4) each session opens N
parallel fake-TCP connections and stripes datagrams across them round-robin; the receiver restores the
original order with a reorder buffer before delivery.

- The reorder gap-wait adapts to the inter-flow path skew, estimated from heartbeat RTTs
  (`(srtt + 4*rttvar) * 2.2`, clamped to 5-500ms); the buffer holds up to 1024 datagrams per session.
- Dead flows (5s silence) are replaced automatically; idle sessions are reaped after `-ttl`.
- Use `-f 8` on rate-limited links. Values above 16 risk triggering per-IP limits or anti-DDoS.
- Datagrams from one UDP source may be assigned sequence numbers a few packets out of order on multi-core
  machines (concurrent readers); the resulting micro-reorder is well within what WireGuard tolerates.

Measured in Docker with each fake-TCP flow shaped to 10mbit (`test/e2e/e2e.sh`):

| | throughput | note |
|---|---|---|
| `-f 1` | 9.2 Mbps | single-flow baseline |
| `-f 8` | 72.5 Mbps | ~8x aggregation |
| `-f 8`, 50ms skew on half the flows | 41/40 Mbps | 0 reorder-induced loss after RTT adaptation |
| `-f 8`, unshaped | 842 Mbps | no regression vs `-f 1` (877 Mbps) |

Run it yourself: `test/e2e/e2e.sh build && test/e2e/e2e.sh limit 8` (also `basic` / `skew` / `clean`).

# MTU

`-mtu` is the budget of one whole frame. The packet on the wire is larger by the IP/TCP headers (40 bytes for IPv4),
and the UDP payload budget is smaller by the frame header (9 bytes, or 33 bytes with `-k`).
For a 1500-byte link with WireGuard on top (its own overhead is 32 bytes for IPv4):

```
udp2faketcp -mtu = 1500 - 40 (IP+TCP) - 32 (WireGuard) = 1428, minus 33 when using -k
WireGuard MTU     = 1500 - 40 - (frame header)          = 1451, or 1427 with -k
```

Datagrams exceeding the payload budget are dropped with a warning telling you to lower the upper-layer MTU.

# Permissions

Requires raw sockets and iptables access. Either run as root, or:

```shell
setcap cap_net_raw,cap_net_admin=+pe udp2faketcp
```

For high throughput you may also need to raise the kernel socket buffer limits so `-sockbuf` can take effect:

```shell
sysctl -w net.core.rmem_max=8388608 net.core.wmem_max=8388608
```

In Docker the container needs `--cap-add NET_ADMIN --cap-add NET_RAW`.

# Example
```shell
# Server
udp2faketcp -s -l 0.0.0.0:12345 -r 127.0.0.1:51820 -d
# Client
udp2faketcp -c -l 0.0.0.0:51821 -r 127.0.0.1:12345 -d
# With authentication (use the same key on both ends)
udp2faketcp -s -l 0.0.0.0:12345 -r 127.0.0.1:51820 -k your-secret
udp2faketcp -c -l 0.0.0.0:51821 -r 127.0.0.1:12345 -k your-secret
```
# Performance Test
iperf3 UDP mode is not used because of a bug mentioned in this issue: https://github.com/esnet/iperf/issues/296

Switched to using iperf, but it seems to have bugs and the speed test is slower in the orbstack container. 

The environment used for testing below is consistent.
```shell
# Server
iperf -s -u -p <PORT>
# Client
iperf -c <HOST> -u -p <PORT> -t 30 -P 5 -b 1G -d -l 1374 
```
udp2faketcp
```azure
[ ID] Interval       Transfer     Bandwidth        Jitter   Lost/Total Datagrams
[  1] 0.00-30.00 sec   197 MBytes  55.2 Mbits/sec   0.000 ms 135654/286189 (0%)
[  2] 0.00-29.98 sec   201 MBytes  56.1 Mbits/sec   0.000 ms 133163/286189 (0%)
[  3] 0.00-30.01 sec   201 MBytes  56.1 Mbits/sec   0.000 ms 132932/286189 (0%)
[  4] 0.00-29.97 sec   199 MBytes  55.6 Mbits/sec   0.000 ms 134690/286188 (0%)
[  5] 0.00-29.96 sec   201 MBytes  56.3 Mbits/sec   0.000 ms 132809/286187 (0%)
```
Compare with udp2raw
```azure
[ ID] Interval       Transfer     Bandwidth        Jitter   Lost/Total Datagrams
[  1] 0.00-30.02 sec  39.8 MBytes  11.1 Mbits/sec   0.000 ms 2824314/2854654 (0%)
[  2] 0.00-30.01 sec  37.9 MBytes  10.6 Mbits/sec   0.000 ms 2793846/2822760 (0%)
[  3] 0.00-30.03 sec  38.8 MBytes  10.8 Mbits/sec   0.000 ms 2804290/2833904 (0%)
[  4] 0.00-30.03 sec  41.1 MBytes  11.5 Mbits/sec   0.000 ms 2797987/2829360 (0%)
[  5] 0.00-30.02 sec  40.7 MBytes  11.4 Mbits/sec   0.000 ms 2819698/2850787 (0%)
```
# Similar

[udp2raw: https://github.com/wangyu-/udp2raw](https://github.com/wangyu-/udp2raw)

[phantun: https://github.com/dndx/phantun](https://github.com/dndx/phantun)
>This is a very good project. I have referenced many of its implementation ideas and reproduced them in Golang, the effect is excellent.
