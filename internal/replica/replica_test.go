// SPDX-License-Identifier: GPL-2.0-only

package replica

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/auth/playgrant"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
	"github.com/deadfin-org/deadfin/internal/transcodeworker"
)

var (
	testEpoch = time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)
	testUser  = dto.UUID{0xa1, 0x52}
	testItem  = dto.UUID{0xb1, 0x52}
)

const testSecret = "replica-test-secret-0123456789"

// tokenResolver resolves "tok-<device>" to a user of client "app".
type tokenResolver struct{}

func (tokenResolver) Resolve(_ context.Context, request *http.Request) (auth.Identity, error) {
	token := auth.Extract(request, false).Token
	device, ok := strings.CutPrefix(token, "tok-")
	if !ok {
		return auth.Identity{}, nil
	}
	return auth.Identity{Authenticated: true, Token: token, UserID: testUser, Client: "app", DeviceID: device, Version: "1"}, nil
}

// served is what a test replica answers.
type served struct {
	Replica string `json:"replica"`
	Remote  string `json:"remote"`
	TLS     bool   `json:"tls"`
	Body    string `json:"body"`
}

// testReplica is one replica: a registry, a router and an HTTP server.
type testReplica struct {
	name     string
	registry *Registry
	router   *Router
	sessions *session.Manager
	grants   *playgrant.Store
	server   *httptest.Server
	posts    chan chan struct{}
	cluster  *Cluster
	// events and messages record what the peers sent (#663).
	events   chan json.RawMessage
	messages chan string
}

type cluster struct {
	t      *testing.T
	stores store.Stores
	store  *store.ReplicaStore
	clock  *clock.Fake
	signer *Signer
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	fake := clock.NewFake(testEpoch)
	signer, err := NewSigner([]byte(testSecret), fake)
	if err != nil {
		t.Fatal(err)
	}
	return &cluster{t: t, stores: stores, store: store.NewReplicaStore(stores), clock: fake, signer: signer}
}

