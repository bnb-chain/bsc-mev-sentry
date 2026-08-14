package config

import (
	"net"
	"testing"
)

func TestDefaultDebugListenAddrIsLoopback(t *testing.T) {
	host, _, err := net.SplitHostPort(defaultConfig.Debug.ListenAddr)
	if err != nil {
		t.Fatalf("invalid default debug listen address: %v", err)
	}

	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("default debug listener must use a loopback address, got %q", host)
	}
}
