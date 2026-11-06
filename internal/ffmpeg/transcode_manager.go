// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
	"golang.org/x/sys/unix"
)

const (
	maxTranscodeStderr   = 1 << 20
	maxFailureDiagnostic = 64 << 10
	// maxFailureLogTail bounds the ffmpeg log tail copied into a failure
	// record: enough for the reason, small enough for one log line.
	maxFailureLogTail = 4 << 10

	// defaultCacheBytesPerSession caps the HLS transcode-cache family at
	// 1 GiB of apparent on-disk bytes. Zero values in TranscodeManagerOptions
	// fall back to this limit.
	defaultCacheBytesPerSession int64 = 1 << 30

	// defaultCacheFreeBytesFloor pauses write-ahead production when the
	// transcodes filesystem drops below 1 GiB of available space.
	defaultCacheFreeBytesFloor int64 = 1 << 30
)

var (
	// ErrManagerClosed is returned when a request is opened after shutdown begins.
	ErrManagerClosed = errors.New("ffmpeg transcode manager is closed")
	// ErrOutputStalled is returned when a growing output makes no progress for the configured limit.
	ErrOutputStalled = errors.New("ffmpeg progressive output stalled")
	// ErrTranscodeExited is returned when ffmpeg exits successfully before producing its output.
	ErrTranscodeExited = errors.New("ffmpeg exited before producing progressive output")
)

// TranscodeManagerOptions controls progressive lifecycle timings. Zero values use
// production defaults; Clock is injectable so timing behavior is deterministic in tests.
type TranscodeManagerOptions struct {
	Clock                  clock.Clock
	OutputPollInterval     time.Duration
	ReaderRetryInterval    time.Duration
	ReaderStallTimeout     time.Duration
	ProgressiveIdleTimeout time.Duration
	KillGracePeriod        time.Duration
	LogDirectory           string
	MaxLogBytes            int64
	ThrottleThreshold      time.Duration
	SegmentRetention       time.Duration
	PKeySupported          bool
	CacheBytesPerSession   int64
	CacheFreeBytesFloor    int64
	// Logger receives one error record when ffmpeg fails to start or exits
	// with an error that the manager did not cause. Nil discards the records.
	Logger *slog.Logger
	// RemoteRunner is the transcode-worker seam (#614). Nil keeps
	// every job local. HLS families move, except a burn-in that reads the
	// media file itself; a RemoteProgressiveRunner also takes progressive
	// transcodes (#624).
	RemoteRunner RemoteRunner
	// KeepFamilies are the remote families that kept running on their
	// hosts across a restart (#618). The startup wipe keeps their files, so
	// the families can be adopted (AdoptHLS).
	KeepFamilies []KeptFamily
	// KeepTranscodes skips the startup wipe: a live server replica may
	// share the directory (#652).
	KeepTranscodes bool
}

// ProgressiveRequest identifies one progressive ffmpeg invocation. Args excludes
// the executable: it is passed directly to the manager's configured ffmpeg path.
type ProgressiveRequest struct {
	OutputPath     string
	Args           []string
	PlaySessionID  string
	DeviceID       string
	SourceRevision string
	OpenSource     func() (*sourcefile.Source, error)
}

// TranscodeManager owns progressive ffmpeg processes and their temporary output.
type TranscodeManager struct {
	logDirectory string
	mu           sync.Mutex

	ffmpegPath            string
	directory             string
	hlsSessionEpoch       map[string]uint64
	hlsSessionTransitions map[string]int
	requestedDirectory    string
	options               TranscodeManagerOptions
	jobs                  map[string]*transcodeJob
	hlsLocks              map[string]*sync.Mutex
	hlsLockUsers          map[string]int
	hlsSessions           map[string]map[*transcodeJob]struct{}
	closed                bool

	workers     sync.WaitGroup
	workerCount int
	// handoffCtx bounds the handoff restarts; Close cancels it.
	handoffCtx    context.Context
	handoffCancel context.CancelFunc
	shutdownDone  chan struct{}
	shutdownOnce  sync.Once
	shutdownErr   error

	beforeIdleStop           func()
	beforeOutputMutation     func()
	beforeStderrOpen         func()
	reapProcessGroup         func(int) error
	removeProgressiveOutput  func(string) error
	beforeHLSRestartStart    func()
	beforeHLSRestartRegister func()
	beforeHLSWaitRetain      func()
	beforeHLSStartRegister   func()
	beforeCleanupAttemptDone func()
	hlsLogOpen               func(*transcodeJob) (*os.File, error)
	stdinPipe                func(*exec.Cmd) (io.WriteCloser, error)
	startCommand             func(*exec.Cmd) error
	afterCleanupEnumeration  func() // test hook: fires after ReadDir, before the delete loop, so a test can make an enumerated output live and observe the re-check
	beforeOutputRead         func()
	beforeDiagnosticRead     func()
}

type transcodeState uint8

const (
	transcodeStarting transcodeState = iota
	transcodeRunning
	transcodeTerminating
	transcodeExited
)

type transcodeJob struct {
	manager *TranscodeManager
	output  string
	args    []string

	startDone chan struct{}
	waitDone  chan struct{}
	// startCtx ends when every HLS request that waits for the start gives
	// up, so a job queued for a transcode worker leaves the queue (#616).
	startCtx     context.Context
	startCancel  context.CancelFunc
	startWaiters int
	startupErr   error
	waitErr      error
	state        transcodeState
	leaderExited bool

	proc   process
	stdin  io.WriteCloser
	stderr *os.File
	// logPath names the ffmpeg log file for failure records.
	logPath string

	requests    int
	idleTimer   clock.Timer
	idleCancel  chan struct{}
	idleTimeout time.Duration
	stopOnce    sync.Once
	reapPending bool
	reapProc    process
	// runnerLost records a remote host that vanished mid-stream.
	runnerLost bool
	// released records a remote family that its host keeps running across
	// the server restart (#618); its artifacts stay.
	released              bool
	reapSucceeded         bool
	reapDone              chan struct{}
	reapAttempt           chan struct{}
	cleanupPending        bool
	cleanupAttempt        chan struct{}
	cleanupSucceeded      bool
	cleanupErr            error
	reapErr               error
	sidecarCancel         context.CancelFunc
	throttler             *transcodingThrottler
	sidecarStderr         io.Closer
	sidecarSource         io.Closer
	sidecarPumpDone       <-chan struct{}
	sidecarLoggerDone     <-chan struct{}
	positionTicks         int64
	bytesTranscoded       int64
	hasBytesTranscoded    bool
	downloadPositionTicks int64
	bytesDownloaded       int64

	hls *hlsJob
	// playSessionID is the progressive request's PlaySessionId. HLS jobs
	// keep theirs on hls.
	playSessionID  string
	sourceRevision string
	openSource     func() (*sourcefile.Source, error)
	source         *sourcefile.Source
	sourceAux      *os.File
	subtitle       *sourcefile.Source
	// fonts are the font files a remote burn-in borrows (#624).
	fonts []*os.File
}

