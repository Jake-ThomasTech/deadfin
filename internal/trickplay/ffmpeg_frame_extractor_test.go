// SPDX-License-Identifier: GPL-2.0-only

package trickplay_test

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/rootfs"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
	"github.com/deadfin-org/deadfin/internal/trickplay"
	"golang.org/x/sys/unix"
)

// TestFFmpegFrameExtractorHelper is a controlled executable standing in for
// ffmpeg. Its leader writes a valid frame, starts a descendant which retains
// inherited descriptors, and then exits with the requested status. The public
// extractor must reap the complete process group, not only the leader.
func TestFFmpegFrameExtractorHelper(t *testing.T) {
	if os.Getenv("GO_WANT_FRAME_EXTRACTOR_HELPER") != "1" {
		return
	}
	if err := runFrameExtractorHelper(os.Args[1:]); err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(125)
	}
	os.Exit(frameExtractorHelperExitCode())
}

func TestFFmpegFrameExtractorReapsDescendantAfterLeaderExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process groups are unavailable on Windows")
	}

	for _, tc := range []struct {
		name      string
		exitCode  string
		assertErr func(*testing.T, error)
	}{
		{
			name:     "successful leader",
			exitCode: "0",
			assertErr: func(t *testing.T, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("Extract() = %v, want successful frame extraction", err)
				}
			},
		},
		{
			name:     "nonzero leader",
			exitCode: "23",
			assertErr: func(t *testing.T, err error) {
				t.Helper()
				if err == nil {
					t.Fatal("Extract() succeeded after ffmpeg leader exited with status 23")
				}
				if errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Extract() = %v, want the leader exit error rather than a deadline", err)
				}
				if !strings.Contains(err.Error(), "exit status 23") {
					t.Fatalf("Extract() = %v, want ffmpeg leader exit status 23", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newFrameExtractorFixture(t, tc.exitCode, 320, 240)
			extractor, err := trickplay.NewFFmpegFrameExtractor(fixture.path, nil)
			if err != nil {
				t.Fatalf("NewFFmpegFrameExtractor(): %v", err)
			}

			_, err = extractor.Extract(t.Context(), fixture.request(t))
			tc.assertErr(t, err)
			fixture.waitForDescendantExit(t)
		})
	}
}

func TestFFmpegFrameExtractorAcceptsCompletedFramesAfterWaitDelay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process groups are unavailable on Windows")
	}

	fixture := newFrameExtractorFixture(t, "0", 320, 240)
	extractor, err := trickplay.NewFFmpegFrameExtractor(fixture.path, nil)
	if err != nil {
		t.Fatalf("NewFFmpegFrameExtractor(): %v", err)
	}
	extraction, err := extractor.Extract(t.Context(), fixture.request(t))
	if err != nil {
		t.Fatalf("Extract(completed ffmpeg leader with pipe-holding descendant): %v", err)
	}
	if got, want := extraction.Height, 240; got != want {
		t.Fatalf("Extract() height = %d, want decoded completed frame height %d", got, want)
	}
	if got, want := len(extraction.Names), 1; got != want {
		t.Fatalf("Extract() frame names = %d, want %d completed frame", got, want)
	}
	if got, want := extraction.Names[0], "00000001.jpg"; got != want {
		t.Fatalf("Extract() completed frame = %q, want %q", got, want)
	}
	fixture.waitForDescendantExit(t)
}

