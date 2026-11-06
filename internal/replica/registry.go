// SPDX-License-Identifier: GPL-2.0-only

// Package replica runs two or more server processes on one PostgreSQL store
// (#652). Each process is a replica with a row and a lease. A play session,
// a PlaySessionId grant and a remote transcode job have an owner replica; a
// replica forwards a request whose owner is a live peer, and claims a row
// whose owner is gone. At SIGTERM a replica gives up its rows, so that a
// rolling update does not interrupt playback.
package replica

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/store"
)

const (
	// DefaultLease is how long a replica row stays live after a heartbeat.
	DefaultLease = 15 * time.Second
	// DefaultHeartbeat is the time between two heartbeats.
	DefaultHeartbeat = 5 * time.Second
)

// RegistryStore keeps the replica rows.
type RegistryStore interface {
	RenewReplica(ctx context.Context, replica store.Replica) error
	LiveReplicas(ctx context.Context, now time.Time) ([]store.Replica, error)
	ReleaseReplica(ctx context.Context, id string) error
}

// RegistryConfig configures a Registry.
type RegistryConfig struct {
	Store RegistryStore
	Clock clock.Clock
	// ID is this replica's random ID; URL is where the peers reach it.
	ID  string
	URL string
	// Lease and Heartbeat default to DefaultLease and DefaultHeartbeat.
	Lease     time.Duration
	Heartbeat time.Duration
	Logger    *slog.Logger
}

// Registry keeps this replica's row live and caches the live peers. It owns
// one heartbeat goroutine; Release or Close stops it.
type Registry struct {
	config RegistryConfig
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	peers    []store.Replica
	draining bool
	stopped  bool
}

// StartRegistry stores this replica's row, reads the peers and starts the
// heartbeat.
func StartRegistry(ctx context.Context, config RegistryConfig) (*Registry, error) {
	if config.Store == nil || config.Clock == nil || config.ID == "" || config.URL == "" {
		return nil, errors.New("replica: registry needs a store, a clock, an ID and a URL")
	}
	if config.Lease <= 0 {
		config.Lease = DefaultLease
	}
	if config.Heartbeat <= 0 {
		config.Heartbeat = DefaultHeartbeat
	}
	if config.Heartbeat >= config.Lease {
		return nil, errors.New("replica: the heartbeat must be shorter than the lease")
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	registry := &Registry{config: config, done: make(chan struct{})}
	if err := registry.beat(ctx); err != nil {
		return nil, err
	}
	loop, cancel := context.WithCancel(context.Background())
	registry.cancel = cancel
	go registry.run(loop)
	return registry, nil
}

// ID returns this replica's ID.
func (registry *Registry) ID() string { return registry.config.ID }

// URL returns where the peers reach this replica.
func (registry *Registry) URL() string { return registry.config.URL }

func (registry *Registry) run(ctx context.Context) {
	defer close(registry.done)
	timer := registry.config.Clock.NewTimer(registry.config.Heartbeat)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
		}
		// Arm the next beat first: the beat's store write is what a
		// test or a peer observes.
		timer.Reset(registry.config.Heartbeat)
		if err := registry.beat(ctx); err != nil && ctx.Err() == nil {
			registry.config.Logger.Warn("replica heartbeat", "error", err.Error())
		}
	}
}

// beat renews this replica's row and reads the live peers.
func (registry *Registry) beat(ctx context.Context) error {
	registry.mu.Lock()
	draining := registry.draining
	registry.mu.Unlock()
	now := registry.config.Clock.Now()
	if err := registry.config.Store.RenewReplica(ctx, store.Replica{
		ID: registry.config.ID, URL: registry.config.URL, Draining: draining, LeaseExpires: now.Add(registry.config.Lease),
	}); err != nil {
		return err
	}
	return registry.Refresh(ctx)
}

// Refresh reads the live peers now.
func (registry *Registry) Refresh(ctx context.Context) error {
	live, err := registry.config.Store.LiveReplicas(ctx, registry.config.Clock.Now())
	if err != nil {
		return err
	}
	peers := live[:0]
	for _, replica := range live {
		if replica.ID != registry.config.ID {
			peers = append(peers, replica)
		}
	}
	registry.mu.Lock()
	registry.peers = peers
	registry.mu.Unlock()
	return nil
}

// Peer returns the live peer id.
func (registry *Registry) Peer(id string) (store.Replica, bool) {
	now := registry.config.Clock.Now()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, peer := range registry.peers {
		if peer.ID == id && peer.LeaseExpires.After(now) {
			return peer, true
		}
	}
	return store.Replica{}, false
}

// Peers returns the live peers.
func (registry *Registry) Peers() []store.Replica {
	now := registry.config.Clock.Now()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	live := make([]store.Replica, 0, len(registry.peers))
	for _, peer := range registry.peers {
		if peer.LeaseExpires.After(now) {
			live = append(live, peer)
		}
	}
	return live
}

// AnyPeer returns a live peer that is not draining.
func (registry *Registry) AnyPeer() (store.Replica, bool) {
	now := registry.config.Clock.Now()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, peer := range registry.peers {
		if !peer.Draining && peer.LeaseExpires.After(now) {
			return peer, true
		}
	}
	return store.Replica{}, false
}

// HasPeer reports whether a live peer exists.
func (registry *Registry) HasPeer() bool {
	now := registry.config.Clock.Now()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, peer := range registry.peers {
		if peer.LeaseExpires.After(now) {
			return true
		}
	}
	return false
}

// MarkDraining stores this replica as draining, so that the peers send it
// no new work.
func (registry *Registry) MarkDraining(ctx context.Context) error {
	registry.mu.Lock()
	registry.draining = true
	registry.mu.Unlock()
	return registry.beat(ctx)
}

// Release stops the heartbeat, clears the owner of this replica's rows and
// deletes its row. The peers can then claim the rows at once.
func (registry *Registry) Release(ctx context.Context) error {
	registry.stop()
	return registry.config.Store.ReleaseReplica(ctx, registry.config.ID)
}

// Close stops the heartbeat and keeps the row; its lease ends on its own.
func (registry *Registry) Close() {
	registry.stop()
}

func (registry *Registry) stop() {
	registry.mu.Lock()
	stopped := registry.stopped
	registry.stopped = true
	registry.mu.Unlock()
	if stopped {
		return
	}
	registry.cancel()
	<-registry.done
}