// start runs a replica. Its server URL is known only after the listener
// opens, so the registry starts after the server.
func (c *cluster) start(name string) *testReplica {
	t := c.t
	t.Helper()
	replica := &testReplica{
		name: name, sessions: session.New(c.clock), grants: playgrant.New(c.clock), posts: make(chan chan struct{}, 4),
		events: make(chan json.RawMessage, 16), messages: make(chan string, 4),
	}
	handler := &lateHandler{}
	replica.server = httptest.NewServer(handler)
	registry, err := StartRegistry(t.Context(), RegistryConfig{Store: c.store, Clock: c.clock, ID: name, URL: replica.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	replica.registry = registry
	routing, err := NewRouter(RouterConfig{
		Registry: registry, Signer: c.signer, Store: c.store, Sessions: replica.sessions, Grants: replica.grants,
		Resolver: tokenResolver{}, Clock: c.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	replica.router = routing
	peers, err := NewCluster(ClusterConfig{Registry: registry, Signer: c.signer})
	if err != nil {
		t.Fatal(err)
	}
	replica.cluster = peers
	rt := router.New(routing.Wrap)
	answer := func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		_ = json.NewEncoder(w).Encode(served{Replica: name, Remote: request.RemoteAddr, TLS: request.TLS != nil, Body: string(body)})
	}
	authenticated := func(w http.ResponseWriter, request *http.Request) {
		identity, _ := tokenResolver{}.Resolve(request.Context(), request)
		if !identity.Authenticated {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		replica.sessions.RecordAuthenticatedActivity(session.Activity{Client: identity.Client, DeviceID: identity.DeviceID, UserID: identity.UserID})
		if request.Method == http.MethodPost {
			select {
			case release := <-replica.posts:
				<-release
			default:
			}
		}
		answer(w, request)
	}
	rt.Handle(http.MethodGet, "/Sessions/Playing", router.Route{OperationID: "Get"}, authenticated)
	rt.Handle(http.MethodPost, "/Sessions/Playing/Progress", router.Route{OperationID: "Progress"}, authenticated)
	rt.Handle(http.MethodGet, "/Videos/{itemId}/main.m3u8", router.Route{OperationID: "Playlist", PlaySessionCredential: true}, func(w http.ResponseWriter, request *http.Request) {
		if !replica.grants.Has(request.URL.Query().Get("PlaySessionId")) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		answer(w, request)
	})
	// GET /Sessions lists the session IDs of every replica, as the
	// GetSessions handler does.
	rt.Handle(http.MethodGet, "/Sessions", router.Route{OperationID: "GetSessions"}, func(w http.ResponseWriter, request *http.Request) {
		ids := []string{}
		for _, snapshot := range replica.sessions.Sessions(session.Query{}) {
			ids = append(ids, snapshot.ID)
		}
		for _, body := range peers.Gather(request) {
			var more []string
			_ = json.Unmarshal(body, &more)
			ids = append(ids, more...)
		}
		_ = json.NewEncoder(w).Encode(ids)
	})
	// A command reaches the replica that holds the target session.
	rt.Handle(http.MethodPost, "/Sessions/{sessionId}/Message", router.Route{OperationID: "Message"}, func(w http.ResponseWriter, request *http.Request) {
		target := request.PathValue("sessionId")
		if target == "" {
			target = strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/Sessions/"), "/Message")
		}
		if routing.ServeOnOwner(w, request, target) {
			return
		}
		if _, found := replica.sessions.SessionByID(target); !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(request.Body)
		replica.messages <- string(body)
		answer(w, request)
	})
	rt.Handle(http.MethodPut, transcodeworker.PathFile, router.Route{OperationID: "File", Worker: true}, answer)
	rt.Handle(http.MethodGet, transcodeworker.PathConnect, router.Route{OperationID: "Connect", Worker: true}, answer)
	handler.set(c.signer.Accept(PeerHandler(PeerEndpoints{
		Events: func(_ context.Context, events []json.RawMessage) {
			for _, event := range events {
				replica.events <- event
			}
		},
	}, rt)))
	t.Cleanup(func() {
		replica.server.Close()
		peers.Close()
		routing.Close()
		registry.Close()
		_ = replica.sessions.Close()
	})
	return replica
}

// lateHandler serves the handler that the replica sets after its server
// opened.
type lateHandler struct {
	mu      sync.Mutex
	handler http.Handler
}

func (h *lateHandler) set(handler http.Handler) {
	h.mu.Lock()
	h.handler = handler
	h.mu.Unlock()
}

func (h *lateHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	h.mu.Lock()
	handler := h.handler
	h.mu.Unlock()
	handler.ServeHTTP(w, request)
}

func (c *cluster) refresh(replicas ...*testReplica) {
	for _, replica := range replicas {
		if err := replica.registry.Refresh(c.t.Context()); err != nil {
			c.t.Fatal(err)
		}
	}
}

func call(t *testing.T, replica *testReplica, method, path, token, body string) (int, served, http.Header) {
	t.Helper()
	request, err := http.NewRequest(method, replica.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", `MediaBrowser Client="app", Device="d", DeviceId="ignored", Version="1", Token="`+token+`"`)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var answer served
	data, _ := io.ReadAll(response.Body)
	_ = json.Unmarshal(data, &answer)
	return response.StatusCode, answer, response.Header
}

func leakCheck(t *testing.T) {
	before := goleak.IgnoreCurrent()
	t.Cleanup(func() {
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		goleak.VerifyNone(t, before)
	})
}

// TestSessionRequestsGoToTheOwner covers #652: the replica that first
// serves a device owns its session; the other replica forwards the device's
// requests, with the client's address, and the owner answers.
func TestSessionRequestsGoToTheOwner(t *testing.T) {
	leakCheck(t)
	c := newCluster(t)
	a, b := c.start("a"), c.start("b")
	c.refresh(a, b)

	code, answer, _ := call(t, a, http.MethodGet, "/Sessions/Playing", "tok-tv", "")
	if code != http.StatusOK || answer.Replica != "a" {
		t.Fatalf("first request = %d %+v", code, answer)
	}
	id := session.IDFor("app", "tv")
	if owner, _, err := c.store.PlaySessionOwner(t.Context(), id); err != nil || owner != "a" {
		t.Fatalf("owner after the first request = %q, %v", owner, err)
	}
	code, answer, _ = call(t, b, http.MethodPost, "/Sessions/Playing/Progress", "tok-tv", `{"PositionTicks":5}`)
	if code != http.StatusOK || answer.Replica != "a" || answer.Body != `{"PositionTicks":5}` || !strings.HasPrefix(answer.Remote, "127.0.0.1:") || answer.TLS {
		t.Fatalf("request through b = %d %+v", code, answer)
	}
	if _, found := b.sessions.SessionByID(id); found {
		t.Fatal("b keeps a copy of a's session")
	}
	// A request with no device is served where it lands.
	if code, answer, _ := call(t, b, http.MethodGet, "/Sessions/Playing", "", ""); code != http.StatusUnauthorized || answer.Replica != "" {
		t.Fatalf("request with no credential = %d %+v", code, answer)
	}
	// A forged forward is not trusted: the headers are dropped.
	request, _ := http.NewRequest(http.MethodGet, a.server.URL+"/Sessions/Playing", nil)
	request.Header.Set("Authorization", `MediaBrowser Client="app", Device="d", DeviceId="x", Version="1", Token="tok-tv"`)
	request.Header.Set(HeaderFrom, "b")
	request.Header.Set(HeaderPeer, "10.9.9.9:1")
	request.Header.Set(HeaderTime, "1")
	request.Header.Set(HeaderSignature, "forged")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var forged served
	_ = json.NewDecoder(response.Body).Decode(&forged)
	_ = response.Body.Close()
	if forged.Remote == "10.9.9.9:1" || forged.Replica != "a" {
		t.Fatalf("forged forward = %+v", forged)
	}
}

// TestDrainHandsTheSessionsToThePeer covers the handover: a draining
// replica holds its routed requests, flushes, releases its rows and then
// forwards; the peer claims and restores each session on its next request.
func TestDrainHandsTheSessionsToThePeer(t *testing.T) {
	leakCheck(t)
	c := newCluster(t)
	a, b := c.start("a"), c.start("b")
	c.refresh(a, b)
	if code, answer, _ := call(t, a, http.MethodGet, "/Sessions/Playing", "tok-tv", ""); code != http.StatusOK || answer.Replica != "a" {
		t.Fatalf("first request = %d %+v", code, answer)
	}
	id := session.IDFor("app", "tv")
	// a's write-behind stored the session state.
	record, found, err := a.sessions.Record(id)
	if err != nil || !found {
		t.Fatalf("Record = %v, %v", found, err)
	}
	if err := store.NewPlaySessionStore(c.stores).WithOwner("a").SavePlaySessions(t.Context(), store.PlaySessionChanges{Sessions: []store.PlaySessionRecord{record}}); err != nil {
		t.Fatal(err)
	}

	// A progress report is in flight on a when the drain starts.
	release := make(chan struct{})
	a.posts <- release
	inflight := make(chan served, 1)
	go func() {
		_, answer, _ := call(t, a, http.MethodPost, "/Sessions/Playing/Progress", "tok-tv", "x")
		inflight <- answer
	}()
	waitFor(t, func() bool {
		a.router.mu.Lock()
		defer a.router.mu.Unlock()
		return a.router.inflight == 1
	})
	a.router.BeginDrain()
	if err := a.registry.MarkDraining(t.Context()); err != nil {
		t.Fatal(err)
	}
	idle := make(chan error, 1)
	go func() { idle <- a.router.WaitIdle(context.Background()) }()
	select {
	case <-idle:
		t.Fatal("WaitIdle returned with a POST in flight")
	default:
	}
	close(release)
	if err := <-idle; err != nil {
		t.Fatal(err)
	}
	if answer := <-inflight; answer.Replica != "a" {
		t.Fatalf("in-flight request = %+v", answer)
	}
	// A new request to a waits for the end of the drain.
	held := make(chan served, 1)
	go func() {
		_, answer, _ := call(t, a, http.MethodGet, "/Sessions/Playing", "tok-tv", "")
		held <- answer
	}()
	if err := a.registry.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.refresh(b)
	a.router.EndDrain()
	if answer := <-held; answer.Replica != "b" {
		t.Fatalf("held request after the drain = %+v, want b", answer)
	}
	snapshot, found := b.sessions.SessionByID(id)
	if !found || snapshot.UserID != testUser {
		t.Fatalf("b restored %+v, %v", snapshot, found)
	}
	if owner, _, err := c.store.PlaySessionOwner(t.Context(), id); err != nil || owner != "b" {
		t.Fatalf("owner after the handover = %q, %v", owner, err)
	}
	if code, answer, _ := call(t, b, http.MethodGet, "/Sessions/Playing", "tok-tv", ""); code != http.StatusOK || answer.Replica != "b" {
		t.Fatalf("request to b = %d %+v", code, answer)
	}
}

// TestPeerThatReleasedAnswersMoved: b still has a's old row in view when a
// has released it; a answers 421 and b claims the session.
func TestPeerThatReleasedAnswersMoved(t *testing.T) {
	leakCheck(t)
	c := newCluster(t)
	a, b := c.start("a"), c.start("b")
	c.refresh(a, b)
	if code, _, _ := call(t, a, http.MethodGet, "/Sessions/Playing", "tok-tv", ""); code != http.StatusOK {
		t.Fatal(code)
	}
	id := session.IDFor("app", "tv")
	a.router.BeginDrain()
	a.router.EndDrain()
	// a has not released its rows yet, and b's view says a is live: b
	// forwards, a answers 421 (it serves nothing now), b reads the owner
	// again (still a) and cannot claim, so b answers 503.
	if code, _, header := call(t, b, http.MethodPost, "/Sessions/Playing/Progress", "tok-tv", "x"); code != http.StatusServiceUnavailable || header.Get("Retry-After") == "" {
		t.Fatalf("before the release = %d", code)
	}
	if err := c.store.ReleaseReplica(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	code, answer, _ := call(t, b, http.MethodPost, "/Sessions/Playing/Progress", "tok-tv", "after")
	if code != http.StatusOK || answer.Replica != "b" || answer.Body != "after" {
		t.Fatalf("after the release = %d %+v", code, answer)
	}
	if owner, _, _ := c.store.PlaySessionOwner(t.Context(), id); owner != "b" {
		t.Fatalf("owner = %q", owner)
	}
}

// TestGrantAndWorkerRequestsGoToTheOwner: a media element with only a
// PlaySessionId reaches the grant's owner, and a worker job request reaches
// the replica its header names.
func TestGrantAndWorkerRequestsGoToTheOwner(t *testing.T) {
	leakCheck(t)
	c := newCluster(t)
	a, b := c.start("a"), c.start("b")
	c.refresh(a, b)
	a.grants.Issue("play-1", testUser, testItem, nil)
	if err := store.NewPlaySessionStore(c.stores).WithOwner("a").SavePlaySessions(t.Context(), store.PlaySessionChanges{
		Grants: []store.PlayGrantRecord{{PlaySessionID: "play-1", UserID: testUser, ItemID: testItem, LastUsed: testEpoch}},
	}); err != nil {
		t.Fatal(err)
	}
	path := "/Videos/" + testItem.Dashed() + "/main.m3u8?PlaySessionId=play-1"
	if code, answer, _ := call(t, b, http.MethodGet, path, "", ""); code != http.StatusOK || answer.Replica != "a" {
		t.Fatalf("grant request through b = %d %+v", code, answer)
	}
	// After a releases, b claims and restores the grant.
	if err := c.store.ReleaseReplica(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	if code, answer, _ := call(t, b, http.MethodGet, path, "", ""); code != http.StatusOK || answer.Replica != "b" || !b.grants.Has("play-1") {
		t.Fatalf("grant request after the release = %d %+v", code, answer)
	}

	c.refresh(a, b)
	request, _ := http.NewRequest(http.MethodPut, a.server.URL+"/deadfin/v1/workers/jobs/j1/files/s0.ts", bytes.NewReader([]byte("segment")))
	request.Header.Set(transcodeworker.ReplicaHeader, "b")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var answer served
	_ = json.NewDecoder(response.Body).Decode(&answer)
	_ = response.Body.Close()
	if answer.Replica != "b" || answer.Body != "segment" {
		t.Fatalf("worker request through a = %+v", answer)
	}
	// A draining replica sends the control connection to a peer.
	a.router.BeginDrain()
	if code, answer, _ := call(t, a, http.MethodGet, transcodeworker.PathConnect, "", ""); code != http.StatusOK || answer.Replica != "b" {
		t.Fatalf("connect to a draining replica = %d %+v", code, answer)
	}
	a.router.EndDrain()
}

// TestRegistryHeartbeat: the heartbeat renews the lease on the fake clock,
// a draining peer is no target for new work, and Release deletes the row.
func TestRegistryHeartbeat(t *testing.T) {
	leakCheck(t)
	c := newCluster(t)
	a, err := StartRegistry(t.Context(), RegistryConfig{Store: c.store, Clock: c.clock, ID: "a", URL: "http://a"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := StartRegistry(t.Context(), RegistryConfig{Store: c.store, Clock: c.clock, ID: "b", URL: "http://b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if peer, ok := a.AnyPeer(); !ok || peer.ID != "b" {
		t.Fatalf("AnyPeer = %+v, %v", peer, ok)
	}
	if err := b.MarkDraining(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.AnyPeer(); ok {
		t.Fatal("a draining peer takes new work")
	}
	if _, ok := a.Peer("b"); !ok {
		t.Fatal("a draining peer still owns its rows")
	}
	// Without heartbeats b's lease ends; with them a's stays live.
	b.Close()
	for range 4 {
		c.clock.Advance(DefaultHeartbeat)
		waitFor(t, func() bool {
			live, err := c.store.LiveReplicas(t.Context(), c.clock.Now())
			return err == nil && len(live) > 0 && live[0].ID == "a" && live[0].LeaseExpires.After(c.clock.Now().Add(DefaultLease-DefaultHeartbeat))
		})
	}
	if _, ok := a.Peer("b"); ok {
		t.Fatal("b is live after its lease")
	}
	if err := a.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if live, err := c.store.LiveReplicas(t.Context(), c.clock.Now()); err != nil || len(live) != 0 {
		t.Fatalf("live after release = %+v, %v", live, err)
	}
}

// waitFor waits for a condition that another goroutine makes true.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("condition not met")
		case <-time.After(time.Millisecond):
		}
	}
}

// TestSignerChecksTheForward: a changed URI, an old time or another key
// fails the check.
func TestSignerChecksTheForward(t *testing.T) {
	fake := clock.NewFake(testEpoch)
	signer, _ := NewSigner([]byte(testSecret), fake)
	other, _ := NewSigner([]byte("another-secret-0123456789"), fake)
	request := httptest.NewRequest(http.MethodGet, "/Videos/x?a=1", nil)
	signer.Sign(request.Header, request.Method, request.RequestURI, Forwarded{From: "a", Peer: "10.0.0.1:5", TLS: true})
	if forwarded, ok := signer.Verify(request); !ok || forwarded != (Forwarded{From: "a", Peer: "10.0.0.1:5", TLS: true}) {
		t.Fatalf("Verify = %+v, %v", forwarded, ok)
	}
	if _, ok := other.Verify(request); ok {
		t.Fatal("another key accepted the forward")
	}
	changed := request.Clone(t.Context())
	changed.RequestURI = "/Videos/y?a=1"
	if _, ok := signer.Verify(changed); ok {
		t.Fatal("a changed URI passed")
	}
	fake.Advance(forwardSkew + time.Second)
	if _, ok := signer.Verify(request); ok {
		t.Fatal("an old forward passed")
	}
	if _, err := NewSigner([]byte("short"), fake); err == nil {
		t.Fatal("a short key was accepted")
	}
}