func TestFFmpegFrameExtractorPreservesSlashRelativeExecutableAcrossSinkChdir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX descriptor inheritance is unavailable on Windows")
	}

	fixture := newFrameExtractorFixture(t, "0", 320, 240)
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd(): %v", err)
	}
	if err := os.Chdir(filepath.Dir(fixture.path)); err != nil {
		t.Fatalf("Chdir(fake ffmpeg directory): %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(workingDirectory); err != nil {
			t.Errorf("restore test working directory: %v", err)
		}
	})

	extractor, err := trickplay.NewFFmpegFrameExtractor("./ffmpeg", nil)
	if err != nil {
		t.Fatalf("NewFFmpegFrameExtractor(./ffmpeg): %v", err)
	}
	extraction, err := extractor.Extract(t.Context(), fixture.request(t))
	if err != nil {
		t.Fatalf("Extract(./ffmpeg through frame sink trampoline): %v", err)
	}
	if got, want := len(extraction.Names), 1; got != want {
		t.Fatalf("Extract(./ffmpeg) frame count = %d, want %d", got, want)
	}
	if got, want := extraction.Names[0], "00000001.jpg"; got != want {
		t.Fatalf("Extract(./ffmpeg) frame name = %q, want %q", got, want)
	}
	fixture.waitForDescendantExit(t)
}

func TestFFmpegFrameExtractorPassesPinnedSourceThroughTrampoline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX descriptor inheritance is unavailable on Windows")
	}

	fixture := newFrameExtractorFixture(t, "0", 320, 240)
	source := openPinnedFrameSource(t, filepath.Join(t.TempDir(), "original.mkv"))
	device, inode := source.Identity()
	t.Setenv("FRAME_EXTRACTOR_EXPECTED_SOURCE_ID", fmt.Sprintf("%d:%d", device, inode))
	extractor, err := trickplay.NewFFmpegFrameExtractor(fixture.path, nil)
	if err != nil {
		t.Fatalf("NewFFmpegFrameExtractor(): %v", err)
	}

	_, err = extractor.Extract(t.Context(), trickplay.FrameRequest{
		Source: source.File(), Output: newFrameExtractorOutput(t, filepath.Join(t.TempDir(), "frames")), Width: 320,
		IntervalMilliseconds: 1_000,
	})
	if err != nil {
		t.Fatalf("Extract(pinned source): %v", err)
	}
	if got, want := frameExtractorArgValue(t, fixture.args(t), "-i"), "/dev/fd/4"; got != want {
		t.Fatalf("ffmpeg input = %q, want inherited descriptor path %q", got, want)
	}
	fixture.waitForDescendantExit(t)
}

func TestFrameSinkTrampolineRejectsForgedSourceIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX descriptor inheritance is unavailable on Windows")
	}

	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable(): %v", err)
	}
	output := newFrameExtractorOutput(t, filepath.Join(t.TempDir(), "frames"))
	sink, err := output.ProcessFile()
	if err != nil {
		t.Fatalf("ProcessFile(frame sink): %v", err)
	}
	t.Cleanup(func() {
		if err := sink.Close(); err != nil {
			t.Errorf("Close(frame sink): %v", err)
		}
	})
	sinkIdentity, err := output.Stat()
	if err != nil {
		t.Fatalf("Stat(frame sink): %v", err)
	}
	source := openPinnedFrameSource(t, filepath.Join(t.TempDir(), "source.mkv"))

	command := exec.Command(binary, "-test.run=^$")
	command.ExtraFiles = []*os.File{sink, source.File()}
	command.Env = append(os.Environ(),
		"DEADFIN_FRAME_SINK_FD=3",
		fmt.Sprintf("DEADFIN_FRAME_SINK_ID=%d:%d", sinkIdentity.Dev, sinkIdentity.Ino),
		"DEADFIN_SOURCE_FD=4",
		"DEADFIN_SOURCE_ID=0:0",
	)
	outputBytes, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("trampoline with forged source identity succeeded: %s", outputBytes)
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 125 {
		t.Fatalf("trampoline forged source identity error = %v (output %q), want exit status 125", err, outputBytes)
	}
	if !strings.Contains(string(outputBytes), "source") {
		t.Fatalf("trampoline forged source identity output = %q, want source diagnostic", outputBytes)
	}
}

