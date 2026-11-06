// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/net/websocket"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
	"github.com/deadfin-org/deadfin/internal/hls"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const testTimeout = 10 * time.Second

var testEpoch = time.Date(2026, time.October, 13, 12, 0, 0, 0, time.UTC)

func mustToken(t *testing.T) Token {
	t.Helper()
	value, err := NewToken("")
	if err != nil {
		t.Fatal(err)
	}
	token, err := ParseToken(value)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// testHub is a hub on a loopback httptest server with a fake clock.
type testHub struct {
	hub     *Hub
	server  *httptest.Server
	clock   *clock.Fake
	events  chan Event
	workers *store.TranscodeWorkerStore
}

func newTestHub(t *testing.T, tokens ...Token) *testHub {
	t.Helper()
	return newTestHubWithConfig(t, HubConfig{}, tokens...)
}

// newTestHubWithConfig fills the clock, the observer and the credentials of
// config. The credentials are an in-memory SQLite store that holds tokens
// as join tokens without a use limit or an expiry.
func newTestHubWithConfig(t *testing.T, config HubConfig, tokens ...Token) *testHub {
	t.Helper()
	return newTestHubOn(t, config, httptest.NewServer, tokens...)
}

func newTestHubOn(t *testing.T, config HubConfig, serve func(http.Handler) *httptest.Server, tokens ...Token) *testHub {
	t.Helper()
	fake := clock.NewFake(testEpoch)
	events := make(chan Event, 256)
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	workers := store.NewTranscodeWorkerStore(stores)
	for _, token := range tokens {
		addJoinToken(t, workers, token, nil, time.Time{})
	}
	config.Clock = fake
	config.Observe = func(event Event) { events <- event }
	if config.Policy.Mode == "" {
		// Phase 1 and 2 tests place every job on a worker that fits.
		config.Policy.Mode = ModeWorkersFirst
	}
	var credentials Credentials = StoreCredentials{Store: workers, Clock: fake}
	if static, ok := config.Credentials.(testStaticTokens); ok {
		credentials = StaticTokens(credentials, static)
	}
	config.Credentials = credentials
	hub, err := NewHub(config)
	if err != nil {
		t.Fatal(err)
	}
	server := serve(hub)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if err := hub.Close(ctx); err != nil {
			t.Errorf("hub close: %v", err)
		}
		server.Close()
	})
	return &testHub{hub: hub, server: server, clock: fake, events: events, workers: workers}
}

// testStaticTokens in HubConfig.Credentials asks newTestHubOn for static
// tokens from the server environment.
type testStaticTokens []Token

func (testStaticTokens) JoinToken(context.Context, string) (Credential, error) { panic("unused") }
func (testStaticTokens) Worker(context.Context, string) (Credential, error)    { panic("unused") }
func (testStaticTokens) Enroll(context.Context, Enrollment) error              { panic("unused") }
func (testStaticTokens) Connected(context.Context, string) error               { panic("unused") }
func (testStaticTokens) Forget(context.Context, string) error                  { panic("unused") }
func (testStaticTokens) Pins(context.Context) ([]string, error)                { panic("unused") }
func (testStaticTokens) Enabled(context.Context) (bool, error)                 { panic("unused") }

// onlyJob returns the id and data-plane token of the hub's one job.
func onlyJob(t *testing.T, hub *testHub) (string, string) {
	t.Helper()
	hub.hub.mu.Lock()
	defer hub.hub.mu.Unlock()
	if len(hub.hub.jobs) != 1 {
		t.Fatalf("hub has %d jobs, want 1", len(hub.hub.jobs))
	}
	for id, job := range hub.hub.jobs {
		return id, job.token
	}
	return "", ""
}

