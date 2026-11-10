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
that come from a torrent or a magnet are not connected to when they are not
public:

- loopback (`127.0.0.0/8`, `::1`)
- private: RFC 1918 and IPv6 ULA `fc00::/7`
- link-local: `169.254.0.0/16`, with the cloud metadata address
  `169.254.169.254`, and `fe80::/10`
- unspecified `0.0.0.0`, `::`
- multicast
- CGNAT `100.64.0.0/10`
- "this network" `0.0.0.0/8`
- reserved `240.0.0.0/4`, with the broadcast address `255.255.255.255`
- site-local `fec0::/10`, deprecated
- NAT64 local-use `64:ff9b:1::/48` (RFC 8215), whatever it carries: none of
  it is on the internet, and where the IPv4 address sits in it depends on
  the translator
- any of the IPv4 ones above inside IPv6: IPv4-mapped `::ffff:0:0/96`,
  IPv4-compatible `::/96`, NAT64 `64:ff9b::/96` (the last 32 bits), 6to4
  `2002::/16` (bits 16 to 48)

The check is on the address actually connected or sent to, after DNS, so a
name that resolves to such an address and a redirect to one are refused too.
It knows address classes, not your deployment: a node's public address, and
the ports the node or a host-network pod listen on there, are public and
pass. Keep those out with an egress NetworkPolicy. A refusal is logged as a
warning at most once a minute per process, naming the refused `ip:port` and
this variable; a counter counts every refusal. Set it to `true` when a
tracker you use is on your own network. torrent-web-seeder and
magnet2torrent read the same variable.

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