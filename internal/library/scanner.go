// SPDX-License-Identifier: GPL-2.0-only

package library

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
)

var ErrScannerClosed = errors.New("library scanner is closed")

var errRefreshMetadataNoInvocation = errors.New("refresh metadata: the remote stage made no provider invocation")

var errRefreshMetadataNoPersistence = errors.New("refresh metadata: the remote stage persisted no provider ID or image")

// RefresherStructuralMutationError reports an enrichment provider attempting to
// change resolver-owned filesystem identity.
type RefresherStructuralMutationError struct{ Field string }

func (e *RefresherStructuralMutationError) Error() string {
	return "provider refresh changed structural field " + e.Field
}

// ScannerOptions controls one scanner instance. ReadDir is an intentionally
// narrow filesystem seam: a nil value reads the operating system filesystem.
type ScannerOptions struct {
	Concurrency     int
	Progress        func(RefreshProgress)
	RefreshComplete func(RefreshResult)
	ReadDir         func(string) ([]Entry, error)
	Now             func() time.Time
	Clock           clock.Clock
	Refresher       Refresher
	MediaProbe      MediaProbe
	Logger          *slog.Logger
	// CollectionsPath is <data>/collections, the media path of the
	// Collections library (CollectionManager.GetCollectionsFolderPath). Empty
	// disables CreateCollection and the post-scan auto-collection step.
	CollectionsPath string
	// MetadataPath is the internal metadata root (config.Paths.Metadata). A
	// scan reads subtitle files from each video's internal metadata
	// directory, where uploaded subtitles go. Empty skips that directory.
	MetadataPath string
	// DynamicImages runs the Screen Grabber and the Embedded Image Extractor
	// for videos on their first refresh. Nil disables them.
	DynamicImages DynamicImageRefresher
}

// FolderRefreshStatus reports one folder's scan lifecycle phase. The wire
// spellings match stock Jellyfin: Active, Queued, and Idle.
type FolderRefreshStatus string

const (
	// FolderRefreshActive means a scan is in progress for this folder.
	FolderRefreshActive FolderRefreshStatus = "Active"
	// FolderRefreshQueued means the folder waits for a scan to reach it.
	FolderRefreshQueued FolderRefreshStatus = "Queued"
	// FolderRefreshIdle means no scan covers this folder.
	FolderRefreshIdle FolderRefreshStatus = "Idle"
)

// FolderRefreshState is the live per-folder scan progress snapshot. Done and
// Outstanding count completed and remaining scan units. Progress is
// 100*done/(done+outstanding), clamped monotonically.
type FolderRefreshState struct {
	Status      FolderRefreshStatus
	Done        int
	Outstanding int
	Progress    float64
}

// RefreshProgress reports a completed unit of scan work. Status is one of
// "started", "scanned", "inaccessible", "failed", "skipped", or "completed".
type RefreshProgress struct {
	ItemID          dto.UUID
	Path            string
	Status          string
	Progress        float64
	Discovered      int
	Inserted        int
	Updated         int
	Deleted         int
	BackfillUpdated int
	BackfillSkipped int
	Error           error
}

// RefreshResult is the aggregate outcome of a scan. Scan returns a non-nil
// error only for cancellation or a topology-wide failure; individual folder
// failures are retained in Errors and do not prevent independent siblings.
//
// ProviderInvocations counts the calls to the injected refresher at the
// refresher boundary, one per refreshed item. PersistedProviderIDs and
// PersistedImages count items the reconcile path accepted with a new or
// changed provider ID or remote image. A metadata-refresh task uses these
// counts to reject a false-success result that did no provider work.
//
// BackfillUpdated and BackfillSkipped count stored items the remote-backfill
// partition sent to MetadataBackfill. BackfillUpdated counts items the
// backfill changed; BackfillSkipped counts items the backfill did not change.
// Absent items that inserted through ordinary reconciliation do not count
// here. The partition at reconcileBackfill is the only code that separates
// stored from absent candidates.
type RefreshResult struct {
	Discovered                 int
	Inserted                   int
	Updated                    int
	Deleted                    int
	BackfillUpdated            int
	BackfillSkipped            int
	Scanned                    int
	Inaccessible               int
	RegenerateTrickplayItemIDs []dto.UUID
	ProviderInvocations        int
	PersistedProviderIDs       int
	PersistedImages            int
	Errors                     []error
}

// refreshCounts is the per-scan accumulator for provider invocation and
// provider-or-image persistence. resolveFolder increments invocations at
// the refresher boundary; the reconcile sites count an accepted provider ID
// or remote image. scanLocked copies the accumulator into the returned
// RefreshResult. The fields are atomic because concurrent scanFolder workers
// increment them in parallel.
type refreshCounts struct {
	invocations       atomic.Int64
	persistedProvider atomic.Int64
	persistedImages   atomic.Int64
}

// remoteImageEntry captures the type and path of one remote image so the
// reconcile site can detect a same-cardinality URL replacement.
type remoteImageEntry struct {
	imageType item.ImageType
	path      string
}

// refreshSatelliteSnapshot captures the provider ID entries and remote image
// entries for one item before reconcile, so the reconcile site can count an
// accepted change. A missing item (an insert) has a zero snapshot. Storing
// the values rather than their count lets the delta detect a replacement
// that keeps the same cardinality but changes the provider ID value or the
// remote image URL.
type refreshSatelliteSnapshot struct {
	providerIDs  []item.ProviderID
	remoteImages []remoteImageEntry
}

// snapshotRefreshSatellites captures the provider ID entries and remote image
// entries for one item.
func snapshotRefreshSatellites(value item.Item) refreshSatelliteSnapshot {
	return refreshSatelliteSnapshot{
		providerIDs:  value.ProviderIDs.Snapshot(),
		remoteImages: snapshotRemoteImages(value.ImageInfos),
	}
}

// snapshotRemoteImages returns the type and path of every remote image on an
// item. A local image keeps a filesystem path, so a remote https URL
// identifies the artwork a provider backfilled.
func snapshotRemoteImages(images []item.ImageInfo) []remoteImageEntry {
	var entries []remoteImageEntry
	for _, image := range images {
		if isRemoteImageURL(image.Path) {
			entries = append(entries, remoteImageEntry{imageType: image.Type, path: image.Path})
		}
	}
	return entries
}

// countPersistedSatellites counts the provider IDs and remote images the
// reconcile accepted. For every inserted or updated item, it compares the
// refreshed satellite values against the before snapshot and increments the
// per-scan accumulator when a provider ID or remote image is new or changed.
func (s *Scanner) countPersistedSatellites(mutation store.ReconcileChildrenResult, desired, virtuals []item.Item, before map[dto.UUID]refreshSatelliteSnapshot) {
	if s.counts == nil {
		return
	}
	after := make(map[dto.UUID]refreshSatelliteSnapshot, len(desired)+len(virtuals))
	for _, value := range desired {
		after[value.ID] = snapshotRefreshSatellites(value)
	}
	for _, value := range virtuals {
		after[value.ID] = snapshotRefreshSatellites(value)
	}
	for _, id := range mutation.Inserted {
		s.counts.persistedProvider.Add(int64(persistedProviderDelta(after[id], refreshSatelliteSnapshot{})))
		s.counts.persistedImages.Add(int64(persistedImageDelta(after[id], refreshSatelliteSnapshot{})))
	}
	for _, id := range mutation.Updated {
		s.counts.persistedProvider.Add(int64(persistedProviderDelta(after[id], before[id])))
		s.counts.persistedImages.Add(int64(persistedImageDelta(after[id], before[id])))
	}
}

// persistedProviderDelta returns the number of provider ID entries in the
// after snapshot that are absent from the before snapshot. A replacement that
// keeps the same count but changes the value of one entry counts as one.
func persistedProviderDelta(after, before refreshSatelliteSnapshot) int {
	if len(after.providerIDs) == 0 {
		return 0
	}
	seen := make(map[string]struct{}, len(before.providerIDs))
	for _, id := range before.providerIDs {
		seen[id.Name+"\x00"+id.Value] = struct{}{}
	}
	delta := 0
	for _, id := range after.providerIDs {
		if _, ok := seen[id.Name+"\x00"+id.Value]; !ok {
			delta++
		}
	}
	return delta
}

// persistedImageDelta returns the number of remote image entries in the
// after snapshot that are absent from the before snapshot. A replacement that
// keeps the same count but changes the URL of one image counts as one.
func persistedImageDelta(after, before refreshSatelliteSnapshot) int {
	if len(after.remoteImages) == 0 {
		return 0
	}
	seen := make(map[string]struct{}, len(before.remoteImages))
	for _, image := range before.remoteImages {
		seen[image.imageType.String()+"\x00"+image.path] = struct{}{}
	}
	delta := 0
	for _, image := range after.remoteImages {
		if _, ok := seen[image.imageType.String()+"\x00"+image.path]; !ok {
			delta++
		}
	}
	return delta
}

// isRemoteImageURL reports whether a path is an http or https URL.
func isRemoteImageURL(path string) bool {
	return strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://")
}

// progressTracker aggregates item-level scan progress across concurrent
// folder workers and serializes callback delivery. Each folder registers
// its discovered item count before refresh starts. Each item refresh
// advances the completed counter. The tracker computes completed divided
// by discovered, clamps the value below 100, and keeps the highest emitted
// value so progress never decreases. A nil callback makes every method a
// no-op so plain Scan and ScanWithOptions keep their behavior.
type progressTracker struct {
	mu         sync.Mutex
	discovered int
	completed  int
	highest    float64
	callback   func(float64)
}

func newProgressTracker(callback func(float64)) *progressTracker {
	return &progressTracker{callback: callback}
}

// register adds n items to the discovered denominator. Concurrent folder
// workers call this before any item refresh starts in that folder.
func (t *progressTracker) register(n int) {
	if t == nil || t.callback == nil || n <= 0 {
		return
	}
	t.mu.Lock()
	t.discovered += n
	t.mu.Unlock()
}

// complete advances the completed counter by n and emits the current
// progress fraction. The emitted value is clamped below 100 and never
// decreases. The mutex stays held through the callback so concurrent
// workers cannot overlap callback delivery. Releasing the mutex before
// the callback lets two workers call the callback at the same time.
func (t *progressTracker) complete(n int) {
	if t == nil || t.callback == nil || n <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.completed += n
	value := t.emitLocked()
	t.callback(value)
}

// emitLocked computes the current progress fraction, clamps it below 100,
// keeps the highest emitted value, and returns the value to emit. The
// caller must hold t.mu.
func (t *progressTracker) emitLocked() float64 {
	if t.discovered == 0 {
		return 0
	}
	value := float64(t.completed) / float64(t.discovered) * 100
	if value >= 100 {
		value = 99.99
	}
	if value <= t.highest {
		return t.highest
	}
	t.highest = value
	return value
}

// Scanner coordinates filesystem enumeration, resolver selection, and the
// ItemStore's atomic direct-child reconciliation seam. It neither probes nor
// refreshes metadata and it does not own Topology or ItemStore lifetimes.
type Scanner struct {
	topology *Topology
	items    *store.ItemStore
	// writeLane admits one scanner store write at a time. See storeWrite.
	writeLane chan struct{}
	chain     *Chain
	options   ScannerOptions
	progress  *RefreshProgressBus
	changes   *ChangeBus

	mu           sync.Mutex
	closed       bool
	activeCancel context.CancelFunc
	active       int
	drained      chan struct{}
	closeOnce    sync.Once
	closeErr     error
	lastResult   RefreshResult
	lastErr      error
	hasResult    bool
	scanMu       sync.Mutex
	// pending is a depth-1, intent-upgrading queue for an async refresh that
	// arrived while a scan held scanMu. The running scan's goroutine drains it on
	// completion (see triggerLocked), so a requested full/replace refresh is
	// queued rather than silently dropped. Guarded by mu.
	pending     bool
	pendingOpts RefreshOptions
	// refreshStates holds the live per-folder scan progress keyed by
	// CollectionFolderID. Guarded by mu. The map has one writer because scans
	// serialize on scanMu.
	refreshStates map[dto.UUID]FolderRefreshState
	// taskProgress carries the scheduled-task progress callback that
	// ScanWithProgress installs. It is nil for plain Scan and ScanWithOptions
	// calls so the existing callers keep their behavior. The field is guarded
	// by mu and installed and cleared inside the scanMu gate by scanWithOptions,
	// so concurrent ScanWithProgress callers cannot race on the shared field.
	taskProgress func(float64)
	// tracker is the scanner-owned progress tracker for the active scan.
	// scanLocked creates it when taskProgress is non-nil and clears it on
	// return. Concurrent folder workers share the same tracker.
	tracker *progressTracker
	// counts is the per-scan accumulator for provider invocation and
	// provider-or-image persistence counts. scanLocked creates it before any
	// folder work and copies it into the returned RefreshResult. resolveFolder
	// counts a provider invocation at the refresher boundary; the reconcile
	// sites count an accepted provider ID or remote image.
	counts *refreshCounts
	// remoteBackfill is the per-scan flag for the metadata-refresh backfill
	// path. When true, resolveFolder skips media probing and the unrelated
	// local work that does not contribute to provider backfill. scanWith sets
	// it under mu inside the scanMu gate and clears it on return, so a plain
	// scan and a backfill scan cannot race on the shared field.
	remoteBackfill bool
	// logger records the structured scanner events for the manual
	// RefreshExistingMetadata plan. It is nil for the construction path that
	// does not inject a logger, so NewScanner substitutes a discard handler
	// and existing callers keep their behavior. Only the production
	// application wiring passes a live logger.
	logger *slog.Logger
}

