// SPDX-License-Identifier: GPL-2.0-only

package replica

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/transcodeworker"
)

// Sessions is the session manager as the routing sees it.
type Sessions interface {
	SessionByID(id string) (session.Snapshot, bool)
	Record(id string) (store.PlaySessionRecord, bool, error)
	Restore(records []store.PlaySessionRecord) int
	Remove(id string) bool
}

// Grants is the PlaySessionId grant store as the routing sees it.
type Grants interface {
	Has(playSessionID string) bool
	Restore(records []store.PlayGrantRecord)
}

// OwnerStore reads and claims the owners of rows.
type OwnerStore interface {
	PlaySessionOwner(ctx context.Context, id string) (string, bool, error)
	PlayGrantOwner(ctx context.Context, playSessionID string) (string, bool, error)
	ClaimPlaySession(ctx context.Context, id, me string, now time.Time) (store.PlaySessionRecord, bool, error)
	ClaimNewPlaySession(ctx context.Context, record store.PlaySessionRecord, me string, now time.Time) (bool, error)
	ClaimPlayGrant(ctx context.Context, playSessionID, me string, now time.Time) (store.PlayGrantRecord, bool, error)
}

// Identities resolves the credential of a request.
type Identities interface {
	Resolve(ctx context.Context, request *http.Request) (auth.Identity, error)
}

// RouterConfig configures a Router.
type RouterConfig struct {
	Registry *Registry
	Signer   *Signer
	Store    OwnerStore
	Sessions Sessions
	Grants   Grants
	Resolver Identities
	// LegacyAuthorization reports whether the legacy credential forms
	// apply; nil means off.
	LegacyAuthorization func() bool
	Clock               clock.Clock
	// Flush writes the play session and grant changes now. The router calls
	// it after each local request that changes state, so that a peer finds
	// the owner of a new grant at once. Optional.
	Flush func(context.Context) error
	// Transport carries the forwards; a new transport when nil.
	Transport http.RoundTripper
	Logger    *slog.Logger
}

type drainState int

const (
	stateServing drainState = iota
	stateHolding
	stateReleased
)

// keyKind is what owns a request.
type keyKind int

const (
	keyNone keyKind = iota
	keySession
	keyGrant
)

type requestKey struct {
	kind keyKind
	id   string
}

// Router sends each request to the replica that owns its play session or
// grant (#652). Wrap is its router wrapper; BeginDrain, WaitIdle and
// EndDrain run the handover at SIGTERM.
type Router struct {
	config    RouterConfig
	forwarder *forwarder

	mu       sync.Mutex
	state    drainState
	held     chan struct{}
	inflight int
	idle     chan struct{}
	sessions map[string]struct{}
}

// NewRouter returns a router.
func NewRouter(config RouterConfig) (*Router, error) {
	if config.Registry == nil || config.Signer == nil || config.Store == nil || config.Sessions == nil || config.Grants == nil ||
		config.Resolver == nil || config.Clock == nil {
		return nil, errors.New("replica: the router needs a registry, a signer, a store, sessions, grants, a resolver and a clock")
	}
	if config.Transport == nil {
		config.Transport = newTransport()
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	return &Router{
		config:    config,
		forwarder: &forwarder{signer: config.Signer, from: config.Registry.ID(), transport: config.Transport},
		sessions:  make(map[string]struct{}),
	}, nil
}

// Close drops the idle connections to the peers.
func (rt *Router) Close() {
	if transport, ok := rt.config.Transport.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}
}

// ClaimedSessions marks sessions that this replica claimed outside a
// request, such as at startup.
func (rt *Router) ClaimedSessions(ids []string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, id := range ids {
		rt.sessions[id] = struct{}{}
	}
}

// BeginDrain holds every new routed request until EndDrain.
func (rt *Router) BeginDrain() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.state == stateServing {
		rt.state = stateHolding
		rt.held = make(chan struct{})
	}
}

