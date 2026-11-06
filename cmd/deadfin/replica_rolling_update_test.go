// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/storetest"
	"github.com/deadfin-org/deadfin/internal/transcodeworker"
)

// replicaServer is one replica's listener; its handler is set after the
// application opens, because the replica URL must exist first.
type replicaServer struct {
	server  *httptest.Server
	handler atomic.Pointer[http.Handler]
}

func newReplicaServer(t *testing.T) *replicaServer {
	t.Helper()
	listener := &replicaServer{}
	listener.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if current := listener.handler.Load(); current != nil {
			(*current).ServeHTTP(w, r)
			return
		}
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	t.Cleanup(listener.server.Close)
	return listener
}

func (listener *replicaServer) serve(app *application) {
	handler := app.AcceptForwards(app.Router())
	listener.handler.Store(&handler)
}

// loadBalancer spreads requests over its backends in turn, as a Kubernetes
// Service does over the ready pods.
type loadBalancer struct {
	server   *httptest.Server
	mu       sync.Mutex
	backends []*url.URL
	next     int
}

func newLoadBalancer(t *testing.T) *loadBalancer {
	t.Helper()
	balancer := &loadBalancer{}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			balancer.mu.Lock()
			target := balancer.backends[balancer.next%len(balancer.backends)]
			balancer.next++
			balancer.mu.Unlock()
			request.SetURL(target)
			request.Out.Host = request.In.Host
		},
		FlushInterval: -1,
	}
	balancer.server = httptest.NewServer(proxy)
	t.Cleanup(balancer.server.Close)
	return balancer
}

func (balancer *loadBalancer) set(backends ...*replicaServer) {
	balancer.mu.Lock()
	defer balancer.mu.Unlock()
	balancer.backends = balancer.backends[:0]
	for _, backend := range backends {
		target, _ := url.Parse(backend.server.URL)
		balancer.backends = append(balancer.backends, target)
	}
}