// NewScanner constructs a scanner. A zero concurrency uses the available CPU
// parallelism; a negative concurrency is invalid.
func NewScanner(topology *Topology, items *store.ItemStore, chain *Chain, options ScannerOptions) (*Scanner, error) {
	if topology == nil {
		return nil, errors.New("library scanner topology is required")
	}
	if items == nil {
		return nil, errors.New("library scanner item store is required")
	}
	if chain == nil {
		return nil, errors.New("library scanner resolver chain is required")
	}
	if options.Concurrency < 0 {
		return nil, errors.New("library scanner concurrency cannot be negative")
	}
	if options.Concurrency == 0 {
		options.Concurrency = runtime.GOMAXPROCS(0)
		if options.Concurrency < 1 {
			options.Concurrency = 1
		}
	}
	if options.Clock == nil {
		options.Clock = clock.NewReal()
	}
	if options.Now == nil {
		options.Now = options.Clock.Now
	}
	logger := options.Logger
	if logger == nil {
		// Tests that do not inject a logger keep a discard handler so the
		// manual RefreshExistingMetadata warnings do not pollute the test
		// output. The production wiring in cmd/deadfin passes a live logger.
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if options.ReadDir == nil {
		options.ReadDir = func(path string) ([]Entry, error) {
			return readDirectoryReporting(path, func(entryPath string, err error) {
				logger.Warn("scanner: skip unreadable directory entry", "path", entryPath, "error", err)
			})
		}
	}
	drained := make(chan struct{})
	close(drained)
	return &Scanner{
		writeLane: make(chan struct{}, 1),
		topology:  topology,
		items:     items,
		chain:     chain,
		options:   options,
		progress:  newRefreshProgressBus(options.Clock),
		changes:   newChangeBus(options.Clock),
		logger:    logger,
		drained:   drained,
	}, nil
}

// Close cancels active scanner work and waits until all scanner-owned work has
// returned. It is safe to call repeatedly and deliberately leaves the injected
// topology, resolver chain, and stores open for their owners.
func (s *Scanner) Close() error {
	return s.CloseContext(context.Background())
}

// CloseContext begins scanner shutdown, cancelling active work, then waits for
// scanner-owned work only until ctx is done. Once shutdown begins the scanner
// remains closed even when ctx expires; the final worker closes its progress bus
// as soon as scanner work drains.
func (s *Scanner) CloseContext(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	if !s.closed {
		s.closed = true
		if s.activeCancel != nil {
			s.activeCancel()
		}
	}
	drained := s.drained
	s.mu.Unlock()

	// Prefer a completed shutdown to a concurrently cancelled context.
	select {
	case <-drained:
		return s.finishClose()
	default:
	}
	select {
	case <-drained:
		return s.finishClose()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// beginWork reserves scanner ownership before a caller can wait on scanMu.
// Close holds mu while sealing the scanner, so no work can be added afterward.
func (s *Scanner) beginWork() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrScannerClosed
	}
	if s.active == 0 {
		s.drained = make(chan struct{})
	}
	s.active++
	return nil
}

// endWork releases scanner ownership when scan work completes. The final worker
// closes the owned progress bus if shutdown has already begun.
func (s *Scanner) endWork() {
	s.mu.Lock()
	s.active--
	if s.active < 0 {
		s.mu.Unlock()
		panic("library scanner work accounting underflow")
	}
	finalize := s.closed && s.active == 0
	if s.active == 0 {
		close(s.drained)
	}
	s.mu.Unlock()
	if finalize {
		_ = s.finishClose()
	}
}

func (s *Scanner) finishClose() error {
	s.closeOnce.Do(func() {
		s.closeErr = errors.Join(
			s.progress.close(),
			s.changes.close(context.Background()),
		)
	})
	return s.closeErr
}

// Trigger starts one asynchronous scan owned by Scanner. A plain library refresh
// arriving while a scan is in flight coalesces into that scan (no queued repeat):
// the running scan already covers the default intent.
func (s *Scanner) Trigger() error {
	return s.startOrQueue(DefaultRefreshOptions(), false)
}

// TriggerRefresh starts a background scan under an explicit refresh intent. It
// is the library seam behind POST /Items/{itemId}/Refresh: a full/replace
// refresh threads RefreshModeFullRefresh so the remote stage re-runs on items
// that were already scanned. When a scan is already in flight the intent is
// queued and upgraded into that scan's follow-up rather than dropped, so a
// requested full/replace refresh always runs.
func (s *Scanner) TriggerRefresh(opts RefreshOptions) error {
	return s.startOrQueue(opts, true)
}

// startOrQueue owns the async trigger handoff. It acquires scanMu under mu so the
// draining goroutine (which releases scanMu under mu) and the queuers serialize:
// either we win the gate and start the scan, or a scan is in flight and — when
// queueOnBusy — we record the intent for that scan's goroutine to drain. Because
// scanMu is only taken and released by the async path while holding mu, there is
// no window in which a queued intent is recorded after the running goroutine has
// stopped checking for it.
func (s *Scanner) startOrQueue(opts RefreshOptions, queueOnBusy bool) error {
	if s == nil {
		return ErrScannerClosed
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrScannerClosed
	}
	if s.scanMu.TryLock() {
		// Gate free: we own the scan. Release mu before beginWork (which takes
		// mu); no other async trigger can seize scanMu meanwhile because they
		// TryLock it only under mu, which we still effectively gate by holding
		// scanMu itself.
		s.mu.Unlock()
		return s.triggerLocked(opts)
	}
	if queueOnBusy {
		if s.pending {
			s.pendingOpts = mergeRefreshOptions(s.pendingOpts, opts)
		} else {
			s.pending = true
			s.pendingOpts = cloneRefreshOptions(opts)
		}
	}
	s.mu.Unlock()
	return nil
}

// mergeRefreshOptions coalesces two queued intents into the stronger of the two:
// the higher metadata/image refresh mode, the union of the replace-all flags,
// and the stable first-seen union of requested trickplay regenerations. A
// full/replace refresh queued during a lighter scan therefore upgrades the
// follow-up rather than being weakened by it.
func mergeRefreshOptions(current, requested RefreshOptions) RefreshOptions {
	if requested.Metadata > current.Metadata {
		current.Metadata = requested.Metadata
	}
	if requested.Image > current.Image {
		current.Image = requested.Image
	}
	current.ReplaceAllMetadata = current.ReplaceAllMetadata || requested.ReplaceAllMetadata
	current.ReplaceAllImages = current.ReplaceAllImages || requested.ReplaceAllImages
	current.RegenerateTrickplayItemIDs = mergeRegenerateTrickplayItemIDs(current.RegenerateTrickplayItemIDs, requested.RegenerateTrickplayItemIDs)
	// An unscoped intent covers every library, so it absorbs any scope.
	if len(current.Targets) == 0 || len(requested.Targets) == 0 {
		current.Targets = nil
	} else {
		current.Targets = append(cloneRefreshSlice(current.Targets), requested.Targets...)
	}
	return current
}

func cloneRefreshOptions(options RefreshOptions) RefreshOptions {
	options.RegenerateTrickplayItemIDs = cloneRefreshSlice(options.RegenerateTrickplayItemIDs)
	options.Targets = cloneRefreshSlice(options.Targets)
	return options
}

// scopeFolders returns the virtual folders that contain the refresh targets.
// A folder contains a target when the target is its CollectionFolder, or when
// the target path is the folder path or lies under one of its media paths.
// This mirrors upstream ProviderManager.RefreshItem, which refreshes only the
// target's subtree. With no targets, or when a target is in no folder (for
// example the aggregate root), every folder stays in scope.
func scopeFolders(folders []VirtualFolder, targets []RefreshTarget) []VirtualFolder {
	if len(targets) == 0 {
		return folders
	}
	keep := make(map[int]struct{}, len(folders))
	for _, target := range targets {
		matched := false
		for index, folder := range folders {
			if folderContainsTarget(folder, target) {
				keep[index] = struct{}{}
				matched = true
			}
		}
		if !matched {
			return folders
		}
	}
	scoped := make([]VirtualFolder, 0, len(keep))
	for index, folder := range folders {
		if _, ok := keep[index]; ok {
			scoped = append(scoped, folder)
		}
	}
	return scoped
}

// libraryRefreshTargets keeps the targets that folder contains. A library
// that no target names was scanned only because a target matched no library
// (scopeFolders), so it drops the targets and takes the requested intent
// whole.
func libraryRefreshTargets(folder VirtualFolder, opts RefreshOptions) RefreshOptions {
	if len(opts.Targets) == 0 {
		return opts
	}
	kept := make([]RefreshTarget, 0, len(opts.Targets))
	for _, target := range opts.Targets {
		if folderContainsTarget(folder, target) {
			kept = append(kept, target)
		}
	}
	opts.Targets = nil
	if len(kept) != 0 {
		opts.Targets = kept
	}
	return opts
}

func folderContainsTarget(folder VirtualFolder, target RefreshTarget) bool {
	if target.ItemID == folder.CollectionFolderID {
		return true
	}
	if target.Path == "" {
		return false
	}
	if pathWithin(target.Path, folder.Path) {
		return true
	}
	for _, mediaPath := range folder.MediaPaths {
		if pathWithin(target.Path, mediaPath) {
			return true
		}
	}
	return false
}

// pathWithin reports whether path is base or lies under base.
func pathWithin(path, base string) bool {
	if base == "" {
		return false
	}
	path = normalizedEntryPath(path)
	base = normalizedEntryPath(base)
	if path == base {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(base, string(filepath.Separator))+string(filepath.Separator))
}

func mergeRegenerateTrickplayItemIDs(current, requested []dto.UUID) []dto.UUID {
	if current == nil && requested == nil {
		return nil
	}
	merged := make([]dto.UUID, 0, len(current)+len(requested))
	if len(current)+len(requested) == 0 {
		return merged
	}
	seen := make(map[dto.UUID]struct{}, len(current)+len(requested))
	for _, id := range current {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		merged = append(merged, id)
	}
	for _, id := range requested {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		merged = append(merged, id)
	}
	return merged
}

// triggerLocked launches the background scan goroutine. The caller must already
// hold scanMu (released when the scan and every queued follow-up complete) and
// must not hold s.mu. The goroutine drains the pending queue: when a refresh was
// queued while this scan ran, it runs one more scan under the coalesced intent
// before releasing scanMu, so a queued full/replace refresh is never lost.
func (s *Scanner) triggerLocked(opts RefreshOptions) error {
	opts = cloneRefreshOptions(opts)
	if err := s.beginWork(); err != nil {
		s.scanMu.Unlock()
		return err
	}

	go func() {
		type completion struct {
			result RefreshResult
			err    error
		}
		var progress []RefreshProgress
		var completions []completion
		for {
			result, batch, err := s.scanLocked(context.Background(), opts)
			progress = append(progress, batch...)
			completions = append(completions, completion{result: result, err: err})

			s.mu.Lock()
			if s.pending && !s.closed {
				opts = s.pendingOpts
				s.pending = false
				s.pendingOpts = RefreshOptions{}
				s.mu.Unlock()
				continue
			}
			// Release the gate under mu so a concurrent queuer either observed a
			// still-held gate (and recorded pending, which we just checked) or
			// finds it free — never a lost handoff. Drop any intent we will not
			// run (for example after Close) so it cannot leak to a later scan.
			s.pending = false
			s.pendingOpts = RefreshOptions{}
			s.clearRefreshStatesLocked()
			s.scanMu.Unlock()
			s.mu.Unlock()
			break
		}
		for _, finished := range completions {
			s.completeRefresh(finished.result, finished.err)
		}
		s.endWork()
		// Progress callbacks retain their historical post-drain semantics so a
		// callback may synchronously close the scanner without deadlocking.
		s.deliver(progress)
	}()
	return nil
}

// LastResult returns the most recently completed scan outcome. The result is a
// snapshot so callers cannot mutate Scanner's retained error list.
func (s *Scanner) LastResult() (RefreshResult, error, bool) {
	if s == nil {
		return RefreshResult{}, ErrScannerClosed, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasResult {
		return RefreshResult{}, nil, false
	}
	return cloneRefreshResult(s.lastResult), s.lastErr, true
}

// RefreshStates returns a copy of the live per-folder scan progress keyed by
// CollectionFolderID. It returns an empty map for a nil or closed scanner, or
// when no scan runs. Active takes precedence over Queued.
func (s *Scanner) RefreshStates() map[dto.UUID]FolderRefreshState {
	if s == nil {
		return map[dto.UUID]FolderRefreshState{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return map[dto.UUID]FolderRefreshState{}
	}
	if s.refreshStates == nil {
		return map[dto.UUID]FolderRefreshState{}
	}
	copy := make(map[dto.UUID]FolderRefreshState, len(s.refreshStates))
	for key, value := range s.refreshStates {
		copy[key] = value
	}
	return copy
}

// setRefreshStateLocked sets one folder's refresh state. Active takes
// precedence over Queued: a Queued state does not overwrite an Active one.
// The caller must hold s.mu.
func (s *Scanner) setRefreshStateLocked(id dto.UUID, state FolderRefreshState) {
	if id == (dto.UUID{}) {
		return
	}
	if existing, ok := s.refreshStates[id]; ok && existing.Status == FolderRefreshActive && state.Status == FolderRefreshQueued {
		return
	}
	if state.Status == FolderRefreshActive {
		total := state.Done + state.Outstanding
		if total > 0 {
			state.Progress = float64(state.Done) / float64(total) * 100
			if existing, ok := s.refreshStates[id]; ok && existing.Progress > state.Progress {
				state.Progress = existing.Progress
			}
		}
	}
	s.refreshStates[id] = state
}

// startRefreshFolder marks a folder Active with progress 0 and one outstanding
// unit (the root scan unit).
func (s *Scanner) startRefreshFolder(id dto.UUID) {
	s.mu.Lock()
	s.setRefreshStateLocked(id, FolderRefreshState{Status: FolderRefreshActive, Outstanding: 1})
	s.mu.Unlock()
}

// completeRefreshRootUnit completes the root scan unit for a folder. On
// success it adds the source-task units. On failure it deletes the entry when
// no units remain.
func (s *Scanner) completeRefreshRootUnit(id dto.UUID, success bool, sourceTasks int) {
	if id == (dto.UUID{}) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshStates == nil {
		return
	}
	state, ok := s.refreshStates[id]
	if !ok {
		return
	}
	state.Outstanding--
	if state.Outstanding < 0 {
		state.Outstanding = 0
	}
	state.Done++
	if success {
		state.Outstanding += sourceTasks
		state.Status = FolderRefreshActive
	} else {
		if state.Outstanding == 0 {
			delete(s.refreshStates, id)
			return
		}
	}
	state.Progress = refreshProgress(state.Done, state.Outstanding)
	if existing, ok := s.refreshStates[id]; ok && existing.Progress > state.Progress {
		state.Progress = existing.Progress
	}
	s.refreshStates[id] = state
}

// completeRefreshUnit processes one scan-queue outcome. It completes one
// outstanding unit, adds child units for scanned outcomes, and deletes the
// entry when it reaches zero outstanding.
func (s *Scanner) completeRefreshUnit(id dto.UUID, status string, children int) {
	if id == (dto.UUID{}) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshStates == nil {
		return
	}
	state, ok := s.refreshStates[id]
	if !ok {
		return
	}
	state.Outstanding--
	if state.Outstanding < 0 {
		state.Outstanding = 0
	}
	state.Done++
	if status == "scanned" {
		state.Outstanding += children
	}
	if state.Outstanding == 0 {
		delete(s.refreshStates, id)
		return
	}
	state.Progress = refreshProgress(state.Done, state.Outstanding)
	if existing, ok := s.refreshStates[id]; ok && existing.Progress > state.Progress {
		state.Progress = existing.Progress
	}
	s.refreshStates[id] = state
}

// clearRefreshStates clears all refresh state. The caller must hold s.mu.
func (s *Scanner) clearRefreshStatesLocked() {
	s.refreshStates = nil
}

func refreshProgress(done, outstanding int) float64 {
	total := done + outstanding
	if total <= 0 {
		return 0
	}
	return float64(done) / float64(total) * 100
}

func cloneRefreshResult(result RefreshResult) RefreshResult {
	result.RegenerateTrickplayItemIDs = cloneRefreshSlice(result.RegenerateTrickplayItemIDs)
	result.Errors = cloneRefreshSlice(result.Errors)
	return result
}

// completeRefresh invokes a successful scan result's process-owned completion
// callback. The callback is scanner-owned work: it completes before scanner
// drain is signaled, so consumers may safely schedule follow-up work through
// their own lifecycle owner.
func (s *Scanner) completeRefresh(result RefreshResult, err error) {
	if err == nil && s.options.RefreshComplete != nil {
		s.options.RefreshComplete(cloneRefreshResult(result))
	}
}

// Scan scans every configured virtual folder and its configured source roots
// under the default refresh intent. Concurrent Scan calls are serialized to keep
// reconciliation and progress ordering deterministic.
func (s *Scanner) Scan(ctx context.Context) (RefreshResult, error) {
	return s.ScanWithOptions(ctx, DefaultRefreshOptions())
}

// ScanWithOptions scans every configured virtual folder and its source roots
// under an explicit refresh intent, synchronously. A full/replace intent
// (RefreshModeFullRefresh or a replace-all flag) re-runs the remote metadata
// stage on items that were already scanned, backfilling their provider ids and
// artwork. It shares Scan's serialization.
func (s *Scanner) ScanWithOptions(ctx context.Context, opts RefreshOptions) (RefreshResult, error) {
	return s.scanWithOptions(ctx, opts, nil)
}

// ScanWithProgress scans under an explicit refresh intent and reports
// aggregate scheduled-task progress through the callback. It installs the
// callback on the scanner so scanLocked can create a progress tracker that
// concurrent folder workers share. The callback receives values in (0,100)
// while items are processed; the runnable calls progress(0) before and
// progress(100) after the scan. Scan and ScanWithOptions are unchanged.
//
// The callback is installed and cleared inside the scan serialization gate
// (scanMu) so concurrent ScanWithProgress callers do not race on the shared
// taskProgress field. Both task adapters share one scanner, so the gate is
// the owner-safe boundary that keeps each callback paired with its own scan.
func (s *Scanner) ScanWithProgress(ctx context.Context, opts RefreshOptions, progress func(float64)) (RefreshResult, error) {
	return s.scanWith(ctx, opts, progress, false)
}

// RefreshExistingMetadata runs the remote-backfill scan mode for the manual
// RefreshMetadata task. It enumerates and resolves the configured library
// paths under a full metadata and image refresh intent, passes
// RefreshModeFullRefresh to the provider context, and skips media probing
// and the unrelated local work that does not contribute to provider
// backfill. The returned RefreshResult carries the provider invocation and
// provider-or-image persistence counts.
//
// The method returns a non-nil error when the remote stage made no provider
// invocation or persisted no provider ID or image, so the task adapter can
// reject a false-success result that did no provider work. Per-folder errors
// remain in RefreshResult.Errors for the adapter to join with this error.
func (s *Scanner) RefreshExistingMetadata(ctx context.Context, progress func(float64)) (RefreshResult, error) {
	result, err := s.scanWith(ctx, RefreshOptions{Metadata: RefreshModeFullRefresh, Image: RefreshModeFullRefresh}, progress, true)
	if err != nil {
		return result, err
	}
	if result.ProviderInvocations == 0 {
		return result, errRefreshMetadataNoInvocation
	}
	if result.PersistedProviderIDs == 0 && result.PersistedImages == 0 {
		return result, errRefreshMetadataNoPersistence
	}
	return result, nil
}

// scanWithOptions runs the synchronous scan-and-drain path. When progress is
// non-nil it installs the callback on the scanner under mu while it holds
// scanMu, so scanLocked creates the tracker for this scan. The callback is
// cleared under mu before scanMu is released. This keeps the taskProgress
// write and clear inside the serialization gate, so two concurrent callers
// cannot overwrite each other's callback or leave a stale one for the next
// scan.
func (s *Scanner) scanWithOptions(ctx context.Context, opts RefreshOptions, progress func(float64)) (RefreshResult, error) {
	return s.scanWith(ctx, opts, progress, false)
}

// scanWith runs the synchronous scan-and-drain path. remoteBackfill sets the
// per-scan flag that makes resolveFolder skip media probing. The flag is
// reset to false under mu before a queued normal scan runs, so a pending
// TriggerRefresh intent does not inherit the backfill mode. When progress
// is non-nil it installs the callback on the scanner under mu while it holds
// scanMu, so scanLocked creates the tracker for this scan. The callback and
// the remoteBackfill flag are cleared under mu before scanMu is released,
// so two concurrent callers cannot overwrite each other's state or leave a
// stale one for the next scan.
func (s *Scanner) scanWith(ctx context.Context, opts RefreshOptions, progress func(float64), remoteBackfill bool) (RefreshResult, error) {
	if s == nil {
		return RefreshResult{}, ErrScannerClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	opts = cloneRefreshOptions(opts)
	if err := s.beginWork(); err != nil {
		return RefreshResult{}, err
	}
	s.scanMu.Lock()
	s.mu.Lock()
	s.taskProgress = progress
	s.remoteBackfill = remoteBackfill
	s.mu.Unlock()
	result, scanProgress, err := s.scanLocked(ctx, opts)
	// Drain any async refresh queued while this scan held the gate, so the
	// synchronous path never strands a TriggerRefresh intent. Queued follow-ups
	// run under their own Background context, and their progress and completion
	// callbacks are delivered without changing this caller's returned result.
	// scanMu is released under mu to keep the async handoff race-free.
	type completion struct {
		result RefreshResult
		err    error
	}
	completions := []completion{{result: result, err: err}}
	var extra []RefreshProgress
	for {
		s.mu.Lock()
		if s.pending && !s.closed {
			next := s.pendingOpts
			s.pending = false
			s.pendingOpts = RefreshOptions{}
			s.remoteBackfill = false
			s.mu.Unlock()
			queuedResult, batch, queuedErr := s.scanLocked(context.Background(), next)
			extra = append(extra, batch...)
			completions = append(completions, completion{result: queuedResult, err: queuedErr})
			continue
		}
		s.pending = false
		s.pendingOpts = RefreshOptions{}
		s.clearRefreshStatesLocked()
		if progress != nil {
			s.taskProgress = nil
		}
		s.remoteBackfill = false
		s.scanMu.Unlock()
		s.mu.Unlock()
		break
	}
	for _, finished := range completions {
		s.completeRefresh(finished.result, finished.err)
	}
	s.endWork()
	// See the async path: completion dispatch is owned work; progress delivery
	// remains post-drain and may synchronously close the scanner.
	s.deliver(scanProgress)
	s.deliver(extra)
	return result, err
}

func (s *Scanner) scanLocked(ctx context.Context, opts RefreshOptions) (result RefreshResult, progress []RefreshProgress, err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return RefreshResult{}, nil, ErrScannerClosed
	}
	scanCtx, cancel := context.WithCancel(ctx)
	s.activeCancel = cancel
	s.mu.Unlock()
	if s.taskProgress != nil {
		s.tracker = newProgressTracker(s.taskProgress)
	}
	s.counts = &refreshCounts{}
	result.RegenerateTrickplayItemIDs = mergeRegenerateTrickplayItemIDs(nil, opts.RegenerateTrickplayItemIDs)
	defer func() {
		cancel()
		s.mu.Lock()
		s.activeCancel = nil
		s.tracker = nil
		if s.counts != nil {
			result.ProviderInvocations = int(s.counts.invocations.Load())
			result.PersistedProviderIDs = int(s.counts.persistedProvider.Load())
			result.PersistedImages = int(s.counts.persistedImages.Load())
		}
		s.counts = nil
		s.lastResult = cloneRefreshResult(result)
		s.lastErr = err
		s.hasResult = true
		s.mu.Unlock()
	}()
	record := func(event RefreshProgress) {
		s.progress.publish(event)
		progress = append(progress, event)
		// Log a folder error when it occurs. Progress delivery waits for the
		// drain, so a scan that stalls or never returns would otherwise log
		// no failure at all (#702).
		if event.Error != nil {
			s.logger.Warn("scanner folder", "path", event.Path, "status", event.Status, "error", event.Error)
		}
	}

	record(RefreshProgress{Status: "started"})
	folders, err := s.topology.VirtualFolders(scanCtx)
	if err != nil {
		result.add(RefreshProgress{Status: "failed", Error: err})
		record(RefreshProgress{Status: "failed", Error: err})
		return result, progress, err
	}
	folders = scopeFolders(folders, opts.Targets)
	sort.SliceStable(folders, func(i, j int) bool {
		if folders[i].Path == folders[j].Path {
			return folders[i].ID.String() < folders[j].ID.String()
		}
		return folders[i].Path < folders[j].Path
	})

	// Mark all folders Queued before the scan reaches them. Active takes
	// precedence over Queued once prepareRoots starts a folder.
	s.mu.Lock()
	if s.refreshStates == nil {
		s.refreshStates = make(map[dto.UUID]FolderRefreshState, len(folders))
	}
	for _, folder := range folders {
		s.setRefreshStateLocked(folder.CollectionFolderID, FolderRefreshState{Status: FolderRefreshQueued})
	}
	s.mu.Unlock()

	initial, setup := s.prepareRoots(scanCtx, folders, opts)
	for _, progress := range setup.progress {
		result.add(progress)
		record(progress)
	}
	if err := scanCtx.Err(); err != nil {
		return result, progress, err
	}

	queue := newScanQueue(s, scanCtx)
	var series []seriesSeasonTask
	var albums []albumImageTask
	albumFolders := newAlbumScanFolders(opts)
	_ = queue.run(initial, func(outcome scanOutcome) {
		for _, skip := range outcome.skipped {
			result.add(skip)
			record(skip)
		}
		result.add(outcome.progress)
		record(outcome.progress)
		albumFolders.record(outcome.progress)
		s.completeRefreshUnit(outcome.collectionFolderID, outcome.progress.Status, len(outcome.children))
		if outcome.series != nil && outcome.progress.Status == "scanned" {
			series = append(series, *outcome.series)
		}
		if outcome.album != nil && outcome.progress.Status == "scanned" {
			albums = append(albums, *outcome.album)
		}
	})
	if err := scanCtx.Err(); err != nil {
		return result, progress, err
	}
	for _, failed := range s.reconcileVirtualSeasons(scanCtx, series) {
		result.add(failed)
		record(failed)
	}
	for _, failed := range s.refreshAlbumImages(scanCtx, albums) {
		result.add(failed)
		record(failed)
	}
	if err := scanCtx.Err(); err != nil {
		return result, progress, err
	}
	// An album refreshes after its children (MusicAlbum.RefreshAllMetadata)
	// and takes its artists from its songs.
	if err := s.mergeMusicAlbumArtists(scanCtx, albumFolders); err != nil {
		s.logger.Error("album artists", "error", err)
	}
	// Library post-scan tasks: CollectionPostScanTask.
	if s.options.CollectionsPath != "" {
		if err := s.AddMoviesToCollections(scanCtx, nil); err != nil {
			s.logger.Error("auto collections", "error", err)
		}
	}
	// ArtistsPostScanTask.
	if err := s.validateArtists(scanCtx); err != nil {
		s.logger.Error("artists", "error", err)
	}
	record(RefreshProgress{Status: "completed", Discovered: result.Discovered, Inserted: result.Inserted, Updated: result.Updated, Deleted: result.Deleted})
	return result, progress, nil
}

type rootPreparation struct {
	progress []RefreshProgress
}

func (s *Scanner) prepareRoots(ctx context.Context, folders []VirtualFolder, opts RefreshOptions) ([]scanTask, rootPreparation) {
	var tasks []scanTask
	preparation := rootPreparation{}
	if len(folders) == 0 {
		return nil, preparation
	}
	aggregatePath := normalizedEntryPath(s.topology.root)
	aggregate := aggregateFolder(aggregatePath, s.topology.anchor)

	for _, folder := range folders {
		folder.Path = normalizedEntryPath(folder.Path)
		s.startRefreshFolder(folder.CollectionFolderID)
		sourceTasks, ok := s.prepareRoot(ctx, folder, aggregate, aggregatePath, opts, &preparation)
		s.completeRefreshRootUnit(folder.CollectionFolderID, ok, len(sourceTasks))
		tasks = append(tasks, sourceTasks...)
	}
	return tasks, preparation
}

// prepareRoot does the per-folder root preparation work: resolve the virtual
// folder, plan its sources, persist, and reconcile. It returns the source
// tasks and a success flag. On failure it records progress into preparation.
func (s *Scanner) prepareRoot(ctx context.Context, folder VirtualFolder, aggregate item.Item, aggregatePath string, opts RefreshOptions, preparation *rootPreparation) ([]scanTask, bool) {
	opts = libraryRefreshTargets(folder, opts)
	if err := ctx.Err(); err != nil {
		preparation.progress = append(preparation.progress, RefreshProgress{Path: folder.Path, Status: "failed", Error: err})
		return nil, false
	}
	collection, ok := collectionType(folder.CollectionType)
	if !ok {
		preparation.progress = append(preparation.progress, RefreshProgress{Path: folder.Path, Status: "failed", Error: fmt.Errorf("unsupported collection type %q", folder.CollectionType)})
		return nil, false
	}
	virtual, err := s.chain.Resolve(ctx, ResolveContext{
		Entry:           Entry{Path: folder.Path, IsDir: true},
		Parent:          &aggregate,
		CollectionType:  collection,
		Options:         folder.Options,
		LibraryRoot:     folder.Path,
		IsVirtualFolder: true,
	})
	if err != nil {
		preparation.progress = append(preparation.progress, RefreshProgress{Path: folder.Path, Status: "failed", Error: fmt.Errorf("resolve virtual folder: %w", err)})
		return nil, false
	}
	if err := validateResolved(&aggregate.ID, virtual); err != nil {
		preparation.progress = append(preparation.progress, RefreshProgress{Path: folder.Path, Status: "failed", Error: fmt.Errorf("resolve virtual folder: %w", err)})
		return nil, false
	}
	if err := validateResolvedPath(folder.Path, virtual); err != nil {
		preparation.progress = append(preparation.progress, RefreshProgress{Path: folder.Path, Status: "failed", Error: fmt.Errorf("resolve virtual folder: %w", err)})
		return nil, false
	}
	virtualPath := folder.Path
	virtual.Path = &virtualPath

	existing, err := s.directChildren(ctx, virtual.ID)
	if err != nil {
		preparation.progress = append(preparation.progress, RefreshProgress{Path: folder.Path, Status: "failed", Error: fmt.Errorf("read configured sources: %w", err)})
		return nil, false
	}
	rootFolders, rootPlans, scannedVirtuals, skipped, progress, ok := s.planSources(ctx, folder, collection, *virtual, existing, opts)
	if !ok {
		preparation.progress = append(preparation.progress, skipped...)
		preparation.progress = append(preparation.progress, progress)
		return nil, false
	}
	preparation.progress = append(preparation.progress, skipped...)
	if err := s.persistVirtualIfChanged(ctx, aggregate); err != nil {
		preparation.progress = append(preparation.progress, RefreshProgress{Path: aggregatePath, Status: "failed", Error: fmt.Errorf("persist aggregate folder: %w", err)})
		return nil, false
	}
	// Write the collection's PhysicalFolderIDs marker so the store
	// projection can flatten the root Folders from a non-recursive browse.
	// The marker is sorted and recomputed every scan from the configured
	// paths, matching upstream CollectionFolder.RefreshLinkedChildrenInternal.
	if virtual.Extra.CollectionFolder == nil {
		virtual.Extra.CollectionFolder = &item.CollectionFolderExtra{}
	}
	virtual.Extra.CollectionFolder.CollectionType = &collection
	virtual.Extra.CollectionFolder.PhysicalFolderIDs = sortedFolderIDs(rootFolders)
	preserveIDs := computePreserveChildIDs(existing, rootFolders, normalizedMediaPaths(folder.MediaPaths), rootPlans)
	if err := s.persistVirtualIfChanged(ctx, *virtual); err != nil {
		preparation.progress = append(preparation.progress, RefreshProgress{Path: folder.Path, Status: "failed", Error: fmt.Errorf("persist virtual folder: %w", err)})
		return nil, false
	}
	forceRefresh := opts.ReplaceAllMetadata || opts.Metadata >= RefreshModeFullRefresh
	// Reconcile the collection-level plan: the root Folders are the desired
	// direct children. In remote-backfill mode the plan routes through
	// reconcileBackfill, so stored root Folders merge enrichment-only and
	// absent roots insert ordinarily; PreserveChildIDs protects legacy
	// flattened children from deletion until the per-root reconcile adopts
	// them.
	var mutation backfillOutcome
	if s.remoteBackfill {
		mutation, err = s.reconcileBackfill(ctx, virtual.ID, rootFolders, scannedVirtuals, forceRefresh, preserveIDs)
	} else {
		var ordinary store.ReconcileChildrenResult
		ordinaryErr := s.storeWrite(ctx, func() (err error) {
			ordinary, err = s.items.ReconcileScannedPlanAtWithOptions(ctx, virtual.ID, rootFolders, nil, scannedVirtuals, s.options.Now(), store.ReconcileOptions{ForceRefresh: forceRefresh, PreserveChildIDs: preserveIDs})
			return err
		})
		mutation = backfillOutcome{ReconcileChildrenResult: ordinary}
		err = ordinaryErr
	}
	if err != nil {
		preparation.progress = append(preparation.progress, RefreshProgress{Path: folder.Path, Status: "failed", Error: fmt.Errorf("reconcile configured sources: %w", err)})
		return nil, false
	}
	s.changes.publish(virtual.ID, virtual.ID, mutation.ReconcileChildrenResult)
	// Reconcile each accessible root's child plan under the root Folder.
	// This performs the legacy adoption: existing rows move to the new
	// parent in place, preserving user_data, satellites, and provider IDs.
	discovered := len(rootFolders)
	inserted := len(mutation.Inserted)
	updated := len(mutation.Updated)
	deleted := len(mutation.Deleted)
	backfillUpdated := mutation.backfillUpdated
	backfillSkipped := mutation.backfillSkipped
	var sourceTasks []scanTask
	for _, plan := range rootPlans {
		if !plan.accessible {
			sourceTasks = append(sourceTasks, scanTask{collectionFolderID: folder.CollectionFolderID, progress: RefreshProgress{Path: *plan.folder.Path, Status: "inaccessible", Error: fmt.Errorf("enumerate source: %w", plan.err)}})
			continue
		}
		rootOutcome, rootErr := s.reconcileRootPlan(ctx, plan, forceRefresh)
		if rootErr != nil {
			preparation.progress = append(preparation.progress, RefreshProgress{Path: *plan.folder.Path, Status: "failed", Error: fmt.Errorf("reconcile root %q: %w", *plan.folder.Path, rootErr)})
			return nil, false
		}
		s.changes.publish(plan.folder.ID, virtual.ID, rootOutcome.ReconcileChildrenResult)
		s.countPersistedSatellites(rootOutcome.ReconcileChildrenResult, plan.desired, nil, plan.beforeSatellites)
		discovered += len(plan.desired)
		inserted += len(rootOutcome.Inserted)
		updated += len(rootOutcome.Updated)
		deleted += len(rootOutcome.Deleted)
		backfillUpdated += rootOutcome.backfillUpdated
		backfillSkipped += rootOutcome.backfillSkipped
		sourceTasks = append(sourceTasks, plan.children...)
	}
	rootProgress := RefreshProgress{ItemID: virtual.ID, Path: folder.Path, Status: "scanned", Progress: 100, Discovered: discovered, Inserted: inserted, Updated: updated, Deleted: deleted, BackfillUpdated: backfillUpdated, BackfillSkipped: backfillSkipped}
	preparation.progress = append(preparation.progress, rootProgress)
	return sourceTasks, true
}

// rootScanPlan holds the per-root resolution data that planSources collects.
// Each configured media root produces a Folder item in the collection-level
// desired plan. Accessible roots also produce a resolved child plan for
// reconciliation under the root Folder. Inaccessible roots produce no child
// plan; their persisted descendants survive via the PreserveChildIDs option.
type rootScanPlan struct {
	folder           item.Item
	desired          []item.Item
	media            []store.ScannedChildMedia
	children         []scanTask
	accessible       bool
	err              error
	skipped          []RefreshProgress
	beforeSatellites map[dto.UUID]refreshSatelliteSnapshot
}

func (s *Scanner) planSources(ctx context.Context, folder VirtualFolder, collection item.CollectionType, virtual item.Item, existing []item.Item, opts RefreshOptions) ([]item.Item, []rootScanPlan, []item.Item, []RefreshProgress, RefreshProgress, bool) {
	mediaPaths := normalizedMediaPaths(folder.MediaPaths)
	byPath := make(map[string]item.Item, len(existing))
	for _, child := range existing {
		if child.Path != nil {
			byPath[normalizedEntryPath(*child.Path)] = child
		}
	}
	rootFolders := make([]item.Item, 0, len(mediaPaths))
	plans := make([]rootScanPlan, 0, len(mediaPaths))
	var virtuals []item.Item
	tasks := make([]scanTask, 0)
	var skipped []RefreshProgress
	for _, path := range mediaPaths {
		rootItem, resolveErr := s.chain.Resolve(ctx, ResolveContext{
			Entry:                 Entry{Path: path, IsDir: true},
			Parent:                &virtual,
			IsPhysicalLibraryRoot: true,
			CollectionType:        collection,
			Options:               folder.Options,
			LibraryRoot:           path,
		})
		if resolveErr != nil {
			return nil, nil, nil, nil, RefreshProgress{Path: folder.Path, Status: "failed", Error: fmt.Errorf("resolve source root %q: %w", path, resolveErr)}, false
		}
		if rootItem == nil {
			return nil, nil, nil, nil, RefreshProgress{Path: folder.Path, Status: "failed", Error: fmt.Errorf("resolve source root %q: returned no item", path)}, false
		}
		rootFolders = append(rootFolders, *rootItem)

		entries, err := s.readMediaDir(path)
		if err != nil {
			// An inaccessible root still enters the collection plan. The
			// scanner cannot enumerate the directory, so no per-root
			// reconcile or scan task runs. The inaccessible progress row
			// remains, and the root Folder survives as a desired item.
			tasks = append(tasks, scanTask{collectionFolderID: folder.CollectionFolderID, progress: RefreshProgress{Path: path, Status: "inaccessible", Error: fmt.Errorf("enumerate source: %w", err)}})
			plans = append(plans, rootScanPlan{folder: *rootItem, accessible: false, err: err})
			continue
		}
		normalizeEntries(entries)
		rootTask := scanTask{folder: *rootItem, entry: Entry{Path: path, IsDir: true}, entries: entries, hasEntries: true, collection: collection, options: folder.Options, libraryRoot: path, ancestors: []*item.Item{&virtual}, refresh: opts, collectionFolderID: folder.CollectionFolderID, branch: visitBranch(nil, directoryIdentity(path))}
		resolution, resolveErr := s.resolveFolder(ctx, rootTask)
		if resolveErr != nil {
			return nil, nil, nil, skipped, RefreshProgress{Path: folder.Path, Status: "failed", Error: fmt.Errorf("resolve source %q: %w", path, resolveErr)}, false
		}
		virtuals = append(virtuals, resolution.virtuals...)
		plans = append(plans, rootScanPlan{folder: *rootItem, desired: resolution.desired, media: resolution.media, children: resolution.children, accessible: true, skipped: resolution.skipped, beforeSatellites: resolution.beforeSatellites})
		skipped = append(skipped, resolution.skipped...)
	}
	virtuals = deduplicateVirtuals(virtuals)
	preserveScannedLifecycleDates(rootFolders, byPath)
	if err := validatePlan(virtual.ID, rootFolders); err != nil {
		return nil, nil, nil, skipped, RefreshProgress{Path: folder.Path, Status: "failed", Error: fmt.Errorf("validate configured sources: %w", err)}, false
	}
	for _, plan := range plans {
		if plan.accessible {
			tasks = append(tasks, plan.children...)
		}
	}
	return rootFolders, plans, virtuals, skipped, RefreshProgress{}, true
}

// deduplicateVirtuals merges virtual items collected from multiple media
// roots. Two accessible roots that share metadata, such as a genre named
// "Drama", produce the same virtual UUID from each root. Appending both without
// deduplication makes reconcileScannedVirtuals reject the plan as a duplicate.
// The merge keeps one item per UUID and selects the lexical-minimum name,
// matching the case-insensitive identity rule in collectTaxonomyEntity.
func deduplicateVirtuals(virtuals []item.Item) []item.Item {
	if len(virtuals) == 0 {
		return virtuals
	}
	merged := make(map[dto.UUID]item.Item, len(virtuals))
	for _, candidate := range virtuals {
		if existing, found := merged[candidate.ID]; !found || (candidate.Name != nil && existing.Name != nil && *candidate.Name < *existing.Name) {
			merged[candidate.ID] = candidate
		}
	}
	ids := make([]dto.UUID, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left].String() < ids[right].String() })
	result := make([]item.Item, len(ids))
	for index, id := range ids {
		result[index] = merged[id]
	}
	return result
}

// sortedFolderIDs returns the IDs of the root Folder items in UUID order.
// The scanner writes this sorted list as the CollectionFolder's
// PhysicalFolderIDs marker.
func sortedFolderIDs(folders []item.Item) []dto.UUID {
	ids := make([]dto.UUID, len(folders))
	for index, folder := range folders {
		ids[index] = folder.ID
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left].String() < ids[right].String() })
	return ids
}

