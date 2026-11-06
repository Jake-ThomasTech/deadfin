// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSegs serves numbered fake segments, so tests need no ffmpeg.
type fakeSegs struct{ content, ad int }

func (f fakeSegs) segments(_ context.Context, c Channel, kind string, _ time.Duration) ([][]byte, error) {
	n := f.content
	if kind == "ad" {
		n = f.ad
	}
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte(fmt.Sprintf("%s %s %d", c.Number, kind, i))
	}
	return out, nil
}

var testEpoch = time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)

func fastServer(t *testing.T, now *time.Time) (*httptest.Server, *server) {
	t.Helper()
	s := newServer("", append(DefaultChannels(), FASTChannels()...), 2, blockSource{started: make(chan string, 8)})
	s.clock = func() time.Time { return *now }
	s.epoch = testEpoch
	s.sign = signer{secret: []byte("test secret")}
	s.segs = fakeSegs{content: 15, ad: 8}
	ts := httptest.NewServer(s.routes())
	s.base = ts.URL
	t.Cleanup(ts.Close)
	return ts, s
}

func body(t *testing.T, u string) (int, string) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestParseHLSPlan(t *testing.T) {
	p, err := parseHLSPlan("ads=120s/32s,cue=scte35,token=60s,geo,unmarked,stale=20s/120s,seg=4s,window=5")
	if err != nil {
		t.Fatal(err)
	}
	want := hlsPlan{Seg: 4 * time.Second, Window: 5, AdEvery: 120 * time.Second, AdLen: 32 * time.Second, Cue: "scte35",
		TokenTTL: time.Minute, Geo: true, Unmarked: true, StaleFor: 20 * time.Second, StaleEvr: 120 * time.Second}
	if p != want {
		t.Fatalf("plan = %+v", p)
	}
	for _, bad := range []string{"ads=120s", "ads=10s/30s", "cue=vast", "window=0", "bogus"} {
		if _, err := parseHLSPlan(bad); err == nil {
			t.Errorf("parseHLSPlan(%q) accepted", bad)
		}
	}
	for _, c := range append(FASTChannels(), GenerateFASTChannels(8)...) {
		if _, err := parseHLSPlan(c.HLS); err != nil {
			t.Errorf("%s: %v", c.Number, err)
		}
	}
}

// segmentDSNs maps each segment's media sequence number to its
// discontinuity sequence number and URI, as a client computes them.
func segmentDSNs(t *testing.T, pl string) map[int64]string {
	t.Helper()
	out := map[int64]string{}
	var seq, dsn int64
	first := true
	for _, line := range strings.Split(pl, "\n") {
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			seq, _ = strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			dsn, _ = strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"), 10, 64)
		case line == "#EXT-X-DISCONTINUITY":
			if first {
				t.Fatal("discontinuity tag before the first segment")
			}
			dsn++
		case line != "" && !strings.HasPrefix(line, "#"):
			out[seq] = fmt.Sprintf("%d %s", dsn, line)
			seq++
			first = false
		}
	}
	return out
}

func TestSSAIPlaylist(t *testing.T) {
	p, _ := parseHLSPlan("ads=120s/32s,cue=scte35")
	// 30 content segments, then an 8-segment break (seq 30-37).
	pl := p.livePlaylist(testEpoch.Add(32*4*time.Second), testEpoch, 15, "")
	for _, want := range []string{
		"#EXT-X-MEDIA-SEQUENCE:26\n",
		"#EXT-X-DISCONTINUITY\n#EXT-X-PROGRAM-DATE-TIME:2026-10-12T18:02:00.000Z\n#EXT-X-CUE-OUT:DURATION=32.000\n#EXTINF:4.000,\nseg/ad/0.ts\n",
		"#EXT-X-CUE-OUT-CONT:ElapsedTime=4.000,Duration=32.000\n#EXTINF:4.000,\nseg/ad/1.ts\n",
	} {
		if !strings.Contains(pl, want) {
			t.Fatalf("missing %q in\n%s", want, pl)
		}
	}
	// Seq 38 resumes content at 38 % 15 = 8, after a marked return.
	pl = p.livePlaylist(testEpoch.Add(39*4*time.Second), testEpoch, 15, "")
	if !strings.Contains(pl, "seg/ad/7.ts\n#EXT-X-DISCONTINUITY\n#EXT-X-PROGRAM-DATE-TIME:2026-10-12T18:02:32.000Z\n#EXT-X-CUE-IN\n#EXTINF:4.000,\nseg/content/8.ts\n") {
		t.Fatalf("no cue-in at the break end:\n%s", pl)
	}
}

