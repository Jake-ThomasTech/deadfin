// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hlsPlan models a FAST-style live HLS channel (free ad-supported TV, and
// station streams of local news). Syntax: comma-separated tokens.
//
//	seg=4s            segment duration (default 4 s)
//	window=6          segments in the live playlist (default 6)
//	ads=120s/30s      an ad break of 30 s after every 120 s of content
//	cue=scte35        mark breaks with EXT-X-CUE-OUT/-CONT/-IN
//	cue=daterange     mark breaks with EXT-X-DATERANGE SCTE35-OUT/-IN
//	token=120s        signed URLs that expire 120 s after the M3U was fetched
//	geo               403 unless the client IP is in -geo-allow
//	unmarked          no EXT-X-DISCONTINUITY at the content loop point, so
//	                  timestamps jump backwards without warning
//	stale=20s/120s    the playlist stops advancing for 20 s every 120 s
//
// Ad creatives use a different resolution and frame rate from the content,
// and every break starts its own timestamps, as server-side ad insertion
// (SSAI) stitching does. Each break is announced with EXT-X-DISCONTINUITY.
type hlsPlan struct {
	Seg      time.Duration
	Window   int
	AdEvery  time.Duration
	AdLen    time.Duration
	Cue      string
	TokenTTL time.Duration
	Geo      bool
	Unmarked bool
	StaleFor time.Duration
	StaleEvr time.Duration
}

func parseHLSPlan(s string) (hlsPlan, error) {
	p := hlsPlan{Seg: 4 * time.Second, Window: 6}
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" || tok == "hls" {
			continue
		}
		key, val, _ := strings.Cut(tok, "=")
		var err error
		switch key {
		case "seg":
			p.Seg, err = time.ParseDuration(val)
		case "window":
			p.Window, err = strconv.Atoi(val)
		case "ads", "stale":
			a, b, ok := strings.Cut(val, "/")
			if !ok {
				return p, fmt.Errorf("%s wants A/B, got %q", key, val)
			}
			var x, y time.Duration
			if x, err = time.ParseDuration(a); err == nil {
				y, err = time.ParseDuration(b)
			}
			if key == "ads" {
				p.AdEvery, p.AdLen = x, y
			} else {
				p.StaleFor, p.StaleEvr = x, y
			}
		case "cue":
			if val != "scte35" && val != "daterange" && val != "none" {
				return p, fmt.Errorf("cue wants scte35, daterange or none, got %q", val)
			}
			p.Cue = val
		case "token":
			p.TokenTTL, err = time.ParseDuration(val)
		case "geo":
			p.Geo = true
		case "unmarked":
			p.Unmarked = true
		default:
			return p, fmt.Errorf("unknown hls option %q", key)
		}
		if err != nil {
			return p, fmt.Errorf("%s: %v", key, err)
		}
	}
	if p.Seg <= 0 || p.Window <= 0 {
		return p, fmt.Errorf("seg and window must be positive")
	}
	if p.AdEvery > 0 && (p.AdEvery%p.Seg != 0 || p.AdLen%p.Seg != 0 || p.AdLen <= 0) {
		return p, fmt.Errorf("ads must be positive multiples of seg %v", p.Seg)
	}
	return p, nil
}

// FASTChannels are HLS channels at 800.x, one per FAST failure mode. They
// appear in the M3U and the XMLTV guide, not in the HDHomeRun lineup.
func FASTChannels() []Channel {
	f := func(minor int, name, plan string) Channel {
		return Channel{
			Number: "800." + strconv.Itoa(minor), Name: "FAST-" + name, Pattern: "testsrc2",
			Size: "1280x720", Rate: "30000/1001", Tone: 500 + 10*minor, Profile: "h264", Group: "News", HD: true, HLS: plan,
		}
	}
	return []Channel{
		f(1, "CLEAN", "hls"),
		f(2, "SSAI", "ads=120s/32s,cue=scte35"),
		f(3, "DATERANGE", "ads=120s/32s,cue=daterange"),
		f(4, "TOKEN", "token=120s"),
		f(5, "GEO", "geo"),
		f(6, "JUMP", "unmarked"),
		f(7, "STALE", "stale=20s/120s"),
		f(8, "ALL", "ads=60s/16s,cue=scte35,token=300s,unmarked,stale=12s/180s"),
	}
}

