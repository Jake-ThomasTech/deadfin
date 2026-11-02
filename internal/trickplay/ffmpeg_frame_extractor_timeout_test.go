// SPDX-License-Identifier: GPL-2.0-only

package trickplay

import (
	"bytes"
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
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	MaybeRunFrameSinkTrampoline()
	os.Exit(m.Run())
}

func TestExtractedFrameNamesRejectsUnexpectedOutputName(t *testing.T) {
	output := newPinnedFrameOutput(t, filepath.Join(t.TempDir(), "frames"))
	file, err := output.CreateExclusive("not-a-frame.jpg", 0o600)
	if err != nil {
		t.Fatalf("CreateExclusive(unexpected frame): %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close(unexpected frame): %v", err)
	}

	_, err = extractedFrameNames(output)
	if err == nil {
		t.Fatal("extractedFrameNames() succeeded for an unexpected output name")
	}
	if !strings.Contains(err.Error(), `unexpected frame extractor output "not-a-frame.jpg"`) {
		t.Fatalf("extractedFrameNames() = %v, want unexpected output-name rejection", err)
	}
}

// TestFFmpegFrameExtractorHungHelper behaves like a no-output ffmpeg process:
// it announces its process tree only through a test-only file, then ignores
// normal completion forever while retaining the extractor's inherited pipes.
func TestFFmpegFrameExtractorHungHelper(t *testing.T) {
	if os.Getenv("GO_WANT_HUNG_FRAME_EXTRACTOR_HELPER") != "1" {
		return
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(125)
	}
	pids := strings.Join([]string{strconv.Itoa(os.Getpid()), strconv.Itoa(child.Process.Pid)}, "\n")
	if err := os.WriteFile(os.Getenv("HUNG_FRAME_EXTRACTOR_PIDS"), []byte(pids+"\n"), 0o600); err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(125)
	}
	select {}
}

func TestFFmpegFrameExtractorTerminatesNoOutputProcessAtWatchdogDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process groups are unavailable on Windows")
	}
	const watchdog = time.Second
	const readinessBound = 5 * time.Second
	const completionBound = 3 * time.Second
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable(): %v", err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec \"$HUNG_FRAME_EXTRACTOR_HELPER\" -test.run '^TestFFmpegFrameExtractorHungHelper$' -- \"$@\"\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(fake ffmpeg): %v", err)
	}
	pidFile := filepath.Join(root, "pids")
	t.Setenv("HUNG_FRAME_EXTRACTOR_HELPER", binary)
	t.Setenv("GO_WANT_HUNG_FRAME_EXTRACTOR_HELPER", "1")
	t.Setenv("HUNG_FRAME_EXTRACTOR_PIDS", pidFile)

	extractor, err := newFFmpegFrameExtractor(path, watchdog)
	if err != nil {
		t.Fatalf("newFFmpegFrameExtractor(): %v", err)
	}
	output := newPinnedFrameOutput(t, filepath.Join(root, "frames"))
	source := openTrickplayTestSource(t).File()
	result := make(chan error, 1)
	go func() {
		_, err := extractor.Extract(context.Background(), FrameRequest{
			Source: source, Output: output, Width: 320,
			IntervalMilliseconds: 1_000,
		})
		result <- err
	}()

	pids := waitForHungFrameExtractorPIDs(t, pidFile, readinessBound)
	t.Cleanup(func() {
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Extract(no-output ffmpeg) = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(completionBound):
		t.Fatal("Extract(no-output ffmpeg) did not return by its injected watchdog deadline")
	}
	for _, pid := range pids {
		waitForHungFrameExtractorExit(t, pid, readinessBound)
	}
}

func waitForHungFrameExtractorPIDs(t *testing.T, path string, timeout time.Duration) []int {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if len(lines) != 2 {
				t.Fatalf("hung helper PIDs = %q, want leader and descendant", lines)
			}
			pids := make([]int, len(lines))
			for index, line := range lines {
				pid, parseErr := strconv.Atoi(line)
				if parseErr != nil || pid <= 0 {
					t.Fatalf("hung helper PID %q = %d, %v; want a positive PID", line, pid, parseErr)
				}
				pids[index] = pid
			}
			return pids
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ReadFile(hung helper PIDs): %v", err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for hung ffmpeg helper %q", path)
		case <-poll.C:
		}
	}
}

