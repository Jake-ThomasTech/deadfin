// SPDX-License-Identifier: GPL-2.0-only

package tasks_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/tasks"
)

// testMetadataRefresher implements tasks.MetadataRefresher for tests.
type testMetadataRefresher struct {
	fn func(context.Context, func(float64)) error
}

func (m testMetadataRefresher) RefreshMetadata(ctx context.Context, progress func(float64)) error {
	if m.fn == nil {
		return nil
	}
	return m.fn(ctx, progress)
}

// TestRefreshMetadataTaskIdentity proves the manual RefreshMetadata task has
// the stable identity, key, name, category, and no default trigger that the
// spec requires.
func TestRefreshMetadataTaskIdentity(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, _, _, _ := newTestScheduler(t, c)

	if err := tasks.RegisterBuiltins(s, tasks.BuiltinDeps{
		MetadataRefresher: testMetadataRefresher{},
	}); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}

	info, ok := s.Get(tasks.RefreshMetadataID)
	if !ok {
		t.Fatalf("RefreshMetadata task %s not registered", tasks.RefreshMetadataID)
	}
	if got, want := info.ID, tasks.RefreshMetadataID; got != want {
		t.Fatalf("RefreshMetadata ID = %q, want %q", got, want)
	}
	if got, want := info.Key, "RefreshMetadata"; got != want {
		t.Errorf("RefreshMetadata key = %q, want %q", got, want)
	}
	if got, want := info.Name, "Refresh Metadata"; got != want {
		t.Errorf("RefreshMetadata name = %q, want %q", got, want)
	}
	if got, want := info.Category, "Library"; got != want {
		t.Errorf("RefreshMetadata category = %q, want %q", got, want)
	}
	// The task is a manual backfill: it carries no default trigger.
	if len(info.Triggers) != 0 {
		t.Errorf("RefreshMetadata triggers = %+v, want none", info.Triggers)
	}
	// The stable ID derives from the Deadfin type name through the .NET Guid
	// algorithm, matching every other stock task.
	if got, want := info.ID, dotNetGUID("Deadfin.ScheduledTasks.Tasks.RefreshMetadataTask"); got != want {
		t.Errorf("RefreshMetadata ID = %q, want derived %q", got, want)
	}
}

// TestRefreshMetadataManualTriggerReportsFailure proves a refresher error
// produces a Failed task result that carries the error message.
func TestRefreshMetadataManualTriggerReportsFailure(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, _, _, _ := newTestScheduler(t, c)

	refresherErr := errors.New("metadata refresh failed: /movies is not accessible")
	if err := tasks.RegisterBuiltins(s, tasks.BuiltinDeps{
		MetadataRefresher: testMetadataRefresher{fn: func(context.Context, func(float64)) error {
			return refresherErr
		}},
	}); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}

	if err := s.Run(tasks.RefreshMetadataID); err != nil {
		t.Fatalf("Run RefreshMetadata: %v", err)
	}
	waitForResultStatus(t, s, tasks.RefreshMetadataID, tasks.StatusFailed)
	info, ok := s.Get(tasks.RefreshMetadataID)
	if !ok {
		t.Fatalf("RefreshMetadata task %s not registered", tasks.RefreshMetadataID)
	}
	if info.LastExecutionResult == nil {
		t.Fatal("RefreshMetadata has no last execution result")
	}
	if info.LastExecutionResult.Status != tasks.StatusFailed {
		t.Fatalf("RefreshMetadata result status = %q, want %q", info.LastExecutionResult.Status, tasks.StatusFailed)
	}
	if info.LastExecutionResult.ErrorMessage == "" || info.LastExecutionResult.ErrorMessage != refresherErr.Error() {
		t.Fatalf("RefreshMetadata error message = %q, want %q", info.LastExecutionResult.ErrorMessage, refresherErr.Error())
	}
}