// computePreserveChildIDs returns the existing direct children whose IDs are
// not in the collection plan and whose source a per-root reconcile will
// adopt, the existing direct children under inaccessible roots, and the
// existing direct children that an accessible root quarantine dropped. All
// three sets survive the collection-level reconcile as preserved IDs.
//
// A legacy flattened child whose source the resolver found appears in an
// accessible root plan with the same deterministic ID. The function preserves
// only those children, so a child whose source was removed before the scan is
// not protected. The collection-level reconcile deletes that stale child, and
// the browse projection omits it.
//
// A quarantined child whose source the resolver found but the validator
// dropped does not appear in any accessible root plan desired set. The
// function preserves it by its quarantined ID so the collection-level
// reconcile does not delete the legacy row with its user_data, satellites,
// provider IDs, and images. The child stays under the collection until the
// next scan, when the validation may pass and the per-root reconcile adopts it.
//
// A child whose path lies under no configured root stays out of the list, so
// a removed root and its subtree still delete. A child under an inaccessible
// root survives via PreserveChildIDs and stays under the collection until the
// root is accessible again. The scanner cannot enumerate the directory, so no
// per-root reconcile runs for that root. Preserving the child prevents the
// collection-level reconcile from deleting it and its user_data, satellites,
// provider IDs, and images before the root becomes accessible again.
func computePreserveChildIDs(existing []item.Item, rootFolders []item.Item, mediaPaths []string, plans []rootScanPlan) []dto.UUID {
	planIDs := make(map[dto.UUID]struct{}, len(rootFolders))
	for _, folder := range rootFolders {
		planIDs[folder.ID] = struct{}{}
	}
	// adoptableIDs holds the deterministic IDs that an accessible root
	// plan will reparent. A legacy child outside this set has no source
	// to adopt it, so preserving it would retain a stale item forever.
	adoptableIDs := make(map[dto.UUID]struct{})
	for _, plan := range plans {
		if !plan.accessible {
			continue
		}
		for _, child := range plan.desired {
			adoptableIDs[child.ID] = struct{}{}
		}
	}
	// quarantinedIDs holds the deterministic IDs of candidates that an
	// accessible root resolver found but the quarantine validator
	// dropped. The source exists, but the candidate failed validation,
	// so the per-root reconcile never receives it in desired. Without
	// preservation, the collection-level reconcile would delete the
	// legacy row with its user_data, satellites, provider IDs, and
	// images. A quarantined child must survive under the collection
	// until the next scan, when the validation may pass.
	quarantinedIDs := make(map[dto.UUID]struct{})
	for _, plan := range plans {
		if !plan.accessible {
			continue
		}
		for _, skip := range plan.skipped {
			if !skip.ItemID.IsNil() {
				quarantinedIDs[skip.ItemID] = struct{}{}
			}
		}
	}
	// inaccessibleRoots holds the normalized paths of inaccessible root
	// Folders. A legacy flattened child under an inaccessible root must
	// survive via PreserveChildIDs and stay under the collection until
	// the root is accessible again. Its source was not resolved, so it
	// is not in adoptableIDs, but the spec requires it to stay.
	inaccessibleRoots := make([]string, 0)
	for _, plan := range plans {
		if plan.accessible || plan.folder.Path == nil {
			continue
		}
		inaccessibleRoots = append(inaccessibleRoots, normalizedEntryPath(*plan.folder.Path))
	}
	normalizedPaths := make([]string, len(mediaPaths))
	for index, path := range mediaPaths {
		normalizedPaths[index] = normalizedEntryPath(path)
	}
	var ids []dto.UUID
	for _, child := range existing {
		if _, planned := planIDs[child.ID]; planned {
			continue
		}
		if child.Path == nil {
			continue
		}
		childPath := normalizedEntryPath(*child.Path)
		descends := false
		for _, root := range normalizedPaths {
			prefix := root + string(filepath.Separator)
			if childPath != root && strings.HasPrefix(childPath, prefix) {
				descends = true
				break
			}
		}
		if !descends {
			continue
		}
		// A child under an inaccessible root must survive until the root
		// is accessible again. A child under an accessible root must be
		// adoptable or quarantined — its source exists and the per-root
		// reconcile will reparent it, or the quarantine validator dropped
		// it and the row must survive until the next scan.
		underInaccessible := false
		for _, root := range inaccessibleRoots {
			prefix := root + string(filepath.Separator)
			if childPath != root && strings.HasPrefix(childPath, prefix) {
				underInaccessible = true
				break
			}
		}
		if !underInaccessible {
			if _, adoptable := adoptableIDs[child.ID]; !adoptable {
				if _, quarantined := quarantinedIDs[child.ID]; !quarantined {
					continue
				}
			}
		}
		ids = append(ids, child.ID)
	}
	return ids
}

