// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/ffmpeg"
)

func (p *fakeProcess) isStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

func (a *testAgent) runResult(t *testing.T) error {
	t.Helper()
	select {
	case err := <-a.done:
		a.done <- err
		return err
	case <-time.After(testTimeout):
		t.Fatal("the worker agent kept running")
		return nil
	}
}

func TestDrainHandsOffAndKillsOnlyAfterTheReplacementStarts(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	first := startTestAgent(t, hub, token, "a-first", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	hub.clock.Advance(time.Second)
	second := startTestAgent(t, hub, token, "b-second", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")

	process := startJob(t, hub, f.job())
	old := first.process(t)
	old.write(t, "abc0.ts", "zero")
	old.write(t, "abc1.ts", "one")
	old.write(t, "abc2.ts", "two")
	eventually(t, first.poll, func() bool {
		one, _ := f.read("abc1.ts")
		_, growing := f.read("abc2.ts")
		return one == "one" && growing
	})

	first.agent.Drain()
	hub.waitEvent(t, EventDraining)
	if err := waitResult(t, process); !errors.Is(err, ffmpeg.ErrRunnerLost) {
		t.Fatalf("Wait after drain = %v, want ErrRunnerLost so the family restarts", err)
	}
	if one, _ := f.read("abc1.ts"); one != "one" {
		t.Fatal("a completed segment was lost in the handoff")
	}
	if _, ok := f.read("abc2.ts"); ok {
		t.Fatal("the incomplete segment's placeholder survived and would look cached")
	}
	stillRunning := func() {
		for range 100 {
			first.poll()
		}
		if old.isStopped() {
			t.Fatal("the draining worker's job was killed before the replacement start")
		}
	}
	stillRunning()

	// The replacement goes to the other worker; job.kill follows its start.
	replacement := startJob(t, hub, f.job())
	placed := hub.waitEvent(t, EventJobPlaced)
	if placed.Name != "b-second" {
		t.Fatalf("replacement placed on %q, want b-second", placed.Name)
	}
	eventually(t, nil, old.isStopped)
	if err := first.runResult(t); err != nil {
		t.Fatalf("drained agent Run = %v, want nil", err)
	}
	second.process(t).end(nil)
	if err := waitResult(t, replacement); err != nil {
		t.Fatalf("replacement = %v", err)
	}
}

func TestDrainWithoutAReplacementKillsAtTheHandoffTimeout(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{HeartbeatInterval: time.Hour, HandoffTimeout: 10 * time.Second}, token)
	worker := startTestAgent(t, hub, token, "only", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")
	process := startJob(t, hub, f.job())
	ffmpegProcess := worker.process(t)

	worker.agent.Drain()
	hub.waitEvent(t, EventJobHandoff)
	_ = waitResult(t, process)
	// A draining worker takes no job.
	if _, err := hub.hub.StartHLS(context.Background(), newFamily(t, "other").job()); !errors.Is(err, ffmpeg.ErrNoRemoteRunner) {
		t.Fatalf("start on a draining fleet = %v, want ErrNoRemoteRunner", err)
	}
	hub.clock.Advance(9 * time.Second)
	if ffmpegProcess.isStopped() {
		t.Fatal("killed before the handoff timeout")
	}
	eventually(t, func() { hub.clock.Advance(time.Second) }, ffmpegProcess.isStopped)
	if err := worker.runResult(t); err != nil {
		t.Fatalf("drained agent Run = %v, want nil", err)
	}
}

func TestStalledWorkerIsQuarantinedOnTheFakeClock(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{HeartbeatInterval: time.Hour, Quarantine: 30 * time.Second}, token)
	worker := startTestAgent(t, hub, token, "flappy", Capabilities{MaxJobs: 2})
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")
	process := startJob(t, hub, f.job())
	worker.process(t)

	hub.clock.Advance(DefaultStallTimeout + 2*time.Second)
	hub.waitEvent(t, EventQuarantined)
	if err := waitResult(t, process); !errors.Is(err, ffmpeg.ErrRunnerLost) {
		t.Fatalf("stalled job = %v", err)
	}
	if _, err := hub.hub.StartHLS(context.Background(), f.job()); !errors.Is(err, ffmpeg.ErrNoRemoteRunner) {
		t.Fatalf("start on a quarantined worker = %v, want ErrNoRemoteRunner", err)
	}
	if state := hub.hub.Scaling().Pools[AllPools].Workers[WorkerQuarantined]; state != 1 {
		t.Fatalf("quarantined workers = %d, want 1", state)
	}
	hub.clock.Advance(30 * time.Second)
	eventually(t, nil, func() bool { return hub.hub.Scaling().Pools[AllPools].Workers[WorkerReady] == 1 })
	again := startJob(t, hub, f.job())
	worker.process(t).end(nil)
	if err := waitResult(t, again); err != nil {
		t.Fatalf("job after the quarantine = %v", err)
	}
}

func TestWorkerWithAnotherFFmpegMajorVersionIsRefused(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{FFmpegMajor: 7}, token)
	old := startTestAgent(t, hub, token, "old", Capabilities{MaxJobs: 1, FFmpegVersion: "6.1.2"})
	if err := old.runResult(t); !errors.Is(err, ErrRejected) {
		t.Fatalf("worker with ffmpeg 6 = %v, want ErrRejected", err)
	}
	startTestAgent(t, hub, token, "same", Capabilities{MaxJobs: 1, FFmpegVersion: "7.1.1"})
	hub.waitEvent(t, EventRegistered)

	for version, want := range map[string]int{"7.1.1": 7, "n7.1": 7, "8.0-Jellyfin": 8, "10.0": 10} {
		if got, ok := ffmpegMajor(version); !ok || got != want {
			t.Errorf("ffmpegMajor(%q) = %d, %v; want %d", version, got, ok, want)
		}
	}
	for _, version := range []string{"", "git-2024", "N-1234"} {
		if _, ok := ffmpegMajor(version); ok {
			t.Errorf("ffmpegMajor(%q) parsed", version)
		}
	}
}