// NewTranscodeManager creates a manager and clears exactly its owned temporary
// transcodes directory before accepting requests.
func NewTranscodeManager(ffmpegPath, transcodesDir string, options TranscodeManagerOptions) (*TranscodeManager, error) {
	if ffmpegPath == "" {
		return nil, errors.New("ffmpeg path is empty")
	}
	if transcodesDir == "" {
		return nil, errors.New("transcodes directory is empty")
	}

	directory, err := filepath.Abs(transcodesDir)
	if err != nil {
		return nil, err
	}
	if directory == filepath.VolumeName(directory)+string(filepath.Separator) {
		return nil, fmt.Errorf("refusing to own filesystem root as transcodes directory")
	}
	options = progressiveDefaults(options)
	if err := validateProgressiveOptions(options); err != nil {
		return nil, err
	}
	if !options.KeepTranscodes {
		if err := clearTranscodes(directory, options.KeepFamilies); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	canonicalDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, fmt.Errorf("canonicalize transcodes directory: %w", err)
	}
	logDirectory := canonicalDirectory
	if options.LogDirectory != "" {
		requestedLogDirectory, logErr := filepath.Abs(options.LogDirectory)
		if logErr != nil {
			return nil, logErr
		}
		if err := os.MkdirAll(requestedLogDirectory, 0o755); err != nil {
			return nil, err
		}
		logDirectory, logErr = filepath.EvalSymlinks(requestedLogDirectory)
		if logErr != nil {
			return nil, fmt.Errorf("canonicalize ffmpeg log directory: %w", logErr)
		}
	}
	handoffCtx, handoffCancel := context.WithCancel(context.Background())
	return &TranscodeManager{
		handoffCtx:            handoffCtx,
		handoffCancel:         handoffCancel,
		ffmpegPath:            ffmpegPath,
		directory:             canonicalDirectory,
		requestedDirectory:    directory,
		logDirectory:          logDirectory,
		options:               options,
		jobs:                  make(map[string]*transcodeJob),
		hlsLocks:              make(map[string]*sync.Mutex),
		hlsSessionEpoch:       make(map[string]uint64),
		hlsSessionTransitions: make(map[string]int),
		hlsLockUsers:          make(map[string]int),
		hlsSessions:           make(map[string]map[*transcodeJob]struct{}),
		shutdownDone:          make(chan struct{}),
	}, nil
}

func progressiveDefaults(options TranscodeManagerOptions) TranscodeManagerOptions {
	if options.Clock == nil {
		options.Clock = clock.NewReal()
	}
	if options.OutputPollInterval == 0 {
		options.OutputPollInterval = 100 * time.Millisecond
	}
	if options.ReaderRetryInterval == 0 {
		options.ReaderRetryInterval = 50 * time.Millisecond
	}
	if options.ReaderStallTimeout == 0 {
		options.ReaderStallTimeout = 30 * time.Second
	}
	if options.ProgressiveIdleTimeout == 0 {
		options.ProgressiveIdleTimeout = 10 * time.Second
	}
	if options.KillGracePeriod == 0 {
		options.KillGracePeriod = 5 * time.Second
	}
	if options.MaxLogBytes == 0 {
		options.MaxLogBytes = maxTranscodeStderr
	}
	if options.ThrottleThreshold == 0 {
		options.ThrottleThreshold = time.Minute
	}
	if options.SegmentRetention == 0 {
		options.SegmentRetention = 20 * time.Second
	}
	if options.CacheBytesPerSession == 0 {
		options.CacheBytesPerSession = defaultCacheBytesPerSession
	}
	if options.CacheFreeBytesFloor == 0 {
		options.CacheFreeBytesFloor = defaultCacheFreeBytesFloor
	}
	return options
}

func validateProgressiveOptions(options TranscodeManagerOptions) error {
	if options.OutputPollInterval < 0 || options.ReaderRetryInterval < 0 || options.ReaderStallTimeout < 0 || options.ProgressiveIdleTimeout < 0 || options.KillGracePeriod < 0 || options.MaxLogBytes < 0 || options.ThrottleThreshold < 0 || options.SegmentRetention < 0 {
		return errors.New("ffmpeg durations and log limit must be positive")
	}
	if options.CacheBytesPerSession < 0 || options.CacheFreeBytesFloor < 0 {
		return errors.New("ffmpeg cache budget and free-space floor must be positive")
	}
	return nil
}

// OpenProgressive opens a reader over a growing ffmpeg output. Its context owns
// only this request and reader; it never directly terminates a shared job.
func (manager *TranscodeManager) OpenProgressive(ctx context.Context, request ProgressiveRequest) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	output, args, err := manager.validateRequest(request)
	if err != nil {
		return nil, err
	}

	for {
		manager.mu.Lock()
		if manager.closed {
			manager.mu.Unlock()
			return nil, ErrManagerClosed
		}
		job, exists := manager.jobs[output]
		if exists && (!reflect.DeepEqual(job.args, args) || job.sourceRevision != request.SourceRevision) {
			manager.mu.Unlock()
			return nil, fmt.Errorf("conflicting ffmpeg argv or source revision for %q", output)
		}
		if exists && job.cleanupPending {
			cleanupErr := job.cleanupErr
			attempt := job.cleanupAttempt
			manager.mu.Unlock()
			if cleanupErr != nil {
				return nil, cleanupErr
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-attempt:
				continue
			}
		}
		if exists && job.reapPending {
			reapDone := job.reapDone
			manager.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-reapDone:
				continue
			}
		}
		if exists && (job.state == transcodeTerminating || job.leaderExited) {
			waitDone := job.waitDone
			manager.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-waitDone:
				continue
			}
		}

		leader := !exists
		if leader {
			job = &transcodeJob{
				manager:        manager,
				output:         output,
				args:           append([]string(nil), args...),
				startDone:      make(chan struct{}),
				waitDone:       make(chan struct{}),
				requests:       1,
				state:          transcodeStarting,
				playSessionID:  request.PlaySessionID,
				sourceRevision: request.SourceRevision,
				openSource:     request.OpenSource,
			}
			manager.addWorkerLocked()
			manager.jobs[output] = job
		} else {
			job.requests++
			manager.cancelIdleLocked(job)
		}
		manager.mu.Unlock()

		if leader {
			go manager.start(job)
		}
		select {
		case <-ctx.Done():
			manager.release(job)
			return nil, ctx.Err()
		case <-job.startDone:
		}
		if startupErr := job.startError(); startupErr != nil {
			manager.release(job)
			return nil, startupErr
		}

		file, err := manager.waitForOutput(ctx, job)
		if err != nil {
			manager.release(job)
			return nil, err
		}
		return &progressiveReader{context: ctx, job: job, file: file, done: make(chan struct{})}, nil
	}
}

