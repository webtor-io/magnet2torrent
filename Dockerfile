FROM golang:latest

# copy the source files
COPY . /go/src/github.com/webtor-io/magnet2torrent

WORKDIR /go/src/github.com/webtor-io/magnet2torrent/server

# disable crosscompiling
ENV CGO_ENABLED=0

# compile linux only
ENV GOOS=linux

# build the binary with debug information removed
RUN go build -ldflags '-w -s' -a -installsuffix cgo -o server

FROM alpine:3.21

# copy our static linked library
COPY --from=0 /go/src/github.com/webtor-io/magnet2torrent/server/server .

# tell we are exposing our service on port 50051
EXPOSE 50051

# run it!
CMD ["./server"]
