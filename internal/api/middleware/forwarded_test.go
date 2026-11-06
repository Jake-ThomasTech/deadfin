// SPDX-License-Identifier: GPL-2.0-only

package middleware_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/router"
)

// proxyList trusts the listed prefixes, as a KnownProxies list does.
type proxyList []netip.Prefix

func (list proxyList) IsKnownProxy(address netip.Addr) bool {
	for _, prefix := range list {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func forwardedRequest(remote string, headers map[string][]string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/deadfin/v1/setup?token=t", nil)
	request.RemoteAddr = remote
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	return request
}

// TestForwardedSchemeFollowsKnownProxies covers the ASP.NET Core
// ForwardedHeadersMiddleware walk that pinned Jellyfin configures from
// KnownProxies (ApiServiceCollectionExtensions.cs:166-187).
func TestForwardedSchemeFollowsKnownProxies(t *testing.T) {
	ingress := proxyList{netip.MustParsePrefix("10.42.0.0/16")}
	for _, tc := range []struct {
		name    string
		proxies middleware.KnownProxies
		remote  string
		headers map[string][]string
		want    string
	}{
		{"no proxies honor no header", nil, "10.42.0.7:4000", map[string][]string{"X-Forwarded-Proto": {"https"}}, ""},
		{"known proxy https", ingress, "10.42.0.7:4000", map[string][]string{"X-Forwarded-For": {"203.0.113.9"}, "X-Forwarded-Proto": {"https"}}, "https"},
		{"known proxy proto without for", ingress, "10.42.0.7:4000", map[string][]string{"X-Forwarded-Proto": {"https"}}, "https"},
		{"known proxy http", ingress, "10.42.0.7:4000", map[string][]string{"X-Forwarded-Proto": {"http"}}, "http"},
		{"unknown peer is ignored", ingress, "192.168.1.20:4000", map[string][]string{"X-Forwarded-Proto": {"https"}}, ""},
		{"IPv4-mapped peer", ingress, "[::ffff:10.42.0.7]:4000", map[string][]string{"X-Forwarded-Proto": {"https"}}, "https"},
		{"chain stops at the first unknown hop", ingress, "10.42.0.7:4000", map[string][]string{
			"X-Forwarded-For":   {"203.0.113.9, 198.51.100.4"},
			"X-Forwarded-Proto": {"http, https"},
		}, "https"},
		{"chain continues through known hops", ingress, "10.42.0.7:4000", map[string][]string{
			"X-Forwarded-For":   {"203.0.113.9, 10.42.0.8"},
			"X-Forwarded-Proto": {"http", "https"},
		}, "http"},
		{"unparsable address stops the walk", ingress, "10.42.0.7:4000", map[string][]string{
			"X-Forwarded-For":   {"not-an-address"},
			"X-Forwarded-Proto": {"https"},
		}, ""},
		{"invalid scheme stops the walk", ingress, "10.42.0.7:4000", map[string][]string{
			"X-Forwarded-For":   {"203.0.113.9"},
			"X-Forwarded-Proto": {"ht tps"},
		}, ""},
		{"address with port", ingress, "10.42.0.7:4000", map[string][]string{
			"X-Forwarded-For":   {"10.42.0.8:5000, 10.42.0.9"},
			"X-Forwarded-Proto": {"https, http"},
		}, "https"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := middleware.ForwardedScheme(forwardedRequest(tc.remote, tc.headers), tc.proxies); got != tc.want {
				t.Fatalf("ForwardedScheme = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientAddressFollowsKnownProxies covers #864: the client address is
// the X-Forwarded-For address that the ApplyForwarders walk reaches from a
// KnownProxies peer, otherwise the transport peer.
func TestClientAddressFollowsKnownProxies(t *testing.T) {
	proxies := proxyList{netip.MustParsePrefix("10.10.50.80/32"), netip.MustParsePrefix("10.10.50.81/32")}
	for _, tc := range []struct {
		name    string
		proxies middleware.KnownProxies
		remote  string
		headers map[string][]string
		want    string
	}{
		{"no proxies: the transport peer", nil, "10.10.50.80:4000", map[string][]string{"X-Forwarded-For": {"203.0.113.7"}}, "10.10.50.80"},
		{"known proxy, remote client", proxies, "10.10.50.80:4000", map[string][]string{"X-Forwarded-For": {"203.0.113.7"}}, "203.0.113.7"},
		{"known proxy, LAN client", proxies, "10.10.50.80:4000", map[string][]string{"X-Forwarded-For": {"10.10.20.15"}}, "10.10.20.15"},
		{"known proxy without header", proxies, "10.10.50.80:4000", nil, "10.10.50.80"},
		{"unknown peer: the header is ignored", proxies, "192.168.1.20:4000", map[string][]string{"X-Forwarded-For": {"10.10.20.15"}}, "192.168.1.20"},
		{"chain through two known proxies", proxies, "10.10.50.80:4000", map[string][]string{"X-Forwarded-For": {"198.51.100.9, 10.10.50.81"}}, "198.51.100.9"},
		{"chain stops at the first unknown hop", proxies, "10.10.50.80:4000", map[string][]string{"X-Forwarded-For": {"10.10.50.81, 198.51.100.9"}}, "198.51.100.9"},
		{"spoofed left entry is not reached", proxies, "10.10.50.80:4000", map[string][]string{"X-Forwarded-For": {"10.10.20.15", "203.0.113.7"}}, "203.0.113.7"},
		{"unparsable entry keeps the hops before it", proxies, "10.10.50.80:4000", map[string][]string{"X-Forwarded-For": {"bogus, 10.10.50.81"}}, "10.10.50.81"},
		{"unparsable first entry", proxies, "10.10.50.80:4000", map[string][]string{"X-Forwarded-For": {"bogus"}}, "10.10.50.80"},
		{"address with port", proxies, "10.10.50.80:4000", map[string][]string{"X-Forwarded-For": {"203.0.113.7:51000"}}, "203.0.113.7"},
		{"bracketed IPv6", proxies, "10.10.50.80:4000", map[string][]string{"X-Forwarded-For": {"[2001:db8::7]"}}, "2001:db8::7"},
		{"IPv4-mapped peer and entry", proxies, "[::ffff:10.10.50.80]:4000", map[string][]string{"X-Forwarded-For": {"::ffff:203.0.113.7"}}, "203.0.113.7"},
		{"Forwarded and X-Real-IP are not read", proxies, "10.10.50.80:4000", map[string][]string{"Forwarded": {"for=203.0.113.7"}, "X-Real-IP": {"203.0.113.7"}}, "10.10.50.80"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := middleware.ClientAddress(forwardedRequest(tc.remote, tc.headers), tc.proxies); got != netip.MustParseAddr(tc.want) {
				t.Fatalf("ClientAddress = %v, want %s", got, tc.want)
			}
		})
	}
}

// TestClientAddressesInstallsTheResolvedAddress covers the route wrapper:
// the handlers read the resolved address from the context and as text.
func TestClientAddressesInstallsTheResolvedAddress(t *testing.T) {
	proxies := proxyList{netip.MustParsePrefix("10.10.50.80/32")}
	var address netip.Addr
	var endpoint string
	handler := middleware.ClientAddresses(proxies)(router.Route{OperationID: "GetEndpointInfo"}, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		address = middleware.ClientAddressFromContext(request.Context())
		endpoint = middleware.RemoteEndpoint(request)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), forwardedRequest("10.10.50.80:4000", map[string][]string{"X-Forwarded-For": {"203.0.113.7"}}))
	if address != netip.MustParseAddr("203.0.113.7") || endpoint != "203.0.113.7" {
		t.Fatalf("client address = %v, endpoint = %q, want 203.0.113.7", address, endpoint)
	}
	if got := middleware.RemoteEndpoint(forwardedRequest("192.168.1.20:4000", nil)); got != "192.168.1.20" {
		t.Fatalf("RemoteEndpoint without the wrapper = %q, want the transport peer", got)
	}
}

// TestSetupGateSecureCookie covers issue #603: the setup cookie carries
// Secure behind a TLS-terminating known proxy, and a direct plain-HTTP LAN
// request still gets a usable cookie without Secure.
func TestSetupGateSecureCookie(t *testing.T) {
	ingress := proxyList{netip.MustParsePrefix("10.42.0.0/16")}
	gate := middleware.NewSetupGate(&setupStartupStub{}, privateNetwork{}, ingress, "token")
	for _, tc := range []struct {
		name    string
		gate    *middleware.SetupGate
		request *http.Request
		want    bool
	}{
		{"known proxy with X-Forwarded-Proto https", gate, forwardedRequest("10.42.0.7:4000", map[string][]string{"X-Forwarded-For": {"203.0.113.9"}, "X-Forwarded-Proto": {"https"}}), true},
		{"direct plain HTTP on the LAN", gate, forwardedRequest("192.168.1.20:5000", nil), false},
		{"unknown peer claiming https", gate, forwardedRequest("192.168.1.20:5000", map[string][]string{"X-Forwarded-Proto": {"https"}}), false},
		{"known proxy with X-Forwarded-Proto http", gate, forwardedRequest("10.42.0.7:4000", map[string][]string{"X-Forwarded-Proto": {"http"}}), false},
		{"no known proxies", middleware.NewSetupGate(&setupStartupStub{}, privateNetwork{}, nil, "token"), forwardedRequest("10.42.0.7:4000", map[string][]string{"X-Forwarded-Proto": {"https"}}), false},
		{"direct TLS", gate, func() *http.Request {
			request := forwardedRequest("192.168.1.20:5000", nil)
			request.TLS = &tls.ConnectionState{}
			return request
		}(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.gate.SecureCookie(tc.request); got != tc.want {
				t.Fatalf("SecureCookie = %t, want %t", got, tc.want)
			}
		})
	}
}
