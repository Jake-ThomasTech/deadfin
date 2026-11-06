// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// KeptFamily names the files of one remote HLS family that the startup wipe
// keeps (#618): the playlist and every file in its directory that starts with
// the playlist stem or the segment prefix.
type KeptFamily struct {
	Playlist       string
	SegmentPattern string
}

// clearTranscodes empties directory, except the files of keep. Without keep
// it removes the directory, as upstream clears its transcode path on start.
func clearTranscodes(directory string, keep []KeptFamily) error {
	if len(keep) == 0 {
		return os.RemoveAll(directory)
	}
	root, err := filepath.EvalSymlinks(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type family struct{ stem, prefix string }
	families := make(map[string][]family)
	ancestors := map[string]bool{root: true}
	for _, kept := range keep {
		parent := filepath.Dir(kept.Playlist)
		relative, err := filepath.Rel(root, parent)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.Dir(kept.SegmentPattern) != parent {
			continue
		}
		base := filepath.Base(kept.Playlist)
		prefix := filepath.Base(kept.SegmentPattern)
		if cut := strings.IndexByte(prefix, '%'); cut >= 0 {
			prefix = prefix[:cut]
		}
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		if stem == "" || prefix == "" {
			continue
		}
		families[parent] = append(families[parent], family{stem: stem, prefix: prefix})
		for current := parent; current != root && !ancestors[current]; current = filepath.Dir(current) {
			ancestors[current] = true
		}
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if entry.IsDir() {
			if ancestors[path] {
				return nil
			}
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			return filepath.SkipDir
		}
		name := entry.Name()
		if !strings.HasPrefix(name, ".") {
			for _, kept := range families[filepath.Dir(path)] {
				if strings.HasPrefix(name, kept.stem) || strings.HasPrefix(name, kept.prefix) {
					return nil
				}
			}
		}
		return os.Remove(path)
	})
}

// AdoptHLSRequest names a remote HLS family that kept running on its host
// across a server restart (#618).
type AdoptHLSRequest struct {
	PlaylistPath         string
	SegmentPattern       string
	PlaySessionID        string
	DeviceID             string
	SegmentLengthSeconds int
	// Remote binds the running remote process. The runner writes its
	// stderr to stderr and closes stderr after the process ends.
	Remote func(stderr io.WriteCloser) RemoteProcess
}

// AdoptHLS registers a running remote family. The family has no rendition
// identity until the first request for its playlist joins it; that request
// supplies the identity, the source authority and the handoff. The idle
// timer, the throttler and the segment cleaner start again here.
func (manager *TranscodeManager) AdoptHLS(ctx context.Context, request AdoptHLSRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.Remote == nil {
		return errors.New("adopted HLS family has no remote process")
	}
	if request.SegmentLengthSeconds <= 0 {
		return errors.New("HLS segment length must be positive")
	}
	spec, err := manager.hlsFamily(request.PlaylistPath, request.SegmentPattern)
	if err != nil {
		return err
	}
	spec.playSessionID = request.PlaySessionID
	spec.deviceID = request.DeviceID
	spec.segmentLength = request.SegmentLengthSeconds
	spec.adopted = true
	job := &transcodeJob{
		manager:     manager,
		output:      spec.playlist,
		startDone:   make(chan struct{}),
		waitDone:    make(chan struct{}),
		state:       transcodeStarting,
		idleTimeout: hlsIdleTimeout,
		hls:         spec,
	}
	sidecars, ok := manager.openRemoteSidecars(job)
	if !ok {
		return fmt.Errorf("open the sidecars of adopted HLS family %q", spec.playlist)
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		sidecars.discard()
		return ErrManagerClosed
	}
	if _, exists := manager.jobs[spec.playlist]; exists {
		manager.mu.Unlock()
		sidecars.discard()
		return fmt.Errorf("HLS playlist %q already has a job", spec.playlist)
	}
	remote := request.Remote(sidecars.destination)
	manager.jobs[spec.playlist] = job
	manager.registerHLSSessionLocked(job)
	manager.attachRemoteLocked(job, remote, sidecars)
	manager.addWorkerLocked()
	close(job.startDone)
	manager.armIdleLocked(job)
	manager.mu.Unlock()
	go manager.wait(job)
	return nil
}

// claimAdoptedLocked gives an adopted family the rendition of the first
// request that joins it.
func claimAdoptedLocked(job *transcodeJob, spec *hlsJob) {
	adopted := job.hls
	if adopted == nil || !adopted.adopted {
		return
	}
	adopted.adopted = false
	adopted.live = spec.live
	adopted.minSegments = spec.minSegments
	adopted.identity = spec.identity
	adopted.facts = spec.facts
	adopted.buildArgs = spec.buildArgs
	adopted.handoff = spec.handoff
	adopted.maxForwardGap = spec.maxForwardGap
	adopted.segmentStarts = spec.segmentStarts
	adopted.runTimeTicks = spec.runTimeTicks
	adopted.startTimeTicks = spec.startTimeTicks
	adopted.mediaSourceID = spec.mediaSourceID
	adopted.sourceRevision = spec.sourceRevision
	adopted.openSource = spec.openSource
	adopted.subtitleSource = spec.subtitleSource
	adopted.externalSubtitleSource = spec.externalSubtitleSource
	adopted.externalSubtitleRevision = spec.externalSubtitleRevision
	adopted.openExternalSubtitle = spec.openExternalSubtitle
	adopted.fontsDir = spec.fontsDir
	adopted.local = spec.local
	adopted.transcodingInfo = spec.transcodingInfo
}

// releaseRemote hands a running remote family to its host at shutdown
// (#618). It reports false when the family is local or the host cannot keep
// it; the caller then stops it.
func (manager *TranscodeManager) releaseRemote(job *transcodeJob) bool {
	manager.mu.Lock()
	remote, isRemote := job.proc.(remoteProcess)
	running := job.state == transcodeRunning && job.hls != nil
	manager.mu.Unlock()
	if !isRemote || !running {
		return false
	}
	releaser, ok := remote.remote.(RemoteReleaser)
	return ok && releaser.Release()
}
