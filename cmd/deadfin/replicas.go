// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/replica"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/ws"
)

// Replica mode (#652, docs/spikes/transcode-workers.md §3.6.3): two or more
// server processes on one PostgreSQL store. envReplicaURL is where the other
// replicas reach this process, such as http://$(POD_IP):8096.
const envReplicaURL = "DEADFIN_REPLICA_URL"

// replicaDrainTimeout bounds the wait for the routed requests in flight at
// SIGTERM.
const replicaDrainTimeout = 5 * time.Second

// replicaEnvironment reads envReplicaURL. "" is no replica mode.
func replicaEnvironment(getenv func(string) string) (string, error) {
	value := strings.TrimSpace(getenv(envReplicaURL))
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s: %q is not an http or https base URL", envReplicaURL, value)
	}
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

// replicaMode is the replica state of one server process.
type replicaMode struct {
	registry *replica.Registry
	signer   *replica.Signer
	store    *store.ReplicaStore
	router   *replica.Router
	// cluster carries the server events and the session lists between
	// the replicas (#663).
	cluster *replica.Cluster
}

// startReplicaMode registers this process as a replica. Replica mode needs
// PostgreSQL; sqliteAllowed lets a test run it on one SQLite file.
func startReplicaMode(ctx context.Context, c clock.Clock, stores store.Stores, postgres, sqliteAllowed bool, replicaURL string, logger *slog.Logger) (*replicaMode, error) {
	if !postgres && !sqliteAllowed {
		return nil, fmt.Errorf("%s needs a PostgreSQL database: replicas share one store", envReplicaURL)
	}
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	replicas := store.NewReplicaStore(stores)
	secret, err := replicas.ReplicaSecret(ctx, func() ([]byte, error) {
		key := make([]byte, 32)
		_, err := rand.Read(key)
		return key, err
	})
	if err != nil {
		return nil, err
	}
	signer, err := replica.NewSigner(secret, c)
	if err != nil {
		return nil, err
	}
	if err := replicas.DeleteExpiredReplicas(ctx, c.Now()); err != nil {
		logger.Warn("delete expired replica rows", "error", err.Error())
	}
	registry, err := replica.StartRegistry(ctx, replica.RegistryConfig{
		Store: replicas, Clock: c, ID: hex.EncodeToString(id), URL: replicaURL, Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("register server replica: %w", err)
	}
	cluster, err := replica.NewCluster(replica.ClusterConfig{Registry: registry, Signer: signer, Logger: logger})
	if err != nil {
		registry.Close()
		return nil, err
	}
	logger.Info("server replica mode", "replica", registry.ID(), "url", replicaURL, "peers", registry.HasPeer())
	return &replicaMode{registry: registry, signer: signer, store: replicas, cluster: cluster}, nil
}

// id returns the replica ID; "" without replica mode.
func (mode *replicaMode) id() string {
	if mode == nil {
		return ""
	}
	return mode.registry.ID()
}

// sharesTranscodes reports a live peer at startup: the peer may share the
// transcode directory, so the startup wipe does not run.
func (mode *replicaMode) sharesTranscodes() bool {
	return mode != nil && mode.registry.HasPeer()
}

// newRouter builds the routing wrapper; without replica mode it changes
// nothing.
func (mode *replicaMode) newRouter(c clock.Clock, sessions replica.Sessions, grants replica.Grants, resolver replica.Identities, legacy func() bool, flush func(context.Context) error, claimed []string) (router.RouteWrapper, error) {
	if mode == nil {
		return func(_ router.Route, next http.Handler) http.Handler { return next }, nil
	}
	routing, err := replica.NewRouter(replica.RouterConfig{
		Registry: mode.registry, Signer: mode.signer, Store: mode.store, Sessions: sessions, Grants: grants,
		Resolver: resolver, LegacyAuthorization: legacy, Clock: c, Flush: flush, Logger: slog.Default(),
	})
	if err != nil {
		return nil, err
	}
	routing.ClaimedSessions(claimed)
	mode.router = routing
	return routing.Wrap, nil
}

// close stops the heartbeat, if the drain did not, stops the event sender
// and drops the idle connections to the peers.
func (mode *replicaMode) close() {
	mode.registry.Close()
	mode.cluster.Close()
	if mode.router != nil {
		mode.router.Close()
	}
}

// relay hands the WebSocket server's events to the peers (#663); nil
// without replica mode.
func (mode *replicaMode) relay() func(ws.PeerEvent) {
	if mode == nil {
		return nil
	}
	return func(event ws.PeerEvent) { mode.cluster.Publish(event) }
}

// peerSessions reads the peers' parts of the WebSocket Sessions feed; nil
// without replica mode.
func (mode *replicaMode) peerSessions() func(context.Context, ws.SessionsQuery) []dto.SessionInfoDto {
	if mode == nil {
		return nil
	}
	return func(ctx context.Context, query ws.SessionsQuery) []dto.SessionInfoDto {
		return decodeSessionLists(mode.cluster.PeerSessions(ctx, query))
	}
}

// sessionPeers reads the peers' parts of GET /Sessions; nil without replica
// mode.
func (mode *replicaMode) sessionPeers() handlers.SessionPeers {
	if mode == nil {
		return nil
	}
	return clusterSessions{cluster: mode.cluster}
}

// sessionOwners sends a command for another client's session to the replica
// that holds it; nil without replica mode.
func (mode *replicaMode) sessionOwners() handlers.SessionOwners {
	if mode == nil || mode.router == nil {
		return nil
	}
	return mode.router
}

type clusterSessions struct {
	cluster *replica.Cluster
}

func (sessions clusterSessions) PeerSessions(request *http.Request) []dto.SessionInfoDto {
	return decodeSessionLists(sessions.cluster.Gather(request))
}

// decodeSessionLists joins the session lists that the peers answered. A
// list that does not decode is left out.
func decodeSessionLists(bodies [][]byte) []dto.SessionInfoDto {
	var result []dto.SessionInfoDto
	for _, body := range bodies {
		var list []dto.SessionInfoDto
		if err := json.Unmarshal(body, &list); err != nil {
			slog.Warn("decode a peer's session list", "error", err.Error())
			continue
		}
		result = append(result, list...)
	}
	return result
}

// claimJob is the hub's job claim: this replica takes a job record whose
// owner is gone. It is nil without replica mode.
func (mode *replicaMode) claimJob(c clock.Clock) func(context.Context, string) (bool, error) {
	if mode == nil {
		return nil
	}
	return func(ctx context.Context, id string) (bool, error) {
		return mode.store.ClaimTranscodeJob(ctx, id, mode.registry.ID(), c.Now())
	}
}

// AcceptForwards wraps the server's whole handler: in replica mode a request
// that a peer forwarded gets the client's transport peer and TLS flag back,
// and the peer paths (#663) take the peers' events and session queries.
func (app *application) AcceptForwards(next http.Handler) http.Handler {
	if app == nil || app.replicas == nil {
		return next
	}
	webSockets := app.webSockets
	return app.replicas.signer.Accept(replica.PeerHandler(replica.PeerEndpoints{
		Events: func(_ context.Context, raw []json.RawMessage) {
			received := make([]ws.PeerEvent, 0, len(raw))
			for _, encoded := range raw {
				var event ws.PeerEvent
				if err := json.Unmarshal(encoded, &event); err == nil {
					received = append(received, event)
				}
			}
			webSockets.ReceivePeerEvents(received)
		},
		Sessions: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var query ws.SessionsQuery
			if err := json.Unmarshal(raw, &query); err != nil {
				return nil, err
			}
			if webSockets == nil {
				return []dto.SessionInfoDto{}, nil
			}
			return webSockets.LocalSessions(ctx, query), nil
		},
	}, next))
}

