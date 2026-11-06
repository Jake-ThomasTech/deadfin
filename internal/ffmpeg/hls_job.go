// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
	"golang.org/x/sys/unix"
)

const (
	hlsIdleTimeout      = 60 * time.Second
	maxHLSPlaylistBytes = 8 << 20
)

// HLSRequest identifies one playlist family produced by ffmpeg. Args excludes
// the executable and is passed directly to the manager's configured ffmpeg path.
type HLSRequest struct {
	PlaylistPath   string
	Live           bool
	MinSegments    int
	SegmentPattern string
	Args           []string
	// IdentityArgs is the rendition argv without the per-request seek and
	// start number. A request joins a running family when its identity is
	// equal; only the restart policy then decides whether ffmpeg relaunches,
	// as upstream keys a job on its playlist path
	// (DynamicHlsController.GetDynamicSegment). Nil uses Args.
	IdentityArgs         []string
	PlaySessionID        string
	DeviceID             string
	MediaSourceID        string
	StartTimeTicks       int64
	SegmentLengthSeconds int
	// MaxForwardGap is the largest forward distance, in segment indexes,
	// that joins the running job instead of restarting it. Zero uses the
	// upstream 24/SegmentLengthSeconds (DynamicHlsController.GetDynamicSegment).
	MaxForwardGap int
	// SegmentStartTicks is the media start of each output file, by file
	// index, when the files do not follow SegmentLengthSeconds: a
	// keyframe-cut remux writes one file per source GOP (D-72). The segment
	// cleaner, the cache pause guard, and the nominal download position then
	// use these boundaries (deadfin#799). Nil uses SegmentLengthSeconds.
	SegmentStartTicks []int64
	RunTimeTicks      int64
	// SourceRevision is the canonical identity of the media source. It must
	// agree for every request that joins this playlist family.
	SourceRevision string
	// OpenSource opens and validates the media source once for this ffmpeg
	// family. The manager owns a successful result through process reap.
	OpenSource func() (*sourcefile.Source, error)
	// EmbeddedSubtitleSource is the exact opaque subtitles=f='…' filter operand
	// naming the primary media source. It requires OpenSource so the manager can
	// provide libass an independently opened pinned descriptor.
	EmbeddedSubtitleSource string
	// ExternalSubtitleSource is the exact opaque subtitles=f='…' filter
	// operand for an external text subtitle. Its revision and opener authorize
	// an independently pinned descriptor.
	ExternalSubtitleSource   string
	ExternalSubtitleRevision string
	OpenExternalSubtitle     func() (*sourcefile.Source, error)
	// SubtitleFontsDir is the fontsdir option of the subtitle burn-in. A
	// transcode worker receives its fonts as job assets (#624).
	SubtitleFontsDir string
	// Local keeps the family on this host. A Live TV input reads the
	// server's loopback stream broker, which a transcode worker cannot reach.
	Local bool
	// RemoteOnly marks a family whose output codec only a transcode worker
	// encodes (#854). Args is built for that worker, so the family never
	// runs on this host: without a remote start, the start fails.
	RemoteOnly bool
	// Facts feed transcode-worker placement (#616).
	Facts JobFacts
	// BuildArgs rebuilds Args for a transcode worker's capabilities and
	// hardware (#622). Nil sends Args unchanged.
	BuildArgs ArgsBuilder
	// Handoff restarts the family at a segment index through the caller's
	// segment path. The manager calls it when a remote host is lost or
	// drains, at the first segment the host did not complete, so the
	// replacement starts before a client asks for that segment (#617). Nil
	// leaves the restart to the next segment request.
	Handoff func(ctx context.Context, index int) error
	// TranscodingInfo is the session projection of this rendition. The
	// manager reports it for DeviceID while the job runs, as upstream
	// TranscodeManager.ReportTranscodingProgress does. Nil reports nothing.
	TranscodingInfo *dto.TranscodingInfo
}

// HLSSegmentTiming is the exact interval represented by one requested HLS segment.
// A nil timing value preserves legacy nominal segment-length accounting.
type HLSSegmentTiming struct {
	RuntimeTicks             int64
	ActualSegmentLengthTicks int64
}

type hlsJob struct {
	playlist    string
	pattern     string
	prefix      string
	suffix      string
	live        bool
	minSegments int
	matcher     *regexp.Regexp
	identity    []string

	playSessionID            string
	deviceID                 string
	facts                    JobFacts
	buildArgs                ArgsBuilder
	handoff                  func(context.Context, int) error
	segmentLength            int
	maxForwardGap            int
	segmentStarts            []int64
	runTimeTicks             int64
	startTimeTicks           int64
	mediaSourceID            string
	sourceRevision           string
	openSource               func() (*sourcefile.Source, error)
	subtitleSource           string
	externalSubtitleSource   string
	externalSubtitleRevision string
	openExternalSubtitle     func() (*sourcefile.Source, error)
	fontsDir                 string
	local                    bool
	remoteOnly               bool
	transcodingInfo          *dto.TranscodingInfo
	retain                   bool
	// adopted marks a remote family that a new server process adopted
	// (#618) and that no request joined yet: it has no rendition identity.
	adopted bool
}

var hlsIndexDirective = regexp.MustCompile(`%(?:[-+# 0]*[0-9]*(?:\.[0-9]+)?)d`)

var ErrHLSPlaylistTooLarge = errors.New("HLS playlist exceeds 8 MiB limit")

