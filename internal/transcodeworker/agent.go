// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/edge"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
)

// Process is one ffmpeg run on the worker. *ffmpeg.Exec implements it.
type Process interface {
	Stdin() io.Writer
	Done() <-chan struct{}
	Err() error
	Stop()
}

// Launcher starts ffmpeg. Tests replace it; the default is ffmpeg.StartExec.
type Launcher func(ctx context.Context, options ffmpeg.ExecOptions) (Process, error)

// PathMap maps a server media path prefix to the worker's mount of it.
type PathMap struct {
	Server string
	Local  string
}

// AgentConfig configures the worker side.
type AgentConfig struct {
	// ServerURL is the server's public base URL, including its base path:
	// https:// (or http:// on loopback only, unless AllowInsecure).
	ServerURL string
	// StateDir keeps the worker identity (identity.json) that a dashboard
	// join token enrolls. Without it the identity lasts until the process
	// ends. A static token from the server environment needs no state.
	StateDir string
	// Ephemeral runs the worker without durable state (#623): it enrolls
	// with the join token at each start, keeps the identity in memory, and
	// the server deletes the identity when the connection ends. StateDir is
	// not used.
	Ephemeral bool
	// JoinToken is the static token or the join token. A saved identity
	// replaces it.
	JoinToken    Token
	Name         string
	FFmpegPath   string
	WorkDir      string
	Capabilities Capabilities
	PathMaps     []PathMap
	Clock        clock.Clock
	// PKeyPause reports that the worker's ffmpeg has the "p"/"u" runtime
	// keys (ffmpeg.Capabilities.PKeyPause). The agent translates the
	// server's throttle keys to the keys its own ffmpeg understands.
	PKeyPause bool
	// TLSConfig is the base client TLS configuration. When the token or
	// the identity carries a fingerprint, the agent pins the server key
	// instead of CA verification.
	TLSConfig *tls.Config
	// AllowInsecure permits plain HTTP to a non-loopback server.
	AllowInsecure bool
	PollInterval  time.Duration
	// ReconnectDelay is the pause after a lost connection.
	ReconnectDelay time.Duration
	// MaxHeldSegments bounds the segments a job keeps on disk while the
	// worker has no server (#618). At the bound the agent pauses ffmpeg
	// until the job is adopted.
	MaxHeldSegments int
	KillGrace       time.Duration
	Launch          Launcher
	Logger          *slog.Logger
}

// Agent is the worker. Run owns every goroutine it starts.
type Agent struct {
	config AgentConfig
	base   *url.URL
	client *http.Client
	// edgeClient reaches the stream edge (#619). The edge has its own
	// certificate, so the server key pins do not apply.
	edgeClient *http.Client
	session    func(context.Context) error
	pinning    bool

	// draining closes when Drain is called.
	draining  chan struct{}
	drainOnce sync.Once

	// jobs are the running jobs. A job outlives the session that started
	// it (#618): without a session it runs for its lease and waits for the
	// next session to adopt it. Run owns the job goroutines.
	jobsMu  sync.Mutex
	jobs    map[string]*agentJob
	jobsCtx context.Context
	jobsWG  sync.WaitGroup

	mu sync.Mutex
	// identity is the enrolled identity; zero before enrollment.
	identity Identity
	// pins are the accepted server key fingerprints in pinning mode.
	pins []string
	// replica is the server replica of the current session (#652).
	replica string
}

