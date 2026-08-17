package main

import (
	"net"
	"testing"
	"time"
)

func TestProbeAcceptsOnlyReachableLoopbackAddress(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := probe(listener.Addr().String(), time.Second); err != nil {
		t.Fatalf("probe(loopback) error = %v", err)
	}
	if err := probe("192.0.2.1:80", time.Second); err == nil {
		t.Fatal("probe(non-loopback) error = nil")
	}
}
