// SPDX-License-Identifier: GPL-2.0-only

package tasks_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/tasks"
)

func TestTrickplayTaskPagesExactHundredsInSnapshotOrderAndGeneratesWidthsSequentially(t *testing.T) {
	candidates := make([]tasks.Candidate, 201)
	for index := range candidates {
		candidates[index] = trickplayCandidate(index)
	}
	candidates[1].ExistingWidths = []int{320}
	candidates[2].Generate = false

	snapshot := &trickplaySnapshot{values: candidates}
	generator := &trickplayGenerator{}
	reporter := &trickplayReporter{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: snapshot},
		generator,
		reporter,
		tasks.Options{Widths: []int{320, 640}, TileWidth: 4, TileHeight: 3, IntervalMilliseconds: 1_500, Quality: 73},
	)

	if err := task.Execute(t.Context()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got, want := snapshot.pages, []trickplayPageCall{{start: 0, limit: 100}, {start: 100, limit: 100}, {start: 200, limit: 100}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot pages = %#v, want exact 100-item pages %#v", got, want)
	}

	want := make([]tasks.Generation, 0, 399)
	for index, candidate := range candidates {
		if index == 2 {
			continue
		}
		for _, width := range []int{320, 640} {
			if index == 1 && width == 320 {
				continue
			}
			want = append(want, tasks.Generation{
				Candidate:            candidate,
				RequestedWidth:       width,
				TileWidth:            4,
				TileHeight:           3,
				IntervalMilliseconds: 1_500,
				Quality:              73,
			})
		}
	}
	if got := generator.generations; !reflect.DeepEqual(got, want) {
		t.Fatalf("generation sequence = %#v, want candidates and widths in stable serial order %#v", got, want)
	}
	if got, want := reporter.progress[len(reporter.progress)-1], 100.0; got != want {
		t.Fatalf("final progress = %v, want %v after all candidates including skipped and disabled items", got, want)
	}
}

func TestTrickplayTaskDisabledCandidateCountsWithoutGeneration(t *testing.T) {
	candidate := trickplayCandidate(1)
	candidate.Generate = false
	generator := &trickplayGenerator{}
	reporter := &trickplayReporter{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{candidate}}},
		generator,
		reporter,
		tasks.Options{Widths: []int{320}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000},
	)

	if err := task.Execute(t.Context()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := generator.generations; len(got) != 0 {
		t.Fatalf("disabled candidate generated %#v, want no work", got)
	}
	if got, want := reporter.progress, []float64{100}; !reflect.DeepEqual(got, want) {
		t.Fatalf("disabled candidate progress = %#v, want completion %#v", got, want)
	}
}

func TestTrickplayTaskSkipsCandidateOnlyWhenEveryConfiguredWidthExists(t *testing.T) {
	candidate := trickplayCandidate(1)
	candidate.ExistingWidths = []int{320, 640}
	generator := &trickplayGenerator{}
	reporter := &trickplayReporter{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{candidate}}},
		generator,
		reporter,
		tasks.Options{Widths: []int{320, 640}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000},
	)

	if err := task.Execute(t.Context()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := generator.generations; len(got) != 0 {
		t.Fatalf("candidate with every configured width generated %#v, want no work", got)
	}
	if got, want := reporter.progress, []float64{100}; !reflect.DeepEqual(got, want) {
		t.Fatalf("all-width skip progress = %#v, want completion %#v", got, want)
	}
}

func TestTrickplayTaskDispatchesOneGenerationPerEffectiveWidthAndSkipsPersistedEffectiveWidths(t *testing.T) {
	cases := []struct {
		name       string
		configured []int
		videoWidth int
		existing   []int
		wantWidths []int
	}{
		{
			name:       "adjacent requested widths normalize to one even width",
			configured: []int{320, 321},
			videoWidth: 1_920,
			wantWidths: []int{320},
		},
		{
			name:       "widths above source cap share one capped effective rendition",
			configured: []int{640, 641},
			videoWidth: 319,
			wantWidths: []int{640},
		},
		{
			name:       "persisted effective width skips every equivalent configured request",
			configured: []int{320, 321},
			videoWidth: 1_920,
			existing:   []int{320},
			wantWidths: []int{},
		},
		{
			name:       "persisted capped effective width skips requests above source cap",
			configured: []int{640, 641},
			videoWidth: 319,
			existing:   []int{318},
			wantWidths: []int{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := trickplayCandidate(1)
			candidate.VideoWidth = &tc.videoWidth
			candidate.ExistingWidths = tc.existing
			generator := &trickplayGenerator{}
			task := tasks.NewTrickplayTask(
				trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{candidate}}},
				generator,
				&trickplayReporter{},
				tasks.Options{Widths: tc.configured, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000},
			)

			if err := task.Execute(t.Context()); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			gotWidths := make([]int, len(generator.generations))
			for index, generation := range generator.generations {
				gotWidths[index] = generation.RequestedWidth
			}
			if !reflect.DeepEqual(gotWidths, tc.wantWidths) {
				t.Fatalf("dispatched requested widths = %#v, want one configured owner per persisted resolution %#v", gotWidths, tc.wantWidths)
			}
		})
	}
}