func (manager *TranscodeManager) validateRequest(request ProgressiveRequest) (string, []string, error) {
	if request.OutputPath == "" {
		return "", nil, errors.New("progressive output path is empty")
	}
	if len(request.Args) == 0 {
		return "", nil, errors.New("ffmpeg argv is empty")
	}
	if request.SourceRevision == "" {
		return "", nil, errors.New("progressive source revision is empty")
	}
	if request.OpenSource == nil {
		return "", nil, errors.New("progressive source opener is required")
	}

	output, err := manager.normalizeOutputPath(request.OutputPath)
	if err != nil {
		return "", nil, err
	}
	args := append([]string(nil), request.Args...)
	argvOutput, err := manager.normalizeOutputPath(args[len(args)-1])
	if err != nil || argvOutput != output {
		return "", nil, errors.New("ffmpeg argv must end with the progressive output path")
	}
	args[len(args)-1] = output
	return output, args, nil
}

func (manager *TranscodeManager) normalizeOutputPath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absPath = filepath.Clean(absPath)
	for index, root := range []string{manager.requestedDirectory, manager.directory} {
		relative, relErr := filepath.Rel(root, absPath)
		if relErr != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if index == 0 {
			resolvedRoot, resolveErr := filepath.EvalSymlinks(root)
			if resolveErr != nil || filepath.Clean(resolvedRoot) != manager.directory {
				return "", fmt.Errorf("transcodes directory alias no longer resolves to its owned directory")
			}
		}
		return filepath.Join(manager.directory, relative), nil
	}
	return "", fmt.Errorf("progressive output %q is outside transcodes directory", path)
}

func (manager *TranscodeManager) start(job *transcodeJob) {
	defer manager.doneWorker()

	if err := manager.prepareTranscodeSource(job); err != nil {
		manager.finishStart(job, err)
		return
	}
	if err := manager.prepareArtifacts(job); err != nil {
		manager.finishStart(job, fmt.Errorf("prepare ffmpeg artifacts %q: %w", job.output, err))
		return
	}

	args, extraFiles, err := manager.commandArgs(job)
	if err != nil {
		manager.failStartAfterPrepare(job, err)
		return
	}
	if manager.startRemote(job) || manager.startRemoteProgressive(job) {
		return
	}
	command := exec.Command(manager.ffmpegPath, args...)
	command.ExtraFiles = extraFiles
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if job.hls != nil {
		command.WaitDelay = manager.options.OutputPollInterval
	}
	stderr, err := manager.openProgressiveStderr(job.output)
	if err != nil {
		manager.failStartAfterPrepare(job, fmt.Errorf("open ffmpeg stderr log: %w", err))
		return
	}
	var sidecarReader *io.PipeReader
	var sidecarWriter *io.PipeWriter
	var sidecarSource *os.File
	var sidecarDestination *os.File
	var sidecarLog *os.File
	if job.hls != nil {
		if manager.hlsLogOpen != nil {
			sidecarLog, err = manager.hlsLogOpen(job)
		} else {
			sidecarLog, err = manager.openHLSLog(job)
		}
		if err != nil {
			_ = stderr.Close()
			manager.failStartAfterPrepare(job, fmt.Errorf("open HLS sidecar log: %w", err))
			return
		}
		sidecarReader, sidecarWriter = io.Pipe()
		sidecarSource, sidecarDestination, err = os.Pipe()
		if err != nil {
			closeHLSStartResources(sidecarReader, sidecarWriter, sidecarLog)
			_ = stderr.Close()
			manager.failStartAfterPrepare(job, fmt.Errorf("open HLS stderr pipe: %w", err))
			return
		}
		command.Stderr = sidecarDestination
	} else {
		command.Stderr = &limitedWriter{writer: stderr, remaining: maxTranscodeStderr}
	}
	command.Stdout = io.Discard

	manager.mu.Lock()
	closed := manager.closed
	manager.mu.Unlock()
	if closed {
		closeHLSStartResources(sidecarReader, sidecarWriter, sidecarLog)
		if sidecarSource != nil {
			_ = sidecarSource.Close()
		}
		if sidecarDestination != nil {
			_ = sidecarDestination.Close()
		}
		_ = stderr.Close()
		manager.failStartAfterPrepare(job, ErrManagerClosed)
		return
	}
	var stdin io.WriteCloser
	if manager.stdinPipe != nil {
		stdin, err = manager.stdinPipe(command)
	} else {
		stdin, err = command.StdinPipe()
	}
	if err != nil {
		closeHLSStartResources(sidecarReader, sidecarWriter, sidecarLog)
		if sidecarSource != nil {
			_ = sidecarSource.Close()
		}
		if sidecarDestination != nil {
			_ = sidecarDestination.Close()
		}
		_ = stderr.Close()
		manager.failStartAfterPrepare(job, fmt.Errorf("open ffmpeg stdin: %w", err))
		return
	}
	start := command.Start
	if manager.startCommand != nil {
		start = func() error { return manager.startCommand(command) }
	}
	if err := start(); err != nil {
		_ = stdin.Close()
		closeHLSStartResources(sidecarReader, sidecarWriter, sidecarLog)
		if sidecarSource != nil {
			_ = sidecarSource.Close()
		}
		if sidecarDestination != nil {
			_ = sidecarDestination.Close()
		}
		_ = stderr.Close()
		manager.logFailure(job, "ffmpeg failed to start", err)
		manager.failStartAfterPrepare(job, fmt.Errorf("start ffmpeg: %w", err))
		return
	}

	manager.mu.Lock()
	job.proc = localProcess{manager: manager, cmd: command, hls: job.hls != nil}
	job.stdin = stdin
	job.stderr = stderr
	job.logPath = stderr.Name()
	if sidecarLog != nil {
		job.logPath = sidecarLog.Name()
	}
	job.state = transcodeRunning
	if job.hls != nil {
		_ = sidecarDestination.Close()
		if job.sourceAux != nil {
			_ = job.sourceAux.Close()
			job.sourceAux = nil
		}
		if job.subtitle != nil {
			_ = job.subtitle.Close()
			job.subtitle = nil
		}
		job.sidecarStderr = sidecarWriter
		job.sidecarSource = sidecarSource
		manager.startHLSSidecarsLocked(job, sidecarReader, sidecarLog)
		manager.startHLSSidecarPumpLocked(job, sidecarSource, stderr, sidecarWriter)
	}
	manager.addWorkerLocked()
	close(job.startDone)
	if job.requests == 0 && !manager.closed {
		manager.armIdleLocked(job)
	}
	manager.mu.Unlock()
	go manager.wait(job)
}

