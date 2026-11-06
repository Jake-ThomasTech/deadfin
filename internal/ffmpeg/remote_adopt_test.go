// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
)

// releasableProcess is a fake remote process whose host keeps the job
// across a server restart (#618).
type releasableProcess struct {
	*fakeRemoteProcess
	released chan struct{}
}

func (p releasableProcess) Release() bool {
	close(p.released)
	p.end(ErrRunnerReleased)
	return true
}

type releasingRunner struct{ *fakeRemoteRunner }

func (r releasingRunner) StartHLS(ctx context.Context, job RemoteHLSJob) (RemoteProcess, error) {
	process, err := r.fakeRemoteRunner.StartHLS(ctx, job)
	if err != nil {
		return nil, err
	}
	return releasableProcess{fakeRemoteProcess: process.(*fakeRemoteProcess), released: make(chan struct{})}, nil
}

func TestClearTranscodesKeepsOnlyLiveFamilies(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "transcodes")
	for _, name := range []string{"abc.m3u8", "abc0.ts", "abc1.ts", "abc-1.mp4", ".abc2.ts.upload-1", "other.m3u8", "other0.ts", "nested/x.ts"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	keep := []KeptFamily{
		{Playlist: filepath.Join(dir, "abc.m3u8"), SegmentPattern: filepath.Join(dir, "abc%d.ts")},
		{Playlist: "/elsewhere/z.m3u8", SegmentPattern: "/elsewhere/z%d.ts"},
	}
	if err := clearTranscodes(dir, keep); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	want := []string{"abc-1.mp4", "abc.m3u8", "abc0.ts", "abc1.ts"}
	if len(names) != len(want) {
		t.Fatalf("kept %v, want %v", names, want)
	}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("kept %v, want %v", names, want)
		}
	}
	if err := clearTranscodes(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a wipe without live families kept the directory: %v", err)
	}
}

// TestReleasedRemoteFamilyIsAdoptedByTheNextManager: Close releases a
// remote family instead of stopping it and keeps its files; the next
// manager keeps them through its startup wipe, adopts the family, and
// serves the next segment with no restart. The first request joins the
// adopted family.
func TestReleasedRemoteFamilyIsAdoptedByTheNextManager(t *testing.T) {
	fixture := newHLSFixture(t, "hold")
	runner := releasingRunner{newFakeRemoteRunner(nil)}
	manager := remoteHLSManager(t, fixture, runner)
	request := fixture.request("kept", 4)
	if err := manager.StartHLS(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	remote := runner.receive(t)
	remote.write(t, "kept.m3u8", "#EXTM3U\n")
	remote.write(t, "kept0.ts", "segment zero")
	closeHLSManager(t, manager)
	remote.mu.Lock()
	stdin := remote.stdin.String()
	remote.mu.Unlock()
	if stdin != "" {
		t.Fatalf("Close sent %q to a released family", stdin)
	}
	if _, err := os.Stat(fixture.segmentPath(request, 0)); err != nil {
		t.Fatalf("Close removed a released family: %v", err)
	}

	next, err := NewTranscodeManager(fixture.ffmpeg, fixture.dir, TranscodeManagerOptions{
		Clock: manager.options.Clock, KillGracePeriod: 5 * time.Second,
		KeepFamilies: []KeptFamily{{Playlist: request.PlaylistPath, SegmentPattern: request.SegmentPattern}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeHLSManager(t, next)
	if _, err := os.Stat(fixture.segmentPath(request, 0)); err != nil {
		t.Fatalf("the startup wipe removed a live family: %v", err)
	}
	adopted := &fakeRemoteProcess{done: make(chan struct{}), quit: make(chan struct{})}
	err = next.AdoptHLS(context.Background(), AdoptHLSRequest{
		PlaylistPath: request.PlaylistPath, SegmentPattern: request.SegmentPattern,
		PlaySessionID: request.PlaySessionID, DeviceID: request.DeviceID, SegmentLengthSeconds: 4,
		Remote: func(stderr io.WriteCloser) RemoteProcess {
			adopted.job = RemoteHLSJob{Directory: fixture.dir, Stderr: stderr}
			return adopted
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.AdoptHLS(context.Background(), AdoptHLSRequest{
		PlaylistPath: request.PlaylistPath, SegmentPattern: request.SegmentPattern, SegmentLengthSeconds: 4,
		Remote: func(io.WriteCloser) RemoteProcess { return adopted },
	}); err == nil {
		t.Fatal("a second adoption of the same family succeeded")
	}
	adopted.write(t, "kept1.ts", "segment one")
	adopted.write(t, "kept2.ts", "")
	file, err := next.WaitSegment(context.Background(), request.PlaylistPath, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := readHLSSegment(t, file); got != "segment one" {
		t.Fatalf("segment 1 = %q", got)
	}
	closeHLSSegment(t, file)
	join := fixture.request("kept", 4)
	container := "ts"
	join.TranscodingInfo = &dto.TranscodingInfo{Container: &container}
	if err := next.RestartHLSAt(context.Background(), join, 2); err != nil {
		t.Fatalf("join the adopted family: %v", err)
	}
	// The joining request gives the family its rendition, with the session
	// projection.
	if info, found := next.TranscodingInfo(join.DeviceID); !found || info.Container == nil || *info.Container != container {
		t.Fatalf("TranscodingInfo of the claimed family = %+v, %v", info, found)
	}
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("remote starts = %d, want 1: the adopted family restarted", calls)
	}
	if got := hlsStartCount(t, fixture, "kept"); got != 0 {
		t.Fatalf("local ffmpeg started %d times for an adopted family", got)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- next.StopPlaySession(context.Background(), request.PlaySessionID, request.DeviceID) }()
	select {
	case <-adopted.quit:
	case <-time.After(fixtureTimeout):
		t.Fatal("stop did not forward q to the adopted family")
	}
	adopted.end(nil)
	if err := <-stopped; err != nil {
		t.Fatalf("StopPlaySession: %v", err)
	}
}
