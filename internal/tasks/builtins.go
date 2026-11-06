// SPDX-License-Identifier: GPL-2.0-only

package tasks

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

// Ticks constants for default triggers. 1 .NET tick = 100ns; 1 hour = 3.6e10 ticks.
const (
	ticksPerHour = int64(36 * 1e9) // 3.6e10 ticks = 1 hour
	ticksPerDay  = 24 * ticksPerHour
)

// Stock task IDs — MD5(UTF-16LE(C# type FullName)) via .NET Guid, rendered "N"
// lowercase. These are the upstream constants verified this session.
// Computed from the C# type names; a unit test re-derives them for provenance.
const (
	RefreshLibraryID         = "7738148ffcd07979c7ceb148e06b3aed"
	RefreshPeopleID          = "866456ed0d44e15468124ce33d85961e"
	RefreshChapterImagesID   = "4e6637c832ed644d1af3370a2506e80a"
	AudioNormalizationID     = "ec2f221fd8e7706b3d3afd2c4591b4d7"
	MediaSegmentExtractionID = "f861734dd71b37f9482b52a820e39013"
	OptimizeDatabaseID       = "31de9ce83b9223d338c77b1a635e144b"
	CleanActivityLogID       = "b461ef918ab28520928183e794350e3c"
	CleanupUserDataID        = "8240e309407dbbf43c99d30a6cbd239e"
	DeleteLogFilesID         = "1c8ede62c521bea0bf851344f5b8ca40"
	DeleteCacheFilesID       = "241d4fcb19a1d557ee62428e411da609"
	DeleteTranscodeFilesID   = "7d8088c10902f1bf4072ded42437bcfb"
	SubtitleScheduledID      = "2c66a88bca43e565d7f8099f825478f1"
	RefreshMetadataID        = "258f662348a5e8ecfbf59f319bfa549b"
	// RefreshTrickplayImagesID is upstream
	// MediaBrowser.Providers.Trickplay.TrickplayImagesTask.
	RefreshTrickplayImagesID = "64f5f44cd30dc273cb9890205473bbcc"
	// RefreshAnimeSeedsID is the Deadfin anime seed task,
	// derived the stock way from "Deadfin.Tasks.RefreshAnimeSeedsTask".
	RefreshAnimeSeedsID = "edc2ff3797c351ef469e4ecbb1667ce1"
	// RefreshGuideID is upstream Jellyfin.LiveTv.Guide.RefreshGuideScheduledTask.
	RefreshGuideID = "bea9b218c97bbf98c5dc1303bdb9a0ca"
)

// Destructive task retention constants (upstream defaults).
const (
	activityRetentionDays  = 30
	logRetentionDays       = 3
	cacheRetentionDays     = 30
	transcodeRetentionDays = 1
)

// ScanLibrary is the synchronous, context-aware library scan seam
// (scanner.Scan). NOT Trigger(), which detaches from scheduler cancellation.
type ScanLibrary interface {
	Scan(context.Context, func(float64)) (any, error) // any = RefreshResult; kept loose to avoid importing library
}

// LibraryScanRunning reports whether a library scan is in progress. It is the
// scheduler's IsRunning(RefreshLibraryID) so OptimizeDatabase can skip while
// a scan runs (upstream OptimizeDatabaseTask.cs:74-94).
type LibraryScanRunning interface {
	IsRunning(id string) bool
}

// StoreMaintainer runs SQLite maintenance on the writer.
type StoreMaintainer interface {
	Maintain(ctx context.Context) error
}

// ActivityLogPruner prunes entries older than a cut time.
type ActivityLogPruner interface {
	Prune(ctx context.Context, before time.Time) (int, error)
}

// TranscodeCleaner cleans up inactive transcode outputs.
type TranscodeCleaner interface {
	CleanupInactive(ctx context.Context, olderThan time.Time) error
}

// UserDataCleaner deletes orphaned user-data rows (upstream CleanupUserDataTask).
type UserDataCleaner interface {
	DeleteDetachedUserData(ctx context.Context) (int64, error)
}