// NewAgent validates the configuration.
func NewAgent(config AgentConfig) (*Agent, error) {
	if config.Clock == nil {
		return nil, errors.New("transcode worker clock is nil")
	}
	if config.WorkDir == "" {
		return nil, errors.New("transcode worker work directory is empty")
	}
	var identity Identity
	err := fs.ErrNotExist
	if config.Ephemeral {
		config.StateDir = ""
	}
	if config.StateDir != "" {
		identity, err = loadIdentity(config.StateDir)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if config.JoinToken.ID == "" || config.JoinToken.Secret == "" {
			return nil, errors.New("transcode worker is not enrolled and has no join token")
		}
		// Check before enrollment: the server keeps the identity it mints,
		// so a worker that cannot save it leaves a new orphan at each
		// restart.
		if config.StateDir != "" {
			if err := checkStateDir(config.StateDir); err != nil {
				return nil, err
			}
		}
	case err != nil:
		return nil, err
	}
	if config.Capabilities.MaxJobs < 1 {
		config.Capabilities.MaxJobs = 1
	}
	// The server checks a shared placement against these prefixes.
	config.Capabilities.PathPrefixes = nil
	for _, mapping := range config.PathMaps {
		config.Capabilities.PathPrefixes = append(config.Capabilities.PathPrefixes, strings.TrimRight(mapping.Server, "/"))
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 100 * time.Millisecond
	}
	if config.ReconnectDelay <= 0 {
		config.ReconnectDelay = 2 * time.Second
	}
	if config.MaxHeldSegments <= 0 {
		config.MaxHeldSegments = defaultMaxHeldSegments
	}
	if config.FFmpegPath == "" {
		config.FFmpegPath = "ffmpeg"
	}
	if config.Launch == nil {
		config.Launch = func(ctx context.Context, options ffmpeg.ExecOptions) (Process, error) {
			return ffmpeg.StartExec(ctx, options)
		}
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	base, err := url.Parse(strings.TrimRight(config.ServerURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse server URL: %w", err)
	}
	switch base.Scheme {
	case "https":
	case "http":
		if !loopbackHost(base.Hostname()) {
			if !config.AllowInsecure {
				return nil, errors.New("transcode worker requires https for a non-loopback server")
			}
			config.Logger.Warn("transcode worker uses plain HTTP: tokens are protected, media and segments are not", "server", base.String())
		}
	default:
		return nil, fmt.Errorf("server URL scheme %q is not http or https", base.Scheme)
	}
	agent := &Agent{config: config, base: base, identity: identity, draining: make(chan struct{}), jobs: make(map[string]*agentJob)}
	if identity.WorkerID != "" {
		agent.pins = unionPins(identity.Fingerprints, nil)
	} else {
		agent.pins = unionPins([]string{config.JoinToken.Fingerprint}, nil)
	}
	agent.pinning = len(agent.pins) > 0
	if agent.pinning {
		agent.config.TLSConfig = pinnedTLSConfig(config.TLSConfig, agent.currentPins)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = agent.config.TLSConfig
	agent.client = &http.Client{Transport: replicaTransport{next: transport, agent: agent}}
	agent.edgeClient = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(), Timeout: 30 * time.Second}
	agent.session = agent.runSession
	return agent, nil
}

// errPinMismatch reports a server key that matches no pinned fingerprint.
var errPinMismatch = errors.New("server certificate does not match a pinned fingerprint")

// pinnedTLSConfig accepts exactly the server keys that pins returns at
// handshake time and skips CA verification, so a self-signed certificate
// works (docs/spikes/transcode-workers.md §3.3). The set can grow while the
// agent runs, so a server key rotation announced through the pins message
// does not strand the worker.
func pinnedTLSConfig(base *tls.Config, pins func() []string) *tls.Config {
	var config *tls.Config
	if base != nil {
		config = base.Clone()
	} else {
		config = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	// Verification happens in VerifyConnection against the pin.
	config.InsecureSkipVerify = true //nolint:gosec // the pin replaces CA verification
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errPinMismatch
		}
		got := CertificateFingerprint(state.PeerCertificates[0])
		for _, pin := range pins() {
			if hmac.Equal([]byte(got), []byte(pin)) {
				return nil
			}
		}
		return errPinMismatch
	}
	return config
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (agent *Agent) currentPins() []string {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return append([]string(nil), agent.pins...)
}

// WorkerID returns the enrolled worker id, or "" before enrollment.
func (agent *Agent) WorkerID() string {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.identity.WorkerID
}

// enrolled stores the identity the server minted for the join token.
func (agent *Agent) enrolled(workerID, secret string) error {
	if workerID == "" || secret == "" {
		return errors.New("server sent an empty worker identity")
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	identity := Identity{WorkerID: workerID, WorkerSecret: secret}
	if agent.pinning {
		identity.Fingerprints = append([]string(nil), agent.pins...)
	}
	if agent.config.StateDir == "" {
		if !agent.config.Ephemeral {
			agent.config.Logger.Info("transcode worker identity is kept in memory only; set a state directory to keep it across restarts")
		}
	} else if err := saveIdentity(agent.config.StateDir, identity); err != nil {
		return err
	}
	agent.identity = identity
	return nil
}

// addPins accepts the extra server key fingerprints that the server
// announced. It only extends the set in pinning mode: a worker that uses CA
// verification keeps it.
func (agent *Agent) addPins(pins []string) {
	if !agent.pinning || len(pins) == 0 {
		return
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	merged := unionPins(agent.pins, pins)
	if len(merged) == len(agent.pins) {
		return
	}
	agent.pins = merged
	if agent.identity.WorkerID == "" {
		return
	}
	identity := agent.identity
	identity.Fingerprints = append([]string(nil), merged...)
	if agent.config.StateDir == "" {
		agent.identity = identity
		return
	}
	if err := saveIdentity(agent.config.StateDir, identity); err != nil {
		agent.config.Logger.Warn("save transcode worker pins", "error", err.Error())
		return
	}
	agent.identity = identity
}

// defaultMaxHeldSegments is AgentConfig.MaxHeldSegments by default.
const defaultMaxHeldSegments = 64

// ErrRejected reports a server that refused the worker's credentials.
var ErrRejected = errors.New("transcode worker rejected by server")

// Drain announces the worker's stop (#617): the agent sends drain, takes no
// new job, and Run returns nil once the server has handed off and killed
// every job. The caller bounds the wait by canceling Run's ctx.
func (agent *Agent) Drain() {
	agent.drainOnce.Do(func() { close(agent.draining) })
}

// Draining reports whether Drain was called.
func (agent *Agent) Draining() bool { return agent.isDraining() }

func (agent *Agent) isDraining() bool {
	select {
	case <-agent.draining:
		return true
	default:
		return false
	}
}

// Run connects and serves until ctx ends or a drain completes, reconnecting
// after a lost connection. It returns ErrRejected without retry when the server refuses
// the token or fails to prove it, and an error wrapping the pin mismatch
// when the server key does not match the token fingerprint.
func (agent *Agent) Run(ctx context.Context) error {
	defer agent.client.CloseIdleConnections()
	// The jobs end with Run: a rejected worker or a canceled Run keeps no
	// job.
	jobsCtx, cancelJobs := context.WithCancel(ctx)
	defer agent.jobsWG.Wait()
	defer cancelJobs()
	agent.jobsMu.Lock()
	agent.jobsCtx = jobsCtx
	agent.jobsMu.Unlock()
	for {
		if agent.isDraining() {
			return nil
		}
		err := agent.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if agent.isDraining() {
			// A drained or disconnected draining worker has no job left
			// to finish: the server restarts lost jobs elsewhere.
			return nil
		}
		if errors.Is(err, ErrRejected) || errors.Is(err, errPinMismatch) {
			return err
		}
		agent.config.Logger.Warn("transcode worker disconnected; reconnecting", "error", fmt.Sprint(err), "delay", agent.config.ReconnectDelay)
		timer := agent.config.Clock.NewTimer(agent.config.ReconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C():
		}
	}
}

// session is one authenticated connection. Every goroutine it starts ends
// before it returns.
type session struct {
	agent *Agent
	ws    *websocket.Conn
	send  chan Message
	ctx   context.Context

	// cancel ends the session; drained closes when a draining session has
	// no job left, and flushed when the writer sent every queued message.
	cancel      context.CancelFunc
	drained     chan struct{}
	drainedOnce sync.Once
	flushed     chan struct{}

	mu       sync.Mutex
	draining bool
	wg       sync.WaitGroup
}

// typeFlush is a session-internal marker; the writer never sends it.
const typeFlush = "flush"

func (agent *Agent) dial(ctx context.Context) (*websocket.Conn, error) {
	location := *agent.base
	location.Path += PathConnect
	switch location.Scheme {
	case "https":
		location.Scheme = "wss"
	default:
		location.Scheme = "ws"
	}
	config, err := websocket.NewConfig(location.String(), agent.base.String())
	if err != nil {
		return nil, err
	}
	config.TlsConfig = agent.config.TLSConfig
	ws, err := config.DialContext(ctx)
	var dialErr *websocket.DialError
	if errors.As(err, &dialErr) {
		// DialError does not unwrap; keep the cause (a pin mismatch) visible.
		return nil, fmt.Errorf("dial %s: %w", location.String(), dialErr.Err)
	}
	return ws, err
}

func (agent *Agent) runSession(parent context.Context) error {
	ws, err := agent.dial(parent)
	if err != nil {
		return err
	}
	ws.MaxPayloadBytes = maxControlMessageBytes
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// The socket closes when the session ends or the parent is canceled,
	// which unblocks every Receive.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		<-ctx.Done()
		_ = ws.Close()
	}()
	defer func() { <-closed }()
	defer cancel()

	heartbeat, err := agent.handshake(ws)
	if err != nil {
		return err
	}
	agent.config.Logger.Info("transcode worker registered", "server", agent.base.String(), "name", agent.config.Name)
	s := &session{
		agent: agent, ws: ws, send: make(chan Message, sendQueueLength), ctx: ctx, cancel: cancel,
		drained: make(chan struct{}), flushed: make(chan struct{}),
	}
	// The jobs of this session wait for the next one (#618).
	defer agent.detachJobs(s)
	defer s.wg.Wait()
	defer cancel()
	s.wg.Add(3)
	go s.writeLoop()
	go s.heartbeatLoop(heartbeat)
	go s.drainLoop()
	s.sendHeld()
	return s.readLoop()
}

// sendHeld lists the jobs that the worker kept running without a server,
// so the server adopts or kills each one.
func (s *session) sendHeld() {
	agent := s.agent
	agent.jobsMu.Lock()
	jobs := make([]*agentJob, 0, len(agent.jobs))
	for _, job := range agent.jobs {
		jobs = append(jobs, job)
	}
	agent.jobsMu.Unlock()
	if len(jobs) == 0 {
		return
	}
	held := make([]HeldJob, 0, len(jobs))
	for _, job := range jobs {
		held = append(held, job.heldState())
	}
	sort.Slice(held, func(i, j int) bool { return held[i].JobID < held[j].JobID })
	s.enqueue(Message{Type: TypeJobsHeld, Held: held})
}

// detachJobs leaves the jobs of an ended session without a server.
func (agent *Agent) detachJobs(s *session) {
	agent.jobsMu.Lock()
	jobs := make([]*agentJob, 0, len(agent.jobs))
	for _, job := range agent.jobs {
		jobs = append(jobs, job)
	}
	agent.jobsMu.Unlock()
	for _, job := range jobs {
		job.detach(s)
	}
}

// jobCount is the number of running jobs.
func (agent *Agent) jobCount() int {
	agent.jobsMu.Lock()
	defer agent.jobsMu.Unlock()
	return len(agent.jobs)
}

func (agent *Agent) job(id string) *agentJob {
	agent.jobsMu.Lock()
	defer agent.jobsMu.Unlock()
	return agent.jobs[id]
}

// drainLoop sends drain when the agent drains, then ends the session once
// no job is left and the writer sent the last job report.
func (s *session) drainLoop() {
	defer s.wg.Done()
	select {
	case <-s.ctx.Done():
		return
	case <-s.agent.draining:
	}
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()
	s.enqueue(Message{Type: TypeDrain})
	s.agent.config.Logger.Info("transcode worker draining; the server hands its jobs off")
	s.checkDrained()
	select {
	case <-s.ctx.Done():
		return
	case <-s.drained:
	}
	s.enqueue(Message{Type: typeFlush})
	select {
	case <-s.ctx.Done():
	case <-s.flushed:
		s.cancel()
	}
}

// checkDrained closes drained when a draining session has no job.
func (s *session) checkDrained() {
	s.mu.Lock()
	empty := s.draining && s.agent.jobCount() == 0
	s.mu.Unlock()
	if empty {
		s.drainedOnce.Do(func() { close(s.drained) })
	}
}

func (agent *Agent) handshake(ws *websocket.Conn) (time.Duration, error) {
	hello := Message{Type: TypeHello, Proto: ProtocolVersion, Name: agent.config.Name, Ephemeral: agent.config.Ephemeral}
	var key []byte
	agent.mu.Lock()
	if agent.identity.WorkerID != "" {
		hello.WorkerID, key = agent.identity.WorkerID, deriveKey(agent.identity.WorkerSecret)
	} else {
		hello.TokenID, key = agent.config.JoinToken.ID, agent.config.JoinToken.Key()
	}
	agent.mu.Unlock()
	workerNonce, err := newNonce()
	if err != nil {
		return 0, err
	}
	hello.Nonce = workerNonce
	if err := websocket.JSON.Send(ws, hello); err != nil {
		return 0, err
	}
	var challenge Message
	if err := websocket.JSON.Receive(ws, &challenge); err != nil {
		return 0, err
	}
	if challenge.Type == TypeReject {
		if agent.config.Ephemeral && hello.WorkerID != "" && agent.config.JoinToken.ID != "" {
			// The server deleted the ephemeral identity after a lost
			// connection: enroll again with the join token.
			agent.mu.Lock()
			agent.identity = Identity{}
			agent.mu.Unlock()
			return 0, fmt.Errorf("ephemeral identity is gone, enrolling again: %s", challenge.Error)
		}
		return 0, fmt.Errorf("%w: %s", ErrRejected, challenge.Error)
	}
	if challenge.Type != TypeChallenge || !hmac.Equal(challenge.Proof, proof(key, "server", workerNonce, challenge.Nonce)) {
		// The server does not hold this token: never send our proof.
		return 0, fmt.Errorf("%w: server failed to prove the token", ErrRejected)
	}
	caps := agent.config.Capabilities
	if err := websocket.JSON.Send(ws, Message{Type: TypeAuth, Proof: proof(key, "worker", workerNonce, challenge.Nonce), Capabilities: &caps}); err != nil {
		return 0, err
	}
	var welcome Message
	if err := websocket.JSON.Receive(ws, &welcome); err != nil {
		return 0, err
	}
	if welcome.Type == TypeEnrolled && hello.TokenID != "" {
		if err := agent.enrolled(welcome.WorkerID, welcome.WorkerSecret); err != nil {
			// Without a saved identity the worker cannot reconnect; stop.
			return 0, fmt.Errorf("%w: %w", ErrRejected, err)
		}
		agent.config.Logger.Info("transcode worker enrolled", "id", welcome.WorkerID)
		welcome = Message{}
		if err := websocket.JSON.Receive(ws, &welcome); err != nil {
			return 0, err
		}
	}
	if welcome.Type == TypeReject {
		return 0, fmt.Errorf("%w: %s", ErrRejected, welcome.Error)
	}
	if welcome.Type != TypeWelcome || welcome.HeartbeatMillis <= 0 {
		return 0, errors.New("unexpected handshake reply")
	}
	agent.addPins(welcome.Pins)
	agent.mu.Lock()
	agent.replica = welcome.Replica
	agent.mu.Unlock()
	return time.Duration(welcome.HeartbeatMillis) * time.Millisecond, nil
}

// replicaTransport names the server replica of the current session on each
// job request (#652), so that another replica forwards the request to it.
type replicaTransport struct {
	next  http.RoundTripper
	agent *Agent
}

func (t replicaTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.agent.mu.Lock()
	replica := t.agent.replica
	t.agent.mu.Unlock()
	if replica != "" && strings.Contains(request.URL.Path, pathJobs) {
		request = request.Clone(request.Context())
		request.Header.Set(ReplicaHeader, replica)
	}
	return t.next.RoundTrip(request)
}

func (s *session) enqueue(message Message) {
	select {
	case s.send <- message:
	case <-s.ctx.Done():
	}
}

func (s *session) writeLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case message := <-s.send:
			if message.Type == typeFlush {
				close(s.flushed)
				continue
			}
			if err := websocket.JSON.Send(s.ws, message); err != nil {
				_ = s.ws.Close()
				return
			}
		}
	}
}

