// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

// TestSourceProxyResumesAnInterruptedRead: the server goes away twice in
// one read, once mid-body and once with 503; ffmpeg still reads every byte
// of the source in order (#618).
func TestSourceProxyResumesAnInterruptedRead(t *testing.T) {
	source := bytes.Repeat([]byte("0123456789"), 1000)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer job-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch requests.Add(1) {
		case 1:
			// The server dies mid-body.
			w.Header().Set("Content-Length", "10000")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(source[:3000])
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		case 2:
			http.Error(w, "restarting", http.StatusServiceUnavailable)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(source))
	}))
	defer upstream.Close()

	fake := clock.NewFake(testEpoch)
	proxy, err := startSourceProxy(t.Context(), sourceProxyConfig{
		Client: upstream.Client(), Upstream: upstream.URL, Token: "job-token", Clock: fake, RetryDelay: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		response, err := http.Get(proxy.url)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(response.Body)
		done <- result{data: data, err: err}
	}()
	var got result
	eventually(t, func() { fake.Advance(time.Second) }, func() bool {
		select {
		case got = <-done:
			return true
		default:
			return false
		}
	})
	if got.err != nil || !bytes.Equal(got.data, source) {
		t.Fatalf("read %d bytes, %v; want the whole source", len(got.data), got.err)
	}
	if count := requests.Load(); count != 3 {
		t.Fatalf("upstream requests = %d, want 3", count)
	}

	// A refused job is not retried.
	refused, err := startSourceProxy(t.Context(), sourceProxyConfig{Client: upstream.Client(), Upstream: upstream.URL, Token: "other", Clock: fake, RetryDelay: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer refused.Close()
	response, err := http.Get(refused.url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "unauthorized") {
		t.Fatalf("refused read = %d %q", response.StatusCode, body)
	}
}

func TestParseByteRange(t *testing.T) {
	for header, want := range map[string]byteRange{"": {0, -1}, "bytes=5-": {5, -1}, "bytes=5-9": {5, 9}} {
		if got, ok := parseByteRange(header); !ok || got != want {
			t.Errorf("parseByteRange(%q) = %+v, %v", header, got, ok)
		}
	}
	for _, header := range []string{"bytes=-5", "bytes=1-2,4-5", "items=1-2", "bytes=9-5"} {
		if _, ok := parseByteRange(header); ok {
			t.Errorf("parseByteRange(%q) accepted", header)
		}
	}
	if got := (byteRange{start: 5, end: 9}).header(7); got != "bytes=7-9" {
		t.Errorf("header = %q", got)
	}
}
