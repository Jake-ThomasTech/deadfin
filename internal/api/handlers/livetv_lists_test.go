// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/dtobuild"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/playlists"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
	"github.com/deadfin-org/deadfin/internal/store/storetest"
)

// TestLiveTvChannelListsSQLite and TestLiveTvChannelListsPostgres cover
// curated channel lists (#719): playlists and collections of LiveTvChannel
// items, sharing, Live TV access, and the guide for an arbitrary channel set.
func TestLiveTvChannelListsSQLite(t *testing.T) {
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	runLiveTvChannelLists(t, stores)
}

func TestLiveTvChannelListsPostgres(t *testing.T) {
	runLiveTvChannelLists(t, storetest.OpenPostgres(t))
}

func runLiveTvChannelLists(t *testing.T, stores store.Stores) {
	t.Helper()
	ctx := t.Context()
	items := store.NewItemStore(stores)
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(data, "playlists"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := items.BootstrapSystemFolders(ctx, root); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(liveTvReadNow)
	guide := liveTvReadGuide(t, items, clk)

	owner := store.User{ID: itemsHandlerUUID(t, 950), Name: "owner", Policy: []byte(`{"EnableAllFolders":true,"EnableLiveTvAccess":true}`)}
	family := store.User{ID: itemsHandlerUUID(t, 951), Name: "family", Policy: []byte(`{"EnableAllFolders":false,"EnabledFolders":[],"EnableLiveTvAccess":true}`)}
	noLiveTv := store.User{ID: itemsHandlerUUID(t, 952), Name: "nolivetv", Policy: []byte(`{"EnableAllFolders":false,"EnabledFolders":[],"EnableLiveTvAccess":false}`)}
	allFoldersNoLiveTv := store.User{ID: itemsHandlerUUID(t, 953), Name: "allfolders-nolivetv", Policy: []byte(`{"EnableAllFolders":true,"EnableLiveTvAccess":false}`)}
	withoutLiveTv := []store.User{noLiveTv, allFoldersNoLiveTv}
	users := itemsAccessUsers{values: map[dto.UUID]store.User{owner.ID: owner, family.ID: family, noLiveTv.ID: noLiveTv, allFoldersNoLiveTv.ID: allFoldersNoLiveTv}}
	identities := map[string]auth.Identity{}
	for _, value := range users.values {
		identities[value.ID.String()] = auth.Identity{Authenticated: true, UserID: value.ID}
	}
	authorizer := middleware.NewAuthorizer(itemsAccessResolver{identities: identities}, itemsAccessPolicy{}, users)
	builder := dtobuild.NewBuilder(items, dtobuild.NewStoreEnricher(items, users, nil)).WithClock(clk)
	service, err := playlists.New(playlists.Options{Items: items, Users: users, Clock: clk, ProgramData: root, DataPath: data})
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(authorizer.Wrap)
	handlers.RegisterPlaylists(rt, service, users, builder, clk)
	handlers.RegisterItems(rt, items, builder, clk, handlers.ItemServices{Users: users})
	handlers.RegisterCollections(rt, items, &collCreator{items: items, id: itemsHandlerUUID(t, 960)})
	handlers.RegisterLiveTv(rt, &fakeLiveTvAdmin{}, handlers.LiveTvReads{Guide: guide, Items: items, Build: builder, Users: users, Clock: clk, AllUsers: liveTvReadUsers{users}})

	call := func(t *testing.T, actor store.User, method, path, body string, out any) int {
		t.Helper()
		var request *http.Request
		if body == "" {
			request = httptest.NewRequest(method, path, nil)
		} else {
			request = httptest.NewRequest(method, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
		}
		request.Header.Set("X-Item-Access-Actor", actor.ID.String())
		record := httptest.NewRecorder()
		rt.ServeHTTP(record, request)
		if record.Code == http.StatusOK && out != nil {
			if err := json.Unmarshal(record.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: decode %v; body=%s", method, path, err, record.Body.String())
			}
		}
		return record.Code
	}
	expect := func(t *testing.T, actor store.User, method, path, body string, status int, out any) {
		t.Helper()
		if code := call(t, actor, method, path, body, out); code != status {
			t.Fatalf("%s %s as %s: status %d, want %d", method, path, actor.Name, code, status)
		}
	}
	join := func(values []dto.UUID) string {
		out := make([]string, len(values))
		for index, value := range values {
			out[index] = value.String()
		}
		return strings.Join(out, ",")
	}
	// browse returns the ids and types of a list's entries.
	browse := func(t *testing.T, actor store.User, path string) ([]dto.UUID, []string) {
		t.Helper()
		var page dto.QueryResult[dto.BaseItemDto]
		expect(t, actor, http.MethodGet, path, "", http.StatusOK, &page)
		ids := make([]dto.UUID, len(page.Items))
		kinds := make([]string, len(page.Items))
		for index, value := range page.Items {
			ids[index], kinds[index] = value.ID, string(value.Type)
		}
		return ids, kinds
	}
	allTvChannel := func(t *testing.T, label string, kinds []string) {
		t.Helper()
		for _, kind := range kinds {
			if kind != "TvChannel" {
				t.Fatalf("%s types %v, want TvChannel", label, kinds)
			}
		}
	}

	var channels dto.QueryResult[dto.BaseItemDto]
	expect(t, owner, http.MethodGet, "/LiveTv/Channels?addCurrentProgram=false", "", http.StatusOK, &channels)
	if len(channels.Items) < 12 {
		t.Fatalf("fixture has %d channels, want at least 12", len(channels.Items))
	}
	all := liveTvChannelIDs(channels.Items)
	// A curated order that is not the channel-number order.
	curated := []dto.UUID{all[9], all[2], all[11], all[0], all[5], all[7], all[1], all[10], all[4], all[3]}

	t.Run("playlist", func(t *testing.T) {
		var created struct{ Id string }
		body := `{"Name":"Mom","IsPublic":false,"UserId":"` + owner.ID.String() + `","Ids":["` + strings.ReplaceAll(join(curated[:8]), ",", `","`) + `"]}`
		expect(t, owner, http.MethodPost, "/Playlists", body, http.StatusOK, &created)
		id, err := dto.ParseUUID(created.Id)
		if err != nil {
			t.Fatal(err)
		}
		base := "/Playlists/" + id.String()
		expect(t, owner, http.MethodPost, base+"/Items?ids="+join(curated[8:]), "", http.StatusNoContent, nil)

		got, kinds := browse(t, owner, base+"/Items?userId="+owner.ID.String())
		sameIDs(t, "playlist items", got, curated)
		allTvChannel(t, "playlist items", kinds)
		got, kinds = browse(t, owner, "/Items?parentId="+id.String()+"&userId="+owner.ID.String())
		sameIDs(t, "items by parent", got, curated)
		allTvChannel(t, "items by parent", kinds)

		// CreatePlaylist takes the media type from LiveTvChannel.MediaType.
		if stored, err := items.Get(ctx, id); err != nil || stored.MediaType == nil || *stored.MediaType != item.MediaTypeVideo {
			t.Fatalf("playlist media type %v, %v; want Video", stored.MediaType, err)
		}

		// Duplicates are ignored; position inserts.
		expect(t, owner, http.MethodPost, base+"/Items?ids="+join([]dto.UUID{curated[0], all[6]})+"&position=1", "", http.StatusNoContent, nil)
		want := append([]dto.UUID{curated[0], all[6]}, curated[1:]...)
		got, _ = browse(t, owner, base+"/Items")
		sameIDs(t, "after positioned add", got, want)

		// Move and remove.
		expect(t, owner, http.MethodPost, base+"/Items/"+all[6].String()+"/Move/10", "", http.StatusNoContent, nil)
		want = append(append([]dto.UUID{curated[0]}, curated[1:]...), all[6])
		got, _ = browse(t, owner, base+"/Items")
		sameIDs(t, "after move", got, want)
		expect(t, owner, http.MethodDelete, base+"/Items?entryIds="+all[6].String()+","+curated[3].String(), "", http.StatusNoContent, nil)
		want = append(append([]dto.UUID{}, curated[:3]...), curated[4:]...)
		got, _ = browse(t, owner, base+"/Items")
		sameIDs(t, "after remove", got, want)

		// A private list is invisible to others until shared.
		expect(t, family, http.MethodGet, base+"/Items", "", http.StatusNotFound, nil)
		expect(t, owner, http.MethodPost, base+"/Users/"+family.ID.String(), `{"CanEdit":false}`, http.StatusNoContent, nil)
		for _, other := range withoutLiveTv {
			expect(t, owner, http.MethodPost, base+"/Users/"+other.ID.String(), `{"CanEdit":false}`, http.StatusNoContent, nil)
		}
		got, kinds = browse(t, family, base+"/Items")
		sameIDs(t, "shared list for family", got, want)
		allTvChannel(t, "shared list for family", kinds)
		got, _ = browse(t, family, "/Items?parentId="+id.String()+"&userId="+family.ID.String())
		sameIDs(t, "shared list by parent for family", got, want)
		// The playlist is visible, but its channels follow EnableLiveTvAccess.
		for _, other := range withoutLiveTv {
			got, _ = browse(t, other, base+"/Items")
			sameIDs(t, "shared list for "+other.Name, got, nil)
			got, _ = browse(t, other, "/Items?parentId="+id.String()+"&userId="+other.ID.String())
			sameIDs(t, "shared list by parent for "+other.Name, got, nil)
		}
		// A read-only share cannot edit.
		expect(t, family, http.MethodPost, base+"/Items?ids="+all[6].String(), "", http.StatusForbidden, nil)
	})

	t.Run("collection", func(t *testing.T) {
		var created dto.CollectionCreationResult
		expect(t, owner, http.MethodPost, "/Collections?name=Cable&ids="+join(curated[:6]), "", http.StatusOK, &created)
		expect(t, owner, http.MethodPost, "/Collections/"+created.ID.String()+"/Items?ids="+join(curated[6:]), "", http.StatusNoContent, nil)
		path := "/Items?parentId=" + created.ID.String()
		got, kinds := browse(t, owner, path+"&userId="+owner.ID.String())
		sameSet(t, "collection items", got, curated)
		allTvChannel(t, "collection items", kinds)
		got, _ = browse(t, family, path+"&userId="+family.ID.String())
		sameSet(t, "collection items for family", got, curated)
		for _, other := range withoutLiveTv {
			got, _ = browse(t, other, path+"&userId="+other.ID.String())
			sameIDs(t, "collection items for "+other.Name, got, nil)
		}

		expect(t, owner, http.MethodDelete, "/Collections/"+created.ID.String()+"/Items?ids="+curated[0].String(), "", http.StatusNoContent, nil)
		got, _ = browse(t, owner, path+"&userId="+owner.ID.String())
		sameSet(t, "collection after remove", got, curated[1:])
	})

	t.Run("guide for a list", func(t *testing.T) {
		set := []dto.UUID{curated[0], curated[4], curated[9]}
		want, err := items.Query(ctx, store.ItemQuery{
			IncludeItemTypes: []item.Kind{item.KindLiveTvProgram},
			ChannelIDs:       set,
			MinStartDate:     liveTvTime(liveTvReadNow.Add(-12 * 3600e9)),
			MaxStartDate:     liveTvTime(liveTvReadNow.Add(12 * 3600e9)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(want.Items) == 0 {
			t.Fatal("fixture has no programmes for the set")
		}
		inSet := map[dto.UUID]bool{}
		for _, id := range set {
			inSet[id] = true
		}
		check := func(t *testing.T, label string, page dto.QueryResult[dto.BaseItemDto]) {
			t.Helper()
			if len(page.Items) != len(want.Items) {
				t.Fatalf("%s: %d programmes, want %d", label, len(page.Items), len(want.Items))
			}
			seen := map[dto.UUID]bool{}
			for _, program := range page.Items {
				if program.Type != "Program" || program.ChannelID == nil || !inSet[*program.ChannelID] {
					t.Fatalf("%s: programme %s type %q channel %v", label, program.Name, program.Type, program.ChannelID)
				}
				seen[*program.ChannelID] = true
			}
			if len(seen) != len(set) {
				t.Fatalf("%s: programmes for %d channels, want %d", label, len(seen), len(set))
			}
		}
		query := url.Values{
			"channelIds":   {join(set)},
			"userId":       {family.ID.String()},
			"minStartDate": {"2026-03-08T00:00:00Z"},
			"maxStartDate": {"2026-03-09T00:00:00Z"},
		}
		var page dto.QueryResult[dto.BaseItemDto]
		expect(t, family, http.MethodGet, "/LiveTv/Programs?"+query.Encode(), "", http.StatusOK, &page)
		check(t, "GET", page)

		quoted := `"` + strings.ReplaceAll(join(set), ",", `","`) + `"`
		body := `{"UserId":"` + family.ID.String() + `","ChannelIds":[` + quoted + `],"MinStartDate":"2026-03-08T00:00:00Z","MaxStartDate":"2026-03-09T00:00:00Z"}`
		page = dto.QueryResult[dto.BaseItemDto]{}
		expect(t, family, http.MethodPost, "/LiveTv/Programs", body, http.StatusOK, &page)
		check(t, "POST", page)

		for _, other := range withoutLiveTv {
			page = dto.QueryResult[dto.BaseItemDto]{}
			expect(t, other, http.MethodPost, "/LiveTv/Programs", strings.Replace(body, family.ID.String(), other.ID.String(), 1), http.StatusOK, &page)
			if len(page.Items) != 0 {
				t.Fatalf("%s got %d programmes", other.Name, len(page.Items))
			}
		}
	})
}

func sameSet(t *testing.T, label string, got, want []dto.UUID) {
	t.Helper()
	have := make(map[dto.UUID]int, len(got))
	for _, id := range got {
		have[id]++
	}
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want the set %v", label, got, want)
	}
	for _, id := range want {
		if have[id] != 1 {
			t.Fatalf("%s = %v, want the set %v", label, got, want)
		}
	}
}
