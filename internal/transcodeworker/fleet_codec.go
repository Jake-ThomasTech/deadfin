// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
	"github.com/deadfin-org/deadfin/internal/store"
)

// Fleet codec choice (#854): with a placement mode other than local, the
// HLS output codec can be one that only a worker encodes. The job is then
// remote-only: it goes to such a worker or waits for one, and fails after
// the queue timeout. The next request chooses again from the fleet as it
// is then.

// RemoteVideoEncoder returns a ready worker that can encode
// facts.TargetCodec with options, in the pools where the policy sends a job
// of facts. It returns false in local mode and when no such worker is
// connected. sourcePath is the server path of the source, for the workers
// that read it from a shared path.
func (hub *Hub) RemoteVideoEncoder(facts JobFacts, sourcePath string, options encoding.HardwareOptions) (ffmpeg.RemoteEncoder, bool) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed || hub.policy.Mode == ModeLocal || hub.policy.Mode == "" {
		return ffmpeg.RemoteEncoder{}, false
	}
	pools := hub.policy.DefaultPools
	if rule, ok := matchRule(facts, hub.policy.Rules, true); ok && len(rule.Pools) > 0 {
		pools = rule.Pools
	}
	now := hub.config.Clock.Now()
	var best *workerConn
	for _, conn := range hub.workers {
		if conn.caps.Encoding == nil || !inPools(conn.pools, pools) || hub.stateLocked(conn, now) != WorkerReady {
			continue
		}
		if conn.inputMode == store.TranscodeWorkerInputShared && !coversPath(conn.caps.PathPrefixes, sourcePath) {
			continue
		}
		if !encoding.CanEncodeVideo(facts.TargetCodec, *conn.caps.Encoding, options.ForHost(hostOf(conn.caps))) {
			continue
		}
		if best == nil || preferEncoder(conn, best) {
			best = conn
		}
	}
	if best == nil {
		return ffmpeg.RemoteEncoder{}, false
	}
	return ffmpeg.RemoteEncoder{WorkerID: best.id, Capabilities: *best.caps.Encoding, Host: hostOf(best.caps)}, true
}

// preferEncoder orders the workers that can encode a codec: a free slot
// first, then the oldest registration, then the ID.
func preferEncoder(left, right *workerConn) bool {
	leftFree, rightFree := len(left.jobs) < left.caps.MaxJobs, len(right.jobs) < right.caps.MaxJobs
	if leftFree != rightFree {
		return leftFree
	}
	if !left.since.Equal(right.since) {
		return left.since.Before(right.since)
	}
	return left.id < right.id
}

func hostOf(caps Capabilities) encoding.HostHardware {
	if caps.Hardware == nil {
		return encoding.HostHardware{}
	}
	return *caps.Hardware
}

// RemotePlacement reports a placement mode other than local.
func (hub *Hub) RemotePlacement() bool {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return !hub.closed && hub.policy.Mode != ModeLocal && hub.policy.Mode != ""
}