// GenerateFASTChannels returns n HLS channels numbered 2000.1 upward, for
// scale runs of the M3U path. Each has ad breaks; every 4th has a token.
func GenerateFASTChannels(n int) []Channel {
	out := make([]Channel, 0, n)
	for i := range n {
		plan := "ads=120s/32s,cue=scte35"
		if i%4 == 3 {
			plan += ",token=300s"
		}
		out = append(out, Channel{
			Number: fmt.Sprintf("%d.%d", 2000+i/10, 1+i%10), Name: fmt.Sprintf("FASTGEN-%05d", i),
			Pattern: "testsrc2", Size: "640x360", Rate: "30", Tone: 200 + i%1800, Profile: "h264",
			Group: generatedGroups[i%len(generatedGroups)], HLS: plan,
		})
	}
	return out
}

// slot is one segment position on the channel's live timeline.
type slot struct {
	Seq     int64 // media sequence number
	Ad      bool
	AdBreak int64 // break number, for ad slots
	AdPos   int   // position within the break, in segments
	Content int   // content clip segment index, for content slots
	Disc    bool  // EXT-X-DISCONTINUITY before this segment
	CueIn   bool  // first content segment after a break
	Start   time.Time
}

func (p hlsPlan) slotAt(seq int64, epoch time.Time, contentSegs int) slot {
	s := slot{Seq: seq, Start: epoch.Add(time.Duration(seq) * p.Seg)}
	contentIdx := func(n int64) int { return int(n % int64(contentSegs)) }
	if p.AdEvery > 0 {
		cyc := int64((p.AdEvery + p.AdLen) / p.Seg)
		body := int64(p.AdEvery / p.Seg)
		pos := seq % cyc
		if pos >= body {
			s.Ad, s.AdBreak, s.AdPos = true, seq/cyc, int(pos-body)
			s.Disc = pos == body
			return s
		}
		s.Disc = pos == 0 && seq > 0
		s.CueIn = s.Disc
		// Content resumes where it would have been: SSAI replaces content,
		// so the content clock runs on during the break.
		s.Content = contentIdx(seq)
		if !s.Disc && s.Content == 0 && seq > 0 && !p.Unmarked {
			s.Disc = true
		}
		return s
	}
	s.Content = contentIdx(seq)
	s.Disc = s.Content == 0 && seq > 0 && !p.Unmarked
	return s
}

