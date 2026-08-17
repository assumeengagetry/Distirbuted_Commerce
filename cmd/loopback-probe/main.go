package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	if err := probe(os.Args[1], time.Second); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func probe(address string, timeout time.Duration) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" || timeout <= 0 {
		return fmt.Errorf("probe requires a loopback host:port and positive timeout")
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("probe address must be loopback")
	}
	connection, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return fmt.Errorf("probe failed")
	}
	return connection.Close()
}