func (s *session) heartbeatLoop(interval time.Duration) {
	defer s.wg.Done()
	timer := s.agent.config.Clock.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C():
		}
		s.enqueue(Message{Type: TypeHeartbeat, Load: &Load{ActiveJobs: s.agent.jobCount()}})
		timer.Reset(interval)
	}
}

func (s *session) readLoop() error {
	for {
		var message Message
		if err := websocket.JSON.Receive(s.ws, &message); err != nil {
			return err
		}
		switch message.Type {
		case TypePins:
			s.agent.addPins(message.Pins)
		case TypeJobStart:
			s.startJob(message.Job)
		case TypeJobStdin, TypeJobKill, TypeJobAdopted:
			job := s.agent.job(message.JobID)
			if job == nil {
				continue
			}
			switch message.Type {
			case TypeJobKill:
				job.kill()
			case TypeJobAdopted:
				job.attach(s, time.Duration(message.LeaseMillis)*time.Millisecond)
			default:
				job.stdin(message.Data)
			}
		}
	}
}

func (s *session) startJob(spec *JobSpec) {
	if spec == nil || spec.ID == "" || strings.ContainsAny(spec.ID, `/\.`) {
		return
	}
	// An HLS job names its playlist; a progressive job (#624) its output.
	if spec.Output == "" && !validArtifactName(spec.Playlist) || spec.Output != "" && (spec.Playlist != "" || !validArtifactName(spec.Output)) {
		return
	}
	s.mu.Lock()
	draining := s.draining
	s.mu.Unlock()
	if draining {
		s.enqueue(Message{Type: TypeJobFailed, JobID: spec.ID, Error: "worker is draining"})
		return
	}
	agent := s.agent
	agent.jobsMu.Lock()
	if len(agent.jobs) >= agent.config.Capabilities.MaxJobs || agent.jobs[spec.ID] != nil || agent.jobsCtx == nil {
		agent.jobsMu.Unlock()
		s.enqueue(Message{Type: TypeJobFailed, JobID: spec.ID, Error: "worker is full"})
		return
	}
	job := &agentJob{
		agent: agent, spec: *spec, session: s, lease: time.Duration(spec.LeaseMillis) * time.Millisecond,
		next: startNumber(spec.Args), wake: make(chan struct{}, 1), killed: make(chan struct{}),
	}
	agent.jobs[spec.ID] = job
	agent.jobsWG.Add(1)
	ctx := agent.jobsCtx
	agent.jobsMu.Unlock()
	go job.run(ctx)
}

