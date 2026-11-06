// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestFFmpegErrorExitIsLoggedWithJobAndLogPath(t *testing.T) {
	fixture := newProgressiveFixture(t, "early-error")
	var records lockedBuffer
	manager, err := NewTranscodeManager(fixture.ffmpeg, fixture.dir, TranscodeManagerOptions{
		Clock:                  progressiveTestClock(clock.NewReal()),
		OutputPollInterval:     100 * time.Millisecond,
		ReaderRetryInterval:    50 * time.Millisecond,
		ReaderStallTimeout:     30 * time.Second,
		ProgressiveIdleTimeout: 10 * time.Second,
		KillGracePeriod:        5 * time.Second,
		Logger:                 slog.New(slog.NewTextHandler(&records, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProgressiveManager(t, manager)
	request := fixture.request("broken.ts")
	if _, err := manager.OpenProgressive(context.Background(), request); err == nil {
		t.Fatal("OpenProgressive succeeded despite early ffmpeg failure")
	}
	got := records.String()
	for _, want := range []string{"level=ERROR", "ffmpeg exited with an error", "/broken.ts error=", "/broken.ts.stderr.log", "exit status 23", "ffmpeg_tail="} {
		if !strings.Contains(got, want) {
			t.Fatalf("log records = %q, want %q", got, want)
		}
	}
}

func TestLogTail(t *testing.T) {
	dir := t.TempDir()
	if got := logTail(filepath.Join(dir, "missing"), 64); got != "" {
		t.Errorf("logTail(missing) = %q, want empty", got)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := logTail(empty, 64); got != "" {
		t.Errorf("logTail(empty) = %q, want empty", got)
	}

	path := filepath.Join(dir, "log")
	body := "ffmpeg version 8\n" + strings.Repeat("x", 4096) + "\n[mp4 @ 0x1] dimensions not set\nConversion failed!\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := logTail(path, 64)
	if !strings.Contains(got, "Conversion failed!") || strings.Contains(got, "ffmpeg version") {
		t.Errorf("logTail() = %q, want the trailing reason only", got)
	}
	if len(got) > 64 {
		t.Errorf("logTail() = %d bytes, want at most 64", len(got))
	}
	if strings.HasPrefix(got, "x") && strings.HasPrefix(strings.TrimPrefix(got, "\n"), "x") {
		t.Errorf("logTail() = %q, want a whole-line start", got)
	}
	if whole := logTail(path, 1<<20); whole != strings.TrimSpace(body) {
		t.Errorf("logTail(whole) truncated a short file")
	}
}
