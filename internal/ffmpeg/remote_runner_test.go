// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

// fakeRemoteRunner answers StartHLS from a script of results: nil places the
// job on a fake remote process, an error refuses it.
type fakeRemoteRunner struct {
	mu      sync.Mutex
	results []error
	calls   []RemoteHLSJob
	placed  chan *fakeRemoteProcess
}

func newFakeRemoteRunner(results ...error) *fakeRemoteRunner {
	return &fakeRemoteRunner{results: results, placed: make(chan *fakeRemoteProcess, len(results))}
}

func (r *fakeRemoteRunner) StartHLS(ctx context.Context, job RemoteHLSJob) (RemoteProcess, error) {
	r.mu.Lock()
	r.calls = append(r.calls, job)
	result := ErrNoRemoteRunner
	if len(r.results) > 0 {
		result = r.results[0]
		r.results = r.results[1:]
	}
	r.mu.Unlock()
	if result != nil {
		return nil, result
	}
	process := &fakeRemoteProcess{job: job, done: make(chan struct{}), quit: make(chan struct{})}
	r.placed <- process
	return process, nil
}

func (r *fakeRemoteRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *fakeRemoteRunner) receive(t *testing.T) *fakeRemoteProcess {
	t.Helper()
	select {
	case process := <-r.placed:
		return process
	case <-time.After(fixtureTimeout):
		t.Fatal("timed out waiting for a remote placement")
		return nil
	}
}

type fakeRemoteProcess struct {
	job RemoteHLSJob

	mu       sync.Mutex
	stdin    bytes.Buffer
	quitOnce sync.Once
	quit     chan struct{}
	endOnce  sync.Once
	done     chan struct{}
	err      error
}

func (p *fakeRemoteProcess) Stdin() io.Writer { return fakeRemoteStdin{p} }

type fakeRemoteStdin struct{ p *fakeRemoteProcess }

func (s fakeRemoteStdin) Write(data []byte) (int, error) {
	s.p.mu.Lock()
	s.p.stdin.Write(data)
	s.p.mu.Unlock()
	if bytes.HasPrefix(data, []byte("q")) {
		s.p.quitOnce.Do(func() { close(s.p.quit) })
	}
	return len(data), nil
}

func (p *fakeRemoteProcess) Wait() error {
	<-p.done
	return p.err
}

func (p *fakeRemoteProcess) Kill() { p.end(errors.New("killed")) }

// end follows the RemoteProcess contract: Stderr closes before Wait returns.
func (p *fakeRemoteProcess) end(err error) {
	p.endOnce.Do(func() {
		_ = p.job.Stderr.Close()
		p.err = err
		close(p.done)
	})
}