// agentJob is one placed job on the worker.
type agentJob struct {
	agent *Agent
	spec  JobSpec
	// wake tells run that the job's session changed.
	wake chan struct{}

	mu sync.Mutex
	// session is the session that reports for the job; nil while the
	// worker has no server. lease is how long the job runs without one,
	// from detachedAt.
	session    *session
	lease      time.Duration
	detachedAt time.Time
	// next is the lowest segment index not uploaded yet. leaseArmed
	// marks a running lease timer.
	next       int
	leaseArmed bool
	process    Process
	quitting   bool
	killOnce   sync.Once
	killed     chan struct{}
}

func (job *agentJob) kill() {
	job.killOnce.Do(func() { close(job.killed) })
}

func (job *agentJob) notify() {
	select {
	case job.wake <- struct{}{}:
	default:
	}
}

// attach makes s the job's session after the server adopted the job.
func (job *agentJob) attach(s *session, lease time.Duration) {
	job.mu.Lock()
	job.session = s
	if lease > 0 {
		job.lease = lease
	}
	job.mu.Unlock()
	job.notify()
}

// detach starts the lease when s, the job's session, ended.
func (job *agentJob) detach(s *session) {
	job.mu.Lock()
	if job.session != s || s == nil {
		job.mu.Unlock()
		return
	}
	job.session = nil
	job.detachedAt = job.agent.config.Clock.Now()
	job.mu.Unlock()
	job.notify()
}