func (manager *TranscodeManager) validateHLSRequest(request HLSRequest) (*hlsJob, []string, error) {
	if request.PlaylistPath == "" {
		return nil, nil, errors.New("HLS playlist path is empty")
	}
	if request.SegmentPattern == "" {
		return nil, nil, errors.New("HLS segment pattern is empty")
	}
	if len(request.Args) == 0 {
		return nil, nil, errors.New("ffmpeg argv is empty")
	}
	if request.SegmentLengthSeconds <= 0 {
		return nil, nil, errors.New("HLS segment length must be positive")
	}
	if request.MinSegments < 0 {
		return nil, nil, errors.New("HLS minimum segment count cannot be negative")
	}
	for index, start := range request.SegmentStartTicks {
		if start < 0 || index > 0 && start <= request.SegmentStartTicks[index-1] {
			return nil, nil, errors.New("HLS segment start ticks must increase from zero or more")
		}
	}
	if request.SourceRevision != "" && request.OpenSource == nil {
		return nil, nil, errors.New("HLS source opener is required for a source revision")
	}
	if request.EmbeddedSubtitleSource != "" && request.OpenSource == nil {
		return nil, nil, errors.New("HLS embedded subtitle source requires a source opener")
	}
	if request.EmbeddedSubtitleSource != "" && request.ExternalSubtitleSource != "" {
		return nil, nil, errors.New("HLS embedded and external subtitle sources are mutually exclusive")
	}
	if request.ExternalSubtitleSource != "" && request.ExternalSubtitleRevision == "" {
		return nil, nil, errors.New("HLS external subtitle revision is required")
	}
	if request.ExternalSubtitleSource != "" && request.OpenExternalSubtitle == nil {
		return nil, nil, errors.New("HLS external subtitle opener is required")
	}
	if request.ExternalSubtitleSource != "" && request.OpenSource == nil {
		return nil, nil, errors.New("HLS external subtitle source requires a primary source opener")
	}
	if request.ExternalSubtitleSource == "" && (request.ExternalSubtitleRevision != "" || request.OpenExternalSubtitle != nil) {
		return nil, nil, errors.New("HLS external subtitle authority has no filter source")
	}

	family, err := manager.hlsFamily(request.PlaylistPath, request.SegmentPattern)
	if err != nil {
		return nil, nil, err
	}
	playlist, pattern := family.playlist, family.pattern
	args := append([]string(nil), request.Args...)
	if err := manager.validateHLSOutputOperands(args, playlist, pattern); err != nil {
		return nil, nil, err
	}
	identity := args
	if request.IdentityArgs != nil {
		identity = append([]string(nil), request.IdentityArgs...)
		if err := manager.validateHLSOutputOperands(identity, playlist, pattern); err != nil {
			return nil, nil, fmt.Errorf("HLS identity argv: %w", err)
		}
	}
	if request.EmbeddedSubtitleSource != "" && countEmbeddedSubtitleOperand(args, request.EmbeddedSubtitleSource) != 1 {
		return nil, nil, errors.New("HLS argv must contain exactly one embedded subtitle source operand")
	}
	if request.ExternalSubtitleSource != "" && countEmbeddedSubtitleOperand(args, request.ExternalSubtitleSource) != 1 {
		return nil, nil, errors.New("HLS argv must contain exactly one external subtitle source operand")
	}
	spec := &hlsJob{
		playlist:                 family.playlist,
		pattern:                  family.pattern,
		prefix:                   family.prefix,
		suffix:                   family.suffix,
		live:                     request.Live,
		minSegments:              request.MinSegments,
		matcher:                  family.matcher,
		identity:                 identity,
		playSessionID:            request.PlaySessionID,
		deviceID:                 request.DeviceID,
		facts:                    request.Facts,
		buildArgs:                request.BuildArgs,
		handoff:                  request.Handoff,
		runTimeTicks:             request.RunTimeTicks,
		startTimeTicks:           request.StartTimeTicks,
		mediaSourceID:            request.MediaSourceID,
		segmentLength:            request.SegmentLengthSeconds,
		maxForwardGap:            request.MaxForwardGap,
		segmentStarts:            slices.Clone(request.SegmentStartTicks),
		sourceRevision:           request.SourceRevision,
		openSource:               request.OpenSource,
		subtitleSource:           request.EmbeddedSubtitleSource,
		externalSubtitleSource:   request.ExternalSubtitleSource,
		externalSubtitleRevision: request.ExternalSubtitleRevision,
		openExternalSubtitle:     request.OpenExternalSubtitle,
		fontsDir:                 request.SubtitleFontsDir,
		local:                    request.Local,
		remoteOnly:               request.RemoteOnly,
		transcodingInfo:          request.TranscodingInfo,
	}
	return spec, args, nil
}

// hlsFamily validates the output paths of a playlist family and returns a
// job with only the family fields set.
func (manager *TranscodeManager) hlsFamily(playlistPath, segmentPattern string) (*hlsJob, error) {
	playlist, err := manager.normalizeOutputPath(playlistPath)
	if err != nil {
		return nil, err
	}
	pattern, err := manager.normalizeOutputPath(segmentPattern)
	if err != nil {
		return nil, err
	}
	matches := hlsIndexDirective.FindAllStringIndex(pattern, -1)
	if len(matches) != 1 {
		return nil, errors.New("HLS segment pattern must contain exactly one integer index directive")
	}
	remaining := pattern[:matches[0][0]] + pattern[matches[0][1]:]
	if strings.Contains(strings.ReplaceAll(remaining, "%%", ""), "%") {
		return nil, errors.New("HLS segment pattern contains an unsupported format directive")
	}
	for _, index := range []int{-1, 0} {
		formatted, formatErr := formatHLSSegment(pattern, index)
		if formatErr != nil {
			return nil, formatErr
		}
		if _, normalizeErr := manager.normalizeOutputPath(formatted); normalizeErr != nil {
			return nil, normalizeErr
		}
	}
	prefix, suffix := filepath.Base(pattern[:matches[0][0]]), pattern[matches[0][1]:]
	if directory := filepath.Dir(pattern); directory != filepath.Dir(fmt.Sprintf(pattern, 0)) {
		return nil, errors.New("HLS segment index directive must be in the filename")
	}
	return &hlsJob{
		playlist: playlist,
		pattern:  pattern,
		prefix:   prefix,
		suffix:   suffix,
		matcher:  regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + `(-?[0-9]+)` + regexp.QuoteMeta(suffix) + "$"),
	}, nil
}

func countEmbeddedSubtitleOperand(args []string, token string) int {
	count := 0
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "-vf" || args[index] == "-filter_complex" {
			count += strings.Count(args[index+1], token)
		}
	}
	return count
}

