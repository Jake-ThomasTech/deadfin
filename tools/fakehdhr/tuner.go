// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Source writes one phase of a channel's MPEG-TS stream to w until the
// phase duration ends (0 = never), ctx ends, or the source fails.
type Source interface {
	Stream(ctx context.Context, c Channel, ph phase, w io.Writer) error
}

// tunerPool models the physical tuner limit. A real HDHomeRun answers a
// request beyond its tuner count with 503 and "X-HDHomeRun-Error: 805". A
// pool of size 0 is unlimited, for concurrency runs.
type tunerPool struct {
	mu     sync.Mutex
	limit  int
	slots  map[int]string // tuner index -> channel number
	client map[int]string
	peak   int
}

func newTunerPool(n int) *tunerPool {
	return &tunerPool{limit: n, slots: map[int]string{}, client: map[int]string{}}
}

func (p *tunerPool) acquire(number, client string) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; p.limit == 0 || i < p.limit; i++ {
		if _, busy := p.slots[i]; !busy {
			p.slots[i], p.client[i] = number, client
			p.peak = max(p.peak, len(p.slots))
			return i, true
		}
	}
	return -1, false
}

func (p *tunerPool) release(i int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.slots, i)
	delete(p.client, i)
}

type tunerStatus struct {
	Resource      string `json:"Resource"`
	VctNumber     string `json:"VctNumber,omitempty"`
	TargetIP      string `json:"TargetIP,omitempty"`
	SignalPercent int    `json:"SignalStrengthPercent,omitempty"`
}

func (p *tunerPool) status() []tunerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.limit
	if n == 0 {
		for i := range p.slots {
			n = max(n, i+1)
		}
	}
	out := make([]tunerStatus, n)
	for i := range n {
		out[i] = tunerStatus{Resource: "tuner" + strconv.Itoa(i)}
		if s, ok := p.slots[i]; ok {
			out[i].VctNumber, out[i].TargetIP, out[i].SignalPercent = s, p.client[i], 100
		}
	}
	return out
}

func (p *tunerPool) counts() (active, peak int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.slots), p.peak
}

// encodeArgs builds the ffmpeg arguments for one phase, without input pacing
// or duration, and without the output target.
func encodeArgs(c Channel, ph phase, overlay bool) []string {
	size := c.Size
	if ph.Size != "" {
		size = ph.Size
	}
	video := fmt.Sprintf("%s=size=%s:rate=%s", c.Pattern, size, c.Rate)
	if overlay {
		video += fmt.Sprintf(",drawtext=text='%s %s  %%{localtime\\:%%T}':fontsize=32:fontcolor=white:box=1:boxcolor=black@0.6:x=20:y=20", c.Number, c.Name)
	}
	args := []string{
		"-f", "lavfi", "-i", video,
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:sample_rate=48000", c.Tone),
		"-map", "0:v", "-map", "1:a",
	}
	br := func(def string) string {
		if ph.Bitrate != "" {
			return ph.Bitrate
		}
		return def
	}
	ac := func(def int) string {
		if ph.AudioCh > 0 {
			return strconv.Itoa(ph.AudioCh)
		}
		return strconv.Itoa(def)
	}
	switch ph.Profile {
	case "lite":
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-b:v", br("300k"), "-g", "30", "-c:a", "aac", "-b:a", "64k", "-ac", ac(2))
	case "h264":
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-b:v", br("4M"), "-maxrate", br("4M"), "-bufsize", "8M", "-g", "60", "-c:a", "aac", "-b:a", "128k", "-ac", ac(2))
	case "hevc":
		args = append(args, "-c:v", "libx265", "-preset", "ultrafast", "-b:v", br("6M"), "-g", "120", "-x265-params", "log-level=error", "-c:a", "ac3", "-b:a", "384k", "-ac", ac(6))
	default:
		// ATSC 1.0: interlaced MPEG-2 with 5.1 AC-3, so the deinterlace and
		// audio downmix paths of the transcoder run.
		args = append(args, "-c:v", "mpeg2video", "-b:v", br("8M"), "-maxrate", br("12M"), "-bufsize", "4M", "-g", "15",
			"-flags", "+ilme+ildct", "-top", "1", "-c:a", "ac3", "-b:a", "384k", "-ac", ac(6))
	}
	return append(args, "-f", "mpegts", "-mpegts_service_type", "digital_tv")
}

