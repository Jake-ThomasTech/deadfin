// SPDX-License-Identifier: GPL-2.0-only

package tasks

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
)

const trickplayPageSize = 100

// ProcessPriority is the requested operating-system scheduling priority for
// trickplay generation.
type ProcessPriority uint8

const (
	// ProcessPriorityNormal leaves trickplay generation at normal priority.
	ProcessPriorityNormal ProcessPriority = iota
)

// Options controls trickplay image generation.
type Options struct {
	IntervalMilliseconds int64
	Widths               []int
	TileWidth            int
	TileHeight           int
	Quality              int
	ProcessPriority      ProcessPriority
}

// DefaultOptions returns fresh default trickplay generation options.
func DefaultOptions() Options {
	return Options{
		IntervalMilliseconds: 10_000,
		Widths:               []int{320},
		TileWidth:            10,
		TileHeight:           10,
		Quality:              90,
		ProcessPriority:      ProcessPriorityNormal,
	}
}

// Candidate is one library video considered by a trickplay refresh.
type Candidate struct {
	ID                 dto.UUID
	Name               string
	SourcePath         string
	SourceRevision     string
	SourceSize         *int64
	SourceModified     *time.Time
	SourceInode        *int64
	SourceChangedAt    *time.Time
	VideoWidth         *int
	RunTimeTicks       *int64
	ReferenceFrameRate *float32
	Generate           bool
	// ExtractDuringScan reports the library's
	// ExtractTrickplayImagesDuringLibraryScan option. Scan-scope executions
	// (TrickplayProvider.cs) generate only for these candidates.
	ExtractDuringScan   bool
	RegenerateTrickplay bool
	ExistingWidths      []int
	// Discover reports that the item's library saves trickplay with the
	// media (SaveTrickplayWithMedia). Before it generates, the task
	// catalogues the existing media-side trickplay, as upstream
	// TrickplayManager.RefreshTrickplayDataAsync does when it does not replace.
	Discover bool
}

// Candidates is the stable result set used for one task execution.
type Candidates interface {
	Count() int
	Page(context.Context, int, int) ([]Candidate, error)
}

// Source creates a stable candidate result set for one task execution.
type Source interface {
	Snapshot(context.Context) (Candidates, error)
}

// Generation describes one candidate resolution requested from a Generator.
type Generation struct {
	Candidate            Candidate
	SourceRevision       string
	RequestedWidth       int
	TileWidth            int
	TileHeight           int
	IntervalMilliseconds int64
	Quality              int
	ReferenceFrameRate   *float32
}

// Generator creates one requested trickplay resolution.
type Generator interface {
	Generate(context.Context, Generation) error
}

// Discoverer persists trickplay saved with the media of a candidate and
// returns the widths it added. A Generator that also implements Discoverer is
// asked to discover before it generates.
type Discoverer interface {
	Discover(context.Context, Candidate) ([]int, error)
}

// Reporter receives task progress and item-level generation failures.
type Reporter interface {
	Progress(float64)
	ItemError(Candidate, error)
}

// Task refreshes missing trickplay resolutions for a snapshot of candidates.
type Task struct {
	source    Source
	generator Generator
	reporter  Reporter
	options   Options

	// running serializes executions from the scheduler and the scan runner.
	running chan struct{}

	regenerationMu sync.Mutex
	regenerations  map[dto.UUID]struct{}
}

// NewTrickplayTask constructs a trickplay refresh task.
func NewTrickplayTask(source Source, generator Generator, reporter Reporter, options Options) *Task {
	return &Task{source: source, generator: generator, reporter: reporter, options: options, running: make(chan struct{}, 1)}
}

// RequestRegenerateTrickplay records one item for forced regeneration during
// the next execution. The task owns this intent queue so manual requests reuse
// the same serialized runner as scheduled refreshes.
func (task *Task) RequestRegenerateTrickplay(itemID dto.UUID) error {
	if task == nil {
		return errors.New("trickplay task is required")
	}
	if itemID.IsNil() {
		return errors.New("trickplay regeneration item id is required")
	}
	task.regenerationMu.Lock()
	defer task.regenerationMu.Unlock()
	if task.regenerations == nil {
		task.regenerations = make(map[dto.UUID]struct{})
	}
	task.regenerations[itemID] = struct{}{}
	return nil
}

// Execute refreshes candidates in snapshot and configured-width order. Source
// and page failures stop the task; failures for an individual candidate are
// reported and processing continues with the next candidate. It has the scope
// of upstream TrickplayImagesTask: every library with
// EnableTrickplayImageExtraction.
func (task *Task) Execute(ctx context.Context) error {
	return task.execute(ctx, false, nil)
}

// ExecuteScan has the scope of upstream TrickplayProvider, which runs during a
// library scan or an item refresh: only libraries with
// ExtractTrickplayImagesDuringLibraryScan. A forced regeneration of an item in
// another library is dropped, as upstream does (TrickplayProvider.cs).
func (task *Task) ExecuteScan(ctx context.Context) error {
	return task.execute(ctx, true, nil)
}

// Run executes the task for the scheduler ("Generate Trickplay Images") and
// reports progress to the scheduler as well as to the task reporter.
func (task *Task) Run(ctx context.Context, progress func(float64)) error {
	return task.execute(ctx, false, progress)
}