// hlsJobConflicts reports whether spec names a different rendition than the
// running family. It compares rendition identities, not the launch argv: a
// sequential segment request carries its own seek and start number, and must
// not relaunch a job that already produces it.
func hlsJobConflicts(job *transcodeJob, spec *hlsJob) bool {
	if job.hls != nil && job.hls.adopted {
		// An adopted family takes the rendition of its first request.
		return job.hls.pattern != spec.pattern
	}
	return job.hls == nil || !reflect.DeepEqual(job.hls.identity, spec.identity) ||
		job.hls.sourceRevision != spec.sourceRevision ||
		job.hls.subtitleSource != spec.subtitleSource ||
		job.hls.externalSubtitleSource != spec.externalSubtitleSource ||
		job.hls.externalSubtitleRevision != spec.externalSubtitleRevision
}

func formatHLSSegment(pattern string, index int) (string, error) {
	path := fmt.Sprintf(pattern, index)
	if strings.Contains(path, "%!") {
		return "", fmt.Errorf("format HLS segment path %q", pattern)
	}
	return path, nil
}

func (manager *TranscodeManager) validateHLSOutputOperands(args []string, playlist, pattern string) error {
	for index, argument := range args {
		if argument != "-hls_segment_filename" {
			continue
		}
		if index+1 == len(args) {
			return errors.New("HLS segment filename operand is missing")
		}
		candidate, err := manager.normalizeOutputPath(args[index+1])
		if err != nil || candidate != pattern {
			return errors.New("ffmpeg HLS segment filename operand does not match the request")
		}
	}
	last := args[len(args)-1]
	if filepath.IsAbs(last) || strings.HasSuffix(strings.ToLower(last), ".m3u8") {
		candidate, err := manager.normalizeOutputPath(last)
		if err != nil || candidate != playlist {
			return errors.New("ffmpeg HLS playlist operand does not match the request")
		}
	}
	return nil
}

// StartHLS starts (or joins) the playlist's shared ffmpeg job. Request contexts
// govern only startup; they never become process ownership.
func (manager *TranscodeManager) StartHLS(ctx context.Context, request HLSRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, _, err := manager.validateHLSRequest(request); err != nil {
		return err
	}
	manager.mu.Lock()
	key, epoch := manager.beginHLSRestartLocked(request.PlaySessionID, request.DeviceID)
	manager.mu.Unlock()
	defer func() {
		manager.mu.Lock()
		manager.releaseHLSRestartSessionLocked(request.PlaySessionID, request.DeviceID, key)
		manager.mu.Unlock()
	}()
	return manager.startHLS(ctx, request, nil, key, epoch, true)
}

func (manager *TranscodeManager) startHLS(ctx context.Context, request HLSRequest, replace *transcodeJob, expectedSessionKey string, expectedSessionEpoch uint64, startAdmission bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	spec, args, err := manager.validateHLSRequest(request)
	if err != nil {
		return err
	}
	beforeRegisterCalled := false
	for {
		manager.mu.Lock()
		if manager.closed {
			manager.mu.Unlock()
			return ErrManagerClosed
		}
		if expectedSessionKey != "" && manager.hlsSessionEpoch[expectedSessionKey] != expectedSessionEpoch {
			manager.mu.Unlock()
			return errors.New("HLS play session stopped during restart")
		}
		job, exists := manager.jobs[spec.playlist]
		if replace != nil && job == replace {
			if job.reapPending || job.cleanupPending {
				waitErr := job.waitErr
				manager.mu.Unlock()
				if waitErr != nil {
					return job.failureError(waitErr)
				}
				return fmt.Errorf("HLS playlist %q cleanup is pending", spec.playlist)
			}
			manager.closeJobSourceLocked(job)
			delete(manager.jobs, spec.playlist)
			manager.unregisterHLSSessionLocked(job)
			manager.deleteHLSLockIfRetiredLocked(spec.playlist)
			job, exists = nil, false
		}
		if exists && hlsJobConflicts(job, spec) {
			manager.mu.Unlock()
			return fmt.Errorf("conflicting ffmpeg argv or source authority for %q", spec.playlist)
		}
		if exists {
			claimAdoptedLocked(job, spec)
		}
		if exists && (job.cleanupPending || job.reapPending || job.state == transcodeTerminating) {
			waitDone := job.waitDone
			manager.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-waitDone:
				state, waitErr := job.result()
				if state == transcodeExited {
					if waitErr != nil {
						return job.failureError(waitErr)
					}
					return fmt.Errorf("HLS playlist %q has exited; use RestartHLSAt", spec.playlist)
				}
				continue
			}
		}
		if exists && job.state == transcodeExited {
			if job.hls != nil && job.hls.retain && job.waitErr == nil {
				manager.mu.Unlock()
				return nil
			}
			manager.mu.Unlock()
			return fmt.Errorf("HLS playlist %q has exited; use RestartHLSAt", spec.playlist)
		}
		leader := !exists
		if leader && startAdmission && !beforeRegisterCalled {
			beforeRegisterCalled = true
			manager.mu.Unlock()
			if manager.beforeHLSStartRegister != nil {
				manager.beforeHLSStartRegister()
			}
			continue
		}
		if leader {
			startCtx, startCancel := context.WithCancel(context.Background())
			job = &transcodeJob{
				manager:     manager,
				output:      spec.playlist,
				args:        args,
				startDone:   make(chan struct{}),
				waitDone:    make(chan struct{}),
				state:       transcodeStarting,
				idleTimeout: hlsIdleTimeout,
				hls:         spec,
				startCtx:    startCtx,
				startCancel: startCancel,
			}
			manager.jobs[spec.playlist] = job
			manager.registerHLSSessionLocked(job)
			manager.addWorkerLocked()
		}
		if !leader {
			manager.resetHLSIdleLocked(job)
		}
		job.startWaiters++
		manager.mu.Unlock()
		if leader {
			go manager.start(job)
		}
		select {
		case <-ctx.Done():
			manager.leaveStart(job)
			return ctx.Err()
		case <-job.startDone:
			manager.leaveStart(job)
		}
		if startupErr := job.startError(); startupErr != nil {
			return startupErr
		}
		return nil
	}
}

