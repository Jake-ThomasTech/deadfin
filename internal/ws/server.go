// SPDX-License-Identifier: GPL-2.0-only

// Package ws owns Jellyfin WebSocket upgrades, envelopes, keepalive, and feeds.
package ws

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/activitylog"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/dtobuild"
	"github.com/deadfin-org/deadfin/internal/events"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/playstate"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/sessiondto"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/tasks"
	"github.com/deadfin-org/deadfin/internal/user"
	"golang.org/x/net/websocket"
)

const (
	keepAliveTimeout = 60 * time.Second
	keepAliveSweep   = 12 * time.Second
	keepAliveForce   = 45 * time.Second
	connectionBuffer = 32
)

// Config contains process-owned WebSocket collaborators.
type Config struct {
	Clock            clock.Clock
	Random           io.Reader
	Sessions         *session.Manager
	Users            *user.Service
	SessionProjector *sessiondto.Projector
	Items            *store.ItemStore
	ItemBuilder      *dtobuild.Builder
	LibraryChanges   *library.ChangeBus
	RefreshProgress  *library.RefreshProgressBus
	UserDataChanges  *events.Bus[playstate.UserDataChanged]
	Activity         *activitylog.Log
	Tasks            *tasks.Catalog
	// Relay hands this server's events to the peer replicas (#663); nil
	// without replicas. It must not wait.
	Relay func(PeerEvent)
	// PeerSessions returns the peer replicas' sessions for the Sessions
	// feed; nil without replicas.
	PeerSessions func(context.Context, SessionsQuery) []dto.SessionInfoDto
}

// PeerEvent is a server event that one replica relays to the others (#663).
// Kind is the WebSocket message that the event starts.
type PeerEvent struct {
	Kind     MessageType                `json:"Kind"`
	UserData *playstate.UserDataChanged `json:"UserData,omitempty"`
	Library  *library.UpdateInfo        `json:"Library,omitempty"`
	Refresh  *RefreshProgressInfo       `json:"Refresh,omitempty"`
	// Force is a session change that the Sessions feed sends at once.
	Force bool `json:"Force,omitempty"`
}

// SessionsQuery selects the sessions of the Sessions feed: those of UserID,
// or all of them.
type SessionsQuery struct {
	UserID dto.UUID `json:"UserId"`
	All    bool     `json:"All"`
}

// Server owns all live WebSocket connections and their supervised workers.
type Server struct {
	config Config

	mu          sync.Mutex
	closed      bool
	connections map[*connection]struct{}
	bySession   map[string]map[*connection]struct{}

	// peerEvents and peerUserData carry the peers' events to the
	// connections; nil without replicas.
	peerEvents   *events.Bus[PeerEvent]
	peerUserData *events.Bus[playstate.UserDataChanged]
	// relayCancel and relayDone stop the relay goroutine.
	relayCancel context.CancelFunc
	relayDone   chan struct{}
}