// WaitIdle waits until no routed request that changes state is in flight.
// It counts every request other than GET and HEAD: a stream or a WebSocket
// can last for hours, and the drain does not wait for it.
func (rt *Router) WaitIdle(ctx context.Context) error {
	rt.mu.Lock()
	if rt.inflight == 0 {
		rt.mu.Unlock()
		return nil
	}
	if rt.idle == nil {
		rt.idle = make(chan struct{})
	}
	idle := rt.idle
	rt.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EndDrain lets the held requests go on: from now on they go to a peer.
func (rt *Router) EndDrain() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.state == stateReleased {
		return
	}
	if rt.held != nil {
		close(rt.held)
		rt.held = nil
	}
	rt.state = stateReleased
}

// enter waits while the router holds requests, and returns the state.
func (rt *Router) enter(ctx context.Context) (drainState, error) {
	for {
		rt.mu.Lock()
		state, held := rt.state, rt.held
		rt.mu.Unlock()
		if state != stateHolding {
			return state, nil
		}
		select {
		case <-held:
		case <-ctx.Done():
			return state, ctx.Err()
		}
	}
}

func (rt *Router) begin(request *http.Request) func() {
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		return func() {}
	}
	rt.mu.Lock()
	rt.inflight++
	rt.mu.Unlock()
	return func() {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		rt.inflight--
		if rt.inflight == 0 && rt.idle != nil {
			close(rt.idle)
			rt.idle = nil
		}
	}
}

// Wrap is the router wrapper. It runs before the authorizer.
func (rt *Router) Wrap(route router.Route, next http.Handler) http.Handler {
	if rt == nil {
		return next
	}
	if route.Worker {
		return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			rt.serveWorker(route, w, request, next)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if IsLocalForward(request) {
			next.ServeHTTP(w, request)
			return
		}
		key := rt.keyOf(route, request)
		if key.kind == keyNone {
			next.ServeHTTP(w, request)
			return
		}
		rt.serveKeyed(key, w, request, next)
	})
}

// IsLocalForward reports a request that a peer forwarded for this replica
// to serve itself (#663).
func IsLocalForward(request *http.Request) bool {
	forwarded, ok := ForwardedFrom(request.Context())
	return ok && forwarded.Local
}

// ServeOnOwner sends a request that acts on the play session sessionID of
// another client, such as a remote-control command, to the live peer that
// owns that session (#663). The peer serves it itself: it holds the
// session's WebSocket. ServeOnOwner reports false, and writes nothing, when
// this replica serves the request: it holds the session, the session has no
// live owner, or a peer sent the request here to serve.
func (rt *Router) ServeOnOwner(w http.ResponseWriter, request *http.Request, sessionID string) bool {
	if rt == nil || sessionID == "" || IsLocalForward(request) {
		return false
	}
	if _, found := rt.config.Sessions.SessionByID(sessionID); found {
		return false
	}
	owner, _, err := rt.config.Store.PlaySessionOwner(request.Context(), sessionID)
	if err != nil {
		rt.config.Logger.Warn("read the owner of a session", "error", err.Error())
		return false
	}
	if owner == "" || owner == rt.config.Registry.ID() {
		return false
	}
	peer, live := rt.config.Registry.Peer(owner)
	if !live {
		if err := rt.config.Registry.Refresh(request.Context()); err == nil {
			peer, live = rt.config.Registry.Peer(owner)
		}
	}
	if !live {
		return false
	}
	body, _, err := replayableBody(request)
	if err != nil {
		writeUnavailable(w)
		return true
	}
	if rt.forwarder.forwardScoped(w, request, peer.URL, body(), true) != forwardDone {
		writeUnavailable(w)
	}
	return true
}

