// SPDX-License-Identifier: GPL-2.0-only

package trickplay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/rootfs"
	"golang.org/x/sys/unix"
)

const (
	maxFrameExtractorDiagnostics     = 32 << 10
	maxFrameExtractorJPEGHeaderBytes = 256 << 10
	defaultFrameExtractionTimeout    = 2 * time.Minute
	MaxFrameExtractionJPEGBytes      = 256 << 20
	MaxFrameExtractionFrameWidth     = 8192
	MaxFrameExtractionFrameHeight    = 8192
	MaxFrameExtractionFramePixels    = 64 << 20
)

var (
	// ErrFrameExtractionOutputTooLarge reports an ffmpeg run that exceeded the
	// runtime-derived frame count bound.
	ErrFrameExtractionOutputTooLarge = errors.New("trickplay: frame extraction output exceeds expected count")
	// ErrFrameExtractionOutputBytesTooLarge reports an ffmpeg run whose JPEG
	// outputs exceeded the aggregate byte bound.
	ErrFrameExtractionOutputBytesTooLarge = errors.New("trickplay: frame extraction JPEG output exceeds byte limit")
	// ErrFrameExtractionFrameGeometryTooLarge reports a JPEG whose decoded
	// dimensions exceed the safe frame geometry bounds.
	ErrFrameExtractionFrameGeometryTooLarge = errors.New("trickplay: extracted frame geometry exceeds limit")
)

// Acceleration returns the hardware facts of one item's extraction:
// TrickplayOptions.EnableHwAcceleration and EnableHwEncoding, the
// encoding.xml selection, the probed capabilities, and the video stream.
// A zero AllowHardwareAcceleration selects the software extraction.
type Acceleration func(context.Context, dto.UUID) (encoding.TrickplayExtraction, encoding.Capabilities, error)

// FFmpegFrameExtractor extracts one JPEG at each requested sampling interval
// with ffmpeg's MJPEG encoder. Acceleration, when set, selects hardware
// decoding, filtering, and encoding for each item.
type FFmpegFrameExtractor struct {
	path         string
	acceleration Acceleration
	selfExecPath string
	// idleTimeout stops a run that writes no new frame for that long.
	idleTimeout time.Duration
	// totalTimeout, when positive, also bounds the whole run. Production
	// extractors leave it zero: upstream ExtractVideoImagesOnIntervalInternal
	// (MediaEncoder.cs) stops only an unresponsive process, so a long video
	// gets as long as it needs while frames keep coming.
	totalTimeout time.Duration
}

// NewFFmpegFrameExtractor validates the executable selection. The executable is
// intentionally started only by Extract so caller cancellation owns the run.
// A run stops when it writes no new frame for two minutes; it has no total
// deadline. A nil acceleration always extracts in software.
func NewFFmpegFrameExtractor(path string, acceleration Acceleration) (*FFmpegFrameExtractor, error) {
	extractor, err := newFFmpegFrameExtractor(path, defaultFrameExtractionTimeout)
	if err != nil {
		return nil, err
	}
	extractor.acceleration = acceleration
	return extractor, nil
}

func newFFmpegFrameExtractor(path string, idleTimeout time.Duration, totalTimeouts ...time.Duration) (*FFmpegFrameExtractor, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("ffmpeg path is empty")
	}
	if idleTimeout <= 0 {
		return nil, errors.New("ffmpeg frame extraction timeout must be positive")
	}
	if len(totalTimeouts) > 1 {
		return nil, errors.New("ffmpeg frame extractor accepts at most one total timeout")
	}
	var totalTimeout time.Duration
	if len(totalTimeouts) == 1 {
		totalTimeout = totalTimeouts[0]
		if totalTimeout <= 0 {
			return nil, errors.New("ffmpeg frame extraction total timeout must be positive")
		}
	}
	selfExecPath, err := frameSinkSelfExecPath()
	if err != nil {
		return nil, fmt.Errorf("resolve frame sink trampoline executable: %w", err)
	}
	return &FFmpegFrameExtractor{path: path, selfExecPath: selfExecPath, idleTimeout: idleTimeout, totalTimeout: totalTimeout}, nil
}

