// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/storetest"
)

func TestParseDatabase(t *testing.T) {
	dataDir := filepath.Join("program", "data")
	for _, tc := range []struct {
		name     string
		raw      string
		postgres string
		sqlite   string
		err      string
	}{
		{name: "empty selects SQLite", raw: "", sqlite: filepath.Join(dataDir, "deadfin.db")},
		{name: "blank selects SQLite", raw: "  ", sqlite: filepath.Join(dataDir, "deadfin.db")},
		{name: "postgres scheme", raw: "postgres://deadfin:secret@db:5432/deadfin?sslmode=disable", postgres: "postgres://deadfin:secret@db:5432/deadfin?sslmode=disable"},
		{name: "postgresql scheme", raw: "postgresql://db/deadfin", postgres: "postgresql://db/deadfin"},
		{name: "scheme case", raw: "POSTGRES://db/deadfin", postgres: "POSTGRES://db/deadfin"},
		{name: "unknown scheme", raw: "mysql://root:secret@db/deadfin", err: `scheme "mysql" is not supported`},
		{name: "sqlite scheme", raw: "sqlite:///data/deadfin.db", err: `scheme "sqlite" is not supported`},
		{name: "keyword form", raw: "host=db password=secret", err: "is not a valid URL"},
		{name: "multi-host", raw: "postgres://deadfin:secret@pg-a:5432,pg-b:5432/deadfin?target_session_attrs=read-write", postgres: "postgres://deadfin:secret@pg-a:5432,pg-b:5432/deadfin?target_session_attrs=read-write"},
		{name: "invalid URL", raw: "postgres://deadfin:secret@db:port/x", err: "is not a valid URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDatabase(tc.raw, dataDir)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("parseDatabase(%q) error = %v, want %q", tc.raw, err, tc.err)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatalf("error %q repeats the password", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDatabase(%q): %v", tc.raw, err)
			}
			if got.postgresDSN != tc.postgres || got.sqlitePath != tc.sqlite {
				t.Fatalf("parseDatabase(%q) = %+v, want postgres %q sqlite %q", tc.raw, got, tc.postgres, tc.sqlite)
			}
		})
	}
}

func TestDatabaseStringRedactsPassword(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"postgres://deadfin:secret@db:5432/deadfin?sslmode=disable", "PostgreSQL postgres://deadfin:xxxxx@db:5432/deadfin?sslmode=disable"},
		{"postgres://deadfin@db/deadfin", "PostgreSQL postgres://deadfin@db/deadfin"},
		{"postgres://deadfin:secret@pg-a:5432,pg-b:5432/deadfin?target_session_attrs=read-write", "PostgreSQL postgres://deadfin:xxxxx@pg-a:5432,pg-b:5432/deadfin?target_session_attrs=read-write"},
		{"postgres://db/deadfin?password=secret&user=deadfin", "PostgreSQL postgres://db/deadfin?password=xxxxx&user=deadfin"},
		{"postgres://deadfin:s%40cr%3At@db/deadfin", "PostgreSQL postgres://deadfin:xxxxx@db/deadfin"},
	} {
		got := database{postgresDSN: tc.raw}.String()
		if got != tc.want {
			t.Errorf("String(%q) = %q, want %q", tc.raw, got, tc.want)
		}
		if strings.Contains(got, "secret") || strings.Contains(got, "s%40cr") {
			t.Errorf("String(%q) = %q, repeats the password", tc.raw, got)
		}
	}
	if got := (database{sqlitePath: "/data/deadfin.db"}).String(); got != "SQLite /data/deadfin.db" {
		t.Errorf("SQLite String = %q", got)
	}
}

func TestResolveDatabaseURL(t *testing.T) {
	if got := resolveDatabaseURL("postgres://flag/db", "postgres://env/db"); got != "postgres://flag/db" {
		t.Errorf("flag value = %q, want the flag", got)
	}
	if got := resolveDatabaseURL("", "postgres://env/db"); got != "postgres://env/db" {
		t.Errorf("empty flag = %q, want the environment", got)
	}
	if got := resolveDatabaseURL("", ""); got != "" {
		t.Errorf("both empty = %q, want empty", got)
	}
}

