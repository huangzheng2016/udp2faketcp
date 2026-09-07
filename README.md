# udp2faketcp

A raw tunnel sending UDP packets through fake TCP. Linux only (raw sockets, CGO disabled).

# Usage

```shell
udp2faketcp -s -l 0.0.0.0:12345 -r 127.0.0.1:51820 [-k secret]
udp2faketcp -c -l 0.0.0.0:51821 -r 127.0.0.1:12345 [-k secret] [-f 8]
```

| Flag | Default | Description |
|---|---|---|
| `-s`, `--server` | | Start as server |
| `-c`, `--client` | | Start as client |
| `-l`, `--listen` | | Listen address |
| `-r`, `--remote` | | Remote address |
| `-t`, `--ttl` | 180 | Idle timeout (seconds) |
| `-m`, `--mtu` | 1440 | Frame budget, see MTU |
| `-b`, `--sockbuf` | 4 | Socket buffer in MB, 0 = kernel default |
| `-f`, `--flows` | 1 | Parallel fake-TCP flows per session (client only) |
| `-k`, `--key` | | Shared key for HMAC authentication |
| `-d`, `--debug` | | Debug logging |

Requires root or `setcap cap_net_raw,cap_net_admin=+pe`; in Docker add `--cap-add NET_ADMIN --cap-add NET_RAW`.
For high throughput also `sysctl -w net.core.rmem_max=8388608 net.core.wmem_max=8388608`.

# Protocol

Each fake-TCP payload is a frame: `[1B type][8B seq][payload]`, plus `[8B hmacSeq][16B HMAC-SHA256]` when
`-k` is set (authentication only, not encryption). Per-second heartbeat/pong frames provide keepalive,
per-flow RTT measurement and 5s dead-flow detection; sessions idle for `-ttl` are torn down with RST.

**Both ends must run the same version and the same `-k`.**

# Multi-flow

A single TCP 4-tuple is often rate-limited by the ISP. `-f N` stripes datagrams round-robin over N parallel
fake-TCP connections and the receiver restores order with an adaptive reorder buffer
(gap-wait `(srtt + 4*rttvar) * 2.2`, window 1024). Dead flows are re-dialed automatically.

Measured with every flow shaped to 10mbit (`test/e2e/e2e.sh`):

| | Throughput |
|---|---|
| `-f 1` | 9.2 Mbps |
| `-f 8` | 72.5 Mbps |

Use `-f 8` on rate-limited links; more than 16 risks triggering per-IP limits.
A few percent of out-of-order datagrams at the receiver is expected (concurrent readers);
it is bounded micro-reorder and well within what WireGuard tolerates.

# MTU

On-wire size = `-mtu` + 40 (IPv4+TCP headers); payload budget = `-mtu` − 9 (− 33 with `-k`).
Example for WireGuard on a 1500 link: WireGuard MTU = 1500 − 40 − 9 = 1451 (1427 with `-k`), tunnel `-m 1428` (1395 with `-k`).

# Performance Test

iperf3 UDP mode is not used because of a bug mentioned in this issue: https://github.com/esnet/iperf/issues/296

The environment used for testing below is consistent.

```shell
# Server
iperf -s -u -p <PORT>
# Client
iperf -c <HOST> -u -p <PORT> -t 30 -P 5 -b 1G -d -l 1374 
```

udp2faketcp

```
[ ID] Interval       Transfer     Bandwidth        Jitter   Lost/Total Datagrams
[  1] 0.00-30.00 sec   197 MBytes  55.2 Mbits/sec   0.000 ms 135654/286189 (0%)
[  2] 0.00-29.98 sec   201 MBytes  56.1 Mbits/sec   0.000 ms 133163/286189 (0%)
[  3] 0.00-30.01 sec   201 MBytes  56.1 Mbits/sec   0.000 ms 132932/286189 (0%)
[  4] 0.00-29.97 sec   199 MBytes  55.6 Mbits/sec   0.000 ms 134690/286188 (0%)
[  5] 0.00-29.96 sec   201 MBytes  56.3 Mbits/sec   0.000 ms 132809/286187 (0%)
```

Compare with udp2raw

```
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