func waitForHungFrameExtractorExit(t *testing.T, pid int, timeout time.Duration) {
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
			t.Fatalf("hung ffmpeg process %d survived watchdog cancellation", pid)
		case <-poll.C:
		}
	}
}

// TestFFmpegFrameExtractorBoundHelper continuously publishes valid numbered
// JPEGs after announcing a leader and descendant. It models an ffmpeg that is
// making observable frame progress but does not terminate on its own.
// A run that keeps writing frames has no total deadline (#806): upstream
// ExtractVideoImagesOnIntervalInternal stops only an unresponsive process.
func TestFFmpegFrameExtractorHasNoTotalDeadlineWhileFramesProgress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process groups are unavailable on Windows")
	}
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	production, err := NewFFmpegFrameExtractor("ffmpeg", nil)
	if err != nil {
		t.Fatalf("NewFFmpegFrameExtractor(): %v", err)
	}
	if production.totalTimeout != 0 {
		t.Fatalf("production total timeout = %s, want none", production.totalTimeout)
	}

	fixture := newFrameExtractorBoundFixture(t, "steady")
	// The helper writes for about five seconds; the idle timeout is shorter
	// but covers helper startup, so a total deadline equal to it would stop
	// the run.
	extractor, err := newFFmpegFrameExtractor(fixture.path, 3*time.Second)
	if err != nil {
		t.Fatalf("newFFmpegFrameExtractor(): %v", err)
	}
	runtimeTicks := int64(50 * 10_000_000)
	source := openTrickplayTestSource(t).File()
	result := make(chan error, 1)
	go func() {
		extraction, err := extractor.Extract(t.Context(), FrameRequest{
			Source: source, Output: fixture.output, Width: 320,
			IntervalMilliseconds: 1_000, RunTimeTicks: &runtimeTicks,
		})
		if err == nil && len(extraction.Names) != 50 {
			err = fmt.Errorf("extracted %d frames, want 50", len(extraction.Names))
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Extract(steady ffmpeg) = %v, want success", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Extract(steady ffmpeg) did not return")
	}
	fixture.waitForExit(t)
}

