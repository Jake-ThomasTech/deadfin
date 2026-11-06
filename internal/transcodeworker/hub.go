// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"context"
	"crypto/hmac"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/edge"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
	"github.com/deadfin-org/deadfin/internal/hls"
	"github.com/deadfin-org/deadfin/internal/store"
)

// Defaults for HubConfig.
const (
	DefaultHeartbeatInterval = 5 * time.Second
	DefaultMissedHeartbeats  = 3
	// DefaultStallTimeout is two 6 s segments: a running, unpaused job
	// that uploads nothing and writes no stderr for this long is lost.
	DefaultStallTimeout = 12 * time.Second
	// DefaultCheckInterval is the period of the expiry and stall checks.
	DefaultCheckInterval = time.Second
	// DefaultQuarantine keeps a worker that lost a job out of placement, so
	// a flapping worker cannot cause a restart storm.
	DefaultQuarantine = 30 * time.Second
	// DefaultHandoffTimeout bounds how long a draining worker keeps a
	// handed-off job when no replacement start comes.
	DefaultHandoffTimeout = 10 * time.Second
	// DefaultLease is how long a worker keeps a job running without a
	// server connection (#618), so the job survives a server restart.
	DefaultLease            = 60 * time.Second
	jobStoreTimeout         = 5 * time.Second
	defaultMaxArtifactBytes = 512 << 20
	maxControlMessageBytes  = 1 << 20
	sendQueueLength         = 256
	maxWorkerJobs           = 64
)

// ArtifactStateHeader carries "growing" or "complete" on a file PUT.
const ArtifactStateHeader = "Deadfin-Artifact-State"

// Artifact states.
const (
	ArtifactGrowing  = "growing"
	ArtifactComplete = "complete"
)

var (
	errJobKilled     = errors.New("remote transcode job killed")
	errConnClosed    = errors.New("transcode worker connection closed")
	errSlowWorker    = errors.New("transcode worker send queue is full")
	segmentDirective = regexp.MustCompile(`%(?:[-+# 0]*[0-9]*(?:\.[0-9]+)?)d`)
	// absolutePath finds an absolute path inside an operand or a filter
	// graph. A division such as "ih/2" does not match.
	absolutePath = regexp.MustCompile(`(?:^|[=:'",\s])(/[^\s:,'"]*)`)
)

// HubConfig configures the server side.
type HubConfig struct {
	Clock clock.Clock
	// Credentials resolves join tokens and worker identities (#615).
	Credentials Credentials
	// Policy is the initial placement policy; the zero value is ModeLocal.
	Policy Policy
	// AllowPlainHTTP reports whether workers may connect over plain HTTP
	// from a non-loopback address. The hub calls it on each worker request,
	// so a dashboard change applies without a restart (#664). Nil, or false,
	// requires TLS on the worker routes, at the server or at a proxy that
	// sets X-Forwarded-Proto.
	AllowPlainHTTP func() bool
	// HeartbeatInterval is the interval workers heartbeat at. A worker is
	// expired after MissedHeartbeats intervals of silence.
	HeartbeatInterval time.Duration
	MissedHeartbeats  int
	// StallTimeout is the progress watchdog: a started job that is not
	// paused by the throttler and shows no upload and no stderr for this
	// long is lost, so a network partition costs one restart instead of
	// the heartbeat limit.
	StallTimeout time.Duration
	// CheckInterval is the period of the heartbeat and stall checks.
	CheckInterval time.Duration
	// Quarantine keeps a worker that lost a running job (disconnect or
	// stall, not a drain) out of placement for this long.
	Quarantine time.Duration
	// HandoffTimeout is the longest a draining worker keeps a handed-off
	// job before job.kill when no replacement start comes.
	HandoffTimeout time.Duration
	// FFmpegMajor is the server's ffmpeg major version. A worker whose
	// ffmpeg has another major version is refused, because it runs the
	// server's argv (argv drift). 0 skips the check.
	FFmpegMajor int
	// Jobs stores the job records that let a job survive a server restart
	// (#618). Nil gives jobs no lease: a job stops with its connection.
	Jobs JobStore
	// Lease is the job lease that workers get at placement.
	Lease time.Duration
	// Replica is this server replica's ID in replica mode (#652); "" without
	// it. The job records carry it as their owner, welcome names it so that
	// the worker's job requests reach this replica, and a held job is adopted
	// only when ClaimJob takes its record.
	Replica  string
	ClaimJob func(ctx context.Context, id string) (bool, error)
	// Edge, when its Signer and URL are set, makes each worker mirror its
	// family files to the stream edge (#619).
	Edge EdgeTarget
	// ScaleHook, when its URL is set, POSTs the pool signals to an outside
	// scaler (#623). WarmupTTL is how long a PlaybackInfo transcode
	// decision counts in slotsWanted; DefaultWarmupTTL when zero.
	ScaleHook        ScaleHook
	WarmupTTL        time.Duration
	MaxArtifactBytes int64
	Logger           *slog.Logger
	// Observe receives lifecycle events. Tests use it to synchronize.
	Observe func(Event)
}

// EdgeTarget names the stream edge that workers mirror segments to.
type EdgeTarget struct {
	// URL is the edge base URL as workers reach it.
	URL    string
	Signer *edge.Signer
	// GrantLifetime is the life of an upload grant; DefaultEdgeGrantLifetime
	// when zero.
	GrantLifetime time.Duration
}

// DefaultEdgeGrantLifetime is the default life of an edge upload grant. It
// covers a long transcode; a job that outlives it mirrors no more files and
// still uploads to the server.
const DefaultEdgeGrantLifetime = 24 * time.Hour

// edgeGrant fills the edge fields of spec.
func (hub *Hub) edgeGrant(spec *JobSpec) {
	target := hub.config.Edge
	if target.Signer == nil || target.URL == "" {
		return
	}
	lifetime := target.GrantLifetime
	if lifetime <= 0 {
		lifetime = DefaultEdgeGrantLifetime
	}
	spec.EdgeURL = target.URL
	spec.EdgeGrant = target.Signer.SignUpload(edge.UploadGrant{
		Family:  edge.Family{Stem: strings.TrimSuffix(spec.Playlist, filepath.Ext(spec.Playlist)), Pattern: spec.SegmentPrefix + "%d" + spec.SegmentSuffix},
		Expires: hub.config.Clock.Now().Add(lifetime),
	})
}

// EventKind names a hub lifecycle event.
type EventKind string

// Hub lifecycle events.
const (
	EventEnrolled     EventKind = "enrolled"
	EventRegistered   EventKind = "registered"
	EventJobQueued    EventKind = "job-queued"
	EventFallback     EventKind = "fallback"
	EventRejected     EventKind = "rejected"
	EventExpired      EventKind = "expired"
	EventDisconnected EventKind = "disconnected"
	EventJobPlaced    EventKind = "job.placed"
	EventJobLost      EventKind = "job.lost"
	EventJobStalled   EventKind = "job.stalled"
	// EventArtifactRefused: a worker uploaded a malformed family file
	// and lost its job (#624).
	EventArtifactRefused EventKind = "artifact.refused"
	EventJobExited       EventKind = "job.exited"
	EventDraining        EventKind = "draining"
	EventJobHandoff      EventKind = "job.handoff"
	EventQuarantined     EventKind = "quarantined"
	EventJobAdopted      EventKind = "job.adopted"
	EventForgotten       EventKind = "forgotten"
	EventScaleHook       EventKind = "scale-hook"
	// EventScaleHookWait marks a scale hook loop that finished its calls
	// and waits for its timer or a wake.
	EventScaleHookWait EventKind = "scale-hook.wait"
	// EventJobRefused is a held job that the server killed at reattach.
	EventJobRefused EventKind = "job.refused"
)

// JobStore keeps the job records (#618). *store.TranscodeWorkerStore
// implements it.
type JobStore interface {
	CreateJob(ctx context.Context, job store.TranscodeWorkerJob) error
	Job(ctx context.Context, id string) (store.TranscodeWorkerJob, error)
	RenewJobs(ctx context.Context, ids []string, expires time.Time) error
	DeleteJob(ctx context.Context, id string) error
}

// Adopter registers a held job with the transcode manager.
// *ffmpeg.TranscodeManager implements it.
type Adopter interface {
	AdoptHLS(ctx context.Context, request ffmpeg.AdoptHLSRequest) error
}

// Event is one hub lifecycle event.
type Event struct {
	Kind     EventKind
	WorkerID string
	Name     string
	JobID    string
	Detail   string
}

