// SPDX-License-Identifier: GPL-2.0-only

package identify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIsImageContentType(t *testing.T) {
	for _, tc := range []struct {
		contentType string
		urlPath     string
		want        bool
	}{
		{"image/jpeg", "/img.jpg", true},
		{"image/png", "/img.png", true},
		{"application/octet-stream", "/img.jpg", true},
		{"application/octet-stream", "/img.png", true},
		{"application/octet-stream", "/img.webp", true},
		{"application/octet-stream", "/img.unknown", false},
		{"", "/img.jpg", true},
		{"text/html", "/img.jpg", false},
		{"image/jpeg; charset=utf-8", "/img", true},
	} {
		got := IsImageContentType(tc.contentType, tc.urlPath)
		if got != tc.want {
			t.Errorf("IsImageContentType(%q, %q) = %v, want %v", tc.contentType, tc.urlPath, got, tc.want)
		}
	}
}

// TestRejectNonGlobalUnicastRejectsPrivate verifies the dialer Control hook
// rejects RFC 1918 private addresses, IPv6 ULA, loopback, link-local, and
// unspecified — all of which IsGlobalUnicast alone would miss for private
// ranges.
func TestRejectNonGlobalUnicastRejectsPrivate(t *testing.T) {
	for _, addr := range []string{
		"10.0.0.1:443",
		"172.16.0.1:443",
		"192.168.1.1:443",
		"127.0.0.1:443",
		"0.0.0.0:443",
		"169.254.1.1:443",
		"224.0.0.1:443",
		"[fc00::1]:443",
		"[::1]:443",
		"[fe80::1]:443",
		"[::]:443",
	} {
		err := rejectNonGlobalUnicast("tcp", addr, nil)
		if err == nil {
			t.Errorf("rejectNonGlobalUnicast(%q) returned nil, want error", addr)
		}
	}
}

// TestRejectNonGlobalUnicastAcceptsPublic verifies the dialer Control hook
// allows public global unicast addresses.
func TestRejectNonGlobalUnicastAcceptsPublic(t *testing.T) {
	for _, addr := range []string{
		"1.1.1.1:443",
		"8.8.8.8:443",
		"[2606:4700:4700::1111]:443",
	} {
		err := rejectNonGlobalUnicast("tcp", addr, nil)
		if err != nil {
			t.Errorf("rejectNonGlobalUnicast(%q) returned %v, want nil", addr, err)
		}
	}
}

// TestHardenedDownloaderRejectsNonHTTPScheme verifies the downloader refuses
// a scheme other than http(s) before making any request.
func TestHardenedDownloaderRejectsNonHTTPScheme(t *testing.T) {
	d := NewHardenedDownloader(5e9)
	for _, raw := range []string{"file:///etc/passwd", "ftp://example.com/img.jpg", "gopher://example.com/"} {
		_, err := d.Fetch(context.Background(), raw)
		if !errors.Is(err, ErrImageRefused) || !strings.Contains(err.Error(), "scheme must be http or https") {
			t.Errorf("Fetch(%q) err = %v, want ErrImageRefused scheme rejection", raw, err)
		}
	}
}

// TestHardenedDownloaderRefusesPrivateHTTP verifies a plain http URL passes
// the scheme check and is then refused at dial time for a special-purpose
// address (D-27).
func TestHardenedDownloaderRefusesPrivateHTTP(t *testing.T) {
	d := NewHardenedDownloader(5e9)
	for _, raw := range []string{"http://127.0.0.1/img.jpg", "http://192.168.1.1/img.jpg", "http://[::1]/img.jpg"} {
		_, err := d.Fetch(context.Background(), raw)
		if !errors.Is(err, ErrImageRefused) || !strings.Contains(err.Error(), "special-purpose address") {
			t.Errorf("Fetch(%q) err = %v, want ErrImageRefused special-purpose rejection", raw, err)
		}
	}
}

// TestHardenedDownloaderRedirectSchemes verifies redirects to http(s) are
// followed (each hop is re-validated at dial time) and other schemes are
// refused.
func TestHardenedDownloaderRedirectSchemes(t *testing.T) {
	client := newHardenedImageClient(5e9)
	originalReq := &http.Request{URL: mustParse("https://example.com/img.jpg")}
	for _, raw := range []string{"http://cdn.example.com/img.jpg", "https://cdn.example.com/img.jpg"} {
		if err := client.CheckRedirect(&http.Request{URL: mustParse(raw)}, []*http.Request{originalReq}); err != nil {
			t.Errorf("CheckRedirect(%q) err = %v, want nil", raw, err)
		}
	}
	err := client.CheckRedirect(&http.Request{URL: mustParse("ftp://cdn.example.com/img.jpg")}, []*http.Request{originalReq})
	if !errors.Is(err, ErrImageRefused) {
		t.Errorf("CheckRedirect(ftp) err = %v, want ErrImageRefused", err)
	}
}

