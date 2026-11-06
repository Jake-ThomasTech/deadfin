// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
	"github.com/deadfin-org/deadfin/internal/store"
)

// hookReceiver is a fake outside scaler that checks each signature.
type hookReceiver struct {
	server *httptest.Server
	calls  chan ScaleHookCall
}

func newHookReceiver(t *testing.T, secret []byte) *hookReceiver {
	t.Helper()
	receiver := &hookReceiver{calls: make(chan ScaleHookCall, 64)}
	receiver.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.Header.Get(ScaleHookSignatureHeader) != SignScaleHook(secret, body) {
			t.Errorf("hook call %s with signature %q is not signed", r.Method, r.Header.Get(ScaleHookSignatureHeader))
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		var call ScaleHookCall
		if err := json.Unmarshal(body, &call); err != nil {
			t.Errorf("hook body: %v", err)
		}
		receiver.calls <- call
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.server.Close)
	return receiver
}

func (r *hookReceiver) next(t *testing.T) ScaleHookCall {
	t.Helper()
	select {
	case call := <-r.calls:
		return call
	case <-time.After(testTimeout):
		t.Fatal("no scale hook call")
		return ScaleHookCall{}
	}
}

// none waits until the hook loop finished its calls and fails when one
// more call arrived.
func (r *hookReceiver) none(t *testing.T, hub *testHub) {
	t.Helper()
	hub.waitEvent(t, EventScaleHookWait)
	select {
	case call := <-r.calls:
		t.Fatalf("unexpected scale hook call %+v", call)
	default:
	}
}

// TestScaleHookCallsAtOnceWhenAJobQueues covers #623: the hook gets the
// initial signals, a signed call at once when a job queues, and a debounced
// call when demand falls.
func TestScaleHookCallsAtOnceWhenAJobQueues(t *testing.T) {
	before := goleak.IgnoreCurrent()
	t.Cleanup(func() {
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		goleak.VerifyNone(t, before)
	})
	secret := []byte("hook-secret")
	receiver := newHookReceiver(t, secret)
	hub := newTestHubWithConfig(t, HubConfig{
		Policy:    Policy{Mode: ModeWorkersOnly, DefaultPools: []string{"gpu"}},
		ScaleHook: ScaleHook{URL: receiver.server.URL, Secret: secret, Client: &http.Client{Transport: http.DefaultTransport}},
	})
	if call := receiver.next(t); call.Pool != AllPools || call.Reason != ScaleReasonChanged || call.Signals.SlotsWanted != 1 {
		t.Fatalf("initial call = %+v", call)
	}
	receiver.none(t, hub)

	ctx, cancel := context.WithCancel(context.Background())
	result := startAsync(ctx, hub, newFamily(t, "media").job())
	hub.waitEvent(t, EventJobQueued)
	// No clock step: the queued job calls the hook at once.
	if call := receiver.next(t); call.Pool != AllPools || call.Reason != ScaleReasonUrgent || call.Signals.QueuedJobs != 1 || call.Signals.SlotsWanted != 2 {
		t.Fatalf("queued call = %+v", call)
	}
	if call := receiver.next(t); call.Pool != "gpu" || call.Signals.QueuedJobs != 1 || call.Signals.SlotsWanted != 2 {
		t.Fatalf("queued pool call = %+v", call)
	}

	cancel()
	if got := receiveStart(t, result); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("canceled start: %v", got.err)
	}
	receiver.none(t, hub)
	hub.clock.Advance(DefaultScaleHookInterval)
	if call := receiver.next(t); call.Pool != AllPools || call.Reason != ScaleReasonChanged || call.Signals.SlotsWanted != 1 {
		t.Fatalf("debounced call = %+v", call)
	}
}

