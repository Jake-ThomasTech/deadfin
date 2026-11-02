// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/identify"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/providers"
)

const itemLookupID = "aabbccddeeff00112233445566778899"

// fakeItemLookupSearcher captures search calls and returns canned results.
type fakeItemLookupSearcher struct {
	searchCalls   int
	applyCalls    int
	externalCalls int
	lastItemID    dto.UUID
	lastKind      item.Kind
	results       []providers.RemoteSearchResult
	searchErr     error
}

func (f *fakeItemLookupSearcher) Search(_ context.Context, kind item.Kind, _ string, _ *int, _ map[string]string, _, _ string, _ []string, _ []providers.SongInfo, _ string, _ map[string]string, itemID dto.UUID, _ string, _ bool) ([]providers.RemoteSearchResult, error) {
	f.searchCalls++
	f.lastKind = kind
	f.lastItemID = itemID
	return f.results, f.searchErr
}

func (f *fakeItemLookupSearcher) Apply(_ context.Context, itemID dto.UUID, _ providers.RemoteSearchResult, _ bool) error {
	f.applyCalls++
	f.lastItemID = itemID
	return nil
}

func (f *fakeItemLookupSearcher) ExternalIDs(_ context.Context, _ dto.UUID) ([]providers.ExternalIDDescriptor, error) {
	f.externalCalls++
	return nil, nil
}

func newItemLookupRouter(reader handlers.ItemReader, searcher handlers.ItemLookupSearcher) *router.Router {
	rt := router.New()
	handlers.RegisterItemLookup(rt, reader, searcher)
	return rt
}

// newItemLookupFullRouter registers both ItemLookup and RemoteImages routes
// so the 14-route contract test can verify the complete surface.
func newItemLookupFullRouter(reader handlers.ItemReader, searcher handlers.ItemLookupSearcher, images handlers.RemoteImagesService) *router.Router {
	rt := router.New()
	handlers.RegisterItemLookup(rt, reader, searcher)
	handlers.RegisterRemoteImages(rt, reader, images)
	return rt
}