// DrainReplica hands this replica's work to its peers (#652). It holds new
// routed requests, waits for the routed requests in flight, flushes the play
// sessions, releases the remote transcode jobs to their workers as at a
// restart (#618), clears the owner of this replica's rows, and from then on
// sends routed requests to a peer. Run it at SIGTERM before the HTTP server
// stops; Close runs it too. It runs once.
func (app *application) DrainReplica(ctx context.Context) error {
	if app == nil {
		return nil
	}
	app.replicaDrainMu.Lock()
	defer app.replicaDrainMu.Unlock()
	if app.replicas == nil || app.replicaDrained {
		return nil
	}
	mode := app.replicas
	mode.router.BeginDrain()
	var drainErr error
	if err := mode.registry.MarkDraining(ctx); err != nil {
		slog.Warn("mark the replica draining", "error", err.Error())
	}
	waitCtx, cancel := context.WithTimeout(ctx, replicaDrainTimeout)
	if err := mode.router.WaitIdle(waitCtx); err != nil {
		slog.Warn("replica drain: routed requests still in flight", "error", err.Error())
	}
	cancel()
	if app.playSessions != nil {
		if err := app.playSessions.Close(ctx); err != nil {
			drainErr = errors.Join(drainErr, fmt.Errorf("persist play sessions: %w", err))
		}
		app.playSessions = nil
	}
	if app.transcodes != nil {
		if err := app.transcodes.Close(ctx); err != nil {
			drainErr = errors.Join(drainErr, fmt.Errorf("release transcodes: %w", err))
		} else {
			app.transcodes = nil
		}
	}
	if app.transcodes == nil && app.transcodeWorkers != nil {
		if err := app.transcodeWorkers.Close(ctx); err != nil {
			drainErr = errors.Join(drainErr, fmt.Errorf("close the transcode worker hub: %w", err))
		} else {
			app.transcodeWorkers = nil
		}
	}
	if err := mode.registry.Release(ctx); err != nil {
		drainErr = errors.Join(drainErr, fmt.Errorf("release the replica rows: %w", err))
	}
	mode.router.EndDrain()
	app.replicaDrained = true
	if drainErr != nil {
		slog.Warn("replica drain", "error", drainErr.Error())
	} else {
		slog.Info("replica drained; its sessions and jobs are free for the peers", "replica", mode.registry.ID())
	}
	return drainErr
}
