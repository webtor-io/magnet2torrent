package main

import (
	"io"

	"github.com/anacrolix/dht/v2"
	log "github.com/sirupsen/logrus"
)

// The torrent clients the tests start get no DHT bootstrap nodes, so no test
// talks to the public DHT. Set before any test runs: client goroutines read it.
func init() { dht.DefaultGlobalBootstrapHostPorts = nil }

// The tests read log entries through hooks, which see every entry whatever
// the output; printed, the expected Info and Warn lines would bury the rest.
func init() { log.SetOutput(io.Discard) }