func normalizedMediaPaths(paths []string) []string {
	normalized := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = normalizedEntryPath(path)
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		normalized = append(normalized, path)
	}
	sort.Strings(normalized)
	result := make([]string, 0, len(normalized))
	for _, path := range normalized {
		nested := false
		for _, root := range result {
			relative, err := filepath.Rel(root, path)
			if err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				nested = true
				break
			}
		}
		if !nested {
			result = append(result, path)
		}
	}
	return result
}

func aggregateFolder(path, programDataRoot string) item.Item {
	return initializeContext(ResolveContext{Entry: Entry{Path: path, IsDir: true}, ProgramDataRoot: programDataRoot}, item.KindAggregateFolder)
}

func (s *Scanner) directChildren(ctx context.Context, parentID dto.UUID) (children []item.Item, err error) {
	err = s.storeWrite(ctx, func() error {
		children, err = s.items.ReconciliationChildren(ctx, parentID)
		return err
	})
	return children, err
}

// storeWrite runs one scanner call that uses the store's writer connection.
// Folder workers resolve and probe in parallel, but they enter the writer one
// at a time. SQLite has one writer connection, and database/sql gives a
// released connection to a random waiter. Without the lane, every folder
// worker waits in that queue, and an interactive write (login, playback
// progress, user data) waits for several scan transactions. With the lane, at
// most one scan write waits, so the released connection goes to the waiting
// interactive write first. Scan throughput does not change: the writer
// serializes the writes in either case.
func (s *Scanner) storeWrite(ctx context.Context, write func() error) error {
	select {
	case s.writeLane <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.writeLane }()
	return write()
}

type scanTask struct {
	folder             item.Item
	entry              Entry
	entries            []Entry
	hasEntries         bool
	childEntries       map[string][]Entry
	collection         item.CollectionType
	options            LibraryOptions
	libraryRoot        string
	ancestors          []*item.Item
	refresh            RefreshOptions
	progress           RefreshProgress
	collectionFolderID dto.UUID
	// branch holds the directory identities from the source root down to
	// and including this folder. A directory entry on the branch is a link
	// cycle and is not scanned.
	branch []visitedDirectory
}

type scanOutcome struct {
	progress           RefreshProgress
	children           []scanTask
	collectionFolderID dto.UUID
	skipped            []RefreshProgress
	// series is set when the folder is a Series. The virtual-season pass
	// runs for it after the scan stores its subtree.
	series *seriesSeasonTask
	// album is set when the folder is a MusicAlbum. The album image pass
	// runs for it after the scan stores its subtree.
	album *albumImageTask
}

// folderResolution holds the resolved items and probe results that
// resolveFolder produces for one directory. The scanner reconciles
// these against the parent folder.
type folderResolution struct {
	desired          []item.Item
	media            []store.ScannedChildMedia
	virtuals         []item.Item
	children         []scanTask
	entryCount       int
	skipped          []RefreshProgress
	beforeSatellites map[dto.UUID]refreshSatelliteSnapshot
	// preserve names the pathless Season children of a Series. The
	// virtual-season pass owns them, so the folder reconcile keeps them.
	preserve []dto.UUID
}

// backfillOutcome carries the ReconcileChildrenResult from one persistence
// site plus the backfill counters that the remote-backfill partition reports.
// The ordinary (non-backfill) path returns a zero-counter outcome so the
// caller can treat both paths the same way.
type backfillOutcome struct {
	store.ReconcileChildrenResult
	backfillUpdated int
	backfillSkipped int
}

// reconcileFolder selects the store persistence path for one resolved folder.
// The remote-backfill mode splits the desired items into existing items, which
// route to MetadataBackfill for enrichment-only merging, and new items, which
// route to ReconcileScannedPlanAtWithOptions for insertion. The ordinary path
// keeps ReconcileScannedPlanAtWithOptions unchanged for all items.
func (s *Scanner) reconcileFolder(ctx context.Context, task scanTask, resolution folderResolution) (backfillOutcome, error) {
	if !s.remoteBackfill {
		var mutation store.ReconcileChildrenResult
		err := s.storeWrite(ctx, func() (err error) {
			mutation, err = s.items.ReconcileScannedPlanAtWithOptions(ctx, task.folder.ID, resolution.desired, resolution.media, resolution.virtuals, s.options.Now(), store.ReconcileOptions{ForceRefresh: task.refresh.ReplaceAllMetadata || task.refresh.Metadata >= RefreshModeFullRefresh, PreserveChildIDs: resolution.preserve})
			return err
		})
		return backfillOutcome{ReconcileChildrenResult: mutation}, err
	}
	return s.reconcileBackfill(ctx, task.folder.ID, resolution.desired, resolution.virtuals, task.refresh.ReplaceAllMetadata || task.refresh.Metadata >= RefreshModeFullRefresh, resolution.preserve)
}

// reconcileRootPlan selects the store persistence path for one root plan's
// child set. The remote-backfill mode partitions the plan into existing rows,
// which route to MetadataBackfill for enrichment-only merging, and absent
// rows, which route to ReconcileScannedPlanAtWithOptions for insertion. The
// ordinary path keeps ReconcileScannedPlanAtWithOptions unchanged.
func (s *Scanner) reconcileRootPlan(ctx context.Context, plan rootScanPlan, forceRefresh bool) (backfillOutcome, error) {
	if !s.remoteBackfill {
		var mutation store.ReconcileChildrenResult
		err := s.storeWrite(ctx, func() (err error) {
			mutation, err = s.items.ReconcileScannedPlanAtWithOptions(ctx, plan.folder.ID, plan.desired, plan.media, nil, s.options.Now(), store.ReconcileOptions{ForceRefresh: forceRefresh})
			return err
		})
		return backfillOutcome{ReconcileChildrenResult: mutation}, err
	}
	return s.reconcileBackfill(ctx, plan.folder.ID, plan.desired, nil, forceRefresh, nil)
}

// reconcileBackfill partitions the remote-backfill plan by stored identity.
// It partitions both item candidates and virtual candidates. Existing items
// and existing virtuals route to MetadataBackfill for enrichment-only
// merging. Absent items and absent virtuals route to
// ReconcileScannedPlanAtWithOptions for ordinary insertion with the full
// candidate. The two results are merged so the scanner's persistence-counting
// path stays unchanged. The absent insert sets PreserveUnplannedChildren so
// the ordinary reconcile inserts absent items but preserves every existing
// direct child the backfill plan did not re-derive. The enrichment-only
// contract never removes a stored child, satellite, or user-data row because
// a backfill plan omits that child. extraPreserve adds caller-named IDs to
// the explicit preserve set the store CTE reads.
//
// The backfill counters come from the stored/absent partition.
// backfillUpdated counts the stored direct items MetadataBackfill changed.
// backfillSkipped counts the stored direct items MetadataBackfill did not
// change. Absent items that inserted through ordinary reconciliation do not
// count here; the partition is the only code that separates the two sets.
func (s *Scanner) reconcileBackfill(ctx context.Context, folderID dto.UUID, desired []item.Item, virtuals []item.Item, forceRefresh bool, extraPreserve []dto.UUID) (backfillOutcome, error) {
	existing, absent, err := s.partitionBackfillPlan(ctx, folderID, desired, true)
	if err != nil {
		return backfillOutcome{}, fmt.Errorf("partition backfill plan: %w", err)
	}
	existingVirtuals, absentVirtuals, err := s.partitionBackfillPlan(ctx, folderID, virtuals, false)
	if err != nil {
		return backfillOutcome{}, fmt.Errorf("partition backfill virtuals: %w", err)
	}
	var merged store.ReconcileChildrenResult
	var backfillUpdated int
	if len(existing) > 0 || len(existingVirtuals) > 0 {
		var backfill store.ReconcileChildrenResult
		err := s.storeWrite(ctx, func() (err error) {
			backfill, err = s.items.MetadataBackfill(ctx, folderID, existing, existingVirtuals, s.options.Now(), store.MetadataBackfillOptions{ForceRefresh: forceRefresh})
			return err
		})
		if err != nil {
			return backfillOutcome{ReconcileChildrenResult: mergeBackfillResults(merged, backfill)}, err
		}
		merged = mergeBackfillResults(merged, backfill)
		backfillUpdated = len(backfill.Updated)
	}
	backfillSkipped := len(existing) - backfillUpdated
	if len(absent) > 0 || len(absentVirtuals) > 0 {
		var inserted store.ReconcileChildrenResult
		err := s.storeWrite(ctx, func() (err error) {
			inserted, err = s.items.ReconcileScannedPlanAtWithOptions(ctx, folderID, absent, nil, absentVirtuals, s.options.Now(), store.ReconcileOptions{
				ForceRefresh:              forceRefresh,
				PreserveChildIDs:          extraPreserve,
				PreserveUnplannedChildren: true,
			})
			return err
		})
		if err != nil {
			return backfillOutcome{ReconcileChildrenResult: mergeBackfillResults(merged, inserted), backfillUpdated: backfillUpdated, backfillSkipped: backfillSkipped}, err
		}
		merged = mergeBackfillResults(merged, inserted)
	}
	return backfillOutcome{ReconcileChildrenResult: merged, backfillUpdated: backfillUpdated, backfillSkipped: backfillSkipped}, nil
}