func TestFFmpegFrameExtractorUsesSourceRateAndDisplayAspectFilter(t *testing.T) {
	const intervalMilliseconds = int64(1_500)
	rate := float32(29.97)
	fixture := newFrameExtractorFixture(t, "0", 320, 240)
	extractor, err := trickplay.NewFFmpegFrameExtractor(fixture.path, nil)
	if err != nil {
		t.Fatalf("NewFFmpegFrameExtractor(): %v", err)
	}
	request := fixture.request(t)
	request.IntervalMilliseconds = intervalMilliseconds
	request.ReferenceFrameRate = &rate

	extraction, err := extractor.Extract(t.Context(), request)
	if err != nil {
		t.Fatalf("Extract(): %v", err)
	}
	if got, want := extraction.Height, 240; got != want {
		t.Fatalf("anamorphic 720x576 SAR 16:15 extraction height = %d, want display-aspect height %d at width 320", got, want)
	}
	if got, want := frameExtractorArgValue(t, fixture.args(t), "-vf"), "setpts=N/29.970/TB,fps=1000/1500,scale=trunc(min(max(iw\\,ih*(a*sar))\\,320)/2)*2:trunc(ow/(a*sar)/2)*2"; got != want {
		t.Fatalf("ffmpeg -vf = %q, want %q", got, want)
	}
	args := fixture.args(t)
	fpsModeIndex := frameExtractorArgIndex(t, args, "-fps_mode")
	if got, want := args[fpsModeIndex+1], "passthrough"; got != want {
		t.Fatalf("ffmpeg -fps_mode value = %q, want %q", got, want)
	}
	formatIndex := frameExtractorArgIndex(t, args, "-f")
	if fpsModeIndex >= formatIndex {
		t.Fatalf("ffmpeg -fps_mode appears at index %d, want before image2 format at index %d; args = %q", fpsModeIndex, formatIndex, args)
	}
	if got, want := args[formatIndex+1], "image2"; got != want {
		t.Fatalf("ffmpeg output format = %q, want %q", got, want)
	}
	if got, want := args[len(args)-1], "%08d.jpg"; got != want {
		t.Fatalf("ffmpeg output pattern = %q, want descriptor-relative %q", got, want)
	}
	fixture.waitForDescendantExit(t)
}

func TestFFmpegFrameExtractorScalesAnamorphicDisplayAspect(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("real FFmpeg extractor smoke requires ffmpeg: %v", err)
	}
	fixture := filepath.Join(t.TempDir(), "720x576-sar16-15.mkv")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=red:s=720x576:r=1:d=2",
		"-vf", "setsar=16/15", "-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", fixture,
	).CombinedOutput()
	if err != nil {
		t.Skipf("real FFmpeg extractor smoke requires mpeg4 encoding: %v\n%s", err, output)
	}

	extractor, err := trickplay.NewFFmpegFrameExtractor(ffmpegPath, nil)
	if err != nil {
		t.Fatalf("NewFFmpegFrameExtractor(%q): %v", ffmpegPath, err)
	}
	rate := float32(1)
	outputDir := newFrameExtractorOutput(t, filepath.Join(t.TempDir(), "frames"))
	extraction, err := extractor.Extract(ctx, trickplay.FrameRequest{
		Source: openExistingPinnedFrameSource(t, fixture).File(), Output: outputDir, Width: 320,
		IntervalMilliseconds: 1_000, ReferenceFrameRate: &rate,
	})
	if err != nil {
		t.Fatalf("Extract(anamorphic fixture): %v", err)
	}
	if got, want := extraction.Height, 240; got != want {
		t.Fatalf("720x576 SAR 16:15 extraction height = %d, want display-aspect height %d at width 320", got, want)
	}
}

