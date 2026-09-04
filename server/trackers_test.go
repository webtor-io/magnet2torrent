package main

import (
	"reflect"
	"testing"
)

func TestParseTrackers(t *testing.T) {
	got := parseTrackers("udp://a:1/announce, udp://b:2/announce\nhttps://c/announce  ")
	want := []string{"udp://a:1/announce", "udp://b:2/announce", "https://c/announce"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if parseTrackers("") != nil {
		t.Fatal("empty list must be nil")
	}
}