func (task *Task) execute(ctx context.Context, scanScope bool, progress func(float64)) (err error) {
	if err := contextError(ctx); err != nil {
		return err
	}
	if task == nil || task.source == nil {
		return errors.New("trickplay task source is required")
	}
	if task.generator == nil {
		return errors.New("trickplay task generator is required")
	}
	if task.options.Quality < 0 || task.options.Quality > 100 {
		return errors.New("trickplay quality must be between 0 and 100")
	}
	if task.running != nil {
		select {
		case task.running <- struct{}{}:
			defer func() { <-task.running }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	report := func(value float64) {
		task.reportProgress(value)
		if progress != nil {
			progress(value)
		}
	}

	regenerations := task.claimRegenerations()
	if len(regenerations) != 0 {
		defer func() {
			if err != nil {
				task.restoreRegenerations(regenerations)
			}
		}()
	}

	candidates, err := task.source.Snapshot(ctx)
	if err != nil {
		return err
	}
	if candidates == nil {
		return errors.New("trickplay candidate snapshot is required")
	}

	count := candidates.Count()
	if count <= 0 {
		report(100)
		return nil
	}

	var deferredRegenerations map[dto.UUID]struct{}
	complete := 0
	for start := 0; start < count; start += trickplayPageSize {
		if err := contextError(ctx); err != nil {
			return err
		}

		page, err := candidates.Page(ctx, start, trickplayPageSize)
		if err != nil {
			return err
		}
		for _, candidate := range page {
			if err := contextError(ctx); err != nil {
				return err
			}
			_, requested := regenerations[candidate.ID]
			if scanScope && !candidate.ExtractDuringScan {
				candidate.Generate = false
				candidate.Discover = false
				requested = false
			}
			if !hasCompleteSourceFingerprint(candidate) {
				candidate.RegenerateTrickplay = false
				if requested {
					if deferredRegenerations == nil {
						deferredRegenerations = make(map[dto.UUID]struct{})
					}
					deferredRegenerations[candidate.ID] = struct{}{}
				}
			} else {
				candidate.RegenerateTrickplay = candidate.RegenerateTrickplay || requested
			}
			if err := task.generateCandidate(ctx, candidate); err != nil {
				return err
			}
			complete++
			report(100 * float64(complete) / float64(count))
		}
	}
	if len(deferredRegenerations) != 0 {
		task.restoreRegenerations(deferredRegenerations)
	}
	return nil
}

func (task *Task) generateCandidate(ctx context.Context, candidate Candidate) error {
	if !hasCompleteSourceFingerprint(candidate) {
		return nil
	}
	if discoverer, ok := task.generator.(Discoverer); ok && candidate.Discover && !candidate.RegenerateTrickplay {
		widths, err := discoverer.Discover(ctx, candidate)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			task.reportItemError(candidate, err)
			return nil
		}
		if len(widths) != 0 {
			candidate.ExistingWidths = append(append([]int(nil), candidate.ExistingWidths...), widths...)
		}
	}
	if !candidate.Generate {
		return nil
	}

	var seenWidths map[int]struct{}
	if len(task.options.Widths) > 1 {
		seenWidths = make(map[int]struct{}, len(task.options.Widths))
	}

	for _, width := range task.options.Widths {
		if err := contextError(ctx); err != nil {
			return err
		}
		effectiveWidth := effectiveTrickplayWidth(candidate.VideoWidth, width)
		if !candidate.RegenerateTrickplay && containsTrickplayWidth(candidate.ExistingWidths, effectiveWidth) {
			continue
		}
		if seenWidths != nil {
			if _, seen := seenWidths[effectiveWidth]; seen {
				continue
			}
			seenWidths[effectiveWidth] = struct{}{}
		}

		err := task.generator.Generate(ctx, Generation{
			Candidate:            candidate,
			SourceRevision:       candidate.SourceRevision,
			ReferenceFrameRate:   cloneFloat32(candidate.ReferenceFrameRate),
			RequestedWidth:       width,
			TileWidth:            task.options.TileWidth,
			TileHeight:           task.options.TileHeight,
			IntervalMilliseconds: task.options.IntervalMilliseconds,
			Quality:              task.options.Quality,
		})
		if err == nil {
			continue
		}
		// Only the task's own cancellation stops the task. An item that
		// fails, also by its own extraction deadline, is reported and the
		// task continues, as upstream TrickplayImagesTask does.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		task.reportItemError(candidate, err)
	}
	return contextError(ctx)
}

func hasCompleteSourceFingerprint(candidate Candidate) bool {
	return candidate.SourceInode != nil && candidate.SourceChangedAt != nil
}

func (task *Task) claimRegenerations() map[dto.UUID]struct{} {
	task.regenerationMu.Lock()
	defer task.regenerationMu.Unlock()
	regenerations := task.regenerations
	task.regenerations = nil
	return regenerations
}

func (task *Task) restoreRegenerations(regenerations map[dto.UUID]struct{}) {
	task.regenerationMu.Lock()
	defer task.regenerationMu.Unlock()
	if task.regenerations == nil {
		task.regenerations = regenerations
		return
	}
	for itemID := range regenerations {
		task.regenerations[itemID] = struct{}{}
	}
}

func (task *Task) reportProgress(progress float64) {
	if task.reporter != nil {
		task.reporter.Progress(progress)
	}
}

func (task *Task) reportItemError(candidate Candidate, err error) {
	if task.reporter != nil {
		task.reporter.ItemError(candidate, err)
	}
}

func containsTrickplayWidth(widths []int, width int) bool {
	for _, existing := range widths {
		if existing == width {
			return true
		}
	}
	return false
}

func effectiveTrickplayWidth(videoWidth *int, requestedWidth int) int {
	width := 2 * (requestedWidth / 2)
	if videoWidth != nil && *videoWidth < requestedWidth {
		width = 2 * (*videoWidth / 2)
	}
	return width
}

func cloneFloat32(value *float32) *float32 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("trickplay context is required")
	}
	return ctx.Err()
}