// WorkerInfo is a snapshot of one registered worker.
type WorkerInfo struct {
	ID   string
	Name string
	// Static marks a worker that connected with a token from the server
	// environment; it has no stored identity.
	Static    bool
	InputMode string
	// Pools are the stored, operator-owned pool labels; a static worker
	// reports its own.
	Pools        []string
	Capabilities Capabilities
	ActiveJobs   int
	LastSeen     time.Time
}

// Hub accepts worker connections and implements ffmpeg.RemoteRunner. It owns
// one check goroutine (heartbeat expiry and the progress watchdog); the HTTP
// server owns the connection handlers, which Close ends and waits for.
type Hub struct {
	config HubConfig

	mu       sync.Mutex
	conns    map[*workerConn]struct{}
	workers  map[string]*workerConn
	jobs     map[string]*remoteJob
	closed   bool
	handlers sync.WaitGroup
	policy   Policy
	// queue holds the jobs that wait for a worker, oldest first.
	queue []*queuedJob
	// quarantine maps a worker ID to the end of its quarantine.
	quarantine map[string]time.Time
	// handoffs maps a playlist directory to the jobs a draining worker
	// still runs for it; job.kill follows the replacement start.
	handoffs map[string][]handoff
	metrics  metrics
	adopter  Adopter

	stop     chan struct{}
	loopDone chan struct{}
	// hookWake asks the scale hook loop for a call at once; hookDone closes
	// when the loop ends. Both are nil without a scale hook (#623).
	hookWake chan struct{}
	hookDone chan struct{}
	// warmups are the play sessions that PlaybackInfo announced a
	// transcode for, with their expiry. Guarded by mu.
	warmups map[string]time.Time
	mux     *http.ServeMux
}

