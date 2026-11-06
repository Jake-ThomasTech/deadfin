// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/deadfin-org/deadfin/internal/ffmpeg"
	"github.com/deadfin-org/deadfin/internal/store"
)

// Job leases (#618): a worker keeps its jobs running while it has no server
// connection, for the lease. The server stores a record of each job at
// placement and renews its lease while the worker is connected. At shutdown
// the transcode manager releases its remote jobs (ffmpeg.RemoteReleaser):
// their records and files stay. When the worker reconnects to the next
// server process, it lists its jobs (jobs.held), and the server adopts each
// job that has a live record of that worker, or kills it.

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// startNumber is the -start_number operand of an HLS argv.
func startNumber(args []string) int {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "-start_number" {
			if value, err := strconv.Atoi(args[index+1]); err == nil {
				return value
			}
		}
	}
	return 0
}

// createRecord stores the record of a placed job and returns the lease the
// worker gets. Without a record the lease is zero.
func (hub *Hub) createRecord(job *remoteJob, conn *workerConn, request ffmpeg.RemoteHLSJob, inputMode string) (lease int64) {
	if hub.config.Jobs == nil || request.SegmentLength <= 0 {
		return 0
	}
	hub.mu.Lock()
	workerID := conn.id
	hub.mu.Unlock()
	now := hub.config.Clock.Now()
	expires := now.Add(hub.config.Lease)
	record := store.TranscodeWorkerJob{
		ID: job.id, WorkerID: workerID, Playlist: request.Playlist, SegmentPattern: request.SegmentPattern,
		PlaySessionID: request.PlaySessionID, DeviceID: request.DeviceID, TokenHash: job.tokenHash,
		SegmentLength: request.SegmentLength, StartNumber: startNumber(request.Args), InputMode: inputMode,
		SourcePath: request.SourcePath, SourceSize: request.SourceSize, Created: now, LeaseExpires: expires,
		Owner: hub.config.Replica,
	}
	ctx, cancel := context.WithTimeout(context.Background(), jobStoreTimeout)
	defer cancel()
	if err := hub.config.Jobs.CreateJob(ctx, record); err != nil {
		hub.logger().Warn("store transcode worker job; the job stops with its connection", "job", job.id, "error", err.Error())
		return 0
	}
	hub.mu.Lock()
	job.record = true
	job.leaseExpires = expires
	hub.mu.Unlock()
	return hub.config.Lease.Milliseconds()
}

func (hub *Hub) deleteRecord(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), jobStoreTimeout)
	defer cancel()
	if err := hub.config.Jobs.DeleteJob(ctx, id); err != nil {
		hub.logger().Warn("delete transcode worker job record", "job", id, "error", err.Error())
	}
}

// leased reports a job whose lease the hub renews. The caller holds hub.mu.
func (job *remoteJob) leased() bool {
	return job.record && !job.leaseExpires.IsZero()
}

// Release implements ffmpeg.RemoteReleaser: the worker keeps the job for
// the next server process. The record gets a full lease, and the job ends
// here without job.kill.
func (job *remoteJob) Release() bool {
	hub := job.hub
	hub.mu.Lock()
	leased := job.leased()
	conn := job.conn
	hub.mu.Unlock()
	if !leased || conn == nil {
		return false
	}
	expires := hub.config.Clock.Now().Add(hub.config.Lease)
	ctx, cancel := context.WithTimeout(context.Background(), jobStoreTimeout)
	defer cancel()
	if err := hub.config.Jobs.RenewJobs(ctx, []string{job.id}, expires); err != nil {
		hub.logger().Warn("renew a released transcode worker job; stopping it", "job", job.id, "error", err.Error())
		return false
	}
	hub.mu.Lock()
	job.leaseExpires = expires
	if conn.jobs[job.id] == job {
		delete(conn.jobs, job.id)
	}
	hub.mu.Unlock()
	return job.finish(ffmpeg.ErrRunnerReleased)
}

// reattach answers the jobs a reconnected worker kept running.
func (hub *Hub) reattach(conn *workerConn, held []HeldJob) {
	if len(held) > maxWorkerJobs {
		held = held[:maxWorkerJobs]
	}
	for _, entry := range held {
		if err := hub.adoptHeld(conn, entry); err != nil {
			_ = conn.enqueue(Message{Type: TypeJobKill, JobID: entry.JobID})
			hub.observe(Event{Kind: EventJobRefused, WorkerID: conn.id, Name: conn.name, JobID: entry.JobID, Detail: err.Error()})
			hub.logger().Info("transcode worker job not adopted; killing it", "worker", conn.name, "job", entry.JobID, "reason", err.Error())
			continue
		}
		_ = conn.enqueue(Message{Type: TypeJobAdopted, JobID: entry.JobID, LeaseMillis: hub.config.Lease.Milliseconds()})
		hub.observe(Event{Kind: EventJobAdopted, WorkerID: conn.id, Name: conn.name, JobID: entry.JobID})
		hub.logger().Info("transcode worker job adopted after a server restart", "worker", conn.name, "job", entry.JobID, "next_segment", entry.NextSegment)
	}
}