func (manager *TranscodeManager) prepareTranscodeSource(job *transcodeJob) error {
	openSource := job.openSource
	if job.hls != nil {
		openSource = job.hls.openSource
	}
	if openSource == nil {
		return nil
	}
	source, err := openSource()
	if err != nil {
		return fmt.Errorf("open transcode source: %w", err)
	}
	if source == nil {
		return errors.New("open transcode source returned nil")
	}
	var aux *os.File
	var subtitle *sourcefile.Source
	if job.hls != nil && job.hls.subtitleSource != "" {
		aux, err = source.OpenAuxiliary()
		if err != nil {
			_ = source.Close()
			return fmt.Errorf("open HLS embedded subtitle source: %w", err)
		}
	}
	if job.hls != nil && job.hls.externalSubtitleSource != "" {
		subtitle, err = job.hls.openExternalSubtitle()
		if err != nil {
			_ = source.Close()
			return fmt.Errorf("open pinned external HLS subtitle source: %w", err)
		}
		if subtitle == nil {
			_ = source.Close()
			return errors.New("open external HLS subtitle source returned nil")
		}
	}

	manager.mu.Lock()
	if job.source != nil {
		manager.mu.Unlock()
		if aux != nil {
			_ = aux.Close()
		}
		if subtitle != nil {
			_ = subtitle.Close()
		}
		_ = source.Close()
		return errors.New("transcode source is already open")
	}
	job.source = source
	job.sourceAux = aux
	job.subtitle = subtitle
	manager.mu.Unlock()
	return nil
}

func (manager *TranscodeManager) commandArgs(job *transcodeJob) ([]string, []*os.File, error) {
	args := append([]string(nil), job.args...)
	if job.source == nil {
		return args, nil, nil
	}
	if err := job.source.VerifyUnchanged(); err != nil {
		return nil, nil, fmt.Errorf("verify transcode source before launch: %w", err)
	}
	if err := job.source.VerifyPathStillCurrent(); err != nil {
		return nil, nil, fmt.Errorf("verify transcode source path before launch: %w", err)
	}

	operand := "file:" + job.source.Path()
	found := 0
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "-i" && args[index+1] == operand {
			args[index+1] = sourcefile.FDPath(3)
			found++
		}
	}
	if found != 1 {
		return nil, nil, fmt.Errorf("ffmpeg argv must contain exactly one source operand %q", operand)
	}
	file := job.source.File()
	if file == nil {
		return nil, nil, errors.New("HLS source descriptor is unavailable")
	}
	if job.hls == nil || job.hls.subtitleSource == "" && job.hls.externalSubtitleSource == "" {
		return args, []*os.File{file}, nil
	}
	subtitleFile := job.sourceAux
	subtitleToken := job.hls.subtitleSource
	if job.hls.externalSubtitleSource != "" {
		if job.subtitle == nil {
			return nil, nil, errors.New("HLS external subtitle descriptor is unavailable")
		}
		if err := job.subtitle.VerifyUnchanged(); err != nil {
			return nil, nil, fmt.Errorf("verify external HLS subtitle before launch: %w", err)
		}
		if err := job.subtitle.VerifyPathStillCurrent(); err != nil {
			return nil, nil, fmt.Errorf("verify external HLS subtitle path before launch: %w", err)
		}
		subtitleFile = job.subtitle.File()
		subtitleToken = job.hls.externalSubtitleSource
	}
	if subtitleFile == nil {
		return nil, nil, errors.New("HLS subtitle descriptor is unavailable")
	}
	replaced := 0
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "-vf" && args[index] != "-filter_complex" {
			continue
		}
		replaced += strings.Count(args[index+1], subtitleToken)
		args[index+1] = strings.ReplaceAll(args[index+1], subtitleToken, "subtitles=f="+sourcefile.FDPath(4))
	}
	if replaced != 1 {
		return nil, nil, errors.New("HLS argv must contain exactly one subtitle source operand")
	}
	return args, []*os.File{file, subtitleFile}, nil
}

func (manager *TranscodeManager) closeJobSourceLocked(job *transcodeJob) {
	for _, font := range job.fonts {
		_ = font.Close()
	}
	job.fonts = nil
	if job.sourceAux != nil {
		_ = job.sourceAux.Close()
		job.sourceAux = nil
	}
	if job.subtitle != nil {
		_ = job.subtitle.Close()
		job.subtitle = nil
	}
	if job.source != nil {
		source := job.source
		job.source = nil
		_ = source.Close()
	}
}

func closeHLSStartResources(reader *io.PipeReader, writer *io.PipeWriter, log *os.File) {
	if writer != nil {
		_ = writer.Close()
	}
	if reader != nil {
		_ = reader.Close()
	}
	if log != nil {
		_ = log.Close()
	}
}

func (manager *TranscodeManager) openProgressiveStderr(output string) (*os.File, error) {
	if manager.beforeStderrOpen != nil {
		manager.beforeStderrOpen()
	}
	stderrPath := output + ".stderr.log"
	parent, name, err := manager.openOutputParent(stderrPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(parent) }()
	fd, err := unix.Openat(parent, name, unix.O_CREAT|unix.O_WRONLY|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open ffmpeg stderr log %q: %w", stderrPath, err)
	}
	return os.NewFile(uintptr(fd), stderrPath), nil
}

func (manager *TranscodeManager) prepareOutput(output string) error {
	if manager.beforeOutputMutation != nil {
		manager.beforeOutputMutation()
	}
	parent, name, err := manager.openOrCreateOutputParent(output)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(parent) }()
	var info unix.Stat_t
	err = unix.Fstatat(parent, name, &info, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect progressive output %q: %w", output, err)
	}
	if info.Mode&unix.S_IFMT == unix.S_IFDIR {
		return fmt.Errorf("progressive output %q is a directory", output)
	}
	if err := unix.Unlinkat(parent, name, 0); err != nil {
		return fmt.Errorf("remove stale progressive output %q: %w", output, err)
	}
	return nil
}

func (manager *TranscodeManager) openOutputParent(output string) (int, string, error) {
	return manager.openOutputParentWithCreate(output, false)
}

func (manager *TranscodeManager) openOrCreateOutputParent(output string) (int, string, error) {
	return manager.openOutputParentWithCreate(output, true)
}

func (manager *TranscodeManager) openOutputParentWithCreate(output string, create bool) (int, string, error) {
	relative, err := filepath.Rel(manager.directory, filepath.Dir(output))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return -1, "", fmt.Errorf("resolve progressive output parent %q", output)
	}
	parent, err := unix.Open(manager.directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("open transcodes directory: %w", err)
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "." || component == "" {
			continue
		}
		next, err := unix.Openat(parent, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err := unix.Mkdirat(parent, component, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
				_ = unix.Close(parent)
				return -1, "", fmt.Errorf("create progressive output parent %q: %w", component, err)
			}
			next, err = unix.Openat(parent, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			_ = unix.Close(parent)
			return -1, "", fmt.Errorf("open progressive output parent %q: %w", component, err)
		}
		_ = unix.Close(parent)
		parent = next
	}
	return parent, filepath.Base(output), nil
}

