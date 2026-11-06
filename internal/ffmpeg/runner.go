// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/deadfin-org/deadfin/internal/encoding"
)

// RemoteRunner places one HLS ffmpeg family on another host. It is the
// transcode-worker seam (#614, docs/spikes/transcode-workers.md). The
// manager keeps the playback decision, the HLS contract, the readiness rule
// and the restart policy; only the ffmpeg process moves. A nil runner keeps
// every job local.
type RemoteRunner interface {
	// StartHLS starts job on a remote host. ErrRemoteRefused and a canceled
	// start fail the job; any other error makes the manager run the job
	// locally, and ErrNoRemoteRunner is the expected "run it here" case.
	// ctx ends when every request that waits for the start gives up.
	StartHLS(ctx context.Context, job RemoteHLSJob) (RemoteProcess, error)
}

// RemoteProgressiveRunner is a RemoteRunner that also places progressive
// transcodes (#624). The remote host streams the output back and the runner
// appends it to Output on the server, so the manager serves it as the
// output of a local ffmpeg. StartProgressive follows the StartHLS error
// contract.
type RemoteProgressiveRunner interface {
	StartProgressive(ctx context.Context, job RemoteProgressiveJob) (RemoteProcess, error)
}

// RemoteProgressiveJob is the complete input a remote host needs for one
// progressive transcode.
type RemoteProgressiveJob struct {
	// Args is the full ffmpeg argv without the executable. The source
	// operand is "file:" + SourcePath; the last operand is Output.
	Args []string
	// SourcePath, Source and SourceSize are as in RemoteHLSJob.
	SourcePath string
	Source     *os.File
	SourceSize int64
	// Output is the absolute output path on the server.
	Output        string
	PlaySessionID string
	DeviceID      string
	// LocalJobs is the number of ffmpeg families running on the server.
	LocalJobs int
	// Stderr receives ffmpeg's stderr. The runner closes it after the
	// remote process ends and before Wait returns.
	Stderr io.WriteCloser
}

// RemoteHLSJob is the complete input a remote host needs for one HLS family.
type RemoteHLSJob struct {
	// Args is the full ffmpeg argv without the executable. The source
	// operand is "file:" + SourcePath; the output operands are absolute
	// paths under Directory.
	Args []string
	// SourcePath is the server path of the media source.
	SourcePath string
	// Source is the pinned source descriptor. It is borrowed until Wait
	// returns. Read it only with ReadAt: the offset is shared.
	Source *os.File
	// SourceSize is the size of Source in bytes.
	SourceSize int64
	// Directory holds the playlist family on the server.
	Directory string
	// Playlist and SegmentPattern are the absolute output operands.
	Playlist       string
	SegmentPattern string
	PlaySessionID  string
	DeviceID       string
	// SegmentLength is the HLS segment length in seconds.
	SegmentLength int
	// Facts are the playback facts that placement rules match (#616).
	Facts JobFacts
	// Build returns the argv of this job for a host with caps and host
	// hardware (#622), in the form of Args: same source operand, same
	// output operands. Nil means Args is the only argv.
	Build ArgsBuilder
	// LocalJobs is the number of ffmpeg families running on the server.
	LocalJobs int
	// RemoteOnly marks a job whose output codec only a transcode worker
	// encodes (#854). Args is built for that worker; the server cannot run
	// it, so placement never falls back to a local encode.
	RemoteOnly bool
	// Assets are the files of a subtitle burn-in that the remote host
	// needs (#624). Args names them under RemoteAssetsPlaceholder.
	Assets []RemoteAsset
	// Stderr receives ffmpeg's stderr. The runner closes it after the
	// remote process ends and before Wait returns.
	Stderr io.WriteCloser
}

// RemoteEncoder is a remote host that can encode an output codec that the
// server cannot (#854). Capabilities and Host build the argv for it.
type RemoteEncoder struct {
	WorkerID     string
	Capabilities encoding.Capabilities
	Host         encoding.HostHardware
}

// RemoteAssetsPlaceholder stands for the job asset directory of the remote
// host in RemoteHLSJob.Args (#624). The host replaces it with its directory,
// escaped for a quoted subtitles filter option.
const RemoteAssetsPlaceholder = "{{deadfin-assets}}"

