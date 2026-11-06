// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/livetv"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

// fakeLiveTvAdmin records the calls of the Live TV administration routes.
type fakeLiveTvAdmin struct {
	tuner    config.TunerHostInfo
	provider config.ListingsProviderInfo
	deleted  []string
	mapping  [3]string
	err      error
}

func (f *fakeLiveTvAdmin) GuideInfo() (time.Time, time.Time) {
	start := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 0, 7)
}

func (f *fakeLiveTvAdmin) TunerHostTypes() []tuner.TypeInfo {
	return []tuner.TypeInfo{{Name: "M3U Tuner", ID: "m3u"}}
}

func (f *fakeLiveTvAdmin) SaveTunerHost(_ context.Context, info config.TunerHostInfo) (config.TunerHostInfo, error) {
	if f.err != nil {
		return config.TunerHostInfo{}, f.err
	}
	info.Id = "00000000000000000000000000000001"
	f.tuner = info
	return info, nil
}

func (f *fakeLiveTvAdmin) DeleteTunerHost(_ context.Context, id string) error {
	f.deleted = append(f.deleted, "tuner:"+id)
	return nil
}

func (f *fakeLiveTvAdmin) SaveListingProvider(_ context.Context, info config.ListingsProviderInfo) (config.ListingsProviderInfo, error) {
	if f.err != nil {
		return config.ListingsProviderInfo{}, f.err
	}
	info.Id = "00000000000000000000000000000002"
	f.provider = info
	return info, nil
}

func (f *fakeLiveTvAdmin) DeleteListingProvider(_ context.Context, id string) error {
	f.deleted = append(f.deleted, "provider:"+id)
	return nil
}

func (f *fakeLiveTvAdmin) Lineups(_ context.Context, providerType, providerID string) ([]livetv.NameID, error) {
	if providerID == "" {
		return nil, fmt.Errorf("%w: id", livetv.ErrNotFound)
	}
	return []livetv.NameID{{Name: "Guide A", ID: "ga"}}, nil
}

func (f *fakeLiveTvAdmin) ChannelMappingOptions(_ context.Context, providerID string) (livetv.ChannelMappingOptions, error) {
	if providerID != "p1" {
		return livetv.ChannelMappingOptions{}, livetv.ErrNotFound
	}
	return livetv.ChannelMappingOptions{
		TunerChannels:    []livetv.TunerChannelMapping{{Name: "1 A", ProviderChannelName: "Guide A", ProviderChannelID: "ga", ID: "m3u_a"}, {Name: "2 B", ID: "m3u_b"}},
		ProviderChannels: []livetv.NameID{{Name: "Guide A", ID: "ga"}},
		ProviderName:     "XmlTV",
	}, nil
}

func (f *fakeLiveTvAdmin) SetChannelMapping(_ context.Context, providerID, tunerChannelID, providerChannelID string) (livetv.TunerChannelMapping, error) {
	f.mapping = [3]string{providerID, tunerChannelID, providerChannelID}
	return livetv.TunerChannelMapping{Name: "2 B", ProviderChannelName: "Guide Z", ProviderChannelID: providerChannelID, ID: tunerChannelID}, nil
}

func (f *fakeLiveTvAdmin) TunerHostGroups(id string) (livetv.TunerHostGroups, error) {
	if id != "t1" {
		return livetv.TunerHostGroups{}, livetv.ErrNotFound
	}
	return livetv.TunerHostGroups{Groups: []tuner.GroupCount{{Name: "News", Count: 9}, {Name: "", Count: 1}}, Loaded: true}, nil
}

func newLiveTvAdminRouter(admin *fakeLiveTvAdmin) *router.Router {
	rt := router.New()
	handlers.RegisterLiveTv(rt, admin, handlers.LiveTvReads{})
	return rt
}

