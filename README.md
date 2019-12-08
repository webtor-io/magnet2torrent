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

## Client usage

It is connecting to local server instance localhost:50051.
It was made for testing purpose only.

```
% ./client magnet-uri
```