func (manager *TranscodeManager) openExistingRegular(path string) (*os.File, error) {
	parent, name, err := manager.openOutputParent(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(parent) }()
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("progressive file %q is not regular", path)
	}
	return os.NewFile(uintptr(fd), path), nil
}

func (manager *TranscodeManager) removeOutput(output string) error {
	if manager.removeProgressiveOutput != nil {
		return manager.removeProgressiveOutput(output)
	}
	parent, name, err := manager.openOutputParent(output)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(parent) }()
	if err := unix.Unlinkat(parent, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove progressive output %q: %w", output, err)
	}
	return nil
}

func (manager *TranscodeManager) prepareArtifacts(job *transcodeJob) error {
	if job.hls != nil {
		return manager.prepareHLSFamily(job.hls)
	}
	return manager.prepareOutput(job.output)
}

func (manager *TranscodeManager) removeArtifacts(job *transcodeJob) error {
	if job.hls != nil {
		return manager.removeHLSFamily(job.hls)
	}
	return manager.removeOutput(job.output)
}

func (manager *TranscodeManager) finishStart(job *transcodeJob, err error) {
	manager.mu.Lock()
	manager.closeJobSourceLocked(job)
	job.startupErr = err
	job.state = transcodeExited
	if job.startCancel != nil {
		job.startCancel()
	}
	if manager.jobs[job.output] == job {
		delete(manager.jobs, job.output)
	}
	manager.unregisterHLSSessionLocked(job)
	close(job.startDone)
	close(job.waitDone)
	manager.mu.Unlock()
}

func (manager *TranscodeManager) failStartAfterPrepare(job *transcodeJob, startupErr error) {
	cleanupErr := manager.removeArtifacts(job)
	if cleanupErr == nil {
		manager.finishStart(job, startupErr)
		return
	}
	manager.mu.Lock()
	manager.closeJobSourceLocked(job)
	job.startupErr = errors.Join(startupErr, cleanupErr)
	job.cleanupErr = cleanupErr
	job.cleanupPending = true
	job.state = transcodeExited
	close(job.startDone)
	close(job.waitDone)
	manager.shutdownErr = errors.Join(manager.shutdownErr, cleanupErr)
	manager.mu.Unlock()
}

func (manager *TranscodeManager) wait(job *transcodeJob) {
	err := job.proc.wait()

	manager.mu.Lock()
	terminating := job.state == transcodeTerminating
	lost := false
	if errors.Is(err, ErrRunnerLost) && job.hls != nil && !terminating {
		// The completed segments stay valid; the handoff or the next
		// segment request restarts the family on another host.
		job.runnerLost = true
		lost = true
		err = nil
	}
	if errors.Is(err, ErrRunnerReleased) && job.hls != nil {
		// The host keeps the family running for the next server process.
		job.released = true
		err = nil
		terminating = false
	}
	removeOutput := err != nil || terminating
	stderr := job.stderr
	stdin := job.stdin
	if job.hls == nil {
		job.stderr = nil
	}
	job.leaderExited = true
	job.stdin = nil
	proc := job.proc
	cleanupErr := job.cleanupErr
	manager.mu.Unlock()
	if err != nil && !terminating {
		manager.logFailure(job, "ffmpeg exited with an error", err)
	}
	if cleanupErr != nil {
		err = errors.Join(err, cleanupErr)
	}

	if stdin != nil {
		_ = stdin.Close()
	}
	if job.hls == nil && stderr != nil {
		_ = stderr.Close()
	}
	var reapErr error
	if groupErr := proc.reap(); groupErr != nil {
		reapErr = fmt.Errorf("reap ffmpeg process group: %w", groupErr)
		err = errors.Join(err, reapErr)
		removeOutput = true
	}
	if job.hls != nil && reapErr == nil {
		manager.drainHLSSidecars(job)
		if stderr != nil {
			_ = stderr.Close()
		}
		manager.mu.Lock()
		if job.stderr == stderr {
			job.stderr = nil
		}
		manager.mu.Unlock()
	}
	if job.hls != nil && reapErr != nil {
		manager.stopHLSSidecarsKeepSource(job)
	}
	if removeOutput && cleanupErr == nil && reapErr == nil {
		if removeErr := manager.removeArtifacts(job); removeErr != nil {
			cleanupErr = fmt.Errorf("remove killed ffmpeg artifacts: %w", removeErr)
			err = errors.Join(err, cleanupErr)
		}
	}

	manager.mu.Lock()
	if reapErr == nil {
		manager.closeJobSourceLocked(job)
	}
	manager.cancelIdleLocked(job)
	job.waitErr = err
	job.state = transcodeExited
	if job.hls != nil && !removeOutput && cleanupErr == nil {
		job.hls.retain = true
	}
	close(job.waitDone)
	if reapErr != nil {
		job.reapPending = true
		job.reapDone = make(chan struct{})
		job.reapProc = proc
		job.reapErr = reapErr
	}
	if reapErr != nil && removeOutput {
		// A surviving group may still write its family. Keep artifacts and their
		// manager ownership until a later reap attempt confirms it is gone.
		job.cleanupPending = true
	}
	if cleanupErr != nil {
		job.cleanupPending = true
		job.cleanupErr = cleanupErr
	}
	if reapErr != nil || cleanupErr != nil {
		manager.shutdownErr = errors.Join(reapErr, cleanupErr)
	} else {
		manager.deleteResolvedJobLocked(job)
	}
	handoff := lost && reapErr == nil && cleanupErr == nil && job.hls.handoff != nil && !manager.closed
	if handoff {
		manager.addWorkerLocked()
	}
	manager.mu.Unlock()
	if handoff {
		go manager.handOff(job)
	}
	manager.doneWorker()
}

// handoffTimeout bounds one handoff restart, which includes a placement
// queue wait.
const handoffTimeout = 2 * time.Minute

// handOff restarts a family whose remote host was lost or drained at the
// first segment the host did not complete (#617), while the client still
// plays its buffered segments. A failed handoff leaves the restart to the
// next segment request.
func (manager *TranscodeManager) handOff(job *transcodeJob) {
	defer manager.doneWorker()
	current, found, err := job.hls.currentIndex(manager)
	if err != nil || !found {
		return
	}
	manager.mu.Lock()
	stale := manager.jobs[job.output] != job
	manager.mu.Unlock()
	if stale {
		return
	}
	ctx, cancel := context.WithTimeout(manager.handoffCtx, handoffTimeout)
	defer cancel()
	if err := job.hls.handoff(ctx, current+1); err != nil && manager.options.Logger != nil {
		manager.options.Logger.Info("HLS handoff restart did not run; the next segment request restarts the family", "playlist", job.output, "segment", current+1, "error", err.Error())
	}
}