// withAdoptedItems returns existing plus the stored items at the paths of
// the candidates that existing lacks. Such an item is stored under another
// parent; the reconcile moves it under this folder.
func (s *Scanner) withAdoptedItems(ctx context.Context, desired []item.Item, existing map[string]item.Item) (map[string]item.Item, error) {
	var ids []dto.UUID
	for _, candidate := range desired {
		if candidate.Path == nil {
			continue
		}
		if _, found := existing[normalizedEntryPath(*candidate.Path)]; !found {
			ids = append(ids, candidate.ID)
		}
	}
	if len(ids) == 0 {
		return existing, nil
	}
	var merged map[string]item.Item
	for start := 0; start < len(ids); start += adoptedLookupBatch {
		end := min(start+adoptedLookupBatch, len(ids))
		batch, err := s.items.GetBatch(ctx, ids[start:end])
		if err != nil {
			return nil, err
		}
		for _, stored := range batch {
			if stored.Path == nil {
				continue
			}
			if merged == nil {
				merged = make(map[string]item.Item, len(existing)+len(batch))
				for path, value := range existing {
					merged[path] = value
				}
			}
			merged[normalizedEntryPath(*stored.Path)] = stored
		}
	}
	if merged == nil {
		return existing, nil
	}
	return merged, nil
}

// adoptedLookupBatch bounds the bind parameters of one adopted-item lookup.
const adoptedLookupBatch = 500

// partitionBackfillPlan splits the given items into existing and absent sets by
// querying the store for each ID. It works for both direct child candidates
// and virtual candidates. An item that exists in the store routes to the
// enrichment-only backfill; an absent item routes to the ordinary reconcile
// insert path. A read failure returns an error so the caller never routes
// existing rows to the insert path on a transient fault.
//
// For direct children (moved true), a stored row with a different Kind,
// ParentID, or Path also routes to the ordinary reconcile, which adopts it
// under this folder as an ordinary scan does. Such a row is the stale child
// of a removed or case-variant folder that shares the candidate's
// case-insensitive ID. The enrichment-only backfill rejects a structural
// change, so without this route the stale row fails the folder on every
// refresh. Virtual candidates (moved false) always route by ID: a Person,
// Studio, or Genre keeps its stored path (store.MetadataBackfill).
func (s *Scanner) partitionBackfillPlan(ctx context.Context, folderID dto.UUID, desired []item.Item, moved bool) (existing, absent []item.Item, err error) {
	if len(desired) == 0 {
		return nil, nil, nil
	}
	ids := make([]dto.UUID, len(desired))
	for index, child := range desired {
		ids[index] = child.ID
	}
	batch, err := s.items.GetBatch(ctx, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("read stored items for folder %s: %w", folderID, err)
	}
	found := make(map[dto.UUID]item.Item, len(batch))
	for _, stored := range batch {
		found[stored.ID] = stored
	}
	for _, child := range desired {
		stored, ok := found[child.ID]
		if ok && moved && !sameBackfillStructure(child, stored) {
			ok = false
		}
		if ok {
			existing = append(existing, child)
		} else {
			absent = append(absent, child)
		}
	}
	return existing, absent, nil
}

// sameBackfillStructure reports whether the candidate keeps the stored row's
// Kind, ParentID, and Path, the structural identity the enrichment-only
// backfill requires.
func sameBackfillStructure(candidate, stored item.Item) bool {
	if candidate.Kind != stored.Kind {
		return false
	}
	if (candidate.ParentID == nil) != (stored.ParentID == nil) || (candidate.ParentID != nil && *candidate.ParentID != *stored.ParentID) {
		return false
	}
	if (candidate.Path == nil) != (stored.Path == nil) || (candidate.Path != nil && *candidate.Path != *stored.Path) {
		return false
	}
	return true
}

// mergeBackfillResults merges two ReconcileChildrenResult values into one.
func mergeBackfillResults(a, b store.ReconcileChildrenResult) store.ReconcileChildrenResult {
	return store.ReconcileChildrenResult{
		Inserted: append(a.Inserted, b.Inserted...),
		Updated:  append(a.Updated, b.Updated...),
		Deleted:  append(a.Deleted, b.Deleted...),
	}
}

func (s *Scanner) scanFolder(ctx context.Context, task scanTask) scanOutcome {
	if task.progress.Status != "" {
		return scanOutcome{progress: task.progress, collectionFolderID: task.collectionFolderID}
	}
	resolution, err := s.resolveFolder(ctx, task)
	if err != nil {
		return scanOutcome{progress: RefreshProgress{Path: task.entry.Path, Status: "failed", Error: err}, collectionFolderID: task.collectionFolderID}
	}
	mutation, err := s.reconcileFolder(ctx, task, resolution)
	if err != nil {
		return scanOutcome{progress: RefreshProgress{Path: task.entry.Path, Status: "failed", Error: fmt.Errorf("reconcile folder: %w", err)}, collectionFolderID: task.collectionFolderID}
	}
	s.changes.publish(task.folder.ID, scanTaskCollectionFolderID(task), mutation.ReconcileChildrenResult)
	s.countPersistedSatellites(mutation.ReconcileChildrenResult, resolution.desired, resolution.virtuals, resolution.beforeSatellites)
	outcome := scanOutcome{progress: RefreshProgress{ItemID: task.folder.ID, Path: task.entry.Path, Status: "scanned", Progress: 100, Discovered: resolution.entryCount, Inserted: len(mutation.Inserted), Updated: len(mutation.Updated), Deleted: len(mutation.Deleted), BackfillUpdated: mutation.backfillUpdated, BackfillSkipped: mutation.backfillSkipped}, children: resolution.children, collectionFolderID: task.collectionFolderID, skipped: resolution.skipped}
	if task.folder.Kind == item.KindSeries {
		outcome.series = &seriesSeasonTask{series: task.folder, options: task.options}
	}
	if task.folder.Kind == item.KindMusicAlbum && !s.remoteBackfill {
		outcome.album = &albumImageTask{albumID: task.folder.ID, collectionFolderID: scanTaskCollectionFolderID(task)}
	}
	return outcome
}

// readDirectoryChildren returns the cached listing of directory, or reads,
// normalizes, and caches it. A listing that cannot be read is empty. branch
// holds the directories above directory; a child directory on that branch
// or equal to directory is a link cycle and is dropped from the listing.
func (s *Scanner) readDirectoryChildren(directoryChildren map[string][]Entry, directory Entry, branch []visitedDirectory) []Entry {
	key := normalizedEntryPath(directory.Path)
	if children, found := directoryChildren[key]; found {
		return children
	}
	children, err := s.readMediaDir(directory.Path)
	if err != nil {
		return nil
	}
	normalizeEntries(children)
	children = s.dropDirectoryCycles(children, visitBranch(branch, directory.Identity))
	directoryChildren[key] = children
	return children
}