// adoptHeld adopts one held job: its record must be live and name this
// worker, and in replica mode this replica must claim it.
func (hub *Hub) adoptHeld(conn *workerConn, held HeldJob) error {
	if hub.config.Jobs == nil {
		return errors.New("the server keeps no job records")
	}
	hub.mu.Lock()
	adopter := hub.adopter
	_, running := hub.jobs[held.JobID]
	workerID, closed := conn.id, hub.closed
	hub.mu.Unlock()
	switch {
	case closed:
		return errors.New("the server is closing")
	case running:
		return errors.New("the job is already running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), jobStoreTimeout)
	defer cancel()
	record, err := hub.config.Jobs.Job(ctx, held.JobID)
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("the job has no record")
	}
	if err != nil {
		return fmt.Errorf("read the job record: %w", err)
	}
	now := hub.config.Clock.Now()
	if record.WorkerID != workerID {
		return errors.New("the job record names another worker")
	}
	if !now.Before(record.LeaseExpires) {
		hub.deleteRecord(record.ID)
		return errors.New("the job lease is over")
	}
	if hub.config.Replica != "" && hub.config.ClaimJob != nil {
		// Another live replica may still run the job (#652).
		claimed, err := hub.config.ClaimJob(ctx, record.ID)
		if err != nil {
			return fmt.Errorf("claim the job record: %w", err)
		}
		if !claimed {
			return errors.New("another live server replica owns the job")
		}
	}
	if adopter == nil {
		return errors.New("no transcode manager adopts jobs")
	}
	pattern := filepath.Base(record.SegmentPattern)
	match := segmentDirective.FindStringIndex(pattern)
	if match == nil || pattern[match[0]:match[1]] != "%d" || filepath.Dir(record.SegmentPattern) != filepath.Dir(record.Playlist) {
		return errors.New("the job record has no usable segment pattern")
	}
	sourceCtx, cancelSource := context.WithCancel(context.Background())
	job := &remoteJob{
		hub:          hub,
		id:           record.ID,
		tokenHash:    record.TokenHash,
		directory:    filepath.Dir(record.Playlist),
		playlist:     filepath.Base(record.Playlist),
		prefix:       pattern[:match[0]],
		suffix:       pattern[match[1]:],
		inputMode:    record.InputMode,
		sourceCtx:    sourceCtx,
		cancelSource: cancelSource,
		started:      make(chan error, 1),
		growing:      make(map[string]bool),
		done:         make(chan struct{}),
		running:      true,
		lastProgress: now,
	}
	for _, name := range held.Files {
		if !job.ownsName(name) {
			cancelSource()
			return fmt.Errorf("held file %q is not part of the job family", name)
		}
	}
	if record.InputMode != store.TranscodeWorkerInputShared && record.SourcePath != "" {
		if file, err := os.Open(record.SourcePath); err == nil {
			if info, statErr := file.Stat(); statErr == nil && info.Size() == record.SourceSize {
				job.source, job.sourceSize, job.ownsSource = file, record.SourceSize, true
			} else {
				_ = file.Close()
			}
		}
		if job.source == nil && record.InputMode == store.TranscodeWorkerInputStream {
			cancelSource()
			return errors.New("the streamed source is gone")
		}
	}
	job.removeStaleSegments(held.NextSegment)

	hub.mu.Lock()
	if hub.closed {
		hub.mu.Unlock()
		job.finish(errConnClosed)
		return errors.New("the server is closing")
	}
	job.conn = conn
	job.record = true
	job.leaseExpires = record.LeaseExpires
	conn.jobs[job.id] = job
	hub.jobs[job.id] = job
	hub.mu.Unlock()
	err = adopter.AdoptHLS(ctx, ffmpeg.AdoptHLSRequest{
		PlaylistPath: record.Playlist, SegmentPattern: record.SegmentPattern,
		PlaySessionID: record.PlaySessionID, DeviceID: record.DeviceID, SegmentLengthSeconds: record.SegmentLength,
		Remote: func(stderr io.WriteCloser) ffmpeg.RemoteProcess {
			job.mu.Lock()
			job.stderr = stderr
			job.mu.Unlock()
			return job
		},
	})
	if err != nil {
		hub.mu.Lock()
		delete(conn.jobs, job.id)
		hub.mu.Unlock()
		job.finish(err)
		return fmt.Errorf("adopt: %w", err)
	}
	hub.renewLeases([]*remoteJob{job}, now)
	return nil
}

// removeStaleSegments removes the segment files at or past the worker's
// next segment, and interrupted uploads: a placeholder that a crashed
// server left must not look like a cached segment.
func (job *remoteJob) removeStaleSegments(next int) {
	entries, err := os.ReadDir(job.directory)
	if err != nil {
		return
	}
	upload := "." + job.prefix
	for _, entry := range entries {
		name := entry.Name()
		if index, ok := segmentIndex(name, job.prefix, job.suffix); ok && index >= next {
			_ = os.Remove(filepath.Join(job.directory, name))
			continue
		}
		if strings.HasPrefix(name, upload) && strings.Contains(name, ".upload-") {
			_ = os.Remove(filepath.Join(job.directory, name))
		}
	}
}