// NewHub validates the configuration and starts the expiry loop.
func NewHub(config HubConfig) (*Hub, error) {
	if config.Clock == nil {
		return nil, errors.New("transcode worker hub clock is nil")
	}
	if config.Credentials == nil {
		return nil, errors.New("transcode worker hub has no credential source")
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if config.MissedHeartbeats <= 0 {
		config.MissedHeartbeats = DefaultMissedHeartbeats
	}
	if config.StallTimeout <= 0 {
		config.StallTimeout = DefaultStallTimeout
	}
	if config.CheckInterval <= 0 {
		config.CheckInterval = DefaultCheckInterval
	}
	if config.MaxArtifactBytes <= 0 {
		config.MaxArtifactBytes = defaultMaxArtifactBytes
	}
	if config.Quarantine <= 0 {
		config.Quarantine = DefaultQuarantine
	}
	if config.HandoffTimeout <= 0 {
		config.HandoffTimeout = DefaultHandoffTimeout
	}
	if config.Lease <= 0 {
		config.Lease = DefaultLease
	}
	hub := &Hub{
		config:     config,
		conns:      make(map[*workerConn]struct{}),
		workers:    make(map[string]*workerConn),
		jobs:       make(map[string]*remoteJob),
		stop:       make(chan struct{}),
		loopDone:   make(chan struct{}),
		policy:     config.Policy,
		quarantine: make(map[string]time.Time),
		handoffs:   make(map[string][]handoff),
		metrics:    newMetrics(),
		warmups:    make(map[string]time.Time),
	}
	if config.ScaleHook.URL != "" {
		hub.hookWake = make(chan struct{}, 1)
		hub.hookDone = make(chan struct{})
	}
	mux := http.NewServeMux()
	connect := websocket.Server{
		// Workers are not browsers; the token handshake authenticates.
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler:   hub.serveConn,
	}
	mux.HandleFunc("GET "+PathConnect, func(w http.ResponseWriter, r *http.Request) {
		// Until the operator creates the first join token, transcode
		// workers are off and the route does not exist.
		enabled, err := hub.config.Credentials.Enabled(r.Context())
		if err != nil {
			hub.logger().Error("transcode worker credential check failed", "error", err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !enabled {
			http.NotFound(w, r)
			return
		}
		connect.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET "+PathSource, hub.serveSource)
	mux.HandleFunc("PUT "+PathFile, hub.serveArtifact)
	mux.HandleFunc("GET "+PathAsset, hub.serveAsset)
	mux.HandleFunc("POST "+PathOutput, hub.serveOutput)
	hub.mux = mux
	go hub.checkLoop()
	if hub.hookDone != nil {
		go hub.scaleHookLoop()
	}
	return hub, nil
}

// ServeHTTP serves the worker routes. The caller strips the server base URL
// first, so the request path is one of PathConnect, PathSource or PathFile.
func (hub *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !secureRequest(r) && (hub.config.AllowPlainHTTP == nil || !hub.config.AllowPlainHTTP()) {
		http.Error(w, "transcode workers require TLS; the operator can allow plain HTTP", http.StatusForbidden)
		return
	}
	hub.mux.ServeHTTP(w, r)
}

// secureRequest reports a request over TLS, through a TLS-terminating proxy,
// or from loopback.
func secureRequest(r *http.Request) bool {
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Workers returns the registered workers, sorted by name.
func (hub *Hub) Workers() []WorkerInfo {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	values := make([]WorkerInfo, 0, len(hub.workers))
	for _, conn := range hub.workers {
		values = append(values, WorkerInfo{ID: conn.id, Name: conn.name, Static: conn.static, InputMode: conn.inputMode, Pools: append([]string(nil), conn.pools...), Capabilities: conn.caps, ActiveJobs: len(conn.jobs), LastSeen: conn.lastSeen})
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].Name != values[j].Name {
			return values[i].Name < values[j].Name
		}
		return values[i].ID < values[j].ID
	})
	return values
}

// Close disconnects every worker and stops the expiry loop. A job that the
// transcode manager released (#618) keeps running on its worker for the
// next server process; any other job is lost.
func (hub *Hub) Close(ctx context.Context) error {
	hub.mu.Lock()
	if !hub.closed {
		hub.closed = true
		close(hub.stop)
	}
	conns := make([]*workerConn, 0, len(hub.conns))
	for conn := range hub.conns {
		conns = append(conns, conn)
	}
	hub.mu.Unlock()
	for _, conn := range conns {
		conn.close()
	}
	done := make(chan struct{})
	go func() {
		<-hub.loopDone
		if hub.hookDone != nil {
			<-hub.hookDone
		}
		hub.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (hub *Hub) observe(event Event) {
	if hub.config.Observe != nil {
		hub.config.Observe(event)
	}
}

func (hub *Hub) logger() *slog.Logger {
	if hub.config.Logger != nil {
		return hub.config.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (hub *Hub) checkLoop() {
	defer close(hub.loopDone)
	timer := hub.config.Clock.NewTimer(hub.config.CheckInterval)
	defer timer.Stop()
	for {
		select {
		case <-hub.stop:
			return
		case <-timer.C():
		}
		hub.expire()
		hub.watchdog()
		hub.maintain()
		timer.Reset(hub.config.CheckInterval)
	}
}

// watchdog loses every started job that made no progress for StallTimeout
// while not paused. The worker slot stays reserved until the worker reports
// the exit or disconnects, as for Kill.
func (hub *Hub) watchdog() {
	now := hub.config.Clock.Now()
	var stalled []*remoteJob
	hub.mu.Lock()
	for _, job := range hub.jobs {
		if job.stalled(now, hub.config.StallTimeout) {
			stalled = append(stalled, job)
		}
	}
	hub.mu.Unlock()
	for _, job := range stalled {
		_ = job.conn.enqueue(Message{Type: TypeJobKill, JobID: job.id})
		if job.finish(ffmpeg.ErrRunnerLost) {
			hub.observe(Event{Kind: EventJobStalled, WorkerID: job.conn.id, Name: job.conn.name, JobID: job.id})
			hub.logger().Warn("transcode worker job stalled; restarting elsewhere", "worker", job.conn.name, "job", job.id, "stall_timeout", hub.config.StallTimeout)
			hub.lostJobs(job.conn, lossStall, 1)
		}
	}
}

// lostJobs counts jobs that a worker lost and quarantines the worker. A
// drained job is a handoff, not a fault: it counts, but quarantines nothing.
func (hub *Hub) lostJobs(conn *workerConn, reason string, count int) {
	if count == 0 {
		return
	}
	hub.mu.Lock()
	hub.metrics.lost[reason] += uint64(count)
	quarantine := reason != lossDrain && conn.id != ""
	if quarantine {
		hub.quarantine[conn.id] = hub.config.Clock.Now().Add(hub.config.Quarantine)
	}
	hub.mu.Unlock()
	if quarantine {
		hub.observe(Event{Kind: EventQuarantined, WorkerID: conn.id, Name: conn.name, Detail: reason})
		hub.logger().Warn("transcode worker quarantined after a lost job", "worker", conn.name, "id", conn.id, "reason", reason, "for", hub.config.Quarantine)
	}
}

// maintain ends quarantines and kills handed-off jobs whose replacement
// start never came.
func (hub *Hub) maintain() {
	now := hub.config.Clock.Now()
	var kills []*remoteJob
	hub.mu.Lock()
	ended := false
	for id, until := range hub.quarantine {
		if !now.Before(until) {
			delete(hub.quarantine, id)
			ended = true
		}
	}
	for directory, pending := range hub.handoffs {
		kept := pending[:0]
		for _, entry := range pending {
			if now.Sub(entry.since) >= hub.config.HandoffTimeout {
				kills = append(kills, entry.job)
			} else {
				kept = append(kept, entry)
			}
		}
		if len(kept) == 0 {
			delete(hub.handoffs, directory)
		} else {
			hub.handoffs[directory] = kept
		}
	}
	if ended {
		hub.drainQueueLocked()
	}
	var renew []*remoteJob
	for _, job := range hub.jobs {
		if job.leased() && job.leaseExpires.Sub(now) < hub.config.Lease/2 {
			renew = append(renew, job)
		}
	}
	hub.mu.Unlock()
	for _, job := range kills {
		_ = job.conn.enqueue(Message{Type: TypeJobKill, JobID: job.id})
	}
	hub.renewLeases(renew, now)
}

// renewLeases moves the stored lease of jobs to now plus the lease.
func (hub *Hub) renewLeases(jobs []*remoteJob, now time.Time) {
	if len(jobs) == 0 {
		return
	}
	ids := make([]string, len(jobs))
	for index, job := range jobs {
		ids[index] = job.id
	}
	expires := now.Add(hub.config.Lease)
	ctx, cancel := context.WithTimeout(context.Background(), jobStoreTimeout)
	defer cancel()
	if err := hub.config.Jobs.RenewJobs(ctx, ids, expires); err != nil {
		hub.logger().Warn("renew transcode worker job leases", "jobs", len(ids), "error", err.Error())
		return
	}
	hub.mu.Lock()
	for _, job := range jobs {
		job.leaseExpires = expires
	}
	hub.mu.Unlock()
}

// SetAdopter sets the manager that adopts the jobs that workers kept
// running across a server restart.
func (hub *Hub) SetAdopter(adopter Adopter) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.adopter = adopter
}

// handoff is a job of a draining worker that waits for its replacement.
type handoff struct {
	job   *remoteJob
	since time.Time
}

// drain stops placement on a worker and hands each of its jobs off: the job
// ends for the manager as a lost job, so the family restarts at the first
// segment the worker did not complete, on another host. The worker keeps
// the job until the replacement start (endHandoff) or HandoffTimeout.
func (hub *Hub) drain(conn *workerConn) {
	now := hub.config.Clock.Now()
	hub.mu.Lock()
	if conn.draining {
		hub.mu.Unlock()
		return
	}
	conn.draining = true
	jobs := make([]*remoteJob, 0, len(conn.jobs))
	for _, job := range conn.jobs {
		jobs = append(jobs, job)
		hub.handoffs[job.directory] = append(hub.handoffs[job.directory], handoff{job: job, since: now})
	}
	hub.mu.Unlock()
	hub.observe(Event{Kind: EventDraining, WorkerID: conn.id, Name: conn.name})
	hub.logger().Info("transcode worker draining; handing its jobs off", "worker", conn.name, "id", conn.id, "jobs", len(jobs))
	handedOff := 0
	for _, job := range jobs {
		job.startResult(ffmpeg.ErrRunnerLost)
		if job.finish(ffmpeg.ErrRunnerLost) {
			handedOff++
			hub.observe(Event{Kind: EventJobHandoff, WorkerID: conn.id, Name: conn.name, JobID: job.id})
		}
	}
	hub.lostJobs(conn, lossDrain, handedOff)
}

// endHandoff kills the draining worker's jobs for directory after the
// replacement start for it returned, whatever its outcome.
func (hub *Hub) endHandoff(directory string) {
	hub.mu.Lock()
	pending := hub.handoffs[directory]
	delete(hub.handoffs, directory)
	hub.mu.Unlock()
	for _, entry := range pending {
		_ = entry.job.conn.enqueue(Message{Type: TypeJobKill, JobID: entry.job.id})
	}
}

// stateLocked is the scaling state of a worker.
func (hub *Hub) stateLocked(conn *workerConn, now time.Time) string {
	switch {
	case conn.draining:
		return WorkerDraining
	case now.Before(hub.quarantine[conn.id]):
		return WorkerQuarantined
	}
	return WorkerReady
}

func (hub *Hub) expire() {
	now := hub.config.Clock.Now()
	limit := time.Duration(hub.config.MissedHeartbeats) * hub.config.HeartbeatInterval
	var victims []*workerConn
	hub.mu.Lock()
	for conn := range hub.conns {
		if now.Sub(conn.lastSeen) > limit {
			victims = append(victims, conn)
		}
	}
	hub.mu.Unlock()
	for _, conn := range victims {
		hub.observe(Event{Kind: EventExpired, WorkerID: conn.id, Name: conn.name})
		conn.close()
	}
}

// workerConn is one worker control connection.
type workerConn struct {
	hub  *Hub
	ws   *websocket.Conn
	send chan Message

	closed    chan struct{}
	closeOnce sync.Once

	// Fields below are guarded by hub.mu.
	id     string
	name   string
	pools  []string
	static bool
	// inputMode is shared, stream or auto; since orders pack placement.
	inputMode string
	since     time.Time
	caps      Capabilities
	// profile is profileKey(caps): workers with one profile share one
	// argv (#622).
	profile  string
	lastSeen time.Time
	jobs     map[string]*remoteJob
	// draining marks a worker that announced its stop (#617).
	draining bool
	// ephemeral marks a worker whose identity ends with its connection
	// (#623).
	ephemeral bool
}

func (conn *workerConn) close() {
	conn.closeOnce.Do(func() {
		close(conn.closed)
		_ = conn.ws.Close()
	})
}

// enqueue never blocks: a worker that does not drain its queue is dropped.
func (conn *workerConn) enqueue(message Message) error {
	select {
	case <-conn.closed:
		return errConnClosed
	default:
	}
	select {
	case conn.send <- message:
		return nil
	case <-conn.closed:
		return errConnClosed
	default:
		conn.close()
		return errSlowWorker
	}
}

func (conn *workerConn) writeLoop(done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-conn.closed:
			return
		case message := <-conn.send:
			if err := websocket.JSON.Send(conn.ws, message); err != nil {
				conn.close()
				return
			}
		}
	}
}

func (hub *Hub) serveConn(ws *websocket.Conn) {
	ws.MaxPayloadBytes = maxControlMessageBytes
	conn := &workerConn{
		hub:    hub,
		ws:     ws,
		send:   make(chan Message, sendQueueLength),
		closed: make(chan struct{}),
		jobs:   make(map[string]*remoteJob),
	}
	hub.mu.Lock()
	if hub.closed {
		hub.mu.Unlock()
		_ = ws.Close()
		return
	}
	conn.lastSeen = hub.config.Clock.Now()
	hub.conns[conn] = struct{}{}
	hub.handlers.Add(1)
	hub.mu.Unlock()
	defer hub.handlers.Done()

	writerDone := make(chan struct{})
	go conn.writeLoop(writerDone)
	defer func() {
		conn.close()
		<-writerDone
		hub.drop(conn)
	}()
	if !hub.handshake(ws.Request().Context(), conn) {
		return
	}
	hub.readLoop(conn)
}

func (hub *Hub) handshake(ctx context.Context, conn *workerConn) bool {
	reject := func(reason string) bool {
		_ = websocket.JSON.Send(conn.ws, Message{Type: TypeReject, Error: reason})
		hub.observe(Event{Kind: EventRejected, Detail: reason})
		return false
	}
	var hello Message
	if err := websocket.JSON.Receive(conn.ws, &hello); err != nil {
		return false
	}
	if hello.Type != TypeHello || hello.Proto != ProtocolVersion {
		return reject("unsupported protocol")
	}
	if len(hello.Nonce) < 16 || (hello.TokenID == "") == (hello.WorkerID == "") {
		return reject("unknown token")
	}
	var credential Credential
	var err error
	if hello.WorkerID != "" {
		credential, err = hub.config.Credentials.Worker(ctx, hello.WorkerID)
	} else {
		credential, err = hub.config.Credentials.JoinToken(ctx, hello.TokenID)
	}
	if errors.Is(err, ErrUnknownCredential) {
		return reject("unknown token")
	}
	if err != nil {
		hub.logger().Error("transcode worker credential lookup failed", "error", err.Error())
		return false
	}
	serverNonce, err := newNonce()
	if err != nil {
		return false
	}
	key := credential.Key
	challenge := Message{Type: TypeChallenge, Nonce: serverNonce, Proof: proof(key, "server", hello.Nonce, serverNonce)}
	if err := websocket.JSON.Send(conn.ws, challenge); err != nil {
		return false
	}
	var auth Message
	if err := websocket.JSON.Receive(conn.ws, &auth); err != nil {
		return false
	}
	if auth.Type != TypeAuth || !hmac.Equal(auth.Proof, proof(key, "worker", hello.Nonce, serverNonce)) {
		return reject("authentication failed")
	}
	if auth.Capabilities == nil || auth.Capabilities.MaxJobs < 1 || auth.Capabilities.MaxJobs > maxWorkerJobs {
		return reject("invalid capabilities")
	}
	if err := validateHardware(*auth.Capabilities); err != nil {
		return reject("invalid capabilities: " + err.Error())
	}
	if want := hub.config.FFmpegMajor; want > 0 {
		if major, ok := ffmpegMajor(auth.Capabilities.FFmpegVersion); !ok || major != want {
			hub.logger().Warn("transcode worker refused: its ffmpeg major version differs from the server's", "worker", hello.Name, "worker_ffmpeg", auth.Capabilities.FFmpegVersion, "server_ffmpeg_major", want)
			return reject(fmt.Sprintf("ffmpeg %q is not major version %d like the server's", auth.Capabilities.FFmpegVersion, want))
		}
	}
	id, name, pools, inputMode := hello.WorkerID, credential.Name, credential.Pools, credential.InputMode
	switch {
	case credential.Static:
		// A static token is a fleet credential: the worker name tells its
		// workers apart, and the worker owns its pool labels.
		name = strings.TrimSpace(hello.Name)
		if name == "" {
			name = hello.TokenID
		}
		id = "static-" + hello.TokenID + "-" + name
		pools = store.NormalizeTranscodeWorkerPools(auth.Capabilities.Pools)
		inputMode = workerInputMode(auth.Capabilities.InputMode)
	case id == "":
		// First connection with a join token: mint the worker identity.
		secret, err := newSecret()
		if err != nil {
			return false
		}
		if id, err = randomID(); err != nil {
			return false
		}
		name, pools = strings.TrimSpace(hello.Name), store.NormalizeTranscodeWorkerPools(auth.Capabilities.Pools)
		if name == "" {
			name = id
		}
		inputMode = workerInputMode(auth.Capabilities.InputMode)
		err = hub.config.Credentials.Enroll(ctx, Enrollment{JoinTokenID: hello.TokenID, WorkerID: id, Key: deriveKey(secret), Name: name, Pools: pools, InputMode: inputMode})
		if errors.Is(err, store.ErrJoinTokenUnusable) {
			return reject("join token is revoked, expired or spent")
		}
		if err != nil {
			hub.logger().Error("transcode worker enrollment failed", "worker", hello.Name, "error", err.Error())
			return reject("enrollment failed")
		}
		if err := websocket.JSON.Send(conn.ws, Message{Type: TypeEnrolled, WorkerID: id, WorkerSecret: secret}); err != nil {
			return false
		}
		hub.observe(Event{Kind: EventEnrolled, WorkerID: id, Name: name})
		hub.logger().Info("transcode worker enrolled", "worker", name, "id", id, "join_token", hello.TokenID)
	default:
		if err := hub.config.Credentials.Connected(ctx, id); err != nil {
			hub.logger().Warn("record transcode worker connection", "id", id, "error", err.Error())
		}
	}
	pins, err := hub.config.Credentials.Pins(ctx)
	if err != nil {
		hub.logger().Warn("read transcode worker pins", "error", err.Error())
	}
	hub.mu.Lock()
	conn.id = id
	conn.name = name
	conn.pools = pools
	conn.inputMode = workerInputMode(inputMode)
	conn.since = hub.config.Clock.Now()
	conn.static = credential.Static
	conn.ephemeral = hello.Ephemeral && !credential.Static
	conn.caps = *auth.Capabilities
	conn.profile = profileKey(conn.caps)
	conn.lastSeen = hub.config.Clock.Now()
	hub.mu.Unlock()
	// The welcome goes out before the worker can take a job, so no job
	// message overtakes it.
	welcome := Message{Type: TypeWelcome, WorkerID: id, HeartbeatMillis: hub.config.HeartbeatInterval.Milliseconds(), Pins: pins, Replica: hub.config.Replica}
	if err := websocket.JSON.Send(conn.ws, welcome); err != nil {
		return false
	}
	hub.mu.Lock()
	previous := hub.workers[id]
	hub.workers[id] = conn
	hub.drainQueueLocked()
	hub.mu.Unlock()
	if previous != nil {
		// A reconnect replaces a half-open connection of the same worker.
		previous.close()
	}
	hub.observe(Event{Kind: EventRegistered, WorkerID: id, Name: name})
	hub.logger().Info("transcode worker registered", "worker", name, "id", id, "max_jobs", conn.caps.MaxJobs, "hwaccels", conn.caps.HWAccels, "hardware", hardwareType(conn.caps), "pools", pools)
	return true
}

// Disconnect closes the live connection of a worker, if any. Its running
// jobs are lost and restart elsewhere. The admin API calls it after it
// revokes the worker.
func (hub *Hub) Disconnect(workerID string) {
	hub.mu.Lock()
	conn := hub.workers[workerID]
	hub.mu.Unlock()
	if conn != nil {
		conn.close()
	}
}

// SetPools updates the pool labels of a connected worker after the operator
// changes them.
func (hub *Hub) SetPools(workerID string, pools []string) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if conn := hub.workers[workerID]; conn != nil {
		conn.pools = append([]string(nil), pools...)
		hub.drainQueueLocked()
	}
}

// SetInputMode updates the input mode of a connected worker after the
// operator changes it.
func (hub *Hub) SetInputMode(workerID, mode string) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if conn := hub.workers[workerID]; conn != nil {
		conn.inputMode = workerInputMode(mode)
		hub.drainQueueLocked()
	}
}

// SetPolicy replaces the placement policy. Queued jobs are placed again.
func (hub *Hub) SetPolicy(policy Policy) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.policy = policy
	hub.drainQueueLocked()
}

// Policy returns the placement policy.
func (hub *Hub) Policy() Policy {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.policy
}

func workerInputMode(mode string) string {
	if store.ValidTranscodeWorkerInputMode(mode) {
		return mode
	}
	return store.TranscodeWorkerInputAuto
}

// BroadcastPins sends the extra server key fingerprints to every connected
// worker, so pinned workers accept the next certificate key before the
// rotation.
func (hub *Hub) BroadcastPins(pins []string) {
	hub.mu.Lock()
	conns := make([]*workerConn, 0, len(hub.workers))
	for _, conn := range hub.workers {
		conns = append(conns, conn)
	}
	hub.mu.Unlock()
	for _, conn := range conns {
		_ = conn.enqueue(Message{Type: TypePins, Pins: append([]string(nil), pins...)})
	}
}

func (hub *Hub) readLoop(conn *workerConn) {
	for {
		var message Message
		if err := websocket.JSON.Receive(conn.ws, &message); err != nil {
			return
		}
		hub.mu.Lock()
		conn.lastSeen = hub.config.Clock.Now()
		job := conn.jobs[message.JobID]
		hub.mu.Unlock()
		switch message.Type {
		case TypeHeartbeat:
		case TypeDrain:
			hub.drain(conn)
		case TypeJobStarted:
			if job != nil {
				job.progress(hub.config.Clock.Now())
				job.startResult(nil)
			}
		case TypeJobFailed:
			if job != nil {
				job.startResult(fmt.Errorf("transcode worker %s: %s", conn.name, message.Error))
			}
		case TypeJobStderr:
			if job != nil {
				job.progress(hub.config.Clock.Now())
				job.writeStderr(message.Data)
			}
		case TypeJobExited:
			hub.jobExited(conn, message.JobID, message.Error)
		case TypeJobsHeld:
			hub.reattach(conn, message.Held)
		}
	}
}

// drop unregisters a closed connection and loses its running jobs.
func (hub *Hub) drop(conn *workerConn) {
	hub.mu.Lock()
	delete(hub.conns, conn)
	// An ephemeral identity ends with its last connection, but not when the
	// server stops: the worker keeps its jobs for the next server process.
	forget := false
	if conn.id != "" && hub.workers[conn.id] == conn {
		delete(hub.workers, conn.id)
		forget = conn.ephemeral && !hub.closed
	}
	jobs := make([]*remoteJob, 0, len(conn.jobs))
	for id, job := range conn.jobs {
		jobs = append(jobs, job)
		delete(conn.jobs, id)
	}
	for directory, pending := range hub.handoffs {
		kept := pending[:0]
		for _, entry := range pending {
			if entry.job.conn != conn {
				kept = append(kept, entry)
			}
		}
		if len(kept) == 0 {
			delete(hub.handoffs, directory)
		} else {
			hub.handoffs[directory] = kept
		}
	}
	hub.mu.Unlock()
	lost := 0
	for _, job := range jobs {
		job.startResult(ffmpeg.ErrRunnerLost)
		if job.finish(ffmpeg.ErrRunnerLost) {
			lost++
			hub.observe(Event{Kind: EventJobLost, WorkerID: conn.id, Name: conn.name, JobID: job.id})
		}
	}
	hub.lostJobs(conn, lossDisconnect, lost)
	if forget {
		hub.forget(conn)
	}
	if conn.id != "" {
		hub.observe(Event{Kind: EventDisconnected, WorkerID: conn.id, Name: conn.name})
		hub.logger().Warn("transcode worker disconnected", "worker", conn.name, "id", conn.id, "lost_jobs", len(jobs))
	}
}

// forget deletes the identity of an ephemeral worker that disconnected.
func (hub *Hub) forget(conn *workerConn) {
	ctx, cancel := context.WithTimeout(context.Background(), jobStoreTimeout)
	defer cancel()
	if err := hub.config.Credentials.Forget(ctx, conn.id); err != nil {
		hub.logger().Warn("delete ephemeral transcode worker identity", "worker", conn.name, "id", conn.id, "error", err.Error())
		return
	}
	hub.observe(Event{Kind: EventForgotten, WorkerID: conn.id, Name: conn.name})
	hub.logger().Info("ephemeral transcode worker identity deleted", "worker", conn.name, "id", conn.id)
}

func (hub *Hub) jobExited(conn *workerConn, id, reason string) {
	hub.mu.Lock()
	job := conn.jobs[id]
	delete(conn.jobs, id)
	hub.drainQueueLocked()
	hub.mu.Unlock()
	if job == nil {
		return
	}
	var err error
	if reason != "" {
		err = fmt.Errorf("transcode worker %s: ffmpeg: %s", conn.name, reason)
	}
	job.startResult(err)
	job.finish(err)
	hub.observe(Event{Kind: EventJobExited, WorkerID: conn.id, Name: conn.name, JobID: id, Detail: reason})
}

// StartHLS implements ffmpeg.RemoteRunner.
func (hub *Hub) StartHLS(ctx context.Context, request ffmpeg.RemoteHLSJob) (ffmpeg.RemoteProcess, error) {
	// A replacement for a handed-off job ends the handoff when its start
	// returns, whatever the outcome.
	defer hub.endHandoff(request.Directory)
	return hub.startJob(ctx, request)
}

// StartProgressive implements ffmpeg.RemoteProgressiveRunner (#624): the
// worker streams the output back through PathOutput, and the hub appends
// it to job.Output. The job has no lease, edge mirror or handoff.
func (hub *Hub) StartProgressive(ctx context.Context, job ffmpeg.RemoteProgressiveJob) (ffmpeg.RemoteProcess, error) {
	return hub.startJob(ctx, ffmpeg.RemoteHLSJob{
		Args: job.Args, SourcePath: job.SourcePath, Source: job.Source, SourceSize: job.SourceSize,
		Directory: filepath.Dir(job.Output), Playlist: job.Output,
		PlaySessionID: job.PlaySessionID, DeviceID: job.DeviceID, LocalJobs: job.LocalJobs, Stderr: job.Stderr,
	})
}

// progressive reports a request that StartProgressive made: it has one
// output, in Playlist, and no segment pattern.
func progressive(request ffmpeg.RemoteHLSJob) bool { return request.SegmentPattern == "" }

// startJob places one HLS family or progressive transcode.
func (hub *Hub) startJob(ctx context.Context, request ffmpeg.RemoteHLSJob) (ffmpeg.RemoteProcess, error) {
	family, err := familyOf(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ffmpeg.ErrNoRemoteRunner, err)
	}
	assets, jobAssets, err := checkAssets(request.Assets)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ffmpeg.ErrNoRemoteRunner, err)
	}
	plan := newJobPlan(request)
	if plan.server.err != nil && request.Build == nil {
		return nil, fmt.Errorf("%w: %v", ffmpeg.ErrNoRemoteRunner, plan.server.err)
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	token, err := randomID()
	if err != nil {
		return nil, err
	}
	sourceCtx, cancel := context.WithCancel(context.Background())
	job := &remoteJob{
		hub:          hub,
		id:           id,
		token:        token,
		tokenHash:    tokenHash(token),
		directory:    request.Directory,
		playlist:     family.Playlist,
		output:       family.Output,
		prefix:       family.SegmentPrefix,
		suffix:       family.SegmentSuffix,
		source:       request.Source,
		sourceSize:   request.SourceSize,
		assets:       assets,
		sourceCtx:    sourceCtx,
		cancelSource: cancel,
		stderr:       request.Stderr,
		started:      make(chan error, 1),
		growing:      make(map[string]bool),
		done:         make(chan struct{}),
	}
	local := LocalState{ActiveJobs: request.LocalJobs, GPU: request.Facts.Hardware != "" && !request.RemoteOnly, RemoteOnly: request.RemoteOnly}
	if !request.RemoteOnly {
		local.Tier = plan.server.tier
	}

	hub.mu.Lock()
	if hub.closed {
		hub.mu.Unlock()
		cancel()
		return nil, ffmpeg.ErrNoRemoteRunner
	}
	// The job that a PlaybackInfo warm-up announced has come (#623).
	delete(hub.warmups, request.PlaySessionID)
	decision := Place(request.Facts, hub.policy, hub.snapshotLocked(plan), local)
	var conn *workerConn
	switch decision.Kind {
	case DecideWorker:
		conn = hub.workers[decision.WorkerID]
		hub.assignLocked(conn, job, plan)
		hub.metrics.placements[[2]string{DecideWorker.String(), decision.Rule}]++
		hub.mu.Unlock()
		hub.logPlaced(conn, decision.Rule, decision.Tier, request.RemoteOnly)
	case DecideQueue:
		waiter := &queuedJob{facts: request.Facts, plan: plan, local: local, job: job, assigned: make(chan *workerConn, 1), pools: decision.Pools, since: hub.config.Clock.Now(), rule: decision.Rule}
		hub.queue = append(hub.queue, waiter)
		hub.mu.Unlock()
		hub.wakeScaleHook()
		conn = hub.waitQueued(ctx, waiter, decision)
		if conn == nil {
			cancel()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, hub.fallback(*decision.Then)
		}
	default:
		hub.mu.Unlock()
		cancel()
		return nil, hub.fallback(decision)
	}
	hub.mu.Lock()
	spec := job.spec
	spec.ID, spec.Token = job.id, job.token
	spec.SourceURL = pathJobs + spec.ID + "/source"
	spec.Assets = jobAssets
	switch job.inputMode {
	case store.TranscodeWorkerInputShared:
		spec.SourceURL = ""
	case store.TranscodeWorkerInputStream:
		spec.SourcePath = ""
	}
	inputMode := job.inputMode
	hub.mu.Unlock()
	if !progressive(request) {
		hub.edgeGrant(&spec)
	}
	spec.LeaseMillis = hub.createRecord(job, conn, request, inputMode)

	abandon := func(err error) (ffmpeg.RemoteProcess, error) {
		_ = conn.enqueue(Message{Type: TypeJobKill, JobID: job.id})
		hub.mu.Lock()
		delete(conn.jobs, job.id)
		hub.drainQueueLocked()
		hub.mu.Unlock()
		// The caller still owns Stderr when the start fails.
		job.mu.Lock()
		job.stderr = nil
		job.mu.Unlock()
		job.finish(err)
		return nil, err
	}
	if err := conn.enqueue(Message{Type: TypeJobStart, Job: &spec}); err != nil {
		return abandon(err)
	}
	select {
	case err := <-job.started:
		if err != nil {
			return abandon(err)
		}
	case <-ctx.Done():
		return abandon(ctx.Err())
	}
	hub.observe(Event{Kind: EventJobPlaced, WorkerID: conn.id, Name: conn.name, JobID: job.id, Detail: decision.Rule})
	return job, nil
}

