// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"context"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// LiveStreamFiles opens readers of open live streams by their unique id.
// *media.Provider implements it.
type LiveStreamFiles interface {
	Reader(ctx context.Context, streamID string) (io.ReadCloser, error)
}

// liveStreamFile serves GET /LiveTv/LiveStreamFiles/{streamId}/stream.{container}
// (LiveTvController.GetLiveStreamFile, LiveTvController.cs:1145-1161). It is
// anonymous, as upstream: the id is random and ends with the stream. The
// body follows the stream until the stream or the client ends it.
func (h liveTvReadHandler) liveStreamFile(w http.ResponseWriter, r *http.Request) {
	container := chi.URLParam(r, "container")
	if h.reads.Files == nil || !validContainerToken(container) {
		if h.reads.Files != nil {
			badRequest(w)
			return
		}
		liveTvStub{}.notConfigured(w, r)
		return
	}
	reader, err := h.reads.Files.Reader(r.Context(), chi.URLParam(r, "streamId"))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	defer func() { _ = reader.Close() }()

	contentType := mime.TypeByExtension("." + container)
	if strings.EqualFold(container, "ts") || contentType == "" {
		contentType = "video/mp2t"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	buffer := make([]byte, 64<<10)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				return
			}
			_ = controller.Flush()
		}
		if readErr != nil {
			// EOF ends a stream whose upstream finished; any other error
			// is a closed stream or client.
			return
		}
	}
}

// validContainerToken is EncodingHelper.ContainerValidationRegex
// (^[a-zA-Z0-9\-\._,|]{0,40}$).
func validContainerToken(value string) bool {
	if len(value) > 40 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._,|", r)) {
			return false
		}
	}
	return true
}
