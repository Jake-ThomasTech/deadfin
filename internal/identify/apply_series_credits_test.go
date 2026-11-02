// SPDX-License-Identifier: GPL-2.0-only

package identify

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/providers"
)

// TestApplyStoresSeriesWithRepeatedRemoteCredits reproduces #803. A remote
// series result for a long-running show credits one person twice with the
// same kind: two cast entries share a name, and one writer is listed as both
// Writer and Screenplay. It also repeats a genre. The store keys credits on
// (item, person, kind) and values on (item, value), so before the fix
// ItemStore.Upsert failed and the route returned an empty 500. Upstream
// PeopleHelper.AddPerson merges such credits; Apply must store one credit
// for each person and kind, and keep the first role.
func TestApplyStoresSeriesWithRepeatedRemoteCredits(t *testing.T) {
	items := setupTestItems(t)

	seriesDir := filepath.Join(t.TempDir(), "Sesame Street (1969)")
	if err := os.MkdirAll(seriesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	series := item.Item{
		ID:          mustUUID("50000000000000000000000000000502"),
		Kind:        item.KindSeries,
		Name:        ptr("Sesame Street: Play with Me Sesame"),
		Path:        &seriesDir,
		IsFolder:    true,
		ProviderIDs: newProviderIDs("Tmdb", "10480", "Tvdb", "71215"),
	}
	if err := items.Upsert(context.Background(), series); err != nil {
		t.Fatalf("upsert series: %v", err)
	}

	remote := &repeatedCreditsProvider{}
	svc := New(Options{
		Items:     items,
		Refresher: providers.NewRegistry(remote),
		Clock:     clock.NewFake(identifyTestInstant()),
	})
	result := providers.RemoteSearchResult{
		Name:           ptr("Sesame Street"),
		ProductionYear: intptr(1969),
		ProviderIDs:    map[string]string{"Tmdb": "502"},
	}
	if err := svc.Apply(context.Background(), series.ID, result, true); err != nil {
		t.Fatalf("Apply = %v, want nil", err)
	}

	loaded, err := items.GetBatch(context.Background(), []dto.UUID{series.ID})
	if err != nil || len(loaded) != 1 {
		t.Fatalf("GetBatch = %d items, %v", len(loaded), err)
	}
	got := loaded[0]
	if v, _ := got.ProviderIDs.Get("Tmdb"); v != "502" {
		t.Errorf("ProviderIDs[Tmdb] = %q, want 502", v)
	}
	type credit struct {
		name string
		kind item.PersonKind
		role string
	}
	var credits []credit
	for _, person := range got.People {
		role := ""
		if person.Role != nil {
			role = *person.Role
		}
		credits = append(credits, credit{person.Name, person.Type, role})
	}
	want := []credit{
		{"Frank Oz", item.PersonKindActor, "Grover"},
		{"Caroll Spinney", item.PersonKindActor, "Big Bird"},
		{"Jon Stone", item.PersonKindWriter, "Writer"},
	}
	if len(credits) != len(want) {
		t.Fatalf("People = %+v, want %+v", credits, want)
	}
	for index := range want {
		if credits[index] != want[index] {
			t.Errorf("People[%d] = %+v, want %+v", index, credits[index], want[index])
		}
	}
	if len(got.Values.Genres) != 2 || got.Values.Genres[0] != "Kids" || got.Values.Genres[1] != "Family" {
		t.Errorf("Genres = %v, want [Kids Family]", got.Values.Genres)
	}
}

// repeatedCreditsProvider is a remote metadata provider that returns the
// repeated credits and values the TMDb mapping can produce for a long series.
type repeatedCreditsProvider struct{}

func (repeatedCreditsProvider) Name() string { return "TheMovieDb" }
func (repeatedCreditsProvider) Stage() providers.ProviderStage {
	return providers.ProviderStageRemoteMetadata
}
func (repeatedCreditsProvider) Refresh(_ context.Context, _ library.RefreshContext, target *item.Item) (bool, error) {
	credit := func(name string, kind item.PersonKind, role string) item.PersonCredit {
		return item.PersonCredit{PersonID: library.PersonEntity(name).ID, Name: name, Type: kind, Role: &role}
	}
	target.People = []item.PersonCredit{
		credit("Frank Oz", item.PersonKindActor, "Grover"),
		credit("Caroll Spinney", item.PersonKindActor, "Big Bird"),
		credit("Frank Oz", item.PersonKindActor, "Cookie Monster"),
		credit("Jon Stone", item.PersonKindWriter, "Writer"),
		credit("Jon Stone", item.PersonKindWriter, "Screenplay"),
	}
	target.Values.Genres = []string{"Kids", "Family", "kids"}
	return true, nil
}

// TestApplyDoesNotRunLocalMetadataReaders verifies the second half of #803.
// Upstream MetadataService does not run local metadata providers when the
// refresh carries a SearchResult (MetadataService.cs:768-769). A stale
// tvshow.nfo must not put back the provider IDs that Identify replaced, or the
// remote stage refreshes the old match.
func TestApplyDoesNotRunLocalMetadataReaders(t *testing.T) {
	items := setupTestItems(t)

	seriesDir := filepath.Join(t.TempDir(), "Show")
	if err := os.MkdirAll(seriesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	series := item.Item{
		ID:          mustUUID("50000000000000000000000000000503"),
		Kind:        item.KindSeries,
		Name:        ptr("Old Match"),
		Path:        &seriesDir,
		IsFolder:    true,
		ProviderIDs: newProviderIDs("Tmdb", "10480"),
	}
	if err := items.Upsert(context.Background(), series); err != nil {
		t.Fatalf("upsert series: %v", err)
	}

	nfo := &staleNFOReader{}
	remote := &providerIDRecorder{}
	svc := New(Options{
		Items:     items,
		Refresher: providers.NewRegistry(nfo, remote),
		Clock:     clock.NewFake(identifyTestInstant()),
	})
	result := providers.RemoteSearchResult{ProviderIDs: map[string]string{"Tmdb": "502"}}
	if err := svc.Apply(context.Background(), series.ID, result, true); err != nil {
		t.Fatalf("Apply = %v, want nil", err)
	}
	if nfo.called {
		t.Error("local metadata reader ran during Apply, want it skipped")
	}
	if remote.tmdb != "502" {
		t.Errorf("remote provider saw Tmdb = %q, want 502", remote.tmdb)
	}
	loaded, err := items.GetBatch(context.Background(), []dto.UUID{series.ID})
	if err != nil || len(loaded) != 1 {
		t.Fatalf("GetBatch = %d items, %v", len(loaded), err)
	}
	if v, _ := loaded[0].ProviderIDs.Get("Tmdb"); v != "502" {
		t.Errorf("stored ProviderIDs[Tmdb] = %q, want 502", v)
	}
}

// staleNFOReader is a local metadata reader that restores old provider IDs,
// as a stale tvshow.nfo does.
type staleNFOReader struct{ called bool }

func (*staleNFOReader) Name() string { return "Nfo" }
func (*staleNFOReader) Stage() providers.ProviderStage {
	return providers.ProviderStageLocalMetadata
}
func (r *staleNFOReader) Refresh(_ context.Context, _ library.RefreshContext, target *item.Item) (bool, error) {
	r.called = true
	target.ProviderIDs = newProviderIDs("Tmdb", "10480")
	return true, nil
}

// providerIDRecorder records the Tmdb ID the remote stage receives.
type providerIDRecorder struct{ tmdb string }

func (*providerIDRecorder) Name() string { return "TheMovieDb" }
func (*providerIDRecorder) Stage() providers.ProviderStage {
	return providers.ProviderStageRemoteMetadata
}
func (r *providerIDRecorder) Refresh(_ context.Context, _ library.RefreshContext, target *item.Item) (bool, error) {
	r.tmdb, _ = target.ProviderIDs.Get("Tmdb")
	return true, nil
}
