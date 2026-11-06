// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/dtobuild"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/livetv"
	"github.com/deadfin-org/deadfin/internal/livetv/fetch"
	"github.com/deadfin-org/deadfin/internal/livetv/guide"
	"github.com/deadfin-org/deadfin/internal/livetv/itemstore"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
	"github.com/deadfin-org/deadfin/internal/store/storetest"
	"github.com/deadfin-org/deadfin/internal/user"
)

func TestLiveTvReadRoutesSQLite(t *testing.T) {
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	runLiveTvReadRoutes(t, stores)
}

func TestLiveTvReadRoutesPostgres(t *testing.T) {
	runLiveTvReadRoutes(t, storetest.OpenPostgres(t))
}

type liveTvReadConfig struct {
	mu    sync.Mutex
	value config.LiveTvOptions
}

func (c *liveTvReadConfig) LiveTv() config.LiveTvOptions {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func (c *liveTvReadConfig) UpdateLiveTv(_ context.Context, update func(*config.LiveTvOptions) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return update(&c.value)
}

// liveTvReadNow is inside the fakehdhr fixture guide.
var liveTvReadNow = time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)

func runLiveTvReadRoutes(t *testing.T, stores store.Stores) {
	t.Helper()
	ctx := t.Context()
	items := store.NewItemStore(stores)
	clk := clock.NewFake(liveTvReadNow)
	service := liveTvReadGuide(t, items, clk)

	admin := store.User{ID: itemsHandlerUUID(t, 900), Policy: []byte(`{"IsAdministrator":true,"EnableAllFolders":true,"EnableLiveTvAccess":true}`)}
	restricted := store.User{ID: itemsHandlerUUID(t, 901), Policy: []byte(`{"EnableAllFolders":false,"EnabledFolders":[],"EnableLiveTvAccess":true}`)}
	noLiveTv := store.User{ID: itemsHandlerUUID(t, 902), Policy: []byte(`{"EnableAllFolders":false,"EnabledFolders":[],"EnableLiveTvAccess":false}`)}
	users := itemsAccessUsers{values: map[dto.UUID]store.User{admin.ID: admin, restricted.ID: restricted, noLiveTv.ID: noLiveTv}}
	authorizer := middleware.NewAuthorizer(itemsAccessResolver{identities: map[string]auth.Identity{
		admin.ID.String():      {Authenticated: true, UserID: admin.ID, Administrator: true},
		restricted.ID.String(): {Authenticated: true, UserID: restricted.ID},
		noLiveTv.ID.String():   {Authenticated: true, UserID: noLiveTv.ID},
	}}, itemsAccessPolicy{}, users)
	builder := dtobuild.NewBuilder(items, dtobuild.NewStoreEnricher(items, users, nil)).WithClock(clk)
	rt := router.New(authorizer.Wrap)
	handlers.RegisterLiveTv(rt, &fakeLiveTvAdmin{}, handlers.LiveTvReads{Guide: service, Items: items, Build: builder, Users: users, Clock: clk, AllUsers: liveTvReadUsers{users}})

	call := func(t *testing.T, actor store.User, method, path, body string, out any) int {
		t.Helper()
		record := httptest.NewRecorder()
		var request *http.Request
		if body == "" {
			request = httptest.NewRequest(method, path, nil)
		} else {
			request = httptest.NewRequest(method, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
		}
		request.Header.Set("X-Item-Access-Actor", actor.ID.String())
		rt.ServeHTTP(record, request)
		if record.Code == http.StatusOK && out != nil {
			if err := json.Unmarshal(record.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: decode %v; body=%s", method, path, err, record.Body.String())
			}
		}
		return record.Code
	}

	var channels dto.QueryResult[dto.BaseItemDto]
	t.Run("channels", func(t *testing.T) {
		if code := call(t, admin, http.MethodGet, "/LiveTv/Channels", "", &channels); code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		if channels.TotalRecordCount != 14 || len(channels.Items) != 14 {
			t.Fatalf("channels total %d items %d, want 14", channels.TotalRecordCount, len(channels.Items))
		}
		current := 0
		for _, channel := range channels.Items {
			if channel.Type != "TvChannel" || channel.ChannelType == nil || *channel.ChannelType != "TV" || channel.ChannelNumber != channel.Number {
				t.Fatalf("channel %s type %q channel type %v number %q/%q", channel.Name, channel.Type, channel.ChannelType, channel.Number, channel.ChannelNumber)
			}
			if channel.CanDelete != nil || channel.Etag != "" {
				t.Fatalf("channel %s keeps removed fields", channel.Name)
			}
			if program := channel.CurrentProgram; program != nil {
				current++
				if program.Type != "Program" || program.ChannelID == nil || *program.ChannelID != channel.ID || program.StartDate == nil || program.StartDate.After(liveTvReadNow) {
					t.Fatalf("channel %s current program %+v", channel.Name, program)
				}
			}
		}
		if current == 0 {
			t.Fatal("no channel has a current programme")
		}
		// LiveTvChannel.CreateSortName orders numbered channels by number.
		previous := -1.0
		for _, channel := range channels.Items {
			if number, err := strconv.ParseFloat(channel.ChannelNumber, 64); err == nil {
				if number < previous {
					t.Fatalf("channel %s number %s after %v", channel.Name, channel.ChannelNumber, previous)
				}
				previous = number
			}
		}
		var paged dto.QueryResult[dto.BaseItemDto]
		if code := call(t, admin, http.MethodGet, "/LiveTv/Channels?startIndex=2&limit=3&addCurrentProgram=false", "", &paged); code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		if paged.StartIndex != 2 || paged.TotalRecordCount != 14 || len(paged.Items) != 3 || paged.Items[0].ID != channels.Items[2].ID || paged.Items[0].CurrentProgram != nil {
			t.Fatalf("paged channels start %d total %d items %d", paged.StartIndex, paged.TotalRecordCount, len(paged.Items))
		}
	})

	t.Run("info", func(t *testing.T) {
		var info struct {
			Services []struct {
				Name   string
				Status string
				Tuners []string
			}
			IsEnabled    bool
			EnabledUsers []string
		}
		if code := call(t, restricted, http.MethodGet, "/LiveTv/Info", "", &info); code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		want := []string{admin.ID.String(), restricted.ID.String()}
		if !info.IsEnabled || len(info.Services) != 1 || info.Services[0].Name != "Emby" || info.Services[0].Status != "Ok" || info.Services[0].Tuners == nil ||
			len(info.EnabledUsers) != 2 || info.EnabledUsers[0] != want[0] || info.EnabledUsers[1] != want[1] {
			t.Fatalf("info %+v, want enabled users %v", info, want)
		}
	})

	t.Run("restricted folders keep live tv", func(t *testing.T) {
		var got dto.QueryResult[dto.BaseItemDto]
		if code := call(t, restricted, http.MethodGet, "/LiveTv/Channels", "", &got); code != http.StatusOK || got.TotalRecordCount != 14 {
			t.Fatalf("restricted status %d total %d, want 14", code, got.TotalRecordCount)
		}
		if code := call(t, noLiveTv, http.MethodGet, "/LiveTv/Channels", "", &got); code != http.StatusOK || got.TotalRecordCount != 0 {
			t.Fatalf("no live tv status %d total %d, want 0", code, got.TotalRecordCount)
		}
	})

	t.Run("guide grid", func(t *testing.T) {
		ids := make([]string, 0, 6)
		for _, channel := range channels.Items[:6] {
			ids = append(ids, `"`+channel.ID.String()+`"`)
		}
		body := `{"UserId":"` + admin.ID.String() + `","ChannelIds":[` + strings.Join(ids, ",") + `],` +
			`"MinStartDate":"2026-03-08T00:00:00Z","MaxStartDate":"2026-03-09T00:00:00Z","Fields":["ChannelInfo","ChannelImage"],"EnableTotalRecordCount":true}`
		var grid dto.QueryResult[dto.BaseItemDto]
		if code := call(t, admin, http.MethodPost, "/LiveTv/Programs", body, &grid); code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		want, err := items.Query(ctx, store.ItemQuery{
			IncludeItemTypes:       []item.Kind{item.KindLiveTvProgram},
			ChannelIDs:             liveTvChannelIDs(channels.Items[:6]),
			MinStartDate:           liveTvTime(time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)),
			MaxStartDate:           liveTvTime(time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)),
			EnableTotalRecordCount: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if grid.TotalRecordCount == 0 || grid.TotalRecordCount != want.TotalRecordCount || len(grid.Items) != want.TotalRecordCount {
			t.Fatalf("grid total %d items %d, want %d", grid.TotalRecordCount, len(grid.Items), want.TotalRecordCount)
		}
		names := make(map[dto.UUID]string, 6)
		for _, channel := range channels.Items[:6] {
			names[channel.ID] = channel.Name
		}
		for index, program := range grid.Items {
			if program.Type != "Program" || program.StartDate == nil || program.ChannelID == nil || program.ChannelName != names[*program.ChannelID] || program.MediaType != "Video" {
				t.Fatalf("programme %d %+v", index, program)
			}
			if index > 0 && program.StartDate.Before(*grid.Items[index-1].StartDate) {
				t.Fatalf("programme %d starts before programme %d", index, index-1)
			}
		}

		// ChannelIds as one comma-delimited string, as JsonCommaDelimitedCollectionConverter reads it.
		comma := `{"ChannelIds":"` + channels.Items[0].ID.String() + `,` + channels.Items[1].ID.String() + `"}`
		var both dto.QueryResult[dto.BaseItemDto]
		if code := call(t, admin, http.MethodPost, "/LiveTv/Programs", comma, &both); code != http.StatusOK || both.TotalRecordCount == 0 {
			t.Fatalf("comma channel ids status %d total %d", code, both.TotalRecordCount)
		}
		for _, program := range both.Items {
			if *program.ChannelID != channels.Items[0].ID && *program.ChannelID != channels.Items[1].ID {
				t.Fatalf("programme on channel %s", program.ChannelID)
			}
		}
	})

	t.Run("airing and recommended", func(t *testing.T) {
		var airing dto.QueryResult[dto.BaseItemDto]
		if code := call(t, admin, http.MethodGet, "/LiveTv/Programs?isAiring=true", "", &airing); code != http.StatusOK || len(airing.Items) == 0 {
			t.Fatalf("airing status %d items %d", code, len(airing.Items))
		}
		for _, program := range airing.Items {
			if program.StartDate.After(liveTvReadNow) || program.EndDate == nil || program.EndDate.Before(liveTvReadNow) {
				t.Fatalf("airing programme %s %v-%v", program.Name, program.StartDate, program.EndDate)
			}
		}
		var recommended dto.QueryResult[dto.BaseItemDto]
		if code := call(t, admin, http.MethodGet, "/LiveTv/Programs/Recommended?isAiring=true&limit=3", "", &recommended); code != http.StatusOK {
			t.Fatalf("recommended status %d", code)
		}
		if len(recommended.Items) != 3 || recommended.TotalRecordCount != len(airing.Items) {
			t.Fatalf("recommended items %d total %d, want 3 of %d", len(recommended.Items), recommended.TotalRecordCount, len(airing.Items))
		}
		var timers dto.QueryResult[dto.BaseItemDto]
		if code := call(t, admin, http.MethodGet, "/LiveTv/Programs?seriesTimerId=abc", "", &timers); code != http.StatusOK || timers.TotalRecordCount != 0 || timers.Items == nil {
			t.Fatalf("series timer status %d result %+v", code, timers)
		}
	})

	t.Run("single items", func(t *testing.T) {
		var channel dto.BaseItemDto
		if code := call(t, admin, http.MethodGet, "/LiveTv/Channels/"+channels.Items[0].ID.String(), "", &channel); code != http.StatusOK || channel.Type != "TvChannel" || channel.ID != channels.Items[0].ID {
			t.Fatalf("channel status %d %+v", code, channel)
		}
		program := channels.Items[0].CurrentProgram
		for _, value := range channels.Items {
			if program == nil {
				program = value.CurrentProgram
			}
		}
		var got dto.BaseItemDto
		if code := call(t, admin, http.MethodGet, "/LiveTv/Programs/"+program.ID.String(), "", &got); code != http.StatusOK || got.Type != "Program" || got.ID != program.ID {
			t.Fatalf("program status %d %+v", code, got)
		}
		missing := itemsHandlerUUID(t, 999).String()
		for _, path := range []string{"/LiveTv/Channels/" + missing, "/LiveTv/Programs/" + missing, "/LiveTv/Programs/not-a-guid"} {
			if code := call(t, admin, http.MethodGet, path, "", nil); code != http.StatusNotFound {
				t.Fatalf("GET %s status %d, want 404", path, code)
			}
		}
	})
}

func liveTvReadGuide(t *testing.T, items *store.ItemStore, clk clock.Clock) *livetv.Service {
	t.Helper()
	testdata, err := filepath.Abs(filepath.Join("..", "..", "livetv", "guide", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	days := 1
	return liveTvGuideFrom(t, items, clk, filepath.Join(testdata, "fakehdhr-quirks.m3u"), filepath.Join(testdata, "fakehdhr-quirks.xml"), &days)
}

// liveTvGuideFrom refreshes a guide from a playlist and an XMLTV file.
func liveTvGuideFrom(t *testing.T, items *store.ItemStore, clk clock.Clock, playlist, xmltv string, days *int) *livetv.Service {
	t.Helper()
	client, err := fetch.New(fetch.Options{HTTP: &http.Client{}, Clock: clk, UserAgent: "Deadfin-test"})
	if err != nil {
		t.Fatal(err)
	}
	options := config.DefaultLiveTvOptions()
	options.GuideDays = days
	host := config.DefaultTunerHostInfo()
	host.Id, host.Type, host.Url = "tuner1", "m3u", playlist
	provider := config.DefaultListingsProviderInfo()
	provider.Id, provider.Type, provider.Path = "guide1", "xmltv", xmltv
	options.TunerHosts = []config.TunerHostInfo{host}
	options.ListingProviders = []config.ListingsProviderInfo{provider}
	service, err := livetv.New(livetv.Options{
		Config:   &liveTvReadConfig{value: options},
		Tuners:   tuner.NewRegistry(tuner.NewM3UHost(client, clk, nil)),
		Listings: []livetv.ListingsProvider{guide.NewXMLTV(client, clk, t.TempDir())},
		Items:    itemstore.New(items),
		Clock:    clk,
		Logger:   slog.New(slog.DiscardHandler),
		Paths:    livetv.Paths{ProgramData: "/data", InternalMetadata: "/data/metadata"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RefreshGuide(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	return service
}

func liveTvChannelIDs(values []dto.BaseItemDto) []dto.UUID {
	out := make([]dto.UUID, len(values))
	for index, value := range values {
		out[index] = value.ID
	}
	return out
}

func liveTvTime(value time.Time) *time.Time { return &value }

// liveTvReadUsers lists the fixture users in id order.
type liveTvReadUsers struct{ itemsAccessUsers }

func (u liveTvReadUsers) List(context.Context, user.Actor, user.UserListOptions) ([]user.UserDTO, error) {
	out := make([]user.UserDTO, 0, len(u.values))
	for _, value := range u.values {
		out = append(out, user.UserDTO{ID: value.ID, Policy: value.Policy})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}