func TestApplicationRejectsUnknownDatabaseScheme(t *testing.T) {
	app, err := newApplicationWithListen(t.Context(), t.TempDir(), defaultListenAddress, withDatabaseURL("mysql://root:secret@db/deadfin"))
	if app != nil {
		_ = app.Close(context.Background())
		t.Fatal("application started with an unknown database scheme")
	}
	if err == nil || !strings.Contains(err.Error(), `scheme "mysql" is not supported`) {
		t.Fatalf("error = %v, want the unsupported scheme", err)
	}
}

// TestApplicationStartsOnPostgres starts the full application against an
// empty PostgreSQL schema, stops it, and starts it again on the migrated
// schema. It leaves no SQLite file in the data directory.
func TestApplicationStartsOnPostgres(t *testing.T) {
	databaseURL := storetest.PostgresSchemaURL(t)
	programData := t.TempDir()
	for run := range 2 {
		app, err := newApplicationWithListen(t.Context(), programData, defaultListenAddress, withDatabaseURL(databaseURL))
		if err != nil {
			t.Fatalf("start %d: %v", run, err)
		}
		users, err := store.NewIdentity(app.stores).ListUsers(t.Context())
		if err != nil {
			t.Fatalf("list users: %v", err)
		}
		// Startup creates the first user once; the second start keeps it.
		if len(users) != 1 {
			t.Fatalf("start %d: users = %d, want 1", run, len(users))
		}
		if err := app.Close(context.Background()); err != nil {
			t.Fatalf("close %d: %v", run, err)
		}
	}
	paths, err := config.NewPaths(programData)
	if err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(paths.Data, "deadfin.db*")); len(matches) != 0 {
		t.Fatalf("PostgreSQL startup created SQLite files %v", matches)
	}
}

// TestSubcommandsOnPostgres runs add-library, import-jellyfin, remap-user-ids,
// and backfill against one PostgreSQL schema.
func TestSubcommandsOnPostgres(t *testing.T) {
	testSubcommands(t, storetest.PostgresSchemaURL(t))
}

func TestSubcommandsOnSQLite(t *testing.T) {
	testSubcommands(t, "")
}

func testSubcommands(t *testing.T, databaseURL string) {
	ctx := t.Context()
	programData := t.TempDir()
	paths, err := config.NewPaths(programData)
	if err != nil {
		t.Fatal(err)
	}
	if err := runAddLibrary(ctx, programData, databaseURL, "Movies", "movies", t.TempDir()); err != nil {
		t.Fatalf("add-library: %v", err)
	}

	var out bytes.Buffer
	if err := runImportJellyfin(ctx, &out, programData, databaseURL, storetest.JellyfinFixture(t), "", false); err != nil {
		t.Fatalf("import-jellyfin: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "users: 1 created with new ids") || !strings.Contains(out.String(), "device tokens: 1 imported") || !strings.Contains(out.String(), "api keys: 1 imported") || !strings.Contains(out.String(), "playlists: 1 imported") {
		t.Fatalf("import report:\n%s", out.String())
	}

	stores, selected, err := openProgramDatabase(ctx, databaseURL, paths.Data)
	if err != nil {
		t.Fatal(err)
	}
	if (selected.postgresDSN != "") != (databaseURL != "") {
		t.Fatalf("selected %s for %q", selected, databaseURL)
	}
	identity := store.NewIdentity(stores)
	users, err := identity.ListUsers(ctx)
	if err != nil || len(users) != 1 || users[0].Name != "whiskey" {
		t.Fatalf("imported users = %+v, %v", users, err)
	}
	if device, err := identity.DeviceByToken(ctx, "token-1"); err != nil || device.UserID != users[0].ID {
		t.Fatalf("imported device = %+v, %v", device, err)
	}
	if _, err := identity.APIKeyByToken(ctx, "key-1"); err != nil {
		t.Fatalf("imported API key: %v", err)
	}
	if err := stores.Close(); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runRemapUserIDs(ctx, &out, programData, databaseURL, clock.NewFake(time.Date(2026, time.October, 13, 0, 0, 0, 0, time.UTC)), true); err != nil {
		t.Fatalf("remap-user-ids: %v", err)
	}
	if !strings.Contains(out.String(), `user "whiskey"`) {
		t.Fatalf("remap report:\n%s", out.String())
	}
	if err := runBackfillCmd(ctx, programData, databaseURL); err != nil {
		t.Fatalf("backfill: %v", err)
	}
}
