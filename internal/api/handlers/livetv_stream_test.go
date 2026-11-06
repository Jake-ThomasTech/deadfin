// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/router"
)

type fakeStreamFiles struct{ data map[string]string }

func (f fakeStreamFiles) Reader(_ context.Context, id string) (io.ReadCloser, error) {
	data, ok := f.data[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

func TestLiveStreamFile(t *testing.T) {
	rt := router.New()
	handlers.RegisterLiveTv(rt, &fakeLiveTvAdmin{}, handlers.LiveTvReads{Files: fakeStreamFiles{data: map[string]string{"abc": "tsdata"}}})

	record := liveTvRequest(rt, http.MethodGet, "/LiveTv/LiveStreamFiles/abc/stream.ts", "")
	if record.Code != http.StatusOK || record.Body.String() != "tsdata" || record.Header().Get("Content-Type") != "video/mp2t" {
		t.Fatalf("stream: %d %q %q", record.Code, record.Body.String(), record.Header().Get("Content-Type"))
	}
	if record := liveTvRequest(rt, http.MethodGet, "/LiveTv/LiveStreamFiles/nope/stream.ts", ""); record.Code != http.StatusNotFound {
		t.Fatalf("unknown stream: %d", record.Code)
	}
	if record := liveTvRequest(rt, http.MethodGet, "/LiveTv/LiveStreamFiles/abc/stream.t%20s", ""); record.Code != http.StatusBadRequest {
		t.Fatalf("bad container: %d", record.Code)
	}

	// Without a broker the route keeps its no-tuner 404.
	stub := router.New()
	handlers.RegisterLiveTv(stub, &fakeLiveTvAdmin{}, handlers.LiveTvReads{})
	if record := liveTvRequest(stub, http.MethodGet, "/LiveTv/LiveStreamFiles/abc/stream.ts", ""); record.Code != http.StatusNotFound {
		t.Fatalf("stub: %d", record.Code)
	}
}
