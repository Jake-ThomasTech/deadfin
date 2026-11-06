// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// blockSource writes one packet, signals, then blocks until the request ends.
type blockSource struct{ started chan string }

func (b blockSource) Stream(ctx context.Context, c Channel, _ phase, w io.Writer) error {
	_, _ = w.Write([]byte{0x47})
	b.started <- c.Number
	<-ctx.Done()
	return nil
}

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

var testAnchor = time.Date(2026, 10, 12, 18, 25, 0, 0, time.UTC)

func testServer(t *testing.T, tuners int, q quirks) (*httptest.Server, *server, blockSource) {
	t.Helper()
	src := blockSource{started: make(chan string, 8)}
	lineup := append(DefaultChannels(), FaultChannels()...)
	s := newServer("", lineup, tuners, src)
	s.drm, s.guideDays, s.quirks = "9.1", 1, q
	s.now = func() time.Time { return testAnchor }
	ts := httptest.NewServer(s.routes())
	s.base = ts.URL
	t.Cleanup(ts.Close)
	return ts, s, src
}

// Upstream's fixed discovery request (HdHomerunHost.cs DiscoverDevices)
// must validate, which pins the frame and CRC layout.
func TestUpstreamDiscoveryRequestValidates(t *testing.T) {
	req := []byte{0, 2, 0, 12, 1, 4, 255, 255, 255, 255, 2, 4, 255, 255, 255, 255, 115, 204, 125, 143}
	if !isDiscoverRequest(req) {
		t.Fatal("upstream discovery request rejected")
	}
	req[5] ^= 1
	if isDiscoverRequest(req) {
		t.Fatal("corrupt request accepted")
	}
}

// Upstream accepts a reply when it has more than 13 bytes and byte 1 is 3.
func TestDiscoveryReplyShape(t *testing.T) {
	r := discoverReply(0x1050FA4E, 2, "http://10.0.0.5:5004")
	if len(r) <= 13 || r[1] != 3 {
		t.Fatalf("reply rejected by upstream check: % x", r[:4])
	}
	if !strings.Contains(string(r), "http://10.0.0.5:5004/lineup.json") {
		t.Fatal("lineup URL missing")
	}
}

func TestDiscoverAndLineup(t *testing.T) {
	ts, _, _ := testServer(t, 2, nil)
	var d discoverResponse
	getJSON(t, ts.URL+"/discover.json", &d)
	if d.TunerCount != 2 || d.LineupURL != ts.URL+"/lineup.json" || d.DeviceID != "1050FA4E" {
		t.Fatalf("discover = %+v", d)
	}
	var lineup []lineupEntry
	getJSON(t, d.LineupURL, &lineup)
	if len(lineup) != 15 || lineup[0].GuideNumber != "2.1" || lineup[0].URL != ts.URL+"/auto/v2.1" {
		t.Fatalf("lineup = %d entries, first %+v", len(lineup), lineup[0])
	}
	if lineup[5].DRM != 1 {
		t.Fatal("DRM flag missing on 9.1")
	}
}

func TestTunerLimitAndRelease(t *testing.T) {
	ts, _, src := testServer(t, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/auto/v2.1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("first tune: %v %v", err, resp)
	}
	<-src.started

	busy := get(t, ts.URL+"/auto/v4.1")
	if busy.StatusCode != http.StatusServiceUnavailable || !strings.HasPrefix(busy.Header.Get("X-HDHomeRun-Error"), "805") {
		t.Fatalf("second tune = %d %q", busy.StatusCode, busy.Header.Get("X-HDHomeRun-Error"))
	}

	cancel()
	resp.Body.Close()
	// The handler releases the tuner after the client disconnects; retry
	// until it does, bounded by the test deadline rather than a sleep. A
	// 200 proves the tuner was free. get closes the body at once, so the
	// handler can see the cancelled request before it starts the source;
	// the test must not wait for src.started here.
	for {
		again := get(t, ts.URL+"/auto/v4.1")
		if again.StatusCode == http.StatusOK {
			return
		}
		runtime.Gosched()
	}
}

