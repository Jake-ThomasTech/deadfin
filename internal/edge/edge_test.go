// SPDX-License-Identifier: GPL-2.0-only

package edge

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/hls"
)

// edgeRig is an edge in front of a server that can stop answering.
type edgeRig struct {
	edge   *Edge
	server *httptest.Server
	front  *httptest.Server
	down   atomic.Bool
	hits   atomic.Int32
	signer *Signer
	clock  *clock.Fake
	family Family
	item   dto.UUID
}

func newEdgeRig(t *testing.T) *edgeRig {
	t.Helper()
	rig := &edgeRig{signer: testSigner(t, "shared-edge-key-0123456789"), clock: clock.NewFake(testEpoch),
		family: Family{Stem: "f00d", Pattern: "f00d%d.ts"}, item: dto.UUID{0xab, 0x01}}
	rig.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.hits.Add(1)
		if rig.down.Load() {
			http.Error(w, "restarting", http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, "server:"+r.URL.Path)
	}))
	t.Cleanup(rig.server.Close)
	upstream, _ := url.Parse(rig.server.URL)
	e, err := New(Config{Signer: rig.signer, Upstream: upstream, StoreDirectory: t.TempDir(), Clock: rig.clock, WaitTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	rig.edge = e
	rig.front = httptest.NewServer(e)
	t.Cleanup(func() {
		rig.front.Close()
		e.Close()
	})
	return rig
}

func (rig *edgeRig) readGrant() string {
	return rig.signer.SignRead(ReadGrant{ItemID: rig.item, PlaySessionID: "play-1", Family: rig.family, Expires: testEpoch.Add(time.Hour)})
}

func (rig *edgeRig) upload(t *testing.T, name, body, grant string) int {
	t.Helper()
	data := []byte(body)
	if strings.HasSuffix(name, ".ts") && body != "not a segment" {
		data = tsPacket(body)
	}
	request, err := http.NewRequest(http.MethodPut, rig.front.URL+PathUpload+name, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if grant != "" {
		request.Header.Set(GrantHeader, grant)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

func (rig *edgeRig) get(t *testing.T, path string) (int, string, string) {
	t.Helper()
	response, err := http.Get(rig.front.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, tsText(body), response.Header.Get("Content-Type")
}

// tsPacket wraps text in one MPEG-TS packet, so a test segment passes the
// family file check (#624). tsText reverses it.
func tsPacket(text string) []byte {
	packet := bytes.Repeat([]byte{0xff}, hls.TSPacketSize)
	packet[0] = hls.TSSyncByte
	copy(packet[1:], text)
	return packet
}

func tsText(data []byte) string {
	if len(data) != hls.TSPacketSize || data[0] != hls.TSSyncByte {
		return string(data)
	}
	return string(bytes.TrimRight(data[1:], "\xff"))
}

func (rig *edgeRig) segmentPath(index string, grant string) string {
	path := "/jellyfin/videos/" + rig.item.Dashed() + "/hls1/main/" + index + ".ts?PlaySessionId=play-1&api_key=t"
	if grant != "" {
		path += "&" + GrantQuery + "=" + url.QueryEscape(grant)
	}
	return path
}

// TestEdgeServesMirroredSegmentsWithAGrant: an uploaded segment is served
// to a request with a valid grant, with no server request; every other
// request goes to the server.
func TestEdgeServesMirroredSegmentsWithAGrant(t *testing.T) {
	before := goleak.IgnoreCurrent()
	t.Cleanup(func() {
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		goleak.VerifyNone(t, before)
	})
	rig := newEdgeRig(t)
	upload := rig.signer.SignUpload(UploadGrant{Family: rig.family, Expires: testEpoch.Add(time.Hour)})
	if code := rig.upload(t, "f00d3.ts", "segment three", upload); code != http.StatusNoContent {
		t.Fatalf("upload = %d", code)
	}
	if code := rig.upload(t, "f00d4.ts", "x", ""); code != http.StatusUnauthorized {
		t.Fatalf("upload without a grant = %d, want 401", code)
	}
	if code := rig.upload(t, "beef4.ts", "x", upload); code != http.StatusForbidden {
		t.Fatalf("upload outside the family = %d, want 403", code)
	}
	if code := rig.upload(t, "f00d.m3u8", "x", upload); code != http.StatusForbidden {
		t.Fatalf("playlist upload = %d, want 403", code)
	}
	if code := rig.upload(t, "f00d5.ts", "not a segment", upload); code != http.StatusUnprocessableEntity {
		t.Fatalf("malformed segment upload = %d, want 422", code)
	}

	code, body, contentType := rig.get(t, rig.segmentPath("3", rig.readGrant()))
	if code != http.StatusOK || body != "segment three" || contentType != "video/mp2t" || rig.hits.Load() != 0 {
		t.Fatalf("granted segment = %d %q %q, server hits %d", code, body, contentType, rig.hits.Load())
	}
	for name, path := range map[string]string{
		"no grant":     rig.segmentPath("3", ""),
		"wrong item":   strings.Replace(rig.segmentPath("3", rig.readGrant()), rig.item.Dashed(), dto.UUID{0xcd}.Dashed(), 1),
		"wrong player": strings.Replace(rig.segmentPath("3", rig.readGrant()), "play-1", "play-2", 1),
		"playlist":     "/videos/" + rig.item.Dashed() + "/main.m3u8?" + GrantQuery + "=" + url.QueryEscape(rig.readGrant()),
	} {
		before := rig.hits.Load()
		if code, body, _ := rig.get(t, path); code != http.StatusOK || !strings.HasPrefix(body, "server:") || rig.hits.Load() != before+1 {
			t.Fatalf("%s: %d %q; want the server's answer", name, code, body)
		}
	}
	rig.clock.Advance(time.Hour)
	if code, body, _ := rig.get(t, rig.segmentPath("3", rig.readGrant())); !strings.HasPrefix(body, "server:") {
		t.Fatalf("expired grant: %d %q; want the server's answer", code, body)
	}
}

// TestEdgeWaitsForTheMirrorWhileTheServerIsDown: with the server down, a
// granted request for a segment the edge does not have yet waits for the
// worker's upload; past the wait it gets 503 with Retry-After.
func TestEdgeWaitsForTheMirrorWhileTheServerIsDown(t *testing.T) {
	rig := newEdgeRig(t)
	rig.down.Store(true)
	upload := rig.signer.SignUpload(UploadGrant{Family: rig.family, Expires: testEpoch.Add(time.Hour)})
	type result struct {
		code int
		body string
	}
	results := make(chan result, 1)
	go func() {
		response, err := http.Get(rig.front.URL + rig.segmentPath("5", rig.readGrant()))
		if err != nil {
			results <- result{}
			return
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		results <- result{response.StatusCode, tsText(body)}
	}()
	// The request reaches the down server before the worker uploads.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for waiting := true; waiting && rig.hits.Load() == 0; {
		select {
		case <-deadline.C:
			waiting = false
		case <-time.After(5 * time.Millisecond):
		}
	}
	if code := rig.upload(t, "f00d5.ts", "segment five", upload); code != http.StatusNoContent {
		t.Fatalf("upload = %d", code)
	}
	got := <-results
	if got.code != http.StatusOK || got.body != "segment five" {
		t.Fatalf("waiting request = %d %q", got.code, got.body)
	}

	response, err := http.Get(rig.front.URL + rig.segmentPath("6", rig.readGrant()))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") == "" {
		t.Fatalf("missing segment while down = %d, Retry-After %q", response.StatusCode, response.Header.Get("Retry-After"))
	}
	if code, _, _ := rig.get(t, rig.segmentPath("6", "")); code != http.StatusBadGateway {
		t.Fatalf("ungranted request while down = %d, want the server's 502", code)
	}
}