// livePlaylist renders the media playlist a client would see at now. The
// timeline starts at epoch; segment URLs carry query (the token, if any).
func (p hlsPlan) livePlaylist(now, epoch time.Time, contentSegs int, query string) string {
	if p.StaleEvr > 0 {
		into := now.Sub(epoch) % p.StaleEvr
		if into < p.StaleFor {
			now = now.Add(-into) // frozen at the start of the stale window
		}
	}
	last := int64(now.Sub(epoch)/p.Seg) - 1 // newest complete segment
	first := max(0, last-int64(p.Window)+1)
	// The first segment's discontinuity sequence number counts its own
	// discontinuity too; its tag is not repeated in the playlist.
	var discSeq int64
	for k := int64(1); k <= first; k++ {
		if p.slotAt(k, epoch, contentSegs).Disc {
			discSeq++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-DISCONTINUITY-SEQUENCE:%d\n",
		int((p.Seg+time.Second-1)/time.Second), first, discSeq)
	q := ""
	if query != "" {
		q = "?" + query
	}
	for k := first; k <= last; k++ {
		s := p.slotAt(k, epoch, contentSegs)
		if s.Disc && k > first {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if s.Disc || k == first {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", s.Start.UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		if s.Ad {
			p.writeCue(&b, s)
		} else if s.CueIn {
			p.writeCueIn(&b, s)
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", p.Seg.Seconds())
		if s.Ad {
			fmt.Fprintf(&b, "seg/ad/%d.ts%s\n", s.AdPos, q)
		} else {
			fmt.Fprintf(&b, "seg/content/%d.ts%s\n", s.Content, q)
		}
	}
	return b.String()
}

func (p hlsPlan) writeCue(b *strings.Builder, s slot) {
	dur := p.AdLen.Seconds()
	switch p.Cue {
	case "scte35":
		if s.AdPos == 0 {
			fmt.Fprintf(b, "#EXT-X-CUE-OUT:DURATION=%.3f\n", dur)
		} else {
			fmt.Fprintf(b, "#EXT-X-CUE-OUT-CONT:ElapsedTime=%.3f,Duration=%.3f\n", float64(s.AdPos)*p.Seg.Seconds(), dur)
		}
	case "daterange":
		if s.AdPos == 0 {
			// The payload is a placeholder, not a decodable splice_info_section.
			fmt.Fprintf(b, "#EXT-X-DATERANGE:ID=\"break-%d\",START-DATE=\"%s\",PLANNED-DURATION=%.3f,SCTE35-OUT=0xFC30%08X\n",
				s.AdBreak, s.Start.UTC().Format("2006-01-02T15:04:05.000Z"), dur, s.AdBreak)
		}
	}
}

func (p hlsPlan) writeCueIn(b *strings.Builder, s slot) {
	brk := s.Seq/int64((p.AdEvery+p.AdLen)/p.Seg) - 1
	switch p.Cue {
	case "scte35":
		b.WriteString("#EXT-X-CUE-IN\n")
	case "daterange":
		fmt.Fprintf(b, "#EXT-X-DATERANGE:ID=\"break-%d\",START-DATE=\"%s\",SCTE35-IN=0xFC30%08X\n",
			brk, s.Start.Add(-p.AdLen).UTC().Format("2006-01-02T15:04:05.000Z"), brk)
	}
}

// signer issues and checks expiring URL tokens, as CDNs that front FAST
// services do. The secret lives in the server, never in package state.
type signer struct{ secret []byte }

func (s signer) token(number string, exp int64) string {
	m := hmac.New(sha256.New, s.secret)
	fmt.Fprintf(m, "%s|%d", number, exp)
	return hex.EncodeToString(m.Sum(nil))[:16]
}

func (s signer) query(number string, exp int64) string {
	return url.Values{"exp": {strconv.FormatInt(exp, 10)}, "tok": {s.token(number, exp)}}.Encode()
}

// check returns "" when the request carries a valid, unexpired token.
func (s signer) check(number string, q url.Values, now time.Time) string {
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || !hmac.Equal([]byte(q.Get("tok")), []byte(s.token(number, exp))) {
		return "bad token"
	}
	if now.Unix() >= exp {
		return "token expired"
	}
	return ""
}

// segStore returns the rendered segments of a channel's content or ad clip.
type segStore interface {
	segments(ctx context.Context, c Channel, kind string, seg time.Duration) ([][]byte, error)
}

// ffmpegSegStore renders a clip per channel shape once with ffmpeg's HLS
// muxer, forcing a keyframe at every segment boundary, and keeps the
// segments in memory.
type ffmpegSegStore struct {
	bin  string
	dir  string
	clip time.Duration // content clip length; ads use the break length
	mu   sync.Mutex
	segs map[string][][]byte
}

// adShape is the ad creative: a different size and frame rate from the
// content, so a client that cannot reinitialise its decoder at a
// discontinuity breaks visibly.
func adShape(c Channel) Channel {
	c.Pattern, c.Size, c.Rate, c.Tone = "smptebars", "1920x1080", "30", 1000
	return c
}

func (st *ffmpegSegStore) segments(ctx context.Context, c Channel, kind string, seg time.Duration) ([][]byte, error) {
	length, ph := st.clip, phase{Profile: "h264"}
	if kind == "ad" {
		c = adShape(c)
		plan, _ := parseHLSPlan(c.HLS)
		length, ph.AudioCh = max(plan.AdLen, seg), 6
	}
	key := strings.NewReplacer("/", "-", ":", "-", " ", "-").Replace(
		fmt.Sprintf("hls_%s_%s_%s_%s_%d_%d_%v_%v", kind, c.Pattern, c.Size, c.Rate, c.Tone, ph.AudioCh, seg, length))
	st.mu.Lock()
	defer st.mu.Unlock()
	if s, ok := st.segs[key]; ok {
		return s, nil
	}
	dir := filepath.Join(st.dir, key)
	if _, err := os.Stat(filepath.Join(dir, "done")); err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		tmp := dir + ".part"
		_ = os.RemoveAll(tmp)
		if err := os.MkdirAll(tmp, 0o755); err != nil {
			return nil, err
		}
		args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}
		enc := encodeArgs(c, ph, false)
		args = append(args, enc[:len(enc)-4]...) // drop "-f mpegts -mpegts_service_type digital_tv"
		args = append(args,
			"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%g)", seg.Seconds()),
			"-t", strconv.FormatFloat(length.Seconds(), 'f', 3, 64),
			"-f", "hls", "-hls_time", strconv.FormatFloat(seg.Seconds(), 'f', 3, 64), "-hls_list_size", "0",
			"-hls_segment_filename", filepath.Join(tmp, "%04d.ts"), filepath.Join(tmp, "index.m3u8"))
		if err := runFFmpeg(rctx, st.bin, args, io.Discard, "hls "+c.Number); err != nil {
			return nil, err
		}
		if rctx.Err() != nil {
			return nil, rctx.Err()
		}
		if err := os.WriteFile(filepath.Join(tmp, "done"), nil, 0o644); err != nil {
			return nil, err
		}
		_ = os.RemoveAll(dir)
		if err := os.Rename(tmp, dir); err != nil {
			return nil, err
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.ts"))
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("no segments in %s: %v", dir, err)
	}
	sort.Strings(files)
	out := make([][]byte, len(files))
	for i, f := range files {
		if out[i], err = os.ReadFile(f); err != nil {
			return nil, err
		}
	}
	st.segs[key] = out
	return out, nil
}

// fastGate applies the geo and token checks shared by every HLS request.
// It returns false after writing the error response.
func (s *server) fastGate(w http.ResponseWriter, r *http.Request, c Channel, p hlsPlan) bool {
	if p.Geo && !s.geoAllowed(r.RemoteAddr) {
		s.geoBlocks.Add(1)
		http.Error(w, "this content is not available in your region", http.StatusForbidden)
		return false
	}
	if p.TokenTTL > 0 {
		if why := s.sign.check(c.Number, r.URL.Query(), s.clock()); why != "" {
			s.tokenRejects.Add(1)
			http.Error(w, why, http.StatusForbidden)
			return false
		}
	}
	return true
}

func (s *server) geoAllowed(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	for _, n := range s.geoAllow {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *server) hlsChannel(w http.ResponseWriter, r *http.Request) (Channel, hlsPlan, bool) {
	c, ok := s.channel(r.PathValue("channel"))
	if !ok || c.HLS == "" {
		http.NotFound(w, r)
		return c, hlsPlan{}, false
	}
	p, err := parseHLSPlan(c.HLS)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return c, p, false
	}
	return c, p, s.fastGate(w, r, c, p)
}

// hlsMaster serves /hls/{channel}/master.m3u8 with one variant, as most
// FAST origins do.
func (s *server) hlsMaster(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.hlsChannel(w, r)
	if !ok {
		return
	}
	s.hlsOpens.Add(1)
	q := ""
	if r.URL.RawQuery != "" {
		q = "?" + r.URL.RawQuery
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-STREAM-INF:BANDWIDTH=4500000,RESOLUTION=%s,CODECS=\"avc1.640028,mp4a.40.2\"\nmedia.m3u8%s\n", c.Size, q)
}

func (s *server) hlsMedia(w http.ResponseWriter, r *http.Request) {
	c, p, ok := s.hlsChannel(w, r)
	if !ok {
		return
	}
	content, err := s.segs.segments(r.Context(), c, "content", p.Seg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "max-age=1")
	_, _ = io.WriteString(w, p.livePlaylist(s.clock(), s.epoch, len(content), r.URL.RawQuery))
}

func (s *server) hlsSegment(w http.ResponseWriter, r *http.Request) {
	c, p, ok := s.hlsChannel(w, r)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if kind != "content" && kind != "ad" {
		http.NotFound(w, r)
		return
	}
	idx, err := strconv.Atoi(strings.TrimSuffix(r.PathValue("file"), ".ts"))
	segs, serr := s.segs.segments(r.Context(), c, kind, p.Seg)
	if serr != nil {
		http.Error(w, serr.Error(), http.StatusBadGateway)
		return
	}
	if err != nil || idx < 0 || len(segs) == 0 {
		http.NotFound(w, r)
		return
	}
	idx %= len(segs) // the encoder may cut one segment fewer than planned
	s.hlsSegments.Add(1)
	s.bytes.Add(int64(len(segs[idx])))
	w.Header().Set("Content-Type", "video/mp2t")
	_, _ = w.Write(segs[idx])
}

// streamURL is the M3U URL for a channel: the HDHomeRun tune URL, or the
// HLS master playlist with a fresh token for token channels.
func (s *server) streamURL(c Channel) string {
	if c.HLS == "" {
		return s.base + "/auto/v" + c.Number
	}
	u := s.base + "/hls/" + c.Number + "/master.m3u8"
	if p, err := parseHLSPlan(c.HLS); err == nil && p.TokenTTL > 0 {
		u += "?" + s.sign.query(c.Number, s.clock().Add(p.TokenTTL).Unix())
	}
	return u
}