// TestTrickplayTaskRegenerateTrickplayDispatchesPersistedWidth proves an
// item-refresh request can re-run one already-persisted rendition without
// changing ordinary scheduled-refresh behavior. The source revision must be
// passed through unchanged so the generator's existing CAS remains authoritative.
func TestTrickplayTaskRegenerateTrickplayDispatchesPersistedWidth(t *testing.T) {
	candidate := trickplayCandidate(1)
	candidate.SourceRevision = "source-revision-before-refresh"
	candidate.ExistingWidths = []int{320}
	generator := &trickplayGenerator{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{candidate}}},
		generator,
		&trickplayReporter{},
		tasks.Options{Widths: []int{320}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73},
	)
	if err := task.RequestRegenerateTrickplay(candidate.ID); err != nil {
		t.Fatalf("RequestRegenerateTrickplay: %v", err)
	}
	candidate.RegenerateTrickplay = true

	if err := task.Execute(t.Context()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := []tasks.Generation{{
		Candidate:            candidate,
		SourceRevision:       candidate.SourceRevision,
		RequestedWidth:       320,
		TileWidth:            10,
		TileHeight:           10,
		IntervalMilliseconds: 10_000,
		Quality:              73,
	}}
	if got := generator.generations; !reflect.DeepEqual(got, want) {
		t.Fatalf("forced persisted-width generations = %#v, want %#v", got, want)
	}
}

func TestTrickplayTaskContinuesAfterItemErrorAndReportsCompletion(t *testing.T) {
	first, second := trickplayCandidate(1), trickplayCandidate(2)
	itemErr := errors.New("ffmpeg failed")
	generator := &trickplayGenerator{errFor: map[dto.UUID]error{first.ID: itemErr}}
	reporter := &trickplayReporter{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{first, second}}},
		generator,
		reporter,
		tasks.Options{Widths: []int{320}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73},
	)

	if err := task.Execute(t.Context()); err != nil {
		t.Fatalf("Execute after item error: %v", err)
	}
	if got, want := generator.generations, []tasks.Generation{
		{Candidate: first, RequestedWidth: 320, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73},
		{Candidate: second, RequestedWidth: 320, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("generation after item error = %#v, want later candidate still generated %#v", got, want)
	}
	if got, want := reporter.itemErrors, []trickplayItemError{{candidate: first, err: itemErr}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reported item errors = %#v, want %#v", got, want)
	}
	if got, want := reporter.progress, []float64{50, 100}; !reflect.DeepEqual(got, want) {
		t.Fatalf("progress after failed item = %#v, want every candidate completion %#v", got, want)
	}
}

// An item's own extraction deadline (#806) is an item failure, not the task's:
// the task reports it and continues with the next item.
func TestTrickplayTaskContinuesAfterItemDeadline(t *testing.T) {
	first, second := trickplayCandidate(1), trickplayCandidate(2)
	itemErr := fmt.Errorf("extract trickplay frames: %w", context.DeadlineExceeded)
	generator := &trickplayGenerator{errFor: map[dto.UUID]error{first.ID: itemErr}}
	reporter := &trickplayReporter{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{first, second}}},
		generator,
		reporter,
		tasks.Options{Widths: []int{320}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73},
	)

	if err := task.Execute(t.Context()); err != nil {
		t.Fatalf("Execute after item deadline: %v", err)
	}
	if got := len(generator.generations); got != 2 {
		t.Fatalf("generations = %d, want both candidates", got)
	}
	if got, want := reporter.itemErrors, []trickplayItemError{{candidate: first, err: itemErr}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reported item errors = %#v, want %#v", got, want)
	}
	if got, want := reporter.progress, []float64{50, 100}; !reflect.DeepEqual(got, want) {
		t.Fatalf("progress = %#v, want %#v", got, want)
	}
}