func (job *agentJob) armLease(armed bool) {
	job.mu.Lock()
	job.leaseArmed = armed
	job.mu.Unlock()
}

func (job *agentJob) link() (*session, time.Time, time.Duration) {
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.session, job.detachedAt, job.lease
}

// send reports to the job's session. Without a session the report is
// dropped: the server that placed the job is gone.
func (job *agentJob) send(message Message) {
	job.mu.Lock()
	s := job.session
	job.mu.Unlock()
	if s != nil {
		s.enqueue(message)
	}
}

// heldState is the job's jobs.held entry.
func (job *agentJob) heldState() HeldJob {
	job.mu.Lock()
	next := job.next
	job.mu.Unlock()
	held := HeldJob{JobID: job.spec.ID, NextSegment: next}
	entries, err := os.ReadDir(filepath.Join(job.agent.config.WorkDir, job.spec.ID))
	if err != nil {
		return held
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !validArtifactName(name) || strings.HasSuffix(name, ".tmp") {
			continue
		}
		held.Files = append(held.Files, name)
		if index, ok := segmentIndex(name, job.spec.SegmentPrefix, job.spec.SegmentSuffix); ok && index < held.NextSegment {
			held.NextSegment = index
		}
	}
	return held
}

func (job *agentJob) stdin(data []byte) {
	job.mu.Lock()
	process := job.process
	if bytes.HasPrefix(data, []byte("q")) {
		// "q" is the manager's stop: the output is discarded, so stop
		// uploading at once.
		job.quitting = true
	}
	job.mu.Unlock()
	if process != nil {
		_, _ = process.Stdin().Write(throttleKeys(data, job.agent.config.PKeyPause))
	}
}

// throttleKeys maps the server's pause and resume keys to the keys of this
// worker's ffmpeg. The server picks "p"/"u" or "c"/newline from its own
// probe; the worker's ffmpeg build can differ (ffmpeg 8 needs "p", #523).
func throttleKeys(data []byte, pKeyPause bool) []byte {
	switch string(data) {
	case "p", "c":
		if pKeyPause {
			return []byte("p")
		}
		return []byte("c")
	case "u", "\n":
		if pKeyPause {
			return []byte("u")
		}
		return []byte("\n")
	}
	return data
}

func (job *agentJob) isQuitting() bool {
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.quitting
}

// exited frees the job slot, then reports the exit, so the server never
// sends a job to a slot that is still taken.
func (job *agentJob) exited(message Message) {
	job.mu.Lock()
	s := job.session
	job.mu.Unlock()
	job.remove()
	if s != nil {
		s.enqueue(message)
		s.checkDrained()
	}
}

func (job *agentJob) remove() {
	agent := job.agent
	agent.jobsMu.Lock()
	if agent.jobs[job.spec.ID] == job {
		delete(agent.jobs, job.spec.ID)
	}
	agent.jobsMu.Unlock()
}