// MetadataRefresher backfills provider IDs and remote artwork for existing
// libraries without a rescan. It is the seam behind the manual RefreshMetadata
// scheduled task.
type MetadataRefresher interface {
	RefreshMetadata(context.Context, func(float64)) error
}

// PeopleRefresher refreshes the metadata of people that have no overview or
// no Primary image (PeopleValidationTask.RefreshPeopleImagesAsync).
type PeopleRefresher interface {
	Run(context.Context, func(float64)) error
}

// AnimeSeedRefresher downloads the offline anime datasets (at most once a
// day per source) and rebuilds the anime crosswalk.
type AnimeSeedRefresher interface {
	Refresh(context.Context, func(float64)) error
}

// TrickplayRefresher runs one "Generate Trickplay Images" pass over every
// library with trickplay extraction enabled (upstream TrickplayImagesTask).
type TrickplayRefresher interface {
	Run(context.Context, func(float64)) error
}

// GuideRefresher runs one Live TV guide refresh (upstream
// RefreshGuideScheduledTask). Hidden reports that no tuner is configured, so
// the task is hidden, as upstream IsHidden is.
type GuideRefresher interface {
	RefreshGuide(context.Context, func(float64)) error
	Hidden() bool
}

// BuiltinDeps carries the composition-root-injected dependencies for built-in
// tasks. internal/tasks imports neither library, store, nor ffmpeg (no
type BuiltinDeps struct {
	ScanLibrary        ScanLibrary
	LibraryScanRunning LibraryScanRunning
	StoreMaintainer    StoreMaintainer
	ActivityLogPruner  ActivityLogPruner
	// ActivityLogRetentionDays reads ServerConfiguration.ActivityLogRetentionDays
	// at run time (CleanActivityLogTask.cs).
	ActivityLogRetentionDays func() *int
	TranscodeCleaner         TranscodeCleaner
	UserDataCleaner          UserDataCleaner
	MetadataRefresher        MetadataRefresher
	People                   PeopleRefresher
	AnimeSeeds               AnimeSeedRefresher
	Trickplay                TrickplayRefresher
	Guide                    GuideRefresher
	Clock                    clock.Clock // injected clock for retention cutoffs (spec §3: all timing through clock.Clock)
	LogPath                  string
	CachePath                string
	TranscodePath            string // the manager-owned transcodes dir
}

// now returns the current time from the injected clock, or the real clock when
// no clock was injected (production wiring always injects one; tests inject a
// fake clock to make retention cutoffs deterministic — spec §3).
func (d BuiltinDeps) now() time.Time {
	if d.Clock != nil {
		return d.Clock.Now()
	}
	return clock.NewReal().Now()
}

// noopRunnable returns nil immediately. Used for honest stubs.
type noopRunnable struct{}

func (noopRunnable) Run(_ context.Context, _ func(float64)) error { return nil }

// RegisterBuiltins registers the 12 stock Phase-A tasks (7 real, 5 honest
// stubs) plus the Deadfin RefreshMetadata backfill when a MetadataRefresher
// dependency is present. Stock tasks always register so the task shape
// matches upstream Jellyfin. RefreshMetadata is a Deadfin extension, so it
// registers only when the composition root supplies a refresher; without
// one, the task count stays at the stock 12.
// The stock RefreshTrickplayImages task registers when a TrickplayRefresher
// is present: the composition root supplies one only when it can compose
// tiles. KeyframeExtraction is not registered.
func RegisterBuiltins(s *Scheduler, deps BuiltinDeps) error {
	defaults := defaultTriggers()
	runnables := builtinsRunnables(deps)

	defs := phaseATaskDefinitions()
	if deps.MetadataRefresher != nil {
		defs = append(defs, refreshMetadataTaskDef())
	}
	if deps.AnimeSeeds != nil {
		defs = append(defs, refreshAnimeSeedsTaskDef())
	}
	if deps.Trickplay != nil {
		defs = append(defs, refreshTrickplayImagesTaskDef())
	}
	if deps.Guide != nil {
		defs = append(defs, refreshGuideTaskDef())
	}
	for _, def := range defs {
		run, ok := runnables[def.ID]
		if !ok {
			run = noopRunnable{}
		}
		if def.ID == RefreshPeopleID && ok {
			// PeopleValidationTask.IsHidden is false. Only the stub hides.
			def.Hidden = false
		}
		d := Definition{
			ID:              def.ID,
			Key:             def.Key,
			Name:            def.Name,
			Description:     def.Description,
			Category:        def.Category,
			Hidden:          def.Hidden,
			DefaultTriggers: defaults[def.ID],
		}
		if def.ID == RefreshGuideID {
			d.HiddenFunc = deps.Guide.Hidden
		}
		if err := s.Register(d, run); err != nil {
			return err
		}
	}
	return nil
}