func TestLiveTvAddTunerHostDecodesOverDefaults(t *testing.T) {
	admin := &fakeLiveTvAdmin{}
	rt := newLiveTvAdminRouter(admin)
	record := liveTvRequest(rt, http.MethodPost, "/LiveTv/TunerHosts", `{"Type":"m3u","Url":"/tmp/a.m3u","FriendlyName":"Home"}`)
	if record.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", record.Code, record.Body.String())
	}
	if !admin.tuner.AllowStreamSharing || admin.tuner.FallbackMaxStreamingBitrate != 30000000 || admin.tuner.Url != "/tmp/a.m3u" {
		t.Fatalf("tuner = %+v", admin.tuner)
	}
	var body map[string]any
	if err := json.Unmarshal(record.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["Id"] != "00000000000000000000000000000001" || body["FriendlyName"] != "Home" || body["Type"] != "m3u" {
		t.Fatalf("body = %v", body)
	}
	if _, ok := body["SkipVodEntries"]; ok {
		t.Fatalf("unset Deadfin field rendered: %v", body)
	}

	for _, tc := range []struct {
		err  error
		want int
	}{
		{livetv.ErrUnknownType, http.StatusNotFound},
		{fmt.Errorf("m3u tuner: %w", os.ErrNotExist), http.StatusNotFound},
		{tuner.ErrNoTunerURL, http.StatusBadRequest},
		{errors.New("m3u tuner: 500"), http.StatusInternalServerError},
	} {
		admin.err = tc.err
		if got := liveTvRequest(rt, http.MethodPost, "/LiveTv/TunerHosts", `{"Type":"m3u"}`).Code; got != tc.want {
			t.Errorf("%v: status %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestLiveTvAddListingProviderHashesPassword(t *testing.T) {
	admin := &fakeLiveTvAdmin{}
	rt := newLiveTvAdminRouter(admin)
	record := liveTvRequest(rt, http.MethodPost, "/LiveTv/ListingProviders?pw=secret&validateListings=true", `{"Type":"xmltv","Path":"/tmp/g.xml"}`)
	if record.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", record.Code, record.Body.String())
	}
	// SHA-1("secret"), lower-case hex (LiveTvController.cs:1003-1006).
	if admin.provider.Password != "e5e9fa1ba31ecd1ae84f75caaa474f3a663f05f4" || !admin.provider.EnableAllTuners || len(admin.provider.MovieCategories) != 1 {
		t.Fatalf("provider = %+v", admin.provider)
	}
}

func TestLiveTvDeletesAndLookups(t *testing.T) {
	admin := &fakeLiveTvAdmin{}
	rt := newLiveTvAdminRouter(admin)
	for _, path := range []string{"/LiveTv/TunerHosts?id=t1", "/LiveTv/ListingProviders?id=p1"} {
		if record := liveTvRequest(rt, http.MethodDelete, path, ""); record.Code != http.StatusNoContent {
			t.Fatalf("DELETE %s = %d", path, record.Code)
		}
	}
	if strings.Join(admin.deleted, ",") != "tuner:t1,provider:p1" {
		t.Fatalf("deleted %v", admin.deleted)
	}

	for _, tc := range []struct {
		path, want string
	}{
		{"/LiveTv/GuideInfo", `{"StartDate":"2026-03-08T12:00:00.0000000Z","EndDate":"2026-03-15T12:00:00.0000000Z"}`},
		{"/LiveTv/TunerHosts/Types", `[{"Name":"M3U Tuner","Id":"m3u"}]`},
		{"/LiveTv/ListingProviders/Lineups?id=p1", `[{"Name":"Guide A","Id":"ga"}]`},
		{"/LiveTv/ChannelMappingOptions?providerId=p1", `{"TunerChannels":[{"Name":"1 A","ProviderChannelName":"Guide A","ProviderChannelId":"ga","Id":"m3u_a"},{"Name":"2 B","Id":"m3u_b"}],"ProviderChannels":[{"Name":"Guide A","Id":"ga"}],"Mappings":[],"ProviderName":"XmlTV"}`},
	} {
		record := liveTvRequest(rt, http.MethodGet, tc.path, "")
		if record.Code != http.StatusOK || strings.TrimSpace(record.Body.String()) != tc.want {
			t.Errorf("GET %s = %d %s, want %s", tc.path, record.Code, record.Body.String(), tc.want)
		}
	}
	if got := liveTvRequest(rt, http.MethodGet, "/LiveTv/ChannelMappingOptions?providerId=nope", "").Code; got != http.StatusNotFound {
		t.Errorf("unknown provider status %d", got)
	}
	if got := liveTvRequest(rt, http.MethodGet, "/LiveTv/ListingProviders/Lineups?type=xmltv", "").Code; got != http.StatusNotFound {
		t.Errorf("lineups without id status %d", got)
	}

	record := liveTvRequest(rt, http.MethodGet, "/LiveTv/ListingProviders/Default", "")
	var provider config.ListingsProviderInfo
	if err := json.Unmarshal(record.Body.Bytes(), &provider); err != nil || !provider.EnableAllTuners || len(provider.KidsCategories) != 5 {
		t.Fatalf("default provider %s %v", record.Body.String(), err)
	}
}

func TestLiveTvSetChannelMapping(t *testing.T) {
	admin := &fakeLiveTvAdmin{}
	rt := newLiveTvAdminRouter(admin)
	record := liveTvRequest(rt, http.MethodPost, "/LiveTv/ChannelMappings", `{"ProviderId":"p1","TunerChannelId":"m3u_b","ProviderChannelId":"gz"}`)
	if record.Code != http.StatusOK || strings.TrimSpace(record.Body.String()) != `{"Name":"2 B","ProviderChannelName":"Guide Z","ProviderChannelId":"gz","Id":"m3u_b"}` {
		t.Fatalf("POST ChannelMappings = %d %s", record.Code, record.Body.String())
	}
	if admin.mapping != [3]string{"p1", "m3u_b", "gz"} {
		t.Fatalf("mapping %v", admin.mapping)
	}
	for _, body := range []string{"", `{}`, `{"ProviderId":"p1","TunerChannelId":"m3u_b"}`} {
		if got := liveTvRequest(rt, http.MethodPost, "/LiveTv/ChannelMappings", body).Code; got != http.StatusBadRequest {
			t.Errorf("body %q status %d, want 400", body, got)
		}
	}
}

func TestLiveTvTunerHostGroups(t *testing.T) {
	rt := newLiveTvAdminRouter(&fakeLiveTvAdmin{})
	record := liveTvRequest(rt, http.MethodGet, "/deadfin/v1/livetv/tunerhosts/t1/groups", "")
	if record.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", record.Code, record.Body.String())
	}
	if got, want := strings.TrimSpace(record.Body.String()), `{"Groups":[{"Name":"News","Count":9},{"Name":"","Count":1}],"Loaded":true}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	if got := liveTvRequest(rt, http.MethodGet, "/deadfin/v1/livetv/tunerhosts/t2/groups", "").Code; got != http.StatusNotFound {
		t.Fatalf("unknown tuner status %d", got)
	}
	for _, route := range rt.Routes() {
		if route.OperationID == "NATIVE:GetTunerHostGroups" {
			if route.Policy == "" || route.Anonymous {
				t.Fatalf("route %+v is not elevated", route)
			}
			return
		}
	}
	t.Fatal("route not registered")
}
