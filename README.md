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

## Tracker addresses

A magnet picks the trackers the server connects to (its `tr=`), and DNS picks
their addresses. By default the server refuses a tracker at a loopback,
private (RFC 1918, IPv6 ULA), link-local (169.254.169.254 included), CGNAT
(100.64.0.0/10), multicast or unspecified address, IPv4-mapped forms included,
and logs a warning naming the flag below. `DEFAULT_TRACKERS` are checked the
same way. The check runs on the resolved address right before connecting
(HTTP) or sending (UDP), so neither a DNS answer nor an HTTP redirect gets
around it. Peer connections are not filtered.

A deployment whose trackers really are on a local network sets
`--allow-local-networks` (`ALLOW_LOCAL_NETWORKS=true`).

## Client usage

It is connecting to local server instance localhost:50051.
It was made for testing purpose only.

```
% ./client magnet-uri
```