func TestTrickplayTaskContinuesRemainingWidthsAfterOneWidthFails(t *testing.T) {
	candidate := trickplayCandidate(1)
	itemErr := errors.New("width 320 generation failed")
	generator := &trickplayGenerator{errForGeneration: func(generation tasks.Generation) error {
		if generation.RequestedWidth == 320 {
			return itemErr
		}
		return nil
	}}
	reporter := &trickplayReporter{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{candidate}}},
		generator,
		reporter,
		tasks.Options{Widths: []int{320, 640}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73},
	)

	if err := task.Execute(t.Context()); err != nil {
		t.Fatalf("Execute after width error: %v", err)
	}
	if got, want := generator.generations, []tasks.Generation{
		{Candidate: candidate, RequestedWidth: 320, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73},
		{Candidate: candidate, RequestedWidth: 640, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("generation after first width error = %#v, want later width still generated %#v", got, want)
	}
	if got, want := reporter.itemErrors, []trickplayItemError{{candidate: candidate, err: itemErr}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reported width errors = %#v, want %#v", got, want)
	}
	if got, want := reporter.progress, []float64{100}; !reflect.DeepEqual(got, want) {
		t.Fatalf("width failure progress = %#v, want candidate completion %#v", got, want)
	}
}

func TestTrickplayTaskThreadsExplicitQualityToEveryGeneration(t *testing.T) {
	candidate := trickplayCandidate(1)
	generator := &trickplayGenerator{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{candidate}}},
		generator,
		&trickplayReporter{},
		tasks.Options{Widths: []int{320, 640}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73},
	)

	if err := task.Execute(t.Context()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, generation := range generator.generations {
		if got, want := generation.Quality, 73; got != want {
			t.Fatalf("generation width %d quality = %d, want explicit option %d", generation.RequestedWidth, got, want)
		}
	}
}

func TestTrickplayTaskPreservesCandidateReferenceFrameRateForEveryGeneration(t *testing.T) {
	rates := []float32{23.976, 24, 60}
	candidates := make([]tasks.Candidate, len(rates))
	for index := range rates {
		candidates[index] = trickplayCandidate(index + 1)
		candidates[index].ReferenceFrameRate = &rates[index]
	}
	generator := &trickplayGenerator{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: candidates}},
		generator,
		&trickplayReporter{},
		tasks.Options{Widths: []int{320}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73},
	)

	if err := task.Execute(t.Context()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := len(generator.generations), len(rates); got != want {
		t.Fatalf("generation count = %d, want %d", got, want)
	}
	for index, generation := range generator.generations {
		if generation.ReferenceFrameRate == nil {
			t.Fatalf("generation %d reference frame rate = nil, want %v", index, rates[index])
		}
		if got, want := *generation.ReferenceFrameRate, rates[index]; got != want {
			t.Fatalf("generation %d reference frame rate = %v, want %v", index, got, want)
		}
	}
}

func TestTrickplayTaskPreservesNullableCandidateSourceFactsWithoutChangingScheduling(t *testing.T) {
	size := int64(1_234_567)
	modified := time.Date(2026, time.July, 28, 12, 34, 56, 789_000_000, time.FixedZone("source", -7*60*60))
	candidates := []tasks.Candidate{
		trickplayCandidate(1),
		trickplayCandidate(2),
		trickplayCandidate(3),
		trickplayCandidate(4),
	}
	candidates[1].SourceSize = &size
	candidates[2].SourceModified = &modified
	candidates[3].SourceSize = &size
	candidates[3].SourceModified = &modified

	generator := &trickplayGenerator{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: candidates}},
		generator,
		&trickplayReporter{},
		tasks.Options{Widths: []int{320, 640}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73},
	)

	if err := task.Execute(t.Context()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got, want := len(generator.generations), len(candidates)*2; got != want {
		t.Fatalf("generation count = %d, want normal scheduling across both widths for every candidate = %d", got, want)
	}
	for candidateIndex, candidate := range candidates {
		for widthIndex, width := range []int{320, 640} {
			generation := generator.generations[candidateIndex*2+widthIndex]
			if got := generation.RequestedWidth; got != width {
				t.Fatalf("candidate %d generation %d requested width = %d, want normal scheduling width %d", candidateIndex, widthIndex, got, width)
			}
			if got, want := generation.Candidate.SourceSize, candidate.SourceSize; (got == nil) != (want == nil) || got != nil && *got != *want {
				t.Fatalf("candidate %d generation source size = %v, want %v", candidateIndex, got, want)
			}
			if got, want := generation.Candidate.SourceModified, candidate.SourceModified; (got == nil) != (want == nil) || got != nil && !got.Equal(*want) {
				t.Fatalf("candidate %d generation source modified = %v, want %v", candidateIndex, got, want)
			}
		}
	}
}

