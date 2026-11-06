// SPDX-License-Identifier: GPL-2.0-only

package replica

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/clock"
)

// knownProxy trusts one address, as a KnownProxies list does.
type knownProxy netip.Addr

func (proxy knownProxy) IsKnownProxy(address netip.Addr) bool {
	return address == netip.Addr(proxy)
}

// TestForwardKeepsTheClientAddress covers #864 with D-121: a replica that
// forwards a request from a known proxy signs the proxy as the transport
// peer and sends the client's X-Forwarded-For chain, so the owner resolves
// the same client address as the replica that the proxy reached.
func TestForwardKeepsTheClientAddress(t *testing.T) {
	signer, err := NewSigner([]byte(testSecret), clock.NewFake(testEpoch))
	if err != nil {
		t.Fatal(err)
	}
	proxies := knownProxy(netip.MustParseAddr("10.10.50.80"))
	var resolved, scheme string
	owner := httptest.NewServer(signer.Accept(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		resolved = middleware.ClientAddress(request, proxies).String()
		scheme = middleware.ForwardedScheme(request, proxies)
	})))
	t.Cleanup(owner.Close)

	request := httptest.NewRequest(http.MethodGet, "/Sessions/Playing", nil)
	request.RemoteAddr = "10.10.50.80:4000"
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	request.Header.Set("X-Forwarded-Proto", "https")
	if direct := middleware.ClientAddress(request, proxies).String(); direct != "203.0.113.7" {
		t.Fatalf("client address on the forwarding replica = %s", direct)
	}
	f := &forwarder{signer: signer, from: "a", transport: newTransport()}
	t.Cleanup(f.transport.(*http.Transport).CloseIdleConnections)
	if outcome := f.forward(httptest.NewRecorder(), request, owner.URL, http.NoBody); outcome != forwardDone {
		t.Fatalf("forward outcome = %v", outcome)
	}
	if resolved != "203.0.113.7" || scheme != "https" {
		t.Fatalf("owner client address = %s, scheme = %q, want 203.0.113.7 and https", resolved, scheme)
	}

	// A client that is not a known proxy cannot pass an address.
	request.RemoteAddr = "192.168.1.20:4000"
	if outcome := f.forward(httptest.NewRecorder(), request, owner.URL, http.NoBody); outcome != forwardDone {
		t.Fatalf("forward outcome = %v", outcome)
	}
	if resolved != "192.168.1.20" || scheme != "" {
		t.Fatalf("owner client address = %s, scheme = %q, want the transport peer", resolved, scheme)
	}
}