// Extract runs one software-only ffmpeg process and returns lexically ordered
// frame names. Every JPEG header is validated without decoding pixel data, so
// callers receive the actual scaled height and resource-safe output.
func (extractor *FFmpegFrameExtractor) Extract(ctx context.Context, request FrameRequest) (extraction FrameExtraction, returnErr error) {
	if extractor == nil || strings.TrimSpace(extractor.path) == "" {
		return FrameExtraction{}, errors.New("ffmpeg frame extractor is not configured")
	}
	if ctx == nil {
		return FrameExtraction{}, errors.New("frame extraction context is nil")
	}
	if err := ctx.Err(); err != nil {
		return FrameExtraction{}, err
	}
	if request.Source == nil {
		return FrameExtraction{}, errors.New("frame extraction source is nil")
	}
	if request.Output == nil {
		return FrameExtraction{}, errors.New("frame extraction output is nil")
	}
	if request.Width <= 0 {
		return FrameExtraction{}, errors.New("frame extraction width must be positive")
	}
	if request.IntervalMilliseconds <= 0 {
		return FrameExtraction{}, errors.New("frame extraction interval must be positive")
	}
	maximumFrames, limitFrames, err := maximumExpectedFrameCount(request.RunTimeTicks, request.IntervalMilliseconds)
	if err != nil {
		return FrameExtraction{}, err
	}
	if err := prepareFrameOutput(request.Output); err != nil {
		return FrameExtraction{}, err
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		if err := removeFrameOutputs(request.Output); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove partial frame outputs: %w", err))
		}
	}()

	var job encoding.TrickplayExtraction
	var caps encoding.Capabilities
	if extractor.acceleration != nil {
		job, caps, err = extractor.acceleration(ctx, request.ItemID)
		if err != nil {
			return FrameExtraction{}, fmt.Errorf("resolve trickplay hardware acceleration: %w", err)
		}
	}
	job.InputPath = fmt.Sprintf("/dev/fd/%d", sourceInputFD)
	job.OutputPattern = "%08d.jpg"
	job.MaxWidth = request.Width
	job.IntervalMilliseconds = request.IntervalMilliseconds
	job.ReferenceFrameRate = 0
	if hasExplicitReferenceFrameRate(request.ReferenceFrameRate) {
		job.ReferenceFrameRate = float64(*request.ReferenceFrameRate)
	}
	job.Threads, job.QualityScale = 1, 4
	args, err := encoding.BuildTrickplayArgs(job, caps)
	if err != nil {
		return FrameExtraction{}, err
	}
	var diagnostics boundedDiagnosticBuffer
	diagnostics.limit = maxFrameExtractorDiagnostics
	watchContext, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	ffmpegPath, err := exec.LookPath(extractor.path)
	if err != nil {
		return FrameExtraction{}, fmt.Errorf("start ffmpeg frame extraction: %w", err)
	}
	if strings.Contains(ffmpegPath, "/") && !filepath.IsAbs(ffmpegPath) {
		ffmpegPath, err = filepath.Abs(ffmpegPath)
		if err != nil {
			return FrameExtraction{}, fmt.Errorf("absolutize ffmpeg frame extraction path: %w", err)
		}
	}
	sink, err := request.Output.ProcessFile()
	if err != nil {
		return FrameExtraction{}, fmt.Errorf("duplicate frame extraction output: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, sink.Close()) }()
	outputIdentity, err := request.Output.Stat()
	if err != nil {
		return FrameExtraction{}, fmt.Errorf("stat frame extraction output: %w", err)
	}
	var sourceStatus unix.Stat_t
	if err := unix.Fstat(int(request.Source.Fd()), &sourceStatus); err != nil {
		return FrameExtraction{}, fmt.Errorf("stat frame extraction source: %w", err)
	}
	command := exec.CommandContext(watchContext, extractor.selfExecPath, append([]string{ffmpegPath}, args...)...)
	command.ExtraFiles = []*os.File{sink, request.Source}
	command.Env = append(os.Environ(),
		"DEADFIN_FRAME_SINK_FD=3",
		fmt.Sprintf("DEADFIN_FRAME_SINK_ID=%d:%d", outputIdentity.Dev, outputIdentity.Ino),
		fmt.Sprintf("DEADFIN_SOURCE_FD=%d", sourceInputFD),
		fmt.Sprintf("DEADFIN_SOURCE_ID=%d:%d", sourceStatus.Dev, sourceStatus.Ino),
	)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 100 * time.Millisecond
	command.Stdout = io.Discard
	command.Stderr = &diagnostics
	if err := command.Start(); err != nil {
		return FrameExtraction{}, fmt.Errorf("start ffmpeg frame extraction: %w", err)
	}
	processID := command.Process.Pid
	stopWatchdog := make(chan struct{})
	watchdogDone := make(chan struct{})
	idleTimedOut := make(chan struct{})
	totalTimedOut := make(chan struct{})
	limitExceeded := make(chan error, 1)
	go watchFrameExtraction(watchContext, cancelWatch, request.Output, extractor.idleTimeout, extractor.totalTimeout, maximumFrames, limitFrames, stopWatchdog, idleTimedOut, totalTimedOut, limitExceeded, watchdogDone)
	waitErr := command.Wait()
	close(stopWatchdog)
	<-watchdogDone
	// ffmpeg can leave demuxer helpers in its process group after its leader
	// exits. Reap the whole group on every terminal path, not just cancellation.
	reapErr := syscall.Kill(-processID, syscall.SIGKILL)
	if err := ctx.Err(); err != nil {
		return FrameExtraction{}, err
	}
	select {
	case err := <-limitExceeded:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return FrameExtraction{}, ctxErr
		}
		return FrameExtraction{}, err
	default:
	}
	select {
	case <-totalTimedOut:
		return FrameExtraction{}, context.DeadlineExceeded
	default:
	}
	select {
	case <-idleTimedOut:
		return FrameExtraction{}, context.DeadlineExceeded
	default:
	}
	if reapErr != nil && !errors.Is(reapErr, syscall.ESRCH) {
		return FrameExtraction{}, fmt.Errorf("reap ffmpeg frame extraction process group: %w", reapErr)
	}
	if waitErr != nil && !errors.Is(waitErr, exec.ErrWaitDelay) {
		if detail := strings.TrimSpace(diagnostics.String()); detail != "" {
			return FrameExtraction{}, fmt.Errorf("ffmpeg frame extraction: %w: %s", waitErr, detail)
		}
		return FrameExtraction{}, fmt.Errorf("ffmpeg frame extraction: %w", waitErr)
	}
	names, height, err := validateExtractedFrameOutputs(request.Output, request.Width)
	if err != nil {
		return FrameExtraction{}, err
	}
	if limitFrames && int64(len(names)) > maximumFrames {
		return FrameExtraction{}, fmt.Errorf("%w: produced more than %d expected frames", ErrFrameExtractionOutputTooLarge, maximumFrames)
	}
	completed = true
	return FrameExtraction{Names: names, Height: height}, nil
}

