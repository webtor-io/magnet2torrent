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
`DEFAULT_TRACKERS`, the trackers `Verify` is given and the metainfo sources a
magnet names (`xs=`, `as=`): HTTP on connect, UDP on every send. The counter
is `m2t_local_network_refusals_total`. Peer connections are not covered.

## Private magnets and Verify

A private torrent (BEP 27, `private=1` in its info) may be announced only to
its own trackers. Until the info arrives nobody knows whether a magnet is
private, so the resolve announces its `tr=` and `DEFAULT_TRACKERS` as for
any magnet. Once the info is in and it is private:

- a magnet without `tr=`, or with only blank ones, is refused with
  `FAILED_PRECONDITION` and
  `ErrorInfo{domain: "magnet2torrent", reason: "private_magnet"}`, counted as
  `m2t_resolve_total{outcome="private_refused"}`;
- a magnet with `tr=` gets a torrent whose announce list is exactly its
  `tr=`, trimmed and without blanks, as one tier in the magnet's order, with
  neither `DEFAULT_TRACKERS` nor web seeds, and the reply says
  `private: true`.

`Verify(info_hash, trackers)` checks that a tracker accepts the key in its
URL for a known info-hash. It needs neither the info nor peers and runs no
torrent client: no DHT, no PEX, no `DEFAULT_TRACKERS`, only the trackers
given. Of those it keeps the `http`, `https` and `udp` URLs with a host,
each once, in tier order. One URL at a time it sends one `started` announce
with `numwant=0`; the first tracker that accepts gets a `stopped` from the
same peer (`peer_id`, `key`, port), which ends the check. A URL gets at most
5 s, all of them together 15 s, and the `stopped` 5 s more, sent even when
the caller has gone: give the call a deadline of at least 20 s.

The reply is an `outcome` and, only when refused, a `class`:

| `outcome` | when |
|---|---|
| `accepted` | a tracker accepted |
| `refused` | none accepted, and one answered with a failure reason (HTTP) or an error response (UDP) |
| `unreachable` | none accepted or refused: network, timeout, an HTTP status other than 200, an unreadable reply, a local network address |
| `invalid` | the info-hash is not 40 hex digits or no URL is usable; nothing is sent |

The class is the first refusal's, read from the tracker's text:
`unregistered`, `key`, `client`, `account`, otherwise `other`. The text
itself never leaves the process and is not logged, since a tracker can quote
the key back; nor is a URL. A call logs one line, `verify finished`, with the
info-hash, the trackers' registrable domains or IP addresses (at most 32),
the number of URLs, the outcome and the class. The counter is
`m2t_verify_total{outcome, class}`; a call whose caller left before any
tracker accepted or refused is not counted.

What a tracker sees: the pod's IP address, the library's User-Agent, a
`peer_id` with the library's BEP 20 prefix, a random port and `key`,
`uploaded` and `downloaded` 0, and `left` unknown (2^63-1 over HTTP). For
the account behind the key this is a short session in which nothing was
downloaded.

## Client usage

It is connecting to local server instance localhost:50051.
It was made for testing purpose only.

```
% ./client magnet-uri
```