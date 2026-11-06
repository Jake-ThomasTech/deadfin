// SPDX-License-Identifier: GPL-2.0-only

package storetest_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

// TestReplicaStore defends the replica rows and the row owners (#652) on
// SQLite and, when DEADFIN_TEST_POSTGRES_DSN is set, PostgreSQL.
func TestReplicaStore(t *testing.T) {
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	t.Run("sqlite", func(t *testing.T) { verifyReplicaStore(t, stores) })
	if dsn := os.Getenv("DEADFIN_TEST_POSTGRES_DSN"); dsn != "" {
		pg := openPostgresFromDSN(t, dsn)
		t.Run("postgres", func(t *testing.T) { verifyReplicaStore(t, pg) })
	}
}

func verifyReplicaStore(t *testing.T, stores store.Stores) {
	ctx := context.Background()
	replicas := store.NewReplicaStore(stores)
	now := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)
	user := dto.UUID{0xa1, 0x02}
	item := dto.UUID{0xb1, 0x03}

	// Replica rows: a lease that ends at now is not live.
	for _, replica := range []store.Replica{
		{ID: "a", URL: "http://a:8096", LeaseExpires: now.Add(15 * time.Second)},
		{ID: "b", URL: "http://b:8096", LeaseExpires: now.Add(15 * time.Second)},
		{ID: "dead", URL: "http://dead:8096", LeaseExpires: now},
	} {
		if err := replicas.RenewReplica(ctx, replica); err != nil {
			t.Fatalf("RenewReplica %s: %v", replica.ID, err)
		}
	}
	if err := replicas.RenewReplica(ctx, store.Replica{ID: "b", URL: "http://b2:8096", Draining: true, LeaseExpires: now.Add(20 * time.Second)}); err != nil {
		t.Fatalf("renew b: %v", err)
	}
	live, err := replicas.LiveReplicas(ctx, now)
	if err != nil || len(live) != 2 || live[0].ID != "a" || live[1].ID != "b" || live[1].URL != "http://b2:8096" || !live[1].Draining || live[0].Draining ||
		!live[1].LeaseExpires.Equal(now.Add(20*time.Second)) {
		t.Fatalf("LiveReplicas = %+v, %v", live, err)
	}

	// The first secret wins; a later generator is not called.
	first, err := replicas.ReplicaSecret(ctx, func() ([]byte, error) { return []byte("first-secret"), nil })
	if err != nil || string(first) != "first-secret" {
		t.Fatalf("ReplicaSecret = %q, %v", first, err)
	}
	again, err := replicas.ReplicaSecret(ctx, func() ([]byte, error) {
		t.Fatal("generator called for a stored secret")
		return nil, nil
	})
	if err != nil || !bytes.Equal(again, first) {
		t.Fatalf("ReplicaSecret again = %q, %v", again, err)
	}

	// Replica a writes its rows through an owned store.
	ownedA := store.NewPlaySessionStore(stores).WithOwner("a")
	if err := ownedA.SavePlaySessions(ctx, store.PlaySessionChanges{
		Sessions: []store.PlaySessionRecord{{ID: "session-a", UserID: user, LastActivity: now, State: []byte(`{"v":1,"a":1}`)}},
		Grants:   []store.PlayGrantRecord{{PlaySessionID: "grant-a", UserID: user, ItemID: item, MediaSourceIDs: []string{"b103"}, LastUsed: now}},
	}); err != nil {
		t.Fatalf("save as a: %v", err)
	}
	if owner, found, err := replicas.PlaySessionOwner(ctx, "session-a"); err != nil || !found || owner != "a" {
		t.Fatalf("PlaySessionOwner = %q, %v, %v", owner, found, err)
	}
	if owner, found, err := replicas.PlayGrantOwner(ctx, "grant-a"); err != nil || !found || owner != "a" {
		t.Fatalf("PlayGrantOwner = %q, %v, %v", owner, found, err)
	}
	if _, found, err := replicas.PlaySessionOwner(ctx, "missing"); err != nil || found {
		t.Fatalf("missing owner found = %v, %v", found, err)
	}

	// Replica b cannot write, delete or claim the rows of live replica a.
	ownedB := store.NewPlaySessionStore(stores).WithOwner("b")
	if err := ownedB.SavePlaySessions(ctx, store.PlaySessionChanges{
		Sessions:      []store.PlaySessionRecord{{ID: "session-a", UserID: user, LastActivity: now.Add(time.Minute), State: []byte(`{"v":1,"b":1}`)}},
		RemovedGrants: []string{"grant-a"},
	}); err != nil {
		t.Fatalf("save as b: %v", err)
	}
	if _, claimed, err := replicas.ClaimPlaySession(ctx, "session-a", "b", now); err != nil || claimed {
		t.Fatalf("b claimed a's live session: %v, %v", claimed, err)
	}
	if _, claimed, err := replicas.ClaimPlayGrant(ctx, "grant-a", "b", now); err != nil || claimed {
		t.Fatalf("b claimed a's live grant: %v, %v", claimed, err)
	}
	if claimed, err := replicas.ClaimNewPlaySession(ctx, store.PlaySessionRecord{ID: "session-a", UserID: user, LastActivity: now, State: []byte(`{}`)}, "b", now); err != nil || claimed {
		t.Fatalf("b claimed a's live session as new: %v, %v", claimed, err)
	}
	record, claimed, err := replicas.ClaimPlaySession(ctx, "session-a", "a", now)
	if err != nil || !claimed || string(record.State) != `{"v":1,"a":1}` || record.UserID != user || !record.LastActivity.Equal(now) {
		t.Fatalf("a's own claim = %+v, %v, %v", record, claimed, err)
	}

	// A new session goes to the first claimer.
	if claimed, err := replicas.ClaimNewPlaySession(ctx, store.PlaySessionRecord{ID: "session-new", UserID: user, LastActivity: now, State: []byte(`{"v":1}`)}, "b", now); err != nil || !claimed {
		t.Fatalf("ClaimNewPlaySession = %v, %v", claimed, err)
	}
	if claimed, err := replicas.ClaimNewPlaySession(ctx, store.PlaySessionRecord{ID: "session-new", UserID: user, LastActivity: now, State: []byte(`{"v":1}`)}, "a", now); err != nil || claimed {
		t.Fatalf("second ClaimNewPlaySession = %v, %v", claimed, err)
	}

	// Remote transcode jobs.
	jobs := store.NewTranscodeWorkerStore(stores)
	job := store.TranscodeWorkerJob{ID: "job-a", WorkerID: "w", Playlist: "/t/a.m3u8", SegmentPattern: "/t/a%d.ts", TokenHash: []byte{1},
		SegmentLength: 3, Created: now, LeaseExpires: now.Add(time.Minute), Owner: "a"}
	if err := jobs.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if got, err := jobs.Job(ctx, "job-a"); err != nil || got.Owner != "a" {
		t.Fatalf("Job = %+v, %v", got, err)
	}
	if claimed, err := replicas.ClaimTranscodeJob(ctx, "job-a", "b", now); err != nil || claimed {
		t.Fatalf("b claimed a's live job: %v, %v", claimed, err)
	}

	// a releases: its rows have no owner, and b claims them.
	if err := replicas.ReleaseReplica(ctx, "a"); err != nil {
		t.Fatalf("ReleaseReplica: %v", err)
	}
	if live, err := replicas.LiveReplicas(ctx, now); err != nil || len(live) != 1 || live[0].ID != "b" {
		t.Fatalf("live after release = %+v, %v", live, err)
	}
	if owner, _, err := replicas.PlaySessionOwner(ctx, "session-a"); err != nil || owner != "" {
		t.Fatalf("owner after release = %q, %v", owner, err)
	}
	if claimed, err := replicas.ClaimTranscodeJob(ctx, "job-a", "b", now); err != nil || !claimed {
		t.Fatalf("b's job claim after release = %v, %v", claimed, err)
	}
	if grant, claimed, err := replicas.ClaimPlayGrant(ctx, "grant-a", "b", now); err != nil || !claimed || grant.ItemID != item || len(grant.MediaSourceIDs) != 1 {
		t.Fatalf("b's grant claim = %+v, %v, %v", grant, claimed, err)
	}
	if err := ownedB.SavePlaySessions(ctx, store.PlaySessionChanges{RemovedGrants: []string{"grant-a"}}); err != nil {
		t.Fatalf("b removes its grant: %v", err)
	}
	if _, found, err := replicas.PlayGrantOwner(ctx, "grant-a"); err != nil || found {
		t.Fatalf("grant after b's removal found = %v, %v", found, err)
	}

	// A dead owner's rows can be claimed in bulk.
	if err := store.NewPlaySessionStore(stores).WithOwner("dead").SavePlaySessions(ctx, store.PlaySessionChanges{
		Sessions: []store.PlaySessionRecord{
			{ID: "dead-1", UserID: user, LastActivity: now.Add(-time.Hour), State: []byte(`{}`)},
			{ID: "dead-old", UserID: user, LastActivity: now.Add(-48 * time.Hour), State: []byte(`{}`)},
		},
		Grants: []store.PlayGrantRecord{{PlaySessionID: "grant-dead", UserID: user, ItemID: item, LastUsed: now.Add(-time.Minute)}},
	}); err != nil {
		t.Fatalf("save as dead: %v", err)
	}
	claimedSessions, err := replicas.ClaimPlaySessions(ctx, "b", now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("ClaimPlaySessions: %v", err)
	}
	ids := make([]string, 0, len(claimedSessions))
	for _, record := range claimedSessions {
		ids = append(ids, record.ID)
	}
	// session-a (released) and dead-1 (dead owner) are claimable; dead-old
	// is too old and session-new is b's already (b may claim its own).
	if len(ids) != 3 || ids[0] != "dead-1" {
		t.Fatalf("claimed sessions = %v", ids)
	}
	claimedGrants, err := replicas.ClaimPlayGrants(ctx, "b", now.Add(-4*time.Hour), now)
	if err != nil || len(claimedGrants) != 1 || claimedGrants[0].PlaySessionID != "grant-dead" {
		t.Fatalf("ClaimPlayGrants = %+v, %v", claimedGrants, err)
	}

	// A single process (no owner) writes every row.
	if err := store.NewPlaySessionStore(stores).SavePlaySessions(ctx, store.PlaySessionChanges{RemovedSessions: []string{"session-new"}}); err != nil {
		t.Fatalf("single-process removal: %v", err)
	}
	if _, found, err := replicas.PlaySessionOwner(ctx, "session-new"); err != nil || found {
		t.Fatalf("session-new after single-process removal found = %v, %v", found, err)
	}

	if err := replicas.DeleteExpiredReplicas(ctx, now.Add(time.Hour)); err != nil {
		t.Fatalf("DeleteExpiredReplicas: %v", err)
	}
	if live, err := replicas.LiveReplicas(ctx, now); err != nil || len(live) != 0 {
		t.Fatalf("live after expiry delete = %+v, %v", live, err)
	}
}