// logFailure records an ffmpeg failure that a client sees only as an HTTP
// 500. The record names the job and the ffmpeg log that holds the cause.
func (manager *TranscodeManager) logFailure(job *transcodeJob, message string, err error) {
	logger := manager.options.Logger
	if logger == nil {
		return
	}
	attrs := []any{"output", job.output, "error", err.Error()}
	if job.logPath != "" {
		attrs = append(attrs, "ffmpeg_log", job.logPath)
		// The reason is at the end of the log, and a live failure may be
		// the only evidence: a remote operator cannot open the file.
		if tail := logTail(job.logPath, maxFailureLogTail); tail != "" {
			attrs = append(attrs, "ffmpeg_tail", tail)
		}
	}
	if job.hls != nil {
		attrs = append(attrs, "media_source_id", job.hls.mediaSourceID, "play_session_id", job.hls.playSessionID)
	}
	logger.Error(message, attrs...)
}

// logTail returns up to limit trailing bytes of the ffmpeg log, starting on a
// whole line. An unreadable or empty log gives an empty string.
func logTail(path string, limit int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return ""
	}
	offset := max(info.Size()-limit, 0)
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return ""
	}
	if offset > 0 {
		if index := strings.IndexByte(string(data), '\n'); index >= 0 {
			data = data[index+1:]
		}
	}
	return strings.TrimSpace(string(data))
}

func (manager *TranscodeManager) retryPendingReaps(ctx context.Context) error {
	manager.mu.Lock()
	pending := make([]*transcodeJob, 0)
	for _, job := range manager.jobs {
		if job.reapPending || job.cleanupPending {
			pending = append(pending, job)
		}
	}
	manager.mu.Unlock()

	var result error
	for _, job := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		manager.mu.Lock()
		if job.reapPending {
			manager.beginReapAttemptLocked(job)
			attempt := job.reapAttempt
			manager.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-attempt:
			}
		} else {
			manager.mu.Unlock()
		}

		manager.mu.Lock()
		reapErr := job.reapErr
		needsCleanup := job.cleanupPending
		manager.mu.Unlock()
		if reapErr != nil {
			result = errors.Join(result, reapErr)
			continue
		}
		if needsCleanup {
			manager.mu.Lock()
			manager.beginCleanupAttemptLocked(job)
			attempt := job.cleanupAttempt
			manager.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-attempt:
			}
			manager.mu.Lock()
			cleanupErr := job.cleanupErr
			manager.mu.Unlock()
			if cleanupErr != nil {
				result = errors.Join(result, cleanupErr)
			}
		}
	}

	manager.mu.Lock()
	if result == nil {
		manager.shutdownErr = nil
	}
	manager.mu.Unlock()
	return result
}

func (manager *TranscodeManager) retryPendingJob(ctx context.Context, job *transcodeJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	manager.mu.Lock()
	if job.reapPending {
		manager.beginReapAttemptLocked(job)
		attempt := job.reapAttempt
		manager.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-attempt:
		}
	} else {
		manager.mu.Unlock()
	}
	manager.mu.Lock()
	reapErr := job.reapErr
	needsCleanup := job.cleanupPending
	manager.mu.Unlock()
	if reapErr != nil {
		return reapErr
	}
	if needsCleanup {
		manager.mu.Lock()
		manager.beginCleanupAttemptLocked(job)
		attempt := job.cleanupAttempt
		manager.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-attempt:
		}
		manager.mu.Lock()
		cleanupErr := job.cleanupErr
		manager.mu.Unlock()
		if cleanupErr != nil {
			return cleanupErr
		}
	}
	manager.mu.Lock()
	hasPending := false
	for _, candidate := range manager.jobs {
		if candidate.reapPending || candidate.cleanupPending {
			hasPending = true
			break
		}
	}
	if !hasPending {
		manager.shutdownErr = nil
	}
	manager.mu.Unlock()
	return nil
}

func (manager *TranscodeManager) beginReapAttemptLocked(job *transcodeJob) {
	if job.reapAttempt != nil {
		return
	}
	attempt := make(chan struct{})
	job.reapAttempt = attempt
	proc := job.reapProc
	manager.addWorkerLocked()
	go func() {
		err := proc.reap()
		manager.mu.Lock()
		if job.reapPending {
			if err != nil {
				job.reapErr = fmt.Errorf("reap ffmpeg process group: %w", err)
				manager.shutdownErr = job.reapErr
			} else {
				job.reapSucceeded = true
				job.reapErr = nil
			}
		}
		manager.mu.Unlock()
		if err == nil && job.hls != nil {
			manager.mu.Lock()
			stderr := job.stderr
			job.stderr = nil
			manager.mu.Unlock()
			manager.stopHLSSidecars(job)
			if stderr != nil {
				_ = stderr.Close()
			}
		}
		manager.doneWorker()
		manager.mu.Lock()
		if job.reapSucceeded {
			job.reapSucceeded = false
			job.reapPending = false
			close(job.reapDone)
			manager.closeJobSourceLocked(job)
			job.reapDone = nil
			manager.deleteResolvedJobLocked(job)
		}
		job.reapAttempt = nil
		close(attempt)
		manager.mu.Unlock()
	}()
}

func (manager *TranscodeManager) beginCleanupAttemptLocked(job *transcodeJob) {
	if job.cleanupAttempt != nil {
		return
	}
	attempt := make(chan struct{})
	job.cleanupAttempt = attempt
	manager.addWorkerLocked()
	go func() {
		err := manager.removeArtifacts(job)
		manager.mu.Lock()
		if job.cleanupPending {
			if err != nil {
				job.cleanupErr = fmt.Errorf("remove killed ffmpeg artifacts: %w", err)
				manager.shutdownErr = job.cleanupErr
			} else {
				job.cleanupSucceeded = true
				job.cleanupErr = nil
			}
		}
		manager.mu.Unlock()
		if err == nil && manager.beforeCleanupAttemptDone != nil {
			manager.beforeCleanupAttemptDone()
		}
		manager.doneWorker()
		manager.mu.Lock()
		if job.cleanupSucceeded {
			job.cleanupSucceeded = false
			job.cleanupPending = false
			manager.deleteResolvedJobLocked(job)
		}
		job.cleanupAttempt = nil
		close(attempt)
		manager.mu.Unlock()
	}()
}

func (manager *TranscodeManager) deleteResolvedJobLocked(job *transcodeJob) {
	if job.hls != nil && job.hls.retain && !manager.closed {
		return
	}
	if !job.reapPending && !job.cleanupPending && manager.jobs[job.output] == job {
		manager.closeJobSourceLocked(job)
		delete(manager.jobs, job.output)
		manager.unregisterHLSSessionLocked(job)
		if job.hls != nil {
			manager.deleteHLSLockIfRetiredLocked(job.output)
		}
	}
}

func (manager *TranscodeManager) deleteHLSLockIfRetiredLocked(playlist string) {
	if manager.jobs[playlist] == nil && manager.hlsLockUsers[playlist] == 0 {
		delete(manager.hlsLocks, playlist)
		delete(manager.hlsLockUsers, playlist)
	}
}