func TestDiscontinuitySequenceIsStable(t *testing.T) {
	for _, plan := range []string{"hls", "ads=60s/16s,cue=scte35", "unmarked"} {
		p, _ := parseHLSPlan(plan)
		prev := map[int64]string{}
		for step := range 200 {
			cur := segmentDSNs(t, p.livePlaylist(testEpoch.Add(time.Duration(step)*4*time.Second), testEpoch, 15, ""))
			for seq, v := range cur {
				if old, ok := prev[seq]; ok && old != v {
					t.Fatalf("%s: segment %d was %q, now %q", plan, seq, old, v)
				}
			}
			prev = cur
		}
	}
}

func TestLoopPointMarkedOrNot(t *testing.T) {
	at := testEpoch.Add(17 * 4 * time.Second) // window holds seq 11-16; seq 15 wraps to content 0
	marked, _ := parseHLSPlan("hls")
	unmarked, _ := parseHLSPlan("unmarked")
	if !strings.Contains(marked.livePlaylist(at, testEpoch, 15, ""), "#EXT-X-DISCONTINUITY\n#EXT-X-PROGRAM-DATE-TIME:2026-10-12T18:01:00.000Z\n#EXTINF:4.000,\nseg/content/0.ts") {
		t.Fatal("marked loop point has no discontinuity")
	}
	if strings.Contains(unmarked.livePlaylist(at, testEpoch, 15, ""), "DISCONTINUITY\n") {
		t.Fatal("unmarked loop point has a discontinuity")
	}
}

func TestStalePlaylistFreezes(t *testing.T) {
	p, _ := parseHLSPlan("stale=20s/120s")
	frozen := p.livePlaylist(testEpoch.Add(120*time.Second), testEpoch, 15, "")
	if got := p.livePlaylist(testEpoch.Add(139*time.Second), testEpoch, 15, ""); got != frozen {
		t.Fatal("playlist moved inside the stale window")
	}
	if got := p.livePlaylist(testEpoch.Add(144*time.Second), testEpoch, 15, ""); got == frozen {
		t.Fatal("playlist still frozen after the stale window")
	}
}

func TestDateRangeCues(t *testing.T) {
	p, _ := parseHLSPlan("ads=120s/32s,cue=daterange")
	for at, re := range map[int]string{
		33: `#EXT-X-DATERANGE:ID="break-0",START-DATE="2026-10-12T18:02:00.000Z",PLANNED-DURATION=32.000,SCTE35-OUT=0xFC30[0-9A-F]{8}`,
		40: `#EXT-X-DATERANGE:ID="break-0",START-DATE="2026-10-12T18:02:00.000Z",SCTE35-IN=0xFC30[0-9A-F]{8}`,
	} {
		pl := p.livePlaylist(testEpoch.Add(time.Duration(at)*4*time.Second), testEpoch, 15, "")
		if !regexp.MustCompile(re).MatchString(pl) {
			t.Fatalf("missing %s in\n%s", re, pl)
		}
	}
}