// OpenLegacyHLS safely opens a legacy HLS artifact associated with exactly one
// manager-owned transcode family. Callers pass names, never filesystem paths.
func (manager *TranscodeManager) OpenLegacyHLS(ctx context.Context, playlistID, artifactName string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !legacyHLSToken(playlistID, true) || !legacyHLSToken(artifactName, false) {
		return nil, errors.New("unsafe legacy HLS artifact token")
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil, ErrManagerClosed
	}
	var selected *transcodeJob
	for _, job := range manager.jobs {
		if !legacyHLSJobMatches(job, playlistID, artifactName) {
			continue
		}
		if selected != nil {
			manager.mu.Unlock()
			return nil, errors.New("ambiguous legacy HLS artifact family")
		}
		selected = job
	}
	if selected == nil {
		manager.mu.Unlock()
		return nil, fmt.Errorf("%w: legacy HLS artifact family not found", fs.ErrNotExist)
	}
	if selected.state == transcodeExited && (selected.waitErr != nil || selected.cleanupPending) {
		manager.mu.Unlock()
		return nil, fmt.Errorf("%w: legacy HLS artifact family is unavailable", fs.ErrNotExist)
	}
	segmentIndex := -1
	if selected.hls != nil {
		selected.requests++
		manager.cancelIdleLocked(selected)
	}
	path := selected.output
	if selected.hls != nil && artifactName != filepath.Base(selected.hls.playlist) {
		var pathErr error
		segmentIndex, _ = selected.hls.matches(artifactName)
		path, pathErr = selected.hls.segmentPathFromName(artifactName)
		if pathErr != nil {
			if selected.hls != nil {
				selected.requests--
			}
			manager.mu.Unlock()
			return nil, pathErr
		}
	}
	manager.mu.Unlock()
	file, err := manager.openExistingRegular(path)
	if err != nil {
		if selected.hls != nil {
			manager.release(selected)
		}
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("%w: legacy HLS artifact %q", fs.ErrNotExist, artifactName)
		}
		return nil, err
	}
	if selected.hls != nil {
		return newHLSSegmentReader(selected, file, segmentIndex, nil), nil
	}
	return file, nil
}

func legacyHLSToken(token string, allowEmpty bool) bool {
	if token == "" {
		return allowEmpty
	}
	if token == "." || token == ".." || strings.ContainsAny(token, "/\\") {
		return false
	}
	for _, value := range []byte(token) {
		if value < 0x20 || value == 0x7f {
			return false
		}
	}
	return true
}

func legacyHLSJobMatches(job *transcodeJob, playlistID, artifactName string) bool {
	if job.hls == nil {
		return playlistID == "" && filepath.Base(job.output) == artifactName && job.state != transcodeExited
	}
	if playlistID != "" && strings.TrimSuffix(filepath.Base(job.hls.playlist), filepath.Ext(job.hls.playlist)) != playlistID {
		return false
	}
	if artifactName == filepath.Base(job.hls.playlist) {
		return true
	}
	_, matched := job.hls.matches(artifactName)
	return matched
}

func (job *hlsJob) segmentPathFromName(name string) (string, error) {
	index, matched := job.matches(name)
	if !matched {
		return "", errors.New("legacy HLS artifact is not in its playlist family")
	}
	path, err := job.segmentPath(index)
	if err != nil {
		return "", err
	}
	if filepath.Base(path) != name {
		return "", errors.New("legacy HLS artifact does not match its segment pattern")
	}
	return path, nil
}

// WaitSegment opens one completed HLS segment. A segment is complete only
// after its successor exists or ffmpeg has exited. The successor of the fMP4
// initialization segment (index -1) is the first media segment of the job,
// which is segment N for a job that restarted at -start_number N. Thus the
// initialization segment is complete once any media segment exists, as in
// upstream GetSegmentResult.
func (manager *TranscodeManager) WaitSegment(ctx context.Context, playlist string, index int, timings ...*HLSSegmentTiming) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(timings) > 1 {
		return nil, errors.New("HLS segment timing accepts at most one value")
	}
	var timing *HLSSegmentTiming
	if len(timings) == 1 {
		timing = timings[0]
	}
	playlist, err := manager.normalizeOutputPath(playlist)
	if err != nil {
		return nil, err
	}

	waitRetainHookCalled := false
rebind:
	for {
		job := manager.snapshotHLSJob(playlist)
		if job == nil || job.hls == nil {
			return nil, fmt.Errorf("HLS playlist %q is not active", playlist)
		}
		if !waitRetainHookCalled && manager.beforeHLSWaitRetain != nil {
			waitRetainHookCalled = true
			manager.beforeHLSWaitRetain()
		}
		if !manager.retainHLSWaitSegment(playlist, job) {
			continue
		}

		for {
			manager.mu.Lock()
			current := manager.jobs[playlist] == job
			manager.mu.Unlock()
			if !current {
				manager.release(job)
				continue rebind
			}
			segment, segmentErr := job.hls.segmentPath(index)
			if segmentErr != nil {
				manager.release(job)
				return nil, segmentErr
			}
			file, openErr := manager.openExistingRegular(segment)
			if openErr != nil && !errors.Is(openErr, unix.ENOENT) {
				manager.release(job)
				return nil, fmt.Errorf("open HLS segment %q: %w", segment, openErr)
			}
			ready := false
			if openErr == nil && index == -1 {
				current, found, scanErr := job.hls.currentIndex(manager)
				if scanErr != nil {
					_ = file.Close()
					manager.release(job)
					return nil, scanErr
				}
				ready = found && current >= 0
			} else if openErr == nil {
				next, nextErr := job.hls.segmentPath(index + 1)
				if nextErr != nil {
					_ = file.Close()
					manager.release(job)
					return nil, nextErr
				}
				nextFile, nextErr := manager.openExistingRegular(next)
				if nextErr == nil {
					_ = nextFile.Close()
					ready = true
				} else if !errors.Is(nextErr, unix.ENOENT) {
					_ = file.Close()
					manager.release(job)
					return nil, fmt.Errorf("open successor HLS segment %q: %w", next, nextErr)
				}
			}
			if openErr == nil && ready {
				manager.mu.Lock()
				current = manager.jobs[playlist] == job
				manager.mu.Unlock()
				if current {
					return newHLSSegmentReader(job, file, index, timing), nil
				}
				_ = file.Close()
				manager.release(job)
				continue rebind
			}
			if file != nil {
				_ = file.Close()
			}
			if isClosed(job.waitDone) {
				manager.mu.Lock()
				current = manager.jobs[playlist] == job
				manager.mu.Unlock()
				if !current {
					manager.release(job)
					continue rebind
				}
				if openErr == nil {
					file, finalErr := manager.openExistingRegular(segment)
					if finalErr != nil {
						manager.release(job)
						return nil, finalErr
					}
					return newHLSSegmentReader(job, file, index, timing), nil
				}
				state, waitErr := job.result()
				manager.mu.Lock()
				lost := job.runnerLost
				manager.mu.Unlock()
				manager.release(job)
				if state == transcodeExited && waitErr != nil {
					return nil, job.failureError(waitErr)
				}
				if lost {
					return nil, errRemoteSegmentLost(playlist, index)
				}
				return nil, ErrTranscodeExited
			}
			timer := manager.options.Clock.NewTimer(manager.options.OutputPollInterval)
			select {
			case <-ctx.Done():
				stopTimer(timer)
				manager.release(job)
				return nil, ctx.Err()
			case <-job.waitDone:
				stopTimer(timer)
			case <-timer.C():
			}
		}
	}
}