// keyOf finds the play session or the grant that owns request.
func (rt *Router) keyOf(route router.Route, request *http.Request) requestKey {
	legacy := rt.config.LegacyAuthorization != nil && rt.config.LegacyAuthorization()
	credentials := auth.Extract(request, legacy)
	if credentials.Token != "" {
		identity, err := rt.config.Resolver.Resolve(request.Context(), request)
		if err == nil && identity.Authenticated && identity.Client != "" && identity.DeviceID != "" {
			return requestKey{kind: keySession, id: session.IDFor(identity.Client, identity.DeviceID)}
		}
	} else if route.Anonymous && credentials.Client != "" && credentials.DeviceID != "" {
		// A sign-in names its client and device before it has a token.
		return requestKey{kind: keySession, id: session.IDFor(credentials.Client, credentials.DeviceID)}
	}
	if route.PlaySessionCredential {
		if playSessionID := queryValue(request, "PlaySessionId"); playSessionID != "" {
			return requestKey{kind: keyGrant, id: playSessionID}
		}
	}
	return requestKey{}
}

func queryValue(request *http.Request, name string) string {
	for key, values := range request.URL.Query() {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func (rt *Router) serveKeyed(key requestKey, w http.ResponseWriter, request *http.Request, next http.Handler) {
	state, err := rt.enter(request.Context())
	if err != nil {
		return
	}
	_, forwarded := ForwardedFrom(request.Context())
	if state == stateReleased {
		if forwarded {
			writeMoved(w)
			return
		}
		if peer, ok := rt.config.Registry.AnyPeer(); ok {
			body, _, err := replayableBody(request)
			if err != nil {
				return
			}
			if rt.forwarder.forward(w, request, peer.URL, body()) != forwardDone {
				writeUnavailable(w)
			}
			return
		}
		// No peer: this replica is the last one, and serves until it stops.
		next.ServeHTTP(w, request)
		return
	}

	if rt.owns(key) {
		rt.serveLocal(key, w, request, next)
		return
	}
	var body func() io.ReadCloser
	replayable := true
	for attempt := 0; attempt < 2; attempt++ {
		owner, err := rt.ownerOf(request.Context(), key)
		if err != nil {
			rt.config.Logger.Warn("read the owner of a request", "error", err.Error())
			writeUnavailable(w)
			return
		}
		if owner != "" && owner != rt.config.Registry.ID() {
			peer, live := rt.config.Registry.Peer(owner)
			if !live {
				// The cache may be older than the owner's row.
				if err := rt.config.Registry.Refresh(request.Context()); err == nil {
					peer, live = rt.config.Registry.Peer(owner)
				}
			}
			if live {
				if forwarded {
					writeMoved(w)
					return
				}
				if !replayable && attempt > 0 {
					writeUnavailable(w)
					return
				}
				if body == nil {
					body, replayable, err = replayableBody(request)
					if err != nil {
						return
					}
				}
				switch rt.forwarder.forward(w, request, peer.URL, body()) {
				case forwardDone:
					return
				case forwardMoved:
					continue
				default:
					// The owner holds its lease but does not answer:
					// it may have crashed. Its rows can be claimed
					// when the lease ends.
					writeUnavailable(w)
					return
				}
			}
		}
		claimed, err := rt.claim(request.Context(), key)
		if err != nil {
			rt.config.Logger.Warn("claim the owner of a request", "error", err.Error())
			writeUnavailable(w)
			return
		}
		if claimed {
			if body != nil {
				request = request.Clone(request.Context())
				request.Body = body()
			}
			rt.serveLocal(key, w, request, next)
			return
		}
		// A peer claimed the row first: read its owner again.
	}
	if forwarded {
		writeMoved(w)
		return
	}
	writeUnavailable(w)
}

// owns reports whether this replica serves key from memory.
func (rt *Router) owns(key requestKey) bool {
	switch key.kind {
	case keySession:
		rt.mu.Lock()
		_, claimed := rt.sessions[key.id]
		rt.mu.Unlock()
		if !claimed {
			return false
		}
		_, found := rt.config.Sessions.SessionByID(key.id)
		return found
	case keyGrant:
		return rt.config.Grants.Has(key.id)
	}
	return false
}

// ownerOf reads the owner of key's row; "" when the row has no owner or
// does not exist.
func (rt *Router) ownerOf(ctx context.Context, key requestKey) (string, error) {
	var owner string
	var err error
	switch key.kind {
	case keySession:
		owner, _, err = rt.config.Store.PlaySessionOwner(ctx, key.id)
	case keyGrant:
		owner, _, err = rt.config.Store.PlayGrantOwner(ctx, key.id)
	}
	return owner, err
}

// claim makes this replica the owner of key's row and loads the row. It
// reports true also when there is no row: the request then makes the
// session (serveLocal claims it after the request) or fails.
func (rt *Router) claim(ctx context.Context, key requestKey) (bool, error) {
	me, now := rt.config.Registry.ID(), rt.config.Clock.Now()
	switch key.kind {
	case keySession:
		record, claimed, err := rt.config.Store.ClaimPlaySession(ctx, key.id, me, now)
		if err != nil {
			return false, err
		}
		if !claimed {
			_, found, err := rt.config.Store.PlaySessionOwner(ctx, key.id)
			return !found, err
		}
		rt.config.Sessions.Restore([]store.PlaySessionRecord{record})
		rt.ClaimedSessions([]string{key.id})
		return true, nil
	case keyGrant:
		grant, claimed, err := rt.config.Store.ClaimPlayGrant(ctx, key.id, me, now)
		if err != nil {
			return false, err
		}
		if !claimed {
			_, found, err := rt.config.Store.PlayGrantOwner(ctx, key.id)
			return !found, err
		}
		rt.config.Grants.Restore([]store.PlayGrantRecord{grant})
		return true, nil
	}
	return true, nil
}

// serveLocal serves request here. A session that the request made is
// claimed after it; when a peer made the same session first, this replica
// drops its copy.
func (rt *Router) serveLocal(key requestKey, w http.ResponseWriter, request *http.Request, next http.Handler) {
	done := rt.begin(request)
	defer done()
	next.ServeHTTP(w, request)
	if rt.config.Flush != nil && request.Method != http.MethodGet && request.Method != http.MethodHead {
		if err := rt.config.Flush(context.WithoutCancel(request.Context())); err != nil {
			rt.config.Logger.Warn("write play session changes", "error", err.Error())
		}
	}
	if key.kind != keySession {
		return
	}
	rt.mu.Lock()
	_, claimed := rt.sessions[key.id]
	rt.mu.Unlock()
	if claimed {
		return
	}
	record, found, err := rt.config.Sessions.Record(key.id)
	if err != nil || !found {
		return
	}
	// The request context may be over; the claim must still land.
	ctx := context.WithoutCancel(request.Context())
	ok, err := rt.config.Store.ClaimNewPlaySession(ctx, record, rt.config.Registry.ID(), rt.config.Clock.Now())
	if err != nil {
		rt.config.Logger.Warn("claim a new play session", "error", err.Error())
		return
	}
	if ok {
		rt.ClaimedSessions([]string{key.id})
		return
	}
	rt.config.Sessions.Remove(key.id)
}

// serveWorker routes a worker request. A job request goes to the replica
// that its transcodeworker.ReplicaHeader names; the control connection of a
// draining replica goes to a peer.
func (rt *Router) serveWorker(route router.Route, w http.ResponseWriter, request *http.Request, next http.Handler) {
	if _, forwarded := ForwardedFrom(request.Context()); forwarded {
		next.ServeHTTP(w, request)
		return
	}
	var target string
	if route.Pattern == transcodeworker.PathConnect {
		rt.mu.Lock()
		state := rt.state
		rt.mu.Unlock()
		if state != stateServing {
			if peer, ok := rt.config.Registry.AnyPeer(); ok {
				target = peer.URL
			}
		}
	} else if owner := request.Header.Get(transcodeworker.ReplicaHeader); owner != "" && owner != rt.config.Registry.ID() {
		if peer, ok := rt.config.Registry.Peer(owner); ok {
			target = peer.URL
		}
	}
	if target == "" {
		next.ServeHTTP(w, request)
		return
	}
	body, _, err := replayableBody(request)
	if err != nil {
		return
	}
	if rt.forwarder.forward(w, request, target, body()) != forwardDone {
		writeUnavailable(w)
	}
}