func TestFFmpegFrameExtractorBoundHelper(t *testing.T) {
	if os.Getenv("GO_WANT_FRAME_EXTRACTOR_BOUND_HELPER") != "1" {
		return
	}
	args := os.Args[1:]
	for index, arg := range args {
		if arg == "--" {
			args = args[index+1:]
			break
		}
	}
	if len(args) == 0 {
		_, _ = os.Stderr.WriteString("frame bound helper received no ffmpeg arguments")
		os.Exit(125)
	}
	child := exec.Command("sleep", "30")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(125)
	}
	pids := strings.Join([]string{strconv.Itoa(os.Getpid()), strconv.Itoa(child.Process.Pid)}, "\n")
	if err := os.WriteFile(os.Getenv("FRAME_EXTRACTOR_BOUND_PIDS"), []byte(pids+"\n"), 0o600); err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(125)
	}
	pattern := args[len(args)-1]
	mode := os.Getenv("FRAME_EXTRACTOR_BOUND_MODE")
	writeFrame := func(index int) error {
		path := fmt.Sprintf(strings.ReplaceAll(pattern, "%08d", "%08d"), index)
		switch mode {
		case "byte-overflow":
			return writePaddedBoundedFrameJPEG(path, 128<<20)
		default:
			return writeBoundedFrameJPEG(path)
		}
	}
	if mode == "pathological-geometry" {
		if err := writePathologicalBoundedFrameJPEG(fmt.Sprintf(strings.ReplaceAll(pattern, "%08d", "%08d"), 1)); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(125)
		}
		if err := os.WriteFile(os.Getenv("FRAME_EXTRACTOR_BOUND_READY"), []byte("published pathological geometry"), 0o600); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(125)
		}
		return
	}
	if mode == "steady" {
		// Write frames for longer than the idle timeout, each well inside
		// one, then finish like a long video that keeps making progress.
		for index := 1; index <= 50; index++ {
			if err := writeFrame(index); err != nil {
				_, _ = os.Stderr.WriteString(err.Error())
				os.Exit(125)
			}
			if index == 1 {
				if err := os.WriteFile(os.Getenv("FRAME_EXTRACTOR_BOUND_READY"), []byte("progressing"), 0o600); err != nil {
					_, _ = os.Stderr.WriteString(err.Error())
					os.Exit(125)
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		return
	}
	if mode == "overproduce" {
		if err := os.WriteFile(os.Getenv("FRAME_EXTRACTOR_BOUND_READY"), []byte("starting overproduction"), 0o600); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(125)
		}
		for index := 1; index <= 3; index++ {
			if err := writeFrame(index); err != nil {
				_, _ = os.Stderr.WriteString(err.Error())
				os.Exit(125)
			}
		}
		return
	}
	for index := 1; ; index++ {
		if err := writeFrame(index); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(125)
		}
		if index == 1 {
			if err := os.WriteFile(os.Getenv("FRAME_EXTRACTOR_BOUND_READY"), []byte("progressing"), 0o600); err != nil {
				_, _ = os.Stderr.WriteString(err.Error())
				os.Exit(125)
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestFFmpegFrameExtractorHardDeadlineStopsContinuousFrameProgress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process groups are unavailable on Windows")
	}
	const (
		hardDeadline      = time.Second
		startupMargin     = 5 * time.Second
		deadlineTolerance = 300 * time.Millisecond
	)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	fixture := newFrameExtractorBoundFixture(t, "continuous")
	// Do not let the test-only total watchdog race helper startup under a
	// package-parallel suite. The helper's ready file proves it has published
	// continuous output; only then does the test arm the deadline it measures.
	extractor, err := newFFmpegFrameExtractor(fixture.path, 3*time.Second, startupMargin+hardDeadline)
	if err != nil {
		t.Fatalf("newFFmpegFrameExtractor(): %v", err)
	}
	runtimeTicks := int64(10_000_000_000)
	source := openTrickplayTestSource(t).File()
	deadline := newFrameExtractorArmedDeadlineContext(t.Context())
	defer deadline.cancel()
	result := make(chan error, 1)
	go func() {
		_, err := extractor.Extract(deadline, FrameRequest{
			Source: source, Output: fixture.output, Width: 320,
			IntervalMilliseconds: 1_000, RunTimeTicks: &runtimeTicks,
		})
		result <- err
	}()
	fixture.waitReady(t)
	fixture.assertFrameProgress(t)

	started := clock.NewReal().Now()
	deadline.arm(hardDeadline)
	err = fixture.waitResult(t, result)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Extract(continuous ffmpeg) = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > hardDeadline+deadlineTolerance {
		t.Fatalf("Extract(continuous ffmpeg) returned after %s, want no more than %s after its armed hard deadline", elapsed, hardDeadline+deadlineTolerance)
	}
	fixture.waitForExit(t)
}

func TestFFmpegFrameExtractorRejectsAndCleansOverproduction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process groups are unavailable on Windows")
	}
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	fixture := newFrameExtractorBoundFixture(t, "overproduce")
	extractor, err := newFFmpegFrameExtractor(fixture.path, 3*time.Second, 3*time.Second)
	if err != nil {
		t.Fatalf("newFFmpegFrameExtractor(): %v", err)
	}
	runtimeTicks := int64(20_000_000)
	source := openTrickplayTestSource(t).File()
	result := make(chan error, 1)
	go func() {
		_, err := extractor.Extract(context.Background(), FrameRequest{
			Source: source, Output: fixture.output, Width: 320,
			IntervalMilliseconds: 1_000, RunTimeTicks: &runtimeTicks,
		})
		result <- err
	}()
	fixture.waitReady(t)
	err = fixture.waitResult(t, result)
	if err == nil {
		t.Fatal("Extract(overproducing ffmpeg) succeeded after three frames exceeded the two-frame duration cap")
	}
	if !errors.Is(err, ErrFrameExtractionOutputTooLarge) {
		t.Fatalf("Extract(overproducing ffmpeg) = %v, want errors.Is(_, ErrFrameExtractionOutputTooLarge)", err)
	}
	if !strings.Contains(err.Error(), "more than 2 expected frames") {
		t.Fatalf("Extract(overproducing ffmpeg) = %v, want the two-frame overproduction detail", err)
	}
	fixture.waitForExit(t)
	entries, err := fixture.output.ReadNames()
	if err != nil {
		t.Fatalf("ReadNames(cleaned overproduction frames): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("overproducing extraction left artifacts = %#v, want caller-owned directory emptied", entries)
	}
}

func TestFFmpegFrameExtractorRejectsAndCleansAggregateByteOverflow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process groups are unavailable on Windows")
	}
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	fixture := newFrameExtractorBoundFixture(t, "byte-overflow")
	extractor, err := newFFmpegFrameExtractor(fixture.path, 3*time.Second, 3*time.Second)
	if err != nil {
		t.Fatalf("newFFmpegFrameExtractor(): %v", err)
	}
	// The helper emits 128 MiB sparse, valid JPEG files continuously. It stays
	// far under this 1,000-frame count cap, so only aggregate byte accounting
	// may terminate the run.
	runtimeTicks := int64(10_000_000_000)
	source := openTrickplayTestSource(t).File()
	result := make(chan error, 1)
	go func() {
		_, err := extractor.Extract(context.Background(), FrameRequest{
			Source: source, Output: fixture.output, Width: 320,
			IntervalMilliseconds: 1_000, RunTimeTicks: &runtimeTicks,
		})
		result <- err
	}()
	fixture.waitReady(t)
	err = fixture.waitResult(t, result)
	fixture.waitForExit(t)
	if !errors.Is(err, ErrFrameExtractionOutputBytesTooLarge) {
		t.Errorf("Extract(byte-overflow ffmpeg) = %v, want errors.Is(_, ErrFrameExtractionOutputBytesTooLarge)", err)
	}
	fixture.assertOutputEmpty(t)
}