// RemoteAsset is one file that a remote host fetches before it starts
// ffmpeg: the subtitle of a burn-in or one of its fonts.
type RemoteAsset struct {
	// Name is "subtitle.<ext>", which the argv names as
	// RemoteAssetsPlaceholder + "/subtitle.<ext>", or "font<NNN>.<ext>",
	// which belongs in the fontsdir RemoteAssetsPlaceholder + "/fonts".
	Name string
	// File is borrowed until Wait returns. Read it only with ReadAt.
	File *os.File
	Size int64
}

// ArgsBuilder builds the ffmpeg argv of one HLS family for the encoding
// capabilities and the hardware of the host that runs it (#622: place
// first, then build the argv).
type ArgsBuilder func(caps encoding.Capabilities, host encoding.HostHardware) ([]string, error)

// RemoteProcess is one remote ffmpeg invocation.
type RemoteProcess interface {
	// Stdin forwards the throttle and quit keystrokes ("p", "u", "q\n").
	Stdin() io.Writer
	// Wait blocks until the remote process ends and no further artifact
	// write can reach Directory. ErrRunnerLost means the host vanished
	// mid-stream; the completed segments stay valid.
	Wait() error
	// Kill stops the process now. Wait still reports the end.
	Kill()
}

// RemoteReleaser is a RemoteProcess whose host can keep running the job
// while the server restarts (#618). Release ends the process for this
// manager with ErrRunnerReleased and keeps it running on the host; the next
// server process adopts it (TranscodeManager.AdoptHLS). Release reports
// false when the host cannot keep the job; the manager then stops it.
type RemoteReleaser interface {
	Release() bool
}

// JobFacts are the facts of the playback decision that placement rules
// match (docs/spikes/transcode-workers.md §3.8). They are known before the
// argv is built.
type JobFacts struct {
	// SourceCodec and SourceProfile are the source video stream codec and
	// its lower-case profile ("hevc", "main10").
	SourceCodec   string
	SourceProfile string
	BitDepth      int
	// HDR is the source range: "sdr", "hdr10", "hdr10plus", "hlg" or "dovi".
	HDR string
	// NeedsToneMap reports an HDR source with an SDR target.
	NeedsToneMap bool
	// TargetCodec is the output video codec, "copy" for a remux.
	TargetCodec string
	// Hardware is the server's hardware family in this argv; empty for a
	// software encode.
	Hardware string
}

var (
	// ErrRemoteRefused reports that placement refused the job and the
	// fallback chain ends without a local encode (cpuFallback off). The
	// manager fails the start; nothing runs.
	ErrRemoteRefused = errors.New("transcode placement refused the job")
	// ErrRemoteOnly reports a remote-only job (#854) that no remote host
	// started. The job cannot run locally, so the start fails.
	ErrRemoteOnly = errors.New("no transcode worker can encode the output codec")
	// ErrNoRemoteRunner reports that no remote host fits a job.
	ErrNoRemoteRunner = errors.New("no remote transcode runner fits")
	// ErrRunnerLost reports a remote host that vanished mid-stream. The
	// HLS layer restarts the family at the requested segment.
	ErrRunnerLost = errors.New("remote transcode runner lost")
	// ErrRunnerReleased reports a remote job that the host keeps running
	// across a server restart (RemoteReleaser). Its artifacts stay.
	ErrRunnerReleased = errors.New("remote transcode job released to its host")
)

// process is the manager's view of one running ffmpeg family leader.
type process interface {
	wait() error
	kill()
	// reap confirms that nothing of the process survives. It may be
	// retried after an error.
	reap() error
}

type localProcess struct {
	manager *TranscodeManager
	cmd     *exec.Cmd
	hls     bool
}

func (p localProcess) wait() error {
	err := p.cmd.Wait()
	if errors.Is(err, exec.ErrWaitDelay) && p.hls && p.cmd.ProcessState != nil && p.cmd.ProcessState.Success() {
		return nil
	}
	return err
}

func (p localProcess) kill() {
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
}

func (p localProcess) reap() error {
	reaper := p.manager.reapProcessGroup
	if reaper == nil {
		reaper = reapProcessGroup
	}
	return reaper(p.cmd.Process.Pid)
}

type remoteProcess struct {
	remote RemoteProcess
}

func (p remoteProcess) wait() error { return p.remote.Wait() }
func (p remoteProcess) kill()       { p.remote.Kill() }

// reap has nothing to do: the remote host owns its process group, and Wait
// already guarantees that no artifact write can follow.
func (p remoteProcess) reap() error { return nil }

type remoteStdin struct {
	writer io.Writer
}

func (s remoteStdin) Write(data []byte) (int, error) { return s.writer.Write(data) }
func (remoteStdin) Close() error                     { return nil }