// TestAnticipateCountsInSlotsWanted: a PlaybackInfo transcode decision
// counts in the default pools until its job starts or the warm-up ends.
func TestAnticipateCountsInSlotsWanted(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersFirst, DefaultPools: []string{"gpu"}}}, token)
	hub.hub.Anticipate("play")
	hub.hub.Anticipate("play")
	hub.hub.Anticipate("other")
	if gpu := hub.hub.Scaling().Pools["gpu"]; gpu.WarmingJobs != 2 || gpu.SlotsWanted != 3 {
		t.Fatalf("gpu signals = %+v, want 2 warming, 3 wanted", gpu)
	}
	worker := startTestAgent(t, hub, token, "gpu", Capabilities{MaxJobs: 1, Pools: []string{"gpu"}})
	hub.waitEvent(t, EventRegistered)
	process := startJob(t, hub, newFamily(t, "media").job()) // PlaySessionId "play"
	worker.process(t)
	if gpu := hub.hub.Scaling().Pools["gpu"]; gpu.WarmingJobs != 1 || gpu.ActiveJobs != 1 {
		t.Fatalf("after the job started: %+v", gpu)
	}
	hub.clock.Advance(DefaultWarmupTTL)
	if gpu := hub.hub.Scaling().Pools["gpu"]; gpu.WarmingJobs != 0 {
		t.Fatalf("after the TTL: %+v", gpu)
	}
	process.Kill()
	_ = waitResult(t, process)

	local := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeLocal}})
	local.hub.Anticipate("play")
	if all := local.hub.Scaling().Pools[AllPools]; all.WarmingJobs != 0 {
		t.Fatalf("local mode counted a warm-up: %+v", all)
	}
}

// TestEphemeralWorkerIdentityIsGoneAfterItDisconnects covers #623: a
// Deployment replica enrolls with the join token, and its identity is
// deleted when its connection ends; the server's own stop keeps it.
func TestEphemeralWorkerIdentityIsGoneAfterItDisconnects(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	ephemeral := startEphemeralAgent(t, hub, token)
	enrolled := hub.waitEvent(t, EventEnrolled)
	hub.waitEvent(t, EventRegistered)
	if _, err := hub.workers.Worker(t.Context(), enrolled.WorkerID); err != nil {
		t.Fatalf("ephemeral identity before the disconnect: %v", err)
	}
	ephemeral.kill()
	if event := hub.waitEvent(t, EventForgotten); event.WorkerID != enrolled.WorkerID {
		t.Fatalf("forgotten %q, want %q", event.WorkerID, enrolled.WorkerID)
	}
	if _, err := hub.workers.Worker(t.Context(), enrolled.WorkerID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ephemeral identity after the disconnect: %v, want ErrNotFound", err)
	}

	// A durable worker keeps its identity.
	durable := startTestAgent(t, hub, token, "durable", Capabilities{MaxJobs: 1})
	kept := hub.waitEvent(t, EventEnrolled)
	hub.waitEvent(t, EventRegistered)
	durable.kill()
	hub.waitEvent(t, EventDisconnected)
	if _, err := hub.workers.Worker(t.Context(), kept.WorkerID); err != nil {
		t.Fatalf("durable identity: %v", err)
	}
}

// TestEphemeralIdentitySurvivesTheServerStop: a server stop is not the end
// of the worker, which reconnects to the next server process.
func TestEphemeralIdentitySurvivesTheServerStop(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	startEphemeralAgent(t, hub, token)
	enrolled := hub.waitEvent(t, EventEnrolled)
	hub.waitEvent(t, EventRegistered)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := hub.hub.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.workers.Worker(t.Context(), enrolled.WorkerID); err != nil {
		t.Fatalf("ephemeral identity after the server stop: %v", err)
	}
}

func startEphemeralAgent(t *testing.T, hub *testHub, token Token) *testAgent {
	t.Helper()
	fake := clock.NewFake(testEpoch)
	launched := make(chan *fakeProcess, 16)
	agent, err := NewAgent(AgentConfig{
		ServerURL: hub.server.URL, StateDir: t.TempDir(), JoinToken: token, Name: "replica", WorkDir: t.TempDir(),
		Capabilities: Capabilities{MaxJobs: 1}, Clock: fake, PKeyPause: true, Ephemeral: true,
		Launch: func(_ context.Context, options ffmpeg.ExecOptions) (Process, error) {
			process := &fakeProcess{options: options, done: make(chan struct{})}
			launched <- process
			return process, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	running := &testAgent{agent: agent, clock: fake, launched: launched, cancel: cancel, done: make(chan error, 1)}
	go func() { running.done <- agent.Run(ctx) }()
	t.Cleanup(running.kill)
	return running
}