// snapshotHLSJob waits for an owned restart registration before reading the
// current generation. The per-playlist lock also pins its map entry while this
// waiter is between the manager lock and the restart lock.
func (manager *TranscodeManager) snapshotHLSJob(playlist string) *transcodeJob {
	lock := manager.retainHLSRestartLock(playlist)
	lock.Lock()
	manager.mu.Lock()
	job := manager.jobs[playlist]
	manager.mu.Unlock()
	lock.Unlock()
	manager.releaseHLSRestartLock(playlist)
	return job
}

// retainHLSWaitSegment takes the restart gate before retaining a job, so a
// generation which was retired while a waiter ran its test hook cannot serve
// its artifacts.
func (manager *TranscodeManager) retainHLSWaitSegment(playlist string, job *transcodeJob) bool {
	lock := manager.retainHLSRestartLock(playlist)
	lock.Lock()
	manager.mu.Lock()
	current := manager.jobs[playlist] == job && job.hls != nil
	if current {
		job.requests++
		manager.cancelIdleLocked(job)
	}
	manager.mu.Unlock()
	lock.Unlock()
	manager.releaseHLSRestartLock(playlist)
	return current
}

func (manager *TranscodeManager) retainHLSRestartLock(playlist string) *sync.Mutex {
	manager.mu.Lock()
	lock := manager.hlsLocks[playlist]
	if lock == nil {
		lock = &sync.Mutex{}
		manager.hlsLocks[playlist] = lock
	}
	manager.hlsLockUsers[playlist]++
	manager.mu.Unlock()
	return lock
}

type hlsSegmentReader struct {
	file     *os.File
	job      *transcodeJob
	index    int
	bytes    int64
	once     sync.Once
	endTicks int64
	exactEnd bool
}

func newHLSSegmentReader(job *transcodeJob, file *os.File, index int, timing *HLSSegmentTiming) io.ReadCloser {
	reader := &hlsSegmentReader{file: file, job: job, index: index}
	if timing != nil {
		reader.endTicks = saturatingTickAdd(timing.RuntimeTicks, timing.ActualSegmentLengthTicks)
		reader.exactEnd = true
	}
	return reader
}

func (reader *hlsSegmentReader) Read(data []byte) (int, error) {
	count, err := reader.file.Read(data)
	if count > 0 {
		reader.bytes += int64(count)
	}
	return count, err
}

func saturatingTickAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	if right < 0 && left < math.MinInt64-right {
		return math.MinInt64
	}
	return left + right
}

func nominalHLSSegmentEnd(index, segmentLength int) int64 {
	if index < 0 || segmentLength <= 0 {
		return 0
	}
	ordinal := int64(index)
	if ordinal == math.MaxInt64 {
		return math.MaxInt64
	}
	ordinal++
	seconds := int64(segmentLength)
	if ordinal > math.MaxInt64/seconds {
		return math.MaxInt64
	}
	segmentsSeconds := ordinal * seconds
	ticksPerSecond := int64(time.Second / 100)
	if segmentsSeconds > math.MaxInt64/ticksPerSecond {
		return math.MaxInt64
	}
	return segmentsSeconds * ticksPerSecond
}

func (reader *hlsSegmentReader) Close() error {
	var err error
	reader.once.Do(func() {
		err = reader.file.Close()
		manager := reader.job.manager
		manager.mu.Lock()
		if reader.job.hls != nil {
			end := reader.endTicks
			if !reader.exactEnd {
				end = reader.job.hls.segmentEnd(reader.index)
			}
			if end > reader.job.downloadPositionTicks {
				reader.job.downloadPositionTicks = end
			}
			if end > reader.job.positionTicks {
				reader.job.positionTicks = end
			}
			reader.job.bytesDownloaded += reader.bytes
		}
		manager.mu.Unlock()
		manager.release(reader.job)
	})
	return err
}

