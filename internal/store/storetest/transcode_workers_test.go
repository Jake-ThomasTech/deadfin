// SPDX-License-Identifier: GPL-2.0-only

package storetest_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

// TestTranscodeWorkerStore defends the transcode worker credential tables
// (#615) on SQLite and, when DEADFIN_TEST_POSTGRES_DSN is set, PostgreSQL.
func TestTranscodeWorkerStore(t *testing.T) {
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	t.Run("sqlite", func(t *testing.T) { verifyTranscodeWorkerStore(t, stores) })
	if dsn := os.Getenv("DEADFIN_TEST_POSTGRES_DSN"); dsn != "" {
		pg := openPostgresFromDSN(t, dsn)
		t.Run("postgres", func(t *testing.T) { verifyTranscodeWorkerStore(t, pg) })
	}
}

func verifyTranscodeWorkerStore(t *testing.T, stores store.Stores) {
	ctx := context.Background()
	workers := store.NewTranscodeWorkerStore(stores)
	now := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)

	if configured, err := workers.Configured(ctx); err != nil || configured {
		t.Fatalf("empty store Configured = %v, %v; want false", configured, err)
	}
	one := 1
	tokens := []store.TranscodeWorkerJoinToken{
		{ID: "single", Name: "one use", DerivedKey: []byte("key-single"), Fingerprint: "pin", UsesLeft: &one, Created: now},
		{ID: "fleet", Name: "fleet", DerivedKey: []byte("key-fleet"), Expires: now.Add(time.Hour), Created: now.Add(time.Second)},
	}
	for _, token := range tokens {
		if err := workers.CreateJoinToken(ctx, token); err != nil {
			t.Fatalf("CreateJoinToken(%s): %v", token.ID, err)
		}
	}
	got, err := workers.JoinTokens(ctx)
	if err != nil || len(got) != 2 || got[0].ID != "single" || got[1].ID != "fleet" {
		t.Fatalf("JoinTokens = %+v, %v", got, err)
	}
	if *got[0].UsesLeft != 1 || got[1].UsesLeft != nil || !got[1].Expires.Equal(now.Add(time.Hour)) || got[0].Fingerprint != "pin" ||
		string(got[0].DerivedKey) != "key-single" || !got[0].Created.Equal(now) || !got[0].Revoked.IsZero() {
		t.Fatalf("stored tokens = %+v", got)
	}
	if configured, err := workers.Configured(ctx); err != nil || !configured {
		t.Fatalf("Configured with tokens = %v, %v; want true", configured, err)
	}

	// The single-use token enrolls one worker, then is spent.
	first := store.TranscodeWorker{ID: "w1", Name: "gpu-a", JoinTokenID: "single", DerivedKey: []byte("worker-key-1"), Pools: []string{" QSV", "big-gpu", "qsv", ""}}
	if err := workers.EnrollWorker(ctx, first, now); err != nil {
		t.Fatalf("EnrollWorker: %v", err)
	}
	second := store.TranscodeWorker{ID: "w2", Name: "gpu-b", JoinTokenID: "single", DerivedKey: []byte("worker-key-2")}
	if err := workers.EnrollWorker(ctx, second, now); !errors.Is(err, store.ErrJoinTokenUnusable) {
		t.Fatalf("spent token: %v, want ErrJoinTokenUnusable", err)
	}
	if _, err := workers.Worker(ctx, "w2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused enrollment stored a worker: %v", err)
	}
	// The fleet token has no use limit but expires.
	second.JoinTokenID = "fleet"
	if err := workers.EnrollWorker(ctx, second, now.Add(time.Minute)); err != nil {
		t.Fatalf("fleet enrollment: %v", err)
	}
	third := store.TranscodeWorker{ID: "w3", Name: "late", JoinTokenID: "fleet", DerivedKey: []byte("k")}
	if err := workers.EnrollWorker(ctx, third, now.Add(time.Hour)); !errors.Is(err, store.ErrJoinTokenUnusable) {
		t.Fatalf("expired token: %v, want ErrJoinTokenUnusable", err)
	}
	third.JoinTokenID = "missing"
	if err := workers.EnrollWorker(ctx, third, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown token: %v, want ErrNotFound", err)
	}
	third.JoinTokenID, third.InputMode = "fleet", "magic"
	if err := workers.EnrollWorker(ctx, third, now); err == nil {
		t.Fatal("an invalid input mode was stored")
	}

	stored, err := workers.Worker(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "gpu-a" || stored.InputMode != store.TranscodeWorkerInputAuto || !reflect.DeepEqual(stored.Pools, []string{"big-gpu", "qsv"}) ||
		string(stored.DerivedKey) != "worker-key-1" || !stored.Created.Equal(now) || !stored.LastSeen.Equal(now) || !stored.Revoked.IsZero() {
		t.Fatalf("worker = %+v", stored)
	}

	name, mode, pools := "gpu-a-renamed", store.TranscodeWorkerInputShared, []string{"nvenc"}
	if err := workers.UpdateWorker(ctx, "w1", store.TranscodeWorkerUpdate{Name: &name, InputMode: &mode, Pools: &pools}); err != nil {
		t.Fatalf("UpdateWorker: %v", err)
	}
	bad := "magic"
	if err := workers.UpdateWorker(ctx, "w1", store.TranscodeWorkerUpdate{InputMode: &bad}); err == nil {
		t.Fatal("UpdateWorker stored an invalid input mode")
	}
	if err := workers.UpdateWorker(ctx, "nobody", store.TranscodeWorkerUpdate{Name: &name}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateWorker(unknown) = %v, want ErrNotFound", err)
	}
	if err := workers.TouchWorker(ctx, "w1", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	all, err := workers.Workers(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("Workers = %+v, %v", all, err)
	}
	if all[0].ID != "w1" || all[0].Name != name || all[0].InputMode != mode || !reflect.DeepEqual(all[0].Pools, pools) ||
		!all[0].LastSeen.Equal(now.Add(2*time.Minute)) || all[1].ID != "w2" || all[1].Pools != nil {
		t.Fatalf("Workers = %+v", all)
	}

	// Revoking a token stops enrollment only; revoking a worker keeps the
	// row and the first revocation time.
	if err := workers.RevokeJoinToken(ctx, "fleet", now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	third.InputMode = ""
	if err := workers.EnrollWorker(ctx, third, now.Add(4*time.Minute)); !errors.Is(err, store.ErrJoinTokenUnusable) {
		t.Fatalf("revoked token: %v, want ErrJoinTokenUnusable", err)
	}
	if w2, err := workers.Worker(ctx, "w2"); err != nil || !w2.Revoked.IsZero() {
		t.Fatalf("token revocation revoked its worker: %+v, %v", w2, err)
	}
	for _, at := range []time.Duration{5 * time.Minute, 6 * time.Minute} {
		if err := workers.RevokeWorker(ctx, "w1", now.Add(at)); err != nil {
			t.Fatal(err)
		}
	}
	if w1, err := workers.Worker(ctx, "w1"); err != nil || !w1.Revoked.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("revoked worker = %+v, %v", w1, err)
	}
	if err := workers.RevokeWorker(ctx, "nobody", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RevokeWorker(unknown) = %v, want ErrNotFound", err)
	}
	if err := workers.RevokeJoinToken(ctx, "nobody", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RevokeJoinToken(unknown) = %v, want ErrNotFound", err)
	}
	if err := workers.RevokeWorker(ctx, "w2", now.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if configured, err := workers.Configured(ctx); err != nil || !configured {
		t.Fatalf("Configured with only revoked credentials = %v, %v; want true", configured, err)
	}

	if err := workers.SetPins(ctx, []string{"next", "current", "next", ""}, now); err != nil {
		t.Fatal(err)
	}
	if pins, err := workers.Pins(ctx); err != nil || !reflect.DeepEqual(pins, []string{"current", "next"}) {
		t.Fatalf("Pins = %v, %v", pins, err)
	}
	if err := workers.SetPins(ctx, nil, now); err != nil {
		t.Fatal(err)
	}
	if pins, err := workers.Pins(ctx); err != nil || len(pins) != 0 {
		t.Fatalf("Pins after clear = %v, %v", pins, err)
	}

	if _, err := workers.Policy(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Policy before set = %v, want ErrNotFound", err)
	}
	for _, policy := range []string{`{"mode":"workers-first"}`, `{"mode":"local"}`} {
		if err := workers.SetPolicy(ctx, []byte(policy), now); err != nil {
			t.Fatal(err)
		}
		if got, err := workers.Policy(ctx); err != nil || string(got) != policy {
			t.Fatalf("Policy = %q, %v; want %q", got, err, policy)
		}
	}

	if _, err := workers.ScalingTokenHash(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ScalingTokenHash before set = %v, want ErrNotFound", err)
	}
	for _, hash := range [][]byte{{1, 2, 3}, {4, 5, 6}} {
		if err := workers.SetScalingTokenHash(ctx, hash, now); err != nil {
			t.Fatal(err)
		}
		if got, err := workers.ScalingTokenHash(ctx); err != nil || !reflect.DeepEqual(got, hash) {
			t.Fatalf("ScalingTokenHash = %v, %v; want %v", got, err, hash)
		}
	}
	if err := workers.DeleteScalingToken(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := workers.ScalingTokenHash(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ScalingTokenHash after delete = %v, want ErrNotFound", err)
	}
	verifyTranscodeWorkerJobs(t, workers, now)
}

// verifyTranscodeWorkerJobs defends the job records (#618).
func verifyTranscodeWorkerJobs(t *testing.T, workers *store.TranscodeWorkerStore, now time.Time) {
	ctx := context.Background()
	if err := workers.CreateJob(ctx, store.TranscodeWorkerJob{ID: "incomplete"}); err == nil {
		t.Fatal("CreateJob accepted a record without a family")
	}
	records := []store.TranscodeWorkerJob{
		{ID: "job-a", WorkerID: "w1", Playlist: "/t/a.m3u8", SegmentPattern: "/t/a%d.ts", PlaySessionID: "play-a", DeviceID: "dev",
			TokenHash: []byte{9, 9}, SegmentLength: 6, StartNumber: 4,
			InputMode: store.TranscodeWorkerInputStream, SourcePath: "/media/a.mkv", SourceSize: 5 << 30, Created: now, LeaseExpires: now.Add(time.Minute)},
		{ID: "job-b", WorkerID: "static-x-y", Playlist: "/t/b.m3u8", SegmentPattern: "/t/b%d.ts",
			TokenHash: []byte{8}, SegmentLength: 3, InputMode: store.TranscodeWorkerInputAuto, Created: now.Add(time.Second), LeaseExpires: now.Add(30 * time.Second)},
	}
	for _, record := range records {
		if err := workers.CreateJob(ctx, record); err != nil {
			t.Fatalf("CreateJob(%s): %v", record.ID, err)
		}
	}
	got, err := workers.Job(ctx, "job-a")
	if err != nil || !reflect.DeepEqual(got, records[0]) {
		t.Fatalf("Job = %+v, %v; want %+v", got, err, records[0])
	}
	if _, err := workers.Job(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Job(missing) = %v, want ErrNotFound", err)
	}
	live, err := workers.LiveJobs(ctx, now.Add(30*time.Second))
	if err != nil || len(live) != 1 || live[0].ID != "job-a" {
		t.Fatalf("LiveJobs at the second lease end = %+v, %v; want job-a only", live, err)
	}
	if err := workers.RenewJobs(ctx, []string{"job-b"}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	live, err = workers.LiveJobs(ctx, now.Add(90*time.Second))
	if err != nil || len(live) != 1 || live[0].ID != "job-b" || !live[0].LeaseExpires.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("LiveJobs after renewal = %+v, %v; want job-b", live, err)
	}
	if err := workers.DeleteExpiredJobs(ctx, now.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := workers.Job(ctx, "job-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired job-a = %v, want ErrNotFound", err)
	}
	if err := workers.DeleteJob(ctx, "job-b"); err != nil {
		t.Fatal(err)
	}
	if err := workers.DeleteJob(ctx, "job-b"); err != nil {
		t.Fatalf("second DeleteJob = %v", err)
	}
	if live, err := workers.LiveJobs(ctx, now); err != nil || len(live) != 0 {
		t.Fatalf("LiveJobs after delete = %+v, %v", live, err)
	}
}

// TestTranscodeWorkerStoreDeleteWorker defends DeleteWorker (#623): an
// ephemeral worker's identity and pool labels are gone after it.
func TestTranscodeWorkerStoreDeleteWorker(t *testing.T) {
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	t.Run("sqlite", func(t *testing.T) { verifyDeleteTranscodeWorker(t, stores) })
	if dsn := os.Getenv("DEADFIN_TEST_POSTGRES_DSN"); dsn != "" {
		pg := openPostgresFromDSN(t, dsn)
		t.Run("postgres", func(t *testing.T) { verifyDeleteTranscodeWorker(t, pg) })
	}
}

func verifyDeleteTranscodeWorker(t *testing.T, stores store.Stores) {
	ctx := context.Background()
	workers := store.NewTranscodeWorkerStore(stores)
	now := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)
	if err := workers.CreateJoinToken(ctx, store.TranscodeWorkerJoinToken{ID: "deploy-del", DerivedKey: []byte("k"), Created: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"eph-del-1", "eph-del-2"} {
		if err := workers.EnrollWorker(ctx, store.TranscodeWorker{ID: id, Name: id, JoinTokenID: "deploy-del", DerivedKey: []byte(id), Pools: []string{"gpu"}}, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := workers.DeleteWorker(ctx, "eph-del-1"); err != nil {
		t.Fatalf("DeleteWorker: %v", err)
	}
	if _, err := workers.Worker(ctx, "eph-del-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted worker = %v, want ErrNotFound", err)
	}
	if kept, err := workers.Worker(ctx, "eph-del-2"); err != nil || !reflect.DeepEqual(kept.Pools, []string{"gpu"}) {
		t.Fatalf("other worker = %+v, %v", kept, err)
	}
	if err := workers.DeleteWorker(ctx, "eph-del-1"); err != nil {
		t.Fatalf("second DeleteWorker: %v", err)
	}
	// The pool rows went with the worker: the id can enroll again cleanly.
	if err := workers.EnrollWorker(ctx, store.TranscodeWorker{ID: "eph-del-1", Name: "again", JoinTokenID: "deploy-del", DerivedKey: []byte("k2")}, now); err != nil {
		t.Fatal(err)
	}
	if again, err := workers.Worker(ctx, "eph-del-1"); err != nil || len(again.Pools) != 0 {
		t.Fatalf("re-enrolled worker = %+v, %v", again, err)
	}
}