// resolveFolder resolves one directory's entries into a child plan without
// reconciling. The caller reconciles the desired items under the parent
// folder and enqueues the child tasks for recursion. Splitting resolution
// from reconciliation lets planSources merge the child plans from all
// accessible media roots into one reconciliation under the CollectionFolder.
func (s *Scanner) resolveFolder(ctx context.Context, task scanTask) (folderResolution, error) {
	entries := append([]Entry(nil), task.entries...)
	if !task.hasEntries {
		var err error
		entries, err = s.readMediaDir(task.entry.Path)
		if err != nil {
			return folderResolution{}, fmt.Errorf("enumerate folder: %w", err)
		}
	}
	normalizeEntries(entries)
	entries = s.dropDirectoryCycles(entries, task.branch)
	entryPaths := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		entryPaths[normalizedEntryPath(entry.Path)] = struct{}{}
	}
	entryByPath := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		entryByPath[normalizedEntryPath(entry.Path)] = entry
	}
	directoryChildren := cloneDirectoryEntries(task.childEntries)
	for _, entry := range entries {
		if !entry.IsDir {
			continue
		}
		key := normalizedEntryPath(entry.Path)
		entryBranch := visitBranch(task.branch, entry.Identity)
		children, found := directoryChildren[key]
		if !found {
			var err error
			children, err = s.readMediaDir(entry.Path)
			if err != nil {
				continue
			}
			normalizeEntries(children)
			children = s.dropDirectoryCycles(children, entryBranch)
			directoryChildren[key] = children
		}
		for _, child := range children {
			if !child.IsDir {
				continue
			}
			grandchildren := s.readDirectoryChildren(directoryChildren, child, entryBranch)
			// An artist folder is an artist when a child album holds its
			// tracks in multi-disc subfolders (MusicArtistResolver calls
			// MusicAlbumResolver.IsMusicAlbum on each child), so a music
			// folder also needs the listings one level further down.
			if task.collection != item.CollectionTypeMusic {
				continue
			}
			for _, grandchild := range grandchildren {
				if grandchild.IsDir {
					s.readDirectoryChildren(directoryChildren, grandchild, visitBranch(entryBranch, child.Identity))
				}
			}
		}
	}
	contextInput := ResolveContext{Entry: task.entry, Children: entries, ChildEntries: directoryChildren, Parent: &task.folder, Ancestors: task.ancestors, CollectionType: task.collection, Options: task.options, LibraryRoot: task.libraryRoot}
	grouped, err := s.chain.ResolveCandidates(ctx, contextInput)
	if err != nil {
		return folderResolution{}, fmt.Errorf("resolve folder group: %w", err)
	}
	resolved := make(map[string]item.Item, len(entries))
	resolutionSources := make(map[string]Entry, len(entries))
	refreshSources := make(map[string]Entry, len(entries))
	consumedPaths := make(map[string]struct{}, len(entries))
	localAlternates := make(map[string]LocalAlternateVersion, len(entries))
	flattened := make([]item.Item, 0)
	stackedParts := make([]item.Item, 0)
	stackedPartIDs := make(map[dto.UUID]struct{})
	for _, candidate := range grouped {
		if err := validateResolved(&task.folder.ID, candidate.Item); err != nil {
			return folderResolution{}, fmt.Errorf("resolve folder group: %w", err)
		}
		if candidate.Item.Path == nil {
			return folderResolution{}, errors.New("resolve folder group: item path is required")
		}
		if _, found := entryPaths[normalizedEntryPath(*candidate.Item.Path)]; !found {
			return folderResolution{}, fmt.Errorf("resolve folder group: item path %q is not an enumerated entry", *candidate.Item.Path)
		}
		candidatePath := normalizedEntryPath(*candidate.Item.Path)
		if _, duplicate := consumedPaths[candidatePath]; duplicate {
			return folderResolution{}, fmt.Errorf("resolve folder group: duplicate path %q", *candidate.Item.Path)
		}
		if err := consumeAdditionalParts(candidate.Item, entryByPath, consumedPaths); err != nil {
			return folderResolution{}, fmt.Errorf("resolve folder group: %w", err)
		}
		parts, err := materializeAdditionalParts(*candidate.Item, entryByPath, entryByPath[candidatePath], resolutionSources, refreshSources)
		if err != nil {
			return folderResolution{}, fmt.Errorf("resolve folder group: %w", err)
		}
		stackedParts = append(stackedParts, parts...)
		for _, part := range parts {
			stackedPartIDs[part.ID] = struct{}{}
		}
		resolved[candidatePath] = *candidate.Item
		if candidate.LocalAlternateVersion != nil {
			localAlternates[candidatePath] = *candidate.LocalAlternateVersion
		}
		consumedPaths[candidatePath] = struct{}{}
		resolutionSources[candidatePath] = entryByPath[candidatePath]
		refreshSources[candidatePath] = entryByPath[candidatePath]
	}
	for _, entry := range entries {
		if _, consumed := consumedPaths[normalizedEntryPath(entry.Path)]; consumed {
			continue
		}
		candidate, err := s.chain.Resolve(ctx, ResolveContext{Entry: entry, Children: directoryChildren[normalizedEntryPath(entry.Path)], ChildEntries: directoryChildren, Parent: &task.folder, Ancestors: task.ancestors, CollectionType: task.collection, Options: task.options, LibraryRoot: task.libraryRoot})
		if err != nil {
			return folderResolution{}, fmt.Errorf("resolve %q: %w", entry.Path, err)
		}
		if candidate == nil {
			continue
		}
		inheritHierarchyNames(candidate, task.folder, task.ancestors)
		if err := validateResolved(&task.folder.ID, candidate); err != nil {
			return folderResolution{}, fmt.Errorf("resolve %q: %w", entry.Path, err)
		}
		if candidate.Path == nil {
			return folderResolution{}, fmt.Errorf("resolve %q: item path is required", entry.Path)
		}
		if err := validateResolvedEntry(entry, candidate, directoryChildren[normalizedEntryPath(entry.Path)]); err != nil {
			return folderResolution{}, fmt.Errorf("resolve %q: %w", entry.Path, err)
		}
		candidatePath := normalizedEntryPath(*candidate.Path)
		if _, duplicate := consumedPaths[candidatePath]; duplicate {
			return folderResolution{}, fmt.Errorf("resolve %q: duplicate path", entry.Path)
		}
		candidateEntries := entryByPath
		if entry.IsDir && candidatePath != normalizedEntryPath(entry.Path) {
			candidateEntries = frozenEntriesByPath(directoryChildren[normalizedEntryPath(entry.Path)])
		}
		if err := consumeAdditionalParts(candidate, candidateEntries, consumedPaths); err != nil {
			return folderResolution{}, fmt.Errorf("resolve %q: %w", entry.Path, err)
		}
		resolved[normalizedEntryPath(entry.Path)] = *candidate
		consumedPaths[candidatePath] = struct{}{}
		sourceEntry := entry
		if entry.IsDir && candidatePath != normalizedEntryPath(entry.Path) {
			for _, child := range directoryChildren[normalizedEntryPath(entry.Path)] {
				if normalizedEntryPath(child.Path) == candidatePath {
					sourceEntry = child
					break
				}
			}
		}
		resolutionSources[candidatePath] = sourceEntry
		refreshSources[candidatePath] = entry
		parts, err := materializeAdditionalParts(*candidate, candidateEntries, entry, resolutionSources, refreshSources)
		if err != nil {
			return folderResolution{}, fmt.Errorf("resolve %q: %w", entry.Path, err)
		}
		stackedParts = append(stackedParts, parts...)
		for _, part := range parts {
			stackedPartIDs[part.ID] = struct{}{}
		}
		if entry.IsDir && candidatePath != normalizedEntryPath(entry.Path) {
			nestedEntries := directoryChildren[normalizedEntryPath(entry.Path)]
			nestedEntryByPath := frozenEntriesByPath(nestedEntries)
			groupedChildren, err := s.chain.ResolveCandidates(ctx, ResolveContext{Entry: entry, Children: nestedEntries, ChildEntries: directoryChildren, Parent: &task.folder, Ancestors: task.ancestors, CollectionType: task.collection, Options: task.options, LibraryRoot: task.libraryRoot})
			if err != nil {
				return folderResolution{}, fmt.Errorf("resolve directory group %q: %w", entry.Path, err)
			}
			hasLocalAlternateProvenance := false
			for _, groupedChild := range groupedChildren {
				if groupedChild.LocalAlternateVersion != nil {
					hasLocalAlternateProvenance = true
					break
				}
			}
			if hasLocalAlternateProvenance {
				for _, groupedChild := range groupedChildren {
					sibling := groupedChild.Item
					if sibling == nil || sibling.IsFolder || sibling.Kind != candidate.Kind {
						continue
					}
					if err := validateResolved(&task.folder.ID, sibling); err != nil {
						return folderResolution{}, fmt.Errorf("resolve directory group %q: %w", entry.Path, err)
					}
					if err := validateResolvedEntry(entry, sibling, nestedEntries); err != nil {
						return folderResolution{}, fmt.Errorf("resolve directory group %q: %w", entry.Path, err)
					}
					siblingPath := normalizedEntryPath(*sibling.Path)
					// The outer directory candidate already consumed and materialized
					// this selected primary's stack. Its provenance still determines
					// local alternate links, but consuming it again would reserve the
					// same immutable listing entries twice.
					if siblingPath == candidatePath {
						if groupedChild.LocalAlternateVersion != nil {
							localAlternates[siblingPath] = *groupedChild.LocalAlternateVersion
						}
						continue
					}
					if _, duplicate := consumedPaths[siblingPath]; duplicate {
						continue
					}
					if err := consumeAdditionalParts(sibling, nestedEntryByPath, consumedPaths); err != nil {
						return folderResolution{}, fmt.Errorf("resolve directory group %q: %w", entry.Path, err)
					}
					inheritHierarchyNames(sibling, task.folder, task.ancestors)
					flattened = append(flattened, *sibling)
					if groupedChild.LocalAlternateVersion != nil {
						localAlternates[siblingPath] = *groupedChild.LocalAlternateVersion
					}
					consumedPaths[siblingPath] = struct{}{}
					for _, child := range nestedEntries {
						if normalizedEntryPath(child.Path) != siblingPath {
							continue
						}
						resolutionSources[siblingPath] = child
						// A flattened item remains physically inside entry. Providers
						// therefore refresh from the directory listing while its probe
						// continues to use the child file entry.
						refreshSources[siblingPath] = entry
						parts, materializeErr := materializeAdditionalParts(*sibling, nestedEntryByPath, entry, resolutionSources, refreshSources)
						if materializeErr != nil {
							return folderResolution{}, fmt.Errorf("resolve directory group %q: %w", entry.Path, materializeErr)
						}
						stackedParts = append(stackedParts, parts...)
						for _, part := range parts {
							stackedPartIDs[part.ID] = struct{}{}
						}
						break
					}
				}
				continue
			}
			for _, child := range directoryChildren[normalizedEntryPath(entry.Path)] {
				if child.IsDir {
					continue
				}
				if _, consumed := consumedPaths[normalizedEntryPath(child.Path)]; consumed {
					continue
				}
				sibling, err := s.chain.Resolve(ctx, ResolveContext{Entry: child, ChildEntries: directoryChildren, Parent: &task.folder, Ancestors: task.ancestors, CollectionType: task.collection, Options: task.options, LibraryRoot: task.libraryRoot})
				if err != nil {
					return folderResolution{}, fmt.Errorf("resolve sibling %q: %w", child.Path, err)
				}
				if sibling == nil || sibling.IsFolder || sibling.Kind != candidate.Kind {
					continue
				}
				if err := validateResolved(&task.folder.ID, sibling); err != nil {
					return folderResolution{}, fmt.Errorf("resolve sibling %q: %w", child.Path, err)
				}
				if err := validateResolvedPath(child.Path, sibling); err != nil {
					return folderResolution{}, fmt.Errorf("resolve sibling %q: %w", child.Path, err)
				}
				siblingPath := normalizedEntryPath(*sibling.Path)
				if _, duplicate := consumedPaths[siblingPath]; duplicate {
					continue
				}
				if err := consumeAdditionalParts(sibling, nestedEntryByPath, consumedPaths); err != nil {
					return folderResolution{}, fmt.Errorf("resolve sibling %q: %w", child.Path, err)
				}
				inheritHierarchyNames(sibling, task.folder, task.ancestors)
				flattened = append(flattened, *sibling)
				consumedPaths[siblingPath] = struct{}{}
				resolutionSources[siblingPath] = child
				refreshSources[siblingPath] = entry
				parts, materializeErr := materializeAdditionalParts(*sibling, nestedEntryByPath, entry, resolutionSources, refreshSources)
				if materializeErr != nil {
					return folderResolution{}, fmt.Errorf("resolve sibling %q: %w", child.Path, materializeErr)
				}
				stackedParts = append(stackedParts, parts...)
				for _, part := range parts {
					stackedPartIDs[part.ID] = struct{}{}
				}
			}
		}
	}
	desired := make([]item.Item, 0, len(resolved))
	for _, entry := range entries {
		if candidate, found := resolved[normalizedEntryPath(entry.Path)]; found {
			desired = append(desired, candidate)
		}
	}
	desired = append(desired, flattened...)
	desired = append(desired, stackedParts...)
	desired, collisions := dropIdentityCollisions(desired)
	for index := range desired {
		inheritHierarchyNames(&desired[index], task.folder, task.ancestors)
	}
	s.tracker.register(len(desired))
	existingByPath := make(map[string]item.Item)
	existing, err := s.directChildren(ctx, task.folder.ID)
	if err != nil {
		return folderResolution{}, fmt.Errorf("read existing folder children: %w", err)
	}
	var preserve []dto.UUID
	for _, previous := range existing {
		if previous.Path != nil {
			existingByPath[normalizedEntryPath(*previous.Path)] = previous
		} else if task.folder.Kind == item.KindSeries && previous.Kind == item.KindSeason {
			preserve = append(preserve, previous.ID)
		}
	}
	// storedByPath adds the stored items that this scan moves under the
	// folder. Upstream keeps the BaseItem when its parent changes, so a moved
	// item keeps its metadata like any other stored item.
	storedByPath := existingByPath
	if s.options.Refresher != nil && !s.remoteBackfill {
		if storedByPath, err = s.withAdoptedItems(ctx, desired, existingByPath); err != nil {
			return folderResolution{}, fmt.Errorf("read moved folder children: %w", err)
		}
	}
	// A resolved item supplies initial filesystem facts only. Existing children
	// retain their persisted lifecycle dates across normal rescans; local metadata
	// may replace DateCreated during Refresh below (for example NFO dateadded).
	preserveScannedLifecycleDates(desired, existingByPath)
	metadataCreated := snapshotDateCreated(desired)
	// dynamicImages lists the candidates whose image refresh runs the
	// dynamic image providers after media probing.
	dynamicImages := make(map[int]RefreshOptions)
	if s.options.Refresher != nil {
		// stamped lists the candidates whose providers all ran without a
		// remote failure. They take one DateLastRefreshed sample after the
		// loop, so the refresh stamp does not move the lifecycle clock
		// between candidates.
		var stamped []int
		for index := range desired {
			candidatePath := normalizedEntryPath(*desired[index].Path)
			entry := refreshSources[candidatePath]
			if previous, found := storedByPath[candidatePath]; found {
				carryStoredProviderIDs(&desired[index], previous)
				rehydrateRefreshLocks(&desired[index], previous)
			}
			directory, refreshEntries := task.entry, entries
			if entry.IsDir {
				directory = entry
				refreshEntries = directoryChildren[normalizedEntryPath(entry.Path)]
			}
			materializeNewFolderLifecycleDate(&desired[index], existingByPath, metadataCreated[candidatePath], s.options.Now)
			nameBefore := snapshotBackfillNames(desired[index], s.remoteBackfill)
			resolvedNames := snapshotRefreshNames(desired[index])
			before := snapshotRefreshIdentity(desired[index])
			itemRefresh := refreshForCandidate(task.refresh, desired[index], task.folder, task.ancestors, task.collectionFolderID)
			refresh := refreshContext(directory, refreshEntries, directoryChildren, task.options, task.libraryRoot, itemRefresh)
			refresh.Ancestors = refreshAncestors(task.folder, task.ancestors)
			// A stored item of another kind is a different upstream item, so
			// it takes a first refresh.
			var stored *item.Item
			if previous, found := storedByPath[candidatePath]; found && previous.Kind == desired[index].Kind {
				stored = &previous
			}
			plan := planMetadataRefresh(itemRefresh, task.options, desired[index], stored, func() bool {
				monitor, ok := s.options.Refresher.(MetadataChangeMonitor)
				return !ok || monitor.LocalMetadataChanged(refresh, *stored)
			}, s.options.Now)
			refresh.SkipRemoteMetadata = plan.skipRemote
			if !s.remoteBackfill && runsDynamicImages(itemRefresh, stored) {
				dynamicImages[index] = itemRefresh
			}
			// Directory candidates receive their own listing as Entries. Their parent
			// listing remains a frozen child fact so local providers can resolve parent
			// conventions (such as season artwork) without another filesystem traversal.
			if entry.IsDir {
				refresh.ChildEntries[normalizedEntryPath(task.entry.Path)] = append([]Entry(nil), entries...)
			}
			if s.counts != nil {
				s.counts.invocations.Add(1)
			}
			outcome, err := refreshWithOutcome(ctx, s.options.Refresher, refresh, &desired[index])
			if err != nil {
				return folderResolution{}, fmt.Errorf("refresh folder: %w", err)
			}
			if err := before.validate(&desired[index]); err != nil {
				before.restore(&desired[index])
				return folderResolution{}, fmt.Errorf("refresh folder: %w", err)
			}
			if previous, found := storedByPath[candidatePath]; found {
				carryStoredImages(&desired[index], previous, itemRefresh, task.libraryRoot)
				if stored != nil && !s.remoteBackfill {
					mergeStoredMetadata(&desired[index], *stored, plan.merge, resolvedNames)
				}
				rehydrateRefreshLocks(&desired[index], previous)
				restoreBackfillSidecarNames(&desired[index], nameBefore, previous)
			}
			if !s.remoteBackfill {
				if carryRefreshed(&desired[index], stored, plan, outcome) {
					stamped = append(stamped, index)
				}
			}
			s.tracker.complete(1)
		}
		if len(stamped) != 0 {
			stamp := scannerLifecycleTimestamp(s.options.Now)
			for _, index := range stamped {
				value := stamp
				desired[index].DateLastRefreshed = &value
			}
		}
	} else {
		for index := range desired {
			candidatePath := normalizedEntryPath(*desired[index].Path)
			materializeNewFolderLifecycleDate(&desired[index], existingByPath, metadataCreated[candidatePath], s.options.Now)
			if previous, found := existingByPath[candidatePath]; found {
				carryStoredProviderIDs(&desired[index], previous)
				carryStoredImages(&desired[index], previous, task.refresh, task.libraryRoot)
				if previous.Kind == desired[index].Kind {
					mergeStoredMetadata(&desired[index], previous, mergeKeepStored, snapshotRefreshNames(desired[index]))
					desired[index].DateLastRefreshed = copyScannedPointer(previous.DateLastRefreshed)
				}
				rehydrateRefreshLocks(&desired[index], previous)
			}
			s.tracker.complete(1)
		}
	}
	for index := range desired {
		if desired[index].Path == nil {
			continue
		}
		entry, found := resolutionSources[normalizedEntryPath(*desired[index].Path)]
		if found {
			applyEntryFileIdentity(&desired[index], entry)
		}
	}
	carryVirtualSeasonLinks(desired, existingByPath)
	canonicalizePeople(desired)
	var media []store.ScannedChildMedia
	if s.remoteBackfill {
		// The remote-backfill mode skips media probing. It re-resolves the
		// library paths for provider enrichment only, so it does not re-probe
		// media the reconcile path retains when the media payload is omitted.
		media = nil
	} else {
		media, err = s.probeResolvedMedia(ctx, desired, resolutionSources, task.entry.Path, entries, directoryChildren, task.options, existingByPath)
		if err != nil {
			return folderResolution{}, fmt.Errorf("probe folder media: %w", err)
		}
		if err := s.refreshDynamicImages(ctx, desired, dynamicImages, media, task.options); err != nil {
			return folderResolution{}, fmt.Errorf("refresh dynamic images: %w", err)
		}
	}
	linkLocalAlternateVersions(desired, existingByPath, localAlternates)
	clearStackPartTopology(desired, stackedPartIDs)
	materializeScannedSortNames(desired)
	materializePresentationKeys(desired, task.folder, task.ancestors)
	materializeScannedSourceFacts(desired, resolutionSources, task.options)
	normalizePlanCredits(desired)
	normalizePlanValues(desired)
	desired, media, quarantined := quarantinePlan(task.folder.ID, desired, media, existingByPath, func(parentID dto.UUID, child item.Item) error {
		return s.items.ValidateReconcileChild(parentID, child)
	})
	quarantinedIDs := make(map[dto.UUID]struct{}, len(quarantined))
	for _, skip := range quarantined {
		quarantinedIDs[skip.ItemID] = struct{}{}
	}
	skipped := append(collisions, quarantined...)
	if err := validatePlan(task.folder.ID, desired); err != nil {
		return folderResolution{}, fmt.Errorf("validate folder plan: %w", err)
	}
	virtuals := append(nfoPeople(desired), taxonomyEntities(task.collection, desired)...)
	clearCreditImageURLs(desired)
	materializeScannedSortNames(virtuals)
	children := make([]scanTask, 0, len(desired))
	for _, candidate := range desired {
		if !candidate.IsFolder || candidate.Path == nil {
			continue
		}
		if _, quarantined := quarantinedIDs[candidate.ID]; quarantined {
			continue
		}
		candidateKey := normalizedEntryPath(*candidate.Path)
		childEntries, found := directoryChildren[candidateKey]
		children = append(children, scanTask{folder: candidate, entry: Entry{Path: *candidate.Path, IsDir: true}, entries: childEntries, hasEntries: found, childEntries: directoryChildren, collection: task.collection, options: task.options, libraryRoot: task.libraryRoot, ancestors: appendAncestor(task.ancestors, task.folder), refresh: task.refresh, collectionFolderID: task.collectionFolderID, branch: visitBranch(task.branch, entryByPath[candidateKey].Identity)})
	}
	before := make(map[dto.UUID]refreshSatelliteSnapshot, len(existingByPath))
	for _, previous := range existingByPath {
		before[previous.ID] = snapshotRefreshSatellites(previous)
	}
	return folderResolution{desired: desired, media: media, virtuals: virtuals, children: children, entryCount: len(entries), skipped: skipped, beforeSatellites: before, preserve: preserve}, nil
}

func scanTaskCollectionFolderID(task scanTask) dto.UUID {
	if len(task.ancestors) != 0 && task.ancestors[0] != nil {
		return task.ancestors[0].ID
	}
	return task.folder.ID
}

func nfoPeople(values []item.Item) []item.Item {
	people := make(map[dto.UUID]item.Item)
	for _, value := range values {
		for _, credit := range value.People {
			entity := PersonEntity(credit.Name)
			existing, exists := people[entity.ID]
			if !exists {
				existing = entity
			}
			// LibraryManager.UpdatePeopleAsync sets PersonInfo.ImageUrl as the
			// Primary image of a person that has none. The store keeps a
			// stored Primary image (mergeVirtualImages).
			if url := strings.TrimSpace(credit.ImageURL); url != "" && len(existing.ImageInfos) == 0 {
				existing.ImageInfos = []item.ImageInfo{{Type: item.ImageTypePrimary, Path: url}}
			}
			// It also sets each PersonInfo.ProviderIds value on the person
			// (SetProviderId when the stored value differs). The store merges
			// them into a stored person (mergeVirtualPerson).
			for _, id := range credit.ProviderIDs.Snapshot() {
				if strings.TrimSpace(id.Value) != "" {
					existing.ProviderIDs.Set(id.Name, id.Value)
				}
			}
			people[entity.ID] = existing
		}
	}
	ids := make([]dto.UUID, 0, len(people))
	for id := range people {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left].String() < ids[right].String() })
	result := make([]item.Item, len(ids))
	for index, id := range ids {
		result[index] = people[id]
	}
	return result
}

// clearCreditImageURLs drops the provider person image URLs and provider IDs
// from credits after nfoPeople moved them to the Person entities. The store
// does not persist them with a credit, so a retained value would make every
// scan look changed.
func clearCreditImageURLs(values []item.Item) {
	for index := range values {
		for credit := range values[index].People {
			values[index].People[credit].ImageURL = ""
			values[index].People[credit].ProviderIDs = item.ProviderIDs{}
		}
	}
}

func taxonomyEntities(collection item.CollectionType, values []item.Item) []item.Item {
	entities := make(map[dto.UUID]item.Item)
	for _, value := range values {
		genreKind := item.KindGenre
		if collection == item.CollectionTypeMusic {
			genreKind = item.KindMusicGenre
		} else if definition, found := item.Definition(value.Kind); found && definition.DefaultMediaType == item.MediaTypeAudio {
			genreKind = item.KindMusicGenre
		}
		for _, name := range value.Values.Genres {
			collectTaxonomyEntity(entities, taxonomyEntity(genreKind, name))
		}
		for _, name := range value.Values.Studios {
			collectTaxonomyEntity(entities, taxonomyEntity(item.KindStudio, name))
		}
	}
	ids := make([]dto.UUID, 0, len(entities))
	for id := range entities {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left].String() < ids[right].String() })
	result := make([]item.Item, len(ids))
	for index, id := range ids {
		result[index] = entities[id]
	}
	return result
}

// collectTaxonomyEntity preserves taxonomyEntity's case-insensitive identity
// while selecting the lexical-minimum raw label for matching identities. That
// makes the persisted presentation casing independent of filesystem traversal.
func collectTaxonomyEntity(entities map[dto.UUID]item.Item, entity item.Item) {
	if entity.ID.IsNil() {
		return
	}
	if existing, found := entities[entity.ID]; !found || *entity.Name < *existing.Name {
		entities[entity.ID] = entity
	}
}

func taxonomyEntity(kind item.Kind, name string) item.Item {
	name = strings.TrimSpace(name)
	if name == "" {
		return item.Item{}
	}
	path := "%MetadataPath%/" + kind.String() + "/" + name
	identityPath := "metadata\\" + strings.ReplaceAll(strings.TrimPrefix(path, "%MetadataPath%/"), "/", "\\")
	result := item.Item{ID: itemID(kind, identityPath, "", false), Kind: kind, Path: &path, Name: &name}
	if keys := item.UserDataKeys(result, item.UserDataInputs{}); len(keys) != 0 {
		result.PresentationUniqueKey = &keys[0]
	}
	return result
}

// linkLocalAlternateVersions applies only the alternate membership and primary
// selection emitted by the resolver for this plan. It clears scanner-owned
// representation for every current Movie and Episode, so a changed resolver
// decision cannot leave an old primary or hidden local link behind.
func linkLocalAlternateVersions(values []item.Item, existing map[string]item.Item, provenance map[string]LocalAlternateVersion) {
	primaryIndexes := make(map[dto.UUID]int, len(values))
	for index := range values {
		candidate := &values[index]
		if !localAlternateVersionKind(candidate.Kind) {
			continue
		}
		candidate.PrimaryVersionID = nil
		priorLinks := candidate.LinkedChildren
		if candidate.Path != nil {
			if previous, found := existing[normalizedEntryPath(*candidate.Path)]; found {
				priorLinks = append(priorLinks, previous.LinkedChildren...)
			}
		}
		candidate.LinkedChildren = linkedAlternateVersions(priorLinks)
		primaryIndexes[candidate.ID] = index
	}
	for index := range values {
		candidate := &values[index]
		if !localAlternateVersionKind(candidate.Kind) || candidate.Path == nil {
			continue
		}
		membership, found := provenance[normalizedEntryPath(*candidate.Path)]
		if !found || membership.Index == 0 {
			continue
		}
		primary, found := primaryIndexes[membership.PrimaryID]
		if !found || primary == index {
			continue
		}
		primaryID := membership.PrimaryID
		candidate.PrimaryVersionID = &primaryID
		if hasLinkedAlternateVersion(values[primary].LinkedChildren, candidate.ID) {
			continue
		}
		sortOrder := membership.Index - 1
		values[primary].LinkedChildren = append(values[primary].LinkedChildren, item.LinkedChildRef{ItemID: candidate.ID, Type: item.LinkedChildTypeLocalAlternateVersion, SortOrder: &sortOrder})
	}
}