func TestUnlimitedTuners(t *testing.T) {
	ts, s, src := testServer(t, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range 20 {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/auto/v4.1", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("tune: %v %v", err, resp)
		}
		defer resp.Body.Close()
		<-src.started
	}
	if active, peak := s.tuners.counts(); active != 20 || peak != 20 {
		t.Fatalf("active=%d peak=%d", active, peak)
	}
}

func TestDRMInvalidAndStatusFault(t *testing.T) {
	ts, _, _ := testServer(t, 2, nil)
	for path, want := range map[string]int{
		"/auto/v9.1":                  http.StatusForbidden,
		"/auto/v99.9":                 http.StatusNotFound,
		"/auto/v900.9":                http.StatusServiceUnavailable, // FAULT-DEAD
		"/auto/v4.1?fault=status=500": http.StatusInternalServerError,
		"/auto/v4.1?fault=bogus":      http.StatusBadRequest,
	} {
		if got := get(t, ts.URL+path).StatusCode; got != want {
			t.Errorf("%s = %d, want %d", path, got, want)
		}
	}
}

func TestHangFaultSendsHeadersOnly(t *testing.T) {
	ts, _, _ := testServer(t, 2, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/auto/v900.8", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("hang tune: %v %v", err, resp)
	}
	cancel()
	if n, _ := io.Copy(io.Discard, resp.Body); n != 0 {
		t.Fatalf("hang sent %d bytes", n)
	}
	resp.Body.Close()
}