func TestFFmpegFrameExtractorDoesNotWaitPastCallerDeadlineForExitedLeader(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process groups are unavailable on Windows")
	}

	const (
		callerDeadline       = 50 * time.Millisecond
		deadlineTolerance    = 300 * time.Millisecond
		helperReadinessBound = 5 * time.Second
	)
	fixture := newFrameExtractorFixture(t, "0", 320, 240)
	releaseFile := filepath.Join(filepath.Dir(fixture.pidFile), "deadline-release")
	readyFile := fixture.enableDeadlineReadiness(t, releaseFile)
	extractor, err := trickplay.NewFFmpegFrameExtractor(fixture.path, nil)
	if err != nil {
		t.Fatalf("NewFFmpegFrameExtractor(): %v", err)
	}
	ctx := newFrameExtractorArmedDeadlineContext(t.Context())
	defer ctx.cancel()
	result := make(chan error, 1)
	request := fixture.request(t)
	go func() {
		_, err := extractor.Extract(ctx, request)
		result <- err
	}()

	// Startup under a parallel suite can take longer than the behavior under
	// test. Arm the caller deadline only after the helper has atomically
	// published both process IDs and the exited leader is observable.
	frameExtractorWaitForFile(t, readyFile, helperReadinessBound)
	pids := fixture.readDeadlineProcessPIDs(t)
	t.Cleanup(func() {
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if err := os.WriteFile(releaseFile, []byte("exit leader"), 0o600); err != nil {
		t.Fatalf("WriteFile(deadline release): %v", err)
	}
	frameExtractorWaitForProcessExit(t, pids[0], helperReadinessBound)

	testClock := clock.NewReal()
	started := testClock.Now()
	ctx.arm(callerDeadline)
	select {
	case err = <-result:
	case <-time.After(callerDeadline + deadlineTolerance):
		t.Fatalf("Extract() did not return by caller deadline tolerance %s", callerDeadline+deadlineTolerance)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Extract() = %v, want caller deadline exceeded after leader exit", err)
	}
	if elapsed := testClock.Now().Sub(started); elapsed > callerDeadline+deadlineTolerance {
		t.Fatalf("Extract() returned after %s, want no later than caller deadline tolerance %s", elapsed, callerDeadline+deadlineTolerance)
	}
	for _, pid := range pids {
		frameExtractorWaitForProcessExit(t, pid, 2*time.Second)
	}
}

func TestFFmpegFrameExtractorPinsOutputDirectoryAcrossAncestorReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX descriptor inheritance is unavailable on Windows")
	}

	fixture := newFrameExtractorFixture(t, "0", 320, 240)
	ancestor := filepath.Join(t.TempDir(), "output-ancestor")
	outputPath := filepath.Join(ancestor, "frames")
	output := newFrameExtractorOutput(t, outputPath)
	movedAncestor := ancestor + "-moved"
	ready := filepath.Join(t.TempDir(), "trampoline-ready")
	release := filepath.Join(t.TempDir(), "trampoline-release")
	if err := unix.Mkfifo(release, 0o600); err != nil {
		t.Fatalf("Mkfifo(trampoline release): %v", err)
	}
	t.Setenv("FRAME_EXTRACTOR_SINK_READY", ready)
	t.Setenv("FRAME_EXTRACTOR_SINK_RELEASE", release)

	extractor, err := trickplay.NewFFmpegFrameExtractor(fixture.path, nil)
	if err != nil {
		t.Fatalf("NewFFmpegFrameExtractor(): %v", err)
	}
	request := fixture.request(t)
	request.Output = output
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := make(chan struct {
		extraction trickplay.FrameExtraction
		err        error
	}, 1)
	go func() {
		extraction, err := extractor.Extract(ctx, request)
		result <- struct {
			extraction trickplay.FrameExtraction
			err        error
		}{extraction: extraction, err: err}
	}()

	frameExtractorWaitForFile(t, ready, 5*time.Second)
	if err := os.Rename(ancestor, movedAncestor); err != nil {
		t.Fatalf("Rename(output ancestor): %v", err)
	}
	if err := os.Mkdir(ancestor, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement output ancestor): %v", err)
	}
	releaseFile, err := os.OpenFile(release, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile(trampoline release): %v", err)
	}
	if err := releaseFile.Close(); err != nil {
		t.Fatalf("Close(trampoline release): %v", err)
	}

	var completed struct {
		extraction trickplay.FrameExtraction
		err        error
	}
	select {
	case completed = <-result:
	case <-ctx.Done():
		t.Fatalf("Extract() after output ancestor replacement did not return: %v", ctx.Err())
	}
	if completed.err != nil {
		t.Fatalf("Extract() after output ancestor replacement: %v", completed.err)
	}
	if got, want := len(completed.extraction.Names), 1; got != want || completed.extraction.Names[0] != "00000001.jpg" {
		t.Fatalf("Extract() names = %q, want [00000001.jpg]", completed.extraction.Names)
	}
	entries, err := output.ReadNames()
	if err != nil {
		t.Fatalf("ReadNames(pinned output): %v", err)
	}
	if got, want := len(entries), 1; got != want {
		t.Fatalf("pinned output entries = %d, want %d", got, want)
	}
	replacementEntries, err := os.ReadDir(ancestor)
	if err != nil {
		t.Fatalf("ReadDir(replacement output ancestor): %v", err)
	}
	if len(replacementEntries) != 0 {
		t.Fatalf("replacement output ancestor entries = %#v, want byte-empty replacement", replacementEntries)
	}
	fixture.waitForDescendantExit(t)
}