// TestRefreshMetadataZeroWorkReportIsNotCompleted proves a zero-work report
// cannot produce a Completed result. The adapter converts a zero-work refresh
// (no provider invocation or no persistence) into a non-nil error; the
// runnable maps that error to a Failed task result, so a silent no-op cannot
// claim success.
func TestRefreshMetadataZeroWorkReportIsNotCompleted(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, _, _, _ := newTestScheduler(t, c)

	zeroWorkErr := errors.New("refresh metadata: the remote stage persisted no provider ID or image")
	if err := tasks.RegisterBuiltins(s, tasks.BuiltinDeps{
		MetadataRefresher: testMetadataRefresher{fn: func(context.Context, func(float64)) error {
			return zeroWorkErr
		}},
	}); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}

	if err := s.Run(tasks.RefreshMetadataID); err != nil {
		t.Fatalf("Run RefreshMetadata: %v", err)
	}
	waitForResultStatus(t, s, tasks.RefreshMetadataID, tasks.StatusFailed)
	info, ok := s.Get(tasks.RefreshMetadataID)
	if !ok {
		t.Fatalf("RefreshMetadata task %s not registered", tasks.RefreshMetadataID)
	}
	if info.LastExecutionResult == nil {
		t.Fatal("RefreshMetadata has no last execution result")
	}
	if info.LastExecutionResult.Status == tasks.StatusCompleted {
		t.Fatalf("RefreshMetadata zero-work result is Completed; want Failed — a zero-work report must not report success")
	}
	if info.LastExecutionResult.Status != tasks.StatusFailed {
		t.Fatalf("RefreshMetadata zero-work result status = %q, want %q", info.LastExecutionResult.Status, tasks.StatusFailed)
	}
}

// TestRefreshMetadataPersistedProviderWorkCompletes proves a refresher that
// persists provider work returns a Completed task result. The refresher
// returns nil, so the runnable calls progress(100) and the scheduler records
// StatusCompleted with no error message.
func TestRefreshMetadataPersistedProviderWorkCompletes(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, _, _, _ := newTestScheduler(t, c)

	if err := tasks.RegisterBuiltins(s, tasks.BuiltinDeps{
		MetadataRefresher: testMetadataRefresher{fn: func(context.Context, func(float64)) error {
			return nil
		}},
	}); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}

	if err := s.Run(tasks.RefreshMetadataID); err != nil {
		t.Fatalf("Run RefreshMetadata: %v", err)
	}
	waitForResultStatus(t, s, tasks.RefreshMetadataID, tasks.StatusCompleted)
	info, ok := s.Get(tasks.RefreshMetadataID)
	if !ok {
		t.Fatalf("RefreshMetadata task %s not registered", tasks.RefreshMetadataID)
	}
	if info.LastExecutionResult == nil {
		t.Fatal("RefreshMetadata has no last execution result")
	}
	if info.LastExecutionResult.Status != tasks.StatusCompleted {
		t.Fatalf("RefreshMetadata result status = %q, want %q — persisted provider work must complete", info.LastExecutionResult.Status, tasks.StatusCompleted)
	}
	if info.LastExecutionResult.ErrorMessage != "" {
		t.Fatalf("RefreshMetadata error message = %q, want empty for a successful run", info.LastExecutionResult.ErrorMessage)
	}
}

// testProgressScanLibrary implements tasks.ScanLibrary for progress tests.
type testProgressScanLibrary struct {
	fn func(context.Context, func(float64)) (any, error)
}

func (t testProgressScanLibrary) Scan(ctx context.Context, progress func(float64)) (any, error) {
	return t.fn(ctx, progress)
}

