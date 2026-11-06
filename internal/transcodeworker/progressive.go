// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// Progressive transcodes on workers (#624): ffmpeg writes one output file
// on the worker, the worker streams new bytes to PathOutput in chunks, and
// the hub appends them to the output on the server. The manager's
// progressive reader tails that file as for a local ffmpeg.

// OutputOffsetHeader carries the offset of a progressive output chunk. A
// 409 answer carries the size that the server has.
const OutputOffsetHeader = "Deadfin-Output-Offset"

// maxOutputChunk bounds one progressive output chunk.
const maxOutputChunk = 8 << 20

// serveOutput appends one chunk to the output of a progressive job.
func (hub *Hub) serveOutput(w http.ResponseWriter, r *http.Request) {
	job := hub.authorizedJob(r)
	if job == nil {
		hub.unauthorized(w, r)
		return
	}
	if job.output == "" {
		http.Error(w, "the job is not progressive", http.StatusBadRequest)
		return
	}
	offset, err := strconv.ParseInt(r.Header.Get(OutputOffsetHeader), 10, 64)
	if err != nil || offset < 0 {
		http.Error(w, "output offset is missing", http.StatusBadRequest)
		return
	}
	if !job.begin() {
		http.Error(w, "job has ended", http.StatusGone)
		return
	}
	defer job.inflight.Done()
	chunk, err := io.ReadAll(io.LimitReader(r.Body, maxOutputChunk+1))
	if err != nil {
		http.Error(w, "read output chunk", http.StatusBadRequest)
		return
	}
	if len(chunk) > maxOutputChunk {
		http.Error(w, "output chunk is too large", http.StatusRequestEntityTooLarge)
		return
	}
	job.mu.Lock()
	if offset != job.written {
		written := job.written
		job.mu.Unlock()
		w.Header().Set(OutputOffsetHeader, strconv.FormatInt(written, 10))
		http.Error(w, "output offset does not match", http.StatusConflict)
		return
	}
	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(filepath.Join(job.directory, job.output), flags, 0o644)
	if err == nil {
		_, err = file.WriteAt(chunk, offset)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	if err == nil {
		job.written += int64(len(chunk))
	}
	job.mu.Unlock()
	if err != nil {
		http.Error(w, "write output", http.StatusInternalServerError)
		return
	}
	now := hub.config.Clock.Now()
	job.progress(now)
	hub.mu.Lock()
	hub.metrics.uploadBytes += uint64(len(chunk))
	hub.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// outputUploader moves a job's output to the server: the HLS family
// uploader, or the progressive streamer.
type outputUploader interface {
	sync(ctx context.Context, exited bool) error
	mirror(ctx context.Context, exited bool)
	pending() int
	reattached()
}

// progressiveUploader streams the growing output of a progressive job.
type progressiveUploader struct {
	job  *agentJob
	path string
	sent int64
}

// maxChunksPerSync bounds the chunks one poll sends while ffmpeg runs, so
// the job loop stays responsive.
const maxChunksPerSync = 16

func (u *progressiveUploader) sync(ctx context.Context, exited bool) error {
	for chunks := 0; exited || chunks < maxChunksPerSync; chunks++ {
		info, err := os.Stat(u.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Size() <= u.sent {
			return nil
		}
		size := min(info.Size()-u.sent, maxOutputChunk)
		if err := u.send(ctx, size, info.Size()); err != nil {
			return err
		}
	}
	return nil
}

// send posts size bytes at u.sent. On a 409 it takes the server's size
// when the local file covers it.
func (u *progressiveUploader) send(ctx context.Context, size, available int64) error {
	file, err := os.Open(u.path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	agent := u.job.agent
	target := *agent.base
	target.Path += pathJobs + u.job.spec.ID + "/output"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), io.NewSectionReader(file, u.sent, size))
	if err != nil {
		return err
	}
	request.ContentLength = size
	request.Header.Set("Authorization", "Bearer "+u.job.spec.Token)
	request.Header.Set(OutputOffsetHeader, strconv.FormatInt(u.sent, 10))
	response, err := agent.client.Do(request)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	switch response.StatusCode {
	case http.StatusNoContent:
		u.sent += size
		return nil
	case http.StatusConflict:
		have, err := strconv.ParseInt(response.Header.Get(OutputOffsetHeader), 10, 64)
		if err != nil || have < 0 || have > available {
			return fmt.Errorf("server output size %q does not match the worker's output", response.Header.Get(OutputOffsetHeader))
		}
		u.sent = have
		return nil
	}
	return &uploadStatusError{name: u.job.spec.Output, status: response.Status, code: response.StatusCode}
}

func (u *progressiveUploader) mirror(context.Context, bool) {}
func (u *progressiveUploader) pending() int                 { return 0 }
func (u *progressiveUploader) reattached()                  {}
