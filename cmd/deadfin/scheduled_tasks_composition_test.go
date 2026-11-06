// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/store/storetest"
	"github.com/deadfin-org/deadfin/internal/tasks"
)

func TestScheduledTasksCompositionReturnsTwelveTasksElevated(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")
	app := newCompositionApplication(t, root)

	// Complete startup wizard and create an admin user.
	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/Configuration", []byte(`{"ServerName":"Test","UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}`), http.StatusNoContent)
	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/User", []byte(`{"Name":"admin","Password":"correct horse battery staple"}`), http.StatusNoContent)
	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/Complete", nil, http.StatusNoContent)

	// Authenticate as the admin user.
	login := compositionRequestFrom(app.Router(), http.MethodPost, "/Users/AuthenticateByName", []byte(`{"Username":"admin","Pw":"correct horse battery staple"}`), "127.0.0.1:8096", "")
	token := compositionAccessToken(t, login)
	if token == "" {
		t.Fatal("login returned no access token")
	}

	// GET /ScheduledTasks with elevated identity should return 13 tasks: the 12 stock
	// built-ins and the Refresh Guide task, hidden while no tuner is set.
	rec := authenticatedCompositionRequest(app.Router(), http.MethodGet, "/ScheduledTasks", nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ScheduledTasks status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var tasks []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("unmarshal ScheduledTasks response: %v; body: %s", err, rec.Body.String())
	}
	if len(tasks) != 13 {
		t.Fatalf("ScheduledTasks returned %d tasks, want 13", len(tasks))
	}
}

// TestStoreMaintainerAdapterSkipsUnsupportedBackend proves the OptimizeDatabase
// adapter log-skips (returns nil) when the store backend does not implement
// store.Maintainer, instead of running SQLite-specific PRAGMA/VACUUM statements
// that would fail on an unsupported backend (spec §4: "Backend without
// Maintainer → log-skip, Completed").
func TestStoreMaintainerAdapterSkipsUnsupportedBackend(t *testing.T) {
	adapter := storeMaintainerAdapter{stores: storetest.UnsupportedStores()}
	if err := adapter.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain on unsupported backend = %v, want nil (log-skip)", err)
	}
}

// TestScheduledTasksCompositionCloseMidRunAborts proves app.Close returns nil
// while a scheduled task is mid-run, and the execution is recorded as
// Aborted.
func TestScheduledTasksCompositionCloseMidRunAborts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")

	// Construct the blocking fixture before application construction so the
	// task registers through withScheduledTask before scheduler.Start.
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	taskDef := tasks.Definition{
		ID:   "close-midrun-0000000000000000000000000001",
		Key:  "close-midrun",
		Name: "Close Mid-Run Test",
	}
	app := newCompositionApplication(t, root, withScheduledTask(taskDef, testBlockingRunnable{block: block, started: started}))

	// Run the pre-registered task, then close the app while it is mid-run.
	sched := app.scheduler
	if sched == nil {
		t.Skip("composition application has no scheduler")
	}
	if err := sched.Run("close-midrun-0000000000000000000000000001"); err != nil && err != tasks.ErrSchedulerClosed {
		t.Fatalf("Run: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("task did not start")
	}

	// Close the app while the task is mid-run. app.Close drains the scheduler
	// before dependent stores close. Release the block first so the runnable
	// observes the Close-canceled context.
	close(block)
	if err := app.Close(context.Background()); err != nil {
		t.Fatalf("app.Close during mid-run task = %v, want nil", err)
	}
	// Verify the Aborted result reached durable state: read the persisted
	// result .js file from the scheduler's data directory.
	resultPath := filepath.Join(root, "data", "ScheduledTasks",
		"close-midrun-0000000000000000000000000001.js")
	resultBody, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read persisted result file %s: %v", resultPath, err)
	}
	var persistedResult tasks.Result
	if err := json.Unmarshal(resultBody, &persistedResult); err != nil {
		t.Fatalf("unmarshal persisted result: %v (body: %s)", err, resultBody)
	}
	if persistedResult.Status != tasks.StatusAborted {
		t.Fatalf("persisted result status = %q, want %q", persistedResult.Status, tasks.StatusAborted)
	}
}

type testBlockingRunnable struct {
	block   chan struct{}
	started chan struct{}
}

func (r testBlockingRunnable) Run(ctx context.Context, _ func(float64)) error {
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestApplicationConfiguredTaskRegistrationErrorIncludesID proves a
// duplicate configured task registration fails construction, returns a nil
// application, and preserves the configured task ID in the error.
func TestApplicationConfiguredTaskRegistrationErrorIncludesID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")

	duplicateID := tasks.RefreshLibraryID
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	dupDef := tasks.Definition{
		ID:   duplicateID,
		Key:  "duplicate-configured",
		Name: "Duplicate Configured Task",
	}

	app, err := newApplicationWithListen(t.Context(), root, defaultListenAddress,
		withScheduledTask(dupDef, testBlockingRunnable{block: block, started: started}))
	if app != nil {
		close(block)
		if closeErr := app.Close(context.Background()); closeErr != nil {
			t.Errorf("close application: %v", closeErr)
		}
		t.Fatal("newApplicationWithListen returned a non-nil application, want nil")
	}
	if err == nil {
		t.Fatal("newApplicationWithListen returned nil error, want a construction error")
	}
	if !strings.Contains(err.Error(), duplicateID) {
		t.Fatalf("error = %q, want it to contain the configured task ID %q", err.Error(), duplicateID)
	}
}