func localAlternateVersionKind(kind item.Kind) bool {
	return kind == item.KindMovie || kind == item.KindEpisode
}

// linkedAlternateVersions keeps the relationship type users create manually
// while dropping scanner-owned LocalAlternateVersion links. The same relation
// may be present on the fresh candidate and its persisted snapshot, so retain
// it once by child identity.
func linkedAlternateVersions(values []item.LinkedChildRef) []item.LinkedChildRef {
	result := make([]item.LinkedChildRef, 0, len(values))
	seen := make(map[dto.UUID]struct{}, len(values))
	for _, value := range values {
		if value.Type != item.LinkedChildTypeLinkedAlternateVersion {
			continue
		}
		if _, exists := seen[value.ItemID]; exists {
			continue
		}
		seen[value.ItemID] = struct{}{}
		result = append(result, value)
	}
	return result
}

// hasLinkedAlternateVersion preserves a user-created relation when the
// resolver independently identifies the same child as a local alternate. The
// child's PrimaryVersionID remains resolver-owned, but the existing relation
// is the only child identity materialized in the reconciliation plan.
func hasLinkedAlternateVersion(values []item.LinkedChildRef, childID dto.UUID) bool {
	for _, value := range values {
		if value.ItemID == childID && value.Type == item.LinkedChildTypeLinkedAlternateVersion {
			return true
		}
	}
	return false
}

func refreshContext(directory Entry, entries []Entry, childEntries map[string][]Entry, options LibraryOptions, libraryRoot string, refresh RefreshOptions) RefreshContext {
	return RefreshContext{
		Directory:          directory,
		Entries:            append([]Entry(nil), entries...),
		ChildEntries:       cloneRefreshChildEntries(childEntries),
		Options:            cloneLibraryOptions(options),
		LibraryRoot:        libraryRoot,
		Mode:               refresh.Metadata,
		Image:              refresh.Image,
		ReplaceAllMetadata: refresh.ReplaceAllMetadata,
		ReplaceAllImages:   refresh.ReplaceAllImages,
	}
}

func cloneRefreshChildEntries(entries map[string][]Entry) map[string][]Entry {
	if entries == nil {
		return nil
	}
	clone := make(map[string][]Entry, len(entries))
	for path, children := range entries {
		clone[path] = append([]Entry(nil), children...)
	}
	return clone
}

func cloneLibraryOptions(options LibraryOptions) LibraryOptions {
	options.PathInfos = cloneRefreshSlice(options.PathInfos)
	options.MetadataSavers = cloneRefreshSlice(options.MetadataSavers)
	options.DisabledLocalMetadataReaders = cloneRefreshSlice(options.DisabledLocalMetadataReaders)
	options.LocalMetadataReaderOrder = cloneRefreshSlice(options.LocalMetadataReaderOrder)
	options.DisabledSubtitleFetchers = cloneRefreshSlice(options.DisabledSubtitleFetchers)
	options.SubtitleFetcherOrder = cloneRefreshSlice(options.SubtitleFetcherOrder)
	options.DisabledMediaSegmentProviders = cloneRefreshSlice(options.DisabledMediaSegmentProviders)
	options.MediaSegmentProviderOrder = cloneRefreshSlice(options.MediaSegmentProviderOrder)
	options.SubtitleDownloadLanguages = cloneRefreshSlice(options.SubtitleDownloadLanguages)
	options.DisabledLyricFetchers = cloneRefreshSlice(options.DisabledLyricFetchers)
	options.LyricFetcherOrder = cloneRefreshSlice(options.LyricFetcherOrder)
	options.CustomTagDelimiters = cloneRefreshSlice(options.CustomTagDelimiters)
	options.DelimiterWhitelist = cloneRefreshSlice(options.DelimiterWhitelist)
	options.TypeOptions = cloneRefreshSlice(options.TypeOptions)
	for index := range options.TypeOptions {
		options.TypeOptions[index].MetadataFetchers = cloneRefreshSlice(options.TypeOptions[index].MetadataFetchers)
		options.TypeOptions[index].MetadataFetcherOrder = cloneRefreshSlice(options.TypeOptions[index].MetadataFetcherOrder)
		options.TypeOptions[index].ImageFetchers = cloneRefreshSlice(options.TypeOptions[index].ImageFetchers)
		options.TypeOptions[index].ImageFetcherOrder = cloneRefreshSlice(options.TypeOptions[index].ImageFetcherOrder)
		options.TypeOptions[index].ImageOptions = cloneRefreshSlice(options.TypeOptions[index].ImageOptions)
		options.TypeOptions[index].SimilarItemProviders = cloneRefreshSlice(options.TypeOptions[index].SimilarItemProviders)
		options.TypeOptions[index].SimilarItemProviderOrder = cloneRefreshSlice(options.TypeOptions[index].SimilarItemProviderOrder)
	}
	return options
}

func cloneRefreshSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	clone := make([]T, len(values))
	copy(clone, values)
	return clone
}

type refreshIdentity struct {
	id       dto.UUID
	parentID *dto.UUID
	path     *string
	kind     item.Kind
	isFolder bool
}

func snapshotRefreshIdentity(value item.Item) refreshIdentity {
	identity := refreshIdentity{id: value.ID, kind: value.Kind, isFolder: value.IsFolder}
	identity.parentID = copyRefreshUUID(value.ParentID)
	identity.path = copyRefreshString(value.Path)
	return identity
}

func (identity refreshIdentity) validate(value *item.Item) error {
	if value.ID != identity.id {
		return &RefresherStructuralMutationError{Field: "ID"}
	}
	if !sameRefreshUUID(identity.parentID, value.ParentID) {
		return &RefresherStructuralMutationError{Field: "ParentID"}
	}
	if !sameRefreshString(identity.path, value.Path) {
		return &RefresherStructuralMutationError{Field: "Path"}
	}
	if value.Kind != identity.kind {
		return &RefresherStructuralMutationError{Field: "Kind"}
	}
	if value.IsFolder != identity.isFolder {
		return &RefresherStructuralMutationError{Field: "IsFolder"}
	}
	return nil
}

func (identity refreshIdentity) restore(value *item.Item) {
	value.ID, value.Kind, value.IsFolder = identity.id, identity.kind, identity.isFolder
	value.ParentID, value.Path = copyRefreshUUID(identity.parentID), copyRefreshString(identity.path)
}

// backfillNameSnapshot captures the Name-family fields the resolver sets from
// the filename before the remote refresher runs. The backfill path uses it to
// detect whether the refresher changed these fields: a provider that supplies
// a title overwrites the filename, but a provider that persists only a
// provider ID leaves the filename in place.
type backfillNameSnapshot struct {
	enabled       bool
	name          *string
	sortName      *string
	forcedSort    *string
	originalTitle *string
}

// snapshotBackfillNames captures the Name-family fields when the scan is a
// metadata backfill, and returns a disabled snapshot otherwise. The snapshot
// copies the pointer values so a later mutation of the desired item does not
// change the captured baseline.
func snapshotBackfillNames(value item.Item, backfill bool) backfillNameSnapshot {
	if !backfill {
		return backfillNameSnapshot{}
	}
	return backfillNameSnapshot{
		enabled:       true,
		name:          copyRefreshString(value.Name),
		sortName:      copyRefreshString(value.SortName),
		forcedSort:    copyRefreshString(value.ForcedSortName),
		originalTitle: copyRefreshString(value.OriginalTitle),
	}
}

// restoreBackfillSidecarNames preserves a stored sidecar Name and SortName
// when the remote refresher did not change the resolver-derived filename. A
// provider such as MusicBrainz can persist a provider ID without touching
// Name, so the desired item still carries the filename the resolver set. The
// store merge treats a non-empty desired Name as a replacement, which would
// overwrite the stored sidecar Name with the filename. This restore puts the
// stored value back onto the desired item when the refresher left the
// filename in place, so the merge sees no change and the sidecar value
// survives. A locked field is already restored by rehydrateRefreshLocks, so
// this function skips locked fields.
func restoreBackfillSidecarNames(desired *item.Item, before backfillNameSnapshot, stored item.Item) {
	if !before.enabled || desired == nil {
		return
	}
	if sameRefreshString(desired.Name, before.name) && !hasRefreshLockedField(stored.LockedFields, item.MetadataFieldName) && !stored.IsLocked {
		desired.Name = copyRefreshString(stored.Name)
	}
	if sameRefreshString(desired.SortName, before.sortName) && !hasRefreshLockedField(stored.LockedFields, item.MetadataFieldName) && !stored.IsLocked {
		desired.SortName = copyRefreshString(stored.SortName)
	}
	if sameRefreshString(desired.ForcedSortName, before.forcedSort) && !hasRefreshLockedField(stored.LockedFields, item.MetadataFieldName) && !stored.IsLocked {
		desired.ForcedSortName = copyRefreshString(stored.ForcedSortName)
	}
	if sameRefreshString(desired.OriginalTitle, before.originalTitle) && !hasRefreshLockedField(stored.LockedFields, item.MetadataFieldName) && !stored.IsLocked {
		desired.OriginalTitle = copyRefreshString(stored.OriginalTitle)
	}
}

func sameRefreshUUID(left, right *dto.UUID) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func sameRefreshString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func copyRefreshUUID(value *dto.UUID) *dto.UUID {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyRefreshString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (s *Scanner) deliver(progresses []RefreshProgress) {
	if s.options.Progress == nil {
		return
	}
	for _, progress := range progresses {
		s.options.Progress(progress)
	}
}

func (r *RefreshResult) add(progress RefreshProgress) {
	r.Discovered += progress.Discovered
	r.Inserted += progress.Inserted
	r.Updated += progress.Updated
	r.Deleted += progress.Deleted
	r.BackfillUpdated += progress.BackfillUpdated
	r.BackfillSkipped += progress.BackfillSkipped
	switch progress.Status {
	case "scanned":
		r.Scanned++
	case "inaccessible":
		r.Inaccessible++
	}
	if progress.Error != nil {
		r.Errors = append(r.Errors, progress.Error)
	}
}

// frozenEntriesByPath indexes one scanner-owned directory snapshot. Resolver
// source paths must be facts from that snapshot, never filesystem guesses.
func frozenEntriesByPath(entries []Entry) map[string]Entry {
	result := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		result[normalizedEntryPath(entry.Path)] = entry
	}
	return result
}

// materializeAdditionalParts turns validated stack paths into durable hidden
// children. They intentionally share the primary's classification and metadata
// but retain the physical source identity, probe input, and refresh directory
// appropriate to each individual file.
func materializeAdditionalParts(primary item.Item, entries map[string]Entry, refreshSource Entry, resolutionSources, refreshSources map[string]Entry) ([]item.Item, error) {
	if primary.Path == nil {
		return nil, errors.New("stacked primary has no path")
	}
	parts := make([]item.Item, 0, len(primary.AdditionalParts))
	for _, path := range primary.AdditionalParts {
		partPath := normalizedEntryPath(path)
		entry, found := entries[partPath]
		if !found {
			return nil, fmt.Errorf("additional part path %q is not an enumerated entry", path)
		}
		part := primary
		part.ID = itemID(primary.Kind, entry.Path, "", false)
		part.Path = copyRefreshString(&entry.Path)
		ownerID := primary.ID
		part.OwnerID = &ownerID
		part.PrimaryVersionID = nil
		part.LinkedChildren = nil
		part.AdditionalParts = nil
		part.IsFolder = false
		part.DateCreated = nil
		if !entry.Created.IsZero() {
			created := entry.Created
			part.DateCreated = &created
		}
		part.DateModified = nil
		if !entry.Modified.IsZero() {
			modified := entry.Modified
			part.DateModified = &modified
		}
		size := entry.Size
		part.Size = &size
		applyEntryFileIdentity(&part, entry)
		resolutionSources[partPath] = entry
		refreshSources[partPath] = refreshSource
		parts = append(parts, part)
	}
	return parts, nil
}

// clearStackPartTopology drops scanner-owned alternate topology from an owned
// stack component while retaining user-created LinkedAlternateVersion links.
func clearStackPartTopology(values []item.Item, ids map[dto.UUID]struct{}) {
	for index := range values {
		if _, found := ids[values[index].ID]; !found {
			continue
		}
		values[index].PrimaryVersionID = nil
		values[index].LinkedChildren = linkedAlternateVersions(values[index].LinkedChildren)
		values[index].AdditionalParts = nil
	}
}

// consumeAdditionalParts validates a stacked candidate's secondary sources
// against the frozen listing and reserves them before fallback resolution.
// materializeAdditionalParts then creates one owned item for every reserved
// source, preserving AdditionalParts order on the primary itself.
func consumeAdditionalParts(candidate *item.Item, entries map[string]Entry, consumed map[string]struct{}) error {
	if candidate == nil || candidate.Path == nil {
		return errors.New("resolver returned stacked item without a path")
	}
	primaryPath := normalizedEntryPath(*candidate.Path)
	additional := make(map[string]struct{}, len(candidate.AdditionalParts))
	for _, part := range candidate.AdditionalParts {
		partPath := normalizedEntryPath(part)
		if _, found := entries[partPath]; !found {
			return fmt.Errorf("additional part path %q is not an enumerated entry", part)
		}
		if partPath == primaryPath {
			return fmt.Errorf("additional part path %q duplicates its primary", part)
		}
		if _, duplicate := additional[partPath]; duplicate {
			return fmt.Errorf("duplicate additional part path %q", part)
		}
		if _, claimed := consumed[partPath]; claimed {
			return fmt.Errorf("additional part path %q is already consumed", part)
		}
		additional[partPath] = struct{}{}
	}
	for partPath := range additional {
		consumed[partPath] = struct{}{}
	}
	return nil
}

func validateResolved(parentID *dto.UUID, value *item.Item) error {
	if value == nil {
		return errors.New("resolver returned no item")
	}
	if value.ID.IsNil() {
		return errors.New("resolver returned item without an id")
	}
	if value.Path == nil || strings.TrimSpace(*value.Path) == "" {
		return errors.New("resolver returned item without a path")
	}
	if parentID == nil {
		if value.ParentID != nil {
			return errors.New("virtual folder unexpectedly has a parent")
		}
		return nil
	}
	if value.ParentID == nil || *value.ParentID != *parentID {
		return errors.New("resolver returned item with the wrong parent")
	}
	return nil
}

func validateResolvedPath(expected string, value *item.Item) error {
	if value == nil || value.Path == nil {
		return errors.New("resolver returned item without a path")
	}
	if normalizedEntryPath(*value.Path) != normalizedEntryPath(expected) {
		return fmt.Errorf("resolver returned path %q, want %q", *value.Path, expected)
	}
	return nil
}

// validateResolvedEntry permits a resolver to claim a direct media file while
// resolving its containing directory. The child must come from the directory's
// already-frozen listing; a resolver cannot invent a path or cross directories.
func validateResolvedEntry(entry Entry, value *item.Item, children []Entry) error {
	if err := validateResolvedPath(entry.Path, value); err == nil {
		return nil
	}
	if !entry.IsDir || value == nil || value.Path == nil || value.IsFolder {
		return validateResolvedPath(entry.Path, value)
	}
	path := normalizedEntryPath(*value.Path)
	for _, child := range children {
		if !child.IsDir && normalizedEntryPath(child.Path) == path {
			return nil
		}
	}
	return fmt.Errorf("resolver returned path %q outside directory %q", *value.Path, entry.Path)
}

func normalizedEntryPath(path string) string { return filepath.Clean(path) }

func normalizeEntries(entries []Entry) {
	for index := range entries {
		entries[index].Path = normalizedEntryPath(entries[index].Path)
	}
}

// refreshAncestors copies the identity facts of parent and its ancestors,
// nearest first, for hierarchy-aware providers (upstream SeasonInfo and
// EpisodeInfo SeriesProviderIds). The parent was refreshed before its children
// were scanned, so a series carries the provider ids its own refresh found.
func refreshAncestors(parent item.Item, ancestors []*item.Item) []RefreshAncestor {
	result := make([]RefreshAncestor, 0, len(ancestors)+1)
	result = append(result, NewRefreshAncestor(&parent))
	for index := len(ancestors) - 1; index >= 0; index-- {
		if ancestors[index] != nil {
			result = append(result, NewRefreshAncestor(ancestors[index]))
		}
	}
	return result
}

func appendAncestor(ancestors []*item.Item, parent item.Item) []*item.Item {
	result := make([]*item.Item, 0, len(ancestors)+1)
	result = append(result, ancestors...)
	parentCopy := parent
	return append(result, &parentCopy)
}