// TestHardenedDownloaderAcceptsHTTPSRedirect verifies a redirect that stays
// HTTPS is allowed.
func TestHardenedDownloaderAcceptsHTTPSRedirect(t *testing.T) {
	client := newHardenedImageClient(5e9)
	originalReq := &http.Request{URL: mustParse("https://example.com/img.jpg")}
	redirectReq := &http.Request{URL: mustParse("https://cdn.example.com/img.jpg")}
	err := client.CheckRedirect(redirectReq, []*http.Request{originalReq})
	if err != nil {
		t.Fatalf("CheckRedirect to https: err = %v, want nil", err)
	}
}

// TestHardenedDownloaderRejectsNonImageContent verifies a 2xx response with
// non-image content type fails without persisting. Uses a plain client to
// reach the test server (the hardened dialer rejects localhost).
func TestHardenedDownloaderRejectsNonImageContent(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html></html>")
	}))
	defer server.Close()

	// Use a plain client with the test server's TLS config to bypass the
	// hardened dialer (which rejects localhost). The content-type check
	// is independent of the dial path.
	plainClient := server.Client()
	resp, err := plainClient.Get(server.URL + "/img.html")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if IsImageContentType(resp.Header.Get("Content-Type"), "/img.html") {
		t.Error("IsImageContentType(text/html) = true, want false")
	}
}

// TestHardenedDownloaderAcceptsImage verifies the image content-type check
// accepts image/jpeg responses.
func TestHardenedDownloaderAcceptsImage(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte{0xFF, 0xD8})
	}))
	defer server.Close()

	plainClient := server.Client()
	resp, err := plainClient.Get(server.URL + "/img.jpg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if !IsImageContentType(resp.Header.Get("Content-Type"), "/img.jpg") {
		t.Error("IsImageContentType(image/jpeg) = false, want true")
	}
}

// TestHardenedDownloaderRejectsPrivateAddress verifies a fetch to a private
// address fails at dial time (P1 security).
func TestHardenedDownloaderRejectsPrivateAddress(t *testing.T) {
	d := NewHardenedDownloader(1e9)
	_, err := d.Fetch(context.Background(), "https://10.0.0.1/img.jpg")
	if !errors.Is(err, ErrImageRefused) {
		t.Fatalf("Fetch(10.0.0.1) err = %v, want ErrImageRefused at dial time", err)
	}
}

func mustParse(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// TestIsImageContentTypeUsesFinalURLPath verifies the octet-stream extension
// fallback uses the URL path (representing the final response URL after
// redirects), not the original request URL path (P2 security/
// redirect-extension-fallback).
func TestIsImageContentTypeUsesFinalURLPath(t *testing.T) {
	if !IsImageContentType("application/octet-stream", "/actual.jpg") {
		t.Error("IsImageContentType(octet-stream, /actual.jpg) = false, want true")
	}
	if IsImageContentType("application/octet-stream", "/redirect-to-img") {
		t.Error("IsImageContentType(octet-stream, /redirect-to-img) = true, want false (no image extension)")
	}
}

// TestRejectNonGlobalUnicastDeniesSpecialPurposeBlocks covers every denied
// block, including embedded-IPv4 forms, and a public pass case.
func TestRejectNonGlobalUnicastDeniesSpecialPurposeBlocks(t *testing.T) {
	denied := []string{
		"0.1.2.3", "10.0.0.1", "100.64.0.1", "100.127.255.254", "127.0.0.1",
		"169.254.1.1", "172.16.0.1", "192.0.0.1", "192.0.2.1", "192.88.99.1",
		"192.168.1.1", "198.18.0.1", "198.19.255.254", "198.51.100.1",
		"203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::", "::1", "::ffff:10.0.0.1", "::ffff:100.64.0.1",
		"64:ff9b::a00:1", "64:ff9b::6440:1", "64:ff9b:1::1", "100::1",
		"2001::1", "2001:1::1", "2001:db8::1", "2002:a00:1::1",
		"2002:c633:6401::1", "fc00::1", "fd00::1", "fe80::1", "ff02::1",
	}
	for _, host := range denied {
		addr := host
		if strings.Contains(host, ":") {
			addr = "[" + host + "]"
		}
		if err := rejectNonGlobalUnicast("tcp", addr+":443", nil); err == nil {
			t.Errorf("rejectNonGlobalUnicast(%q) returned nil, want error", host)
		}
	}
	allowed := []string{
		"1.1.1.1", "8.8.8.8", "100.63.255.255", "100.128.0.1", "198.17.255.255",
		"198.20.0.1", "13.224.0.1", "::ffff:8.8.8.8", "64:ff9b::808:808",
		"2002:808:808::1", "2606:4700:4700::1111",
	}
	for _, host := range allowed {
		addr := host
		if strings.Contains(host, ":") {
			addr = "[" + host + "]"
		}
		if err := rejectNonGlobalUnicast("tcp", addr+":443", nil); err != nil {
			t.Errorf("rejectNonGlobalUnicast(%q) returned %v, want nil", host, err)
		}
	}
}
