// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

const testLease = 30 * time.Second

// restartRig is one server address whose server process restarts: each
// generation is a new Hub, and optionally a new TranscodeManager, on the
// same store and transcode directory (#618).
type restartRig struct {
	server  *httptest.Server
	handler atomic.Pointer[http.Handler]
	workers *store.TranscodeWorkerStore
	clock   *clock.Fake
	root    string
	dir     string
	logs    string
	// edge is the stream edge target each generation gives its workers.
	edge EdgeTarget
	// replica and claim are each generation's replica mode (#652).
	replica string
	claim   func(context.Context, string) (bool, error)
}

// generation is one server process.
type generation struct {
	hub     *Hub
	manager *ffmpeg.TranscodeManager
	events  chan Event
}

func newRestartRig(t *testing.T, token Token) *restartRig {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	rig := &restartRig{workers: store.NewTranscodeWorkerStore(stores), clock: clock.NewFake(testEpoch), root: root,
		dir: filepath.Join(root, "transcodes"), logs: filepath.Join(root, "logs")}
	addJoinToken(t, rig.workers, token, nil, time.Time{})
	rig.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if current := rig.handler.Load(); current != nil {
			(*current).ServeHTTP(w, r)
			return
		}
		http.Error(w, "restarting", http.StatusServiceUnavailable)
	}))
	t.Cleanup(rig.server.Close)
	return rig
}

