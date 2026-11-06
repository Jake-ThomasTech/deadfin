// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/deadfin-org/deadfin/internal/auth/playgrant"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/store"
)

// envPlaySessionRestore turns on the play session reload (#620) for a
// server with no replicas and no stream edge. Those two always turn it on.
const envPlaySessionRestore = "DEADFIN_PLAY_SESSION_RESTORE"

// playSessionRestore reports whether play sessions and their grants persist
// and reload after a restart. It is off by default: sessions live in memory
// only, as upstream keeps them (D-06). Server replicas (#652) and the stream
// edge (#619) need the reload, so they turn it on.
func playSessionRestore(getenv func(string) string, replicas *replicaMode, streamEdge serverEdge) (bool, error) {
	if replicas != nil || streamEdge.signer != nil {
		return true, nil
	}
	value := strings.TrimSpace(getenv(envPlaySessionRestore))
	if value == "" {
		return false, nil
	}
	restore, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s: %q is not a boolean", envPlaySessionRestore, value)
	}
	return restore, nil
}

// playSessionUsers reports whether a user still exists.
type playSessionUsers interface {
	Get(context.Context, dto.UUID) (store.User, error)
}

// restorePlaySessions restores the play sessions and PlaySessionId grants
// of the previous server process and starts their write-behind (#620). A
// store failure here does not stop the startup: the server then starts with
// no sessions, as before #620. A replica (#652) restores only the rows it
// claims: rows with no owner or a dead owner; it writes only its own rows.
// It returns the IDs of the restored sessions.
func restorePlaySessions(ctx context.Context, c clock.Clock, stores store.Stores, users playSessionUsers, sessions *session.Manager, grants *playgrant.Store, replicas *replicaMode, logger *slog.Logger) (*session.Persister, []string) {
	playSessions := store.NewPlaySessionStore(stores)
	now := c.Now()
	readSessions := func() ([]store.PlaySessionRecord, error) {
		return playSessions.PlaySessions(ctx, now.Add(-session.ReloadWindow))
	}
	readGrants := func() ([]store.PlayGrantRecord, error) {
		return playSessions.PlayGrants(ctx, now.Add(-playgrant.IdleTimeout))
	}
	if replicas != nil {
		me := replicas.id()
		playSessions = playSessions.WithOwner(me)
		readSessions = func() ([]store.PlaySessionRecord, error) {
			return replicas.store.ClaimPlaySessions(ctx, me, now.Add(-session.ReloadWindow), now)
		}
		readGrants = func() ([]store.PlayGrantRecord, error) {
			return replicas.store.ClaimPlayGrants(ctx, me, now.Add(-playgrant.IdleTimeout), now)
		}
	}
	sessionCutoff, grantCutoff := now.Add(-session.ReloadWindow), now.Add(-playgrant.IdleTimeout)
	if err := playSessions.DeleteStalePlaySessions(ctx, sessionCutoff, grantCutoff); err != nil {
		logger.Warn("delete stale play sessions", "error", err.Error())
	}
	known := make(map[dto.UUID]bool)
	exists := func(id dto.UUID) bool {
		if present, found := known[id]; found {
			return present
		}
		_, err := users.Get(ctx, id)
		known[id] = !store.IsNotFound(err)
		return known[id]
	}
	restoredSessions, restoredGrants := 0, 0
	var restoredIDs []string
	if records, err := readSessions(); err != nil {
		logger.Warn("read play sessions; starting without them", "error", err.Error())
	} else {
		kept := records[:0]
		for _, record := range records {
			if exists(record.UserID) {
				kept = append(kept, record)
			}
		}
		restoredSessions = sessions.Restore(kept)
		for _, record := range kept {
			if _, found := sessions.SessionByID(record.ID); found {
				restoredIDs = append(restoredIDs, record.ID)
			}
		}
	}
	if records, err := readGrants(); err != nil {
		logger.Warn("read play session grants; starting without them", "error", err.Error())
	} else {
		kept := records[:0]
		for _, record := range records {
			if exists(record.UserID) {
				kept = append(kept, record)
			}
		}
		grants.Restore(kept)
		restoredGrants = len(kept)
	}
	if restoredSessions > 0 || restoredGrants > 0 {
		logger.Info("restored play sessions after a restart", "sessions", restoredSessions, "grants", restoredGrants)
	}
	return session.StartPersister(session.PersisterConfig{
		Clock: c, Store: playSessions, Sessions: sessions, Grants: grants, Logger: logger,
	}), restoredIDs
}