func (job *agentJob) run(ctx context.Context) {
	agent := job.agent
	defer agent.jobsWG.Done()
	defer func() {
		job.remove()
		job.mu.Lock()
		s := job.session
		job.mu.Unlock()
		if s != nil {
			s.checkDrained()
		}
	}()
	directory := filepath.Join(agent.config.WorkDir, job.spec.ID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		job.exited(Message{Type: TypeJobFailed, JobID: job.spec.ID, Error: "create job directory"})
		return
	}
	defer func() { _ = os.RemoveAll(directory) }()

	args, proxy, err := job.resolveArgs(ctx, directory)
	if err != nil {
		job.exited(Message{Type: TypeJobFailed, JobID: job.spec.ID, Error: err.Error()})
		return
	}
	if proxy != nil {
		defer proxy.Close()
	}
	stderr := &stderrForwarder{job: job}
	process, err := agent.config.Launch(ctx, ffmpeg.ExecOptions{
		Path: agent.config.FFmpegPath, Args: args, Dir: directory, Stderr: stderr,
		Clock: agent.config.Clock, KillGrace: agent.config.KillGrace,
	})
	if err != nil {
		job.exited(Message{Type: TypeJobFailed, JobID: job.spec.ID, Error: "start ffmpeg: " + err.Error()})
		return
	}
	job.mu.Lock()
	job.process = process
	job.mu.Unlock()
	job.send(Message{Type: TypeJobStarted, JobID: job.spec.ID})

	var uploader outputUploader = &uploader{job: job, directory: directory, uploaded: make(map[int]bool), announced: make(map[int]bool), files: make(map[string]fileStamp),
		mirrored: make(map[string]bool)}
	if job.spec.Output != "" {
		uploader = &progressiveUploader{job: job, path: filepath.Join(directory, job.spec.Output)}
	}
	poll := agent.config.Clock.NewTimer(agent.config.PollInterval)
	defer poll.Stop()
	var lease clock.Timer
	defer func() {
		if lease != nil {
			lease.Stop()
		}
	}()
	done := process.Done()
	ended := false
	// attached is the session the uploads go to; held marks an ffmpeg
	// that the agent paused because the held segments reached the bound.
	attached, _, _ := job.link()
	held := false
	for {
		current, detachedAt, leaseDuration := job.link()
		var sessionDone <-chan struct{}
		var leaseExpired <-chan time.Time
		if current != nil {
			if lease != nil {
				lease.Stop()
				lease = nil
				job.armLease(false)
			}
			if current != attached {
				// Adopted by the next server: its directory has the
				// uploaded files, not the announcements.
				attached = current
				uploader.reattached()
				if held {
					held = false
					_, _ = process.Stdin().Write(throttleKeys([]byte("u"), agent.config.PKeyPause))
				}
			}
			if ended {
				job.finish(uploader, current, process)
				return
			}
			sessionDone = current.ctx.Done()
		} else {
			if leaseDuration <= 0 {
				// No lease: the server restarts the family elsewhere.
				process.Stop()
				return
			}
			if lease == nil {
				deadline := detachedAt.Add(leaseDuration)
				lease = agent.config.Clock.NewTimer(deadline.Sub(agent.config.Clock.Now()))
				if !agent.config.Clock.Now().Before(deadline) {
					lease.Reset(0)
				}
				job.armLease(true)
			}
			leaseExpired = lease.C()
		}
		select {
		case <-ctx.Done():
			process.Stop()
			return
		case <-job.killed:
			process.Stop()
			job.exited(Message{Type: TypeJobExited, JobID: job.spec.ID, Error: "killed"})
			return
		case <-done:
			done, ended = nil, true
			continue
		case <-job.wake:
			continue
		case <-sessionDone:
			job.detach(current)
			continue
		case <-leaseExpired:
			agent.config.Logger.Warn("transcode worker job lease expired without a server; stopping job", "job", job.spec.ID)
			process.Stop()
			return
		case <-poll.C():
		}
		poll.Reset(agent.config.PollInterval)
		if current == nil {
			// The edge keeps getting the segments while no server
			// takes them (#619).
			uploader.mirror(ctx, false)
			if !held && uploader.pending() >= agent.config.MaxHeldSegments {
				held = true
				_, _ = process.Stdin().Write(throttleKeys([]byte("p"), agent.config.PKeyPause))
			}
			continue
		}
		if job.isQuitting() {
			continue
		}
		if err := uploader.sync(current.ctx, false); err != nil && current.ctx.Err() == nil && !transientUpload(err) {
			agent.config.Logger.Warn("transcode worker upload failed; stopping job", "job", job.spec.ID, "error", err.Error())
			process.Stop()
			job.exited(Message{Type: TypeJobExited, JobID: job.spec.ID, Error: "upload: " + err.Error()})
			return
		}
	}
}

// finish reports the exit of ffmpeg after the last upload.
func (job *agentJob) finish(uploader outputUploader, s *session, process Process) {
	reason := ""
	if err := process.Err(); err != nil && !job.isQuitting() {
		reason = err.Error()
	}
	if reason == "" && !job.isQuitting() {
		if err := uploader.sync(s.ctx, true); err != nil {
			reason = "upload: " + err.Error()
		}
	}
	job.exited(Message{Type: TypeJobExited, JobID: job.spec.ID, Error: reason})
}

