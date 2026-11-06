// SPDX-License-Identifier: GPL-2.0-only

package livetv_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/livetv"
	"github.com/deadfin-org/deadfin/internal/livetv/catalog"
	"github.com/deadfin-org/deadfin/internal/livetv/fetch"
	"github.com/deadfin-org/deadfin/internal/livetv/guide"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

var testNow = time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)

// memoryConfig is an in-memory livetv.Config.
type memoryConfig struct {
	mu    sync.Mutex
	value config.LiveTvOptions
}

func (c *memoryConfig) LiveTv() config.LiveTvOptions {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copyOptions(c.value)
}

func (c *memoryConfig) UpdateLiveTv(_ context.Context, update func(*config.LiveTvOptions) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := copyOptions(c.value)
	if err := update(&next); err != nil {
		return err
	}
	c.value = next
	return nil
}

func copyOptions(value config.LiveTvOptions) config.LiveTvOptions {
	value.TunerHosts = slices.Clone(value.TunerHosts)
	value.ListingProviders = slices.Clone(value.ListingProviders)
	for index := range value.ListingProviders {
		value.ListingProviders[index].ChannelMappings = slices.Clone(value.ListingProviders[index].ChannelMappings)
	}
	return value
}

// memoryItems is an in-memory livetv.Items with the ReconcileChildren
// semantics of store.ItemStore.
type memoryItems struct {
	mu     sync.Mutex
	items  map[dto.UUID]item.Item
	writes int
}

func newMemoryItems() *memoryItems { return &memoryItems{items: make(map[dto.UUID]item.Item)} }

func (m *memoryItems) Item(_ context.Context, id dto.UUID) (item.Item, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.items[id]
	return value, ok, nil
}

func (m *memoryItems) Upsert(_ context.Context, value item.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[value.ID] = value
	m.writes++
	return nil
}

func (m *memoryItems) Children(_ context.Context, parentID dto.UUID) ([]item.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.children(parentID), nil
}

func (m *memoryItems) children(parentID dto.UUID) []item.Item {
	var out []item.Item
	for _, value := range m.items {
		if value.ParentID != nil && *value.ParentID == parentID {
			out = append(out, value)
		}
	}
	slices.SortFunc(out, func(a, b item.Item) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	return out
}

func (m *memoryItems) ReplaceChildren(_ context.Context, parentID dto.UUID, desired []item.Item) (livetv.Changes, error) {
	return m.reconcile(parentID, desired, false)
}

func (m *memoryItems) MergeChildren(_ context.Context, parentID dto.UUID, desired []item.Item) (livetv.Changes, error) {
	return m.reconcile(parentID, desired, true)
}

func (m *memoryItems) reconcile(parentID dto.UUID, desired []item.Item, keep bool) (livetv.Changes, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var changes livetv.Changes
	want := make(map[dto.UUID]struct{}, len(desired))
	for _, value := range desired {
		if _, dup := want[value.ID]; dup {
			return changes, errors.New("duplicate child " + value.ID.String())
		}
		want[value.ID] = struct{}{}
		if value.ParentID == nil || *value.ParentID != parentID {
			return changes, errors.New("child of another parent")
		}
		previous, ok := m.items[value.ID]
		switch {
		case !ok:
			changes.Inserted++
		case !reflect.DeepEqual(previous, value):
			changes.Updated++
		default:
			continue
		}
		m.items[value.ID] = value
		m.writes++
	}
	for _, child := range m.children(parentID) {
		if _, ok := want[child.ID]; !ok && !keep {
			m.deleteTree(child.ID)
			changes.Deleted++
		}
	}
	return changes, nil
}

func (m *memoryItems) deleteTree(id dto.UUID) {
	for _, child := range m.children(id) {
		m.deleteTree(child.ID)
	}
	delete(m.items, id)
}

func (m *memoryItems) kinds() map[item.Kind]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[item.Kind]int)
	for _, value := range m.items {
		out[value.Kind]++
	}
	return out
}

