// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
	"github.com/deadfin-org/deadfin/internal/transcodeworker"
)

const envWorkerHelperArgs = "DEADFIN_TEST_WORKER_HELPER_ARGS"

// TestTranscodeWorkerProcessHelper is the worker process of the two-process
// test: the test binary re-executes itself as "deadfin worker".
func TestTranscodeWorkerProcessHelper(t *testing.T) {
	args := os.Getenv(envWorkerHelperArgs)
	if args == "" {
		t.Skip("helper process only")
	}
	runWorker(strings.Split(args, "\x1f"))
	os.Exit(0)
}

// TestTwoProcessTranscodeWorker runs the server and a "deadfin worker"
// process with real ffmpeg (#617): the worker dials the server's main
// listener over TLS with a pinned static token, a software HLS encode of a
// 10 s fixture produces the same playlist as a local encode, and a SIGKILL
// of the worker mid-stream costs one restart that serves the next segment
// within 2 s.
func TestTwoProcessTranscodeWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture.mkv")
	generate := exec.Command(ffmpegPath, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "10", "-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-c:a", "aac", "-y", fixture)
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v\n%s", err, output)
	}

	// The listener comes first: the static token pins its certificate key.
	var handler atomic.Pointer[http.Handler]
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if current := handler.Load(); current != nil {
			(*current).ServeHTTP(w, r)
			return
		}
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	server.StartTLS()
	defer server.Close()
	token, err := transcodeworker.NewToken(transcodeworker.CertificateFingerprint(server.Certificate()))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWorkerTokens, token)
	t.Setenv(envWorkerMode, string(transcodeworker.ModeWorkersFirst))
	programData := filepath.Join(root, "program-data")
	app := newCompositionApplication(t, programData)
	router := http.Handler(app.Router())
	handler.Store(&router)

	worker := startWorkerProcess(t, server.URL, token, ffmpegPath)
	waitFor(t, func() bool { return len(app.transcodeWorkers.Workers()) == 1 })

	paths, err := config.NewPaths(programData)
	if err != nil {
		t.Fatal(err)
	}
	transcodes, err := filepath.EvalSymlinks(paths.Transcodes)
	if err != nil {
		t.Fatal(err)
	}

	// A remote encode writes the same playlist as a local one.
	remote := workerHLSRequest(transcodes, "remote", fixture, 0, false)
	if err := app.transcodes.StartHLS(t.Context(), remote); err != nil {
		t.Fatal(err)
	}
	if workers := app.transcodeWorkers.Workers(); len(workers) != 1 || workers[0].ActiveJobs != 1 {
		t.Fatalf("the remote encode is not on the worker: %+v", workers)
	}
	remotePlaylist := waitForEndList(t, filepath.Join(transcodes, "remote.m3u8"))
	localDirectory := filepath.Join(root, "local")
	if err := os.MkdirAll(localDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	local := workerHLSRequest(localDirectory, "remote", fixture, 0, false)
	if output, err := exec.Command(ffmpegPath, append([]string{"-v", "error"}, local.Args...)...).CombinedOutput(); err != nil {
		t.Fatalf("local encode: %v\n%s", err, output)
	}
	localPlaylist, err := os.ReadFile(filepath.Join(localDirectory, "remote.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if remotePlaylist != string(localPlaylist) {
		t.Fatalf("remote playlist differs from the local one\nremote:\n%s\nlocal:\n%s", remotePlaylist, localPlaylist)
	}

	// A burn-in of an ASS subtitle with its fonts runs on the worker
	// (#624) and writes the same playlist as a local encode.
	subtitle, fonts := burnInFixture(t, root)
	burn := workerBurnInRequest(transcodes, "burn", fixture, subtitle, fonts)
	if err := app.transcodes.StartHLS(t.Context(), burn); err != nil {
		t.Fatal(err)
	}
	if workers := app.transcodeWorkers.Workers(); len(workers) != 1 || workers[0].ActiveJobs != 1 {
		t.Fatalf("the burn-in is not on the worker: %+v", workers)
	}
	burnPlaylist := waitForEndList(t, filepath.Join(transcodes, "burn.m3u8"))
	localBurn := workerBurnInRequest(localDirectory, "burn", fixture, subtitle, fonts)
	if output, err := exec.Command(ffmpegPath, append([]string{"-v", "error"}, localBurn.Args...)...).CombinedOutput(); err != nil {
		t.Fatalf("local burn-in: %v\n%s", err, output)
	}
	localBurnPlaylist, err := os.ReadFile(filepath.Join(localDirectory, "burn.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if burnPlaylist != string(localBurnPlaylist) {
		t.Fatalf("remote burn-in playlist differs from the local one\nremote:\n%s\nlocal:\n%s", burnPlaylist, localBurnPlaylist)
	}
	_ = app.transcodes.StopPlaySession(t.Context(), burn.PlaySessionID, burn.DeviceID)

	// A progressive transcode plays through the worker (#624), and a seek
	// (a new request with -ss) plays the rest.
	ffprobePath, probeErr := exec.LookPath("ffprobe")
	for _, test := range []struct {
		name  string
		start int
		want  float64
	}{{"progressive.mkv", 0, 10}, {"progressive-seek.mkv", 5, 5}} {
		request := workerProgressiveRequest(transcodes, test.name, fixture, test.start)
		placed := workerPlacements(t, app.transcodeWorkers)
		reader, err := app.transcodes.OpenProgressive(t.Context(), request)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if got := workerPlacements(t, app.transcodeWorkers); got != placed+1 {
			t.Fatalf("%s is not on the worker: worker placements %d, want %d", test.name, got, placed+1)
		}
		played := filepath.Join(localDirectory, test.name)
		body, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil || len(body) == 0 {
			t.Fatalf("%s: read %d bytes, %v", test.name, len(body), err)
		}
		if err := os.WriteFile(played, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if probeErr != nil {
			continue
		}
		output, err := exec.Command(ffprobePath, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", played).Output()
		if err != nil {
			t.Fatalf("%s: ffprobe: %v", test.name, err)
		}
		duration, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
		if err != nil || duration < test.want-0.5 || duration > test.want+0.5 {
			t.Fatalf("%s: duration %q, want %.0f s", test.name, output, test.want)
		}
	}

	// A SIGKILL of the worker mid-stream: the next segment is served
	// within 2 s, from a restart at its index.
	paced := workerHLSRequest(transcodes, "paced", fixture, 0, true)
	if err := app.transcodes.StartHLS(t.Context(), paced); err != nil {
		t.Fatal(err)
	}
	segment, err := app.transcodes.WaitSegment(t.Context(), paced.PlaylistPath, 1)
	if err != nil {
		t.Fatalf("segment 1 from the worker: %v", err)
	}
	_ = segment.Close()
	if err := worker.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	realClock := clock.NewReal()
	killed := realClock.Now()
	next := 2
	for {
		if _, err := os.Stat(filepath.Join(transcodes, fmt.Sprintf("paced%d.ts", next+1))); err != nil {
			break
		}
		next++
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	segment, err = app.transcodes.WaitSegment(ctx, paced.PlaylistPath, next)
	if errors.Is(err, ffmpeg.ErrRunnerLost) {
		if err := app.transcodes.RestartHLSAt(ctx, workerHLSRequest(transcodes, "paced", fixture, next, false), next); err != nil {
			t.Fatalf("restart at segment %d: %v", next, err)
		}
		segment, err = app.transcodes.WaitSegment(ctx, paced.PlaylistPath, next)
	}
	if err != nil {
		t.Fatalf("segment %d after the worker loss: %v", next, err)
	}
	_ = segment.Close()
	if elapsed := realClock.Now().Sub(killed); elapsed > 2*time.Second {
		t.Fatalf("segment %d after the worker loss took %s, want at most 2 s", next, elapsed)
	}
	_ = app.transcodes.StopPlaySession(t.Context(), paced.PlaySessionID, paced.DeviceID)
}

func startWorkerProcess(t *testing.T, serverURL, token, ffmpegPath string) *exec.Cmd {
	t.Helper()
	args := []string{"-server", serverURL, "-name", "ci-worker", "-work-dir", t.TempDir(), "-ffmpeg", ffmpegPath, "-max-jobs", "2"}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTranscodeWorkerProcessHelper$")
	cmd.Env = append(os.Environ(), envWorkerHelperArgs+"="+strings.Join(args, "\x1f"), envWorkerToken+"="+token)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// workerHLSRequest is a software HLS encode with 2 s segments. paced reads
// the input at its native rate, so the encode is still running mid-test.
func workerHLSRequest(directory, label, source string, start int, paced bool) ffmpeg.HLSRequest {
	playlist := filepath.Join(directory, label+".m3u8")
	pattern := filepath.Join(directory, label+"%d.ts")
	identity := []string{"-i", "file:" + source, "-map", "0:v:0", "-map", "0:a:0", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "1",
		"-g", "48", "-keyint_min", "48", "-sc_threshold", "0", "-c:a", "aac", "-ac", "2", "-f", "hls", "-hls_time", "2",
		"-hls_playlist_type", "vod", "-hls_segment_type", "mpegts", "-start_number", "0", "-hls_segment_filename", pattern, "-y", playlist}
	args := append([]string(nil), identity...)
	if start > 0 {
		args = append([]string{"-ss", fmt.Sprint(start * 2)}, args...)
		for index := range args {
			if args[index] == "-start_number" {
				args[index+1] = fmt.Sprint(start)
			}
		}
	}
	if paced {
		args = append([]string{"-re"}, args...)
	}
	return ffmpeg.HLSRequest{
		PlaylistPath: playlist, SegmentPattern: pattern, Args: args, IdentityArgs: identity,
		PlaySessionID: "play-" + label, DeviceID: "device-" + label, MediaSourceID: "source",
		SourceRevision: "fixture", OpenSource: func() (*sourcefile.Source, error) { return sourcefile.Open(source, sourcefile.Expected{}) },
		SegmentLengthSeconds: 2, RunTimeTicks: 10 * 10_000_000,
	}
}

func waitForEndList(t *testing.T, path string) string {
	t.Helper()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), "#EXT-X-ENDLIST") {
			return string(data)
		}
		poll := time.NewTimer(50 * time.Millisecond)
		select {
		case <-deadline.C:
			poll.Stop()
			t.Fatalf("%s never completed", path)
			return ""
		case <-poll.C:
		}
	}
}

// burnInFixture writes an ASS subtitle and a fonts directory. The font is a
// system font when one exists; libass skips a file that is not a font.
func burnInFixture(t *testing.T, root string) (subtitle, fonts string) {
	t.Helper()
	fonts = filepath.Join(root, "fonts: cache")
	if err := os.MkdirAll(fonts, 0o755); err != nil {
		t.Fatal(err)
	}
	font := []byte("not a font")
	for _, candidate := range []string{"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", "/System/Library/Fonts/Supplemental/Arial.ttf", "/Library/Fonts/Arial Unicode.ttf"} {
		if data, err := os.ReadFile(candidate); err == nil {
			font = data
			break
		}
	}
	if err := os.WriteFile(filepath.Join(fonts, "Sans.ttf"), font, 0o644); err != nil {
		t.Fatal(err)
	}
	subtitle = filepath.Join(root, "burn.ass")
	ass := "[Script Info]\nScriptType: v4.00+\nPlayResX: 320\nPlayResY: 180\n\n" +
		"[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, Bold, Italic, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV\n" +
		"Style: Default,Sans,24,&H00FFFFFF,0,0,1,2,0,2,10,10,10\n\n" +
		"[Events]\nFormat: Layer, Start, End, Style, Text\nDialogue: 0,0:00:00.50,0:00:08.00,Default,Burned in on a worker\n"
	if err := os.WriteFile(subtitle, []byte(ass), 0o644); err != nil {
		t.Fatal(err)
	}
	return subtitle, fonts
}

// workerBurnInRequest is workerHLSRequest with a pinned external ASS
// subtitle burned in, as the HLS service builds it.
func workerBurnInRequest(directory, label, source, subtitle, fonts string) ffmpeg.HLSRequest {
	request := workerHLSRequest(directory, label, source, 0, false)
	operand := "subtitles=f='" + encoding.EscapeSubtitleFilterPath(subtitle) + "'"
	filter := operand + ":fontsdir='" + encoding.EscapeSubtitleFilterPath(fonts) + "'"
	withFilter := func(args []string) []string {
		index := indexOfArg(args, "-c:v")
		out := append([]string(nil), args[:index]...)
		out = append(out, "-vf", filter)
		return append(out, args[index:]...)
	}
	request.Args, request.IdentityArgs = withFilter(request.Args), withFilter(request.IdentityArgs)
	request.ExternalSubtitleSource = operand
	request.ExternalSubtitleRevision = "burn-subtitle"
	request.OpenExternalSubtitle = func() (*sourcefile.Source, error) { return sourcefile.Open(subtitle, sourcefile.Expected{}) }
	request.SubtitleFontsDir = fonts
	return request
}

func indexOfArg(args []string, value string) int {
	for index, argument := range args {
		if argument == value {
			return index
		}
	}
	return len(args)
}

// workerPlacements sums the hub's worker placements.
func workerPlacements(t *testing.T, hub *transcodeworker.Hub) int {
	t.Helper()
	var metrics strings.Builder
	if err := hub.WriteMetrics(&metrics); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, line := range strings.Split(metrics.String(), "\n") {
		if strings.HasPrefix(line, `deadfin_transcode_placements_total{target="worker"`) {
			fields := strings.Fields(line)
			value, _ := strconv.Atoi(fields[len(fields)-1])
			total += value
		}
	}
	return total
}

// workerProgressiveRequest is a software progressive transcode to Matroska.
// start seeks, as a client seek does.
func workerProgressiveRequest(directory, name, source string, start int) ffmpeg.ProgressiveRequest {
	output := filepath.Join(directory, name)
	var args []string
	if start > 0 {
		args = append(args, "-ss", fmt.Sprint(start))
	}
	args = append(args, "-i", "file:"+source, "-map", "0:v:0", "-map", "0:a:0", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "1",
		"-c:a", "aac", "-f", "matroska", "-y", output)
	return ffmpeg.ProgressiveRequest{
		OutputPath: output, Args: args, PlaySessionID: "play-" + name, DeviceID: "device-" + name, SourceRevision: "fixture",
		OpenSource: func() (*sourcefile.Source, error) { return sourcefile.Open(source, sourcefile.Expected{}) },
	}
}