// TestRollingUpdateOfTwoReplicas is the #652 acceptance: two replicas on one
// store and one program-data volume. A client plays on replica A, with a
// remote HLS job on a worker process and a PlaySessionId grant. Requests
// that reach replica B go to A. A drains, as at SIGTERM in a rolling update:
// progress, the PlaySessionId stream and the remote HLS job go on through B,
// and B adopts the job with no restart. The store is PostgreSQL when
// DEADFIN_TEST_POSTGRES_DSN is set, else one SQLite file.
func TestRollingUpdateOfTwoReplicas(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	const (
		runtimeTicks  int64 = 20 * 60 * 10_000_000
		startTicks    int64 = runtimeTicks / 4
		forwardTicks  int64 = runtimeTicks / 3
		drainedTicks  int64 = runtimeTicks / 2
		replicaDevice       = "deadfin-test-device"
	)
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture.mkv")
	generate := exec.Command(ffmpegPath, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "10", "-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-c:a", "aac", "-y", fixture)
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v\n%s", err, output)
	}
	token, err := transcodeworker.NewToken("")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWorkerTokens, token)
	t.Setenv(envWorkerMode, string(transcodeworker.ModeWorkersFirst))
	options := []applicationOption{withReplicaOnSQLite()}
	if os.Getenv("DEADFIN_TEST_POSTGRES_DSN") != "" {
		options = []applicationOption{withDatabaseURL(storetest.PostgresSchemaURL(t))}
	}
	programData := filepath.Join(root, "program-data")

	listenerA, listenerB := newReplicaServer(t), newReplicaServer(t)
	appA := newCompositionApplication(t, programData, append(options, withReplicaURL(listenerA.server.URL))...)
	listenerA.serve(appA)
	configureComposedAdministrator(t, appA.Router())
	clientToken, userID := loginPlaystateTracerAdministrator(t, appA)
	movieID := seedPlaystateTracerMovie(t, appA, runtimeTicks)
	playSessionID := mintPlaystateTracerSession(t, appA, clientToken, movieID)
	start := fmt.Sprintf(`{"ItemId":%q,"CanSeek":true,"IsPaused":false,"PositionTicks":%d,"PlayMethod":"Transcode","PlaySessionId":%q}`, movieID.Dashed(), startTicks, playSessionID)
	if record := authenticatedCompositionRequest(appA.Router(), http.MethodPost, "/Sessions/Playing", []byte(start), clientToken); record.Code != http.StatusNoContent {
		t.Fatalf("start status = %d; body: %s", record.Code, record.Body.String())
	}

	// Replica B starts beside A: a rolling update with a surge of one.
	appB := newCompositionApplication(t, programData, append(options, withReplicaURL(listenerB.server.URL))...)
	listenerB.serve(appB)
	waitFor(t, func() bool {
		return appA.replicas.registry.Refresh(t.Context()) == nil && appA.replicas.registry.HasPeer()
	})
	sessionID := session.IDFor("Deadfin Test", replicaDevice)
	replicas := store.NewReplicaStore(appA.stores)
	if owner, _, err := replicas.PlaySessionOwner(t.Context(), sessionID); err != nil || owner != appA.replicas.registry.ID() {
		t.Fatalf("session owner = %q, %v; want replica A", owner, err)
	}

	// The worker reaches the server through the load balancer. The job is
	// placed while only A is ready.
	balancer := newLoadBalancer(t)
	balancer.set(listenerA)
	startWorkerProcess(t, balancer.server.URL, token, ffmpegPath)
	waitFor(t, func() bool { return len(appA.transcodeWorkers.Workers()) == 1 })
	paths, err := config.NewPaths(programData)
	if err != nil {
		t.Fatal(err)
	}
	transcodes, err := filepath.EvalSymlinks(paths.Transcodes)
	if err != nil {
		t.Fatal(err)
	}
	job := workerHLSRequest(transcodes, "rolling", fixture, 0, true)
	job.PlaySessionID, job.DeviceID = playSessionID, replicaDevice
	if err := appA.transcodes.StartHLS(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	segment, err := appA.transcodes.WaitSegment(t.Context(), job.PlaylistPath, 0)
	if err != nil {
		t.Fatalf("segment 0 on A: %v", err)
	}
	_ = segment.Close()
	if placed := workerPlacements(t, appA.transcodeWorkers); placed != 1 {
		t.Fatalf("worker placements on A = %d, want 1", placed)
	}
	// Both replicas are ready now: the worker's uploads reach B too, which
	// forwards them to A.
	balancer.set(listenerA, listenerB)

	// Through B: progress and the PlaySessionId stream reach A.
	progress := func(app *application, ticks int64) int {
		body := fmt.Sprintf(`{"ItemId":%q,"CanSeek":true,"IsPaused":false,"PositionTicks":%d,"PlayMethod":"Transcode","PlaySessionId":%q}`, movieID.Dashed(), ticks, playSessionID)
		return authenticatedCompositionRequest(app.Router(), http.MethodPost, "/Sessions/Playing/Progress", []byte(body), clientToken).Code
	}
	if code := progress(appB, forwardTicks); code != http.StatusNoContent {
		t.Fatalf("progress through B = %d", code)
	}
	if snapshot, found := appA.sessions.SessionByID(sessionID); !found || snapshot.NowPlaying == nil || snapshot.NowPlaying.PositionTicks == nil || *snapshot.NowPlaying.PositionTicks != forwardTicks {
		t.Fatalf("A's session after progress through B = %+v, %v", snapshot.NowPlaying, found)
	}
	if _, found := appB.sessions.SessionByID(sessionID); found {
		t.Fatal("B keeps a copy of A's session")
	}
	streamPath := fmt.Sprintf("/Videos/%s/stream?static=true&PlaySessionId=%s", movieID.Dashed(), playSessionID)
	streamCode := grantRequest(appA, streamPath).Code
	if streamCode == http.StatusUnauthorized {
		t.Fatal("the PlaySessionId does not authorize the stream on A")
	}
	if code := grantRequest(appB, streamPath).Code; code != streamCode {
		t.Fatalf("PlaySessionId stream through B = %d, want A's %d", code, streamCode)
	}

	// A drains: the load balancer stops sending it requests, as when the
	// pod is no longer ready, and A hands its work over.
	balancer.set(listenerB)
	realClock := clock.NewReal()
	drained := realClock.Now()
	drainCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := appA.DrainReplica(drainCtx); err != nil {
		t.Fatalf("drain A: %v", err)
	}
	t.Logf("replica drain took %s", realClock.Now().Sub(drained).Round(time.Millisecond))

	// The worker reconnects through the load balancer to B, which claims
	// and adopts the job. The hub counts the job on the worker before the
	// transcode manager holds the family, so wait for the family too.
	waitFor(t, func() bool {
		workers := appB.transcodeWorkers.Workers()
		return len(workers) == 1 && workers[0].ActiveJobs == 1 && appB.transcodes.HasTranscodingJob(playSessionID)
	})
	if records, err := store.NewTranscodeWorkerStore(appB.stores).LiveJobs(t.Context(), realClock.Now()); err != nil || len(records) != 1 || records[0].Owner != appB.replicas.registry.ID() {
		t.Fatalf("job records after the adoption = %+v, %v; want one owned by B", records, err)
	}
	if err := appB.transcodes.StartHLS(t.Context(), job); err != nil {
		t.Fatalf("join the adopted family on B: %v", err)
	}
	for index := 1; index <= 3; index++ {
		segment, err := appB.transcodes.WaitSegment(t.Context(), job.PlaylistPath, index)
		if err != nil {
			t.Fatalf("segment %d on B: %v", index, err)
		}
		_ = segment.Close()
	}
	if placed := workerPlacements(t, appB.transcodeWorkers); placed != 0 {
		t.Fatalf("worker placements on B = %d: the job restarted instead of moving", placed)
	}

	// Progress and the PlaySessionId stream go on through B, which claims
	// the session and the grant that A released.
	if code := progress(appB, drainedTicks); code != http.StatusNoContent {
		t.Fatalf("progress through B after the drain = %d", code)
	}
	snapshot, found := appB.sessions.SessionByID(sessionID)
	if !found || snapshot.UserID != userID || snapshot.NowPlaying == nil || snapshot.NowPlaying.ItemID != movieID ||
		snapshot.NowPlaying.PositionTicks == nil || *snapshot.NowPlaying.PositionTicks != drainedTicks {
		t.Fatalf("B's session after the drain = %+v, %v", snapshot, found)
	}
	if owner, _, err := replicas.PlaySessionOwner(t.Context(), sessionID); err != nil || owner != appB.replicas.registry.ID() {
		t.Fatalf("session owner after the drain = %q, %v; want replica B", owner, err)
	}
	if code := grantRequest(appB, streamPath).Code; code != streamCode {
		t.Fatalf("PlaySessionId stream through B after the drain = %d, want %d", code, streamCode)
	}
	_ = appB.transcodes.StopPlaySession(t.Context(), job.PlaySessionID, job.DeviceID)
}