// New constructs a WebSocket server.
func New(config Config) *Server {
	if config.Clock == nil {
		panic("ws: clock is required")
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	server := &Server{
		config: config, connections: make(map[*connection]struct{}),
		bySession: make(map[string]map[*connection]struct{}),
	}
	if config.Relay != nil {
		server.peerEvents = events.NewBus[PeerEvent](config.Clock)
		server.peerUserData = events.NewBus[playstate.UserDataChanged](config.Clock)
		ctx, cancel := context.WithCancel(context.Background())
		server.relayCancel, server.relayDone = cancel, make(chan struct{})
		go server.relay(ctx, server.relaySources())
	}
	return server
}

// relaySources subscribes to the events that the peers' WebSockets also
// send. The relay goroutine closes the subscriptions.
func (server *Server) relaySources() relaySources {
	config := server.config
	var sources relaySources
	if config.Sessions != nil {
		sources.sessions = config.Sessions.Subscribe(events.SubscriptionOptions{Buffer: 64})
	}
	if config.UserDataChanges != nil {
		sources.userData = config.UserDataChanges.Subscribe(events.SubscriptionOptions{Buffer: 256})
	}
	if config.LibraryChanges != nil {
		sources.library = config.LibraryChanges.Subscribe(16)
	}
	if config.RefreshProgress != nil {
		sources.refresh = config.RefreshProgress.Subscribe(events.SubscriptionOptions{Buffer: 256})
	}
	if config.Activity != nil {
		sources.activity = config.Activity.Subscribe(events.SubscriptionOptions{Buffer: 64})
	}
	return sources
}

type relaySources struct {
	sessions *events.Subscription[session.Change]
	userData *events.Subscription[playstate.UserDataChanged]
	library  *events.Subscription[library.UpdateInfo]
	refresh  *events.Subscription[library.RefreshProgress]
	activity *events.Subscription[activitylog.Entry]
}

// relay hands this server's events to Config.Relay until ctx ends.
func (server *Server) relay(ctx context.Context, sources relaySources) {
	defer close(server.relayDone)
	defer func() {
		background := context.Background()
		if sources.sessions != nil {
			_ = sources.sessions.Close(background)
		}
		if sources.userData != nil {
			_ = sources.userData.Close(background)
		}
		if sources.library != nil {
			_ = sources.library.Close(background)
		}
		if sources.refresh != nil {
			_ = sources.refresh.Close(background)
		}
		if sources.activity != nil {
			_ = sources.activity.Close(background)
		}
	}()
	sessions, userData, libraryChanges := channelOf(sources.sessions), channelOf(sources.userData), channelOf(sources.library)
	refresh, activity := channelOf(sources.refresh), channelOf(sources.activity)
	publish := server.config.Relay
	for sessions != nil || userData != nil || libraryChanges != nil || refresh != nil || activity != nil {
		select {
		case <-ctx.Done():
			return
		case change, open := <-sessions:
			if !open {
				sessions = nil
				continue
			}
			publish(PeerEvent{Kind: MessageSessions, Force: change.Force})
		case change, open := <-userData:
			if !open {
				userData = nil
				continue
			}
			publish(PeerEvent{Kind: MessageUserDataChanged, UserData: &change})
		case info, open := <-libraryChanges:
			if !open {
				libraryChanges = nil
				continue
			}
			publish(PeerEvent{Kind: MessageLibraryChanged, Library: &info})
		case progress, open := <-refresh:
			if !open {
				refresh = nil
				continue
			}
			publish(PeerEvent{Kind: MessageRefreshProgress, Refresh: &RefreshProgressInfo{ItemID: progress.ItemID, Progress: progress.Progress}})
		case _, open := <-activity:
			if !open {
				activity = nil
				continue
			}
			publish(PeerEvent{Kind: MessageActivityLogEntry})
		}
	}
}

// channelOf returns the events of subscription; nil, which never sends,
// when there is none.
func channelOf[T any](subscription *events.Subscription[T]) <-chan T {
	if subscription == nil {
		return nil
	}
	return subscription.Events()
}

// ReceivePeerEvents sends the events of a peer replica to this server's
// WebSockets, as the peer sends them to its own (#663).
func (server *Server) ReceivePeerEvents(received []PeerEvent) {
	if server == nil || server.peerEvents == nil {
		return
	}
	for _, event := range received {
		if event.Kind == MessageUserDataChanged {
			if event.UserData != nil {
				server.peerUserData.Publish(*event.UserData)
			}
			continue
		}
		server.peerEvents.Publish(event)
	}
}

// Register adds Jellyfin's authenticated socket endpoint to the route ledger.
func Register(rt *router.Router, server *Server) {
	rt.Handle(http.MethodGet, "/socket", router.Route{
		OperationID: "OpenWebSocket",
		Policy:      string(auth.PolicyDefaultAuthorization),
	}, func(w http.ResponseWriter, request *http.Request) {
		if server == nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		server.ServeHTTP(w, request)
	})
}

// ServeHTTP authenticates the upgraded connection against the caller's exact
// current Client+DeviceId session.
func (server *Server) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	identity, found := middleware.IdentityFromContext(request.Context())
	if !found || !identity.Authenticated {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	server.serveAuthenticated(w, request, identity)
}

func (server *Server) serveAuthenticated(w http.ResponseWriter, request *http.Request, identity auth.Identity) {
	if server.config.Sessions == nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	snapshot, found := server.config.Sessions.Session(identity.Client, identity.DeviceID)
	if !found || (!identity.APIKey && snapshot.UserID != identity.UserID) {
		http.Error(w, http.StatusText(http.StatusConflict), http.StatusConflict)
		return
	}

	handler := websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(socket *websocket.Conn) {
			connection := newConnection(server, socket, identity, snapshot.ID)
			if !server.add(connection) {
				_ = socket.Close()
				return
			}
			defer server.remove(connection)
			connection.run()
		},
	}
	handler.ServeHTTP(w, request)
}

