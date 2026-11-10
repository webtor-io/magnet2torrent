# magnet2torrent

Magnet-uri to torrent converter as GRPC-service.
Here is two parts: server and client.

## Server usage

```
% ./server help
NAME:
   magnet2torrent - runs magnet2torrent server

USAGE:
   server [global options] command [command options] [arguments...]

VERSION:
   0.0.1

COMMANDS:
     help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --host value, -H value  listening host [$LISTEN_HOST]
   --port value, -P value  listening port (default: 50051) [$LISTEN_PORT]
   --help, -h              show help
   --version, -v           print the version
```

## Local network addresses

`--allow-local-networks` (`ALLOW_LOCAL_NETWORKS`, default `false`). Addresses
that come from a torrent or a magnet are not connected to when they are on a
local network: loopback, private (RFC 1918, IPv6 ULA `fc00::/7`), link-local
(`169.254.0.0/16` with the cloud metadata address `169.254.169.254`, and
`fe80::/10`), CGNAT (`100.64.0.0/10`), multicast or unspecified, IPv4-mapped
forms included. The check is on the address actually connected or sent to,
after DNS, so a name that resolves to such an address and a redirect to one
are refused too. A refusal is logged as a warning at most once a minute per
process, naming the refused `ip:port` and this variable; a counter counts
every refusal. Set it to `true` when a tracker you use is on your own network.
torrent-web-seeder and magnet2torrent read the same variable.

In magnet2torrent it covers the trackers of a magnet (`tr=`),
`DEFAULT_TRACKERS` and the metainfo sources a magnet names (`xs=`, `as=`):
HTTP on connect, UDP on every send. The counter is
`m2t_local_network_refusals_total`. Peer connections are not covered.

## Client usage

It is connecting to local server instance localhost:50051.
It was made for testing purpose only.

```
% ./client magnet-uri
```