// taskDef holds the static identity fields for one stock task.
type taskDef struct {
	ID, Key, Name, Description, Category string
	Hidden                               bool
}

func phaseATaskDefinitions() []taskDef {
	return []taskDef{
		{ID: RefreshLibraryID, Key: "RefreshLibrary", Name: "Scan Media Library", Description: "Scans all media libraries and refreshes metadata.", Category: "Library"},
		{ID: RefreshPeopleID, Key: "RefreshPeople", Name: "Refresh People", Description: "Updates person metadata.", Category: "Library", Hidden: true},
		{ID: RefreshChapterImagesID, Key: "RefreshChapterImages", Name: "Refresh Chapter Images", Description: "Creates images for chapters.", Category: "Library", Hidden: true},
		{ID: AudioNormalizationID, Key: "AudioNormalization", Name: "Normalize Audio", Description: "Normalizes audio levels.", Category: "Encoding", Hidden: true},
		{ID: MediaSegmentExtractionID, Key: "TaskExtractMediaSegments", Name: "Extract Media Segments", Description: "Extracts media segments.", Category: "Media Segments"},
		{ID: OptimizeDatabaseID, Key: "OptimizeDatabaseTask", Name: "Optimize Database", Description: "Optimizes the database.", Category: "Maintenance"},
		{ID: CleanActivityLogID, Key: "CleanActivityLog", Name: "Clean Activity Log", Description: "Deletes activity log entries older than 30 days.", Category: "Maintenance"},
		{ID: CleanupUserDataID, Key: "CleanupUserDataTask", Name: "Clean UpUserData", Description: "Removes orphaned user data.", Category: "Maintenance", Hidden: true},
		{ID: DeleteLogFilesID, Key: "CleanLogFiles", Name: "Delete Log Files", Description: "Deletes log files older than 3 days.", Category: "Maintenance"},
		{ID: DeleteCacheFilesID, Key: "DeleteCacheFiles", Name: "Delete Cache Files", Description: "Deletes cache files older than 30 days.", Category: "Maintenance"},
		{ID: DeleteTranscodeFilesID, Key: "DeleteTranscodeFiles", Name: "Delete Transcode Files", Description: "Deletes inactive transcode files older than 1 day.", Category: "Maintenance"},
		{ID: SubtitleScheduledID, Key: "DownloadSubtitles", Name: "Download Subtitles", Description: "Downloads missing subtitles.", Category: "Subtitles"},
	}
}

// refreshMetadataTaskDef returns the Deadfin RefreshMetadata extension task
// definition. It is not a stock Jellyfin built-in, so it is registered
// separately from the 12 Phase-A tasks and only when a MetadataRefresher
// dependency is present.
func refreshMetadataTaskDef() taskDef {
	return taskDef{ID: RefreshMetadataID, Key: "RefreshMetadata", Name: "Refresh Metadata", Description: "Backfills provider IDs and remote artwork for existing libraries.", Category: "Library"}
}