// logPlaced logs one placement on a worker with the hardware tier of its
// argv there (#854).
func (hub *Hub) logPlaced(conn *workerConn, rule string, tier int, remoteOnly bool) {
	hub.logger().Info("transcode job placed", "worker", conn.name, "id", conn.id, "rule", rule, "tier", tier, "remote_only", remoteOnly)
}

// queuedJob is a job that waits for a worker of its pools.
type queuedJob struct {
	facts JobFacts
	plan  *jobPlan
	local LocalState
	job   *remoteJob
	// assigned receives the worker that took the job.
	assigned chan *workerConn
	// pools, since and rule feed the scaling signals and metrics.
	pools []string
	since time.Time
	rule  string
}

// waitQueued waits for a worker until timeout, ctx or hub close. It returns
// nil when no worker took the job; the job has then left the queue.
func (hub *Hub) waitQueued(ctx context.Context, waiter *queuedJob, decision Decision) *workerConn {
	timer := hub.config.Clock.NewTimer(decision.QueueTimeout)
	defer timer.Stop()
	hub.observe(Event{Kind: EventJobQueued, JobID: waiter.job.id, Detail: strings.Join(decision.Pools, ",")})
	timedOut := false
	select {
	case conn := <-waiter.assigned:
		return conn
	case <-timer.C():
		timedOut = true
	case <-ctx.Done():
	case <-hub.stop:
	}
	hub.mu.Lock()
	removed := false
	for index, queued := range hub.queue {
		if queued == waiter {
			hub.queue = append(hub.queue[:index], hub.queue[index+1:]...)
			removed = true
			break
		}
	}
	if removed && timedOut {
		hub.metrics.observeWaitLocked(waiter.pools, waitTimeout, hub.config.Clock.Now().Sub(waiter.since).Seconds())
	}
	hub.mu.Unlock()
	if removed {
		return nil
	}
	// A worker took the job while the wait ended: use it.
	return <-waiter.assigned
}

