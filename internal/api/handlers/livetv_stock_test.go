// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/dtobuild"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
	"github.com/deadfin-org/deadfin/internal/store/storetest"
)

// liveTvStockCapture is testdata/livetv-stock-programs.json, written by
// tools/fakehdhr/capture-programs.sh from the pinned stock image.
type liveTvStockCapture struct {
	Captured     time.Time `json:"captured"`
	MinEndDate   string    `json:"minEndDate"`
	MaxStartDate string    `json:"maxStartDate"`
	Playlist     string    `json:"playlist"`
	Guide        string    `json:"guide"`
	Channels     []struct {
		Number string `json:"Number"`
		Name   string `json:"Name"`
	} `json:"channels"`
	TotalRecordCount int              `json:"totalRecordCount"`
	Programs         []liveTvStockRow `json:"programs"`
}

type liveTvStockRow struct {
	ChannelNumber string `json:"ChannelNumber"`
	Name          string `json:"Name"`
	StartDate     string `json:"StartDate"`
}

func TestLiveTvProgramsMatchStockSQLite(t *testing.T) {
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	runLiveTvProgramsMatchStock(t, stores)
}

func TestLiveTvProgramsMatchStockPostgres(t *testing.T) {
	runLiveTvProgramsMatchStock(t, storetest.OpenPostgres(t))
}

// runLiveTvProgramsMatchStock replays the captured fakehdhr playlist and
// guide into Deadfin at the capture time and asks POST /LiveTv/Programs the
// question the capture asked stock: the first 6 channels over 3 days.
func runLiveTvProgramsMatchStock(t *testing.T, stores store.Stores) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "livetv-stock-programs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var capture liveTvStockCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	playlist, guidePath := filepath.Join(dir, "playlist.m3u"), filepath.Join(dir, "guide.xml")
	if err := os.WriteFile(playlist, []byte(capture.Playlist), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(guidePath, []byte(capture.Guide), 0o600); err != nil {
		t.Fatal(err)
	}

	items := store.NewItemStore(stores)
	clk := clock.NewFake(capture.Captured)
	service := liveTvGuideFrom(t, items, clk, playlist, guidePath, nil)
	viewer := store.User{ID: itemsHandlerUUID(t, 950), Policy: []byte(`{"IsAdministrator":true,"EnableAllFolders":true,"EnableLiveTvAccess":true}`)}
	users := itemsAccessUsers{values: map[dto.UUID]store.User{viewer.ID: viewer}}
	authorizer := middleware.NewAuthorizer(itemsAccessResolver{identities: map[string]auth.Identity{
		viewer.ID.String(): {Authenticated: true, UserID: viewer.ID, Administrator: true},
	}}, itemsAccessPolicy{}, users)
	builder := dtobuild.NewBuilder(items, dtobuild.NewStoreEnricher(items, users, nil)).WithClock(clk)
	rt := router.New(authorizer.Wrap)
	handlers.RegisterLiveTv(rt, &fakeLiveTvAdmin{}, handlers.LiveTvReads{Guide: service, Items: items, Build: builder, Users: users, Clock: clk})
	call := func(method, path, body string, out any) {
		t.Helper()
		record := httptest.NewRecorder()
		var request *http.Request
		if body == "" {
			request = httptest.NewRequest(method, path, nil)
		} else {
			request = httptest.NewRequest(method, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
		}
		request.Header.Set("X-Item-Access-Actor", viewer.ID.String())
		rt.ServeHTTP(record, request)
		if record.Code != http.StatusOK {
			t.Fatalf("%s %s status %d; body=%s", method, path, record.Code, record.Body.String())
		}
		if err := json.Unmarshal(record.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}

	var channels dto.QueryResult[dto.BaseItemDto]
	call(http.MethodGet, "/LiveTv/Channels?addCurrentProgram=false&enableImages=false", "", &channels)
	if len(channels.Items) < len(capture.Channels) {
		t.Fatalf("Deadfin lists %d channels, stock listed at least %d", len(channels.Items), len(capture.Channels))
	}
	ids := make([]string, len(capture.Channels))
	for index, want := range capture.Channels {
		got := channels.Items[index]
		if got.ChannelNumber != want.Number || got.Name != want.Name {
			t.Fatalf("channel %d = %s %s, stock %s %s", index, got.ChannelNumber, got.Name, want.Number, want.Name)
		}
		ids[index] = `"` + got.ID.String() + `"`
	}

	body := fmt.Sprintf(`{"UserId":%q,"ChannelIds":[%s],"MinEndDate":%q,"MaxStartDate":%q,"SortBy":["StartDate"],"EnableImages":false,"EnableUserData":false,"EnableTotalRecordCount":true,"Fields":["ChannelInfo"]}`,
		viewer.ID.String(), strings.Join(ids, ","), capture.MinEndDate, capture.MaxStartDate)
	var programs dto.QueryResult[dto.BaseItemDto]
	call(http.MethodPost, "/LiveTv/Programs", body, &programs)
	if programs.TotalRecordCount != capture.TotalRecordCount {
		t.Errorf("TotalRecordCount = %d, stock %d", programs.TotalRecordCount, capture.TotalRecordCount)
	}
	got := make([]liveTvStockRow, len(programs.Items))
	for index, program := range programs.Items {
		if program.StartDate == nil {
			t.Fatalf("programme %d has no StartDate", index)
		}
		if index > 0 && program.StartDate.Before(*programs.Items[index-1].StartDate) {
			t.Fatalf("programme %d starts before programme %d", index, index-1)
		}
		got[index] = liveTvStockRow{ChannelNumber: program.ChannelNumber, Name: program.Name, StartDate: program.StartDate.UTC().Format(time.RFC3339)}
	}
	want := make([]liveTvStockRow, len(capture.Programs))
	for index, row := range capture.Programs {
		start, err := time.Parse(time.RFC3339Nano, row.StartDate)
		if err != nil {
			t.Fatal(err)
		}
		row.StartDate = start.UTC().Format(time.RFC3339)
		want[index] = row
	}
	// Both servers order by StartDate; rows that start together tie on
	// storage order, which differs.
	for _, rows := range [][]liveTvStockRow{got, want} {
		sort.Slice(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			if a.StartDate != b.StartDate {
				return a.StartDate < b.StartDate
			}
			if a.ChannelNumber != b.ChannelNumber {
				return a.ChannelNumber < b.ChannelNumber
			}
			return a.Name < b.Name
		})
	}
	if len(got) != len(want) {
		t.Fatalf("Deadfin returned %d programmes, stock %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("programme %d = %+v, stock %+v", index, got[index], want[index])
		}
	}
}