func (p *fakeRemoteProcess) write(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.job.Directory, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func remoteHLSManager(t *testing.T, fixture hlsFixture, runner RemoteRunner) *TranscodeManager {
	t.Helper()
	manager, err := NewTranscodeManager(fixture.ffmpeg, fixture.dir, TranscodeManagerOptions{
		Clock:                  clock.NewReal(),
		OutputPollInterval:     10 * time.Millisecond,
		ReaderRetryInterval:    10 * time.Millisecond,
		ReaderStallTimeout:     30 * time.Second,
		ProgressiveIdleTimeout: 10 * time.Second,
		KillGracePeriod:        5 * time.Second,
		RemoteRunner:           runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestRemoteRunnerRunsHLSFamilyBehindUnchangedManagerContract(t *testing.T) {
	fixture := newHLSFixture(t, "hold")
	runner := newFakeRemoteRunner(nil)
	manager := remoteHLSManager(t, fixture, runner)
	defer closeHLSManager(t, manager)
	request := fixture.request("remote", 4)
	if err := manager.StartHLS(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	remote := runner.receive(t)
	if remote.job.Playlist != request.PlaylistPath || remote.job.SegmentPattern != request.SegmentPattern || remote.job.Directory != fixture.dir {
		t.Fatalf("remote job outputs = %q %q %q, want the request's family", remote.job.Playlist, remote.job.SegmentPattern, remote.job.Directory)
	}
	if got := hlsStartCount(t, fixture, "remote"); got != 0 {
		t.Fatalf("local ffmpeg started %d times for a remote family", got)
	}
	remote.write(t, "remote.m3u8", "#EXTM3U\n#EXTINF:4,\nremote0.ts\n")
	remote.write(t, "remote0.ts", "remote segment zero")
	remote.write(t, "remote1.ts", "")
	file, err := manager.WaitSegment(context.Background(), request.PlaylistPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := readHLSSegment(t, file); got != "remote segment zero" {
		t.Fatalf("segment 0 = %q", got)
	}
	closeHLSSegment(t, file)
	playlist, err := manager.WaitPlaylist(context.Background(), request.PlaylistPath, 1)
	if err != nil || playlist == "" {
		t.Fatalf("WaitPlaylist = %q, %v", playlist, err)
	}

	stopped := make(chan error, 1)
	go func() {
		stopped <- manager.StopPlaySession(context.Background(), request.PlaySessionID, request.DeviceID)
	}()
	select {
	case <-remote.quit:
	case <-time.After(fixtureTimeout):
		t.Fatal("stop did not forward q to the remote stdin")
	}
	remote.end(nil)
	if err := <-stopped; err != nil {
		t.Fatalf("StopPlaySession: %v", err)
	}
	if _, err := os.Stat(fixture.segmentPath(request, 0)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped remote family kept segment 0: %v", err)
	}
}

func TestRemoteRunnerLostMidStreamRestartsAtRequestedSegmentLocally(t *testing.T) {
	fixture := newHLSFixture(t, "hold")
	runner := newFakeRemoteRunner(nil, ErrNoRemoteRunner)
	manager := remoteHLSManager(t, fixture, runner)
	defer closeHLSManager(t, manager)
	request := fixture.request("failover", 4)
	if err := manager.StartHLS(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	remote := runner.receive(t)
	remote.write(t, "failover.m3u8", "#EXTM3U\n")
	remote.write(t, "failover0.ts", "segment zero")
	remote.write(t, "failover1.ts", "segment one")

	pending := make(chan hlsWaitResult, 1)
	go func() {
		file, err := manager.WaitSegment(context.Background(), request.PlaylistPath, 2)
		pending <- hlsWaitResult{file: file, err: err}
	}()
	remote.end(ErrRunnerLost)
	waitForHLSJobExit(t, manager, request.PlaylistPath)
	result := <-pending
	if !errors.Is(result.err, ErrRunnerLost) {
		t.Fatalf("WaitSegment(2) after host loss = %v, want ErrRunnerLost", result.err)
	}

	// Completed segments survive the loss and serve without a relaunch.
	if err := manager.RestartHLSAt(context.Background(), request, 0); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("cached segment relaunched: %d placements", runner.callCount())
	}
	file, err := manager.WaitSegment(context.Background(), request.PlaylistPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := readHLSSegment(t, file); got != "segment zero" {
		t.Fatalf("retained segment 0 = %q", got)
	}
	closeHLSSegment(t, file)

	// The missing segment restarts the family; no worker fits, so it
	// runs locally.
	if err := manager.RestartHLSAt(context.Background(), request, 2); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 2 {
		t.Fatalf("restart placements = %d, want a second remote offer", runner.callCount())
	}
	fixture.waitStarted(t, "failover")
	if got := hlsStartCount(t, fixture, "failover"); got != 1 {
		t.Fatalf("local starts after failover = %d, want 1", got)
	}
}

func TestRemoteRunnerFallsBackToLocalWhenNoWorkerFits(t *testing.T) {
	for _, refusal := range []error{ErrNoRemoteRunner, errors.New("worker refused the job")} {
		t.Run(refusal.Error(), func(t *testing.T) {
			fixture := newHLSFixture(t, "hold")
			runner := newFakeRemoteRunner(refusal)
			manager := remoteHLSManager(t, fixture, runner)
			defer closeHLSManager(t, manager)
			request := fixture.request("fallback", 4)
			if err := manager.StartHLS(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			fixture.waitStarted(t, "fallback")
			if runner.callCount() != 1 {
				t.Fatalf("remote offers = %d, want 1", runner.callCount())
			}
			if got := hlsStartCount(t, fixture, "fallback"); got != 1 {
				t.Fatalf("local starts = %d, want 1", got)
			}
		})
	}
}

func TestRemoteRunnerIsNotOfferedSubtitleBurnIn(t *testing.T) {
	fixture := newHLSFixture(t, "hold")
	runner := newFakeRemoteRunner(nil)
	manager := remoteHLSManager(t, fixture, runner)
	job := &transcodeJob{hls: &hlsJob{subtitleSource: "subtitles=f='x'"}}
	if manager.startRemote(job) {
		t.Fatal("a burn-in family was offered to the remote runner")
	}
	if runner.callCount() != 0 {
		t.Fatalf("remote offers = %d, want 0", runner.callCount())
	}
	closeHLSManager(t, manager)
}

func TestRemoteRunnerIsNotOfferedALocalFamily(t *testing.T) {
	fixture := newHLSFixture(t, "hold")
	runner := newFakeRemoteRunner(nil)
	manager := remoteHLSManager(t, fixture, runner)
	job := &transcodeJob{hls: &hlsJob{local: true}}
	if manager.startRemote(job) {
		t.Fatal("a local family was offered to the remote runner")
	}
	if runner.callCount() != 0 {
		t.Fatalf("remote offers = %d, want 0", runner.callCount())
	}
	closeHLSManager(t, manager)
}

func TestRemoteRunnerReceivesCanonicalOutputOperands(t *testing.T) {
	fixture := newHLSFixture(t, "hold")
	real := filepath.Join(fixture.root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(fixture.root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	fixture.dir = filepath.Join(alias, "transcodes")
	runner := newFakeRemoteRunner(nil)
	manager := remoteHLSManager(t, fixture, runner)
	request := fixture.request("canonical", 4)
	request.Args = append(request.Args, "-hls_segment_filename", request.SegmentPattern, "-y", request.PlaylistPath)
	if err := manager.StartHLS(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	remote := runner.receive(t)
	if strings.HasPrefix(remote.job.Playlist, alias) || !strings.HasPrefix(remote.job.Directory, mustEvalSymlinks(t, real)) {
		t.Fatalf("remote family %q is not canonical", remote.job.Directory)
	}
	args := strings.Join(remote.job.Args, " ")
	if strings.Contains(args, alias) {
		t.Fatalf("remote argv keeps the aliased path: %s", args)
	}
	if !strings.Contains(args, "-hls_segment_filename "+remote.job.SegmentPattern) || !strings.HasSuffix(args, "-y "+remote.job.Playlist) {
		t.Fatalf("remote argv = %s, want canonical output operands", args)
	}
	remote.end(nil)
	closeHLSManager(t, manager)
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestRemoteRunnerRefusalFailsTheStartAndRunsNothing(t *testing.T) {
	fixture := newHLSFixture(t, "hold")
	runner := newFakeRemoteRunner(fmt.Errorf("%w: queue-timeout", ErrRemoteRefused))
	manager := remoteHLSManager(t, fixture, runner)
	defer closeHLSManager(t, manager)
	if err := manager.StartHLS(context.Background(), fixture.request("refused", 4)); !errors.Is(err, ErrRemoteRefused) {
		t.Fatalf("StartHLS = %v, want ErrRemoteRefused", err)
	}
	if got := hlsStartCount(t, fixture, "refused"); got != 0 {
		t.Fatalf("local starts = %d, want 0", got)
	}
}

// blockingRemoteRunner holds StartHLS until its ctx ends, like a queued job.
type blockingRemoteRunner struct{ entered, left chan error }

func (r blockingRemoteRunner) StartHLS(ctx context.Context, _ RemoteHLSJob) (RemoteProcess, error) {
	r.entered <- nil
	<-ctx.Done()
	r.left <- ctx.Err()
	return nil, ctx.Err()
}

func TestCanceledRequestEndsAQueuedRemoteStartAndRunsNothing(t *testing.T) {
	fixture := newHLSFixture(t, "hold")
	runner := blockingRemoteRunner{entered: make(chan error, 1), left: make(chan error, 1)}
	manager := remoteHLSManager(t, fixture, runner)
	defer closeHLSManager(t, manager)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- manager.StartHLS(ctx, fixture.request("queued", 4)) }()
	<-runner.entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("StartHLS = %v, want context.Canceled", err)
	}
	if err := <-runner.left; !errors.Is(err, context.Canceled) {
		t.Fatalf("remote start ctx = %v, want canceled when no request waits", err)
	}
	if got := hlsStartCount(t, fixture, "queued"); got != 0 {
		t.Fatalf("local starts = %d, want 0", got)
	}
}

func TestLostRemoteFamilyHandsOffAtTheFirstIncompleteSegment(t *testing.T) {
	fixture := newHLSFixture(t, "hold")
	runner := newFakeRemoteRunner(nil, ErrNoRemoteRunner)
	manager := remoteHLSManager(t, fixture, runner)
	defer closeHLSManager(t, manager)
	request := fixture.request("handoff", 4)
	handoffs := make(chan int, 1)
	request.Handoff = func(ctx context.Context, index int) error {
		handoffs <- index
		restart := fixture.request("handoff", 4)
		return manager.RestartHLSAt(ctx, restart, index)
	}
	if err := manager.StartHLS(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	remote := runner.receive(t)
	remote.write(t, "handoff.m3u8", "#EXTM3U\n")
	remote.write(t, "handoff0.ts", "segment zero")
	remote.write(t, "handoff1.ts", "segment one")
	remote.end(ErrRunnerLost)

	select {
	case index := <-handoffs:
		if index != 2 {
			t.Fatalf("handoff index = %d, want 2, the first incomplete segment", index)
		}
	case <-time.After(fixtureTimeout):
		t.Fatal("a lost family was not handed off")
	}
	// The replacement starts before any client asks for segment 2.
	fixture.waitStarted(t, "handoff")
	if runner.callCount() != 2 {
		t.Fatalf("placements = %d, want the replacement offered to the runner", runner.callCount())
	}
}

// TestRemoteOnlyFamilyNeverRunsLocally: a remote-only family (#854) that
// no worker takes, or that no runner can take, fails its start and runs
// nothing on this host.
func TestRemoteOnlyFamilyNeverRunsLocally(t *testing.T) {
	for name, runner := range map[string]RemoteRunner{
		"no worker fits": newFakeRemoteRunner(ErrNoRemoteRunner),
		"start error":    newFakeRemoteRunner(errors.New("worker refused the job")),
		"no runner":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newHLSFixture(t, "hold")
			manager := remoteHLSManager(t, fixture, runner)
			defer closeHLSManager(t, manager)
			request := fixture.request("remote-only", 4)
			request.RemoteOnly = true
			if err := manager.StartHLS(context.Background(), request); !errors.Is(err, ErrRemoteOnly) {
				t.Fatalf("StartHLS = %v, want ErrRemoteOnly", err)
			}
			if got := hlsStartCount(t, fixture, "remote-only"); got != 0 {
				t.Fatalf("local starts = %d, want 0", got)
			}
			if fake, ok := runner.(*fakeRemoteRunner); ok && (fake.callCount() != 1 || !fake.calls[0].RemoteOnly) {
				t.Fatalf("remote offers = %+v, want one remote-only offer", fake.calls)
			}
		})
	}
}