func inheritHierarchyNames(value *item.Item, parent item.Item, ancestors []*item.Item) {
	if value == nil || value.SeriesName != nil {
		return
	}
	if parent.SeriesName != nil {
		name := *parent.SeriesName
		value.SeriesName = &name
		return
	}
	if parent.Kind == item.KindSeries && parent.Name != nil {
		name := *parent.Name
		value.SeriesName = &name
		return
	}
	for _, ancestor := range ancestors {
		if ancestor != nil && ancestor.Kind == item.KindSeries && ancestor.Name != nil {
			name := *ancestor.Name
			value.SeriesName = &name
			return
		}
	}
}

// materializePresentationKeys ports MetadataService.BeforeSaveInternal, which
// stores CreatePresentationUniqueKey on every save, and the Season and Episode
// metadata services, which store Series?.PresentationUniqueKey as
// SeriesPresentationUniqueKey. NextUp groups played episodes by that series
// key. It runs after alternate-version linking because a secondary version's
// key is its primary's id.
func materializePresentationKeys(values []item.Item, parent item.Item, ancestors []*item.Item) {
	series := nearestSeries(parent, ancestors)
	in := item.PresentationInputs{Series: series}
	for index := range values {
		value := &values[index]
		created := *value
		created.PresentationUniqueKey = nil
		key := item.PresentationUniqueKey(created, in)
		value.PresentationUniqueKey = &key
		if value.Kind == item.KindEpisode || value.Kind == item.KindSeason {
			value.SeriesPresentationUniqueKey = item.SeriesPresentationUniqueKey(*value, in)
		}
	}
}

// nearestSeries returns the series that owns a folder's children, with its
// presentation key resolved. A series stored before keys were materialized
// has none, so its key is computed as GetPresentationUniqueKey does.
func nearestSeries(parent item.Item, ancestors []*item.Item) *item.Item {
	var found *item.Item
	if parent.Kind == item.KindSeries {
		found = &parent
	} else {
		for index := len(ancestors) - 1; index >= 0; index-- {
			if ancestors[index] != nil && ancestors[index].Kind == item.KindSeries {
				found = ancestors[index]
				break
			}
		}
	}
	if found == nil {
		return nil
	}
	series := *found
	key := item.PresentationUniqueKey(series, item.PresentationInputs{})
	series.PresentationUniqueKey = &key
	return &series
}

// materializeScannedSortName commits the same final sort fact that stock derives
// after resolver and metadata-provider enrichment. It intentionally preserves an
// explicit SortName and leaves nameless items nullable.
func materializeScannedSortName(value *item.Item, options item.SortOptions) {
	if value == nil || value.SortName != nil {
		return
	}
	if sortName, present := item.ComputeSortName(*value, options); present {
		value.SortName = &sortName
	}
}

func materializeScannedSortNames(values []item.Item) {
	options := item.DefaultSortOptions()
	for index := range values {
		materializeScannedSortName(&values[index], options)
	}
}

func (s *Scanner) persistVirtualIfChanged(ctx context.Context, desired item.Item) error {
	materializeScannedSortName(&desired, item.DefaultSortOptions())
	stored, err := s.items.Get(ctx, desired.ID)
	if err != nil {
		if store.IsNotFound(err) {
			materializeVirtualFolderLifecycle(&desired, nil, s.options.Now)
			return s.storeWrite(ctx, func() error { return s.items.Upsert(ctx, desired) })
		}
		return err
	}
	materializeVirtualFolderLifecycle(&desired, &stored, s.options.Now)
	if equivalentVirtualFolder(stored, desired) {
		return nil
	}
	saved := scannerLifecycleTimestamp(s.options.Now)
	desired.DateLastSaved = &saved
	return s.storeWrite(ctx, func() error { return s.items.Upsert(ctx, desired) })
}

// materializeVirtualFolderLifecycle gives scanner-owned virtual roots the same
// durable lifecycle facts as resolved folders. A virtual root has no filesystem
// Entry of its own, so its creation and save facts come from the injected scan
// lifecycle clock. Once persisted, both facts are copied back before comparison
// so an unchanged rescan cannot rewrite them.
func materializeVirtualFolderLifecycle(candidate *item.Item, stored *item.Item, now func() time.Time) {
	if candidate == nil {
		return
	}
	if stored != nil {
		if stored.DateCreated != nil {
			created := *stored.DateCreated
			candidate.DateCreated = &created
		}
		if stored.DateLastSaved != nil {
			saved := *stored.DateLastSaved
			candidate.DateLastSaved = &saved
		}
	}
	var lifecycle *time.Time
	sample := func() time.Time {
		if lifecycle == nil {
			value := scannerLifecycleTimestamp(now)
			lifecycle = &value
		}
		return *lifecycle
	}
	if candidate.DateCreated == nil {
		created := sample()
		candidate.DateCreated = &created
	}
	if candidate.DateLastSaved == nil {
		saved := sample()
		candidate.DateLastSaved = &saved
	}
}

func scannerLifecycleTimestamp(now func() time.Time) time.Time {
	return now().UTC().Truncate(time.Microsecond)
}

func equivalentVirtualFolder(left, right item.Item) bool {
	for _, value := range []*item.Item{&left, &right} {
		value.TopParentID = nil
		value.SeriesID = nil
		value.SeasonID = nil
		value.CleanName = nil
	}
	return reflect.DeepEqual(left, right)
}

// dropIdentityCollisions keeps one candidate per item ID and reports the
// others as skipped. Item IDs hash the lowercased path, so two entries whose
// names differ only in case (for example "Live" and "live" on a
// case-sensitive filesystem) resolve to the same ID. Without this pass the
// folder plan fails validation on every scan, and the folder's whole subtree
// stays out of the library. Upstream resolves each path on its own and
// isolates a failure to that path (LibraryManager.ResolveFileList). The
// candidate with the ordinal-lowest path wins, so the choice does not depend
// on directory listing order.
func dropIdentityCollisions(desired []item.Item) ([]item.Item, []RefreshProgress) {
	winners := make(map[dto.UUID]int, len(desired))
	collided := false
	for index, candidate := range desired {
		previous, found := winners[candidate.ID]
		if !found {
			winners[candidate.ID] = index
			continue
		}
		collided = true
		if plannedPath(candidate) < plannedPath(desired[previous]) {
			winners[candidate.ID] = index
		}
	}
	if !collided {
		return desired, nil
	}
	result := make([]item.Item, 0, len(winners))
	var skipped []RefreshProgress
	for index, candidate := range desired {
		winner := winners[candidate.ID]
		if winner == index {
			result = append(result, candidate)
			continue
		}
		path := plannedPath(candidate)
		skipped = append(skipped, RefreshProgress{
			ItemID: candidate.ID,
			Path:   path,
			Status: "skipped",
			Error:  fmt.Errorf("skip item %s: item id %s is also the id of %s; the paths differ only in case", path, candidate.ID, plannedPath(desired[winner])),
		})
	}
	return result, skipped
}

func plannedPath(candidate item.Item) string {
	if candidate.Path == nil {
		return ""
	}
	return *candidate.Path
}

func validatePlan(parentID dto.UUID, desired []item.Item) error {
	if parentID.IsNil() {
		return errors.New("parent id is required")
	}
	ids := make(map[dto.UUID]struct{}, len(desired))
	paths := make(map[string]struct{}, len(desired))
	for _, candidate := range desired {
		if err := validateResolved(&parentID, &candidate); err != nil {
			return err
		}
		if _, exists := ids[candidate.ID]; exists {
			return fmt.Errorf("duplicate item id %s", candidate.ID)
		}
		ids[candidate.ID] = struct{}{}
		path := normalizedEntryPath(*candidate.Path)
		if _, exists := paths[path]; exists {
			return fmt.Errorf("duplicate item path %q", *candidate.Path)
		}
		paths[path] = struct{}{}
	}
	return nil
}

func readDirectory(path string) ([]Entry, error) {
	return readDirectoryReporting(path, nil)
}

// readDirectoryReporting lists path. An entry whose file status cannot be
// read is skipped and reported to skip, and the other entries stay in the
// listing. Upstream enumerates with IgnoreInaccessible and reads each entry's
// metadata separately (ManagedFileSystem.GetFileSystemEntries), so one
// unreadable entry does not hide its siblings. A transient stat error on a
// network mount (ESTALE, EIO) would otherwise drop the whole directory and
// its subtree from the scan.
//
// A symbolic link is followed. Upstream resolves a file link to its final
// target (ManagedFileSystem.GetFileSystemMetadata), and .NET enumeration
// reports a link to a directory as a directory. The entry keeps the link path
// and carries the facts of the target. A dangling link, or a link loop that
// the kernel rejects with ELOOP, is reported to skip like an unreadable entry.
func readDirectoryReporting(path string, skip func(string, error)) (result []Entry, err error) {
	directory, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := directory.Close(); closeErr != nil && err == nil {
			result = nil
			err = closeErr
		}
	}()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	result = make([]Entry, 0, len(entries))
	for _, entry := range entries {
		entryPath := filepath.Join(path, entry.Name())
		var info os.FileInfo
		if entry.Type()&os.ModeSymlink != 0 {
			info, err = os.Stat(entryPath)
		} else {
			info, err = entry.Info()
		}
		if err != nil {
			if skip != nil {
				skip(entryPath, err)
			}
			continue
		}
		mode := info.Mode()
		if !mode.IsRegular() && !mode.IsDir() {
			continue
		}
		created := entryCreatedAt(entryPath, info)
		result = append(result, Entry{Path: entryPath, IsDir: info.IsDir(), Size: info.Size(), Created: created, Modified: info.ModTime(), Identity: entryFileIdentity(info)})
	}
	return result, nil
}

// directoryIdentity returns the device and inode of the directory at path,
// following links. It returns an invalid identity when the platform does not
// expose one or path cannot be read.
func directoryIdentity(path string) FileIdentity {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return FileIdentity{}
	}
	return entryFileIdentity(info)
}

// visitedDirectory is the device and inode of one directory on a scan branch.
type visitedDirectory struct{ device, inode uint64 }

// visitBranch returns branch extended with identity. The result never shares
// storage with branch, so sibling tasks do not see each other's directories.
func visitBranch(branch []visitedDirectory, identity FileIdentity) []visitedDirectory {
	result := make([]visitedDirectory, len(branch), len(branch)+1)
	copy(result, branch)
	if identity.Valid {
		result = append(result, visitedDirectory{device: identity.Device, inode: identity.Inode})
	}
	return result
}

func onBranch(branch []visitedDirectory, identity FileIdentity) bool {
	if !identity.Valid {
		return false
	}
	for _, visited := range branch {
		if visited.device == identity.Device && visited.inode == identity.Inode {
			return true
		}
	}
	return false
}

// dropDirectoryCycles removes each directory entry that is a directory on
// branch: the directory being listed or one of its ancestors. Only a
// directory link can produce such an entry. Upstream follows the link and
// nests copies of the subtree until path resolution fails with ELOOP. The
// scanner skips the entry instead (D-86).
func (s *Scanner) dropDirectoryCycles(entries []Entry, branch []visitedDirectory) []Entry {
	if len(branch) == 0 {
		return entries
	}
	kept := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir && onBranch(branch, entry.Identity) {
			s.logger.Warn("scanner: skip directory link to an ancestor", "path", entry.Path)
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

func cloneDirectoryEntries(values map[string][]Entry) map[string][]Entry {
	if len(values) == 0 {
		return make(map[string][]Entry)
	}
	result := make(map[string][]Entry, len(values))
	for path, entries := range values {
		result[path] = append([]Entry(nil), entries...)
	}
	return result
}

// preserveScannedLifecycleDates applies the resolver lifecycle for an existing
// child: a fresh filesystem resolution does not replace persistent dates. A
// provider refresh remains later in the pipeline so explicit local metadata
// keeps its higher DateCreated precedence.
func preserveScannedLifecycleDates(values []item.Item, existing map[string]item.Item) {
	for index := range values {
		if values[index].Path == nil {
			continue
		}
		previous, found := existing[normalizedEntryPath(*values[index].Path)]
		if !found {
			continue
		}
		if previous.DateCreated != nil {
			created := *previous.DateCreated
			values[index].DateCreated = &created
		}
		if previous.DateLastSaved != nil {
			saved := *previous.DateLastSaved
			values[index].DateLastSaved = &saved
		}
	}
}

type dateCreatedFact struct {
	value   time.Time
	present bool
}

func snapshotDateCreated(values []item.Item) map[string]dateCreatedFact {
	result := make(map[string]dateCreatedFact, len(values))
	for _, value := range values {
		if value.Path == nil {
			continue
		}
		fact := dateCreatedFact{present: value.DateCreated != nil}
		if fact.present {
			fact.value = *value.DateCreated
		}
		result[normalizedEntryPath(*value.Path)] = fact
	}
	return result
}

// materializeNewFolderLifecycleDate assigns one lifecycle instant to a newly
// discovered directory before metadata refresh. Providers may then replace it
// with an explicit dateadded value; existing items and media retain their
// persisted or resolver-supplied filesystem creation facts.
func materializeNewFolderLifecycleDate(candidate *item.Item, existing map[string]item.Item, beforeMetadata dateCreatedFact, now func() time.Time) {
	if candidate == nil || !candidate.IsFolder || candidate.Path == nil {
		return
	}
	path := normalizedEntryPath(*candidate.Path)
	if _, found := existing[path]; found {
		return
	}
	if beforeMetadata.present != (candidate.DateCreated != nil) || beforeMetadata.present && !beforeMetadata.value.Equal(*candidate.DateCreated) {
		return
	}
	created := now().UTC()
	candidate.DateCreated = &created
}

// collectionType maps a library's .collection marker to its type. No marker
// is upstream's null CollectionType: a mixed-content library. A "mixed" marker
// (CollectionTypeOptions.mixed) also gives null, because SpecialFolderResolver
// parses the marker as CollectionType, which has no mixed value.
func collectionType(value string) (item.CollectionType, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "mixed" {
		return item.CollectionTypeUnknown, true
	}
	for _, candidate := range []item.CollectionType{item.CollectionTypeUnknown, item.CollectionTypeMovies, item.CollectionTypeTVShows, item.CollectionTypeMusic, item.CollectionTypeMusicVideos, item.CollectionTypeTrailers, item.CollectionTypeHomeVideos, item.CollectionTypeBoxSets, item.CollectionTypeBooks, item.CollectionTypePhotos, item.CollectionTypeLiveTV, item.CollectionTypePlaylists, item.CollectionTypeFolders} {
		if candidate.String() == value {
			return candidate, true
		}
	}
	return item.CollectionTypeUnknown, false
}

// scanQueue runs at most ScannerOptions.Concurrency directory plans. It expands
// completed tasks in dispatch order. This makes progress deterministic
// while allowing independent sibling enumeration and resolution in parallel.
type scanQueue struct {
	scanner *Scanner
	ctx     context.Context
}

func newScanQueue(scanner *Scanner, ctx context.Context) scanQueue {
	return scanQueue{scanner: scanner, ctx: ctx}
}

type queuedScanTask struct {
	sequence int
	task     scanTask
}

type queuedScanOutcome struct {
	sequence int
	outcome  scanOutcome
}

func (q scanQueue) run(initial []scanTask, onOutcome func(scanOutcome)) []scanOutcome {
	if len(initial) == 0 {
		return nil
	}
	workers := q.scanner.options.Concurrency
	jobs := make(chan queuedScanTask)
	out := make(chan queuedScanOutcome, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for queued := range jobs {
				if err := q.ctx.Err(); err != nil {
					out <- queuedScanOutcome{sequence: queued.sequence, outcome: scanOutcome{progress: RefreshProgress{Path: queued.task.entry.Path, Status: "failed", Error: err}, collectionFolderID: queued.task.collectionFolderID}}
					continue
				}
				outcome := q.scanner.scanFolder(q.ctx, queued.task)
				outcome.collectionFolderID = queued.task.collectionFolderID
				out <- queuedScanOutcome{sequence: queued.sequence, outcome: outcome}
			}
		}()
	}

	pending := append([]scanTask(nil), initial...)
	outcomes := make([]scanOutcome, 0, len(initial))
	completed := make(map[int]scanOutcome)
	nextSequence := 0
	nextEmit := 0
	active := 0
	for len(pending) != 0 || active != 0 {
		for len(pending) != 0 && active < workers && q.ctx.Err() == nil {
			jobs <- queuedScanTask{sequence: nextSequence, task: pending[0]}
			nextSequence++
			pending = pending[1:]
			active++
		}
		if active == 0 {
			break
		}
		queued := <-out
		active--
		completed[queued.sequence] = queued.outcome
		for {
			outcome, ready := completed[nextEmit]
			if !ready {
				break
			}
			delete(completed, nextEmit)
			nextEmit++
			outcomes = append(outcomes, outcome)
			if onOutcome != nil {
				onOutcome(outcome)
			}
			if outcome.progress.Status == "scanned" {
				pending = append(pending, outcome.children...)
			}
		}
	}
	close(jobs)
	wg.Wait()
	return outcomes
}

// TaxonomyEntity returns the by-name Genre, MusicGenre, or Studio entity for
// name, with the identity LibraryManager.CreateItemByName derives. It does not
// read or write the store.
func TaxonomyEntity(kind item.Kind, name string) item.Item {
	return taxonomyEntity(kind, name)
}
