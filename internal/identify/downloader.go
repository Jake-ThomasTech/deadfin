// SPDX-License-Identifier: GPL-2.0-only

package identify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Downloader is the byte-oriented fetch seam for remote image downloads
// (ring security/remote-image-download@v1, option 1). Implementations enforce
// transport hardening: http(s) only, dial-time rejection of non-global-unicast
// addresses on every redirect hop, response bounded, and validated as image
// content.
type Downloader interface {
	Fetch(ctx context.Context, imageURL string) ([]byte, error)
}

// MaxRemoteImageBytes bounds a downloaded remote image body. Provider posters
// and backdrops are well under this; the bound prevents a hostile or broken
// provider from exhausting memory.
const MaxRemoteImageBytes = 32 << 20

// ErrImageRefused marks a download that the hardened downloader refused: a
// scheme other than http(s), a special-purpose destination address, a body
// over MaxRemoteImageBytes, or a response that is not an image. The wrapping
// error text names the reason.
var ErrImageRefused = errors.New("remote image refused")

// HardenedDownloader adapts a hardened HTTP transport to the Downloader seam
// (D-27). It enforces the transport hardening that defense-in-depth requires:
// scheme must be http or https, as in stock; the transport uses no proxy, and
// the dialer Control hook rejects any connection to a non-global-unicast
// address (loopback, private, link-local, multicast, unspecified), so every
// redirect hop is re-validated at dial time; the response is bounded by
// MaxRemoteImageBytes and must be 2xx with an image/* Content-Type, with the
// upstream octet-stream fallback that derives the type from the URL-path
// extension (ProviderManager.cs:227-236).
type HardenedDownloader struct{ client *http.Client }

// NewHardenedDownloader builds a Downloader with a hardened HTTP client.
func NewHardenedDownloader(timeout time.Duration) HardenedDownloader {
	return HardenedDownloader{client: newHardenedImageClient(timeout)}
}

// Fetch downloads and validates a remote image. The URL must be http(s); every
// redirect hop is re-validated for scheme and destination address. The response
// body must be 2xx, bounded by MaxRemoteImageBytes, and have an image content
// type (with the octet-stream/extension fallback).
func (d HardenedDownloader) Fetch(ctx context.Context, imageURL string) ([]byte, error) {
	parsed, err := url.Parse(imageURL)
	if err != nil {
		return nil, fmt.Errorf("remote image: invalid url: %w", err)
	}
	if !isHTTPScheme(parsed.Scheme) {
		return nil, fmt.Errorf("%w: scheme must be http or https, got %q", ErrImageRefused, parsed.Scheme)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("remote image: build request: %w", err)
	}
	response, err := d.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("remote image: fetch: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("remote image: status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxRemoteImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("remote image: read body: %w", err)
	}
	if len(body) > MaxRemoteImageBytes {
		return nil, fmt.Errorf("%w: body exceeds %d bytes", ErrImageRefused, MaxRemoteImageBytes)
	}
	if !IsImageContentType(response.Header.Get("Content-Type"), response.Request.URL.Path) {
		return nil, fmt.Errorf("%w: response is not an image", ErrImageRefused)
	}
	return body, nil
}

// IsImageContentType reports whether contentType is image/*. When the server
// sends application/octet-stream (or an empty type), the type is derived from
// the URL-path extension, mirroring upstream's fallback
// (ProviderManager.cs:227-236).
func IsImageContentType(contentType, urlPath string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err == nil && strings.HasPrefix(mediaType, "image/") {
		return true
	}
	if mediaType != "application/octet-stream" && contentType != "" {
		return false
	}
	ext := strings.ToLower(filepath.Ext(urlPath))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".tiff", ".tif":
		return true
	}
	return false
}

// isHTTPScheme reports whether scheme is http or https.
func isHTTPScheme(scheme string) bool {
	scheme = strings.ToLower(scheme)
	return scheme == "http" || scheme == "https"
}

// newHardenedImageClient builds an *http.Client whose dialer rejects any
// connection to a non-global-unicast address, so every redirect hop is
// re-validated at dial time. The transport has no proxy, so the dial-time
// check sees the real destination for http and https alike. The CheckRedirect
// hook rejects any redirect whose scheme is not http(s). The redirect count is
// capped at Go's default 10.
func newHardenedImageClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, Control: rejectNonGlobalUnicast}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:       dialer.DialContext,
			ForceAttemptHTTP2: true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("remote image: stopped after 10 redirects")
			}
			if !isHTTPScheme(req.URL.Scheme) {
				return fmt.Errorf("%w: redirect to scheme %q", ErrImageRefused, req.URL.Scheme)
			}
			return nil
		},
	}
}

// deniedPrefixes lists the IANA special-purpose blocks the downloader refuses
// at dial time. IPv4-mapped (::ffff:0:0/96), NAT64 (64:ff9b::/96) and 6to4
// (2002::/16) addresses are handled by isDeniedAddr, which checks the
// embedded IPv4 address.
func deniedPrefixes() []netip.Prefix {
	blocks := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
		"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"255.255.255.255/32",
		"::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "2001::/23",
		"2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	}
	prefixes := make([]netip.Prefix, len(blocks))
	for i, block := range blocks {
		prefixes[i] = netip.MustParsePrefix(block)
	}
	return prefixes
}

// isDeniedAddr reports whether addr falls in a special-purpose block.
func isDeniedAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range deniedPrefixes() {
		if prefix.Contains(addr) {
			return true
		}
	}
	if !addr.Is6() {
		return false
	}
	raw := addr.As16()
	switch {
	case netip.MustParsePrefix("64:ff9b::/96").Contains(addr):
		return isDeniedAddr(netip.AddrFrom4([4]byte(raw[12:16])))
	case netip.MustParsePrefix("2002::/16").Contains(addr):
		return isDeniedAddr(netip.AddrFrom4([4]byte(raw[2:6])))
	}
	return false
}

// rejectNonGlobalUnicast is a dialer Control hook that refuses connections to
// special-purpose addresses (see deniedPrefixes). It runs on every dial,
// including each redirect hop, before the connection is established.
func rejectNonGlobalUnicast(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	parsed, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if isDeniedAddr(parsed) {
		return fmt.Errorf("%w: special-purpose address %s", ErrImageRefused, host)
	}
	return nil
}
