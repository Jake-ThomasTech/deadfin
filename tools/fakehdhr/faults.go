// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// faultSpec is a stream fault plan. Syntax: comma-separated tokens, applied
// per tune request (channel default from the lineup, or ?fault= override):
//
//	slowstart=8s           HTTP 200 at once, first byte after 8 s
//	hang                   HTTP 200, then no bytes ever
//	stall=3s/20s           stop sending for 3 s after every 20 s of output
//	die=30s                close the connection after 30 s
//	status=503             answer with this HTTP status, no stream
//	codec=atsc1|h264       alternate profiles (atsc1, h264, hevc, lite)
//	bitrate=1M|25M         alternate video bitrates
//	audio=2|6              alternate audio channel counts
//	size=1280x720|1920x1080 alternate resolutions
//	every=60s              phase length for the alternations (default 60 s)
//	cap=8M                 throttle the stream to 8 Mbit/s, as a remote
//	                       tuner's WAN uplink does (VPN placeshifting)
//	latency=150ms          delay the response and the first byte, as a WAN
//	                       round trip does
//
// Packet loss and jitter are below HTTP; use tc netem or toxiproxy in front
// of fakehdhr for those.
// Each phase is a fresh encoder run, so a phase change also restarts
// timestamps and continuity counters, as a real broadcast splice does.
type faultSpec struct {
	SlowStart  time.Duration
	Hang       bool
	StallFor   time.Duration
	StallEvery time.Duration
	DieAfter   time.Duration
	Status     int
	Codec      []string
	Bitrate    []string
	Audio      []int
	Size       []string
	Every      time.Duration
	CapBits    int64 // bits per second; 0 = no cap
	Latency    time.Duration
}

func parseFault(s string) (faultSpec, error) {
	f := faultSpec{Every: 60 * time.Second}
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		key, val, _ := strings.Cut(tok, "=")
		var err error
		switch key {
		case "slowstart":
			f.SlowStart, err = time.ParseDuration(val)
		case "hang":
			f.Hang = true
		case "stall":
			d, p, ok := strings.Cut(val, "/")
			if !ok {
				return f, fmt.Errorf("stall wants D/P, got %q", val)
			}
			if f.StallFor, err = time.ParseDuration(d); err == nil {
				f.StallEvery, err = time.ParseDuration(p)
			}
		case "die":
			f.DieAfter, err = time.ParseDuration(val)
		case "status":
			f.Status, err = strconv.Atoi(val)
		case "codec":
			f.Codec = strings.Split(val, "|")
			for _, c := range f.Codec {
				if !slices.Contains([]string{"atsc1", "h264", "hevc", "lite"}, c) {
					return f, fmt.Errorf("unknown codec profile %q", c)
				}
			}
		case "bitrate":
			f.Bitrate = strings.Split(val, "|")
		case "audio":
			for _, a := range strings.Split(val, "|") {
				n, e := strconv.Atoi(a)
				if e != nil {
					return f, fmt.Errorf("audio: %v", e)
				}
				f.Audio = append(f.Audio, n)
			}
		case "size":
			f.Size = strings.Split(val, "|")
		case "every":
			f.Every, err = time.ParseDuration(val)
		case "cap":
			f.CapBits, err = parseBits(val)
		case "latency":
			f.Latency, err = time.ParseDuration(val)
		default:
			return f, fmt.Errorf("unknown fault %q", key)
		}
		if err != nil {
			return f, fmt.Errorf("%s: %v", key, err)
		}
	}
	return f, nil
}

// parseBits reads 8M, 500k or 2000000 as bits per second.
func parseBits(s string) (int64, error) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "M"):
		mult, s = 1_000_000, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "k"):
		mult, s = 1_000, strings.TrimSuffix(s, "k")
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad rate %q", s)
	}
	return int64(n * float64(mult)), nil
}

// phase is one encoder run of a stream plan.
type phase struct {
	Profile  string
	Bitrate  string // "" = profile default
	Size     string // "" = channel default
	AudioCh  int    // 0 = profile default
	Duration time.Duration
}

// phases expands the alternations into a repeating phase list. Without
// alternations the stream is one endless phase.
func (f faultSpec) phases(c Channel) []phase {
	n := max(len(f.Codec), len(f.Bitrate), len(f.Audio), len(f.Size))
	if n == 0 {
		return []phase{{Profile: c.Profile}}
	}
	out := make([]phase, n)
	for i := range n {
		p := phase{Profile: c.Profile, Duration: f.Every}
		if len(f.Codec) > 0 {
			p.Profile = f.Codec[i%len(f.Codec)]
		}
		if len(f.Bitrate) > 0 {
			p.Bitrate = f.Bitrate[i%len(f.Bitrate)]
		}
		if len(f.Audio) > 0 {
			p.AudioCh = f.Audio[i%len(f.Audio)]
		}
		if len(f.Size) > 0 {
			p.Size = f.Size[i%len(f.Size)]
		}
		out[i] = p
	}
	return out
}

// pacer abstracts time so fault behavior is testable without sleeping.
type pacer interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realPacer struct{}

func (realPacer) Now() time.Time { return time.Now() }

func (realPacer) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// faultWriter applies slow start and periodic stalls to a stream. Stalls
// block the writer, so an upstream ffmpeg backs up and then bursts, as a
// congested tuner network does.
type faultWriter struct {
	ctx       context.Context
	w         io.Writer
	p         pacer
	spec      faultSpec
	started   bool
	nextStall time.Time
	capStart  time.Time
	capSent   int64
}

func (fw *faultWriter) Write(b []byte) (int, error) {
	if !fw.started {
		fw.started = true
		if fw.spec.SlowStart > 0 {
			if err := fw.p.Sleep(fw.ctx, fw.spec.SlowStart); err != nil {
				return 0, err
			}
		}
		if fw.spec.StallEvery > 0 {
			fw.nextStall = fw.p.Now().Add(fw.spec.StallEvery)
		}
		fw.capStart = fw.p.Now()
	}
	if fw.spec.StallEvery > 0 && !fw.p.Now().Before(fw.nextStall) {
		if err := fw.p.Sleep(fw.ctx, fw.spec.StallFor); err != nil {
			return 0, err
		}
		fw.nextStall = fw.p.Now().Add(fw.spec.StallEvery)
	}
	if fw.spec.CapBits > 0 {
		// Hold each write until the cap allows the bytes already sent.
		due := fw.capStart.Add(time.Duration(float64(fw.capSent*8) / float64(fw.spec.CapBits) * float64(time.Second)))
		if wait := due.Sub(fw.p.Now()); wait > 0 {
			if err := fw.p.Sleep(fw.ctx, wait); err != nil {
				return 0, err
			}
		}
		fw.capSent += int64(len(b))
	}
	return fw.w.Write(b)
}