func TestParseFaultAndPhases(t *testing.T) {
	f, err := parseFault("slowstart=8s,stall=3s/20s,die=30s,codec=atsc1|h264,bitrate=1M|25M|5M,every=45s")
	if err != nil {
		t.Fatal(err)
	}
	if f.SlowStart != 8*time.Second || f.StallFor != 3*time.Second || f.StallEvery != 20*time.Second || f.DieAfter != 30*time.Second {
		t.Fatalf("spec = %+v", f)
	}
	ph := f.phases(DefaultChannels()[2])
	if len(ph) != 3 || ph[0].Profile != "atsc1" || ph[1].Profile != "h264" || ph[2].Profile != "atsc1" || ph[2].Bitrate != "5M" || ph[1].Duration != 45*time.Second {
		t.Fatalf("phases = %+v", ph)
	}
	if one := (faultSpec{}).phases(DefaultChannels()[0]); len(one) != 1 || one[0].Duration != 0 || one[0].Profile != "atsc1" {
		t.Fatalf("no alternation = %+v", one)
	}
	for _, bad := range []string{"stall=3s", "codec=vp9", "audio=x", "nope"} {
		if _, err := parseFault(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, c := range FaultChannels() {
		if _, err := parseFault(c.Fault); err != nil {
			t.Errorf("%s: %v", c.Number, err)
		}
	}
}

// fakePacer advances its clock on Sleep, so pacing and stalls run instantly.
type fakePacer struct {
	t      time.Time
	slept  []time.Duration
	budget int
}

func (p *fakePacer) Now() time.Time { return p.t }

func (p *fakePacer) Sleep(_ context.Context, d time.Duration) error {
	p.slept = append(p.slept, d)
	p.t = p.t.Add(d)
	if p.budget--; p.budget < 0 {
		return context.Canceled
	}
	return nil
}

func TestFaultWriterSlowStartAndStall(t *testing.T) {
	p := &fakePacer{t: testAnchor, budget: 100}
	var out bytes.Buffer
	fw := &faultWriter{ctx: context.Background(), w: &out, p: p, spec: faultSpec{SlowStart: 8 * time.Second, StallFor: 3 * time.Second, StallEvery: 20 * time.Second}}
	_, _ = fw.Write([]byte("a")) // slow start
	p.t = p.t.Add(19 * time.Second)
	_, _ = fw.Write([]byte("b")) // before the stall point
	p.t = p.t.Add(1 * time.Second)
	_, _ = fw.Write([]byte("c")) // stall
	if out.String() != "abc" || len(p.slept) != 2 || p.slept[0] != 8*time.Second || p.slept[1] != 3*time.Second {
		t.Fatalf("out=%q slept=%v", out.String(), p.slept)
	}
}

func TestFaultWriterCap(t *testing.T) {
	f, err := parseFault("cap=8M,latency=150ms")
	if err != nil || f.CapBits != 8_000_000 || f.Latency != 150*time.Millisecond {
		t.Fatalf("parse = %+v, %v", f, err)
	}
	p := &fakePacer{t: testAnchor, budget: 100}
	fw := &faultWriter{ctx: context.Background(), w: io.Discard, p: p, spec: f}
	chunk := make([]byte, 100_000) // 0.1 s at 8 Mbit/s
	for range 11 {
		_, _ = fw.Write(chunk)
	}
	// 10 chunks already sent before the 11th write: 1 s of cap.
	if got := p.t.Sub(testAnchor); got != time.Second {
		t.Fatalf("1.1 MB at 8 Mbit/s took %v before the last write, want 1s", got)
	}
}

func TestPaceClipRateAndDuration(t *testing.T) {
	clip := bytes.Repeat([]byte{0x47}, 188*1000) // 188 kB per 10 s clip
	p := &fakePacer{t: testAnchor, budget: 1000}
	var out bytes.Buffer
	if err := paceClip(context.Background(), p, clip, 10*time.Second, 25*time.Second, &out); err != nil {
		t.Fatal(err)
	}
	// 25 s at 18.8 kB/s, in 100 ms chunks.
	if got, want := out.Len(), 188*1000*25/10; got != want {
		t.Fatalf("sent %d bytes, want %d", got, want)
	}
}

func TestGuideIsContiguousAndParses(t *testing.T) {
	doc := fetchGuide(t, nil, DefaultChannels(), 1)
	if len(doc.Channels) != len(DefaultChannels()) {
		t.Fatalf("channels = %d", len(doc.Channels))
	}
	last := map[string]string{}
	var sawNew, sawRerun, sawEpisode bool
	for _, p := range doc.Programmes {
		if prev, ok := last[p.Channel]; ok && prev != p.Start {
			t.Fatalf("gap on %s: %s then %s", p.Channel, prev, p.Start)
		}
		last[p.Channel] = p.Stop
		sawNew = sawNew || p.New != nil
		sawRerun = sawRerun || p.PreviouslyShown != nil
		sawEpisode = sawEpisode || len(p.EpisodeNum) == 2
	}
	if first := doc.Programmes[0].Start; first != "20261012180000 +0000" {
		t.Fatalf("anchor = %s", first)
	}
	if !sawNew || !sawRerun || !sawEpisode {
		t.Fatalf("new=%v rerun=%v episode=%v", sawNew, sawRerun, sawEpisode)
	}
}

func TestGuideQuirks(t *testing.T) {
	q, err := parseQuirks("all")
	if err != nil {
		t.Fatal(err)
	}
	chans := append(DefaultChannels(), GenerateChannels(60)...)
	// Start before the US fall-back transition (2026-11-01 06:00Z).
	var buf bytes.Buffer
	if _, err := writeGuide(&buf, "http://x", chans, time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), 2, q); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Channels   []xmltvChannel   `xml:"channel"`
		Programmes []xmltvProgramme `xml:"programme"`
	}
	if err := xml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("quirky guide is not well-formed XML: %v", err)
	}
	offsets := map[string]bool{}
	var noStop, entity, unicode, reversed bool
	for i, p := range doc.Programmes {
		offsets[p.Start[len(p.Start)-5:]] = true
		noStop = noStop || p.Stop == ""
		entity = entity || strings.Contains(p.Title, `& "Friends" <Live>`)
		unicode = unicode || strings.Contains(p.Title, "📺")
		if i > 0 && p.Channel == doc.Programmes[i-1].Channel && p.Start < doc.Programmes[i-1].Start && p.Start[len(p.Start)-5:] == "+0000" {
			reversed = true
		}
	}
	for _, want := range []string{"+0000", "-0400", "-0500", "+0530", "+1345"} {
		if !offsets[want] {
			t.Errorf("offset %s missing (have %v)", want, offsets)
		}
	}
	if len(doc.Channels) != len(chans)+1 {
		t.Errorf("duplicate channel missing: %d channels", len(doc.Channels))
	}
	if !noStop || !entity || !unicode || !reversed {
		t.Errorf("noStop=%v entity=%v unicode=%v reversed=%v", noStop, entity, unicode, reversed)
	}
	m3u := buildM3U("http://x", chans, q, nil)
	if strings.Count(m3u, "#EXTINF") != len(chans)+1 || strings.Count(m3u, "tvg-id=") >= len(chans) {
		t.Errorf("M3U quirks missing: %d entries, %d tvg-ids", strings.Count(m3u, "#EXTINF"), strings.Count(m3u, "tvg-id="))
	}
	if _, err := parseQuirks("nope"); err == nil {
		t.Error("unknown quirk accepted")
	}
}