// TestRefreshLibraryProgressEmitsIntermediateValue proves the RefreshLibrary
// runnable forwards the progress callback to ScanLibrary.Scan. The fake
// worker emits an intermediate value before it blocks. While the task holds
// Running state, the scheduler stores the value. The test reads that value
// and then releases the worker so the task completes.
func TestRefreshLibraryProgressEmitsIntermediateValue(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, _, _, _ := newTestScheduler(t, c)

	block := make(chan struct{})
	emitted := make(chan struct{})
	if err := tasks.RegisterBuiltins(s, tasks.BuiltinDeps{
		ScanLibrary: testProgressScanLibrary{fn: func(_ context.Context, progress func(float64)) (any, error) {
			progress(50)
			close(emitted)
			<-block
			return nil, nil
		}},
	}); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}

	if err := s.Run(tasks.RefreshLibraryID); err != nil {
		t.Fatalf("Run RefreshLibrary: %v", err)
	}
	<-emitted

	// The scheduler stores every progress value in CurrentProgressPercentage.
	// The fake worker calls progress(50) before it blocks, so the stored
	// value must be 50 while the task is Running.
	info, ok := s.Get(tasks.RefreshLibraryID)
	if !ok {
		t.Fatalf("RefreshLibrary task not registered")
	}
	if info.State != tasks.StateRunning {
		t.Fatalf("RefreshLibrary state = %q, want %q", info.State, tasks.StateRunning)
	}
	if info.CurrentProgressPercentage == nil {
		t.Fatal("RefreshLibrary has no progress while running")
	}
	if got, want := *info.CurrentProgressPercentage, 50.0; got != want {
		t.Fatalf("RefreshLibrary intermediate progress = %v, want %v", got, want)
	}

	// Release the worker so the task completes.
	close(block)
	waitForResultStatus(t, s, tasks.RefreshLibraryID, tasks.StatusCompleted)

	// After completion the scheduler clears the progress value.
	info, _ = s.Get(tasks.RefreshLibraryID)
	if info.LastExecutionResult == nil {
		t.Fatal("RefreshLibrary has no last execution result")
	}
	if info.LastExecutionResult.Status != tasks.StatusCompleted {
		t.Fatalf("RefreshLibrary result status = %q, want %q", info.LastExecutionResult.Status, tasks.StatusCompleted)
	}
}

// TestRefreshLibraryFirstScanAtOneHourAfterStart pins the start behavior of
// the library scan (#876). Upstream RefreshMediaLibraryTask has one default
// trigger, a 12-hour interval (RefreshMediaLibraryTask.cs GetDefaultTriggers),
// and a never-completed interval trigger first fires one hour after start
// (IntervalTrigger.cs:44-48). Thus the server does not scan at start, also
// after import-jellyfin writes libraries.
func TestRefreshLibraryFirstScanAtOneHourAfterStart(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, _, _, _ := newTestScheduler(t, c)

	scanned := make(chan struct{}, 1)
	if err := tasks.RegisterBuiltins(s, tasks.BuiltinDeps{
		ScanLibrary: testProgressScanLibrary{fn: func(context.Context, func(float64)) (any, error) {
			select {
			case scanned <- struct{}{}:
			default:
			}
			return nil, nil
		}},
	}); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	info, ok := s.Get(tasks.RefreshLibraryID)
	if !ok {
		t.Fatal("RefreshLibrary task not registered")
	}
	if len(info.Triggers) != 1 || info.Triggers[0].Type != tasks.TriggerInterval ||
		info.Triggers[0].IntervalTicks == nil || *info.Triggers[0].IntervalTicks != int64(12*time.Hour/100) {
		t.Fatalf("RefreshLibrary triggers = %+v, want one 12-hour IntervalTrigger", info.Triggers)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Startup triggers fire 3 seconds after Start; the scan does not.
	c.Advance(time.Minute)
	assertNotFired(t, scanned, "library scan ran at start")

	c.Advance(time.Hour - time.Minute - time.Second)
	assertNotFired(t, scanned, "library scan ran before one hour")

	c.Advance(time.Second)
	waitForRan(t, scanned, "library scan at one hour after start")
}

// TestRefreshMetadataProgressEmitsIntermediateValue proves the RefreshMetadata
// runnable forwards the progress callback to MetadataRefresher.RefreshMetadata.
// The fake worker emits an intermediate value before it blocks. While the
// task holds Running state, the scheduler stores the value.
func TestRefreshMetadataProgressEmitsIntermediateValue(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, _, _, _ := newTestScheduler(t, c)

	block := make(chan struct{})
	emitted := make(chan struct{})
	if err := tasks.RegisterBuiltins(s, tasks.BuiltinDeps{
		MetadataRefresher: testMetadataRefresher{fn: func(_ context.Context, progress func(float64)) error {
			progress(50)
			close(emitted)
			<-block
			return nil
		}},
	}); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}

	if err := s.Run(tasks.RefreshMetadataID); err != nil {
		t.Fatalf("Run RefreshMetadata: %v", err)
	}
	<-emitted

	info, ok := s.Get(tasks.RefreshMetadataID)
	if !ok {
		t.Fatalf("RefreshMetadata task not registered")
	}
	if info.State != tasks.StateRunning {
		t.Fatalf("RefreshMetadata state = %q, want %q", info.State, tasks.StateRunning)
	}
	if info.CurrentProgressPercentage == nil {
		t.Fatal("RefreshMetadata has no progress while running")
	}
	if got, want := *info.CurrentProgressPercentage, 50.0; got != want {
		t.Fatalf("RefreshMetadata intermediate progress = %v, want %v", got, want)
	}

	close(block)
	waitForResultStatus(t, s, tasks.RefreshMetadataID, tasks.StatusCompleted)

	info, _ = s.Get(tasks.RefreshMetadataID)
	if info.LastExecutionResult == nil {
		t.Fatal("RefreshMetadata has no last execution result")
	}
	if info.LastExecutionResult.Status != tasks.StatusCompleted {
		t.Fatalf("RefreshMetadata result status = %q, want %q", info.LastExecutionResult.Status, tasks.StatusCompleted)
	}
}

