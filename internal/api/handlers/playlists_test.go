// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/playlists"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

// playlistUsers is an in-memory user resolver.
type playlistUsers map[dto.UUID]store.User

func (users playlistUsers) Get(_ context.Context, id dto.UUID) (store.User, error) {
	if value, ok := users[id]; ok {
		return value, nil
	}
	return store.User{}, store.ErrNotFound
}

// playlistResolver authenticates the request as the user named by the
// X-Test-User header, or as an API key when the header is "apikey".
type playlistResolver struct{}

func (playlistResolver) Resolve(_ context.Context, r *http.Request) (auth.Identity, error) {
	value := r.Header.Get("X-Test-User")
	if value == "apikey" {
		return auth.Identity{Authenticated: true, APIKey: true, Administrator: true}, nil
	}
	id, err := dto.ParseUUID(value)
	if err != nil {
		return auth.Identity{}, nil
	}
	return auth.Identity{Authenticated: true, UserID: id}, nil
}

type playlistAllowAll struct{}

func (playlistAllowAll) Evaluate(context.Context, auth.PolicyRequest) (auth.Decision, error) {
	return auth.DecisionAllowed, nil
}

type playlistFixture struct {
	t        *testing.T
	items    *store.ItemStore
	router   *router.Router
	owner    dto.UUID
	friend   dto.UUID
	stranger dto.UUID
	movies   []item.Item
	album    item.Item
	tracks   []item.Item
	data     string
	service  *playlists.Service
}