// refreshTrickplayImagesTaskDef returns the upstream TrickplayImagesTask
// identity: TrickplayImagesTask.cs Key, and the en-US
// TaskRefreshTrickplayImages / TasksLibraryCategory strings.
func refreshTrickplayImagesTaskDef() taskDef {
	return taskDef{ID: RefreshTrickplayImagesID, Key: "RefreshTrickplayImages", Name: "Generate Trickplay Images", Description: "Creates trickplay previews for videos in enabled libraries.", Category: "Library"}
}

// refreshAnimeSeedsTaskDef returns the Deadfin anime seed task definition.
// Like RefreshMetadata it is an extension task and registers only with its
// dependency.
func refreshAnimeSeedsTaskDef() taskDef {
	return taskDef{ID: RefreshAnimeSeedsID, Key: "RefreshAnimeSeeds", Name: "Refresh Anime Seed Data", Description: "Downloads the offline anime datasets (manami, AniDB titles, Anime-Lists) at most once a day and rebuilds the anime id crosswalk.", Category: "Library"}
}

// refreshGuideTaskDef returns the upstream RefreshGuideScheduledTask
// identity (RefreshGuideScheduledTask.cs). It registers only with a guide
// refresher.
func refreshGuideTaskDef() taskDef {
	return taskDef{ID: RefreshGuideID, Key: "RefreshGuide", Name: "Refresh Guide", Description: "Downloads channel information from live tv services.", Category: "Live TV"}
}

// defaultTriggers returns the upstream default triggers per task ID. Every
// stock task retains its upstream default triggers, including the honest
// stubs — so the registered shape matches stock Jellyfin even where the
// backend is a no-op (spec §4: stubs retain "stock ... default triggers").
func defaultTriggers() map[string][]TriggerInfo {
	interval := func(ticks int64) *int64 { return &ticks }
	ticks := func(d time.Duration) *int64 { t := d.Nanoseconds() / 100; return &t }
	return map[string][]TriggerInfo{
		RefreshLibraryID:       {{Type: TriggerInterval, IntervalTicks: interval(12 * ticksPerHour)}},
		OptimizeDatabaseID:     {{Type: TriggerInterval, IntervalTicks: interval(6 * ticksPerHour)}},
		DeleteLogFilesID:       {{Type: TriggerInterval, IntervalTicks: interval(ticksPerDay)}},
		DeleteCacheFilesID:     {{Type: TriggerInterval, IntervalTicks: interval(ticksPerDay)}},
		DeleteTranscodeFilesID: {{Type: TriggerStartup}, {Type: TriggerInterval, IntervalTicks: interval(ticksPerDay)}},
		// The seed fetcher itself enforces one request per source per day;
		// the startup trigger loads the cached crosswalk early.
		RefreshAnimeSeedsID:      {{Type: TriggerStartup}, {Type: TriggerInterval, IntervalTicks: interval(ticksPerDay)}},
		RefreshTrickplayImagesID: {{Type: TriggerDaily, TimeOfDayTicks: ticks(3 * time.Hour)}},    // TrickplayImagesTask.cs GetDefaultTriggers
		RefreshGuideID:           {{Type: TriggerInterval, IntervalTicks: interval(ticksPerDay)}}, // RefreshGuideScheduledTask.cs GetDefaultTriggers
		// Honest stubs (5) — upstream default triggers retained so the
		// registered task shape matches stock (the backend is a no-op).
		RefreshPeopleID:          {{Type: TriggerInterval, IntervalTicks: interval(7 * ticksPerDay)}},                                 // PeopleValidationTask.cs:86-89
		RefreshChapterImagesID:   {{Type: TriggerDaily, TimeOfDayTicks: ticks(2 * time.Hour), MaxRuntimeTicks: ticks(4 * time.Hour)}}, // ChapterImagesTask.cs:74-78
		AudioNormalizationID:     {{Type: TriggerInterval, IntervalTicks: interval(ticksPerDay)}},                                     // AudioNormalizationTask.cs:221-224
		MediaSegmentExtractionID: {{Type: TriggerInterval, IntervalTicks: interval(12 * ticksPerHour)}},                               // MediaSegmentExtractionTask.cs:114-117
		SubtitleScheduledID:      {{Type: TriggerInterval, IntervalTicks: interval(ticksPerDay)}},                                     // SubtitleScheduledTask.cs:216
		// CleanActivityLog and CleanupUserData have no default triggers
		// upstream (CleanActivityLogTask.cs:72 returns []; CleanupUserDataTask
		// yield break), so they are absent here.
	}
}

