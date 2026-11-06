// SPDX-License-Identifier: GPL-2.0-only

// Command fakehdhr is a synthetic HDHomeRun network tuner and a Live TV
// limits harness. It serves the HTTP API that Jellyfin's HdHomerunHost reads
// (/discover.json, /lineup.json, /lineup_status.json, /auto/v<channel>),
// answers UDP discovery on port 65001, enforces a tuner limit, and publishes
// the same lineup as M3U (/playlist.m3u) with an XMLTV guide (/xmltv.xml,
// /xmltv.xml.gz).
//
// For limits work it scales the lineup (-channels 20000), streams guides of
// any size in constant memory (-guide-days 14), injects real-world guide and
// playlist quirks (-quirks all), puts controllable stream faults on the 900.x
// channels or any tune (?fault=stall=3s/20s, see faults.go), and serves
// hundreds of concurrent tunes without an ffmpeg per viewer (-source cache).
// The 800.x channels are FAST-style live HLS for the M3U path: SSAI ad
// breaks with discontinuities and SCTE-35 cues, expiring tokens, geo
// blocks, unmarked timestamp jumps and stale playlists (fast.go).
// /stats.json reports what the tuner side saw.
//
// Throwaway prototype from the Live TV spike; not part of the server. See
// docs/spikes/livetv.md §5.
package main

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type server struct {
	base      string
	deviceID  uint32
	model     string
	tuners    *tunerPool
	channels  []Channel
	byNumber  map[string]int
	drm       string
	guideDays int
	quirks    quirks
	now       func() time.Time
	source    Source
	pace      pacer
	dropAfter time.Duration

	// FAST-style HLS channels (fast.go).
	clock    func() time.Time // wall clock for live playlists and tokens; s.now may be a fixed guide anchor
	epoch    time.Time        // start of every HLS channel's live timeline
	sign     signer
	segs     segStore
	geoAllow []*net.IPNet

	opens, rejects, faults atomic.Int64
	bytes                  atomic.Int64

	hlsOpens, hlsSegments, tokenRejects, geoBlocks atomic.Int64

	xt                                  *xtream // nil = no Xtream panel
	xtRequests, authRejects, banRejects atomic.Int64
	jsonEPGCalls                        atomic.Int64

	// User-Agent per request kind, and the blocking of tool defaults.
	uaMu      sync.Mutex
	userAgent map[string]int // "kind|ua" -> requests
	blockUA   bool

	truth []truthEntry // canonical identities under -persona
}

func newServer(base string, channels []Channel, tuners int, src Source) *server {
	s := &server{
		base: strings.TrimSuffix(base, "/"), deviceID: 0x1050FA4E, model: "HDHR5-4K",
		tuners: newTunerPool(tuners), channels: channels, byNumber: map[string]int{},
		guideDays: 3, quirks: quirks{}, now: time.Now, source: src, pace: realPacer{},
		clock: time.Now, epoch: time.Now().Truncate(time.Minute),
	}
	for i, c := range channels {
		s.byNumber[c.Number] = i
	}
	return s
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /discover.json", s.discover)
	mux.HandleFunc("GET /lineup.json", s.lineup)
	mux.HandleFunc("GET /lineup_status.json", s.lineupStatus)
	mux.HandleFunc("GET /status.json", s.status)
	mux.HandleFunc("GET /stats.json", s.stats)
	mux.HandleFunc("GET /playlist.m3u", s.m3u)
	// An HDHomeRun serves the same playlist as /lineup.m3u.
	mux.HandleFunc("GET /lineup.m3u", s.m3u)
	mux.HandleFunc("GET /xmltv.xml", s.guide)
	mux.HandleFunc("GET /xmltv.xml.gz", s.guide)
	mux.HandleFunc("GET /logo/{file}", s.logo)
	mux.HandleFunc("GET /auto/{channel}", s.stream)
	mux.HandleFunc("GET /hls/{channel}/master.m3u8", s.hlsMaster)
	mux.HandleFunc("GET /hls/{channel}/media.m3u8", s.hlsMedia)
	mux.HandleFunc("GET /hls/{channel}/seg/{kind}/{file}", s.hlsSegment)
	mux.HandleFunc("GET /truth.json", s.truthJSON)
	mux.HandleFunc("GET /get.php", s.xtreamGet)
	mux.HandleFunc("GET /xmltv.php", s.xtreamGuide)
	mux.HandleFunc("GET /player_api.php", s.xtreamAPI)
	mux.HandleFunc("GET /live/{user}/{pass}/{file}", s.xtreamLive)
	mux.HandleFunc("GET /movie/{user}/{pass}/{file}", s.xtreamVOD)
	mux.HandleFunc("GET /series/{user}/{pass}/{file}", s.xtreamVOD)
	return s.recordUA(mux)
}

