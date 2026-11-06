// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/encoding"
)

// liveHLSFixture renders a short local HLS stream with ffmpeg and serves it
// under a path that carries marker credentials.
func liveHLSFixture(t *testing.T) (ffmpegPath string, server *httptest.Server) {
	t.Helper()
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("live remux test requires ffmpeg: %v", err)
	}
	dir := t.TempDir()
	render := exec.CommandContext(t.Context(), ffmpegPath, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10", "-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "4", "-c:v", "mpeg2video", "-g", "10", "-c:a", "mp2", "-f", "hls", "-hls_time", "1",
		"-hls_list_size", "0", "-hls_playlist_type", "vod", filepath.Join(dir, "index.m3u8"))
	if output, err := render.CombinedOutput(); err != nil {
		t.Skipf("render HLS fixture: %v: %s", err, output)
	}
	files := http.StripPrefix("/live/MARKERUSER/MARKERPASS/", http.FileServer(http.Dir(dir)))
	server = httptest.NewServer(files)
	t.Cleanup(server.Close)
	return ffmpegPath, server
}

func TestLiveRemuxCopiesHLSToTransportStream(t *testing.T) {
	ffmpegPath, server := liveHLSFixture(t)
	stream, err := LiveRemuxer{Path: ffmpegPath}.Remux(t.Context(), encoding.LiveInput{URL: server.URL + "/live/MARKERUSER/MARKERPASS/index.m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if len(data) < 188*10 || len(data)%188 != 0 || data[0] != 0x47 || data[188] != 0x47 {
		t.Fatalf("output is not MPEG-TS: %d bytes, first % x", len(data), data[:min(len(data), 4)])
	}
}

// A failed source ends the stream with an error that carries no part of
// the source URL.
func TestLiveRemuxReportsFailureWithoutTheURL(t *testing.T) {
	ffmpegPath, server := liveHLSFixture(t)
	stream, err := LiveRemuxer{Path: ffmpegPath}.Remux(t.Context(), encoding.LiveInput{URL: server.URL + "/live/MARKERUSER/MARKERPASS/missing.m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	_, err = io.ReadAll(stream)
	if err == nil {
		t.Fatal("missing source read without error")
	}
	for _, marker := range []string{"MARKERUSER", "MARKERPASS", "missing.m3u8", server.URL} {
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("error carries %q: %v", marker, err)
		}
	}
}

// Close stops a running ffmpeg and waits for it; a cancelled context does
// the same.
func TestLiveRemuxCloseAndCancelStopFFmpeg(t *testing.T) {
	ffmpegPath, server := liveHLSFixture(t)
	url := server.URL + "/live/MARKERUSER/MARKERPASS/index.m3u8"
	input := encoding.LiveInput{URL: url}

	stream, err := LiveRemuxer{Path: ffmpegPath}.Remux(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	remux := stream.(*liveRemuxStream)
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if remux.command.ProcessState == nil {
		t.Fatal("Close returned before ffmpeg exited")
	}

	ctx, cancel := context.WithCancel(t.Context())
	stream, err = LiveRemuxer{Path: ffmpegPath}.Remux(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := io.ReadAll(stream); err == nil {
		t.Fatal("cancelled remux read without error")
	}
	_ = stream.Close()
	if stream.(*liveRemuxStream).command.ProcessState == nil {
		t.Fatal("ffmpeg still running after cancel")
	}
}

func TestLiveRemuxRejectsBadInput(t *testing.T) {
	if _, err := (LiveRemuxer{}).Remux(t.Context(), encoding.LiveInput{URL: "http://host/a.m3u8"}); err == nil {
		t.Fatal("empty ffmpeg path accepted")
	}
	_, err := LiveRemuxer{Path: "ffmpeg"}.Remux(t.Context(), encoding.LiveInput{URL: "/etc/passwd"})
	if err == nil || errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("file input: %v", err)
	}
}