// WaitPlaylist returns a completed live playlist without allowing a request to
// own or cancel the shared ffmpeg process.
func (manager *TranscodeManager) WaitPlaylist(ctx context.Context, playlist string, minSegments int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if minSegments < 0 {
		return "", errors.New("HLS minimum segment count cannot be negative")
	}
	playlist, err := manager.normalizeOutputPath(playlist)
	if err != nil {
		return "", err
	}

rebind:
	for {
		job := manager.snapshotHLSJob(playlist)
		if job == nil || job.hls == nil {
			return "", fmt.Errorf("HLS playlist %q is not active", playlist)
		}
		if minSegments == 0 && !job.hls.live {
			return "", errors.New("HLS minimum segment count must be positive for VOD")
		}
		if !manager.retainHLSWaitSegment(playlist, job) {
			continue
		}

		for {
			manager.mu.Lock()
			current := manager.jobs[playlist] == job
			manager.mu.Unlock()
			if !current {
				manager.release(job)
				continue rebind
			}
			playlistText, readErr := manager.readHLSPlaylist(playlist)
			manager.mu.Lock()
			current = manager.jobs[playlist] == job
			manager.mu.Unlock()
			if !current {
				manager.release(job)
				continue rebind
			}
			if readErr == nil && completePlaylistSegments(playlistText) >= minSegments {
				manager.release(job)
				return playlistText, nil
			}
			if errors.Is(readErr, ErrHLSPlaylistTooLarge) {
				go job.stopOnce.Do(job.stop)
				stopErr := manager.finishHLSFamilyStop(context.Background(), job, false)
				manager.release(job)
				if stopErr != nil {
					return "", errors.Join(readErr, stopErr)
				}
				return "", readErr
			}
			if readErr != nil && !errors.Is(readErr, unix.ENOENT) {
				manager.release(job)
				return "", readErr
			}
			if isClosed(job.waitDone) {
				state, waitErr := job.result()
				manager.release(job)
				replacement := manager.snapshotHLSJob(playlist)
				if replacement != nil && replacement != job && replacement.hls != nil {
					continue rebind
				}
				if replacement == nil || replacement.hls == nil {
					return "", fmt.Errorf("HLS playlist %q is not active", playlist)
				}
				if state == transcodeExited && waitErr != nil {
					return "", job.failureError(waitErr)
				}
				return "", ErrTranscodeExited
			}
			timer := manager.options.Clock.NewTimer(manager.options.OutputPollInterval)
			select {
			case <-ctx.Done():
				stopTimer(timer)
				manager.release(job)
				return "", ctx.Err()
			case <-job.waitDone:
				stopTimer(timer)
			case <-timer.C():
			}
		}
	}
}

func (manager *TranscodeManager) retainHLSWaiter(job *transcodeJob) {
	manager.mu.Lock()
	if job.hls != nil {
		job.requests++
		manager.cancelIdleLocked(job)
	}
	manager.mu.Unlock()
}

func (manager *TranscodeManager) readHLSPlaylist(playlist string) (playlistText string, err error) {
	file, err := manager.openExistingRegular(playlist)
	if err != nil {
		return "", err
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	if size < 0 || size > maxHLSPlaylistBytes {
		return "", ErrHLSPlaylistTooLarge
	}
	var content strings.Builder
	content.Grow(int(size))
	if _, err := io.CopyN(&content, file, size); err != nil {
		return "", err
	}
	return content.String(), nil
}

func completePlaylistSegments(playlist string) int {
	pending := false
	count := 0
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXTINF:") {
			pending = true
			continue
		}
		if pending && line != "" && !strings.HasPrefix(line, "#") {
			count++
			pending = false
		}
	}
	return count
}

// RestartHLSAt applies Jellyfin's seek-restart boundaries while serializing every
// transition for a playlist. A caller never overlaps ffmpeg process groups.

func (manager *TranscodeManager) RestartHLSAt(ctx context.Context, request HLSRequest, index int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	spec, _, err := manager.validateHLSRequest(request)
	if err != nil {
		return err
	}
	manager.mu.Lock()
	restartSessionKey, restartSessionEpoch := manager.beginHLSRestartLocked(request.PlaySessionID, request.DeviceID)
	manager.mu.Unlock()
	defer func() {
		manager.mu.Lock()
		manager.releaseHLSRestartSessionLocked(request.PlaySessionID, request.DeviceID, restartSessionKey)
		manager.mu.Unlock()
	}()

	manager.mu.Lock()
	lock := manager.hlsLocks[spec.playlist]
	if lock == nil {
		lock = &sync.Mutex{}
		manager.hlsLocks[spec.playlist] = lock
	}
	manager.hlsLockUsers[spec.playlist]++
	manager.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	defer manager.releaseHLSRestartLock(spec.playlist)

	manager.mu.Lock()
	stopped := manager.hlsSessionEpoch[restartSessionKey] != restartSessionEpoch
	manager.mu.Unlock()
	if stopped {
		return errors.New("HLS play session stopped during restart")
	}
	manager.mu.Lock()
	job := manager.jobs[spec.playlist]
	waitForRequestedProgress := job != nil && hlsJobConflicts(job, spec)
	if job != nil && !waitForRequestedProgress {
		claimAdoptedLocked(job, spec)
	}
	manager.mu.Unlock()
	if job != nil {
		state, waitErr := job.result()
		manager.mu.Lock()
		pending := job.reapPending || job.cleanupPending
		manager.mu.Unlock()
		if pending || state == transcodeExited && waitErr != nil {
			if waitErr != nil {
				return job.failureError(waitErr)
			}
			return fmt.Errorf("HLS playlist %q cleanup is pending", spec.playlist)
		}
	}
	if !waitForRequestedProgress {
		if cached, cacheErr := manager.hasHLSSegment(job, index); cacheErr != nil {
			return cacheErr
		} else if cached {
			return manager.confirmHLSRestartSession(restartSessionKey, restartSessionEpoch)
		}
	}
	restart := job == nil || waitForRequestedProgress
	if job != nil && !restart {
		state, _ := job.result()
		current, found, scanErr := job.hls.currentIndex(manager)
		if scanErr != nil {
			return scanErr
		}
		limit := 24 / job.hls.segmentLength
		if spec.maxForwardGap > 0 {
			limit = spec.maxForwardGap
		}
		if index < 0 {
			// A running job writes the initialization segment before its
			// first media segment, whatever its start number. A seek
			// restart removes the family before the replacement writes it,
			// so a missing file must not restart the job at segment 0.
			restart = state == transcodeExited
		} else {
			restart = state == transcodeExited || !found || index < current || index-current > limit
		}
	}
	if !restart {
		return manager.confirmHLSRestartSession(restartSessionKey, restartSessionEpoch)
	}
	replacementOf := (*transcodeJob)(nil)
	if job != nil {
		state, waitErr := job.result()
		if state == transcodeExited {
			manager.mu.Lock()
			pending := job.reapPending || job.cleanupPending
			manager.mu.Unlock()
			if pending {
				if waitErr != nil {
					return job.failureError(waitErr)
				}
				return fmt.Errorf("HLS playlist %q cleanup is pending", spec.playlist)
			}
			if waitErr != nil {
				return job.failureError(waitErr)
			}
			replacementOf = job
		} else {
			go job.stopOnce.Do(job.stop)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-job.waitDone:
			}
		}
	}
	if err := manager.confirmHLSRestartSession(restartSessionKey, restartSessionEpoch); err != nil {
		return err
	}
	if manager.beforeHLSRestartRegister != nil {
		manager.beforeHLSRestartRegister()
	}
	if err := manager.startHLS(ctx, request, replacementOf, restartSessionKey, restartSessionEpoch, false); err != nil {
		return err
	}
	if !waitForRequestedProgress {
		return nil
	}
	manager.mu.Lock()
	replacement := manager.jobs[spec.playlist]
	manager.mu.Unlock()
	if replacement == nil || replacement.hls == nil {
		return ErrTranscodeExited
	}
	return manager.waitForHLSArtifact(ctx, replacement, index)
}