type frameExtractorFixture struct {
	path    string
	argsLog string
	pidFile string
	request func(*testing.T) trickplay.FrameRequest
}

func newFrameExtractorFixture(t *testing.T, exitCode string, width, height int) frameExtractorFixture {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable(): %v", err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec \"$FRAME_EXTRACTOR_HELPER\" -test.run '^TestFFmpegFrameExtractorHelper$' -- \"$@\"\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(fake ffmpeg): %v", err)
	}
	argsLog := filepath.Join(root, "args")
	pidFile := filepath.Join(root, "descendant.pid")
	t.Setenv("FRAME_EXTRACTOR_HELPER", binary)
	t.Setenv("GO_WANT_FRAME_EXTRACTOR_HELPER", "1")
	t.Setenv("FRAME_EXTRACTOR_ARGS", argsLog)
	t.Setenv("FRAME_EXTRACTOR_DESCENDANT_PID", pidFile)
	t.Setenv("FRAME_EXTRACTOR_EXIT_CODE", exitCode)
	t.Setenv("FRAME_EXTRACTOR_OUTPUT_WIDTH", strconv.Itoa(width))
	t.Setenv("FRAME_EXTRACTOR_OUTPUT_HEIGHT", strconv.Itoa(height))
	return frameExtractorFixture{
		path: path, argsLog: argsLog, pidFile: pidFile,
		request: func(t *testing.T) trickplay.FrameRequest {
			t.Helper()
			return trickplay.FrameRequest{
				Source: openPinnedFrameSource(t, filepath.Join(t.TempDir(), "input.mkv")).File(), Output: newFrameExtractorOutput(t, filepath.Join(t.TempDir(), "frames")), Width: width,
				IntervalMilliseconds: 1_000,
			}
		},
	}
}

func (fixture frameExtractorFixture) enableDeadlineReadiness(t *testing.T, releaseFile string) string {
	t.Helper()
	readyFile := filepath.Join(filepath.Dir(fixture.pidFile), "deadline-ready")
	t.Setenv("FRAME_EXTRACTOR_DEADLINE_READY", readyFile)
	t.Setenv("FRAME_EXTRACTOR_DEADLINE_RELEASE", releaseFile)
	return readyFile
}