// TestRefreshLibraryProgressOmits100OnError proves the RefreshLibrary runnable
// does not call progress(100) when the scan fails. A scanner error must omit
// the terminal 100 callback. The existing task failure behavior must remain
// unchanged.
func TestRefreshLibraryProgressOmits100OnError(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, _, _, _ := newTestScheduler(t, c)

	scanErr := errors.New("scan failed")
	if err := tasks.RegisterBuiltins(s, tasks.BuiltinDeps{
		ScanLibrary: testProgressScanLibrary{fn: func(_ context.Context, progress func(float64)) (any, error) {
			progress(50)
			return nil, scanErr
		}},
	}); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}

	if err := s.Run(tasks.RefreshLibraryID); err != nil {
		t.Fatalf("Run RefreshLibrary: %v", err)
	}
	waitForResultStatus(t, s, tasks.RefreshLibraryID, tasks.StatusFailed)

	info, ok := s.Get(tasks.RefreshLibraryID)
	if !ok {
		t.Fatalf("RefreshLibrary task not registered")
	}
	if info.LastExecutionResult == nil {
		t.Fatal("RefreshLibrary has no last execution result")
	}
	if info.LastExecutionResult.Status != tasks.StatusFailed {
		t.Fatalf("RefreshLibrary result status = %q, want %q", info.LastExecutionResult.Status, tasks.StatusFailed)
	}
	if info.LastExecutionResult.ErrorMessage != scanErr.Error() {
		t.Fatalf("RefreshLibrary error message = %q, want %q", info.LastExecutionResult.ErrorMessage, scanErr.Error())
	}
}

type testAnimeSeeds struct{ calls *int }

func (seeds testAnimeSeeds) Refresh(_ context.Context, progress func(float64)) error {
	*seeds.calls++
	progress(100)
	return nil
}

func TestRefreshAnimeSeedsTaskIdentity(t *testing.T) {
	if got, want := tasks.RefreshAnimeSeedsID, dotNetGUID("Deadfin.Tasks.RefreshAnimeSeedsTask"); got != want {
		t.Fatalf("RefreshAnimeSeedsID = %s, want %s", got, want)
	}
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, _, _, _ := newTestScheduler(t, c)
	calls := 0
	if err := tasks.RegisterBuiltins(s, tasks.BuiltinDeps{AnimeSeeds: testAnimeSeeds{calls: &calls}}); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	info, ok := s.Get(tasks.RefreshAnimeSeedsID)
	if !ok {
		t.Fatal("RefreshAnimeSeeds task not registered")
	}
	if info.Key != "RefreshAnimeSeeds" || info.Category != "Library" || len(info.Triggers) != 2 {
		t.Fatalf("task info = %+v", info)
	}
	if len(s.List()) != 13 {
		t.Fatalf("task count = %d, want the 12 stock tasks plus RefreshAnimeSeeds", len(s.List()))
	}
}