func (manager *TranscodeManager) confirmHLSRestartSession(key string, epoch uint64) error {
	if manager.beforeHLSRestartStart != nil {
		manager.beforeHLSRestartStart()
	}
	manager.mu.Lock()
	stopped := manager.hlsSessionEpoch[key] != epoch
	manager.mu.Unlock()
	if stopped {
		return errors.New("HLS play session stopped during restart")
	}
	return nil
}

func (manager *TranscodeManager) hasHLSSegment(job *transcodeJob, index int) (bool, error) {
	if job == nil || job.hls == nil {
		return false, nil
	}
	path, err := job.hls.segmentPath(index)
	if err != nil {
		return false, err
	}
	file, err := manager.openExistingRegular(path)
	if err == nil {
		return true, file.Close()
	}
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	return false, err
}

func (manager *TranscodeManager) releaseHLSRestartLock(playlist string) {
	manager.mu.Lock()
	manager.hlsLockUsers[playlist]--
	manager.deleteHLSLockIfRetiredLocked(playlist)
	manager.mu.Unlock()
}

func (manager *TranscodeManager) waitForHLSArtifact(ctx context.Context, job *transcodeJob, index int) error {
	manager.retainHLSWaiter(job)
	defer manager.release(job)
	segment, err := job.hls.segmentPath(index)
	if err != nil {
		return err
	}
	for {
		file, openErr := manager.openExistingRegular(segment)
		if openErr == nil {
			return file.Close()
		}
		if !errors.Is(openErr, unix.ENOENT) {
			return openErr
		}
		if isClosed(job.waitDone) {
			_, waitErr := job.result()
			if waitErr != nil {
				return job.failureError(waitErr)
			}
			return ErrTranscodeExited
		}
		timer := manager.options.Clock.NewTimer(manager.options.OutputPollInterval)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return ctx.Err()
		case <-job.waitDone:
			stopTimer(timer)
		case <-timer.C():
		}
	}
}

func (manager *TranscodeManager) PingPlaySession(playSessionID string) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	jobs := manager.hlsSessions[playSessionID]
	found := false
	for job := range jobs {
		if job.state == transcodeRunning {
			manager.resetHLSIdleLocked(job)
			found = true
		}
	}
	return found
}

// StopPlaySession stops exactly the requested opaque play-session/device pair.
func (manager *TranscodeManager) StopPlaySession(ctx context.Context, playSessionID, deviceID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := hlsSessionKey(playSessionID, deviceID)
	manager.mu.Lock()
	manager.hlsSessionEpoch[key]++
	jobs := make([]*transcodeJob, 0)
	for job := range manager.hlsSessions[playSessionID] {
		if job.hls.deviceID == deviceID {
			jobs = append(jobs, job)
		}
	}
	manager.mu.Unlock()
	alreadyExited := make(map[*transcodeJob]bool, len(jobs))
	for _, job := range jobs {
		state, _ := job.result()
		alreadyExited[job] = state == transcodeExited
		if state != transcodeExited {
			go job.stopOnce.Do(job.stop)
		}
	}
	for _, job := range jobs {
		if err := manager.finishHLSFamilyStop(ctx, job, alreadyExited[job]); err != nil {
			return err
		}
	}
	manager.mu.Lock()
	manager.deleteHLSSessionEpochIfRetiredLocked(playSessionID, deviceID)
	manager.mu.Unlock()
	return nil
}

// finishHLSFamilyStop waits for an already-requested family stop and retires
// any retained completed artifacts. Callers must request the stop first.
func (manager *TranscodeManager) finishHLSFamilyStop(ctx context.Context, job *transcodeJob, alreadyExited bool) error {
	if alreadyExited {
		manager.mu.Lock()
		pending := job.cleanupPending || job.reapPending
		manager.mu.Unlock()
		if pending {
			if err := manager.retryPendingJob(ctx, job); err != nil {
				return err
			}
		}
	}
	state, _ := job.result()
	if state != transcodeExited {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-job.waitDone:
		}
	}
	state, waitErr := job.result()
	manager.mu.Lock()
	pendingFailure := job.cleanupPending || job.reapPending
	manager.mu.Unlock()
	if pendingFailure && waitErr != nil {
		return job.failureError(waitErr)
	}
	if state != transcodeExited {
		return errors.New("HLS play session did not stop")
	}
	if !job.hls.retain {
		return nil
	}
	if err := manager.removeArtifacts(job); err != nil {
		manager.mu.Lock()
		if manager.jobs[job.output] == job {
			job.cleanupPending = true
			job.cleanupErr = fmt.Errorf("remove completed HLS artifacts: %w", err)
			manager.shutdownErr = errors.Join(manager.shutdownErr, job.cleanupErr)
		}
		manager.mu.Unlock()
		return err
	}
	manager.mu.Lock()
	if manager.jobs[job.output] == job {
		job.hls.retain = false
		manager.closeJobSourceLocked(job)
		delete(manager.jobs, job.output)
		manager.unregisterHLSSessionLocked(job)
		manager.deleteHLSLockIfRetiredLocked(job.output)
	}
	manager.mu.Unlock()
	return nil
}

