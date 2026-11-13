package main

import "github.com/anacrolix/dht/v2"

// The torrent clients the tests start get no DHT bootstrap nodes, so no test
// talks to the public DHT. Set before any test runs: client goroutines read it.
func init() { dht.DefaultGlobalBootstrapHostPorts = nil }