func TestScalingSignalsAndMetricsAgree(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersOnly, DefaultPools: []string{"gpu"}}}, token)
	workers := []*testAgent{
		startTestAgent(t, hub, token, "one", Capabilities{MaxJobs: 2, Pools: []string{"gpu"}}),
		startTestAgent(t, hub, token, "two", Capabilities{MaxJobs: 2, Pools: []string{"gpu"}}),
	}
	hub.waitEvent(t, EventRegistered)
	hub.waitEvent(t, EventRegistered)

	var processes []ffmpeg.RemoteProcess
	for index := range 4 {
		processes = append(processes, startJob(t, hub, newFamily(t, fmt.Sprint("media", index)).job()))
	}
	queued := startAsync(context.Background(), hub, newFamily(t, "media5").job())
	hub.waitEvent(t, EventJobQueued)
	assertSignals(t, hub, PoolSignals{ActiveJobs: 4, QueuedJobs: 1, FreeSlots: 0, SlotsWanted: 6})

	// Two playbacks stop; the queued job takes one of the freed slots.
	var launched []*fakeProcess
	for _, worker := range workers {
		for range 2 {
			launched = append(launched, worker.process(t))
		}
	}
	launched[0].end(nil)
	launched[2].end(nil)
	_ = waitResult(t, processes[0])
	got := receiveStart(t, queued)
	if got.err != nil {
		t.Fatalf("queued job: %v", got.err)
	}
	eventually(t, nil, func() bool { return hub.hub.Scaling().Pools["gpu"].ActiveJobs == 3 })
	assertSignals(t, hub, PoolSignals{ActiveJobs: 3, QueuedJobs: 0, FreeSlots: 1, SlotsWanted: 4})

	var text bytes.Buffer
	if err := hub.hub.WriteMetrics(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), `deadfin_transcode_placements_total{target="worker",rule=""} 5`) ||
		!strings.Contains(text.String(), `deadfin_transcode_pool_wait_seconds_count{pool="gpu",outcome="placed"} 1`) {
		t.Fatalf("metrics:\n%s", text.String())
	}
	for _, worker := range workers {
		for {
			select {
			case process := <-worker.launched:
				process.end(nil)
				continue
			default:
			}
			break
		}
	}
	for _, process := range launched {
		process.end(nil)
	}
	_ = waitResult(t, got.process)
}

// assertSignals compares the pool "gpu" and the fleet in the JSON signals
// and in the Prometheus text.
func assertSignals(t *testing.T, hub *testHub, want PoolSignals) {
	t.Helper()
	scaling := hub.hub.Scaling()
	var text bytes.Buffer
	if err := hub.hub.WriteMetrics(&text); err != nil {
		t.Fatal(err)
	}
	samples := map[string]string{}
	scanner := bufio.NewScanner(&text)
	for scanner.Scan() {
		if line := scanner.Text(); !strings.HasPrefix(line, "#") {
			name, value, _ := strings.Cut(line, " ")
			samples[name] = value
		}
	}
	for _, pool := range []string{"gpu", AllPools} {
		got := scaling.Pools[pool]
		if got.ActiveJobs != want.ActiveJobs || got.QueuedJobs != want.QueuedJobs || got.FreeSlots != want.FreeSlots || got.SlotsWanted != want.SlotsWanted {
			t.Fatalf("pool %q signals = %+v, want %+v", pool, got, want)
		}
		for metric, value := range map[string]int{"active_jobs": want.ActiveJobs, "queued_jobs": want.QueuedJobs, "free_slots": want.FreeSlots, "slots_wanted": want.SlotsWanted} {
			name := fmt.Sprintf("deadfin_transcode_pool_%s{pool=%q}", metric, pool)
			if samples[name] != fmt.Sprint(value) {
				t.Fatalf("%s = %q, want %d (JSON %+v)", name, samples[name], value, got)
			}
		}
		if got.Workers[WorkerReady] != 2 || samples[fmt.Sprintf("deadfin_transcode_pool_workers{pool=%q,state=\"ready\"}", pool)] != "2" {
			t.Fatalf("pool %q ready workers: JSON %v", pool, got.Workers)
		}
	}
}
