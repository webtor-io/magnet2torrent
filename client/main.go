package main

import (
	"bytes"
	"log"
	"os"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	pb "github.com/webtor-io/magnet2torrent/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

const (
	address = "localhost:50051"
)

func main() {
	// Set up a connection to the server.
	conn, err := grpc.Dial(address, grpc.WithInsecure())
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()
	c := pb.NewMagnet2TorrentClient(conn)

	magnet := ""
	if len(os.Args) > 1 {
		magnet = os.Args[1]
	} else {
		log.Fatal("no magnet url provided")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r, err := c.Magnet2Torrent(ctx, &pb.Magnet2TorrentRequest{Magnet: magnet})
	if err != nil {
		log.Fatalf("could not load torrent err=%s", err)
	}
	log.Printf("got request len=%v", len(r.GetTorrent()))
	reader := bytes.NewReader(r.GetTorrent())
	mi, err := metainfo.Load(reader)
	if err != nil {
		log.Fatalf("error loading info: %s", err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		log.Fatalf("error unmarshalling info: %s", err)
	}
	log.Printf("got torrent name=%s", info.Name)
	f, err := os.Create(info.Name + ".torrent")
	if err != nil {
		log.Fatalf("error creating torrent metainfo file=%s", err)
	}
	defer f.Close()
	err = bencode.NewEncoder(f).Encode(mi)
	if err != nil {
		log.Fatalf("error writing torrent metainfo file: %s", err)
	}

	if err != nil {
		log.Fatalf("could not load torrent err=%s", err)
	}
}