// requestKind classifies a request for the User-Agent record.
func requestKind(path string) string {
	switch {
	case strings.HasPrefix(path, "/auto/"), strings.HasPrefix(path, "/live/"), strings.HasPrefix(path, "/hls/"),
		strings.HasPrefix(path, "/movie/"), strings.HasPrefix(path, "/series/"):
		return "stream"
	case strings.HasPrefix(path, "/xmltv"):
		return "guide"
	case path == "/playlist.m3u", path == "/lineup.m3u", path == "/get.php", path == "/lineup.json":
		return "lineup"
	case path == "/player_api.php":
		return "api"
	}
	return ""
}

// defaultToolUA reports a User-Agent that some providers block: none, or
// the default of ffmpeg (Lavf) or Go's HTTP client.
func defaultToolUA(ua string) bool {
	return ua == "" || strings.HasPrefix(ua, "Lavf") || strings.HasPrefix(ua, "Go-http-client")
}

// recordUA counts the User-Agent of provider-side requests and, with
// -block-default-ua, answers 403 to the default tool agents, as some IPTV
// providers do.
func (s *server) recordUA(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := requestKind(r.URL.Path)
		if kind == "" {
			next.ServeHTTP(w, r)
			return
		}
		ua := r.UserAgent()
		s.uaMu.Lock()
		if s.userAgent == nil {
			s.userAgent = map[string]int{}
		}
		s.userAgent[kind+"|"+ua]++
		s.uaMu.Unlock()
		if s.blockUA && defaultToolUA(ua) {
			http.Error(w, "blocked user agent", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) userAgents() map[string]int {
	s.uaMu.Lock()
	defer s.uaMu.Unlock()
	return maps.Clone(s.userAgent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) discover(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, discoverResponse{
		FriendlyName:    "Deadfin Fake HDHomeRun",
		ModelNumber:     s.model,
		FirmwareName:    "hdhomerun_fake",
		FirmwareVersion: "20260101",
		DeviceID:        fmt.Sprintf("%08X", s.deviceID),
		DeviceAuth:      "fakeauth",
		BaseURL:         s.base,
		LineupURL:       s.base + "/lineup.json",
		TunerCount:      s.tuners.limit,
	})
}

func (s *server) lineup(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, buildLineup(s.base, s.channels, s.drm))
}

func (s *server) lineupStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ScanInProgress": 0, "ScanPossible": 1, "Source": "Antenna", "SourceList": []string{"Antenna", "Cable"}})
}

func (s *server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.tuners.status())
}