func hasExplicitReferenceFrameRate(rate *float32) bool {
	return rate != nil && *rate > 0 && *rate == *rate
}

func watchFrameExtraction(ctx context.Context, cancel context.CancelFunc, output *rootfs.Dir, idleTimeout, totalTimeout time.Duration, maximumFrames int64, limitFrames bool, stop <-chan struct{}, idleTimedOut, totalTimedOut chan struct{}, limitExceeded chan<- error, done chan<- struct{}) {
	defer close(done)
	interval := idleTimeout / 10
	if interval > 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	if limitFrames && interval > 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var totalDeadline <-chan time.Time
	if totalTimeout > 0 {
		timer := time.NewTimer(totalTimeout)
		defer timer.Stop()
		totalDeadline = timer.C
	}
	timeSource := clock.NewReal()
	lastProgress := timeSource.Now()
	lastCount := 0
	if scan, err := scanFrameOutputLimits(output); err == nil {
		lastCount = scan.count
		if violation := frameOutputLimitViolation(scan, maximumFrames, limitFrames); violation != nil {
			limitExceeded <- violation
			cancel()
			return
		}
	}
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-totalDeadline:
			close(totalTimedOut)
			cancel()
			return
		case now := <-ticker.C:
			scan, err := scanFrameOutputLimits(output)
			if err != nil {
				continue
			}
			if violation := frameOutputLimitViolation(scan, maximumFrames, limitFrames); violation != nil {
				limitExceeded <- violation
				cancel()
				return
			}
			if scan.count > lastCount {
				lastCount = scan.count
				lastProgress = now
				continue
			}
			if now.Sub(lastProgress) >= idleTimeout {
				close(idleTimedOut)
				cancel()
				return
			}
		}
	}
}