func addJoinToken(t *testing.T, workers *store.TranscodeWorkerStore, token Token, uses *int, expires time.Time) {
	t.Helper()
	err := workers.CreateJoinToken(context.Background(), store.TranscodeWorkerJoinToken{
		ID: token.ID, Name: "test", DerivedKey: token.Key(), Fingerprint: token.Fingerprint,
		UsesLeft: uses, Expires: expires, Created: testEpoch,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (h *testHub) waitEvent(t *testing.T, kind EventKind) Event {
	t.Helper()
	deadline := time.NewTimer(testTimeout)
	defer deadline.Stop()
	for {
		select {
		case event := <-h.events:
			if event.Kind == kind {
				return event
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for hub event %s", kind)
			return Event{}
		}
	}
}

// eventually steps fake time and yields until cond holds. It never sleeps.
func eventually(t *testing.T, step func(), cond func() bool) {
	t.Helper()
	deadline := time.NewTimer(testTimeout)
	defer deadline.Stop()
	for !cond() {
		select {
		case <-deadline.C:
			t.Fatal("condition not reached")
		default:
		}
		if step != nil {
			step()
		}
		runtime.Gosched()
	}
}

// fakeProcess stands in for ffmpeg on the worker.
type fakeProcess struct {
	options ffmpeg.ExecOptions

	mu      sync.Mutex
	stdin   bytes.Buffer
	stopped bool
	endOnce sync.Once
	done    chan struct{}
	err     error
}

func (p *fakeProcess) Stdin() io.Writer { return fakeStdin{p} }

type fakeStdin struct{ p *fakeProcess }

func (s fakeStdin) Write(data []byte) (int, error) {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	return s.p.stdin.Write(data)
}

func (p *fakeProcess) Done() <-chan struct{} { return p.done }
func (p *fakeProcess) Err() error            { return p.err }

func (p *fakeProcess) Stop() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.end(nil)
}

func (p *fakeProcess) end(err error) {
	p.endOnce.Do(func() {
		p.err = err
		close(p.done)
	})
}

func (p *fakeProcess) write(t *testing.T, name, contents string) {
	t.Helper()
	data := []byte(contents)
	if strings.HasSuffix(name, ".ts") {
		data = tsPacket(contents)
	}
	if err := os.WriteFile(filepath.Join(p.options.Dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// tsPacket wraps text in one MPEG-TS packet, so a test segment passes the
// server's artifact check (#624). tsText reverses it.
func tsPacket(text string) []byte {
	packet := bytes.Repeat([]byte{0xff}, hls.TSPacketSize)
	packet[0] = hls.TSSyncByte
	copy(packet[1:], text)
	return packet
}

func tsText(data []byte) string {
	if len(data) != hls.TSPacketSize || data[0] != hls.TSSyncByte {
		return string(data)
	}
	return string(bytes.TrimRight(data[1:], "\xff"))
}

// testAgent runs one worker agent until the test ends or kill is called.
type testAgent struct {
	agent    *Agent
	clock    *clock.Fake
	launched chan *fakeProcess
	cancel   context.CancelFunc
	done     chan error
}

func startTestAgent(t *testing.T, hub *testHub, token Token, name string, caps Capabilities, maps ...PathMap) *testAgent {
	t.Helper()
	return startTestAgentIn(t, hub, t.TempDir(), token, name, caps, maps...)
}

// startTestAgentIn runs an agent with the state directory dir, so a test
// can restart a worker with its saved identity.
func startTestAgentIn(t *testing.T, hub *testHub, dir string, token Token, name string, caps Capabilities, maps ...PathMap) *testAgent {
	t.Helper()
	fake := clock.NewFake(testEpoch)
	launched := make(chan *fakeProcess, 16)
	agent, err := NewAgent(AgentConfig{
		ServerURL:    hub.server.URL,
		StateDir:     dir,
		JoinToken:    token,
		Name:         name,
		WorkDir:      t.TempDir(),
		Capabilities: caps,
		PathMaps:     maps,
		Clock:        fake,
		PKeyPause:    true,
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

func (a *testAgent) kill() {
	a.cancel()
	<-a.done
	a.done <- context.Canceled
}

func (a *testAgent) process(t *testing.T) *fakeProcess {
	t.Helper()
	select {
	case process := <-a.launched:
		return process
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the worker to launch ffmpeg")
		return nil
	}
}

// poll advances the agent clock by one upload poll.
func (a *testAgent) poll() { a.clock.Advance(100 * time.Millisecond) }

// collectStderr is the server side of the stderr pipe.
type collectStderr struct {
	mu     sync.Mutex
	data   bytes.Buffer
	closed bool
}

func (c *collectStderr) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.Write(data)
}

func (c *collectStderr) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *collectStderr) snapshot() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.String(), c.closed
}

// family is a server-side playlist family with a pinned source.
type family struct {
	dir    string
	source *os.File
	path   string
	stderr *collectStderr
}

func newFamily(t *testing.T, sourceContents string) family {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "transcodes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "media", "movie.mkv")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(sourceContents), 0o644); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return family{dir: dir, source: source, path: path, stderr: &collectStderr{}}
}

func (f family) job(extra ...string) ffmpeg.RemoteHLSJob {
	playlist := filepath.Join(f.dir, "abc.m3u8")
	pattern := filepath.Join(f.dir, "abc%d.ts")
	args := append([]string{"-ss", "0"}, extra...)
	args = append(args, "-i", "file:"+f.path, "-c:v", "libx264", "-hls_segment_filename", pattern, "-hls_base_url", "hls/abc/", "-y", playlist)
	info, _ := f.source.Stat()
	return ffmpeg.RemoteHLSJob{
		Args: args, SourcePath: f.path, Source: f.source, SourceSize: info.Size(),
		Directory: f.dir, Playlist: playlist, SegmentPattern: pattern, PlaySessionID: "play", Stderr: f.stderr,
	}
}

func (f family) read(name string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		return "", false
	}
	return tsText(data), true
}

func startJob(t *testing.T, hub *testHub, job ffmpeg.RemoteHLSJob) ffmpeg.RemoteProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	process, err := hub.hub.StartHLS(ctx, job)
	if err != nil {
		t.Fatalf("StartHLS: %v", err)
	}
	return process
}

func waitResult(t *testing.T, process ffmpeg.RemoteProcess) error {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- process.Wait() }()
	select {
	case err := <-result:
		return err
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the remote process to end")
		return nil
	}
}

func TestTokenParsingAndHandshakeProofs(t *testing.T) {
	token := mustToken(t)
	if !bytes.Equal(token.Key(), token.Key()) {
		t.Fatal("token key is not deterministic")
	}
	other := mustToken(t)
	if bytes.Equal(token.Key(), other.Key()) {
		t.Fatal("two tokens share a key")
	}
	worker, server := []byte("worker-nonce-0123456789"), []byte("server-nonce-0123456789")
	if bytes.Equal(proof(token.Key(), "worker", worker, server), proof(token.Key(), "server", worker, server)) {
		t.Fatal("worker and server proofs are interchangeable")
	}
	pin := CertificateFingerprint(&x509.Certificate{RawSubjectPublicKeyInfo: []byte("server key")})
	pinned, err := NewToken(pin)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := ParseToken(pinned); err != nil || parsed.Fingerprint != pin {
		t.Fatalf("pinned token = %+v, %v", parsed, err)
	}
	if _, err := NewToken("not-a-digest"); err == nil {
		t.Fatal("NewToken accepted a malformed fingerprint")
	}
	secret := strings.Repeat("s", 43)
	for _, bad := range []string{"", "dfw1", "dfw1..secret.", "dfw2.id." + secret + ".", "dfw1.id.short.", "dfw1.id." + secret, "dfw1.id." + secret + ".AAAA", "dfw1.id." + secret + "." + pin + ".x"} {
		if _, err := ParseToken(bad); err == nil {
			t.Errorf("ParseToken(%q) accepted a malformed token", bad)
		}
	}
}

func TestBuildJobSpecRewritesServerPathsAndRefusesLocalResources(t *testing.T) {
	f := newFamily(t, "media")
	spec, need, err := buildJobSpec(f.job("-init_hw_device", "vaapi=va:/dev/dri/renderD128", "-hwaccel", "vaapi", "-vf", "scale=trunc(min(max(iw\\,ih*a)\\,1280)/2)*2:-2"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(spec.Args, " ")
	if strings.Contains(joined, f.dir) || strings.Contains(joined, f.path) {
		t.Fatalf("worker argv leaks server paths: %s", joined)
	}
	for _, want := range []string{"-i " + PlaceholderSource, PlaceholderOut + "/abc%d.ts", "-y " + PlaceholderOut + "/abc.m3u8", "-hls_base_url hls/abc/"} {
		if !strings.Contains(joined, want) {
			t.Errorf("worker argv %q lacks %q", joined, want)
		}
	}
	if spec.Playlist != "abc.m3u8" || spec.SegmentPrefix != "abc" || spec.SegmentSuffix != ".ts" {
		t.Fatalf("family names = %q %q %q", spec.Playlist, spec.SegmentPrefix, spec.SegmentSuffix)
	}
	if strings.Join(need.hwaccels, ",") != "vaapi,vaapi" || strings.Join(need.encoders, ",") != "libx264" {
		t.Fatalf("requirement = %+v", need)
	}
	if need.satisfiedBy(Capabilities{MaxJobs: 1}) || !need.satisfiedBy(Capabilities{HWAccels: []string{"vaapi"}, MaxJobs: 1}) {
		t.Fatal("requirement matching is wrong")
	}

	refused := map[string][]string{
		"burn-in font":    {"-vf", "subtitles=f=" + filepath.Join(f.dir, "fonts")},
		"descriptor":      {"-i", "fd:4"},
		"pipe":            {"-i", "pipe:0"},
		"second source":   {"-i", "file:" + f.path},
		"proc descriptor": {"-i", "/proc/self/fd/3"},
		"aliased output":  {"-hls_fmp4_init_filename", "/var/alias/abc-1.mp4"},
		"font directory":  {"-vf", "scale=1280:-2,subtitles=f=x:fontsdir=/srv/fonts"},
	}
	for name, extra := range refused {
		if _, _, err := buildJobSpec(f.job(extra...)); err == nil {
			t.Errorf("%s: buildJobSpec accepted a server-local resource", name)
		}
	}
}

func TestRegistrationRequiresMutualProofOfTheJoinToken(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)

	good := startTestAgent(t, hub, token, "gpu-a", Capabilities{HWAccels: []string{"qsv"}, MaxJobs: 2})
	registered := hub.waitEvent(t, EventRegistered)
	if registered.Name != "gpu-a" {
		t.Fatalf("registered %q", registered.Name)
	}
	workers := hub.hub.Workers()
	if len(workers) != 1 || workers[0].Capabilities.MaxJobs != 2 || workers[0].Capabilities.HWAccels[0] != "qsv" {
		t.Fatalf("workers = %+v", workers)
	}
	good.kill()
	hub.waitEvent(t, EventDisconnected)

	// Same token ID, wrong secret: the server cannot prove the worker's
	// token, so the worker refuses before it sends its own proof.
	forged := token
	forged.Secret = strings.Repeat("x", 43)
	agent := startTestAgent(t, hub, forged, "forged", Capabilities{MaxJobs: 1})
	if err := <-agent.done; !errors.Is(err, ErrRejected) {
		t.Fatalf("forged secret: Run = %v, want ErrRejected", err)
	}
	agent.done <- nil

	unknown := mustToken(t)
	agent = startTestAgent(t, hub, unknown, "stranger", Capabilities{MaxJobs: 1})
	if err := <-agent.done; !errors.Is(err, ErrRejected) {
		t.Fatalf("unknown token: Run = %v, want ErrRejected", err)
	}
	agent.done <- nil
	if event := hub.waitEvent(t, EventRejected); event.Detail != "unknown token" {
		t.Fatalf("reject detail = %q", event.Detail)
	}

	// A client that knows the token ID but not the key fails the
	// server's check.
	ws := dialRaw(t, hub)
	nonce := bytes.Repeat([]byte{1}, 32)
	sendRaw(t, ws, Message{Type: TypeHello, Proto: ProtocolVersion, TokenID: token.ID, Name: "replay", Nonce: nonce})
	challenge := receiveRaw(t, ws)
	if challenge.Type != TypeChallenge {
		t.Fatalf("challenge = %+v", challenge)
	}
	sendRaw(t, ws, Message{Type: TypeAuth, Proof: proof([]byte("wrong key"), "worker", nonce, challenge.Nonce), Capabilities: &Capabilities{MaxJobs: 1}})
	if reply := receiveRaw(t, ws); reply.Type != TypeReject {
		t.Fatalf("bad proof reply = %+v", reply)
	}
	if event := hub.waitEvent(t, EventRejected); event.Detail != "authentication failed" {
		t.Fatalf("reject detail = %q", event.Detail)
	}
	if len(hub.hub.Workers()) != 0 {
		t.Fatalf("rejected clients registered: %+v", hub.hub.Workers())
	}
}

func dialRaw(t *testing.T, hub *testHub) *websocket.Conn {
	t.Helper()
	location := "ws" + strings.TrimPrefix(hub.server.URL, "http") + PathConnect
	ws, err := websocket.Dial(location, "", hub.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

func sendRaw(t *testing.T, ws *websocket.Conn, message Message) {
	t.Helper()
	if err := websocket.JSON.Send(ws, message); err != nil {
		t.Fatal(err)
	}
}

func receiveRaw(t *testing.T, ws *websocket.Conn) Message {
	t.Helper()
	var message Message
	if err := websocket.JSON.Receive(ws, &message); err != nil {
		t.Fatal(err)
	}
	return message
}

func registerRaw(t *testing.T, hub *testHub, token Token) *websocket.Conn {
	t.Helper()
	ws := dialRaw(t, hub)
	nonce := bytes.Repeat([]byte{7}, 32)
	sendRaw(t, ws, Message{Type: TypeHello, Proto: ProtocolVersion, TokenID: token.ID, Name: "raw", Nonce: nonce})
	challenge := receiveRaw(t, ws)
	sendRaw(t, ws, Message{Type: TypeAuth, Proof: proof(token.Key(), "worker", nonce, challenge.Nonce), Capabilities: &Capabilities{MaxJobs: 1}})
	if enrolled := receiveRaw(t, ws); enrolled.Type != TypeEnrolled || enrolled.WorkerID == "" || enrolled.WorkerSecret == "" {
		t.Fatalf("enrolled = %+v", enrolled)
	}
	if welcome := receiveRaw(t, ws); welcome.Type != TypeWelcome || welcome.HeartbeatMillis != DefaultHeartbeatInterval.Milliseconds() {
		t.Fatalf("welcome = %+v", welcome)
	}
	hub.waitEvent(t, EventRegistered)
	return ws
}

func TestHeartbeatExpiryOnFakeClock(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	ws := registerRaw(t, hub, token)
	limit := DefaultMissedHeartbeats * DefaultHeartbeatInterval

	// Silence up to the limit keeps the worker.
	hub.clock.Advance(limit)
	hub.hub.expire()
	if len(hub.hub.Workers()) != 1 {
		t.Fatal("worker expired at exactly the heartbeat limit")
	}
	// A heartbeat resets the deadline.
	sendRaw(t, ws, Message{Type: TypeHeartbeat, Load: &Load{}})
	eventually(t, nil, func() bool {
		workers := hub.hub.Workers()
		return len(workers) == 1 && workers[0].LastSeen.Equal(hub.clock.Now())
	})
	hub.clock.Advance(limit)
	hub.hub.expire()
	if len(hub.hub.Workers()) != 1 {
		t.Fatal("worker expired within the limit after a heartbeat")
	}
	// Further silence expires it through the supervised expiry loop, and
	// the hub closes the connection.
	eventually(t, func() { hub.clock.Advance(DefaultHeartbeatInterval) }, func() bool { return len(hub.hub.Workers()) == 0 })
	hub.waitEvent(t, EventExpired)
	hub.waitEvent(t, EventDisconnected)
	if len(hub.hub.Workers()) != 0 {
		t.Fatalf("expired worker is still registered: %+v", hub.hub.Workers())
	}
	var message Message
	if err := websocket.JSON.Receive(ws, &message); err == nil {
		t.Fatalf("expired connection still delivers %+v", message)
	}
}

func TestJobRunsOnWorkerAndSegmentsFlowBack(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	worker := startTestAgent(t, hub, token, "gpu-a", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "0123456789abcdef")

	process := startJob(t, hub, f.job())
	ffmpegProcess := worker.process(t)
	args := strings.Join(ffmpegProcess.options.Args, " ")
	if strings.Contains(args, f.dir) || !strings.Contains(args, filepath.Join(ffmpegProcess.options.Dir, "abc.m3u8")) {
		t.Fatalf("worker argv = %s", args)
	}
	// No path map: ffmpeg streams the source from the server through the
	// worker's loopback proxy, which holds the job token.
	proxyURL := ffmpegProcess.options.Args[indexOf(ffmpegProcess.options.Args, "-i")+1]
	if !strings.HasPrefix(proxyURL, "http://127.0.0.1:") || strings.Contains(args, "Authorization") {
		t.Fatalf("streamed input is not the loopback proxy: %s", args)
	}
	jobID, jobToken := onlyJob(t, hub)
	sourceURL := hub.server.URL + pathJobs + jobID + "/source"
	request, _ := http.NewRequest(http.MethodGet, proxyURL, nil)
	request.Header.Set("Range", "bytes=4-7")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusPartialContent || string(body) != "4567" {
		t.Fatalf("ranged source = %d %q", response.StatusCode, body)
	}
	// The server route itself needs the job token.
	for bearer, want := range map[string]int{"": http.StatusUnauthorized, jobToken: http.StatusPartialContent} {
		direct, _ := http.NewRequest(http.MethodGet, sourceURL, nil)
		direct.Header.Set("Authorization", "Bearer "+bearer)
		direct.Header.Set("Range", "bytes=0-1")
		response, err := http.DefaultClient.Do(direct)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("direct source with bearer %q = %d, want %d", bearer, response.StatusCode, want)
		}
	}

	// ffmpeg's stderr reaches the server pipe.
	_, _ = ffmpegProcess.options.Stderr.Write([]byte("frame=1 time=00:00:01.00\n"))
	eventually(t, nil, func() bool { text, _ := f.stderr.snapshot(); return strings.Contains(text, "frame=1") })

	// The newest segment appears as an empty growing file; it completes
	// when its successor exists.
	ffmpegProcess.write(t, "abc0.ts", "segment zero")
	eventually(t, worker.poll, func() bool { _, ok := f.read("abc0.ts"); return ok })
	if text, _ := f.read("abc0.ts"); text != "" {
		t.Fatalf("growing segment 0 = %q before its successor exists", text)
	}
	ffmpegProcess.write(t, "abc1.ts", "segment one")
	ffmpegProcess.write(t, "abc.m3u8", "#EXTM3U\n#EXTINF:4,\nhls/abc/abc0.ts\n")
	eventually(t, worker.poll, func() bool {
		text, _ := f.read("abc0.ts")
		_, growing := f.read("abc1.ts")
		playlist, _ := f.read("abc.m3u8")
		return text == "segment zero" && growing && strings.Contains(playlist, "abc0.ts")
	})
	if _, err := os.Stat(filepath.Join(ffmpegProcess.options.Dir, "abc0.ts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker kept an uploaded segment: %v", err)
	}

	// The throttle keystrokes reach ffmpeg's stdin.
	if _, err := process.Stdin().Write([]byte("p")); err != nil {
		t.Fatal(err)
	}
	eventually(t, nil, func() bool {
		ffmpegProcess.mu.Lock()
		defer ffmpegProcess.mu.Unlock()
		return ffmpegProcess.stdin.String() == "p"
	})

	// Exit completes the last segment before Wait returns.
	ffmpegProcess.end(nil)
	if err := waitResult(t, process); err != nil {
		t.Fatalf("Wait = %v", err)
	}
	if text, _ := f.read("abc1.ts"); text != "segment one" {
		t.Fatalf("final segment = %q", text)
	}
	if _, closed := f.stderr.snapshot(); !closed {
		t.Fatal("stderr is open after Wait")
	}
	// The job's data plane closes with the job.
	put, _ := http.NewRequest(http.MethodPut, hub.server.URL+pathJobs+jobID+"/files/abc2.ts", strings.NewReader("late"))
	put.Header.Set("Authorization", "Bearer "+jobToken)
	put.Header.Set(ArtifactStateHeader, ArtifactComplete)
	response, err = http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("late PUT = %d, want 401", response.StatusCode)
	}
	if _, ok := f.read("abc2.ts"); ok {
		t.Fatal("a late write landed after Wait")
	}
}

func indexOf(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
}

func TestSharedPathMapReadsSourceDirectly(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	f := newFamily(t, "media bytes")
	mount := filepath.Dir(f.path)
	worker := startTestAgent(t, hub, token, "nfs", Capabilities{MaxJobs: 1}, PathMap{Server: mount, Local: mount})
	hub.waitEvent(t, EventRegistered)
	process := startJob(t, hub, f.job())
	ffmpegProcess := worker.process(t)
	if got := ffmpegProcess.options.Args[indexOf(ffmpegProcess.options.Args, "-i")+1]; got != "file:"+f.path {
		t.Fatalf("mapped input = %q", got)
	}
	if indexOf(ffmpegProcess.options.Args, "-headers") >= 0 {
		t.Fatal("a mapped input carries the streaming header")
	}
	process.Kill()
	if err := waitResult(t, process); err == nil {
		t.Fatal("killed job reported success")
	}
}

func TestWorkerLossMidStreamLosesJobAndOtherWorkerTakesTheRestart(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	first := startTestAgent(t, hub, token, "a-first", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	// pack places on the oldest worker.
	hub.clock.Advance(time.Second)
	second := startTestAgent(t, hub, token, "b-second", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")

	process := startJob(t, hub, f.job())
	ffmpegProcess := first.process(t)
	ffmpegProcess.write(t, "abc0.ts", "zero")
	ffmpegProcess.write(t, "abc1.ts", "one")
	eventually(t, first.poll, func() bool {
		zero, _ := f.read("abc0.ts")
		_, growing := f.read("abc1.ts")
		return zero == "zero" && growing
	})

	first.kill()
	if err := waitResult(t, process); !errors.Is(err, ffmpeg.ErrRunnerLost) {
		t.Fatalf("Wait after worker loss = %v, want ErrRunnerLost", err)
	}
	if zero, _ := f.read("abc0.ts"); zero != "zero" {
		t.Fatal("a completed segment was lost with the worker")
	}
	if _, ok := f.read("abc1.ts"); ok {
		t.Fatal("the lost worker's growing placeholder survived and would look cached")
	}

	// The restart lands on the surviving worker.
	restart := startJob(t, hub, f.job())
	placed := hub.waitEvent(t, EventJobPlaced)
	for placed.Name != "b-second" {
		placed = hub.waitEvent(t, EventJobPlaced)
	}
	second.process(t).end(nil)
	if err := waitResult(t, restart); err != nil {
		t.Fatalf("restarted job = %v", err)
	}
}

func TestPlacementRespectsSessionLimitsAndCapabilitiesAndFallsBack(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	f := newFamily(t, "media")
	ctx := context.Background()
	if _, err := hub.hub.StartHLS(ctx, f.job()); !errors.Is(err, ffmpeg.ErrNoRemoteRunner) {
		t.Fatalf("no workers: %v, want ErrNoRemoteRunner", err)
	}
	worker := startTestAgent(t, hub, token, "nvenc", Capabilities{HWAccels: []string{"cuda"}, Encoders: []string{"libx264", "h264_nvenc"}, MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)

	if _, err := hub.hub.StartHLS(ctx, f.job("-init_hw_device", "qsv=qs@va")); !errors.Is(err, ffmpeg.ErrNoRemoteRunner) {
		t.Fatalf("missing hwaccel: %v, want ErrNoRemoteRunner", err)
	}
	first := startJob(t, hub, f.job("-init_hw_device", "cuda=cu:0"))
	ffmpegProcess := worker.process(t)
	if _, err := hub.hub.StartHLS(ctx, f.job()); !errors.Is(err, ffmpeg.ErrNoRemoteRunner) {
		t.Fatalf("full worker: %v, want ErrNoRemoteRunner (session limit)", err)
	}
	// Kill frees the slot only when the worker confirms the exit.
	first.Kill()
	if err := waitResult(t, first); err == nil {
		t.Fatal("killed job reported success")
	}
	hub.waitEvent(t, EventJobExited)
	ffmpegProcess.mu.Lock()
	stopped := ffmpegProcess.stopped
	ffmpegProcess.mu.Unlock()
	if !stopped {
		t.Fatal("kill did not stop ffmpeg on the worker")
	}
	next := startJob(t, hub, f.job())
	worker.process(t).end(nil)
	if err := waitResult(t, next); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactEndpointRejectsForeignNamesAndTokens(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	worker := startTestAgent(t, hub, token, "w", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")
	process := startJob(t, hub, f.job())
	ffmpegProcess := worker.process(t)
	_ = ffmpegProcess
	jobID, jobToken := onlyJob(t, hub)
	jobBase := hub.server.URL + pathJobs + jobID

	put := func(name, bearer, state string) int {
		return putArtifact(t, jobBase, name, bearer, state, validArtifact(name))
	}
	cases := []struct {
		name, bearer, state string
		want                int
	}{
		{"abc0.ts", "wrong", ArtifactComplete, http.StatusUnauthorized},
		{"other.ts", jobToken, ArtifactComplete, http.StatusBadRequest},
		{".abc0.ts", jobToken, ArtifactComplete, http.StatusBadRequest},
		{"abc-1.mp4", jobToken, ArtifactComplete, http.StatusNoContent},
		{"abc.m3u8", jobToken, ArtifactComplete, http.StatusNoContent},
		{"abc0.ts", jobToken, "", http.StatusBadRequest},
		{"abc0.ts", jobToken, ArtifactComplete, http.StatusNoContent},
	}
	for _, c := range cases {
		if got := put(c.name, c.bearer, c.state); got != c.want {
			t.Errorf("PUT %s bearer=%s state=%q = %d, want %d", c.name, c.bearer, c.state, got, c.want)
		}
	}
	if _, ok := f.read("other.ts"); ok {
		t.Fatal("a foreign name was written")
	}
	entries, _ := os.ReadDir(f.dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("temporary upload left behind: %s", entry.Name())
		}
	}
	process.Kill()
	_ = waitResult(t, process)
}

// validArtifact is a body that passes the artifact check for name.
func validArtifact(name string) []byte {
	switch filepath.Ext(name) {
	case ".ts":
		return tsPacket("segment")
	case ".mp4":
		return []byte("\x00\x00\x00\x10ftypiso6\x00\x00\x00\x00")
	default:
		return []byte("#EXTM3U\n")
	}
}

func putArtifact(t *testing.T, jobBase, name, bearer, state string, body []byte) int {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPut, jobBase+"/files/"+name, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set(ArtifactStateHeader, state)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

// TestMalformedSegmentUploadLosesTheJob covers #624: a segment that is not
// MPEG-TS is refused, the job is lost, and the worker is quarantined.
func TestMalformedSegmentUploadLosesTheJob(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	startTestAgent(t, hub, token, "w", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")
	process := startJob(t, hub, f.job())
	jobID, jobToken := onlyJob(t, hub)
	jobBase := hub.server.URL + pathJobs + jobID
	if got := putArtifact(t, jobBase, "abc0.ts", jobToken, ArtifactComplete, []byte("<html>not a segment</html>")); got != http.StatusUnprocessableEntity {
		t.Fatalf("malformed segment = %d, want 422", got)
	}
	if err := waitResult(t, process); !errors.Is(err, ffmpeg.ErrRunnerLost) {
		t.Fatalf("job after a malformed segment: %v, want ErrRunnerLost", err)
	}
	hub.waitEvent(t, EventArtifactRefused)
	hub.waitEvent(t, EventQuarantined)
	if _, ok := f.read("abc0.ts"); ok {
		t.Fatal("the malformed segment was stored")
	}
}

func TestAgentRefusesPlainHTTPOffLoopback(t *testing.T) {
	config := AgentConfig{ServerURL: "http://192.0.2.10:8097", JoinToken: mustToken(t), StateDir: t.TempDir(), WorkDir: t.TempDir(), Clock: clock.NewFake(testEpoch)}
	if _, err := NewAgent(config); err == nil {
		t.Fatal("plain http to a remote server was accepted")
	}
	config.AllowInsecure = true
	if _, err := NewAgent(config); err != nil {
		t.Fatalf("explicit insecure: %v", err)
	}
	config.ServerURL = "https://server.example:8097"
	config.AllowInsecure = false
	if _, err := NewAgent(config); err != nil {
		t.Fatalf("https: %v", err)
	}
}

func TestHubCloseLosesJobsAndEndsAgentSessions(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	worker := startTestAgent(t, hub, token, "w", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")
	process := startJob(t, hub, f.job())
	worker.process(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := hub.hub.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := waitResult(t, process); !errors.Is(err, ffmpeg.ErrRunnerLost) {
		t.Fatalf("Wait after hub close = %v", err)
	}
	if _, err := hub.hub.StartHLS(ctx, f.job()); !errors.Is(err, ffmpeg.ErrNoRemoteRunner) {
		t.Fatalf("closed hub placed a job: %v", err)
	}
}

func TestAgentPinsTheTokenFingerprintBeforeTheHandshake(t *testing.T) {
	token := mustToken(t)
	tlsHub := newTestHubOn(t, HubConfig{}, httptest.NewTLSServer, token)
	server, events := tlsHub.server, tlsHub.events
	pinned := func(fingerprint string) Token {
		value := token
		value.Fingerprint = fingerprint
		return value
	}

	// The test server's certificate is self-signed, so only the pin can
	// admit the worker.
	good := startTestAgent(t, tlsHub, pinned(CertificateFingerprint(server.Certificate())), "pinned", Capabilities{MaxJobs: 1})
	tlsHub.waitEvent(t, EventRegistered)
	good.kill()
	tlsHub.waitEvent(t, EventDisconnected)

	wrong := CertificateFingerprint(&x509.Certificate{RawSubjectPublicKeyInfo: []byte("another key")})
	bad := startTestAgent(t, tlsHub, pinned(wrong), "wrong-pin", Capabilities{MaxJobs: 1})
	if err := <-bad.done; !errors.Is(err, errPinMismatch) {
		t.Fatalf("wrong pin: Run = %v, want the pin mismatch", err)
	}
	bad.done <- nil
	for drained := false; !drained; {
		select {
		case event := <-events:
			if event.Kind == EventRejected || event.Kind == EventRegistered {
				t.Fatalf("a worker with a wrong pin reached the handshake: %+v", event)
			}
		default:
			drained = true
		}
	}
}

func TestWatchdogLosesAStalledJobButNotAPausedOne(t *testing.T) {
	token := mustToken(t)
	// The agent clock does not move, so the worker sends no heartbeats; a
	// long heartbeat limit keeps expiry out of this test.
	hub := newTestHubWithConfig(t, HubConfig{HeartbeatInterval: time.Hour}, token)
	worker := startTestAgent(t, hub, token, "w", Capabilities{MaxJobs: 2})
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")

	paused := startJob(t, hub, f.job())
	pausedProcess := worker.process(t)
	if _, err := paused.Stdin().Write([]byte("p")); err != nil {
		t.Fatal(err)
	}
	stalled := startJob(t, hub, f.job())
	stalledProcess := worker.process(t)

	// Exactly the stall timeout is not a stall.
	hub.clock.Advance(DefaultStallTimeout)
	hub.hub.watchdog()
	select {
	case <-stalled.(*remoteJob).done:
		t.Fatal("job lost at exactly the stall timeout")
	default:
	}
	// Past it, the supervised check loop loses the silent job.
	eventually(t, func() { hub.clock.Advance(DefaultCheckInterval) }, func() bool {
		select {
		case <-stalled.(*remoteJob).done:
			return true
		default:
			return false
		}
	})
	if err := waitResult(t, stalled); !errors.Is(err, ffmpeg.ErrRunnerLost) {
		t.Fatalf("stalled job = %v, want ErrRunnerLost", err)
	}
	if event := hub.waitEvent(t, EventJobStalled); event.JobID != stalled.(*remoteJob).id {
		t.Fatalf("stalled event names job %s", event.JobID)
	}
	eventually(t, nil, func() bool {
		stalledProcess.mu.Lock()
		defer stalledProcess.mu.Unlock()
		return stalledProcess.stopped
	})

	// The paused job survived the same silence. Resuming restarts its
	// clock; silence after that loses it too.
	select {
	case <-paused.(*remoteJob).done:
		t.Fatal("a paused job was lost")
	default:
	}
	if _, err := paused.Stdin().Write([]byte("u")); err != nil {
		t.Fatal(err)
	}
	hub.clock.Advance(DefaultStallTimeout)
	hub.hub.watchdog()
	select {
	case <-paused.(*remoteJob).done:
		t.Fatal("a resumed job was lost before the stall timeout")
	default:
	}
	hub.clock.Advance(DefaultCheckInterval)
	hub.hub.watchdog()
	if err := waitResult(t, paused); !errors.Is(err, ffmpeg.ErrRunnerLost) {
		t.Fatalf("resumed silent job = %v, want ErrRunnerLost", err)
	}
	if event := hub.waitEvent(t, EventJobStalled); event.JobID != paused.(*remoteJob).id {
		t.Fatalf("stalled event names job %s", event.JobID)
	}
	eventually(t, nil, func() bool {
		pausedProcess.mu.Lock()
		defer pausedProcess.mu.Unlock()
		return pausedProcess.stopped && pausedProcess.stdin.String() == "pu"
	})
}

func TestWorkerRefusesHostPathsInTheJobArgv(t *testing.T) {
	agent, err := NewAgent(AgentConfig{ServerURL: "http://127.0.0.1:8096", JoinToken: mustToken(t), StateDir: t.TempDir(), WorkDir: t.TempDir(), Clock: clock.NewFake(testEpoch)})
	if err != nil {
		t.Fatal(err)
	}
	job := func(extra ...string) *agentJob {
		args := append([]string{"-init_hw_device", "vaapi=va:/dev/dri/renderD128"}, extra...)
		args = append(args, "-i", PlaceholderSource, "-hls_segment_filename", PlaceholderOut+"/abc%d.ts", PlaceholderOut+"/abc.m3u8")
		return &agentJob{agent: agent, spec: JobSpec{ID: "job", Token: "job-bearer-token", Args: args, SourceURL: pathJobs + "job/source"}}
	}
	args, proxy, err := job().resolveArgs(t.Context(), "/work/job")
	if err != nil {
		t.Fatalf("device node refused: %v", err)
	}
	proxy.Close()
	// The streamed source goes through the loopback proxy: the job token
	// is not in the argv.
	if got := strings.Join(args, " "); !strings.Contains(got, "/work/job/abc.m3u8") || !strings.Contains(got, proxy.url) ||
		!strings.HasPrefix(proxy.url, "http://127.0.0.1:") || strings.Contains(got, "job-bearer-token") || strings.Contains(got, "Authorization") {
		t.Fatalf("resolved argv = %s", got)
	}
	for name, extra := range map[string][]string{
		"host file":     {"-vf", "subtitles=f=/etc/passwd"},
		"host operand":  {"-attach", "/home/user/font.ttf"},
		"descriptor":    {"-progress", "pipe:1"},
		"fd":            {"-progress", "fd:3"},
		"escaping name": nil,
		"second source": {"-i", PlaceholderSource},
	} {
		spec := job(extra...)
		if name == "escaping name" {
			spec.spec.Args[len(spec.spec.Args)-1] = PlaceholderOut + "/../abc.m3u8"
		}
		if _, proxy, err := spec.resolveArgs(t.Context(), "/work/job"); err == nil || proxy != nil {
			t.Errorf("%s: worker accepted the argv", name)
		}
	}
}

func TestThrottleKeysFollowTheWorkerFFmpeg(t *testing.T) {
	cases := []struct {
		in        string
		pKeyPause bool
		want      string
	}{
		{"p", true, "p"}, {"c", true, "p"}, {"u", true, "u"}, {"\n", true, "u"},
		{"p", false, "c"}, {"c", false, "c"}, {"u", false, "\n"}, {"\n", false, "\n"},
		{"q\n", true, "q\n"}, {"q\n", false, "q\n"},
	}
	for _, c := range cases {
		if got := string(throttleKeys([]byte(c.in), c.pKeyPause)); got != c.want {
			t.Errorf("throttleKeys(%q, %v) = %q, want %q", c.in, c.pKeyPause, got, c.want)
		}
	}
}

// A client that connects to the public route and never sends hello is
// closed by the same expiry check as a silent worker.
func TestSilentUnauthenticatedConnectionIsClosed(t *testing.T) {
	hub := newTestHub(t, mustToken(t))
	ws := dialRaw(t, hub)
	connections := func() int {
		hub.hub.mu.Lock()
		defer hub.hub.mu.Unlock()
		return len(hub.hub.conns)
	}
	// The upgrade can finish before the hub tracks the connection.
	eventually(t, nil, func() bool { return connections() == 1 })
	eventually(t, func() { hub.clock.Advance(DefaultCheckInterval) }, func() bool {
		hub.hub.mu.Lock()
		defer hub.hub.mu.Unlock()
		return len(hub.hub.conns) == 0
	})
	hub.waitEvent(t, EventExpired)
	var message Message
	if err := websocket.JSON.Receive(ws, &message); err == nil {
		t.Fatalf("silent connection still delivers %+v", message)
	}
}
