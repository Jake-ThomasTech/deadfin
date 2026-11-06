// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/ffmpeg"
)

func (f family) progressive() ffmpeg.RemoteProgressiveJob {
	info, _ := f.source.Stat()
	output := filepath.Join(f.dir, "stream.mkv")
	return ffmpeg.RemoteProgressiveJob{
		Args:       []string{"-ss", "5", "-i", "file:" + f.path, "-c:v", "libx264", "-f", "matroska", "-y", output},
		SourcePath: f.path, Source: f.source, SourceSize: info.Size(), Output: output,
		PlaySessionID: "play", DeviceID: "device", Stderr: f.stderr,
	}
}

// TestProgressiveTranscodeStreamsTheOutputBack covers #624: a progressive
// job runs on the worker, and its growing output reaches the server file
// while ffmpeg writes it.
func TestProgressiveTranscodeStreamsTheOutputBack(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	worker := startTestAgent(t, hub, token, "w", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	process, err := hub.hub.StartProgressive(ctx, f.progressive())
	if err != nil {
		t.Fatalf("StartProgressive: %v", err)
	}
	ffmpegProcess := worker.process(t)
	args := strings.Join(ffmpegProcess.options.Args, " ")
	if !strings.HasSuffix(args, "-y "+filepath.Join(ffmpegProcess.options.Dir, "stream.mkv")) || !strings.Contains(args, "-ss 5") {
		t.Fatalf("worker argv = %s", args)
	}
	output := filepath.Join(f.dir, "stream.mkv")
	first := bytes.Repeat([]byte("a"), 3*maxOutputChunk/2)
	ffmpegProcess.write(t, "stream.mkv", string(first))
	eventually(t, worker.poll, func() bool {
		data, err := os.ReadFile(output)
		return err == nil && bytes.Equal(data, first)
	})
	whole := append(append([]byte(nil), first...), "and the rest"...)
	ffmpegProcess.write(t, "stream.mkv", string(whole))
	ffmpegProcess.end(nil)
	if err := waitResult(t, process); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(output); !bytes.Equal(data, whole) {
		t.Fatalf("server output has %d bytes, want %d", len(data), len(whole))
	}
}

// TestProgressiveOutputRefusesAGapAndFileUploads: an append at the wrong
// offset gets the server's size back, and the family file route refuses
// a progressive job.
func TestProgressiveOutputRefusesAGapAndFileUploads(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	startTestAgent(t, hub, token, "w", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	process, err := hub.hub.StartProgressive(ctx, f.progressive())
	if err != nil {
		t.Fatal(err)
	}
	jobID, jobToken := onlyJob(t, hub)
	jobBase := hub.server.URL + pathJobs + jobID
	post := func(offset int, body string) (int, string) {
		request, _ := http.NewRequest(http.MethodPost, jobBase+"/output", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+jobToken)
		request.Header.Set(OutputOffsetHeader, strconv.Itoa(offset))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		return response.StatusCode, response.Header.Get(OutputOffsetHeader)
	}
	if code, _ := post(0, "head"); code != http.StatusNoContent {
		t.Fatalf("first chunk = %d", code)
	}
	if code, have := post(10, "gap"); code != http.StatusConflict || have != "4" {
		t.Fatalf("gap = %d, server size %q; want 409 and 4", code, have)
	}
	if code, _ := post(4, "tail"); code != http.StatusNoContent {
		t.Fatalf("second chunk = %d", code)
	}
	if got := putArtifact(t, jobBase, "stream.mkv", jobToken, ArtifactComplete, []byte("x")); got != http.StatusBadRequest {
		t.Fatalf("file PUT on a progressive job = %d, want 400", got)
	}
	if data, _ := os.ReadFile(filepath.Join(f.dir, "stream.mkv")); string(data) != "headtail" {
		t.Fatalf("server output = %q", data)
	}
	process.Kill()
	_ = waitResult(t, process)
}
