package node

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

func newValidatorHTTPClient(skipTLSVerify bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 60 * time.Second,
	}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		MaxIdleConnsPerHost: 50,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
	}

	if skipTLSVerify {
		// #nosec G402 -- this is an explicit per-validator compatibility opt-out.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: transport,
	}
}