func (fixture frameExtractorFixture) readDeadlineProcessPIDs(t *testing.T) []int {
	t.Helper()
	data, err := os.ReadFile(fixture.pidFile)
	if err != nil {
		t.Fatalf("ReadFile(frame extractor process IDs): %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("frame extractor process IDs = %q, want leader and descendant", lines)
	}
	pids := make([]int, len(lines))
	for index, line := range lines {
		pid, parseErr := strconv.Atoi(line)
		if parseErr != nil || pid <= 0 {
			t.Fatalf("frame extractor process ID %q = %d, %v; want a positive PID", line, pid, parseErr)
		}
		pids[index] = pid
	}
	if pids[0] == pids[1] {
		t.Fatalf("frame extractor process IDs = %v, want distinct leader and descendant", pids)
	}
	return pids
}

type frameExtractorArmedDeadlineContext struct {
	context.Context
	done chan struct{}

	mu       sync.Mutex
	clock    clock.Clock
	deadline time.Time
	timer    *time.Timer
	once     sync.Once
}

func newFrameExtractorArmedDeadlineContext(parent context.Context) *frameExtractorArmedDeadlineContext {
	return &frameExtractorArmedDeadlineContext{Context: parent, clock: clock.NewReal(), done: make(chan struct{})}
}

func (ctx *frameExtractorArmedDeadlineContext) Deadline() (time.Time, bool) {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	return ctx.deadline, !ctx.deadline.IsZero()
}

func (ctx *frameExtractorArmedDeadlineContext) Done() <-chan struct{} {
	return ctx.done
}

func (ctx *frameExtractorArmedDeadlineContext) Err() error {
	select {
	case <-ctx.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (ctx *frameExtractorArmedDeadlineContext) arm(timeout time.Duration) {
	ctx.mu.Lock()
	ctx.deadline = ctx.clock.Now().Add(timeout)
	ctx.timer = time.AfterFunc(timeout, ctx.expire)
	ctx.mu.Unlock()
}

func (ctx *frameExtractorArmedDeadlineContext) cancel() {
	ctx.mu.Lock()
	if ctx.timer != nil {
		ctx.timer.Stop()
	}
	ctx.mu.Unlock()
	ctx.expire()
}

func (ctx *frameExtractorArmedDeadlineContext) expire() {
	ctx.once.Do(func() { close(ctx.done) })
}

func (fixture frameExtractorFixture) args(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(fixture.argsLog)
	if err != nil {
		t.Fatalf("ReadFile(ffmpeg args): %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func newFrameExtractorOutput(t *testing.T, path string) *rootfs.Dir {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(frame output): %v", err)
	}
	output, err := rootfs.Pin(path)
	if err != nil {
		t.Fatalf("Pin(frame output): %v", err)
	}
	t.Cleanup(func() {
		if err := output.Close(); err != nil {
			t.Errorf("Close(frame output): %v", err)
		}
	})
	return output
}

func openPinnedFrameSource(t *testing.T, path string) *sourcefile.Source {
	t.Helper()
	if err := os.WriteFile(path, []byte("pinned source bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile(source fixture): %v", err)
	}
	source, err := sourcefile.Open(path, sourcefile.Expected{})
	if err != nil {
		t.Fatalf("Open(source fixture): %v", err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Errorf("Close(source fixture): %v", err)
		}
	})
	return source
}

func openExistingPinnedFrameSource(t *testing.T, path string) *sourcefile.Source {
	t.Helper()
	source, err := sourcefile.Open(path, sourcefile.Expected{})
	if err != nil {
		t.Fatalf("Open(existing source fixture): %v", err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Errorf("Close(existing source fixture): %v", err)
		}
	})
	return source
}

func (fixture frameExtractorFixture) waitForDescendant(t *testing.T) int {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		data, err := os.ReadFile(fixture.pidFile)
		if err == nil {
			pid, parseErr := strconv.Atoi(string(data))
			if parseErr != nil || pid <= 0 {
				t.Fatalf("descendant pid = %q (%d, %v), want a positive PID", data, pid, parseErr)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ReadFile(descendant pid): %v", err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for ffmpeg descendant pid %q", fixture.pidFile)
		case <-poll.C:
		}
	}
}

func (fixture frameExtractorFixture) waitForDescendantExit(t *testing.T) {
	t.Helper()
	pid := fixture.waitForDescendant(t)
	frameExtractorWaitForProcessExit(t, pid, 2*time.Second)
}

