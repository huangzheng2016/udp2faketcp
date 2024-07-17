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
  -r, --remote [host:port]  Listen address
  -t, --ttl [int]        TTL: default 180 (seconds)
  -m, --mtu [int]        MTU: default 1440 
```
# Example
```shell
# Server
udp2faketcp -s -l 0.0.0.0:12345 -r 127.0.0.1:51820 -d
# Client
udp2faketcp -c -l 0.0.0.0:51821 -r 127.0.0.1:12345 -d
```
# Peformance Test
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
[  5] 0.00-29.96 sec   201 MBytes  56.3 Mbits/sec   0.000 ms 132809/286187 (0%)
[  3] 0.00-30.01 sec   201 MBytes  56.1 Mbits/sec   0.000 ms 132932/286189 (0%)
[  1] 0.00-30.00 sec   197 MBytes  55.2 Mbits/sec   0.000 ms 135654/286189 (0%)
[  2] 0.00-29.98 sec   201 MBytes  56.1 Mbits/sec   0.000 ms 133163/286189 (0%)
[  4] 0.00-29.97 sec   199 MBytes  55.6 Mbits/sec   0.000 ms 134690/286188 (0%)
```
Compare with udp2raw
```azure
[ ID] Interval       Transfer     Bandwidth        Jitter   Lost/Total Datagrams
[  5] 0.00-30.02 sec  40.7 MBytes  11.4 Mbits/sec   0.000 ms 2819698/2850787 (0%)
[  2] 0.00-30.01 sec  37.9 MBytes  10.6 Mbits/sec   0.000 ms 2793846/2822760 (0%)
[  3] 0.00-30.03 sec  38.8 MBytes  10.8 Mbits/sec   0.000 ms 2804290/2833904 (0%)
[  1] 0.00-30.02 sec  39.8 MBytes  11.1 Mbits/sec   0.000 ms 2824314/2854654 (0%)
[  4] 0.00-30.03 sec  41.1 MBytes  11.5 Mbits/sec   0.000 ms 2797987/2829360 (0%)
```
# Similar

[udp2raw: https://github.com/wangyu-/udp2raw](https://github.com/wangyu-/udp2raw)

[phantun: https://github.com/dndx/phantun](https://github.com/dndx/phantun)
>This is a very good project. I have referenced many of its implementation ideas and reproduced them in Golang, the effect is excellent.