// The guide streams: memory stays flat while output grows with channels.
func TestGuideScalesInConstantMemory(t *testing.T) {
	chans := GenerateChannels(2000)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	cw := &countWriter{}
	n, err := writeGuide(cw, "http://x", chans, testAnchor, 2, quirks{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if n < 2000*2*24 || cw.n < 40<<20 {
		t.Fatalf("programmes=%d bytes=%d", n, cw.n)
	}
	if grew := int64(after.HeapInuse) - int64(before.HeapInuse); grew > 64<<20 {
		t.Fatalf("heap grew %d MB for a %d MB guide", grew>>20, cw.n>>20)
	}
	if len(buildLineup("http://x", GenerateChannels(20000), "")) != 20000 {
		t.Fatal("20k lineup")
	}
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func TestGzipGuide(t *testing.T) {
	ts, _, _ := testServer(t, 2, nil)
	resp := get(t, ts.URL+"/xmltv.xml.gz")
	if resp.Header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
}

func TestFFmpegArgsPerProfile(t *testing.T) {
	src := ffmpegSource{bin: "ffmpeg"}
	for _, c := range append(DefaultChannels(), GenerateChannels(1)...) {
		args := strings.Join(src.args(c, phase{Profile: c.Profile}), " ")
		if !strings.Contains(args, "-re ") || !strings.HasSuffix(args, "pipe:1") || strings.Contains(args, " -t ") {
			t.Errorf("%s: %s", c.Number, args)
		}
		if c.Profile == "atsc1" && !strings.Contains(args, "+ilme+ildct") {
			t.Errorf("%s: ATSC profile not interlaced", c.Number)
		}
	}
	ph := phase{Profile: "h264", Bitrate: "25M", Size: "1920x1080", AudioCh: 6, Duration: 30 * time.Second}
	args := strings.Join(src.args(DefaultChannels()[2], ph), " ")
	if !strings.HasSuffix(args, "-t 30.000 pipe:1") {
		t.Errorf("-t must be an output option: %s", args)
	}
	for _, want := range []string{"-b:v 25M", "size=1920x1080", "-ac 6"} {
		if !strings.Contains(args, want) {
			t.Errorf("phase args missing %q: %s", want, args)
		}
	}
	loop := strings.Join(ffmpegSource{bin: "ffmpeg", loopFile: "cap.ts"}.args(DefaultChannels()[0], phase{}), " ")
	if !strings.Contains(loop, "-stream_loop -1 -i cap.ts") || !strings.Contains(loop, "-c copy") {
		t.Errorf("loop args: %s", loop)
	}
}

func fetchGuide(t *testing.T, q quirks, chans []Channel, days int) struct {
	Channels   []xmltvChannel   `xml:"channel"`
	Programmes []xmltvProgramme `xml:"programme"`
} {
	t.Helper()
	var buf bytes.Buffer
	if _, err := writeGuide(&buf, "http://x", chans, testAnchor, days, q); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Channels   []xmltvChannel   `xml:"channel"`
		Programmes []xmltvProgramme `xml:"programme"`
	}
	if err := xml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	// Do not drain: a 200 tune is an endless stream. Closing the body
	// disconnects, which releases the tuner.
	resp.Body.Close()
	return resp
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func TestCacheKeyIsAFileName(t *testing.T) {
	k := (&cacheSource{}).key(DefaultChannels()[0], phase{Profile: "atsc1"})
	if strings.ContainsAny(k, `/\: `) {
		t.Fatalf("cache key %q is not a plain file name", k)
	}
}