// TestTrickplayTaskDefersIncompleteCandidatesAndForcedRegeneration proves an
// absent or partial file identity is not sufficient authority to generate or
// reuse a queued refresh. The forced request remains queued for the next scan
// that carries both mandatory fingerprint facts.
func TestTrickplayTaskDefersIncompleteCandidatesAndForcedRegeneration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*tasks.Candidate)
	}{
		{name: "neither", mutate: func(candidate *tasks.Candidate) { candidate.SourceInode, candidate.SourceChangedAt = nil, nil }},
		{name: "inode only", mutate: func(candidate *tasks.Candidate) { candidate.SourceChangedAt = nil }},
		{name: "changed-at only", mutate: func(candidate *tasks.Candidate) { candidate.SourceInode = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			incomplete := trickplayCandidate(1)
			incomplete.ExistingWidths = []int{320}
			tc.mutate(&incomplete)
			complete := trickplayCandidate(1)
			complete.ExistingWidths = []int{320}
			generator := &trickplayGenerator{}
			reporter := &trickplayReporter{}
			source := &trickplaySequentialSource{snapshots: []*trickplaySnapshot{{values: []tasks.Candidate{incomplete}}, {values: []tasks.Candidate{complete}}}}
			task := tasks.NewTrickplayTask(source, generator, reporter, tasks.Options{Widths: []int{320}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73})

			if err := task.RequestRegenerateTrickplay(incomplete.ID); err != nil {
				t.Fatalf("queue forced regeneration: %v", err)
			}
			if err := task.Execute(t.Context()); err != nil {
				t.Fatalf("Execute incomplete candidate: %v", err)
			}
			if got := generator.generations; len(got) != 0 {
				t.Fatalf("incomplete candidate generations = %#v, want no work", got)
			}
			if got, want := reporter.progress, []float64{100}; !reflect.DeepEqual(got, want) {
				t.Fatalf("incomplete candidate progress = %#v, want %#v", got, want)
			}

			if err := task.Execute(t.Context()); err != nil {
				t.Fatalf("Execute backfilled candidate: %v", err)
			}
			want := []tasks.Generation{{Candidate: func() tasks.Candidate { complete.RegenerateTrickplay = true; return complete }(), SourceRevision: complete.SourceRevision, RequestedWidth: 320, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73}}
			if got := generator.generations; !reflect.DeepEqual(got, want) {
				t.Fatalf("backfilled candidate generations = %#v, want deferred forced refresh %#v", got, want)
			}
		})
	}
}

func TestTrickplayTaskPreservesCandidateSourceRevisionForEveryGeneration(t *testing.T) {
	revisions := []string{"library-scan-a", "library-scan-b", "library-scan-c"}
	candidates := make([]tasks.Candidate, len(revisions))
	for index, revision := range revisions {
		candidates[index] = trickplayCandidate(index + 1)
		candidates[index].SourceRevision = revision
	}
	generator := &trickplayGenerator{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: candidates}},
		generator,
		&trickplayReporter{},
		tasks.Options{Widths: []int{320}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73},
	)

	if err := task.Execute(t.Context()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := len(generator.generations), len(revisions); got != want {
		t.Fatalf("generation count = %d, want %d", got, want)
	}
	for index, generation := range generator.generations {
		if got, want := generation.SourceRevision, revisions[index]; got != want {
			t.Fatalf("generation %d source revision = %q, want %q", index, got, want)
		}
	}
}