func TestFFmpegFrameExtractorRejectsAndCleansPathologicalDecodedGeometry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process groups are unavailable on Windows")
	}
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	fixture := newFrameExtractorBoundFixture(t, "pathological-geometry")
	extractor, err := newFFmpegFrameExtractor(fixture.path, 3*time.Second, 3*time.Second)
	if err != nil {
		t.Fatalf("newFFmpegFrameExtractor(): %v", err)
	}
	source := openTrickplayTestSource(t).File()
	result := make(chan error, 1)
	go func() {
		_, err := extractor.Extract(context.Background(), FrameRequest{
			Source: source, Output: fixture.output, Width: 320,
			IntervalMilliseconds: 1_000,
		})
		result <- err
	}()
	fixture.waitReady(t)
	err = fixture.waitResult(t, result)
	fixture.waitForExit(t)
	if !errors.Is(err, ErrFrameExtractionFrameGeometryTooLarge) {
		t.Errorf("Extract(pathological decoded geometry) = %v, want errors.Is(_, ErrFrameExtractionFrameGeometryTooLarge)", err)
	}
	fixture.assertOutputEmpty(t)
}

type frameExtractorBoundFixture struct {
	path, frames, ready, pids string
	output                    *rootfs.Dir
}

func newFrameExtractorBoundFixture(t *testing.T, mode string) frameExtractorBoundFixture {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable(): %v", err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec \"$FRAME_EXTRACTOR_BOUND_HELPER\" -test.run '^TestFFmpegFrameExtractorBoundHelper$' -- \"$@\"\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(bound ffmpeg): %v", err)
	}
	frames := filepath.Join(root, "frames")
	fixture := frameExtractorBoundFixture{
		path: path, frames: frames, ready: filepath.Join(root, "ready"), pids: filepath.Join(root, "pids"),
		output: newPinnedFrameOutput(t, frames),
	}
	t.Setenv("GO_WANT_FRAME_EXTRACTOR_BOUND_HELPER", "1")
	t.Setenv("FRAME_EXTRACTOR_BOUND_HELPER", binary)
	t.Setenv("FRAME_EXTRACTOR_BOUND_MODE", mode)
	t.Setenv("FRAME_EXTRACTOR_BOUND_READY", fixture.ready)
	t.Setenv("FRAME_EXTRACTOR_BOUND_PIDS", fixture.pids)
	return fixture
}