func maximumExpectedFrameCount(runtimeTicks *int64, intervalMilliseconds int64) (int64, bool, error) {
	if runtimeTicks == nil {
		return 0, false, nil
	}
	if *runtimeTicks < 0 {
		return 0, false, errors.New("frame extraction runtime must not be negative")
	}
	if *runtimeTicks == 0 {
		return 0, true, nil
	}
	maxInt64 := int64(^uint64(0) >> 1)
	if intervalMilliseconds > maxInt64/ticksPerMillisecond {
		return 1, true, nil
	}
	intervalTicks := intervalMilliseconds * ticksPerMillisecond
	count := *runtimeTicks / intervalTicks
	if *runtimeTicks%intervalTicks != 0 {
		count++
	}
	return count, true, nil
}

func removeFrameOutputs(output *rootfs.Dir) error {
	entries, err := output.ReadNames()
	if err != nil {
		return fmt.Errorf("read frame extraction directory: %w", err)
	}
	for _, entry := range entries {
		if err := output.RemoveAll(entry.Name()); err != nil {
			return fmt.Errorf("remove frame extraction output %q: %w", entry.Name(), err)
		}
	}
	return nil
}

type frameOutputLimits struct {
	count    int
	bytes    int64
	geometry error
}

func scanFrameOutputLimits(output *rootfs.Dir) (frameOutputLimits, error) {
	entries, err := output.ReadNames()
	if err != nil {
		return frameOutputLimits{}, err
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	var limits frameOutputLimits
	for _, entry := range entries {
		name := entry.Name()
		if !validFrameName(name) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := output.Lstat(name)
		if err != nil {
			return frameOutputLimits{}, err
		}
		if info.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		limits.count++
		if info.Size > MaxFrameExtractionJPEGBytes-limits.bytes {
			limits.bytes = MaxFrameExtractionJPEGBytes + 1
		} else {
			limits.bytes += info.Size
		}
		if limits.geometry != nil || info.Size == 0 {
			continue
		}
		config, err := decodeFrameJPEGConfig(output, name)
		if err != nil {
			// ffmpeg may still be writing this frame. Final validation rejects an
			// incomplete image after the process exits.
			continue
		}
		limits.geometry = validateFrameGeometry(name, config.Width, config.Height)
	}
	return limits, nil
}

func frameOutputLimitViolation(limits frameOutputLimits, maximumFrames int64, limitFrames bool) error {
	if limitFrames && int64(limits.count) > maximumFrames {
		return fmt.Errorf("%w: produced more than %d expected frames", ErrFrameExtractionOutputTooLarge, maximumFrames)
	}
	if limits.bytes > MaxFrameExtractionJPEGBytes {
		return fmt.Errorf("%w: produced %d bytes, limit %d", ErrFrameExtractionOutputBytesTooLarge, limits.bytes, MaxFrameExtractionJPEGBytes)
	}
	return limits.geometry
}

func prepareFrameOutput(output *rootfs.Dir) error {
	entries, err := output.ReadNames()
	if err != nil {
		return fmt.Errorf("read frame extraction directory: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("frame extraction directory is not empty")
	}
	return nil
}

func extractedFrameNames(output *rootfs.Dir) ([]string, error) {
	entries, err := output.ReadNames()
	if err != nil {
		return nil, fmt.Errorf("read extracted frames: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !validFrameName(name) {
			return nil, fmt.Errorf("unexpected frame extractor output %q", name)
		}
		info, err := output.Lstat(name)
		if err != nil {
			return nil, fmt.Errorf("stat extracted frame %q: %w", name, err)
		}
		if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Size == 0 {
			return nil, fmt.Errorf("extracted frame %q is not a nonempty regular file", name)
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errors.New("ffmpeg produced no frames")
	}
	sort.Strings(names)
	return names, nil
}

func validFrameName(name string) bool {
	if len(name) != len("00000000.jpg") || !strings.HasSuffix(name, ".jpg") {
		return false
	}
	for _, value := range name[:8] {
		if value < '0' || value > '9' {
			return false
		}
	}
	return true
}

func validateExtractedFrameOutputs(output *rootfs.Dir, width int) ([]string, int, error) {
	names, err := extractedFrameNames(output)
	if err != nil {
		return nil, 0, err
	}
	var totalBytes int64
	height := 0
	for index, name := range names {
		info, err := output.Lstat(name)
		if err != nil {
			return nil, 0, fmt.Errorf("stat extracted frame %q: %w", name, err)
		}
		if info.Size > MaxFrameExtractionJPEGBytes-totalBytes {
			return nil, 0, fmt.Errorf("%w: produced more than %d bytes", ErrFrameExtractionOutputBytesTooLarge, MaxFrameExtractionJPEGBytes)
		}
		totalBytes += info.Size
		config, err := decodeFrameJPEGConfig(output, name)
		if err != nil {
			return nil, 0, fmt.Errorf("decode extracted frame %q: %w", name, err)
		}
		if err := validateFrameGeometry(name, config.Width, config.Height); err != nil {
			return nil, 0, err
		}
		if config.Width != width {
			return nil, 0, fmt.Errorf("extracted frame width = %d, want %d", config.Width, width)
		}
		if index == 0 {
			height = config.Height
		}
	}
	return names, height, nil
}

func decodeFrameJPEGConfig(output *rootfs.Dir, name string) (config image.Config, returnErr error) {
	file, err := output.OpenFile(name)
	if err != nil {
		return image.Config{}, fmt.Errorf("open extracted frame %q: %w", name, err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	config, returnErr = jpeg.DecodeConfig(io.LimitReader(file, maxFrameExtractorJPEGHeaderBytes))
	return config, returnErr
}

func validateFrameGeometry(name string, width, height int) error {
	if width <= 0 || height <= 0 || width > MaxFrameExtractionFrameWidth || height > MaxFrameExtractionFrameHeight || uint64(width)*uint64(height) > MaxFrameExtractionFramePixels {
		return fmt.Errorf("%w: frame %q is %dx%d, maximum %dx%d and %d pixels", ErrFrameExtractionFrameGeometryTooLarge, name, width, height, MaxFrameExtractionFrameWidth, MaxFrameExtractionFrameHeight, MaxFrameExtractionFramePixels)
	}
	return nil
}

type boundedDiagnosticBuffer struct {
	bytes.Buffer
	limit int
}

func (buffer *boundedDiagnosticBuffer) Write(value []byte) (int, error) {
	written := len(value)
	if buffer.limit <= buffer.Buffer.Len() {
		return written, nil
	}
	remaining := buffer.limit - buffer.Buffer.Len()
	if len(value) > remaining {
		value = value[:remaining]
	}
	_, _ = buffer.Buffer.Write(value)
	return written, nil
}