func TestFASTChannelsAreM3UOnly(t *testing.T) {
	now := testEpoch.Add(time.Hour)
	ts, _ := fastServer(t, &now)
	var lineup []lineupEntry
	getJSON(t, ts.URL+"/lineup.json", &lineup)
	for _, e := range lineup {
		if strings.HasPrefix(e.GuideNumber, "800.") {
			t.Fatalf("FAST channel %s in the HDHomeRun lineup", e.GuideNumber)
		}
	}
	if code, _ := body(t, ts.URL+"/auto/v800.1"); code != http.StatusNotFound {
		t.Fatalf("tune of an HLS channel = %d", code)
	}
	_, m3u := body(t, ts.URL+"/playlist.m3u")
	if !strings.Contains(m3u, ts.URL+"/hls/800.1/master.m3u8\n") {
		t.Fatalf("M3U lacks the HLS URL:\n%s", m3u)
	}
	if _, lm := body(t, ts.URL+"/lineup.m3u"); lm != m3u {
		t.Fatal("/lineup.m3u differs from /playlist.m3u")
	}
	code, master := body(t, ts.URL+"/hls/800.2/master.m3u8")
	if code != 200 || !strings.Contains(master, "\nmedia.m3u8\n") {
		t.Fatalf("master = %d %q", code, master)
	}
	if code, b := body(t, ts.URL+"/hls/800.2/seg/ad/9.ts"); code != 200 || b != "800.2 ad 1" {
		t.Fatalf("ad segment 9 of 8 = %d %q", code, b)
	}
}

func TestTokenExpiry(t *testing.T) {
	now := testEpoch.Add(time.Hour)
	ts, s := fastServer(t, &now)
	_, m3u := body(t, ts.URL+"/playlist.m3u")
	m := regexp.MustCompile(`(?m)^(\S+/hls/800\.4/master\.m3u8\?\S+)$`).FindStringSubmatch(m3u)
	if m == nil {
		t.Fatalf("no tokenized URL in\n%s", m3u)
	}
	master := m[1]
	if code, _ := body(t, master); code != 200 {
		t.Fatalf("fresh token = %d", code)
	}
	u, _ := url.Parse(master)
	media := ts.URL + "/hls/800.4/media.m3u8?" + u.RawQuery
	_, pl := body(t, media)
	if !strings.Contains(pl, ".ts?"+u.RawQuery+"\n") {
		t.Fatalf("segment URLs lack the token:\n%s", pl)
	}
	bad := strings.Replace(master, "tok=", "tok=0", 1)
	if code, b := body(t, bad); code != 403 || !strings.Contains(b, "bad token") {
		t.Fatalf("bad token = %d %q", code, b)
	}
	now = now.Add(121 * time.Second)
	if code, b := body(t, media); code != 403 || !strings.Contains(b, "expired") {
		t.Fatalf("expired token = %d %q", code, b)
	}
	if code, _ := body(t, ts.URL+"/hls/800.4/master.m3u8"); code != 403 {
		t.Fatalf("no token = %d", code)
	}
	if s.tokenRejects.Load() != 3 {
		t.Fatalf("token rejects = %d", s.tokenRejects.Load())
	}
	// A fresh M3U fetch gives a working URL again: the refresh hook.
	_, m3u = body(t, ts.URL+"/playlist.m3u")
	if code, _ := body(t, regexp.MustCompile(`(?m)^(\S+/hls/800\.4/master\.m3u8\?\S+)$`).FindStringSubmatch(m3u)[1]); code != 200 {
		t.Fatalf("refreshed token = %d", code)
	}
}

func TestGeoBlock(t *testing.T) {
	now := testEpoch.Add(time.Hour)
	ts, s := fastServer(t, &now)
	if code, b := body(t, ts.URL+"/hls/800.5/master.m3u8"); code != 403 || !strings.Contains(b, "region") {
		t.Fatalf("geo channel = %d %q", code, b)
	}
	_, n, _ := net.ParseCIDR("127.0.0.0/8")
	s.geoAllow = []*net.IPNet{n}
	if code, _ := body(t, ts.URL+"/hls/800.5/master.m3u8"); code != 200 {
		t.Fatalf("allowed client = %d", code)
	}
	if s.geoBlocks.Load() != 1 {
		t.Fatalf("geo blocks = %d", s.geoBlocks.Load())
	}
}