func (fixture frameExtractorBoundFixture) waitReady(t *testing.T) {
	t.Helper()
	frameExtractorWaitForFile(t, fixture.ready, 5*time.Second)
	pids := waitForHungFrameExtractorPIDs(t, fixture.pids, 5*time.Second)
	t.Cleanup(func() {
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

func (fixture frameExtractorBoundFixture) assertFrameProgress(t *testing.T) {
	t.Helper()
	frameExtractorWaitForFrameCount(t, fixture.frames, 2, 5*time.Second)
}

func (fixture frameExtractorBoundFixture) waitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Extract() did not return by the strict frame-extraction test bound")
		return nil
	}
}

func (fixture frameExtractorBoundFixture) waitForExit(t *testing.T) {
	t.Helper()
	for _, pid := range waitForHungFrameExtractorPIDs(t, fixture.pids, 5*time.Second) {
		waitForHungFrameExtractorExit(t, pid, 5*time.Second)
	}
}

func (fixture frameExtractorBoundFixture) assertOutputEmpty(t *testing.T) {
	t.Helper()
	entries, err := fixture.output.ReadNames()
	if err != nil {
		t.Fatalf("ReadNames(cleaned frame outputs): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("bounded frame extraction left artifacts = %#v, want caller-owned directory emptied", entries)
	}
}

// frameExtractorArmedDeadlineContext postpones a caller deadline until the
// helper has proved the lifecycle state the test needs to observe.
type frameExtractorArmedDeadlineContext struct {
	context.Context
	done chan struct{}

	mu       sync.Mutex
	deadline time.Time
	timer    *time.Timer
	once     sync.Once
}

func newFrameExtractorArmedDeadlineContext(parent context.Context) *frameExtractorArmedDeadlineContext {
	return &frameExtractorArmedDeadlineContext{Context: parent, done: make(chan struct{})}
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
	ctx.deadline = clock.NewReal().Now().Add(timeout)
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

func writeBoundedFrameJPEG(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	encodeErr := jpeg.Encode(file, image.NewRGBA(image.Rect(0, 0, 320, 240)), &jpeg.Options{Quality: 80})
	closeErr := file.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

func writePaddedBoundedFrameJPEG(path string, size int64) error {
	if err := writeBoundedFrameJPEG(path); err != nil {
		return err
	}
	return os.Truncate(path, size)
}

func writePathologicalBoundedFrameJPEG(path string) error {
	if err := writeBoundedFrameJPEG(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	marker := bytes.Index(data, []byte{0xff, 0xc0})
	if marker < 0 || marker+8 >= len(data) {
		return errors.New("baseline JPEG frame header not found")
	}
	// DecodeConfig trusts SOF dimensions and does not need pixel data. Retain
	// the valid 320-pixel width while declaring a pathological 65,535-pixel
	// height without allocating or encoding that image.
	data[marker+5] = 0xff
	data[marker+6] = 0xff
	return os.WriteFile(path, data, 0o600)
}

func newPinnedFrameOutput(t *testing.T, path string) *rootfs.Dir {
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

func frameExtractorWaitForFrameCount(t *testing.T, directory string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		entries, err := os.ReadDir(directory)
		if err == nil && len(entries) >= want {
			return
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ReadDir(%q): %v", directory, err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("frame output never reached %d files in %q", want, directory)
		case <-poll.C:
		}
	}
}