// builtinsRunnables maps task IDs to their real runnable implementations.
// Honest stubs (5) are not in this map and get noopRunnable.
func builtinsRunnables(deps BuiltinDeps) map[string]Runnable {
	m := make(map[string]Runnable)

	if deps.ScanLibrary != nil {
		m[RefreshLibraryID] = &refreshLibraryRunnable{deps: deps}
	}
	if deps.MetadataRefresher != nil {
		m[RefreshMetadataID] = &refreshMetadataRunnable{deps: deps}
	}
	if deps.People != nil {
		m[RefreshPeopleID] = &refreshPeopleRunnable{deps: deps}
	}
	if deps.AnimeSeeds != nil {
		m[RefreshAnimeSeedsID] = &refreshAnimeSeedsRunnable{deps: deps}
	}
	if deps.Trickplay != nil {
		m[RefreshTrickplayImagesID] = deps.Trickplay
	}
	if deps.Guide != nil {
		m[RefreshGuideID] = guideRunnable{deps.Guide}
	}
	if deps.StoreMaintainer != nil {
		m[OptimizeDatabaseID] = &optimizeDatabaseRunnable{deps: deps}
	}
	if deps.ActivityLogPruner != nil {
		m[CleanActivityLogID] = &cleanActivityLogRunnable{deps: deps}
	}
	if deps.LogPath != "" {
		m[DeleteLogFilesID] = &deleteLogFilesRunnable{deps: deps}
	}
	if deps.CachePath != "" {
		m[DeleteCacheFilesID] = &deleteCacheFilesRunnable{deps: deps}
	}
	if deps.TranscodeCleaner != nil {
		m[DeleteTranscodeFilesID] = &deleteTranscodeRunnable{deps: deps}
	}
	if deps.UserDataCleaner != nil {
		m[CleanupUserDataID] = &cleanupUserDataRunnable{deps: deps}
	}

	return m
}

// --- Real runnables ---

type refreshLibraryRunnable struct{ deps BuiltinDeps }

func (r *refreshLibraryRunnable) Run(ctx context.Context, progress func(float64)) error {
	progress(0)
	if _, err := r.deps.ScanLibrary.Scan(ctx, progress); err != nil {
		return err
	}
	progress(100)
	return nil
}

type refreshMetadataRunnable struct{ deps BuiltinDeps }

func (r *refreshMetadataRunnable) Run(ctx context.Context, progress func(float64)) error {
	progress(0)
	if err := r.deps.MetadataRefresher.RefreshMetadata(ctx, progress); err != nil {
		return err
	}
	progress(100)
	return nil
}

// refreshPeopleRunnable ports PeopleValidationTask.ExecuteAsync. Upstream
// defers the task while a library scan runs; the task runs again on its next
// trigger.
type refreshPeopleRunnable struct{ deps BuiltinDeps }

func (r *refreshPeopleRunnable) Run(ctx context.Context, progress func(float64)) error {
	if r.deps.LibraryScanRunning != nil && r.deps.LibraryScanRunning.IsRunning(RefreshLibraryID) {
		log.Printf("tasks: RefreshPeople skipped: library scan in progress")
		return nil
	}
	return r.deps.People.Run(ctx, progress)
}

type refreshAnimeSeedsRunnable struct{ deps BuiltinDeps }