// drainQueueLocked gives freed or new worker slots to the queued jobs,
// oldest first.
func (hub *Hub) drainQueueLocked() {
	if hub.closed || len(hub.queue) == 0 {
		return
	}
	kept := make([]*queuedJob, 0, len(hub.queue))
	for _, waiter := range hub.queue {
		decision := Place(waiter.facts, hub.policy, hub.snapshotLocked(waiter.plan), waiter.local)
		if decision.Kind != DecideWorker {
			kept = append(kept, waiter)
			continue
		}
		conn := hub.workers[decision.WorkerID]
		hub.assignLocked(conn, waiter.job, waiter.plan)
		hub.metrics.placements[[2]string{DecideWorker.String(), waiter.rule}]++
		hub.metrics.observeWaitLocked(waiter.pools, waitPlaced, hub.config.Clock.Now().Sub(waiter.since).Seconds())
		hub.logPlaced(conn, waiter.rule, decision.Tier, waiter.local.RemoteOnly)
		waiter.assigned <- conn
	}
	hub.queue = kept
}

// QueueLength reports the jobs that wait for a worker.
func (hub *Hub) QueueLength() int {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return len(hub.queue)
}

// assignLocked places job on conn with the argv that plan builds for it.
func (hub *Hub) assignLocked(conn *workerConn, job *remoteJob, plan *jobPlan) {
	form := plan.forWorker(conn.caps, conn.profile)
	job.spec = form.spec
	job.hardware = form.hardware
	job.conn = conn
	job.inputMode = conn.inputMode
	conn.jobs[job.id] = job
	hub.jobs[job.id] = job
}