func TestTrickplayTaskRejectsInvalidExplicitQualityWithoutGeneration(t *testing.T) {
	generator := &trickplayGenerator{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{trickplayCandidate(1)}}},
		generator,
		&trickplayReporter{},
		tasks.Options{Widths: []int{320}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 101},
	)

	if err := task.Execute(t.Context()); err == nil {
		t.Fatal("Execute accepted invalid explicit quality 101")
	}
	if got := generator.generations; len(got) != 0 {
		t.Fatalf("invalid quality generated %#v, want no work", got)
	}
}

func TestTrickplayTaskChecksCancellationBeforeTheNextCandidate(t *testing.T) {
	first, second := trickplayCandidate(1), trickplayCandidate(2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	generator := &trickplayGenerator{onGenerate: func(generation tasks.Generation) {
		if generation.Candidate.ID == first.ID {
			cancel()
		}
	}}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{first, second}}},
		generator,
		&trickplayReporter{},
		tasks.Options{Widths: []int{320}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73},
	)

	if err := task.Execute(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute cancellation = %v, want context.Canceled", err)
	}
	if got, want := generator.generations, []tasks.Generation{{Candidate: first, RequestedWidth: 320, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 73}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("generations after cancellation = %#v, want no next candidate work %#v", got, want)
	}
}

func TestTrickplayTaskTreatsSourceAndPageErrorsAsFatal(t *testing.T) {
	sourceErr := errors.New("source unavailable")
	pageErr := errors.New("snapshot page unavailable")
	cases := []struct {
		name      string
		source    tasks.Source
		wantErr   error
		wantPages []trickplayPageCall
	}{
		{
			name:    "snapshot",
			source:  trickplaySource{err: sourceErr},
			wantErr: sourceErr,
		},
		{
			name:      "page",
			source:    trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{trickplayCandidate(1)}, pageErr: pageErr}},
			wantErr:   pageErr,
			wantPages: []trickplayPageCall{{start: 0, limit: 100}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			generator := &trickplayGenerator{}
			task := tasks.NewTrickplayTask(tc.source, generator, &trickplayReporter{}, tasks.Options{Widths: []int{320}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000})

			if err := task.Execute(t.Context()); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Execute error = %v, want %v", err, tc.wantErr)
			}
			if got := generator.generations; len(got) != 0 {
				t.Fatalf("generation after %s error = %#v, want no work", tc.name, got)
			}
			if source, ok := tc.source.(trickplaySource); ok && source.snapshot != nil && !reflect.DeepEqual(source.snapshot.pages, tc.wantPages) {
				t.Fatalf("snapshot pages after %s error = %#v, want %#v", tc.name, source.snapshot.pages, tc.wantPages)
			}
		})
	}
}

func TestTrickplayTaskReportsCompleteProgressForEmptySnapshot(t *testing.T) {
	reporter := &trickplayReporter{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{}},
		&trickplayGenerator{},
		reporter,
		tasks.Options{Widths: []int{320}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000},
	)

	if err := task.Execute(t.Context()); err != nil {
		t.Fatalf("Execute empty snapshot: %v", err)
	}
	if got, want := reporter.progress, []float64{100}; !reflect.DeepEqual(got, want) {
		t.Fatalf("empty snapshot progress = %#v, want final completion %#v", got, want)
	}
}

