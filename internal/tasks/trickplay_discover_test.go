// SPDX-License-Identifier: GPL-2.0-only

package tasks_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/tasks"
)

type discoveringGenerator struct {
	trickplayGenerator
	widths     map[dto.UUID][]int
	errFor     map[dto.UUID]error
	discovered []dto.UUID
}

func (g *discoveringGenerator) Discover(_ context.Context, candidate tasks.Candidate) ([]int, error) {
	g.discovered = append(g.discovered, candidate.ID)
	return g.widths[candidate.ID], g.errFor[candidate.ID]
}

func discoverOptions() tasks.Options {
	return tasks.Options{Widths: []int{320}, TileWidth: 10, TileHeight: 10, IntervalMilliseconds: 10_000, Quality: 90}
}

// Existing trickplay saved with the media is catalogued before generation,
// so a discovered width is not generated again (#806).
func TestTrickplayTaskDiscoversBeforeItGenerates(t *testing.T) {
	found, missing, disabled := trickplayCandidate(1), trickplayCandidate(2), trickplayCandidate(3)
	found.Discover, missing.Discover = true, true
	disabled.Discover, disabled.Generate = true, false
	generator := &discoveringGenerator{widths: map[dto.UUID][]int{found.ID: {320}, disabled.ID: {320}}}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{found, missing, disabled}}},
		generator, &trickplayReporter{}, discoverOptions(),
	)

	if err := task.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := []dto.UUID{found.ID, missing.ID, disabled.ID}; !reflect.DeepEqual(generator.discovered, want) {
		t.Fatalf("discovered = %v, want every Discover candidate %v", generator.discovered, want)
	}
	if len(generator.generations) != 1 || generator.generations[0].Candidate.ID != missing.ID {
		t.Fatalf("generations = %+v, want only the candidate without media-side trickplay", generator.generations)
	}
}

func TestTrickplayTaskDoesNotDiscoverWhenReplacingOrOutOfScanScope(t *testing.T) {
	regenerate, noDiscover, notDuringScan := trickplayCandidate(1), trickplayCandidate(2), trickplayCandidate(3)
	regenerate.Discover, regenerate.RegenerateTrickplay, regenerate.ExtractDuringScan = true, true, true
	noDiscover.ExtractDuringScan = true
	notDuringScan.Discover = true
	generator := &discoveringGenerator{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{regenerate, noDiscover, notDuringScan}}},
		generator, &trickplayReporter{}, discoverOptions(),
	)

	if err := task.ExecuteScan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(generator.discovered) != 0 {
		t.Fatalf("discovered = %v, want none", generator.discovered)
	}
}

func TestTrickplayTaskReportsDiscoveryFailureAndContinues(t *testing.T) {
	broken, next := trickplayCandidate(1), trickplayCandidate(2)
	broken.Discover, next.Discover = true, true
	discoverErr := errors.New("read trickplay tiles: permission denied")
	generator := &discoveringGenerator{errFor: map[dto.UUID]error{broken.ID: discoverErr}}
	reporter := &trickplayReporter{}
	task := tasks.NewTrickplayTask(
		trickplaySource{snapshot: &trickplaySnapshot{values: []tasks.Candidate{broken, next}}},
		generator, reporter, discoverOptions(),
	)

	if err := task.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := []trickplayItemError{{candidate: broken, err: discoverErr}}; !reflect.DeepEqual(reporter.itemErrors, want) {
		t.Fatalf("item errors = %+v, want %+v", reporter.itemErrors, want)
	}
	if len(generator.generations) != 1 || generator.generations[0].Candidate.ID != next.ID {
		t.Fatalf("generations = %+v, want only the next candidate", generator.generations)
	}
}