// resolveArgs replaces the placeholders. A mapped, readable source path is
// read directly; otherwise ffmpeg streams the source from the server through
// a loopback proxy, which the caller closes after ffmpeg ends.
func (job *agentJob) resolveArgs(ctx context.Context, directory string) (args []string, proxy *sourceProxy, err error) {
	defer func() {
		if err != nil && proxy != nil {
			proxy.Close()
			proxy = nil
		}
	}()
	agent := job.agent
	input := ""
	for _, mapping := range agent.config.PathMaps {
		prefix := strings.TrimRight(mapping.Server, "/") + "/"
		if job.spec.SourcePath == "" || !strings.HasPrefix(job.spec.SourcePath, prefix) {
			continue
		}
		local := filepath.Join(mapping.Local, strings.TrimPrefix(job.spec.SourcePath, prefix))
		if info, err := os.Stat(local); err == nil && info.Mode().IsRegular() && info.Size() == job.spec.SourceSize {
			input = "file:" + local
			break
		}
	}
	if input == "" {
		if job.spec.SourceURL == "" {
			return nil, nil, errors.New("source is not readable from a mapped path on this worker")
		}
		source := *agent.base
		source.Path += job.spec.SourceURL
		if proxy, err = startSourceProxy(ctx, sourceProxyConfig{
			Client: agent.client, Upstream: source.String(), Token: job.spec.Token,
			Clock: agent.config.Clock, RetryDelay: agent.config.ReconnectDelay, Logger: agent.config.Logger,
		}); err != nil {
			return nil, nil, fmt.Errorf("start source proxy: %w", err)
		}
		input = proxy.url
	}
	assetDir := ""
	if len(job.spec.Assets) > 0 {
		if assetDir, err = job.fetchAssets(ctx, directory); err != nil {
			return nil, proxy, fmt.Errorf("fetch job assets: %w", err)
		}
	}
	args = make([]string, 0, len(job.spec.Args))
	sources := 0
	for index, argument := range job.spec.Args {
		switch {
		case argument == PlaceholderSource:
			sources++
			args = append(args, input)
		case strings.HasPrefix(argument, PlaceholderOut+"/"):
			name := strings.TrimPrefix(argument, PlaceholderOut+"/")
			if strings.ContainsAny(name, `/\`) {
				return nil, proxy, errors.New("output operand is not a bare name")
			}
			args = append(args, filepath.Join(directory, name))
		default:
			// The symmetric check of the hub's rewrite: the server may
			// name no host path except a device node.
			for _, match := range absolutePath.FindAllStringSubmatch(argument, -1) {
				if !strings.HasPrefix(match[1], "/dev/") {
					return nil, proxy, fmt.Errorf("argv operand %d names a host path", index)
				}
			}
			if strings.HasPrefix(argument, "fd:") || strings.HasPrefix(argument, "pipe:") {
				return nil, proxy, fmt.Errorf("argv operand %d names a descriptor", index)
			}
			if strings.Contains(argument, ffmpeg.RemoteAssetsPlaceholder) {
				if assetDir == "" {
					return nil, proxy, fmt.Errorf("argv operand %d names job assets, and the job has none", index)
				}
				argument = strings.ReplaceAll(argument, ffmpeg.RemoteAssetsPlaceholder, encoding.EscapeSubtitleFilterPath(assetDir))
			}
			args = append(args, argument)
		}
	}
	if sources != 1 {
		return nil, proxy, errors.New("job argv must name the source exactly once")
	}
	return args, proxy, nil
}

// stderrForwarder sends ffmpeg stderr to the server in bounded chunks.
type stderrForwarder struct {
	job *agentJob
}

func (f *stderrForwarder) Write(data []byte) (int, error) {
	for offset := 0; offset < len(data); offset += 32 << 10 {
		end := min(offset+32<<10, len(data))
		f.job.send(Message{Type: TypeJobStderr, JobID: f.job.spec.ID, Data: append([]byte(nil), data[offset:end]...)})
	}
	return len(data), nil
}

type fileStamp struct {
	size    int64
	modTime time.Time
}

// uploader mirrors the job directory to the server. It follows the
// manager's readiness rule: segment N is complete when N+1 exists or ffmpeg
// has exited. The newest segment is announced as an empty "growing" file
// after every older segment is complete, so the server's own rule sees the
// same completion one poll later, not one segment later.
type uploader struct {
	job       *agentJob
	directory string
	uploaded  map[int]bool
	announced map[int]bool
	files     map[string]fileStamp
	// mirrored are the files the edge has (#619); edgeOff stops the
	// mirror after the edge refuses the grant.
	mirrored map[string]bool
	edgeOff  bool
}

// familyFiles is one scan of the job directory.
type familyFiles struct {
	segments []int
	names    map[int]string
	others   []string
	playlist bool
}

func (u *uploader) scan() (familyFiles, error) {
	entries, err := os.ReadDir(u.directory)
	if err != nil {
		return familyFiles{}, err
	}
	spec := u.job.spec
	files := familyFiles{names: make(map[int]string)}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || strings.HasSuffix(name, ".tmp") || !validArtifactName(name) {
			continue
		}
		if name == spec.Playlist {
			files.playlist = true
			continue
		}
		if index, ok := segmentIndex(name, spec.SegmentPrefix, spec.SegmentSuffix); ok {
			files.segments = append(files.segments, index)
			files.names[index] = name
			continue
		}
		files.others = append(files.others, name)
	}
	sort.Ints(files.segments)
	return files, nil
}

// complete applies the readiness rule to the segment at position.
func (files familyFiles) complete(position int, exited bool) bool {
	return exited || position+1 < len(files.segments)
}

func (u *uploader) sync(ctx context.Context, exited bool) error {
	files, err := u.scan()
	if err != nil {
		return err
	}
	u.mirrorFiles(ctx, files, exited)
	spec := u.job.spec
	segments, names, others, playlist := files.segments, files.names, files.others, files.playlist
	// Family files such as the fMP4 init segment are complete once the
	// first media segment exists.
	if len(segments) > 0 || exited {
		for _, name := range others {
			if err := u.putIfChanged(ctx, name); err != nil {
				return err
			}
		}
	}
	present := make(map[int]bool, len(segments))
	for _, index := range segments {
		present[index] = true
	}
	for position, index := range segments {
		if u.uploaded[index] {
			continue
		}
		complete := exited || present[index+1] || position+1 < len(segments)
		if complete {
			if err := u.put(ctx, names[index], ArtifactComplete, true); err != nil {
				return err
			}
			u.uploaded[index] = true
			_ = os.Remove(filepath.Join(u.directory, names[index]))
			u.job.mu.Lock()
			u.job.next = max(u.job.next, index+1)
			u.job.mu.Unlock()
			continue
		}
		if !u.announced[index] {
			if err := u.put(ctx, names[index], ArtifactGrowing, false); err != nil {
				return err
			}
			u.announced[index] = true
		}
	}
	if playlist {
		return u.putIfChanged(ctx, spec.Playlist)
	}
	return nil
}

func (u *uploader) putIfChanged(ctx context.Context, name string) error {
	info, err := os.Stat(filepath.Join(u.directory, name))
	if err != nil {
		return nil
	}
	stamp := fileStamp{size: info.Size(), modTime: info.ModTime()}
	if previous, ok := u.files[name]; ok && previous == stamp {
		return nil
	}
	if err := u.put(ctx, name, ArtifactComplete, true); err != nil {
		return err
	}
	u.files[name] = stamp
	return nil
}

func (u *uploader) put(ctx context.Context, name, state string, body bool) error {
	agent := u.job.agent
	target := *agent.base
	target.Path += pathJobs + u.job.spec.ID + "/files/" + name
	var reader io.Reader = http.NoBody
	if body {
		file, err := os.Open(filepath.Join(u.directory, name))
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, target.String(), reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+u.job.spec.Token)
	request.Header.Set(ArtifactStateHeader, state)
	response, err := agent.client.Do(request)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return &uploadStatusError{name: name, status: response.Status, code: response.StatusCode}
	}
	return nil
}

// uploadStatusError is a refused upload.
type uploadStatusError struct {
	name   string
	status string
	code   int
}

func (e *uploadStatusError) Error() string { return fmt.Sprintf("PUT %s: %s", e.name, e.status) }

// transientUpload reports an upload error that a server restart causes: no
// answer, a gateway error, or 503 for a job that waits for its reattach.
// The job keeps its files and retries.
func transientUpload(err error) bool {
	var status *uploadStatusError
	if !errors.As(err, &status) {
		return true
	}
	switch status.code {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// mirror copies each completed family file to the stream edge.
func (u *uploader) mirror(ctx context.Context, exited bool) {
	if u.job.spec.EdgeURL == "" || u.edgeOff {
		return
	}
	if files, err := u.scan(); err == nil {
		u.mirrorFiles(ctx, files, exited)
	}
}

// mirrorFiles copies the completed segments and the other family files
// (the fMP4 init segment) that the edge does not have yet. It runs before
// the server upload, which removes a segment. A failure ends the round; the
// next poll retries, and the server upload does not wait for the edge.
func (u *uploader) mirrorFiles(ctx context.Context, files familyFiles, exited bool) {
	if u.job.spec.EdgeURL == "" || u.edgeOff {
		return
	}
	var pending []string
	if len(files.segments) > 0 || exited {
		pending = append(pending, files.others...)
	}
	for position, index := range files.segments {
		if !u.uploaded[index] && files.complete(position, exited) {
			pending = append(pending, files.names[index])
		}
	}
	for _, name := range pending {
		if u.mirrored[name] {
			continue
		}
		if err := u.putEdge(ctx, name); err != nil {
			var status *uploadStatusError
			if errors.As(err, &status) && (status.code == http.StatusUnauthorized || status.code == http.StatusForbidden) {
				u.edgeOff = true
				u.job.agent.config.Logger.Warn("stream edge refused the job grant; the job no longer mirrors segments", "job", u.job.spec.ID, "error", err.Error())
			}
			return
		}
		u.mirrored[name] = true
	}
}

func (u *uploader) putEdge(ctx context.Context, name string) error {
	target, err := url.Parse(u.job.spec.EdgeURL)
	if err != nil {
		return err
	}
	target.Path = strings.TrimSuffix(target.Path, "/") + edge.PathUpload + name
	file, err := os.Open(filepath.Join(u.directory, name))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, target.String(), file)
	if err != nil {
		return err
	}
	request.Header.Set(edge.GrantHeader, u.job.spec.EdgeGrant)
	response, err := u.job.agent.edgeClient.Do(request)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return &uploadStatusError{name: name, status: response.Status, code: response.StatusCode}
	}
	return nil
}

// reattached forgets what the previous server saw: the next server process
// has the uploaded files but removed the announcements.
func (u *uploader) reattached() {
	u.announced = make(map[int]bool)
	u.files = make(map[string]fileStamp)
}

// pending counts the segments on disk that wait for an upload.
func (u *uploader) pending() int {
	entries, err := os.ReadDir(u.directory)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if _, ok := segmentIndex(entry.Name(), u.job.spec.SegmentPrefix, u.job.spec.SegmentSuffix); ok {
			count++
		}
	}
	return count
}