// TestTrickplayTaskScopes pins the two upstream scopes. Execute and Run
// (TrickplayImagesTask) cover every enabled library. ExecuteScan
// (TrickplayProvider) covers only libraries with
// ExtractTrickplayImagesDuringLibraryScan, and drops a forced regeneration
// for an item outside them.
func TestTrickplayTaskScopes(t *testing.T) {
	duringScan := trickplayCandidate(1)
	duringScan.ExtractDuringScan = true
	scheduledOnly := trickplayCandidate(2)
	options := tasks.Options{Widths: []int{320}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 90}
	newTask := func() (*tasks.Task, *trickplayGenerator, *trickplayReporter) {
		generator := &trickplayGenerator{}
		reporter := &trickplayReporter{}
		task := tasks.NewTrickplayTask(
			trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{duringScan, scheduledOnly}}},
			generator, reporter, options,
		)
		return task, generator, reporter
	}
	generatedIDs := func(generator *trickplayGenerator) []dto.UUID {
		var ids []dto.UUID
		for _, generation := range generator.generations {
			ids = append(ids, generation.Candidate.ID)
		}
		return ids
	}

	task, generator, reporter := newTask()
	if err := task.RequestRegenerateTrickplay(scheduledOnly.ID); err != nil {
		t.Fatalf("RequestRegenerateTrickplay: %v", err)
	}
	if err := task.ExecuteScan(t.Context()); err != nil {
		t.Fatalf("ExecuteScan: %v", err)
	}
	if got, want := generatedIDs(generator), []dto.UUID{duringScan.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scan-scope generations = %v, want only the during-scan library item %v", got, want)
	}
	if got, want := reporter.progress, []float64{50, 100}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scan-scope progress = %v, want %v", got, want)
	}
	if err := task.Execute(t.Context()); err != nil {
		t.Fatalf("Execute after dropped regeneration: %v", err)
	}
	for _, generation := range generator.generations[1:] {
		if generation.Candidate.RegenerateTrickplay {
			t.Fatalf("scan scope kept the forced regeneration for %v; upstream TrickplayProvider drops it", generation.Candidate.ID)
		}
	}

	task, generator, _ = newTask()
	var progress []float64
	if err := task.Run(t.Context(), func(value float64) { progress = append(progress, value) }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := generatedIDs(generator), []dto.UUID{duringScan.ID, scheduledOnly.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scheduled-scope generations = %v, want every enabled library item %v", got, want)
	}
	if got, want := progress, []float64{50, 100}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scheduled progress = %v, want %v", got, want)
	}
}

type trickplaySource struct {
	snapshot *trickplaySnapshot
	err      error
}

func (s trickplaySource) Snapshot(context.Context) (tasks.Candidates, error) {
	return s.snapshot, s.err
}

type trickplaySequentialSource struct {
	snapshots []*trickplaySnapshot
	index     int
}

func (s *trickplaySequentialSource) Snapshot(context.Context) (tasks.Candidates, error) {
	if s.index >= len(s.snapshots) {
		return s.snapshots[len(s.snapshots)-1], nil
	}
	snapshot := s.snapshots[s.index]
	s.index++
	return snapshot, nil
}

type trickplayPageCall struct {
	start int
	limit int
}

type trickplaySnapshot struct {
	values  []tasks.Candidate
	pages   []trickplayPageCall
	pageErr error
}

func (s *trickplaySnapshot) Count() int { return len(s.values) }

func (s *trickplaySnapshot) Page(_ context.Context, start, limit int) ([]tasks.Candidate, error) {
	s.pages = append(s.pages, trickplayPageCall{start: start, limit: limit})
	if s.pageErr != nil {
		return nil, s.pageErr
	}
	if start >= len(s.values) {
		return nil, nil
	}
	end := min(start+limit, len(s.values))
	return s.values[start:end], nil
}

type trickplayGenerator struct {
	generations      []tasks.Generation
	errFor           map[dto.UUID]error
	errForGeneration func(tasks.Generation) error
	onGenerate       func(tasks.Generation)
}

func (g *trickplayGenerator) Generate(_ context.Context, generation tasks.Generation) error {
	g.generations = append(g.generations, generation)
	if g.onGenerate != nil {
		g.onGenerate(generation)
	}
	if g.errForGeneration != nil {
		return g.errForGeneration(generation)
	}
	return g.errFor[generation.Candidate.ID]
}

type trickplayItemError struct {
	candidate tasks.Candidate
	err       error
}

type trickplayReporter struct {
	progress   []float64
	itemErrors []trickplayItemError
}

func (r *trickplayReporter) Progress(progress float64) {
	r.progress = append(r.progress, progress)
}

func (r *trickplayReporter) ItemError(candidate tasks.Candidate, err error) {
	r.itemErrors = append(r.itemErrors, trickplayItemError{candidate: candidate, err: err})
}

func trickplayCandidate(index int) tasks.Candidate {
	width := 1_920
	runtime := int64(300_000_000)
	inode := int64(index + 1)
	changedAt := time.Date(2026, time.July, 28, 12, 0, index, 0, time.UTC)
	return tasks.Candidate{
		ID:              dto.UUID{byte(index)},
		Name:            "candidate",
		SourcePath:      "/library/candidate.mkv",
		SourceInode:     &inode,
		SourceChangedAt: &changedAt,
		VideoWidth:      &width,
		RunTimeTicks:    &runtime,
		Generate:        true,
		ExistingWidths:  nil,
	}
}