// ffmpegSource renders each phase live with a paced ffmpeg (or loops a
// capture file). One ffmpeg per tuned stream: realistic, but CPU bound.
type ffmpegSource struct {
	bin      string
	loopFile string // when set, loop this capture with -c copy
	overlay  bool   // draw channel name and wall clock (needs libfreetype)
}

func (s ffmpegSource) args(c Channel, ph phase) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error"}
	if s.loopFile != "" {
		args = append(args, "-re", "-stream_loop", "-1", "-i", s.loopFile, "-map", "0", "-c", "copy", "-f", "mpegts")
	} else {
		args = append(args, "-re")
		args = append(args, encodeArgs(c, ph, s.overlay)...)
	}
	return append(args, outputDuration(ph.Duration, "pipe:1")...)
}

// outputDuration places -t as an output option. The lavfi inputs are
// endless, so an input -t on one of them would not end the output.
func outputDuration(d time.Duration, target string) []string {
	if d <= 0 {
		return []string{target}
	}
	return []string{"-t", strconv.FormatFloat(d.Seconds(), 'f', 3, 64), target}
}

func (s ffmpegSource) Stream(ctx context.Context, c Channel, ph phase, w io.Writer) error {
	return runFFmpeg(ctx, s.bin, s.args(c, ph), w, c.Number)
}

func runFFmpeg(ctx context.Context, bin string, args []string, w io.Writer, tag string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = w
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	scan := bufio.NewScanner(stderr)
	for scan.Scan() {
		log.Printf("ffmpeg %s: %s", tag, scan.Text())
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// cacheSource renders a short clip per (channel shape, phase) once, then
// replays it from memory paced to its own bitrate. No ffmpeg per viewer, so
// hundreds of concurrent tunes are cheap on the tuner side. The loop point
// is a timestamp discontinuity, which is itself a real-world quirk.
type cacheSource struct {
	bin   string
	dir   string
	clip  time.Duration
	pace  pacer
	mu    sync.Mutex
	clips map[string][]byte
}

func (s *cacheSource) key(c Channel, ph phase) string {
	size := c.Size
	if ph.Size != "" {
		size = ph.Size
	}
	k := fmt.Sprintf("%s_%s_%s_%s_%d_%s_%d", c.Pattern, size, c.Rate, ph.Profile, ph.AudioCh, ph.Bitrate, c.Tone)
	return strings.NewReplacer("/", "-", ":", "-", " ", "-").Replace(k) // rates like 30000/1001
}

func (s *cacheSource) clipFor(ctx context.Context, c Channel, ph phase) ([]byte, error) {
	k := s.key(c, ph)
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.clips[k]; ok {
		return b, nil
	}
	path := filepath.Join(s.dir, k+".ts")
	if _, err := os.Stat(path); err != nil {
		// Render outside the requester's cancellation and publish by rename,
		// so a viewer who gives up cannot leave a partial clip behind.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		tmp := path + ".part"
		args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}
		args = append(args, encodeArgs(c, ph, false)...)
		if err := runFFmpeg(rctx, s.bin, append(args, outputDuration(s.clip, tmp)...), io.Discard, "cache "+c.Number); err != nil {
			return nil, err
		}
		if rctx.Err() != nil {
			return nil, rctx.Err()
		}
		if err := os.Rename(tmp, path); err != nil {
			return nil, err
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s.clips[k] = b
	return b, nil
}

func (s *cacheSource) Stream(ctx context.Context, c Channel, ph phase, w io.Writer) error {
	clip, err := s.clipFor(ctx, c, ph)
	if err != nil || len(clip) == 0 {
		return fmt.Errorf("clip %s: %v", c.Number, err)
	}
	return paceClip(ctx, s.pace, clip, s.clip, ph.Duration, w)
}

// paceClip writes clip in a loop at len(clip)/clipLen bytes per second, in
// 100 ms chunks, until total elapses (0 = never) or ctx ends.
func paceClip(ctx context.Context, p pacer, clip []byte, clipLen, total time.Duration, w io.Writer) error {
	const tick = 100 * time.Millisecond
	chunk := max(188, int(float64(len(clip))*tick.Seconds()/clipLen.Seconds())/188*188)
	start := p.Now()
	for off := 0; ; {
		if total > 0 && p.Now().Sub(start) >= total {
			return nil
		}
		end := min(off+chunk, len(clip))
		if _, err := w.Write(clip[off:end]); err != nil {
			return err
		}
		off = end % len(clip)
		if err := p.Sleep(ctx, tick); err != nil {
			return nil
		}
	}
}