func runFrameExtractorHelper(args []string) error {
	for index, arg := range args {
		if arg == "--" {
			args = args[index+1:]
			break
		}
	}
	if len(args) == 0 {
		return errors.New("frame extractor helper received no ffmpeg arguments")
	}
	if expectedSourceID := os.Getenv("FRAME_EXTRACTOR_EXPECTED_SOURCE_ID"); expectedSourceID != "" {
		var sourceStat unix.Stat_t
		if err := unix.Fstat(4, &sourceStat); err != nil {
			return fmt.Errorf("stat inherited source fd 4: %w", err)
		}
		if got := fmt.Sprintf("%d:%d", sourceStat.Dev, sourceStat.Ino); got != expectedSourceID {
			return fmt.Errorf("inherited source fd 4 identity = %s, want %s", got, expectedSourceID)
		}
	}
	if err := os.WriteFile(os.Getenv("FRAME_EXTRACTOR_ARGS"), []byte(strings.Join(args, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	output := strings.ReplaceAll(args[len(args)-1], "%08d", "00000001")
	if ready := os.Getenv("FRAME_EXTRACTOR_SINK_READY"); ready != "" {
		if err := os.WriteFile(ready, []byte("fchdir complete"), 0o600); err != nil {
			return err
		}
		gate, err := os.Open(os.Getenv("FRAME_EXTRACTOR_SINK_RELEASE"))
		if err != nil {
			return err
		}
		if err := gate.Close(); err != nil {
			return err
		}
	}
	width, err := strconv.Atoi(os.Getenv("FRAME_EXTRACTOR_OUTPUT_WIDTH"))
	if err != nil {
		return err
	}
	height, err := strconv.Atoi(os.Getenv("FRAME_EXTRACTOR_OUTPUT_HEIGHT"))
	if err != nil {
		return err
	}
	if err := writeFrameExtractorJPEG(output, width, height); err != nil {
		return err
	}
	descendant := exec.Command("sleep", "30")
	// os/exec normally inherits these descriptors for a nil child stream, but
	// assignment makes the pipe-retention condition explicit: the descendant
	// holds both frame-extractor capture pipes after the leader exits.
	descendant.Stdout = os.Stdout
	descendant.Stderr = os.Stderr
	if err := descendant.Start(); err != nil {
		return err
	}
	if ready := os.Getenv("FRAME_EXTRACTOR_DEADLINE_READY"); ready != "" {
		pids := strings.Join([]string{strconv.Itoa(os.Getpid()), strconv.Itoa(descendant.Process.Pid)}, "\n")
		if err := os.WriteFile(os.Getenv("FRAME_EXTRACTOR_DESCENDANT_PID"), []byte(pids+"\n"), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(ready, []byte("leader and descendant started"), 0o600); err != nil {
			return err
		}
		if release := os.Getenv("FRAME_EXTRACTOR_DEADLINE_RELEASE"); release != "" {
			for {
				if _, err := os.Stat(release); err == nil {
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		return nil
	}
	return os.WriteFile(os.Getenv("FRAME_EXTRACTOR_DESCENDANT_PID"), []byte(strconv.Itoa(descendant.Process.Pid)), 0o600)
}

func frameExtractorHelperExitCode() int {
	code, err := strconv.Atoi(os.Getenv("FRAME_EXTRACTOR_EXIT_CODE"))
	if err != nil {
		return 125
	}
	return code
}

func writeFrameExtractorJPEG(path string, width, height int) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	encodeErr := jpeg.Encode(file, image.NewRGBA(image.Rect(0, 0, width, height)), &jpeg.Options{Quality: 80})
	closeErr := file.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

func frameExtractorArgValue(t *testing.T, args []string, name string) string {
	t.Helper()
	index := frameExtractorArgIndex(t, args, name)
	if index+1 >= len(args) {
		t.Fatalf("ffmpeg argument %q has no value: %q", name, args)
	}
	return args[index+1]
}

func frameExtractorArgIndex(t *testing.T, args []string, name string) int {
	t.Helper()
	for index, arg := range args {
		if arg == name {
			return index
		}
	}
	t.Fatalf("ffmpeg arguments = %q, want %q", args, name)
	return -1
}

func frameExtractorWaitForProcessExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("ffmpeg descendant %d survived extractor completion", pid)
		case <-poll.C:
		}
	}
}

func frameExtractorWaitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Stat(%q): %v", path, err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for helper signal %q", path)
		case <-poll.C:
		}
	}
}