// Close cancels every connection and waits for its writer and feed workers.
func (server *Server) Close(ctx context.Context) error {
	server.mu.Lock()
	if !server.closed {
		server.closed = true
	}
	connections := make([]*connection, 0, len(server.connections))
	for connection := range server.connections {
		connections = append(connections, connection)
	}
	server.mu.Unlock()

	for _, connection := range connections {
		connection.cancel()
		_ = connection.socket.Close()
	}
	for _, connection := range connections {
		select {
		case <-connection.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if server.relayCancel != nil {
		server.relayCancel()
		select {
		case <-server.relayDone:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := server.peerEvents.Close(ctx); err != nil {
			return err
		}
		return server.peerUserData.Close(ctx)
	}
	return nil
}

// CloseSession closes every open WebSocket controller of sessionID and waits
// until each connection stops. It ports the controller disposal of
// SessionInfo.DisposeAsync for an ended session.
func (server *Server) CloseSession(ctx context.Context, sessionID string) error {
	if server == nil {
		return nil
	}
	server.mu.Lock()
	connections := make([]*connection, 0, len(server.bySession[sessionID]))
	for connection := range server.bySession[sessionID] {
		connections = append(connections, connection)
	}
	server.mu.Unlock()

	for _, connection := range connections {
		connection.cancel()
		_ = connection.socket.Close()
	}
	for _, connection := range connections {
		select {
		case <-connection.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (server *Server) add(candidate *connection) bool {
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return false
	}
	server.connections[candidate] = struct{}{}
	connections := server.bySession[candidate.sessionID]
	if connections == nil {
		connections = make(map[*connection]struct{})
		server.bySession[candidate.sessionID] = connections
	}
	connections[candidate] = struct{}{}
	server.mu.Unlock()
	if err := server.config.Sessions.ControllerConnected(candidate.sessionID); err != nil {
		server.remove(candidate)
		return false
	}
	return true
}

func (server *Server) remove(candidate *connection) {
	server.mu.Lock()
	_, found := server.connections[candidate]
	if found {
		delete(server.connections, candidate)
		connections := server.bySession[candidate.sessionID]
		delete(connections, candidate)
		if len(connections) == 0 {
			delete(server.bySession, candidate.sessionID)
		}
	}
	server.mu.Unlock()
	if found {
		if server.config.Sessions.ControllerDisconnected(candidate.sessionID) {
			server.config.Sessions.ReportSessionEnded(candidate.sessionID)
		}
	}
}

// Send routes one command to the target session's most recently active open
// WebSocket controller. A session without an open controller is a successful
// no-op, matching SessionManager.SendMessageToSession.
func (server *Server) Send(ctx context.Context, sessionID string, messageType MessageType, data any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return session.ErrClosed
	}
	var (
		selected *connection
		latest   time.Time
	)
	for candidate := range server.bySession[sessionID] {
		candidate.lastMu.Lock()
		activity := candidate.lastKeepAlive
		candidate.lastMu.Unlock()
		if candidate.ctx.Err() == nil && (selected == nil || activity.After(latest)) {
			selected = candidate
			latest = activity
		}
	}
	server.mu.Unlock()
	if selected == nil {
		return nil
	}
	if !selected.send(messageType, data) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return session.ErrClosed
	}
	return nil
}

type connection struct {
	server    *Server
	socket    *websocket.Conn
	identity  auth.Identity
	sessionID string
	factory   envelopeFactory

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	out    chan Envelope

	lastMu        sync.Mutex
	lastKeepAlive time.Time
	feeds         map[MessageType]*periodicFeed
	wg            sync.WaitGroup
}

func newConnection(server *Server, socket *websocket.Conn, identity auth.Identity, sessionID string) *connection {
	ctx, cancel := context.WithCancel(socket.Request().Context())
	return &connection{
		server: server, socket: socket, identity: identity, sessionID: sessionID,
		factory: envelopeFactory{random: server.config.Random},
		ctx:     ctx, cancel: cancel, done: make(chan struct{}), out: make(chan Envelope, connectionBuffer),
		lastKeepAlive: server.config.Clock.Now(),
		feeds:         make(map[MessageType]*periodicFeed),
	}
}

func (connection *connection) run() {
	defer close(connection.done)
	connection.wg.Add(2)
	go connection.writeLoop()
	go connection.closeOnCancel()
	connection.startFeeds()
	connection.startKeepAlive()

	if !connection.send(MessageForceKeepAlive, int(keepAliveTimeout/time.Second)) {
		connection.cancel()
	}
	for connection.ctx.Err() == nil {
		var message InboundEnvelope
		if err := websocket.JSON.Receive(connection.socket, &message); err != nil {
			break
		}
		connection.process(message)
	}
	connection.cancel()
	_ = connection.socket.Close()
	for _, feed := range connection.feeds {
		feed.wait()
	}
	connection.wg.Wait()
}

func (connection *connection) closeOnCancel() {
	defer connection.wg.Done()
	<-connection.ctx.Done()
	_ = connection.socket.Close()
}

func (connection *connection) writeLoop() {
	defer connection.wg.Done()
	for {
		select {
		case <-connection.ctx.Done():
			return
		case message := <-connection.out:
			if err := websocket.JSON.Send(connection.socket, message); err != nil {
				connection.cancel()
				return
			}
		}
	}
}

func (connection *connection) send(messageType MessageType, data any) bool {
	message, err := connection.factory.new(messageType, data)
	if err != nil {
		connection.cancel()
		return false
	}
	select {
	case connection.out <- message:
		return true
	case <-connection.ctx.Done():
		return false
	default:
		connection.cancel()
		_ = connection.socket.Close()
		return false
	}
}

func (connection *connection) process(message InboundEnvelope) {
	if message.MessageType == MessageKeepAlive {
		connection.lastMu.Lock()
		connection.lastKeepAlive = connection.server.config.Clock.Now()
		connection.lastMu.Unlock()
		connection.send(MessageKeepAlive, nil)
		return
	}
	feedType, start, found := periodicMessage(message.MessageType)
	if !found {
		return
	}
	feed := connection.feeds[feedType]
	if feed == nil {
		return
	}
	if !start {
		feed.stop()
		return
	}
	if feedType == MessageActivityLogEntry && !connection.identity.APIKey && !connection.identity.Administrator {
		return
	}
	var raw string
	if err := json.Unmarshal(message.Data, &raw); err != nil {
		return
	}
	interval, err := parsePeriodicInterval(raw)
	if err != nil {
		return
	}
	feed.start(interval)
}

func periodicMessage(messageType MessageType) (MessageType, bool, bool) {
	switch messageType {
	case MessageSessionsStart:
		return MessageSessions, true, true
	case MessageSessionsStop:
		return MessageSessions, false, true
	case MessageActivityLogEntryStart:
		return MessageActivityLogEntry, true, true
	case MessageActivityLogEntryStop:
		return MessageActivityLogEntry, false, true
	case MessageScheduledTasksInfoStart:
		return MessageScheduledTasksInfo, true, true
	case MessageScheduledTasksInfoStop:
		return MessageScheduledTasksInfo, false, true
	default:
		return "", false, false
	}
}

func (connection *connection) startFeeds() {
	config := connection.server.config
	if config.Sessions != nil {
		connection.feeds[MessageSessions] = newPeriodicFeed(connection.ctx, config.Clock, MessageSessions, connection.loadSessions, connection.send, connection.cancel)
		connection.forwardSessionChanges()
	}
	if config.Activity != nil {
		connection.feeds[MessageActivityLogEntry] = newPeriodicFeed(connection.ctx, config.Clock, MessageActivityLogEntry, func(context.Context) (any, error) {
			return []activitylog.Entry{}, nil
		}, connection.send, connection.cancel)
		connection.forwardActivityChanges()
	}
	if config.Tasks != nil {
		connection.feeds[MessageScheduledTasksInfo] = newPeriodicFeed(connection.ctx, config.Clock, MessageScheduledTasksInfo, func(context.Context) (any, error) {
			return config.Tasks.Snapshot(), nil
		}, connection.send, connection.cancel)
		connection.forwardTaskChanges()
	}
	connection.forwardLibraryChanges()
	connection.forwardUserDataChanges()
	connection.forwardRefreshProgress()
	connection.forwardPeerEvents()
}

func (connection *connection) loadSessions(ctx context.Context) (any, error) {
	query := SessionsQuery{UserID: connection.identity.UserID, All: connection.identity.APIKey || connection.identity.Administrator}
	result := connection.server.LocalSessions(ctx, query)
	if peers := connection.server.config.PeerSessions; peers != nil {
		result = sessiondto.Merge(result, peers(ctx, query))
	}
	return result, nil
}

// LocalSessions returns the Sessions feed of this replica's own sessions.
func (server *Server) LocalSessions(ctx context.Context, query SessionsQuery) []dto.SessionInfoDto {
	config := server.config
	if config.Sessions == nil {
		return []dto.SessionInfoDto{}
	}
	snapshots := config.Sessions.Sessions(session.Query{})
	result := make([]dto.SessionInfoDto, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if !query.All && !containsUser(snapshot, query.UserID) {
			continue
		}
		userName := ""
		if config.Users != nil {
			if value, err := config.Users.GetDTO(ctx, snapshot.UserID); err == nil {
				userName = value.Name
			}
		}
		if config.SessionProjector == nil {
			result = append(result, sessiondto.ProjectBase(snapshot, userName, ""))
		} else {
			result = append(result, config.SessionProjector.Project(ctx, snapshot, userName))
		}
	}
	return result
}

func containsUser(snapshot session.Snapshot, userID dto.UUID) bool {
	if snapshot.UserID == userID {
		return true
	}
	for _, additional := range snapshot.AdditionalUserIDs {
		if additional == userID {
			return true
		}
	}
	return false
}

func (connection *connection) forwardSessionChanges() {
	subscription := connection.server.config.Sessions.Subscribe(events.SubscriptionOptions{Buffer: 8})
	forward(connection, subscription.Events(), func(change session.Change) {
		connection.feeds[MessageSessions].changed(change.Force)
	}, func() { _ = subscription.Close(context.Background()) })
}

func (connection *connection) forwardActivityChanges() {
	subscription := connection.server.config.Activity.Subscribe(events.SubscriptionOptions{Buffer: 8})
	forward(connection, subscription.Events(), func(activitylog.Entry) {
		connection.feeds[MessageActivityLogEntry].changed(true)
	}, func() { _ = subscription.Close(context.Background()) })
}

func (connection *connection) forwardTaskChanges() {
	subscription := connection.server.config.Tasks.Subscribe(events.SubscriptionOptions{Buffer: 8})
	forward(connection, subscription.Events(), func([]tasks.Info) {
		connection.feeds[MessageScheduledTasksInfo].changed(true)
	}, func() { _ = subscription.Close(context.Background()) })
}

func (connection *connection) forwardLibraryChanges() {
	if connection.server.config.LibraryChanges == nil {
		return
	}
	subscription := connection.server.config.LibraryChanges.Subscribe(4)
	forward(connection, subscription.Events(), func(info library.UpdateInfo) {
		connection.send(MessageLibraryChanged, info)
	}, func() { _ = subscription.Close(context.Background()) })
}

func (connection *connection) forwardUserDataChanges() {
	if connection.identity.UserID.IsNil() || connection.server.config.Items == nil || connection.server.config.ItemBuilder == nil {
		return
	}
	for _, bus := range []*events.Bus[playstate.UserDataChanged]{connection.server.config.UserDataChanges, connection.server.peerUserData} {
		if bus == nil {
			continue
		}
		subscription := playstate.SubscribeUserDataChanges(bus, connection.identity.UserID, 4)
		forward(connection, subscription.Events(), func(change playstate.UserDataChanged) {
			info, err := connection.projectUserData(connection.ctx, change)
			if err != nil {
				connection.cancel()
				return
			}
			connection.send(MessageUserDataChanged, info)
		}, func() { _ = subscription.Close(context.Background()) })
	}
}

// forwardPeerEvents sends the peer replicas' events other than user data,
// as forwardLibraryChanges and the feeds send this replica's own.
func (connection *connection) forwardPeerEvents() {
	if connection.server.peerEvents == nil {
		return
	}
	administrator := connection.identity.APIKey || connection.identity.Administrator
	subscription := connection.server.peerEvents.Subscribe(events.SubscriptionOptions{Buffer: 32})
	forward(connection, subscription.Events(), func(event PeerEvent) {
		switch event.Kind {
		case MessageSessions:
			if feed := connection.feeds[MessageSessions]; feed != nil {
				feed.changed(event.Force)
			}
		case MessageActivityLogEntry:
			if feed := connection.feeds[MessageActivityLogEntry]; feed != nil {
				feed.changed(true)
			}
		case MessageLibraryChanged:
			if event.Library != nil && !event.Library.Empty() {
				connection.send(MessageLibraryChanged, *event.Library)
			}
		case MessageRefreshProgress:
			if event.Refresh != nil && administrator {
				connection.send(MessageRefreshProgress, *event.Refresh)
			}
		}
	}, func() { _ = subscription.Close(context.Background()) })
}

func (connection *connection) projectUserData(ctx context.Context, change playstate.UserDataChanged) (UserDataChangeInfo, error) {
	config := connection.server.config
	seen := make(map[dto.UUID]struct{}, len(change.ItemIDs)*2)
	values := make([]item.Item, 0, len(change.ItemIDs)*2)
	ids := append([]dto.UUID(nil), change.ItemIDs...)
	if len(ids) == 0 && !change.ItemID.IsNil() {
		ids = append(ids, change.ItemID)
	}
	for index := 0; index < len(ids); index++ {
		id := ids[index]
		if id.IsNil() {
			continue
		}
		if _, found := seen[id]; found {
			continue
		}
		value, err := config.Items.Get(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return UserDataChangeInfo{}, err
		}
		seen[id] = struct{}{}
		values = append(values, value)
		if value.ParentID != nil {
			ids = append(ids, *value.ParentID)
		}
	}
	projected, err := config.ItemBuilder.Build(ctx, values, store.ItemQueryDTOOptions{EnableUserData: true}, &change.UserID)
	if err != nil {
		return UserDataChangeInfo{}, err
	}
	result := UserDataChangeInfo{UserID: change.UserID, UserDataList: make([]dto.UserItemDataDto, 0, len(projected.Items))}
	for _, value := range projected.Items {
		if value.UserData != nil {
			result.UserDataList = append(result.UserDataList, *value.UserData)
		}
	}
	return result, nil
}

func (connection *connection) forwardRefreshProgress() {
	if (!connection.identity.APIKey && !connection.identity.Administrator) || connection.server.config.RefreshProgress == nil {
		return
	}
	subscription := connection.server.config.RefreshProgress.Subscribe(events.SubscriptionOptions{Buffer: 16})
	forward(connection, subscription.Events(), func(progress library.RefreshProgress) {
		connection.send(MessageRefreshProgress, RefreshProgressInfo{ItemID: progress.ItemID, Progress: progress.Progress})
	}, func() { _ = subscription.Close(context.Background()) })
}

func forward[T any](connection *connection, values <-chan T, receive func(T), close func()) {
	connection.wg.Add(1)
	go func() {
		defer connection.wg.Done()
		defer close()
		for {
			select {
			case <-connection.ctx.Done():
				return
			case value, open := <-values:
				if !open {
					if connection.ctx.Err() == nil {
						connection.cancel()
					}
					return
				}
				receive(value)
			}
		}
	}()
}

func (connection *connection) startKeepAlive() {
	timer := connection.server.config.Clock.NewTimer(keepAliveSweep)
	connection.wg.Add(1)
	go func() {
		defer connection.wg.Done()
		defer timer.Stop()
		for {
			select {
			case <-connection.ctx.Done():
				return
			case <-timer.C():
				connection.lastMu.Lock()
				elapsed := connection.server.config.Clock.Now().Sub(connection.lastKeepAlive)
				connection.lastMu.Unlock()
				switch {
				case elapsed >= keepAliveTimeout:
					connection.cancel()
					return
				case elapsed > keepAliveForce:
					// Re-arm before the send: a peer that sees the
					// message must also see the next sweep scheduled.
					timer.Reset(keepAliveSweep)
					if !connection.send(MessageForceKeepAlive, int(keepAliveTimeout/time.Second)) {
						return
					}
					continue
				}
				timer.Reset(keepAliveSweep)
			}
		}
	}()
}