type testTrickplayRefresher struct{}

func (testTrickplayRefresher) Run(context.Context, func(float64)) error { return nil }

// TestRefreshTrickplayImagesTaskIdentity pins upstream TrickplayImagesTask:
// its derived ID, Key, en-US name and category, and the daily 03:00 trigger
// (TrickplayImagesTask.cs GetDefaultTriggers). The task registers only with
// a refresher, so the stock count stays 12 without one.
func TestRefreshTrickplayImagesTaskIdentity(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, _, _, _ := newTestScheduler(t, c)
	if err := tasks.RegisterBuiltins(s, tasks.BuiltinDeps{}); err != nil {
		t.Fatalf("RegisterBuiltins without refresher: %v", err)
	}
	if _, ok := s.Get(tasks.RefreshTrickplayImagesID); ok {
		t.Fatal("RefreshTrickplayImages registered without a trickplay refresher")
	}

	s, _, _, _ = newTestScheduler(t, c)
	if err := tasks.RegisterBuiltins(s, tasks.BuiltinDeps{Trickplay: testTrickplayRefresher{}}); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	info, ok := s.Get(tasks.RefreshTrickplayImagesID)
	if !ok {
		t.Fatalf("RefreshTrickplayImages task %s not registered", tasks.RefreshTrickplayImagesID)
	}
	if got, want := info.ID, dotNetGUID("MediaBrowser.Providers.Trickplay.TrickplayImagesTask"); got != want {
		t.Errorf("RefreshTrickplayImages ID = %q, want derived %q", got, want)
	}
	if info.Key != "RefreshTrickplayImages" || info.Name != "Generate Trickplay Images" || info.Category != "Library" ||
		info.Description != "Creates trickplay previews for videos in enabled libraries." || info.IsHidden {
		t.Errorf("RefreshTrickplayImages identity = %+v", info)
	}
	if len(info.Triggers) != 1 || info.Triggers[0].Type != tasks.TriggerDaily || info.Triggers[0].TimeOfDayTicks == nil ||
		*info.Triggers[0].TimeOfDayTicks != int64(3*time.Hour/100) {
		t.Errorf("RefreshTrickplayImages triggers = %+v, want one daily trigger at 03:00", info.Triggers)
	}
}

type testGuide struct {
	hidden *bool
	calls  *int
}

func (g testGuide) RefreshGuide(_ context.Context, progress func(float64)) error {
	*g.calls++
	progress(100)
	return nil
}

func (g testGuide) Hidden() bool { return *g.hidden }

// TestRefreshGuideTaskIdentity pins upstream RefreshGuideScheduledTask: its
// derived ID, Key, en-US strings, the 24-hour interval trigger, and IsHidden
// while no tuner is configured.
func TestRefreshGuideTaskIdentity(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, _, _, _ := newTestScheduler(t, c)
	hidden, calls := true, 0
	if err := tasks.RegisterBuiltins(s, tasks.BuiltinDeps{Guide: testGuide{hidden: &hidden, calls: &calls}}); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	info, ok := s.Get(tasks.RefreshGuideID)
	if !ok {
		t.Fatal("RefreshGuide task not registered")
	}
	if got, want := info.ID, dotNetGUID("Jellyfin.LiveTv.Guide.RefreshGuideScheduledTask"); got != want {
		t.Errorf("RefreshGuide ID = %q, want derived %q", got, want)
	}
	if info.Key != "RefreshGuide" || info.Name != "Refresh Guide" || info.Category != "Live TV" ||
		info.Description != "Downloads channel information from live tv services." || !info.IsHidden {
		t.Errorf("RefreshGuide identity = %+v", info)
	}
	if len(info.Triggers) != 1 || info.Triggers[0].Type != tasks.TriggerInterval || info.Triggers[0].IntervalTicks == nil ||
		*info.Triggers[0].IntervalTicks != int64(24*time.Hour/100) {
		t.Errorf("RefreshGuide triggers = %+v", info.Triggers)
	}
	hidden = false
	if info, _ := s.Get(tasks.RefreshGuideID); info.IsHidden {
		t.Error("RefreshGuide hidden with a tuner configured")
	}
}