func (job *transcodeJob) startError() error {
	job.manager.mu.Lock()
	defer job.manager.mu.Unlock()
	return job.startupErr
}

func (job *transcodeJob) result() (transcodeState, error) {
	job.manager.mu.Lock()
	defer job.manager.mu.Unlock()
	return job.state, job.waitErr
}

func (manager *TranscodeManager) waitForOutput(ctx context.Context, job *transcodeJob) (*os.File, error) {
	for {
		if manager.beforeOutputRead != nil {
			manager.beforeOutputRead()
		}
		file, err := manager.openExistingRegular(job.output)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("open progressive output %q: %w", job.output, err)
		}

		timer := manager.options.Clock.NewTimer(manager.options.OutputPollInterval)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return nil, ctx.Err()
		case <-job.waitDone:
			stopTimer(timer)
			if manager.beforeOutputRead != nil {
				manager.beforeOutputRead()
			}
			file, openErr := manager.openExistingRegular(job.output)
			if openErr == nil {
				return file, nil
			}
			_, waitErr := job.result()
			if waitErr != nil {
				return nil, job.failureError(waitErr)
			}
			if !errors.Is(openErr, unix.ENOENT) {
				return nil, fmt.Errorf("open completed progressive output %q: %w", job.output, openErr)
			}
			return nil, ErrTranscodeExited
		case <-timer.C():
		}
	}
}

func (job *transcodeJob) failureError(waitErr error) error {
	manager := job.manager
	if manager.beforeDiagnosticRead != nil {
		manager.beforeDiagnosticRead()
	}
	file, err := manager.openExistingRegular(job.output + ".stderr.log")
	if err != nil {
		return waitErr
	}
	diagnostic, readErr := io.ReadAll(io.LimitReader(file, maxFailureDiagnostic))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(diagnostic) == 0 {
		return waitErr
	}
	return fmt.Errorf("%w: %s", waitErr, diagnostic)
}

// CleanupInactive deletes files in the transcodes directory that do not belong
// to a live job and whose mtime is before olderThan. It is the scheduled-task
// seam behind DeleteTranscodeFiles: process/file control stays in internal/ffmpeg.
// The manager mutex serializes the output-path set against concurrent job
// lifecycle changes — each deletion re-checks liveness under the mutex so a
// job that becomes live during the walk is never removed. Manager closed →
// ErrManagerClosed → task Failed.
func (manager *TranscodeManager) CleanupInactive(ctx context.Context, olderThan time.Time) error {
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return ErrManagerClosed
	}
	dir := manager.directory
	manager.mu.Unlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if manager.afterCleanupEnumeration != nil {
		manager.afterCleanupEnumeration()
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !info.ModTime().Before(olderThan) {
			continue
		}
		fullPath := filepath.Join(dir, entry.Name())
		// Re-check liveness and remove under the same mutex acquisition so a
		// job that becomes live during the walk cannot have its output
		// removed: the check-then-delete is atomic with respect to job
		// lifecycle changes (spec §4: skip every output path belonging to a
		// live job).
		manager.mu.Lock()
		isLive := false
		for _, job := range manager.jobs {
			if job.state != transcodeExited && job.output == fullPath {
				isLive = true
				break
			}
		}
		if !isLive {
			_ = os.Remove(fullPath)
		}
		manager.mu.Unlock()
	}
	return nil
}

// Close starts shutdown once and waits until every owned lifecycle goroutine has
// completed. If context expires it may be called again to continue waiting.
func (manager *TranscodeManager) Close(ctx context.Context) error {
	manager.handoffCancel()
	manager.mu.Lock()
	wasClosed := manager.closed
	if !manager.closed {
		manager.closed = true
		jobs := make([]*transcodeJob, 0, len(manager.jobs))
		exitedHLS := make([]*transcodeJob, 0)
		for _, job := range manager.jobs {
			manager.cancelIdleLocked(job)
			if job.state == transcodeStarting && job.startCancel != nil {
				job.startCancel()
			}
			if job.state != transcodeExited {
				jobs = append(jobs, job)
			} else if job.hls != nil {
				exitedHLS = append(exitedHLS, job)
			}
		}
		for range jobs {
			manager.addWorkerLocked()
		}
		for range exitedHLS {
			manager.addWorkerLocked()
		}
		manager.closeIfDrainedLocked()
		manager.mu.Unlock()
		for _, job := range jobs {
			go func(job *transcodeJob) {
				defer manager.doneWorker()
				if manager.releaseRemote(job) {
					return
				}
				job.stopOnce.Do(job.stop)
			}(job)
		}
		for _, job := range exitedHLS {
			go func(job *transcodeJob) {
				defer manager.doneWorker()
				manager.cleanupExitedHLS(job)
			}(job)
		}
	} else {
		manager.mu.Unlock()
	}
	if err := manager.retryPendingHLSReaps(ctx); err != nil {
		manager.stopPendingHLSSidecars()
		select {
		case <-manager.shutdownDone:
		case <-ctx.Done():
		}
		return err
	}

	// A completed shutdown is observable state, not work that remains subject to
	// the caller's deadline. Prefer it when both channels are already ready.
	select {
	case <-manager.shutdownDone:
		return manager.closeResult(ctx, wasClosed)
	default:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-manager.shutdownDone:
		return manager.closeResult(ctx, wasClosed)
	}
}

func (manager *TranscodeManager) cleanupExitedHLS(job *transcodeJob) {
	manager.mu.Lock()
	pending := job.reapPending || job.cleanupPending
	manager.mu.Unlock()
	if pending {
		return
	}
	err := manager.removeArtifacts(job)
	manager.mu.Lock()
	if err != nil {
		job.cleanupPending = true
		job.cleanupErr = fmt.Errorf("remove completed HLS artifacts: %w", err)
		manager.shutdownErr = errors.Join(manager.shutdownErr, job.cleanupErr)
	} else {
		manager.deleteResolvedJobLocked(job)
	}
	manager.mu.Unlock()
}

func (manager *TranscodeManager) stopPendingHLSSidecars() {
	manager.mu.Lock()
	jobs := make([]*transcodeJob, 0)
	for _, job := range manager.jobs {
		if job.hls != nil && (job.reapPending || job.cleanupPending) {
			jobs = append(jobs, job)
		}
	}
	manager.mu.Unlock()
	for _, job := range jobs {
		manager.mu.Lock()
		stderr := job.stderr
		job.stderr = nil
		manager.mu.Unlock()
		manager.stopHLSSidecars(job)
		if stderr != nil {
			_ = stderr.Close()
		}
	}
}