func (r *refreshAnimeSeedsRunnable) Run(ctx context.Context, progress func(float64)) error {
	progress(0)
	return r.deps.AnimeSeeds.Refresh(ctx, progress)
}

type guideRunnable struct{ GuideRefresher }

func (r guideRunnable) Run(ctx context.Context, progress func(float64)) error {
	return r.RefreshGuide(ctx, progress)
}

type optimizeDatabaseRunnable struct{ deps BuiltinDeps }

func (r *optimizeDatabaseRunnable) Run(ctx context.Context, progress func(float64)) error {
	if r.deps.LibraryScanRunning != nil && r.deps.LibraryScanRunning.IsRunning(RefreshLibraryID) {
		log.Printf("tasks: OptimizeDatabase skipped: library scan in progress")
		return nil
	}
	progress(0)
	if err := r.deps.StoreMaintainer.Maintain(ctx); err != nil {
		return err
	}
	progress(100)
	return nil
}

type cleanActivityLogRunnable struct{ deps BuiltinDeps }

func (r *cleanActivityLogRunnable) Run(ctx context.Context, progress func(float64)) error {
	progress(0)
	retentionDays := activityRetentionDays
	if r.deps.ActivityLogRetentionDays != nil {
		configured := r.deps.ActivityLogRetentionDays()
		if configured == nil || *configured < 0 {
			value := "null"
			if configured != nil {
				value = strconv.Itoa(*configured)
			}
			return fmt.Errorf("activity log retention days must be at least 0; currently %s", value)
		}
		retentionDays = *configured
	}
	before := r.deps.now().AddDate(0, 0, -retentionDays)
	if _, err := r.deps.ActivityLogPruner.Prune(ctx, before); err != nil {
		return err
	}
	progress(100)
	return nil
}

type deleteLogFilesRunnable struct{ deps BuiltinDeps }

func (r *deleteLogFilesRunnable) Run(ctx context.Context, progress func(float64)) error {
	olderThan := r.deps.now().AddDate(0, 0, -logRetentionDays)
	return fsclean(ctx, fscleanOptions{
		Root:      r.deps.LogPath,
		OlderThan: olderThan,
		skipName:  func(name string) bool { return len(name) >= 4 && name[:4] == "log_" },
		Progress:  progress,
	})
}

type deleteCacheFilesRunnable struct{ deps BuiltinDeps }

func (r *deleteCacheFilesRunnable) Run(ctx context.Context, progress func(float64)) error {
	olderThan := r.deps.now().AddDate(0, 0, -cacheRetentionDays)
	if err := fsclean(ctx, fscleanOptions{
		Root:      r.deps.CachePath,
		OlderThan: olderThan,
		Exclude:   r.deps.TranscodePath,
		Progress:  progress,
	}); err != nil {
		return err
	}
	// Empty-dir removal must exclude the transcodes subtree too: the
	// manager-owned transcodes root and its directories must survive
	// (spec §4: exclude the paths.Transcodes subtree owned by TranscodeManager).
	return fscleanRemoveEmptyDirs(ctx, r.deps.CachePath, r.deps.TranscodePath)
}

type deleteTranscodeRunnable struct{ deps BuiltinDeps }

func (r *deleteTranscodeRunnable) Run(ctx context.Context, progress func(float64)) error {
	olderThan := r.deps.now().AddDate(0, 0, -transcodeRetentionDays)
	return r.deps.TranscodeCleaner.CleanupInactive(ctx, olderThan)
}

type cleanupUserDataRunnable struct{ deps BuiltinDeps }

func (r *cleanupUserDataRunnable) Run(ctx context.Context, progress func(float64)) error {
	progress(0)
	if _, err := r.deps.UserDataCleaner.DeleteDetachedUserData(ctx); err != nil {
		return err
	}
	progress(100)
	return nil
}

// Assert noopRunnable satisfies Runnable.
var _ Runnable = noopRunnable{}
var _ Runnable = (*refreshLibraryRunnable)(nil)
var _ Runnable = (*refreshMetadataRunnable)(nil)