type fixture struct {
	config  *memoryConfig
	items   *memoryItems
	clock   *clock.Fake
	service *livetv.Service
	queued  int
	cache   string
}

func newFixture(t *testing.T, options config.LiveTvOptions, hosts []tuner.Host, listings ...livetv.ListingsProvider) *fixture {
	t.Helper()
	f := &fixture{
		config: &memoryConfig{value: options},
		items:  newMemoryItems(),
		clock:  clock.NewFake(testNow),
		cache:  t.TempDir(),
	}
	if hosts == nil {
		client, err := fetch.New(fetch.Options{HTTP: &http.Client{}, Clock: f.clock, UserAgent: "Deadfin-test"})
		if err != nil {
			t.Fatal(err)
		}
		hosts = []tuner.Host{tuner.NewM3UHost(client, f.clock, nil)}
		if listings == nil {
			listings = []livetv.ListingsProvider{guide.NewXMLTV(client, f.clock, f.cache)}
		}
	}
	ids := 0
	service, err := livetv.New(livetv.Options{
		Config:   f.config,
		Tuners:   tuner.NewRegistry(hosts...),
		Listings: listings,
		Items:    f.items,
		Clock:    f.clock,
		Paths:    livetv.Paths{ProgramData: "/data", InternalMetadata: "/data/metadata", Cache: f.cache},
		Logger:   slog.New(slog.DiscardHandler),
		Queue:    func() { f.queued++ },
		NewID: func() string {
			ids++
			return strings.Repeat("0", 31) + string(rune('0'+ids))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	return f
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("guide", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func fakehdhrOptions(t *testing.T) config.LiveTvOptions {
	options := config.DefaultLiveTvOptions()
	days := 1
	options.GuideDays = &days
	host := config.DefaultTunerHostInfo()
	host.Id, host.Type, host.Url = "tuner1", "m3u", fixturePath(t, "fakehdhr-quirks.m3u")
	options.TunerHosts = []config.TunerHostInfo{host}
	provider := config.DefaultListingsProviderInfo()
	provider.Id, provider.Type, provider.Path = "guide1", "xmltv", fixturePath(t, "fakehdhr-quirks.xml")
	options.ListingProviders = []config.ListingsProviderInfo{provider}
	return options
}

func TestRefreshGuideStoresFakehdhrGuide(t *testing.T) {
	f := newFixture(t, fakehdhrOptions(t), nil)
	var progress []float64
	result, err := f.service.RefreshGuide(t.Context(), func(value float64) { progress = append(progress, value) })
	if err != nil {
		t.Fatal(err)
	}
	if result.Channels != 14 || result.Kept {
		t.Fatalf("result %+v", result)
	}
	kinds := f.items.kinds()
	if kinds[item.KindUserView] != 1 || kinds[item.KindLiveTvChannel] != 14 || kinds[item.KindLiveTvProgram] != result.Programs {
		t.Fatalf("stored kinds %v, result %+v", kinds, result)
	}
	if result.Inserted != 14+result.Programs || result.Updated != 0 || result.Deleted != 0 {
		t.Fatalf("changes %+v", result.Changes)
	}
	if len(progress) == 0 || progress[len(progress)-1] != 100 {
		t.Fatalf("progress %v", progress)
	}

	folder := catalog.Folder("/data/metadata", "/data", testNow)
	channels, _ := f.items.Children(t.Context(), folder.ID)
	start, end := guide.RefreshWindow(testNow, fakehdhrOptions(t).GuideDays)
	withPrograms := 0
	for _, channel := range channels {
		programs, _ := f.items.Children(t.Context(), channel.ID)
		if len(programs) > 0 {
			withPrograms++
		}
		for _, program := range programs {
			if program.ChannelID == nil || *program.ChannelID != channel.ID {
				t.Fatalf("program %v on channel %v", program.ChannelID, channel.ID)
			}
			if program.ExternalID == nil || !strings.HasSuffix(*program.ExternalID, "_"+*channel.ExternalID) {
				t.Fatalf("program external id %v, channel %v", program.ExternalID, *channel.ExternalID)
			}
			if program.ID != catalog.ProgramID(*program.ExternalID) {
				t.Fatalf("program id %v", program.ID)
			}
			if program.EndDate.Before(start) || program.StartDate.After(end) {
				t.Fatalf("program %v-%v outside %v-%v", program.StartDate, program.EndDate, start, end)
			}
			if catalog.StoredEtag(program) == "" {
				t.Fatal("program without etag")
			}
		}
		if channel.Name == nil || channel.Extra.LiveTV == nil {
			t.Fatalf("channel %+v", channel)
		}
		// The M3U group-title is the channel's tag (D-90).
		wantTag := "News"
		switch *channel.Name {
		case "FAKE-ATSC", "FAKE-SD", "FAKE-HEVC":
			wantTag = "Broadcast"
		case "FAKE-H264":
			wantTag = "Cable"
		case "FAKE-SPORTS":
			wantTag = "Sports"
		}
		if !slices.Contains(channel.Values.Tags, wantTag) {
			t.Fatalf("channel %s tags %v, want %s", *channel.Name, channel.Values.Tags, wantTag)
		}
	}
	if withPrograms != 14 {
		t.Fatalf("%d channels with programs", withPrograms)
	}

	// A second refresh with the same sources writes nothing.
	again, err := f.service.RefreshGuide(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changes != (livetv.Changes{}) || again.Programs != result.Programs {
		t.Fatalf("unchanged refresh %+v", again)
	}

	// Later, programmes that ended an hour ago leave the guide.
	f.clock.Advance(3 * time.Hour)
	later, err := f.service.RefreshGuide(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if later.Deleted == 0 || later.Inserted != 0 || later.Programs >= result.Programs {
		t.Fatalf("later refresh %+v (first %+v)", later, result)
	}
}

func TestRefreshGuideWithoutTunersWritesNothing(t *testing.T) {
	f := newFixture(t, config.DefaultLiveTvOptions(), nil)
	if _, err := f.service.RefreshGuide(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if f.items.writes != 0 {
		t.Fatalf("%d writes with Live TV off", f.items.writes)
	}
}

func TestRefreshGuideRemovesChannelsOfDeletedTuner(t *testing.T) {
	f := newFixture(t, fakehdhrOptions(t), nil)
	if _, err := f.service.RefreshGuide(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DeleteTunerHost(t.Context(), "TUNER1"); err != nil {
		t.Fatal(err)
	}
	if f.queued != 1 || len(f.config.LiveTv().TunerHosts) != 0 {
		t.Fatalf("queued %d, tuners %v", f.queued, f.config.LiveTv().TunerHosts)
	}
	if _, err := f.service.RefreshGuide(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	kinds := f.items.kinds()
	if kinds[item.KindLiveTvChannel] != 0 || kinds[item.KindLiveTvProgram] != 0 || kinds[item.KindUserView] != 1 {
		t.Fatalf("stored kinds %v", kinds)
	}
}

// fakeHost is a tuner type with scripted channels.
type fakeHost struct {
	mu       sync.Mutex
	channels map[string][]tuner.Channel
	err      error
	forgot   []string
}

func (*fakeHost) Type() string { return "fake" }
func (*fakeHost) Name() string { return "Fake Tuner" }

func (h *fakeHost) Channels(_ context.Context, info config.TunerHostInfo) ([]tuner.Channel, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return nil, h.err
	}
	return h.channels[info.Id], nil
}

func (h *fakeHost) Validate(_ context.Context, info config.TunerHostInfo) error {
	if info.Url == "bad" {
		return errors.New("cannot open")
	}
	return nil
}

func (h *fakeHost) Forget(info config.TunerHostInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.forgot = append(h.forgot, info.Id)
}

// fakeListings is a listing provider with scripted programmes keyed by
// guide channel id.
type fakeListings struct {
	kind     string
	channels []guide.Channel
	programs map[string][]guide.Program
	err      error
	calls    int
}

func (l *fakeListings) Type() string                               { return l.kind }
func (l *fakeListings) Name() string                               { return "Fake " + l.kind }
func (l *fakeListings) Validate(config.ListingsProviderInfo) error { return nil }

func (l *fakeListings) Channels(context.Context, config.ListingsProviderInfo, string) ([]guide.Channel, error) {
	return l.channels, nil
}

func (l *fakeListings) Programs(_ context.Context, _ config.ListingsProviderInfo, _ string, ids []string, _, _ time.Time, yield func(guide.Program) error) error {
	l.calls++
	if l.err != nil {
		return l.err
	}
	for _, id := range ids {
		for _, program := range l.programs[id] {
			if err := yield(program); err != nil {
				return err
			}
		}
	}
	return nil
}

func program(channel, id string, hour int, genres ...string) guide.Program {
	name := "Show " + id
	p := guide.Program{
		ID:        id,
		ChannelID: channel,
		Name:      &name,
		StartDate: testNow.Add(time.Duration(hour) * time.Hour),
		EndDate:   testNow.Add(time.Duration(hour+1) * time.Hour),
		Genres:    genres,
		IsMovie:   slices.Contains(genres, "movie"),
		IsKids:    slices.Contains(genres, "kids"),
	}
	p.Etag, _ = guide.Etag(p)
	return p
}

func fakeOptions(providers ...string) config.LiveTvOptions {
	options := config.DefaultLiveTvOptions()
	options.TunerHosts = []config.TunerHostInfo{{Id: "t1", Type: "fake"}}
	for index, kind := range providers {
		provider := config.DefaultListingsProviderInfo()
		provider.Id, provider.Type = "p"+string(rune('1'+index)), kind
		options.ListingProviders = append(options.ListingProviders, provider)
	}
	return options
}

func fakeChannels() map[string][]tuner.Channel {
	return map[string][]tuner.Channel{"t1": {
		{ID: "m3u_a", Name: "A", Number: "1", TunerHostID: "t1", Type: tuner.ChannelTypeTV},
		{ID: "m3u_b", Name: "B", Number: "2", TunerHostID: "t1", Type: tuner.ChannelTypeTV},
	}}
}

func TestRefreshGuideFirstProviderWithProgramsWins(t *testing.T) {
	host := &fakeHost{channels: fakeChannels()}
	first := &fakeListings{
		kind:     "one",
		channels: []guide.Channel{{ID: "ga", Name: "A", Number: "1", ImageURL: "https://guide/a.png"}},
		programs: map[string][]guide.Program{"ga": {program("ga", "x1", 0, "movie")}},
	}
	second := &fakeListings{
		kind:     "two",
		channels: []guide.Channel{{ID: "ga", Name: "A", Number: "1"}, {ID: "gb", Name: "B", Number: "2"}},
		programs: map[string][]guide.Program{
			"ga": {program("ga", "y1", 0)},
			"gb": {program("gb", "y2", 0, "kids")},
		},
	}
	f := newFixture(t, fakeOptions("one", "two"), []tuner.Host{host}, first, second)
	if _, err := f.service.RefreshGuide(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	a, _, _ := f.items.Item(t.Context(), catalog.ChannelID("m3u_a"))
	b, _, _ := f.items.Item(t.Context(), catalog.ChannelID("m3u_b"))
	programsA, _ := f.items.Children(t.Context(), a.ID)
	programsB, _ := f.items.Children(t.Context(), b.ID)
	if len(programsA) != 1 || *programsA[0].ExternalID != "x1_m3u_a" {
		t.Fatalf("channel A programmes %+v", programsA)
	}
	if len(programsB) != 1 || *programsB[0].ExternalID != "y2_m3u_b" {
		t.Fatalf("channel B programmes %+v", programsB)
	}
	if !a.IsMovie || a.IsSeries || !slices.Contains(b.Values.Tags, "Kids") || b.IsMovie {
		t.Fatalf("flags A %v/%v B %v/%v", a.IsMovie, a.Values.Tags, b.IsMovie, b.Values.Tags)
	}
	if len(a.ImageInfos) != 1 || a.ImageInfos[0].Path != "https://guide/a.png" {
		t.Fatalf("channel A images %+v", a.ImageInfos)
	}
}

func TestRefreshGuideKeepsStoredDataWhenReadsFail(t *testing.T) {
	host := &fakeHost{channels: fakeChannels()}
	listings := &fakeListings{
		kind:     "one",
		channels: []guide.Channel{{ID: "ga", Name: "A", Number: "1"}, {ID: "gb", Name: "B", Number: "2"}},
		programs: map[string][]guide.Program{
			"ga": {program("ga", "a1", 0), program("ga", "a2", 1)},
			"gb": {program("gb", "b1", 0)},
		},
	}
	f := newFixture(t, fakeOptions("one"), []tuner.Host{host}, listings)
	first, err := f.service.RefreshGuide(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Programs != 3 {
		t.Fatalf("first %+v", first)
	}

	// A guide failure keeps the programmes (D-76).
	listings.err = errors.New("guide offline")
	kept, err := f.service.RefreshGuide(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !kept.Kept || kept.Deleted != 0 || f.items.kinds()[item.KindLiveTvProgram] != 3 {
		t.Fatalf("guide failure %+v, kinds %v", kept, f.items.kinds())
	}

	// A tuner failure keeps the channels and their programmes.
	listings.err = nil
	host.err = errors.New("tuner offline")
	kept, err = f.service.RefreshGuide(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	kinds := f.items.kinds()
	if !kept.Kept || kept.Deleted != 0 || kinds[item.KindLiveTvChannel] != 2 || kinds[item.KindLiveTvProgram] != 3 {
		t.Fatalf("tuner failure %+v, kinds %v", kept, kinds)
	}

	// A channel the guide no longer covers loses its programmes.
	host.err = nil
	delete(listings.programs, "gb")
	cleaned, err := f.service.RefreshGuide(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.Kept || cleaned.Deleted != 1 || f.items.kinds()[item.KindLiveTvProgram] != 2 {
		t.Fatalf("cleaned %+v", cleaned)
	}
}

func TestRefreshGuideSkipsProvidersNotEnabledForTuner(t *testing.T) {
	host := &fakeHost{channels: fakeChannels()}
	listings := &fakeListings{
		kind:     "one",
		channels: []guide.Channel{{ID: "ga", Name: "A", Number: "1"}},
		programs: map[string][]guide.Program{"ga": {program("ga", "a1", 0)}},
	}
	options := fakeOptions("one")
	options.ListingProviders[0].EnableAllTuners = false
	options.ListingProviders[0].EnabledTuners = []string{"other"}
	f := newFixture(t, options, []tuner.Host{host}, listings)
	result, err := f.service.RefreshGuide(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Programs != 0 || listings.calls != 0 || result.Channels != 2 {
		t.Fatalf("result %+v, calls %d", result, listings.calls)
	}
}

func TestSaveTunerHost(t *testing.T) {
	host := &fakeHost{}
	f := newFixture(t, config.DefaultLiveTvOptions(), []tuner.Host{host})
	if _, err := f.service.SaveTunerHost(t.Context(), config.TunerHostInfo{Type: "hdhomerun"}); !errors.Is(err, livetv.ErrUnknownType) {
		t.Fatalf("unknown type: %v", err)
	}
	if _, err := f.service.SaveTunerHost(t.Context(), config.TunerHostInfo{Type: "FAKE", Url: "bad"}); err == nil {
		t.Fatal("invalid tuner saved")
	}
	saved, err := f.service.SaveTunerHost(t.Context(), config.TunerHostInfo{Id: "unknown", Type: "FAKE", Url: "http://a"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Id != "00000000000000000000000000000002" || f.queued != 1 {
		t.Fatalf("saved %+v, queued %d", saved, f.queued)
	}
	saved.Url = "http://b"
	saved.Id = strings.ToUpper(saved.Id)
	if _, err := f.service.SaveTunerHost(t.Context(), saved); err != nil {
		t.Fatal(err)
	}
	hosts := f.config.LiveTv().TunerHosts
	if len(hosts) != 1 || hosts[0].Url != "http://b" || f.queued != 2 {
		t.Fatalf("hosts %+v, queued %d", hosts, f.queued)
	}
	if !slices.Equal(host.forgot, []string{"00000000000000000000000000000002"}) {
		t.Fatalf("forgot %v", host.forgot)
	}
	if types := f.service.TunerHostTypes(); len(types) != 1 || types[0] != (tuner.TypeInfo{Name: "Fake Tuner", ID: "fake"}) {
		t.Fatalf("types %+v", types)
	}
}

// TestDeleteListingProvider ports
// ListingsManagerTests.DeleteListingsProvider_DeletesProvider.
func TestDeleteListingProvider(t *testing.T) {
	options := config.DefaultLiveTvOptions()
	options.ListingProviders = []config.ListingsProviderInfo{{Id: "MockId"}}
	f := newFixture(t, options, []tuner.Host{&fakeHost{}})
	if err := f.service.DeleteListingProvider(t.Context(), "MockId"); err != nil {
		t.Fatal(err)
	}
	if providers := f.config.LiveTv().ListingProviders; len(providers) != 0 || f.queued != 1 {
		t.Fatalf("providers %+v, queued %d", providers, f.queued)
	}
}

func TestSaveListingProviderDropsCachedGuide(t *testing.T) {
	f := newFixture(t, config.DefaultLiveTvOptions(), nil)
	path := fixturePath(t, "fakehdhr-quirks.xml")
	saved, err := f.service.SaveListingProvider(t.Context(), config.ListingsProviderInfo{Type: "xmltv", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(f.cache, "xmltv", saved.Id+".xml")
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved.Id = strings.ToUpper(saved.Id)
	if _, err := f.service.SaveListingProvider(t.Context(), saved); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cached); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cached guide kept: %v", err)
	}
	if providers := f.config.LiveTv().ListingProviders; len(providers) != 1 || f.queued != 2 {
		t.Fatalf("providers %+v, queued %d", providers, f.queued)
	}
	if _, err := f.service.SaveListingProvider(t.Context(), config.ListingsProviderInfo{Type: "xmltv", Path: filepath.Join(f.cache, "missing.xml")}); err == nil {
		t.Fatal("missing file saved")
	}
	if _, err := f.service.SaveListingProvider(t.Context(), config.ListingsProviderInfo{Type: "schedulesdirect"}); !errors.Is(err, livetv.ErrUnknownType) {
		t.Fatalf("unknown type: %v", err)
	}
}

// TestSaveListingProviderKeepsDescriptionEpisodes covers D-123: a stock
// client save that omits ParseDescriptionEpisodes keeps the stored value.
func TestSaveListingProviderKeepsDescriptionEpisodes(t *testing.T) {
	f := newFixture(t, config.DefaultLiveTvOptions(), nil)
	path := fixturePath(t, "fakehdhr-quirks.xml")
	off := false
	saved, err := f.service.SaveListingProvider(t.Context(), config.ListingsProviderInfo{Type: "xmltv", Path: path, ParseDescriptionEpisodes: &off})
	if err != nil {
		t.Fatal(err)
	}
	saved.ParseDescriptionEpisodes = nil
	resaved, err := f.service.SaveListingProvider(t.Context(), saved)
	if err != nil {
		t.Fatal(err)
	}
	if resaved.DescriptionEpisodes() || f.config.LiveTv().ListingProviders[0].DescriptionEpisodes() {
		t.Fatalf("stock save turned description episodes back on: %+v", f.config.LiveTv().ListingProviders[0])
	}
}

func TestChannelMapping(t *testing.T) {
	host := &fakeHost{channels: fakeChannels()}
	listings := &fakeListings{kind: "one", channels: []guide.Channel{{ID: "ga", Name: "Guide A", Number: "1"}, {ID: "gz", Name: "Guide Z", Number: "26"}}}
	f := newFixture(t, fakeOptions("one"), []tuner.Host{host}, listings)

	options, err := f.service.ChannelMappingOptions(t.Context(), "P1")
	if err != nil {
		t.Fatal(err)
	}
	want := []livetv.TunerChannelMapping{
		{Name: "1 A", ProviderChannelName: "Guide A", ProviderChannelID: "ga", ID: "m3u_a"},
		{Name: "2 B", ID: "m3u_b"},
	}
	if !reflect.DeepEqual(options.TunerChannels, want) || options.ProviderName != "Fake one" || len(options.ProviderChannels) != 2 || options.Mappings == nil {
		t.Fatalf("options %+v", options)
	}

	mapped, err := f.service.SetChannelMapping(t.Context(), "p1", "m3u_b", "gz")
	if err != nil {
		t.Fatal(err)
	}
	if mapped != (livetv.TunerChannelMapping{Name: "2 B", ProviderChannelName: "Guide Z", ProviderChannelID: "gz", ID: "m3u_b"}) || f.queued != 1 {
		t.Fatalf("mapped %+v, queued %d", mapped, f.queued)
	}
	// Setting the same pair again removes it.
	mapped, err = f.service.SetChannelMapping(t.Context(), "p1", "M3U_B", "GZ")
	if err != nil {
		t.Fatal(err)
	}
	if mapped.ProviderChannelID != "" || len(f.config.LiveTv().ListingProviders[0].ChannelMappings) != 0 {
		t.Fatalf("toggle %+v, mappings %v", mapped, f.config.LiveTv().ListingProviders[0].ChannelMappings)
	}
	if _, err := f.service.ChannelMappingOptions(t.Context(), "nope"); !errors.Is(err, livetv.ErrNotFound) {
		t.Fatalf("unknown provider: %v", err)
	}
	lineups, err := f.service.Lineups(t.Context(), "", "p1")
	if err != nil || len(lineups) != 2 || lineups[0] != (livetv.NameID{Name: "Guide A", ID: "ga"}) {
		t.Fatalf("lineups %+v %v", lineups, err)
	}
}

func TestGuideInfo(t *testing.T) {
	f := newFixture(t, config.DefaultLiveTvOptions(), []tuner.Host{&fakeHost{}})
	start, end := f.service.GuideInfo()
	if !start.Equal(testNow) || !end.Equal(testNow.AddDate(0, 0, 7)) {
		t.Fatalf("guide info %v %v", start, end)
	}
}

// A stock client save keeps the Deadfin-only tuner fields (D-78).
func TestSaveTunerHostKeepsDeadfinFields(t *testing.T) {
	skip, minutes, interval := true, 60, 250
	options := config.DefaultLiveTvOptions()
	options.TunerHosts = []config.TunerHostInfo{{Id: "t1", Type: "fake", Url: "http://a", SkipVodEntries: &skip, PlaylistRefreshMinutes: &minutes, StreamOpenIntervalMs: &interval, IncludeGroups: []string{"News"}, ExcludeGroups: []string{"Kids"}}}
	host := &fakeHost{}
	f := newFixture(t, options, []tuner.Host{host})
	saved, err := f.service.SaveTunerHost(t.Context(), config.TunerHostInfo{Id: "T1", Type: "fake", Url: "http://b"})
	if err != nil {
		t.Fatal(err)
	}
	stored := f.config.LiveTv().TunerHosts[0]
	for _, value := range []config.TunerHostInfo{saved, stored} {
		if value.SkipVodEntries == nil || !*value.SkipVodEntries || value.PlaylistRefreshMinutes == nil || *value.PlaylistRefreshMinutes != 60 || value.Url != "http://b" ||
			value.StreamOpenIntervalMs == nil || *value.StreamOpenIntervalMs != 250 ||
			!slices.Equal(value.IncludeGroups, []string{"News"}) || !slices.Equal(value.ExcludeGroups, []string{"Kids"}) {
			t.Fatalf("saved %+v", value)
		}
	}
	if !slices.Equal(host.forgot, []string{"t1"}) {
		t.Fatalf("forgot %v", host.forgot)
	}
	if f.service.Hidden() {
		t.Fatal("hidden with a tuner")
	}
}

// TestRefreshGuideAppliesTunerGroupFilter checks D-91: a group filter edit
// reuses the cached playlist, the groups count the whole playlist, and the
// next refresh drops the filtered channels.
func TestRefreshGuideAppliesTunerGroupFilter(t *testing.T) {
	options := fakehdhrOptions(t)
	playlist := filepath.Join(t.TempDir(), "tuner.m3u")
	data, err := os.ReadFile(options.TunerHosts[0].Url)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(playlist, data, 0o600); err != nil {
		t.Fatal(err)
	}
	options.TunerHosts[0].Url = playlist
	f := newFixture(t, options, nil)
	if groups, err := f.service.TunerHostGroups("tuner1"); err != nil || groups.Loaded || len(groups.Groups) != 0 {
		t.Fatalf("groups before a download %+v, %v", groups, err)
	}
	if result, err := f.service.RefreshGuide(t.Context(), nil); err != nil || result.Channels != 14 {
		t.Fatalf("first refresh %+v, %v", result, err)
	}

	// The filter edit must not read the playlist again.
	if err := os.Remove(playlist); err != nil {
		t.Fatal(err)
	}
	info := f.config.LiveTv().TunerHosts[0]
	info.ExcludeGroups = []string{" news "}
	if _, err := f.service.SaveTunerHost(t.Context(), info); err != nil {
		t.Fatalf("filter save read the playlist: %v", err)
	}
	groups, err := f.service.TunerHostGroups("TUNER1")
	if err != nil {
		t.Fatal(err)
	}
	want := []tuner.GroupCount{{Name: "Broadcast", Count: 3}, {Name: "Cable", Count: 1}, {Name: "News", Count: 9}, {Name: "Sports", Count: 1}}
	if !groups.Loaded || !slices.Equal(groups.Groups, want) {
		t.Fatalf("groups %+v, want %+v", groups, want)
	}
	result, err := f.service.RefreshGuide(t.Context(), nil)
	if err != nil || result.Channels != 5 {
		t.Fatalf("filtered refresh %+v, %v", result, err)
	}
	folder := catalog.Folder("/data/metadata", "/data", testNow)
	channels, _ := f.items.Children(t.Context(), folder.ID)
	for _, channel := range channels {
		if slices.Contains(channel.Values.Tags, "News") {
			t.Fatalf("filtered channel %s kept", *channel.Name)
		}
	}
	if len(channels) != 5 {
		t.Fatalf("%d channels stored, want 5", len(channels))
	}

	info.ExcludeGroups = []string{}
	info.IncludeGroups = []string{"cable", "SPORTS"}
	saved, err := f.service.SaveTunerHost(t.Context(), info)
	if err != nil || len(saved.ExcludeGroups) != 0 {
		t.Fatalf("include save %+v, %v", saved, err)
	}
	if result, err := f.service.RefreshGuide(t.Context(), nil); err != nil || result.Channels != 2 {
		t.Fatalf("include refresh %+v, %v", result, err)
	}

	// Any other edit is a retry: it drops the cache and reads the source.
	info.FriendlyName = "Renamed"
	if _, err := f.service.SaveTunerHost(t.Context(), info); err == nil {
		t.Fatal("a non-filter save reused the cache")
	}
	if _, err := f.service.TunerHostGroups("missing"); !errors.Is(err, livetv.ErrNotFound) {
		t.Fatalf("unknown tuner: %v", err)
	}
}