func (manager *TranscodeManager) retryPendingHLSReaps(ctx context.Context) error {
	manager.mu.Lock()
	jobs := make([]*transcodeJob, 0)
	for _, job := range manager.jobs {
		if job.hls != nil && job.reapPending {
			jobs = append(jobs, job)
		}
	}
	manager.mu.Unlock()
	for _, job := range jobs {
		if err := manager.retryPendingJob(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

func (manager *TranscodeManager) closeResult(ctx context.Context, wasClosed bool) error {
	if wasClosed {
		return manager.retryPendingReaps(ctx)
	}
	manager.mu.Lock()
	err := manager.shutdownErr
	manager.mu.Unlock()
	return err
}

// leaveStart records that one request stopped waiting for the start. When
// none waits for a job that is still starting, its start is canceled.
func (manager *TranscodeManager) leaveStart(job *transcodeJob) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if job.startWaiters > 0 {
		job.startWaiters--
	}
	if job.startWaiters == 0 && job.state == transcodeStarting && job.startCancel != nil {
		job.startCancel()
	}
}

func (manager *TranscodeManager) release(job *transcodeJob) {
	manager.mu.Lock()
	if job.requests > 0 {
		job.requests--
	}
	if job.requests == 0 && job.state == transcodeRunning && !job.leaderExited && !manager.closed {
		manager.armIdleLocked(job)
	}
	manager.mu.Unlock()
}

func (manager *TranscodeManager) armIdleLocked(job *transcodeJob) {
	if job.idleTimer != nil || job.state != transcodeRunning || job.requests != 0 {
		return
	}
	timeout := job.idleTimeout
	if timeout == 0 {
		timeout = manager.options.ProgressiveIdleTimeout
	}
	timer := manager.options.Clock.NewTimer(timeout)
	cancel := make(chan struct{})
	job.idleTimer = timer
	job.idleCancel = cancel
	manager.addWorkerLocked()
	go manager.idle(job, timer, cancel)
}

func (manager *TranscodeManager) cancelIdleLocked(job *transcodeJob) {
	if job.idleTimer == nil {
		return
	}
	stopTimer(job.idleTimer)
	close(job.idleCancel)
	job.idleTimer = nil
	job.idleCancel = nil
}

func (manager *TranscodeManager) idle(job *transcodeJob, timer clock.Timer, cancel <-chan struct{}) {
	defer manager.doneWorker()
	select {
	case <-cancel:
		return
	case <-timer.C():
	}

	manager.mu.Lock()
	shouldStop := job.idleTimer == timer && job.requests == 0 && job.state == transcodeRunning
	if shouldStop {
		job.idleTimer = nil
		job.idleCancel = nil
		job.state = transcodeTerminating
	}
	manager.mu.Unlock()
	if shouldStop && manager.beforeIdleStop != nil {
		manager.beforeIdleStop()
	}
	if shouldStop {
		job.stopOnce.Do(job.stop)
	}
}

func (job *transcodeJob) stop() {
	manager := job.manager
	for {
		manager.mu.Lock()
		if job.state != transcodeStarting {
			break
		}
		startDone := job.startDone
		manager.mu.Unlock()
		<-startDone
	}
	if (job.state != transcodeRunning && job.state != transcodeTerminating) || job.proc == nil {
		manager.mu.Unlock()
		return
	}
	job.state = transcodeTerminating
	stdin := job.stdin
	throttler := job.throttler
	proc := job.proc
	waitDone := job.waitDone
	manager.mu.Unlock()

	// Unpause first, as pinned TranscodingJob.Stop does: a paused ffmpeg
	// does not read "q" as the quit key.
	if throttler != nil {
		_ = throttler.Close(context.Background())
	}
	if stdin != nil {
		_, _ = io.WriteString(stdin, "q\n")
	}
	grace := manager.options.Clock.NewTimer(manager.options.KillGracePeriod)
	select {
	case <-waitDone:
		stopTimer(grace)
		return
	case <-grace.C():
	}
	proc.kill()
	<-waitDone
}

func (manager *TranscodeManager) addWorkerLocked() {
	manager.workers.Add(1)
	manager.workerCount++
}

func (manager *TranscodeManager) doneWorker() {
	manager.workers.Done()
	manager.mu.Lock()
	manager.workerCount--
	manager.closeIfDrainedLocked()
	manager.mu.Unlock()
}

func (manager *TranscodeManager) closeIfDrainedLocked() {
	if manager.closed && manager.workerCount == 0 {
		manager.shutdownOnce.Do(func() { close(manager.shutdownDone) })
	}
}

type progressiveReader struct {
	context context.Context
	job     *transcodeJob
	file    *os.File

	mu         sync.Mutex
	stallTimer clock.Timer
	done       chan struct{}
	once       sync.Once
}

func (reader *progressiveReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	for {
		count, err := reader.file.Read(buffer)
		if count > 0 {
			reader.resetStall()
			return count, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			reader.stopStall()
			return 0, err
		}
		state, waitErr := reader.job.result()
		if state == transcodeExited {
			reader.stopStall()
			if waitErr != nil {
				return 0, waitErr
			}
			return 0, io.EOF
		}
		if err := reader.waitForMore(); err != nil {
			reader.stopStall()
			return 0, err
		}
	}
}

func (reader *progressiveReader) resetStall() {
	reader.mu.Lock()
	stopTimer(reader.stallTimer)
	reader.stallTimer = nil
	reader.mu.Unlock()
}

func (reader *progressiveReader) stopStall() {
	reader.resetStall()
}

func (reader *progressiveReader) waitForMore() error {
	manager := reader.job.manager
	reader.mu.Lock()
	if reader.stallTimer == nil {
		reader.stallTimer = manager.options.Clock.NewTimer(manager.options.ReaderStallTimeout)
	}
	stall := reader.stallTimer
	reader.mu.Unlock()

	retry := manager.options.Clock.NewTimer(manager.options.ReaderRetryInterval)
	select {
	case <-reader.done:
		stopTimer(retry)
		return os.ErrClosed
	case <-reader.context.Done():
		stopTimer(retry)
		return reader.context.Err()
	case <-reader.job.waitDone:
		stopTimer(retry)
		return nil
	case <-stall.C():
		stopTimer(retry)
		return ErrOutputStalled
	case <-retry.C():
		select {
		case <-stall.C():
			return ErrOutputStalled
		default:
			return nil
		}
	}
}

func (reader *progressiveReader) Close() error {
	var err error
	reader.once.Do(func() {
		close(reader.done)
		reader.stopStall()
		err = reader.file.Close()
		reader.job.manager.release(reader.job)
	})
	return err
}

func stopTimer(timer clock.Timer) {
	if timer != nil {
		timer.Stop()
	}
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (writer *limitedWriter) Write(data []byte) (int, error) {
	if writer.remaining <= 0 {
		return len(data), nil
	}
	written := len(data)
	if len(data) > writer.remaining {
		data = data[:writer.remaining]
	}
	n, err := writer.writer.Write(data)
	writer.remaining -= n
	if err != nil {
		return n, err
	}
	if n != len(data) {
		return n, io.ErrShortWrite
	}
	return written, nil
}