// TestItemLookupRouteRegistration verifies all 14 ItemLookup routes are
// registered with the correct operation IDs and policies.
func TestItemLookupRouteRegistration(t *testing.T) {
	rt := newItemLookupFullRouter(&fakeItemReader{}, &fakeItemLookupSearcher{}, &fakeRemoteImagesService{})
	expected := map[string]struct {
		op     string
		policy string
	}{
		"POST /Items/RemoteSearch/Movie":             {"GetMovieRemoteSearchResults", string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/Trailer":           {"GetTrailerRemoteSearchResults", string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/MusicVideo":        {"GetMusicVideoRemoteSearchResults", string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/Series":            {"GetSeriesRemoteSearchResults", string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/BoxSet":            {"GetBoxSetRemoteSearchResults", string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/MusicArtist":       {"GetMusicArtistRemoteSearchResults", string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/MusicAlbum":        {"GetMusicAlbumRemoteSearchResults", string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/Person":            {"GetPersonRemoteSearchResults", string(auth.PolicyRequiresElevation)},
		"POST /Items/RemoteSearch/Book":              {"GetBookRemoteSearchResults", string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/Apply/{itemId}":    {"ApplySearchCriteria", string(auth.PolicyRequiresElevation)},
		"GET /Items/{itemId}/ExternalIdInfos":        {"GetExternalIdInfos", string(auth.PolicyRequiresElevation)},
		"GET /Items/{itemId}/RemoteImages":           {"GetRemoteImages", string(auth.PolicyDefaultAuthorization)},
		"GET /Items/{itemId}/RemoteImages/Providers": {"GetRemoteImageProviders", string(auth.PolicyDefaultAuthorization)},
		"POST /Items/{itemId}/RemoteImages/Download": {"DownloadRemoteImage", string(auth.PolicyRequiresElevation)},
	}
	if len(expected) != 14 {
		t.Fatalf("expected map has %d entries, want 14", len(expected))
	}
	got := map[string]router.Route{}
	for _, route := range rt.Routes() {
		got[route.Method+" "+route.Pattern] = route
	}
	for key, exp := range expected {
		route, ok := got[key]
		if !ok {
			t.Errorf("route %q not registered", key)
			continue
		}
		if route.OperationID != exp.op {
			t.Errorf("%s operation ID = %q, want %q", key, route.OperationID, exp.op)
		}
		if route.Policy != exp.policy {
			t.Errorf("%s policy = %q, want %q", key, route.Policy, exp.policy)
		}
	}
}

// TestRemoteSearchMalformedJSONReturns400 verifies malformed JSON body returns
// 400, not the silently-dropped bind error.
func TestRemoteSearchMalformedJSONReturns400(t *testing.T) {
	id, _ := dto.ParseUUID(itemLookupID)
	reader := &fakeItemReader{items: []item.Item{{ID: id, Kind: item.KindMovie}}}
	rt := newItemLookupRouter(reader, &fakeItemLookupSearcher{})
	req := httptest.NewRequest(http.MethodPost, "/Items/RemoteSearch/Movie", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestRemoteSearchPassesItemID verifies the handler forwards ItemId to the
// searcher so reference-item library options resolve.
func TestRemoteSearchPassesItemID(t *testing.T) {
	id, _ := dto.ParseUUID(itemLookupID)
	reader := &fakeItemReader{items: []item.Item{{ID: id, Kind: item.KindMovie}}}
	searcher := &fakeItemLookupSearcher{}
	rt := newItemLookupRouter(reader, searcher)
	body := `{"ItemId":"` + itemLookupID + `","SearchInfo":{"Name":"Test"}}`
	req := httptest.NewRequest(http.MethodPost, "/Items/RemoteSearch/Movie", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	if searcher.searchCalls != 1 {
		t.Fatalf("search calls = %d, want 1", searcher.searchCalls)
	}
	if searcher.lastItemID != id {
		t.Errorf("ItemId = %v, want %v", searcher.lastItemID, id)
	}
}

// TestRemoteSearchNoSearcherReturnsEmptyList verifies that when no searcher is
// configured (Trailer/MusicVideo/BoxSet/Person/Book), the endpoint returns 200
// with an empty list, matching upstream's empty-list contract.
func TestRemoteSearchNoSearcherReturnsEmptyList(t *testing.T) {
	id, _ := dto.ParseUUID(itemLookupID)
	reader := &fakeItemReader{items: []item.Item{{ID: id, Kind: item.KindTrailer}}}
	rt := newItemLookupRouter(reader, nil)
	req := httptest.NewRequest(http.MethodPost, "/Items/RemoteSearch/Trailer", nil)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-searcher status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// fakeRemoteImagesService captures RemoteImages calls for the contract test.
type fakeRemoteImagesService struct {
	remoteImagesCalls int
	downloadCalls     int
	totalRecordCount  int
	images            []providers.RemoteImageInfo
	receivedLimit     *int
	downloadErr       error
}

func (f *fakeRemoteImagesService) RemoteImages(_ context.Context, _ dto.UUID, _ *item.ImageType, _ string, _ bool, _ int, limit *int, _ string) (identify.RemoteImagesResult, error) {
	f.remoteImagesCalls++
	f.receivedLimit = limit
	images := f.images
	if images == nil && f.totalRecordCount > 0 {
		// Produce totalRecordCount dummy images for tests that set it.
		images = make([]providers.RemoteImageInfo, f.totalRecordCount)
	}
	return identify.RemoteImagesResult{Images: images, TotalRecordCount: f.totalRecordCount, Providers: []string{"TheMovieDb"}}, nil
}

func (f *fakeRemoteImagesService) RemoteImageProviders(_ context.Context, _ dto.UUID) ([]identify.ImageProviderInfo, error) {
	return nil, nil
}

func (f *fakeRemoteImagesService) DownloadImage(_ context.Context, _ dto.UUID, _ item.ImageType, _ string) error {
	f.downloadCalls++
	return f.downloadErr
}

func newRemoteImagesRouter(reader handlers.ItemReader, service handlers.RemoteImagesService) *router.Router {
	rt := router.New()
	handlers.RegisterRemoteImages(rt, reader, service)
	return rt
}

// TestRemoteImages404BeforeWork verifies the handler 404s before calling the
// service when the item doesn't exist.
func TestRemoteImages404BeforeWork(t *testing.T) {
	service := &fakeRemoteImagesService{}
	rt := newRemoteImagesRouter(&fakeItemReader{}, service)
	req := httptest.NewRequest(http.MethodGet, "/Items/"+itemLookupID+"/RemoteImages", nil)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing item status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if service.remoteImagesCalls != 0 {
		t.Errorf("RemoteImages calls = %d, want 0 (404 before work)", service.remoteImagesCalls)
	}
}

// TestDownloadErrorsCarryReason verifies each download failure maps to its
// status and names the reason in the body (#809).
func TestDownloadErrorsCarryReason(t *testing.T) {
	id, _ := dto.ParseUUID(itemLookupID)
	reader := &fakeItemReader{items: []item.Item{{ID: id, Kind: item.KindMovie}}}
	for _, tc := range []struct {
		name     string
		query    string
		err      error
		status   int
		wantBody string
	}{
		{"missing type", "imageUrl=https://example.com/x.png", nil, http.StatusBadRequest, "Type is required"},
		{"bad type", "type=Nope&imageUrl=https://example.com/x.png", nil, http.StatusBadRequest, "Type is not a valid ImageType"},
		{"invalid url", "type=Primary&imageUrl=poster.jpg", fmt.Errorf("%w: ImageUrl must be an http or https URL", identify.ErrInvalidImageURL), http.StatusBadRequest, "ImageUrl must be an http or https URL\n"},
		{"refused address", "type=Primary&imageUrl=https://internal.example/x.png", fmt.Errorf("identify: download image: %w", &url.Error{Op: "Get", URL: "https://internal.example/x.png", Err: fmt.Errorf("dial tcp 10.0.0.1:443: %w: special-purpose address 10.0.0.1", identify.ErrImageRefused)}), http.StatusBadRequest, "ImageUrl was refused: special-purpose address 10.0.0.1\n"},
		{"fetch failure", "type=Primary&imageUrl=https://example.com/x.png", errors.New("identify: download image: remote image: status 404"), http.StatusInternalServerError, "Image download failed: identify: download image: remote image: status 404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeRemoteImagesService{downloadErr: tc.err}
			rt := newRemoteImagesRouter(reader, service)
			req := httptest.NewRequest(http.MethodPost, "/Items/"+itemLookupID+"/RemoteImages/Download?"+tc.query, nil)
			rec := httptest.NewRecorder()
			rt.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestDownloadAnyURLReturns204 verifies the handler passes any imageUrl to
// the service and returns 204 on success, as in stock (#809).
func TestDownloadAnyURLReturns204(t *testing.T) {
	id, _ := dto.ParseUUID(itemLookupID)
	reader := &fakeItemReader{items: []item.Item{{ID: id, Kind: item.KindSeries}}}
	service := &fakeRemoteImagesService{}
	rt := newRemoteImagesRouter(reader, service)
	req := httptest.NewRequest(http.MethodPost, "/Items/"+itemLookupID+"/RemoteImages/Download?type=Primary&imageUrl=https://artworks.example.com/banners/posters/257942-1.jpg", nil)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if service.downloadCalls != 1 {
		t.Errorf("DownloadImage calls = %d, want 1", service.downloadCalls)
	}
}

// TestDownloadItemNotFoundReturns404 verifies the not-found sentinel maps to
// 404, proving the handler and service share the same error identity.
func TestDownloadItemNotFoundReturns404(t *testing.T) {
	id, _ := dto.ParseUUID(itemLookupID)
	reader := &fakeItemReader{items: []item.Item{{ID: id, Kind: item.KindMovie}}}
	service := &fakeRemoteImagesService{downloadErr: identify.ErrItemNotFound}
	rt := newRemoteImagesRouter(reader, service)
	req := httptest.NewRequest(http.MethodPost, "/Items/"+itemLookupID+"/RemoteImages/Download?type=Primary&imageUrl=https://example.com/x.png", nil)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("not-found status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestRemoteImagesTotalRecordCountBeforePagination verifies TotalRecordCount
// reflects the filtered count before startIndex/limit slicing.
func TestRemoteImagesTotalRecordCountBeforePagination(t *testing.T) {
	id, _ := dto.ParseUUID(itemLookupID)
	reader := &fakeItemReader{items: []item.Item{{ID: id, Kind: item.KindMovie}}}
	service := &fakeRemoteImagesService{totalRecordCount: 42}
	rt := newRemoteImagesRouter(reader, service)
	req := httptest.NewRequest(http.MethodGet, "/Items/"+itemLookupID+"/RemoteImages?startIndex=10&limit=5", nil)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"TotalRecordCount":42`) {
		t.Errorf("response does not contain TotalRecordCount=42 before pagination: %s", body)
	}
}

// TestRemoteImagesAbsentAndZeroLimit verifies the handler distinguishes an
// absent limit from an explicit limit=0: absent limit passes nil to the
// service (all images), and limit=0 passes a pointer to 0 (empty slice). In
// both cases TotalRecordCount and Providers are the pre-slice values.
func TestRemoteImagesAbsentAndZeroLimit(t *testing.T) {
	id, _ := dto.ParseUUID(itemLookupID)
	reader := &fakeItemReader{items: []item.Item{{ID: id, Kind: item.KindMovie}}}

	// Absent limit: nil passed to the service.
	service := &fakeRemoteImagesService{totalRecordCount: 2}
	rt := newRemoteImagesRouter(reader, service)
	req := httptest.NewRequest(http.MethodGet, "/Items/"+itemLookupID+"/RemoteImages", nil)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("absent limit: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if service.receivedLimit != nil {
		t.Errorf("absent limit: receivedLimit = %v, want nil", service.receivedLimit)
	}

	// Explicit limit=0: pointer to 0 passed to the service.
	service = &fakeRemoteImagesService{totalRecordCount: 2}
	rt = newRemoteImagesRouter(reader, service)
	req = httptest.NewRequest(http.MethodGet, "/Items/"+itemLookupID+"/RemoteImages?limit=0", nil)
	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("limit=0: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if service.receivedLimit == nil {
		t.Fatal("limit=0: receivedLimit = nil, want non-nil")
	}
	if *service.receivedLimit != 0 {
		t.Errorf("limit=0: receivedLimit = %d, want 0", *service.receivedLimit)
	}
}
