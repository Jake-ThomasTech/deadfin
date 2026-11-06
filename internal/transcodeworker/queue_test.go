// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/ffmpeg"
)

type startResult struct {
	process ffmpeg.RemoteProcess
	err     error
}

func startAsync(ctx context.Context, hub *testHub, job ffmpeg.RemoteHLSJob) <-chan startResult {
	result := make(chan startResult, 1)
	go func() {
		process, err := hub.hub.StartHLS(ctx, job)
		result <- startResult{process, err}
	}()
	return result
}

func receiveStart(t *testing.T, result <-chan startResult) startResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(testTimeout):
		t.Fatal("StartHLS did not return")
		return startResult{}
	}
}

// stillWaiting yields to the start goroutine and fails if it returned.
func stillWaiting(t *testing.T, result <-chan startResult) {
	t.Helper()
	for range 100 {
		runtime.Gosched()
	}
	select {
	case got := <-result:
		t.Fatalf("StartHLS returned early: %+v", got)
	default:
	}
}

func TestQueuedJobTakesAWorkerThatRegistersBeforeTheTimeout(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersOnly}}, token)
	f := newFamily(t, "media")
	result := startAsync(context.Background(), hub, f.job())
	hub.waitEvent(t, EventJobQueued)
	hub.clock.Advance(DefaultQueueTimeout / 2)
	stillWaiting(t, result)

	worker := startTestAgent(t, hub, token, "late", Capabilities{MaxJobs: 1})
	process := worker.process(t)
	got := receiveStart(t, result)
	if got.err != nil {
		t.Fatalf("queued job: %v", got.err)
	}
	if hub.hub.QueueLength() != 0 {
		t.Fatal("the placed job stayed queued")
	}
	process.end(nil)
	if err := waitResult(t, got.process); err != nil {
		t.Fatal(err)
	}
}

func TestQueueTimeoutTakesTheFallbackChainExactlyAtTheTimeout(t *testing.T) {
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersOnly}})
	f := newFamily(t, "media")
	result := startAsync(context.Background(), hub, f.job())
	hub.waitEvent(t, EventJobQueued)
	hub.clock.Advance(DefaultQueueTimeout - time.Millisecond)
	stillWaiting(t, result)
	hub.clock.Advance(time.Millisecond)
	// No local GPU, cpuFallback on: the manager runs a software encode.
	if got := receiveStart(t, result); !errors.Is(got.err, ffmpeg.ErrNoRemoteRunner) {
		t.Fatalf("after the timeout: %v, want ErrNoRemoteRunner (local CPU)", got.err)
	}
	if event := hub.waitEvent(t, EventFallback); event.Detail != "local_cpu:queue-timeout" {
		t.Fatalf("fallback = %q", event.Detail)
	}
	if hub.hub.QueueLength() != 0 {
		t.Fatal("the timed-out job stayed queued")
	}
}

func TestQueueWithoutCPUFallbackRefusesTheJob(t *testing.T) {
	off := false
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersOnly, CPUFallback: &off}})
	f := newFamily(t, "media")
	result := startAsync(context.Background(), hub, f.job())
	hub.waitEvent(t, EventJobQueued)
	hub.clock.Advance(DefaultQueueTimeout)
	if got := receiveStart(t, result); !errors.Is(got.err, ffmpeg.ErrRemoteRefused) {
		t.Fatalf("after the timeout: %v, want ErrRemoteRefused", got.err)
	}
	if event := hub.waitEvent(t, EventFallback); event.Detail != "refused:queue-timeout" {
		t.Fatalf("fallback = %q", event.Detail)
	}
}

func TestCanceledRequestLeavesTheQueue(t *testing.T) {
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersOnly}})
	f := newFamily(t, "media")
	ctx, cancel := context.WithCancel(context.Background())
	result := startAsync(ctx, hub, f.job())
	hub.waitEvent(t, EventJobQueued)
	cancel()
	if got := receiveStart(t, result); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("canceled: %v, want context.Canceled", got.err)
	}
	if hub.hub.QueueLength() != 0 {
		t.Fatal("the canceled job stayed queued")
	}
}

func TestOldestQueuedJobTakesTheFreedSlot(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersOnly}}, token)
	worker := startTestAgent(t, hub, token, "w", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	first := newFamily(t, "first")
	running := startJob(t, hub, first.job())
	process := worker.process(t)

	older, newer := newFamily(t, "older"), newFamily(t, "newer")
	olderResult := startAsync(context.Background(), hub, older.job())
	olderID := hub.waitEvent(t, EventJobQueued).JobID
	newerResult := startAsync(context.Background(), hub, newer.job())
	hub.waitEvent(t, EventJobQueued)

	process.end(nil)
	if err := waitResult(t, running); err != nil {
		t.Fatal(err)
	}
	got := receiveStart(t, olderResult)
	if got.err != nil {
		t.Fatalf("older job: %v", got.err)
	}
	if placed := hub.waitEvent(t, EventJobPlaced); placed.JobID != olderID {
		t.Fatalf("placed %q, want the older job %q", placed.JobID, olderID)
	}
	olderProcess := worker.process(t)
	stillWaiting(t, newerResult)
	olderProcess.end(nil)
	if err := waitResult(t, got.process); err != nil {
		t.Fatal(err)
	}
	worker.process(t).end(nil)
	if newer := receiveStart(t, newerResult); newer.err != nil {
		t.Fatalf("newer job: %v", newer.err)
	} else {
		_ = waitResult(t, newer.process)
	}
}

func TestSharedWorkerGetsNoJobOutsideItsMapsAndNoSourceRoute(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	f := newFamily(t, "media")
	other := t.TempDir()
	outside := startTestAgent(t, hub, token, "outside", Capabilities{MaxJobs: 1, InputMode: "shared"}, PathMap{Server: other, Local: other})
	hub.waitEvent(t, EventRegistered)
	if _, err := hub.hub.StartHLS(context.Background(), f.job()); !errors.Is(err, ffmpeg.ErrNoRemoteRunner) {
		t.Fatalf("shared worker without a covering map: %v, want ErrNoRemoteRunner", err)
	}
	outside.kill()
	hub.waitEvent(t, EventDisconnected)

	mount := filepath.Dir(f.path)
	worker := startTestAgent(t, hub, token, "mapped", Capabilities{MaxJobs: 1, InputMode: "shared"}, PathMap{Server: mount, Local: mount})
	hub.waitEvent(t, EventRegistered)
	process := startJob(t, hub, f.job())
	ffmpegProcess := worker.process(t)
	if input := ffmpegProcess.options.Args[indexOf(ffmpegProcess.options.Args, "-i")+1]; input != "file:"+f.path {
		t.Fatalf("shared input = %q", input)
	}
	jobID, jobToken := onlyJob(t, hub)
	request, _ := http.NewRequest(http.MethodGet, hub.server.URL+pathJobs+jobID+"/source", nil)
	request.Header.Set("Authorization", "Bearer "+jobToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("source GET for a shared job = %d, want 403", response.StatusCode)
	}
	process.Kill()
	_ = waitResult(t, process)
}