// stats reports the tuner side of a limits run, for comparison with what
// the server under test claims.
func (s *server) stats(w http.ResponseWriter, _ *http.Request) {
	active, peak := s.tuners.counts()
	writeJSON(w, map[string]any{
		"Channels": len(s.channels), "ActiveStreams": active, "PeakStreams": peak,
		"Opens": s.opens.Load(), "Rejected": s.rejects.Load(), "FaultedOpens": s.faults.Load(),
		"BytesSent": s.bytes.Load(),
		"HLSOpens":  s.hlsOpens.Load(), "HLSSegments": s.hlsSegments.Load(),
		"TokenRejects": s.tokenRejects.Load(), "GeoBlocks": s.geoBlocks.Load(),
		"XtreamRequests": s.xtRequests.Load(), "AuthRejects": s.authRejects.Load(), "BanRejects": s.banRejects.Load(),
		"JSONEPGCalls": s.jsonEPGCalls.Load(), "UserAgents": s.userAgents(),
	})
}

func (s *server) m3u(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "audio/x-mpegurl")
	_, _ = io.WriteString(w, buildM3U(s.base, s.channels, s.quirks, s.streamURL))
}

func (s *server) guide(w http.ResponseWriter, r *http.Request) {
	var out io.Writer = w
	if strings.HasSuffix(r.URL.Path, ".gz") {
		w.Header().Set("Content-Type", "application/gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		out = gz
	} else {
		w.Header().Set("Content-Type", "application/xml")
	}
	if _, err := writeGuide(out, s.base, s.channels, s.now(), s.guideDays, s.quirks); err != nil {
		log.Printf("guide: %v", err)
	}
}

func (s *server) channel(number string) (Channel, bool) {
	i, ok := s.byNumber[number]
	if !ok {
		return Channel{}, false
	}
	return s.channels[i], true
}

func (s *server) logo(w http.ResponseWriter, r *http.Request) {
	c, ok := s.channel(strings.TrimSuffix(r.PathValue("file"), ".svg"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	_, _ = io.WriteString(w, logoSVG(c))
}

// stream serves /auto/v<number>. The ?transcode= profile that upstream adds
// for "hdtc" models is accepted and ignored. ?fault= replaces the channel's
// own fault plan for this tune.
func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	number, ok := strings.CutPrefix(r.PathValue("channel"), "v")
	c, found := s.channel(number)
	if !ok || !found || c.HLS != "" {
		w.Header().Set("X-HDHomeRun-Error", "804 Invalid Channel")
		http.Error(w, "invalid channel", http.StatusNotFound)
		return
	}
	s.tune(w, r, c)
}

// tune serves one tuned channel: tuner limit, faults and the stream.
func (s *server) tune(w http.ResponseWriter, r *http.Request, c Channel) {
	number := c.Number
	if number == s.drm {
		w.Header().Set("X-HDHomeRun-Error", "807 DRM Protected")
		http.Error(w, "drm protected channel", http.StatusForbidden)
		return
	}
	spec := c.Fault
	if q := r.URL.Query().Get("fault"); q != "" {
		spec = q
	}
	fault, err := parseFault(spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if fault.DieAfter == 0 {
		fault.DieAfter = s.dropAfter
	}
	if spec != "" {
		s.faults.Add(1)
	}
	if fault.Status != 0 {
		http.Error(w, "fault: status", fault.Status)
		return
	}
	if fault.Latency > 0 {
		if err := s.pace.Sleep(r.Context(), fault.Latency); err != nil {
			return
		}
	}
	slot, ok := s.tuners.acquire(number, r.RemoteAddr)
	if !ok {
		s.rejects.Add(1)
		w.Header().Set("X-HDHomeRun-Error", "805 All Tuners In Use")
		http.Error(w, "all tuners in use", http.StatusServiceUnavailable)
		return
	}
	defer s.tuners.release(slot)
	s.opens.Add(1)
	log.Printf("tuner%d: %s %s -> %s fault=%q", slot, c.Number, c.Name, r.RemoteAddr, spec)
	w.Header().Set("Content-Type", "video/mp2t")
	w.WriteHeader(http.StatusOK)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	ctx := r.Context()
	if fault.DieAfter > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, fault.DieAfter)
		defer cancel()
	}
	if fault.Hang {
		<-ctx.Done()
		log.Printf("tuner%d: released (hang)", slot)
		return
	}
	out := &faultWriter{ctx: ctx, w: countingWriter{flushWriter{w}, &s.bytes}, p: s.pace, spec: fault}
	if err := s.playPhases(ctx, c, fault.phases(c), out); err != nil {
		log.Printf("tuner%d: %s ended: %v", slot, c.Number, err)
	}
	log.Printf("tuner%d: released", slot)
}

// playPhases runs the phase list in a loop. An endless phase that returns
// means the source died, so the stream ends.
func (s *server) playPhases(ctx context.Context, c Channel, phases []phase, w io.Writer) error {
	for i := 0; ctx.Err() == nil; i++ {
		ph := phases[i%len(phases)]
		if err := s.source.Stream(ctx, c, ph, w); err != nil {
			return err
		}
		if ph.Duration == 0 {
			return nil
		}
	}
	return nil
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

func main() {
	listen := flag.String("listen", ":5004", "HTTP listen address")
	advertise := flag.String("advertise", "", "base URL clients use (default http://<first non-loopback IPv4><listen port>)")
	discovery := flag.String("discovery", ":65001", "UDP discovery listen address; empty disables")
	tuners := flag.Int("tuners", 2, "tuner count; 0 = unlimited (concurrency runs)")
	model := flag.String("model", "HDHR5-4K", "ModelNumber; include \"HDTC\" to enable upstream transcode profiles")
	drm := flag.String("drm", "", "channel number to mark DRM protected (returns 403)")
	days := flag.Int("guide-days", 3, "days of XMLTV guide")
	channels := flag.Int("channels", 0, "add this many generated channels (1000.1 upward) for scale runs")
	noFaults := flag.Bool("no-fault-channels", false, "omit the 900.x fault channels")
	noFAST := flag.Bool("no-fast-channels", false, "omit the 800.x FAST-style HLS channels")
	fastN := flag.Int("fast-channels", 0, "add this many generated FAST HLS channels (2000.1 upward), each with ad breaks")
	xtUser := flag.String("xtream-user", "fakeuser", "username of the synthetic Xtream panel (get.php, xmltv.php, /live/U/P/)")
	xtPass := flag.String("xtream-pass", "FAKEPASS-7b1e", "password of the synthetic Xtream panel; a marker string for redaction tests")
	xtMovies := flag.Int("xtream-movies", 0, "VOD entries in the Xtream m3u_plus playlist")
	xtEpisodes := flag.Int("xtream-episodes", 0, "series episode entries in the Xtream m3u_plus playlist")
	xtBanAfter := flag.Int("xtream-ban-after", 0, "failed logins that ban the client IP, as real panels do; 0 = never")
	xtBanFor := flag.Duration("xtream-ban-for", 10*time.Minute, "ban length and failure-counting window for -xtream-ban-after")
	persona := flag.String("persona", "", "list the channels as provider persona a, b or c (multi-source merging tests; see persona.go)")
	blockUA := flag.Bool("block-default-ua", false, "answer 403 to requests with no User-Agent, or ffmpeg's or Go's default one, as some providers do")
	guideShare := flag.Float64("guide-share", 1, "fraction of generated channels with guide data (Xtream guides cover a subset)")
	geoAllow := flag.String("geo-allow", "", "comma-separated CIDRs that pass the geo check of geo HLS channels (default: none pass)")
	quirkList := flag.String("quirks", "none", "guide/playlist quirks: all, none, or a list of "+strings.Join(allQuirks, ","))
	ffmpegBin := flag.String("ffmpeg", "ffmpeg", "ffmpeg binary")
	sourceKind := flag.String("source", "ffmpeg", "stream source: ffmpeg (one paced encoder per tune) or cache (pre-rendered clips replayed from memory)")
	cacheDir := flag.String("cache-dir", filepath.Join(os.TempDir(), "fakehdhr-cache"), "clip cache for -source cache and the HLS channels")
	loop := flag.String("loop", "", "loop this MPEG-TS capture on every channel instead of test patterns (-source ffmpeg)")
	dropAfter := flag.Duration("drop-after", 0, "end every stream after this long (same as fault die=)")
	overlay := flag.Bool("overlay", true, "draw channel and clock on the picture (needs ffmpeg drawtext)")
	anchor := flag.String("guide-anchor", "", "fixed RFC 3339 guide start, for deterministic runs (default: now)")
	flag.Parse()

	q, err := parseQuirks(*quirkList)
	if err != nil {
		log.Fatal(err)
	}
	now := time.Now
	if *anchor != "" {
		t, err := time.Parse(time.RFC3339, *anchor)
		if err != nil {
			log.Fatalf("-guide-anchor: %v", err)
		}
		now = func() time.Time { return t }
	}
	base := *advertise
	if base == "" {
		_, port, _ := net.SplitHostPort(*listen)
		base = "http://" + firstIPv4() + ":" + port
	}
	lineup := DefaultChannels()
	if !*noFaults {
		lineup = append(lineup, FaultChannels()...)
	}
	if !*noFAST {
		lineup = append(lineup, FASTChannels()...)
	}
	gen := GenerateChannels(*channels)
	for i := range gen {
		if float64(i) >= *guideShare*float64(len(gen)) {
			gen[i].NoGuide = true
		}
	}
	lineup = append(lineup, gen...)
	lineup = append(lineup, GenerateFASTChannels(*fastN)...)
	lineup, truth, err := applyPersona(*persona, lineup)
	if err != nil {
		log.Fatal(err)
	}

	var src Source
	switch *sourceKind {
	case "ffmpeg":
		src = ffmpegSource{bin: *ffmpegBin, loopFile: *loop, overlay: *overlay}
	case "cache":
		if err := os.MkdirAll(*cacheDir, 0o755); err != nil {
			log.Fatal(err)
		}
		src = &cacheSource{bin: *ffmpegBin, dir: *cacheDir, clip: 10 * time.Second, pace: realPacer{}, clips: map[string][]byte{}}
	default:
		log.Fatalf("-source %q: want ffmpeg or cache", *sourceKind)
	}

	s := newServer(base, lineup, *tuners, src)
	s.model, s.drm, s.guideDays, s.quirks, s.now, s.dropAfter = *model, *drm, *days, q, now, *dropAfter
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		log.Fatal(err)
	}
	s.sign = signer{secret: secret}
	s.blockUA, s.truth = *blockUA, truth
	s.xt = &xtream{user: *xtUser, pass: *xtPass, movies: *xtMovies, episodes: *xtEpisodes, banAfter: *xtBanAfter, banFor: *xtBanFor}
	if err := os.MkdirAll(*cacheDir, 0o755); err != nil {
		log.Fatal(err)
	}
	s.segs = &ffmpegSegStore{bin: *ffmpegBin, dir: *cacheDir, clip: 60 * time.Second, segs: map[string][][]byte{}}
	for _, cidr := range strings.Split(*geoAllow, ",") {
		if cidr = strings.TrimSpace(cidr); cidr == "" {
			continue
		}
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			log.Fatalf("-geo-allow: %v", err)
		}
		s.geoAllow = append(s.geoAllow, n)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *discovery != "" {
		go func() {
			if err := serveDiscovery(ctx, *discovery, s.deviceID, *tuners, s.base); err != nil {
				log.Printf("discovery disabled: %v", err)
			}
		}()
	}
	srv := &http.Server{Addr: *listen, Handler: s.routes(), BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("fakehdhr %s: %d tuners, %d channels, quirks=%s, source=%s; M3U %s/playlist.m3u, XMLTV %s/xmltv.xml",
		s.base, *tuners, len(s.channels), *quirkList, *sourceKind, s.base, s.base)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func firstIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
				return ipn.IP.String()
			}
		}
	}
	return "127.0.0.1"
}