// start runs a server process. withManager adds a transcode manager whose
// startup wipe keeps the families of the live job records.
func (rig *restartRig) start(t *testing.T, withManager bool) *generation {
	t.Helper()
	g := &generation{events: make(chan Event, 256)}
	hub, err := NewHub(HubConfig{
		Clock: rig.clock, Credentials: StoreCredentials{Store: rig.workers, Clock: rig.clock},
		Policy: Policy{Mode: ModeWorkersFirst}, Jobs: rig.workers, Lease: testLease, Edge: rig.edge,
		Replica: rig.replica, ClaimJob: rig.claim,
		HeartbeatInterval: time.Hour, Observe: func(event Event) { g.events <- event },
	})
	if err != nil {
		t.Fatal(err)
	}
	g.hub = hub
	if withManager {
		live, err := rig.workers.LiveJobs(t.Context(), rig.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		var kept []ffmpeg.KeptFamily
		for _, job := range live {
			kept = append(kept, ffmpeg.KeptFamily{Playlist: job.Playlist, SegmentPattern: job.SegmentPattern})
		}
		g.manager, err = ffmpeg.NewTranscodeManager("ffmpeg", rig.dir, ffmpeg.TranscodeManagerOptions{
			Clock: rig.clock, LogDirectory: rig.logs, RemoteRunner: hub, KeepFamilies: kept,
		})
		if err != nil {
			t.Fatal(err)
		}
		hub.SetAdopter(g.manager)
	}
	var handler http.Handler = hub
	rig.handler.Store(&handler)
	t.Cleanup(func() { g.stop(t, rig) })
	return g
}

// stop shuts the server process down as the application does: the
// manager releases its remote jobs, then the hub closes.
func (g *generation) stop(t *testing.T, rig *restartRig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if g.manager != nil {
		if err := g.manager.Close(ctx); err != nil {
			t.Errorf("manager close: %v", err)
		}
	}
	if current := rig.handler.Load(); current != nil && *current == http.Handler(g.hub) {
		rig.handler.Store(nil)
	}
	if err := g.hub.Close(ctx); err != nil {
		t.Errorf("hub close: %v", err)
	}
}

func (g *generation) waitEvent(t *testing.T, kind EventKind) Event {
	t.Helper()
	deadline := time.NewTimer(testTimeout)
	defer deadline.Stop()
	for {
		select {
		case event := <-g.events:
			if event.Kind == kind {
				return event
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for hub event %s", kind)
			return Event{}
		}
	}
}

func (rig *restartRig) agent(t *testing.T, token Token) *testAgent {
	t.Helper()
	return startTestAgentIn(t, &testHub{server: rig.server}, t.TempDir(), token, "survivor", Capabilities{MaxJobs: 2})
}

// hlsRequest is a remote-eligible HLS family with 2 s segments.
func (rig *restartRig) hlsRequest(t *testing.T) ffmpeg.HLSRequest {
	t.Helper()
	source := filepath.Join(rig.root, "media", "movie.mkv")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("movie bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	playlist := filepath.Join(rig.dir, "abc.m3u8")
	pattern := filepath.Join(rig.dir, "abc%d.ts")
	args := []string{"-i", "file:" + source, "-c:v", "libx264", "-start_number", "0", "-hls_segment_filename", pattern, "-y", playlist}
	return ffmpeg.HLSRequest{
		PlaylistPath: playlist, SegmentPattern: pattern, Args: args, PlaySessionID: "play", DeviceID: "device",
		SegmentLengthSeconds: 2, SourceRevision: "rev",
		OpenSource: func() (*sourcefile.Source, error) { return sourcefile.Open(source, sourcefile.Expected{}) },
	}
}

func readSegment(t *testing.T, manager *ffmpeg.TranscodeManager, playlist string, index int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	body, err := manager.WaitSegment(ctx, playlist, index)
	if err != nil {
		t.Fatalf("WaitSegment(%d): %v", index, err)
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return tsText(data)
}

func (a *testAgent) heldJob(t *testing.T) *agentJob {
	t.Helper()
	var job *agentJob
	eventually(t, nil, func() bool {
		a.agent.jobsMu.Lock()
		defer a.agent.jobsMu.Unlock()
		for _, candidate := range a.agent.jobs {
			job = candidate
		}
		return job != nil
	})
	return job
}

func (job *agentJob) detached() bool {
	current, _, _ := job.link()
	return current == nil
}

// reconnect steps the worker clock by its reconnect delay until the hub
// reports kind for the worker's held job. Each step waits for the dial, so
// the clock never runs past the job lease.
func reconnect(t *testing.T, a *testAgent, g *generation, kind EventKind) Event {
	t.Helper()
	for step := 0; step < 10; step++ {
		a.clock.Advance(2 * time.Second)
		wait := time.NewTimer(200 * time.Millisecond)
		for waiting := true; waiting; {
			select {
			case event := <-g.events:
				if event.Kind == kind {
					wait.Stop()
					return event
				}
			case <-wait.C:
				waiting = false
			}
		}
	}
	t.Fatalf("the worker never reattached (%s)", kind)
	return Event{}
}

func noLaunch(t *testing.T, a *testAgent) {
	t.Helper()
	select {
	case <-a.launched:
		t.Fatal("the worker launched ffmpeg again: the family restarted")
	default:
	}
}

// TestHeldJobSurvivesAServerRestart is the redeploy path: the server
// releases its remote job at shutdown, the worker keeps ffmpeg running and
// its new segments on disk, and the next server process on the same store
// and transcode directory adopts the job. The next WaitSegment returns the
// segment the worker made while the server was away, with no restart and no
// gap.
func TestHeldJobSurvivesAServerRestart(t *testing.T) {
	token := mustToken(t)
	rig := newRestartRig(t, token)
	first := rig.start(t, true)
	worker := rig.agent(t, token)
	first.waitEvent(t, EventRegistered)

	request := rig.hlsRequest(t)
	if err := first.manager.StartHLS(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	process := worker.process(t)
	process.write(t, "abc0.ts", "zero")
	process.write(t, "abc1.ts", "on")
	eventually(t, worker.poll, func() bool {
		_, err := os.Stat(filepath.Join(rig.dir, "abc1.ts"))
		return err == nil
	})
	if got := readSegment(t, first.manager, request.PlaylistPath, 0); got != "zero" {
		t.Fatalf("segment 0 = %q", got)
	}
	job := worker.heldJob(t)

	// The server process stops. The worker keeps the job.
	first.stop(t, rig)
	eventually(t, nil, job.detached)
	if process.isStopped() {
		t.Fatal("the worker stopped ffmpeg when the server went away")
	}
	process.write(t, "abc1.ts", "one")
	process.write(t, "abc2.ts", "tw")
	for range 5 {
		worker.poll()
	}

	// The next server process keeps the family on disk and adopts it.
	second := rig.start(t, true)
	if data, err := os.ReadFile(filepath.Join(rig.dir, "abc0.ts")); err != nil || tsText(data) != "zero" {
		t.Fatalf("the startup wipe removed a live family: %q, %v", data, err)
	}
	reconnect(t, worker, second, EventJobAdopted)
	eventually(t, worker.poll, func() bool {
		data, err := os.ReadFile(filepath.Join(rig.dir, "abc1.ts"))
		_, growing := os.Stat(filepath.Join(rig.dir, "abc2.ts"))
		return !job.detached() && err == nil && tsText(data) == "one" && growing == nil
	})
	if got := readSegment(t, second.manager, request.PlaylistPath, 1); got != "one" {
		t.Fatalf("segment 1 after the restart = %q, want the held segment", got)
	}
	if got := readSegment(t, second.manager, request.PlaylistPath, 0); got != "zero" {
		t.Fatalf("segment 0 after the restart = %q", got)
	}
	noLaunch(t, worker)

	// The first request joins the adopted family without a restart.
	if err := second.manager.StartHLS(t.Context(), request); err != nil {
		t.Fatalf("join the adopted family: %v", err)
	}
	noLaunch(t, worker)
	process.write(t, "abc3.ts", "three")
	eventually(t, worker.poll, func() bool {
		data, err := os.ReadFile(filepath.Join(rig.dir, "abc2.ts"))
		return err == nil && tsText(data) == "tw"
	})

	// Stopping the play session stops the adopted job on the worker.
	stopped := make(chan error, 1)
	go func() { stopped <- second.manager.StopPlaySession(context.Background(), "play", "device") }()
	eventually(t, worker.poll, func() bool {
		process.mu.Lock()
		defer process.mu.Unlock()
		return strings.Contains(process.stdin.String(), "q")
	})
	process.end(nil)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testTimeout):
		t.Fatal("StopPlaySession did not return")
	}
	eventually(t, nil, func() bool {
		_, err := rig.workers.Job(context.Background(), job.spec.ID)
		return errors.Is(err, store.ErrNotFound)
	})
}

// TestHeldJobStopsAtLeaseExpiry: with no server, the worker stops a held
// job exactly when its lease ends.
func TestHeldJobStopsAtLeaseExpiry(t *testing.T) {
	token := mustToken(t)
	rig := newRestartRig(t, token)
	g := rig.start(t, false)
	worker := rig.agent(t, token)
	g.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")
	request := f.job()
	request.SegmentLength = 2
	remote := startJob(t, &testHub{hub: g.hub}, request)
	process := worker.process(t)
	job := worker.heldJob(t)
	if job.lease != testLease {
		t.Fatalf("job lease = %s, want %s", job.lease, testLease)
	}

	releaser, ok := remote.(ffmpeg.RemoteReleaser)
	if !ok || !releaser.Release() {
		t.Fatal("the hub did not release the job")
	}
	if err := waitResult(t, remote); !errors.Is(err, ffmpeg.ErrRunnerReleased) {
		t.Fatalf("released job Wait = %v, want ErrRunnerReleased", err)
	}
	g.stop(t, rig)
	eventually(t, nil, func() bool {
		job.mu.Lock()
		defer job.mu.Unlock()
		return job.session == nil && job.leaseArmed
	})

	worker.clock.Advance(testLease - time.Millisecond)
	for range 200 {
		runtime.Gosched()
	}
	if process.isStopped() {
		t.Fatal("the held job stopped before its lease ended")
	}
	worker.clock.Advance(time.Millisecond)
	eventually(t, nil, process.isStopped)
	eventually(t, nil, func() bool { return worker.agent.jobCount() == 0 })
}

// TestHeldJobWithoutARecordIsKilled: a worker that lost its connection to a
// running server keeps the job for the lease, but the server restarted the
// family elsewhere and deleted the record, so the reattach kills the job.
func TestHeldJobWithoutARecordIsKilled(t *testing.T) {
	token := mustToken(t)
	rig := newRestartRig(t, token)
	g := rig.start(t, false)
	worker := rig.agent(t, token)
	registered := g.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")
	request := f.job()
	request.SegmentLength = 2
	remote := startJob(t, &testHub{hub: g.hub}, request)
	process := worker.process(t)
	job := worker.heldJob(t)

	g.hub.Disconnect(registered.WorkerID)
	if err := waitResult(t, remote); !errors.Is(err, ffmpeg.ErrRunnerLost) {
		t.Fatalf("lost job Wait = %v, want ErrRunnerLost", err)
	}
	if _, err := rig.workers.Job(context.Background(), job.spec.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("record of a lost job = %v, want ErrNotFound", err)
	}
	refused := reconnect(t, worker, g, EventJobRefused)
	if refused.JobID != job.spec.ID || !strings.Contains(refused.Detail, "no record") {
		t.Fatalf("refusal = %+v", refused)
	}
	eventually(t, worker.poll, process.isStopped)
}

// TestUnauthorizedUploadOfAHeldJobIsRetried: while a job waits for its
// worker's reattach, its uploads get 503, which the worker retries.
func TestUnauthorizedUploadOfAHeldJobIsRetried(t *testing.T) {
	token := mustToken(t)
	rig := newRestartRig(t, token)
	g := rig.start(t, false)
	if err := rig.workers.CreateJob(t.Context(), store.TranscodeWorkerJob{
		ID: "held", WorkerID: "w", Playlist: filepath.Join(rig.dir, "abc.m3u8"), SegmentPattern: filepath.Join(rig.dir, "abc%d.ts"),
		TokenHash: tokenHash("secret"), SegmentLength: 2, Created: rig.clock.Now(), LeaseExpires: rig.clock.Now().Add(testLease),
	}); err != nil {
		t.Fatal(err)
	}
	put := func(id string) int {
		request, err := http.NewRequest(http.MethodPut, rig.server.URL+pathJobs+id+"/files/abc0.ts", strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set(ArtifactStateHeader, ArtifactComplete)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	if code := put("held"); code != http.StatusServiceUnavailable {
		t.Fatalf("upload of a held job = %d, want 503", code)
	}
	if code := put("unknown"); code != http.StatusUnauthorized {
		t.Fatalf("upload of an unknown job = %d, want 401", code)
	}
	rig.clock.Advance(testLease)
	if code := put("held"); code != http.StatusUnauthorized {
		t.Fatalf("upload after the lease = %d, want 401", code)
	}
	if !transientUpload(&uploadStatusError{code: http.StatusServiceUnavailable}) || transientUpload(&uploadStatusError{code: http.StatusUnauthorized}) ||
		!transientUpload(errors.New("connection refused")) {
		t.Fatal("transientUpload misclassifies")
	}
	_ = g
}

// TestHeldJobOfALiveReplicaIsKilled covers #652: a worker that reconnects
// to another server replica while the replica that placed its job is still
// live does not move the job; the new replica kills it. A replica names
// itself in welcome, and the worker sends that name on its job requests.
func TestHeldJobOfALiveReplicaIsKilled(t *testing.T) {
	token := mustToken(t)
	rig := newRestartRig(t, token)
	rig.replica = "replica-b"
	var claims atomic.Int32
	rig.claim = func(_ context.Context, id string) (bool, error) {
		claims.Add(1)
		return false, nil
	}
	g := rig.start(t, false)
	worker := rig.agent(t, token)
	registered := g.waitEvent(t, EventRegistered)
	eventually(t, nil, func() bool {
		worker.agent.mu.Lock()
		defer worker.agent.mu.Unlock()
		return worker.agent.replica == "replica-b"
	})
	f := newFamily(t, "media")
	request := f.job()
	request.SegmentLength = 2
	remote := startJob(t, &testHub{hub: g.hub}, request)
	process := worker.process(t)
	job := worker.heldJob(t)
	if record, err := rig.workers.Job(t.Context(), job.spec.ID); err != nil || record.Owner != "replica-b" {
		t.Fatalf("job record = %+v, %v; want owner replica-b", record, err)
	}

	// Release keeps the record; the reconnect presents the job, and the
	// claim fails: another live replica owns it.
	releaser, ok := remote.(ffmpeg.RemoteReleaser)
	if !ok || !releaser.Release() {
		t.Fatal("the hub did not release the job")
	}
	_ = waitResult(t, remote)
	g.hub.Disconnect(registered.WorkerID)
	refused := reconnect(t, worker, g, EventJobRefused)
	if refused.JobID != job.spec.ID || !strings.Contains(refused.Detail, "another live server replica") || claims.Load() == 0 {
		t.Fatalf("refusal = %+v, claims %d", refused, claims.Load())
	}
	eventually(t, worker.poll, process.isStopped)
}

// TestReplicaTransportNamesTheReplica: job requests carry the replica of
// the session; other requests do not.
func TestReplicaTransportNamesTheReplica(t *testing.T) {
	var seen []string
	agent := &Agent{replica: "replica-a"}
	transport := replicaTransport{agent: agent, next: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen = append(seen, request.URL.Path+"="+request.Header.Get(ReplicaHeader))
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: request}, nil
	})}
	for _, path := range []string{"/base/deadfin/v1/workers/jobs/j/files/a0.ts", "/base/deadfin/v1/workers/connect"} {
		request := httptest.NewRequest(http.MethodPut, "https://server"+path, nil)
		if _, err := transport.RoundTrip(request); err != nil {
			t.Fatal(err)
		}
		if request.Header.Get(ReplicaHeader) != "" {
			t.Fatal("the transport changed the caller's request")
		}
	}
	want := []string{"/base/deadfin/v1/workers/jobs/j/files/a0.ts=replica-a", "/base/deadfin/v1/workers/connect="}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("requests = %v, want %v", seen, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
