package node

import (
	"net/http"
	"testing"
)

func TestValidatorHTTPClientVerifiesTLSByDefault(t *testing.T) {
	transport := newValidatorHTTPClient(false).Transport.(*http.Transport)

	if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("validator HTTP client must verify TLS certificates by default")
	}
}

func TestValidatorHTTPClientAllowsExplicitTLSOptOut(t *testing.T) {
	transport := newValidatorHTTPClient(true).Transport.(*http.Transport)

	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("explicit TLS verification opt-out was not applied")
	}
}