// snapshotLocked is the fleet as placement sees it for one job, in a
// stable order.
func (hub *Hub) snapshotLocked(plan *jobPlan) []WorkerSnapshot {
	now := hub.config.Clock.Now()
	fleet := make([]WorkerSnapshot, 0, len(hub.workers))
	for _, conn := range hub.workers {
		fits, tier := plan.fits(conn)
		fleet = append(fleet, WorkerSnapshot{
			ID: conn.id, Pools: conn.pools, Fits: fits, Tier: tier, InputMode: conn.inputMode,
			Shared: coversPath(conn.caps.PathPrefixes, plan.request.SourcePath), ActiveJobs: len(conn.jobs), MaxJobs: conn.caps.MaxJobs, Since: conn.since,
			Closed: hub.stateLocked(conn, now) != WorkerReady,
		})
	}
	sort.Slice(fleet, func(i, j int) bool { return fleet[i].ID < fleet[j].ID })
	return fleet
}

func coversPath(prefixes []string, path string) bool {
	if path == "" {
		return false
	}
	for _, prefix := range prefixes {
		if prefix = strings.TrimRight(prefix, "/"); prefix != "" && strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// fallback turns a local or refused decision into the runner's answer. A
// chain step taken because no worker fit logs one warning.
func (hub *Hub) fallback(decision Decision) error {
	hub.mu.Lock()
	hub.metrics.placements[[2]string{decision.Kind.String(), decision.Rule}]++
	if decision.Fallback {
		hub.metrics.fallbacks[[2]string{decision.Kind.String(), decision.Reason}]++
	}
	hub.mu.Unlock()
	if decision.Fallback {
		hub.observe(Event{Kind: EventFallback, Detail: decision.Kind.String() + ":" + decision.Reason})
		hub.logger().Warn("transcode placement fell back", "target", decision.Kind.String(), "rule", decision.Rule, "pools", decision.Pools, "reason", decision.Reason, "tier", decision.Tier)
	}
	if decision.Kind == DecideRefuse {
		return fmt.Errorf("%w: %s", ffmpeg.ErrRemoteRefused, decision.Reason)
	}
	return ffmpeg.ErrNoRemoteRunner
}

// requirement is what an argv needs from a worker.
type requirement struct {
	hwaccels []string
	encoders []string
	// decoders are the video decoders the argv names before its input.
	decoders []string
	// hwdecode reports a hardware decode: a -hwaccel family or a hardware
	// decoder before the input (#854).
	hwdecode bool
}

func (need requirement) satisfiedBy(caps Capabilities) bool {
	for _, family := range need.hwaccels {
		if !contains(caps.HWAccels, family) {
			return false
		}
	}
	if len(caps.Encoders) > 0 {
		for _, encoder := range need.encoders {
			if !contains(caps.Encoders, encoder) {
				return false
			}
		}
	}
	if caps.Encoding != nil && len(caps.Encoding.Decoders) > 0 {
		for _, decoder := range need.decoders {
			if !contains(caps.Encoding.Decoders, decoder) {
				return false
			}
		}
	}
	return true
}

// videoCodecOption reports an ffmpeg video codec option: -c:v, -codec:v,
// -vcodec, and the stream-indexed -c:v:0 and -codec:v:0.
func videoCodecOption(option string) bool {
	if option == "-vcodec" {
		return true
	}
	for _, prefix := range []string{"-c:v", "-codec:v"} {
		if option == prefix || strings.HasPrefix(option, prefix+":") {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

// buildJobSpec rewrites the server argv into worker form. It refuses any
// argv that names a server path it cannot map.
func buildJobSpec(request ffmpeg.RemoteHLSJob) (JobSpec, requirement, error) {
	return buildJobSpecArgs(request, request.Args)
}

// familyOf returns the family names of request: the playlist, and the
// segment prefix and suffix around the index.
func familyOf(request ffmpeg.RemoteHLSJob) (JobSpec, error) {
	if request.SourcePath == "" || request.Source == nil {
		return JobSpec{}, errors.New("job has no pinned source")
	}
	if progressive(request) {
		output := filepath.Base(request.Playlist)
		if filepath.Dir(request.Playlist) != request.Directory || !validArtifactName(output) {
			return JobSpec{}, errors.New("progressive output name is not portable")
		}
		return JobSpec{Output: output, SourcePath: request.SourcePath, SourceSize: request.SourceSize}, nil
	}
	playlist := filepath.Base(request.Playlist)
	pattern := filepath.Base(request.SegmentPattern)
	if filepath.Dir(request.Playlist) != request.Directory || filepath.Dir(request.SegmentPattern) != request.Directory {
		return JobSpec{}, errors.New("outputs are outside the family directory")
	}
	match := segmentDirective.FindStringIndex(pattern)
	if match == nil {
		return JobSpec{}, errors.New("segment pattern has no index directive")
	}
	if pattern[match[0]:match[1]] != "%d" {
		return JobSpec{}, errors.New("segment pattern directive is not %d")
	}
	spec := JobSpec{
		Playlist:      playlist,
		SegmentPrefix: pattern[:match[0]],
		SegmentSuffix: pattern[match[1]:],
		SourcePath:    request.SourcePath,
		SourceSize:    request.SourceSize,
	}
	if !validArtifactName(playlist) || !validArtifactName(spec.SegmentPrefix+"0"+spec.SegmentSuffix) {
		return JobSpec{}, errors.New("output names are not portable")
	}
	return spec, nil
}

// buildJobSpecArgs rewrites argv, an argv of request for one host, into
// worker form.
func buildJobSpecArgs(request ffmpeg.RemoteHLSJob, argv []string) (JobSpec, requirement, error) {
	var need requirement
	spec, err := familyOf(request)
	if err != nil {
		return JobSpec{}, need, err
	}
	playlist, pattern := spec.Playlist, filepath.Base(request.SegmentPattern)
	if progressive(request) {
		playlist, pattern = spec.Output, ""
	}
	sourceOperand := "file:" + request.SourcePath
	sources := 0
	// The codec options before the source input select decoders.
	input := len(argv)
	for index := 1; index < len(argv); index++ {
		if argv[index] == sourceOperand && argv[index-1] == "-i" {
			input = index
			break
		}
	}
	args := make([]string, len(argv))
	for index, argument := range argv {
		switch {
		case argument == sourceOperand && index > 0 && argv[index-1] == "-i":
			args[index] = PlaceholderSource
			sources++
			continue
		case argument == request.Playlist:
			args[index] = PlaceholderOut + "/" + playlist
			continue
		case pattern != "" && argument == request.SegmentPattern:
			args[index] = PlaceholderOut + "/" + pattern
			continue
		case strings.Contains(argument, request.Directory), strings.Contains(argument, request.SourcePath),
			strings.HasPrefix(argument, "fd:"), strings.HasPrefix(argument, "pipe:"), strings.Contains(argument, "/proc/self/fd"):
			return JobSpec{}, need, fmt.Errorf("argv operand %d names a server-local resource", index)
		}
		for _, match := range absolutePath.FindAllStringSubmatch(argument, -1) {
			// Device nodes are the only absolute paths a worker may
			// open; the worker must expose the same node.
			if !strings.HasPrefix(match[1], "/dev/") {
				return JobSpec{}, need, fmt.Errorf("argv operand %d names a server path", index)
			}
		}
		args[index] = argument
		if index == 0 {
			continue
		}
		switch argv[index-1] {
		case "-hwaccel":
			if argument != "none" && argument != "auto" {
				need.hwaccels = append(need.hwaccels, argument)
				need.hwdecode = true
			}
		case "-init_hw_device":
			family := argument
			if cut := strings.IndexAny(family, "=:@"); cut >= 0 {
				family = family[:cut]
			}
			need.hwaccels = append(need.hwaccels, family)
		}
		if videoCodecOption(argv[index-1]) && argument != "copy" {
			if index < input {
				need.decoders = append(need.decoders, argument)
				need.hwdecode = need.hwdecode || hasSuffix(argument, hardwareDecoderSuffixes)
			} else {
				need.encoders = append(need.encoders, argument)
			}
		}
	}
	if sources != 1 {
		return JobSpec{}, need, errors.New("argv must name the source exactly once")
	}
	spec.Args = args
	return spec, need, nil
}

// checkAssets validates the asset names and returns them by name and in
// the job spec form.
func checkAssets(list []ffmpeg.RemoteAsset) (map[string]ffmpeg.RemoteAsset, []JobAsset, error) {
	if len(list) == 0 {
		return nil, nil, nil
	}
	assets := make(map[string]ffmpeg.RemoteAsset, len(list))
	specs := make([]JobAsset, 0, len(list))
	for _, asset := range list {
		if !assetName.MatchString(asset.Name) || asset.File == nil || asset.Size < 0 {
			return nil, nil, fmt.Errorf("job asset %q is invalid", asset.Name)
		}
		if _, dup := assets[asset.Name]; dup {
			return nil, nil, fmt.Errorf("job asset %q is duplicated", asset.Name)
		}
		assets[asset.Name] = asset
		specs = append(specs, JobAsset{Name: asset.Name, Size: asset.Size})
	}
	return assets, specs, nil
}

// remoteJob is the server half of one placed job. It implements
// ffmpeg.RemoteProcess.
type remoteJob struct {
	hub  *Hub
	conn *workerConn
	id   string
	// token is the data-plane token of a job placed by this process; an
	// adopted job has only tokenHash.
	token     string
	tokenHash []byte
	directory string
	playlist  string
	// output is the one output of a progressive job (#624); empty for an
	// HLS family.
	output string
	// written is the size of the progressive output so far, guarded by mu.
	written      int64
	prefix       string
	suffix       string
	source       *os.File
	sourceSize   int64
	sourceCtx    context.Context
	cancelSource context.CancelFunc
	// assets are the burn-in files by name (#624); read-only after the
	// start.
	assets  map[string]ffmpeg.RemoteAsset
	started chan error
	// inputMode is the worker's input mode at placement. A shared job
	// has no source route.
	inputMode string
	// spec is the worker form of the job on conn, and hardware reports
	// that its argv uses the hardware family (#622). Guarded by hub.mu.
	spec     JobSpec
	hardware bool
	// record marks a job with a stored record (#618); leaseExpires is its
	// stored lease, guarded by hub.mu. ownsSource marks a source file
	// that the hub opened at adoption and closes at the end.
	record       bool
	leaseExpires time.Time
	ownsSource   bool

	mu       sync.Mutex
	stderr   io.WriteCloser
	closed   bool
	growing  map[string]bool
	inflight sync.WaitGroup
	// running, lastProgress, paused and quitting feed the watchdog.
	running      bool
	lastProgress time.Time
	paused       bool
	quitting     bool

	doneOnce sync.Once
	done     chan struct{}
	err      error
}

func (job *remoteJob) startResult(err error) {
	select {
	case job.started <- err:
	default:
	}
}

// progress records worker activity for the watchdog. The first call marks
// the job started.
func (job *remoteJob) progress(now time.Time) {
	job.mu.Lock()
	job.running = true
	job.lastProgress = now
	job.mu.Unlock()
}

// keystroke tracks the throttle and stop keys the manager sends: "p" or "c"
// pauses ffmpeg, "u" or a newline resumes it, "q" stops it. A paused or
// stopping job makes no progress by design.
func (job *remoteJob) keystroke(data []byte, now time.Time) {
	job.mu.Lock()
	defer job.mu.Unlock()
	switch {
	case len(data) == 0:
	case data[0] == 'q':
		job.quitting = true
	case string(data) == "p" || string(data) == "c":
		job.paused = true
	case string(data) == "u" || string(data) == "\n":
		job.paused = false
		job.lastProgress = now
	}
}

func (job *remoteJob) stalled(now time.Time, limit time.Duration) bool {
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.running && !job.closed && !job.paused && !job.quitting && now.Sub(job.lastProgress) > limit
}

func (job *remoteJob) writeStderr(data []byte) {
	job.mu.Lock()
	stderr := job.stderr
	job.mu.Unlock()
	if stderr != nil {
		_, _ = stderr.Write(data)
	}
}

// finish ends the job once. No artifact write lands after it returns. It
// reports whether this call ended the job.
func (job *remoteJob) finish(err error) bool {
	ended := false
	job.doneOnce.Do(func() {
		ended = true
		job.mu.Lock()
		job.closed = true
		stderr := job.stderr
		job.stderr = nil
		job.mu.Unlock()
		job.cancelSource()
		job.inflight.Wait()
		// A growing placeholder that never completed must not look like
		// a cached segment to the restart path.
		job.mu.Lock()
		for name := range job.growing {
			_ = os.Remove(filepath.Join(job.directory, name))
		}
		job.growing = nil
		job.mu.Unlock()
		if stderr != nil {
			_ = stderr.Close()
		}
		if job.ownsSource && job.source != nil {
			_ = job.source.Close()
		}
		job.hub.mu.Lock()
		delete(job.hub.jobs, job.id)
		record := job.record
		job.hub.mu.Unlock()
		if record && !errors.Is(err, ffmpeg.ErrRunnerReleased) {
			job.hub.deleteRecord(job.id)
		}
		job.err = err
		close(job.done)
	})
	return ended
}

// Stdin implements ffmpeg.RemoteProcess.
func (job *remoteJob) Stdin() io.Writer { return jobStdin{job: job} }

// Wait implements ffmpeg.RemoteProcess.
func (job *remoteJob) Wait() error {
	<-job.done
	return job.err
}

// Kill implements ffmpeg.RemoteProcess. The worker slot stays reserved until
// the worker reports the exit or disconnects.
func (job *remoteJob) Kill() {
	_ = job.conn.enqueue(Message{Type: TypeJobKill, JobID: job.id})
	job.finish(errJobKilled)
}

type jobStdin struct{ job *remoteJob }

func (s jobStdin) Write(data []byte) (int, error) {
	s.job.keystroke(data, s.job.hub.config.Clock.Now())
	if err := s.job.conn.enqueue(Message{Type: TypeJobStdin, JobID: s.job.id, Data: append([]byte(nil), data...)}); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (hub *Hub) authorizedJob(r *http.Request) *remoteJob {
	const scheme = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, scheme) {
		return nil
	}
	hub.mu.Lock()
	job := hub.jobs[r.PathValue("id")]
	hub.mu.Unlock()
	if job == nil || subtle.ConstantTimeCompare(tokenHash(header[len(scheme):]), job.tokenHash) != 1 {
		return nil
	}
	return job
}

// unauthorized refuses a data-plane request. A job that a worker may still
// hold (a live record, #618) gets 503, so the worker retries after it
// reattaches instead of stopping the job.
func (hub *Hub) unauthorized(w http.ResponseWriter, r *http.Request) {
	if hub.config.Jobs != nil {
		record, err := hub.config.Jobs.Job(r.Context(), r.PathValue("id"))
		if err == nil && hub.config.Clock.Now().Before(record.LeaseExpires) {
			http.Error(w, "job waits for its worker to reattach", http.StatusServiceUnavailable)
			return
		}
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// serveSource streams the pinned source with range support.
func (hub *Hub) serveSource(w http.ResponseWriter, r *http.Request) {
	job := hub.authorizedJob(r)
	if job == nil {
		hub.unauthorized(w, r)
		return
	}
	if job.inputMode == store.TranscodeWorkerInputShared {
		// A shared worker reads the source from its own mount only.
		http.Error(w, "this worker reads the source from a shared mount", http.StatusForbidden)
		return
	}
	if job.source == nil {
		http.Error(w, "the source of this job is not available", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	reader := io.NewSectionReader(contextReaderAt{ctx: job.sourceCtx, reader: job.source}, 0, job.sourceSize)
	http.ServeContent(w, r, "", time.Time{}, reader)
}

// serveAsset serves one subtitle burn-in asset of the job (#624).
func (hub *Hub) serveAsset(w http.ResponseWriter, r *http.Request) {
	job := hub.authorizedJob(r)
	if job == nil {
		hub.unauthorized(w, r)
		return
	}
	asset, ok := job.assets[r.PathValue("name")]
	if !ok || asset.File == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	reader := io.NewSectionReader(contextReaderAt{ctx: job.sourceCtx, reader: asset.File}, 0, asset.Size)
	http.ServeContent(w, r, "", time.Time{}, reader)
}

type contextReaderAt struct {
	ctx    context.Context
	reader io.ReaderAt
}

func (c contextReaderAt) ReadAt(data []byte, offset int64) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.reader.ReadAt(data, offset)
}

// serveArtifact writes one playlist-family file into the server directory.
// A complete file lands by rename, so readers never see a partial file.
func (hub *Hub) serveArtifact(w http.ResponseWriter, r *http.Request) {
	job := hub.authorizedJob(r)
	if job == nil {
		hub.unauthorized(w, r)
		return
	}
	name := r.PathValue("name")
	if job.output != "" || !job.ownsName(name) {
		http.Error(w, "artifact name is not part of the job family", http.StatusBadRequest)
		return
	}
	state := r.Header.Get(ArtifactStateHeader)
	if state != ArtifactGrowing && state != ArtifactComplete {
		http.Error(w, "artifact state is missing", http.StatusBadRequest)
		return
	}
	if !job.begin() {
		http.Error(w, "job has ended", http.StatusGone)
		return
	}
	var malformed error
	defer func() {
		job.inflight.Done()
		if malformed != nil {
			hub.loseMalformed(job, malformed)
		}
	}()
	job.progress(hub.config.Clock.Now())
	target := filepath.Join(job.directory, name)
	if state == ArtifactGrowing {
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = file.Close()
			job.mu.Lock()
			job.growing[name] = true
			job.mu.Unlock()
		} else if !errors.Is(err, os.ErrExist) {
			http.Error(w, "create artifact", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	uploadStart := hub.config.Clock.Now()
	temporary, err := os.CreateTemp(job.directory, "."+name+".upload-*")
	if err != nil {
		http.Error(w, "create artifact", http.StatusInternalServerError)
		return
	}
	written, copyErr := io.Copy(temporary, io.LimitReader(r.Body, hub.config.MaxArtifactBytes+1))
	closeErr := temporary.Close()
	if copyErr != nil || closeErr != nil || written > hub.config.MaxArtifactBytes {
		_ = os.Remove(temporary.Name())
		http.Error(w, "write artifact", http.StatusBadRequest)
		return
	}
	if err := hls.CheckFamilyFile(name, temporary.Name()); err != nil {
		_ = os.Remove(temporary.Name())
		if errors.Is(err, hls.ErrMalformedFile) {
			malformed = err
			http.Error(w, "artifact is malformed", http.StatusUnprocessableEntity)
		} else {
			http.Error(w, "check artifact", http.StatusInternalServerError)
		}
		return
	}
	if err := os.Chmod(temporary.Name(), 0o644); err != nil {
		_ = os.Remove(temporary.Name())
		http.Error(w, "write artifact", http.StatusInternalServerError)
		return
	}
	if err := os.Rename(temporary.Name(), target); err != nil {
		_ = os.Remove(temporary.Name())
		http.Error(w, "write artifact", http.StatusInternalServerError)
		return
	}
	job.mu.Lock()
	delete(job.growing, name)
	job.mu.Unlock()
	now := hub.config.Clock.Now()
	job.progress(now)
	hub.mu.Lock()
	hub.metrics.uploadBytes += uint64(written)
	hub.metrics.uploads.observe(now.Sub(uploadStart).Seconds())
	hub.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// loseMalformed loses a job whose worker uploaded a malformed family file
// (#624): the job is killed, the HLS layer restarts the family elsewhere,
// and the worker is quarantined.
func (hub *Hub) loseMalformed(job *remoteJob, cause error) {
	_ = job.conn.enqueue(Message{Type: TypeJobKill, JobID: job.id})
	if job.finish(ffmpeg.ErrRunnerLost) {
		hub.observe(Event{Kind: EventArtifactRefused, WorkerID: job.conn.id, Name: job.conn.name, JobID: job.id, Detail: cause.Error()})
		hub.logger().Warn("transcode worker uploaded a malformed file; restarting elsewhere", "worker", job.conn.name, "job", job.id, "error", cause.Error())
		hub.lostJobs(job.conn, lossMalformed, 1)
	}
}

// begin admits one artifact write unless the job has ended.
func (job *remoteJob) begin() bool {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.closed {
		return false
	}
	job.inflight.Add(1)
	return true
}

// ownsName accepts the playlist, a segment, and the family's other files
// (the fMP4 init segment), all of which share the playlist stem.
func (job *remoteJob) ownsName(name string) bool {
	if !validArtifactName(name) {
		return false
	}
	if name == job.playlist {
		return true
	}
	if _, ok := segmentIndex(name, job.prefix, job.suffix); ok {
		return true
	}
	stem := strings.TrimSuffix(job.playlist, filepath.Ext(job.playlist))
	return stem != "" && strings.HasPrefix(name, stem) && !strings.HasSuffix(name, ".m3u8")
}

func segmentIndex(name, prefix, suffix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) || len(name) <= len(prefix)+len(suffix) {
		return 0, false
	}
	digits := name[len(prefix) : len(name)-len(suffix)]
	value := 0
	for index, r := range digits {
		if r == '-' && index == 0 && len(digits) > 1 {
			continue
		}
		if r < '0' || r > '9' || value > 1<<30 {
			return 0, false
		}
		value = value*10 + int(r-'0')
	}
	if digits[0] == '-' {
		value = -value
	}
	return value, true
}

// ffmpegMajor parses the major version of an ffmpeg version string such as
// "7.1.1", "n7.1" or "8.0-Jellyfin".
func ffmpegMajor(version string) (int, bool) {
	version = strings.TrimLeft(strings.TrimSpace(version), "nN")
	end := 0
	for end < len(version) && version[end] >= '0' && version[end] <= '9' {
		end++
	}
	if end == 0 || end > 4 {
		return 0, false
	}
	major := 0
	for _, digit := range version[:end] {
		major = major*10 + int(digit-'0')
	}
	return major, true
}