func newPlaylistFixture(t *testing.T) *playlistFixture {
	t.Helper()
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	items := store.NewItemStore(stores)
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(data, "playlists"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := items.BootstrapSystemFolders(t.Context(), root); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	f := &playlistFixture{t: t, items: items, owner: collUUID(t, 900), friend: collUUID(t, 901), stranger: collUUID(t, 902), data: data}
	users := playlistUsers{}
	for _, id := range []dto.UUID{f.owner, f.friend, f.stranger} {
		users[id] = store.User{ID: id, Name: id.String()}
	}
	library := item.Item{ID: collUUID(t, 910), Kind: item.KindCollectionFolder, Name: strPtr("Movies"), IsFolder: true}
	if err := items.Upsert(t.Context(), library); err != nil {
		t.Fatalf("seed library: %v", err)
	}
	for index, name := range []string{"Alpha", "Bravo", "Charlie", "Delta"} {
		movie := collMovieItem(t, 920+index, name)
		movie.ParentID = &library.ID
		video := item.MediaTypeVideo
		movie.MediaType = &video
		if err := items.Upsert(t.Context(), movie); err != nil {
			t.Fatalf("seed movie: %v", err)
		}
		f.movies = append(f.movies, movie)
	}
	f.album = item.Item{ID: collUUID(t, 940), Kind: item.KindMusicAlbum, Name: strPtr("Album"), IsFolder: true, ParentID: &library.ID}
	if err := items.Upsert(t.Context(), f.album); err != nil {
		t.Fatalf("seed album: %v", err)
	}
	for index, name := range []string{"0002 - Second", "0001 - First"} {
		audio := item.MediaTypeAudio
		track := item.Item{ID: collUUID(t, 941+index), Kind: item.KindAudio, Name: strPtr(name), SortName: strPtr(name), ParentID: &f.album.ID, MediaType: &audio}
		if err := items.Upsert(t.Context(), track); err != nil {
			t.Fatalf("seed track: %v", err)
		}
		f.tracks = append(f.tracks, track)
	}
	fake := clock.NewFake(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	service, err := playlists.New(playlists.Options{Items: items, Users: users, Clock: fake, ProgramData: root, DataPath: data})
	if err != nil {
		t.Fatalf("playlists: %v", err)
	}
	f.service = service
	authorizer := middleware.NewAuthorizer(playlistResolver{}, playlistAllowAll{}, users)
	f.router = router.New(authorizer.Wrap)
	builder := dtobuild.NewBuilder(items, dtobuild.NewStoreEnricher(items, nil, nil))
	handlers.RegisterPlaylists(f.router, service, users, builder, fake)
	handlers.RegisterItems(f.router, items, builder, fake)
	return f
}

func strPtr(value string) *string { return &value }

func (f *playlistFixture) do(method, path, as, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("X-Test-User", as)
	record := httptest.NewRecorder()
	f.router.ServeHTTP(record, request)
	return record
}

func (f *playlistFixture) expect(record *httptest.ResponseRecorder, status int) {
	f.t.Helper()
	if record.Code != status {
		f.t.Fatalf("status = %d, want %d; body=%s", record.Code, status, record.Body.String())
	}
}

func (f *playlistFixture) create(as, body string) dto.UUID {
	f.t.Helper()
	record := f.do(http.MethodPost, "/Playlists", as, body)
	f.expect(record, http.StatusOK)
	var result struct{ Id string }
	if err := json.Unmarshal(record.Body.Bytes(), &result); err != nil {
		f.t.Fatalf("decode creation result: %v", err)
	}
	id, err := dto.ParseUUID(result.Id)
	if err != nil || len(result.Id) != 32 {
		f.t.Fatalf("creation id %q is not an N-format guid: %v", result.Id, err)
	}
	return id
}

func (f *playlistFixture) playlist(id dto.UUID, as string) dto.PlaylistDto {
	f.t.Helper()
	record := f.do(http.MethodGet, "/Playlists/"+id.String(), as, "")
	f.expect(record, http.StatusOK)
	var result dto.PlaylistDto
	if err := json.Unmarshal(record.Body.Bytes(), &result); err != nil {
		f.t.Fatalf("decode playlist: %v", err)
	}
	return result
}

func (f *playlistFixture) entries(id dto.UUID, as string) []dto.UUID {
	f.t.Helper()
	return f.playlist(id, as).ItemIDs
}

func ids(values ...item.Item) []dto.UUID {
	out := make([]dto.UUID, len(values))
	for index, value := range values {
		out[index] = value.ID
	}
	return out
}

func sameIDs(t *testing.T, label string, got, want []dto.UUID) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	for index := range got {
		if got[index] != want[index] {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
	}
}

// TestPlaylistRoutesRegistered pins the eleven PlaylistsController routes and
// their default authorization policy.
func TestPlaylistRoutesRegistered(t *testing.T) {
	rt := router.New()
	handlers.RegisterPlaylists(rt, nil, nil, nil, nil)
	want := map[string]string{
		"POST /Playlists":                                             "CreatePlaylist",
		"POST /Playlists/{playlistId}":                                "UpdatePlaylist",
		"GET /Playlists/{playlistId}":                                 "GetPlaylist",
		"GET /Playlists/{playlistId}/Users":                           "GetPlaylistUsers",
		"GET /Playlists/{playlistId}/Users/{userId}":                  "GetPlaylistUser",
		"POST /Playlists/{playlistId}/Users/{userId}":                 "UpdatePlaylistUser",
		"DELETE /Playlists/{playlistId}/Users/{userId}":               "RemoveUserFromPlaylist",
		"POST /Playlists/{playlistId}/Items":                          "AddItemToPlaylist",
		"POST /Playlists/{playlistId}/Items/{itemId}/Move/{newIndex}": "MoveItem",
		"DELETE /Playlists/{playlistId}/Items":                        "RemoveItemFromPlaylist",
		"GET /Playlists/{playlistId}/Items":                           "GetPlaylistItems",
	}
	routes := rt.Routes()
	if len(routes) != len(want) {
		t.Fatalf("registered %d routes, want %d", len(routes), len(want))
	}
	for _, route := range routes {
		key := route.Method + " " + route.Pattern
		if want[key] != route.OperationID || route.Policy != string(auth.PolicyDefaultAuthorization) || route.Anonymous {
			t.Fatalf("route %s = %#v", key, route)
		}
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/Playlists/"+collUUID(t, 1).String(), nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil service status = %d, want 503", rec.Code)
	}
}

// TestPlaylistCreateAndRead covers CreatePlaylist through the body DTO and the
// query parameters, the directory upstream creates, media-type inference, and
// GetPlaylist.
func TestPlaylistCreateAndRead(t *testing.T) {
	f := newPlaylistFixture(t)
	owner := f.owner.String()
	id := f.create(owner, `{"Name":"Road Trip","Ids":["`+f.movies[1].ID.String()+`","`+f.movies[0].ID.String()+`"],"Users":[{"UserId":"`+f.friend.String()+`","CanEdit":false}]}`)
	got := f.playlist(id, owner)
	sameIDs(t, "entries", got.ItemIDs, ids(f.movies[1], f.movies[0]))
	if !got.OpenAccess {
		t.Fatalf("body without IsPublic must default to public: %+v", got)
	}
	if len(got.Shares) != 1 || got.Shares[0].UserID != f.friend || got.Shares[0].CanEdit {
		t.Fatalf("shares = %+v", got.Shares)
	}
	if info, err := os.Stat(filepath.Join(f.data, "playlists", "Road Trip")); err != nil || !info.IsDir() {
		t.Fatalf("playlist directory: %v", err)
	}
	stored, err := f.items.Get(t.Context(), id)
	if err != nil || stored.MediaType == nil || *stored.MediaType != item.MediaTypeVideo || stored.PlaylistOwnerUserID != f.owner {
		t.Fatalf("stored playlist = %+v, %v", stored, err)
	}

	// The legacy query form: no body means OpenAccess false. A second
	// playlist of the same name gets the "1" directory suffix.
	record := f.do(http.MethodPost, "/Playlists?name=Road%20Trip&ids="+f.album.ID.String(), owner, "")
	f.expect(record, http.StatusOK)
	var result struct{ Id string }
	_ = json.Unmarshal(record.Body.Bytes(), &result)
	third, _ := dto.ParseUUID(result.Id)
	thirdPlaylist := f.playlist(third, owner)
	if thirdPlaylist.OpenAccess {
		t.Fatalf("query-only create must be private")
	}
	sameIDs(t, "album entries", thirdPlaylist.ItemIDs, ids(f.tracks[1], f.tracks[0]))
	if _, err := os.Stat(filepath.Join(f.data, "playlists", "Road Trip1")); err != nil {
		t.Fatalf("suffixed playlist directory: %v", err)
	}
	stored, _ = f.items.Get(t.Context(), third)
	if stored.MediaType == nil || *stored.MediaType != item.MediaTypeAudio {
		t.Fatalf("album playlist media type = %v, want Audio", stored.MediaType)
	}

	// An unknown item id is an ArgumentException: 400.
	f.expect(f.do(http.MethodPost, "/Playlists", owner, `{"Name":"Bad","Ids":["`+collUUID(t, 999).String()+`"]}`), http.StatusBadRequest)
	// Another user may not create for the owner without administrator rights.
	f.expect(f.do(http.MethodPost, "/Playlists", f.friend.String(), `{"Name":"X","UserId":"`+owner+`"}`), http.StatusForbidden)
}

// TestPlaylistCreateDotNamesStayInFolder pins GetTargetPath for "." and
// "..": Path.Combine keeps them as existing names, so they take the "1"
// suffix inside the playlists folder.
func TestPlaylistCreateDotNamesStayInFolder(t *testing.T) {
	f := newPlaylistFixture(t)
	owner := f.owner.String()
	f.create(owner, `{"Name":"..","Ids":[]}`)
	f.create(owner, `{"Name":".","Ids":[]}`)
	f.create(owner, `{"Name":"../..","Ids":[]}`)
	for _, name := range []string{"..1", ".1", ".. .."} {
		if info, err := os.Stat(filepath.Join(f.data, "playlists", name)); err != nil || !info.IsDir() {
			t.Fatalf("playlist directory %q: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.data), "data1")); !os.IsNotExist(err) {
		t.Fatalf("playlist directory escaped the playlists folder: %v", err)
	}
}

// TestPlaylistCreateRequiresName returns 400 for a nameless request.
func TestPlaylistCreateRequiresName(t *testing.T) {
	f := newPlaylistFixture(t)
	f.expect(f.do(http.MethodPost, "/Playlists", f.owner.String(), `{"Ids":[]}`), http.StatusBadRequest)
}

// TestPlaylistVisibilityAndSharing covers GetPlaylistForUser and the share
// endpoints' permission matrix.
func TestPlaylistVisibilityAndSharing(t *testing.T) {
	f := newPlaylistFixture(t)
	owner, friend, stranger := f.owner.String(), f.friend.String(), f.stranger.String()
	id := f.create(owner, `{"Name":"Private","IsPublic":false,"Ids":["`+f.movies[0].ID.String()+`"]}`)
	base := "/Playlists/" + id.String()

	// A private playlist is invisible to others: 404 with the upstream body.
	record := f.do(http.MethodGet, base, stranger, "")
	f.expect(record, http.StatusNotFound)
	if strings.TrimSpace(record.Body.String()) != `"Playlist not found"` {
		t.Fatalf("404 body = %s", record.Body.String())
	}

	// The owner shares read-only access with friend.
	f.expect(f.do(http.MethodPost, base+"/Users/"+friend, owner, `{"CanEdit":false}`), http.StatusNoContent)
	f.playlist(id, friend)
	f.expect(f.do(http.MethodGet, base+"/Users", friend, ""), http.StatusForbidden)
	record = f.do(http.MethodGet, base+"/Users", owner, "")
	f.expect(record, http.StatusOK)
	var shares []dto.PlaylistUserPermissions
	_ = json.Unmarshal(record.Body.Bytes(), &shares)
	if len(shares) != 1 || shares[0].UserID != f.friend || shares[0].CanEdit {
		t.Fatalf("shares = %+v", shares)
	}

	// GetPlaylistUser: the owner always gets its own edit grant; a reader may
	// read its own share but not another's.
	record = f.do(http.MethodGet, base+"/Users/"+stranger, owner, "")
	f.expect(record, http.StatusOK)
	var permission dto.PlaylistUserPermissions
	_ = json.Unmarshal(record.Body.Bytes(), &permission)
	if permission.UserID != f.owner || !permission.CanEdit {
		t.Fatalf("owner permission = %+v", permission)
	}
	f.expect(f.do(http.MethodGet, base+"/Users/"+friend, friend, ""), http.StatusOK)
	f.expect(f.do(http.MethodGet, base+"/Users/"+owner, friend, ""), http.StatusForbidden)

	// A read-only share cannot edit.
	f.expect(f.do(http.MethodPost, base+"/Items?ids="+f.movies[1].ID.String(), friend, ""), http.StatusForbidden)
	f.expect(f.do(http.MethodPost, base, friend, `{"Name":"Mine"}`), http.StatusForbidden)

	// Upgrading the share to CanEdit lets friend add items.
	f.expect(f.do(http.MethodPost, base+"/Users/"+friend, owner, `{"CanEdit":true}`), http.StatusNoContent)
	f.expect(f.do(http.MethodPost, base+"/Items?ids="+f.movies[1].ID.String(), friend, ""), http.StatusNoContent)
	sameIDs(t, "entries after friend add", f.entries(id, owner), ids(f.movies[0], f.movies[1]))
	// Only the owner manages shares.
	f.expect(f.do(http.MethodPost, base+"/Users/"+stranger, friend, `{"CanEdit":true}`), http.StatusForbidden)

	// Removing an absent share is 404; removing friend hides the playlist.
	f.expect(f.do(http.MethodDelete, base+"/Users/"+stranger, owner, ""), http.StatusNotFound)
	f.expect(f.do(http.MethodDelete, base+"/Users/"+friend, owner, ""), http.StatusNoContent)
	f.expect(f.do(http.MethodGet, base, friend, ""), http.StatusNotFound)

	// UpdatePlaylist replaces the share list and opens access.
	f.expect(f.do(http.MethodPost, base, owner, `{"IsPublic":true,"Users":[{"UserId":"`+stranger+`","CanEdit":true}]}`), http.StatusNoContent)
	got := f.playlist(id, friend)
	if !got.OpenAccess || len(got.Shares) != 1 || got.Shares[0].UserID != f.stranger {
		t.Fatalf("after update = %+v", got)
	}
}

// TestPlaylistEntryEditing covers AddItemToPlaylist positions and duplicate
// filtering, MoveItem, RemoveItemFromPlaylist, and UpdatePlaylist Ids/Name.
func TestPlaylistEntryEditing(t *testing.T) {
	f := newPlaylistFixture(t)
	owner := f.owner.String()
	a, b, c, d := f.movies[0], f.movies[1], f.movies[2], f.movies[3]
	id := f.create(owner, `{"Name":"Edit","Ids":["`+a.ID.String()+`","`+b.ID.String()+`"]}`)
	base := "/Playlists/" + id.String()

	// Duplicates of present entries are ignored; position 1 inserts after A.
	f.expect(f.do(http.MethodPost, base+"/Items?ids="+a.ID.String()+","+c.ID.String()+"&position=1", owner, ""), http.StatusNoContent)
	sameIDs(t, "after positioned add", f.entries(id, owner), ids(a, c, b))
	f.expect(f.do(http.MethodPost, base+"/Items?ids="+d.ID.String()+"&position=0", owner, ""), http.StatusNoContent)
	sameIDs(t, "after prepend", f.entries(id, owner), ids(d, a, c, b))

	// MoveItem with the N-format entry id.
	f.expect(f.do(http.MethodPost, base+"/Items/"+d.ID.String()+"/Move/3", owner, ""), http.StatusNoContent)
	sameIDs(t, "after move to end", f.entries(id, owner), ids(a, c, b, d))
	f.expect(f.do(http.MethodPost, base+"/Items/"+b.ID.String()+"/Move/0", owner, ""), http.StatusNoContent)
	sameIDs(t, "after move to front", f.entries(id, owner), ids(b, a, c, d))
	f.expect(f.do(http.MethodPost, base+"/Items/"+d.ID.String()+"/Move/1", owner, ""), http.StatusNoContent)
	sameIDs(t, "after move to 1", f.entries(id, owner), ids(b, d, a, c))

	// GetPlaylistItems carries PlaylistItemId and pages.
	record := f.do(http.MethodGet, base+"/Items?startIndex=1&limit=2", owner, "")
	f.expect(record, http.StatusOK)
	var page struct {
		Items []struct {
			Id             string
			PlaylistItemId string
		}
		TotalRecordCount int
		StartIndex       int
	}
	if err := json.Unmarshal(record.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode items: %v", err)
	}
	if page.TotalRecordCount != 4 || page.StartIndex != 1 || len(page.Items) != 2 || page.Items[0].Id != d.ID.String() || page.Items[0].PlaylistItemId != d.ID.String() || page.Items[1].Id != a.ID.String() {
		t.Fatalf("items page = %+v", page)
	}

	// /Items?ParentId=<playlist> browses the entries in playlist order.
	record = f.do(http.MethodGet, "/Items?ParentId="+id.String()+"&userId="+owner, owner, "")
	f.expect(record, http.StatusOK)
	var browse struct{ Items []struct{ Id string } }
	_ = json.Unmarshal(record.Body.Bytes(), &browse)
	if len(browse.Items) != 4 || browse.Items[0].Id != b.ID.String() || browse.Items[3].Id != c.ID.String() {
		t.Fatalf("parent browse = %+v", browse)
	}

	// RemoveItemFromPlaylist ignores unknown entry ids.
	f.expect(f.do(http.MethodDelete, base+"/Items?entryIds="+d.ID.String()+",ffffffffffffffffffffffffffffffff", owner, ""), http.StatusNoContent)
	sameIDs(t, "after remove", f.entries(id, owner), ids(b, a, c))

	// UpdatePlaylist with Ids replaces the entries; Name renames.
	f.expect(f.do(http.MethodPost, base, owner, `{"Name":"Renamed","Ids":"`+c.ID.String()+`,`+a.ID.String()+`"}`), http.StatusNoContent)
	sameIDs(t, "after update ids", f.entries(id, owner), ids(c, a))
	stored, err := f.items.Get(t.Context(), id)
	if err != nil || stored.Name == nil || *stored.Name != "Renamed" || stored.SortName == nil || *stored.SortName != "renamed" {
		t.Fatalf("renamed playlist = %v / %v, %v", stored.Name, stored.SortName, err)
	}
	if stored.DateLastMediaAdded == nil {
		t.Fatalf("DateLastMediaAdded is not set after adding items")
	}

	// An empty UpdatePlaylist body is rejected.
	f.expect(f.do(http.MethodPost, base, owner, ""), http.StatusBadRequest)
	// Unknown playlists are 404 for every route.
	missing := "/Playlists/" + collUUID(t, 998).String()
	f.expect(f.do(http.MethodGet, missing+"/Items", owner, ""), http.StatusNotFound)
	f.expect(f.do(http.MethodPost, missing+"/Items?ids="+a.ID.String(), owner, ""), http.StatusNotFound)
	f.expect(f.do(http.MethodDelete, missing+"/Items?entryIds="+a.ID.String(), "apikey", ""), http.StatusNotFound)
}

// TestPlaylistRemoveWithAPIKey proves RemoveItemFromPlaylist accepts an API
// key without a user, as upstream does.
func TestPlaylistRemoveWithAPIKey(t *testing.T) {
	f := newPlaylistFixture(t)
	a, b := f.movies[0], f.movies[1]
	id := f.create(f.owner.String(), `{"Name":"Keyed","Ids":["`+a.ID.String()+`","`+b.ID.String()+`"]}`)
	f.expect(f.do(http.MethodDelete, "/Playlists/"+id.String()+"/Items?entryIds="+a.ID.String(), "apikey", ""), http.StatusNoContent)
	sameIDs(t, "after API-key remove", f.entries(id, f.owner.String()), ids(b))
}

// TestPlaylistItemsWithAPIKeyUserID proves GetPlaylistItems honors the userId
// query for an API key, as upstream does (userId ?? User.GetUserId()).
func TestPlaylistItemsWithAPIKeyUserID(t *testing.T) {
	f := newPlaylistFixture(t)
	a, b := f.movies[0], f.movies[1]
	id := f.create(f.owner.String(), `{"Name":"Keyed","Ids":["`+a.ID.String()+`","`+b.ID.String()+`"]}`)
	base := "/Playlists/" + id.String() + "/Items"
	f.expect(f.do(http.MethodGet, base, "apikey", ""), http.StatusNotFound)
	record := f.do(http.MethodGet, base+"?userId="+f.owner.String(), "apikey", "")
	f.expect(record, http.StatusOK)
	var page struct {
		Items            []struct{ Id string }
		TotalRecordCount int
	}
	if err := json.Unmarshal(record.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode items: %v", err)
	}
	if page.TotalRecordCount != 2 || len(page.Items) != 2 || page.Items[0].Id != a.ID.String() || page.Items[1].Id != b.ID.String() {
		t.Fatalf("items page = %+v", page)
	}
	f.expect(f.do(http.MethodGet, base+"?userId=not-a-guid", "apikey", ""), http.StatusBadRequest)
}

// TestPlaylistImport proves an imported Jellyfin playlist keeps its owner,
// shares, open access, and entry order, and that a rerun keeps it.
func TestPlaylistImport(t *testing.T) {
	f := newPlaylistFixture(t)
	created := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	request := playlists.ImportRequest{
		Name: "Imported: mix", OwnerUserID: f.owner, MediaType: item.MediaTypeVideo, DateCreated: &created,
		Shares:   []store.ItemShare{{UserID: f.friend, CanEdit: true}},
		Children: []dto.UUID{f.movies[2].ID, f.movies[0].ID, f.movies[2].ID},
	}
	ok, err := f.service.Import(t.Context(), request)
	if err != nil || !ok {
		t.Fatalf("import = %v, %v", ok, err)
	}
	if ok, err := f.service.Import(t.Context(), request); err != nil || ok {
		t.Fatalf("rerun import = %v, %v, want an existing playlist", ok, err)
	}
	if _, err := os.Stat(filepath.Join(f.data, "playlists", "Imported  mix")); err != nil {
		t.Fatalf("playlist directory: %v", err)
	}
	page, err := f.items.Query(t.Context(), store.ItemQuery{IncludeItemTypes: []item.Kind{item.KindPlaylist}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("playlists = %+v, %v", page.Items, err)
	}
	value := page.Items[0]
	if value.DateCreated == nil || !value.DateCreated.Equal(created) || value.MediaType == nil || *value.MediaType != item.MediaTypeVideo {
		t.Fatalf("playlist item = %+v", value)
	}
	got := f.playlist(value.ID, f.friend.String())
	sameIDs(t, "imported entries", got.ItemIDs, ids(f.movies[2], f.movies[0]))
	if len(got.Shares) != 1 || got.Shares[0].UserID != f.friend || !got.Shares[0].CanEdit || got.OpenAccess {
		t.Fatalf("imported playlist = %+v", got)
	}
	f.expect(f.do(http.MethodGet, "/Playlists/"+value.ID.String(), f.stranger.String(), ""), http.StatusNotFound)
}