func hlsSessionKey(playSessionID, deviceID string) string {
	return strconv.Itoa(len(playSessionID)) + ":" + playSessionID + strconv.Itoa(len(deviceID)) + ":" + deviceID
}

func (manager *TranscodeManager) beginHLSRestartLocked(playSessionID, deviceID string) (string, uint64) {
	key := hlsSessionKey(playSessionID, deviceID)
	manager.hlsSessionTransitions[key]++
	return key, manager.hlsSessionEpoch[key]
}

func (manager *TranscodeManager) releaseHLSRestartSessionLocked(playSessionID, deviceID, key string) {
	if manager.hlsSessionTransitions[key] > 1 {
		manager.hlsSessionTransitions[key]--
	} else {
		delete(manager.hlsSessionTransitions, key)
	}
	manager.deleteHLSSessionEpochIfRetiredLocked(playSessionID, deviceID)
}

func (manager *TranscodeManager) deleteHLSSessionEpochIfRetiredLocked(playSessionID, deviceID string) {
	key := hlsSessionKey(playSessionID, deviceID)
	if manager.hlsSessionTransitions[key] != 0 {
		return
	}
	for job := range manager.hlsSessions[playSessionID] {
		if job.hls.deviceID == deviceID {
			return
		}
	}
	delete(manager.hlsSessionEpoch, key)
}

func (manager *TranscodeManager) registerHLSSessionLocked(job *transcodeJob) {
	if job.hls == nil {
		return
	}
	jobs := manager.hlsSessions[job.hls.playSessionID]
	if jobs == nil {
		jobs = make(map[*transcodeJob]struct{})
		manager.hlsSessions[job.hls.playSessionID] = jobs
	}
	jobs[job] = struct{}{}
}

func (manager *TranscodeManager) unregisterHLSSessionLocked(job *transcodeJob) {
	if job.hls == nil {
		return
	}
	playSessionID, deviceID := job.hls.playSessionID, job.hls.deviceID
	jobs := manager.hlsSessions[playSessionID]
	delete(jobs, job)
	if len(jobs) == 0 {
		delete(manager.hlsSessions, playSessionID)
	}
	manager.deleteHLSSessionEpochIfRetiredLocked(playSessionID, deviceID)
}

func (manager *TranscodeManager) resetHLSIdleLocked(job *transcodeJob) {
	manager.cancelIdleLocked(job)
	manager.armIdleLocked(job)
}

// segmentEnd returns the media end of output file index.
func (job *hlsJob) segmentEnd(index int) int64 {
	if len(job.segmentStarts) == 0 || index < 0 {
		return nominalHLSSegmentEnd(index, job.segmentLength)
	}
	if index+1 < len(job.segmentStarts) {
		return job.segmentStarts[index+1]
	}
	return max(job.runTimeTicks, job.segmentStarts[len(job.segmentStarts)-1])
}

// guardSegmentTicks is the segment length that the cache pause guard keeps
// written ahead of the player twice. A keyframe-cut segment joins whole GOPs,
// so it can be one GOP longer than the requested length.
func (job *hlsJob) guardSegmentTicks() int64 {
	ticks := int64(job.segmentLength) * int64(time.Second/(100*time.Nanosecond))
	longest := int64(0)
	for index := 1; index < len(job.segmentStarts); index++ {
		longest = max(longest, job.segmentStarts[index]-job.segmentStarts[index-1])
	}
	return saturatingTickAdd(ticks, longest)
}

func (job *hlsJob) segmentPath(index int) (string, error) {
	return formatHLSSegment(job.pattern, index)
}

func (job *hlsJob) matches(name string) (int, bool) {
	match := job.matcher.FindStringSubmatch(name)
	if match == nil {
		return 0, false
	}
	index, err := strconv.Atoi(match[1])
	return index, err == nil
}

func (job *hlsJob) currentIndex(manager *TranscodeManager) (int, bool, error) {
	path, err := job.segmentPath(0)
	if err != nil {
		return 0, false, err
	}
	parent, _, err := manager.openOutputParent(path)
	if errors.Is(err, unix.ENOENT) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	directory := os.NewFile(uintptr(parent), filepath.Dir(path))
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return 0, false, readErr
	}
	if closeErr != nil {
		return 0, false, closeErr
	}
	current, found := 0, false
	for _, entry := range entries {
		if index, match := job.matches(entry.Name()); match && (!found || index > current) {
			current, found = index, true
		}
	}
	return current, found, nil
}

func (manager *TranscodeManager) prepareHLSFamily(job *hlsJob) error {
	if err := manager.removeHLSFamily(job); err != nil {
		return err
	}
	parent, _, err := manager.openOrCreateOutputParent(job.playlist)
	if err != nil {
		return err
	}
	return unix.Close(parent)
}

func (manager *TranscodeManager) removeHLSFamily(job *hlsJob) error {
	for _, path := range []string{job.playlist, job.playlist + ".stderr.log"} {
		if err := manager.removeOutput(path); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	path, err := job.segmentPath(0)
	if err != nil {
		return err
	}
	parent, _, err := manager.openOutputParent(path)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(parent), filepath.Dir(path))
	entries, readErr := directory.ReadDir(-1)
	if readErr != nil {
		_ = directory.Close()
		return readErr
	}
	for _, entry := range entries {
		if _, match := job.matches(entry.Name()); match {
			if unlinkErr := unix.Unlinkat(parent, entry.Name(), 0); unlinkErr != nil && !errors.Is(unlinkErr, unix.ENOENT) {
				_ = directory.Close()
				return unlinkErr
			}
		}
	}
	return directory.Close()
}

func isClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
