package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/net/context"
)

// readCountingStorage is noopStorage with its piece reads counted. Hashing a
// piece reads it through ReadAt, so a read means a hash check ran.
type readCountingStorage struct{ reads *atomic.Int64 }

func (s readCountingStorage) OpenTorrent(ctx context.Context, info *metainfo.Info, ih metainfo.Hash) (storage.TorrentImpl, error) {
	ti, err := noopStorage{}.OpenTorrent(ctx, info, ih)
	piece := ti.Piece
	ti.Piece = func(p metainfo.Piece) storage.PieceImpl { return readCountingPiece{piece(p), s.reads} }
	return ti, err
}

type readCountingPiece struct {
	storage.PieceImpl
	reads *atomic.Int64
}

func (p readCountingPiece) ReadAt(b []byte, off int64) (int, error) {
	p.reads.Add(1)
	return p.PieceImpl.ReadAt(b, off)
}

// TestNoInitialHashCheck: getting the info must not queue a hash check of
// every piece. Nothing is ever stored, and the hashers keep running after
// Drop and Close.
func TestNoInitialHashCheck(t *testing.T) {
	cl, err := torrent.NewClient(torrent.TestingConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	const pieces = 64
	b, err := bencode.Marshal(metainfo.Info{Name: "x", PieceLength: 1 << 14, Length: pieces << 14, Pieces: make([]byte, 20*pieces)})
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int64
	tor, _ := cl.AddTorrentOpt(torrent.AddTorrentOpts{InfoHash: metainfo.HashBytes(b), Storage: readCountingStorage{&reads}})
	if err := tor.SetInfoBytes(b); err != nil {
		t.Fatal(err)
	}
	<-tor.GotInfo()
	// Hashers start inside SetInfoBytes; 64 empty pieces hash in well under this.
	time.Sleep(500 * time.Millisecond)
	if n := reads.Load(); n != 0 {
		t.Fatalf("%d piece reads after GotInfo, want 0: initial hash check queued", n)
	}
}
