// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/identify"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/store"
)

// RemoteImagesService is the handler-facing interface for remote-image
// listing and download. It uses the identify package's RemoteImagesResult
// and ImageProviderInfo types directly, so identify.RemoteImagesAdapter
// satisfies this interface without a composition-root adapter.
type RemoteImagesService interface {
	RemoteImages(ctx context.Context, itemID dto.UUID, imageType *item.ImageType, providerName string, includeAllLanguages bool, startIndex int, limit *int, language string) (identify.RemoteImagesResult, error)
	RemoteImageProviders(ctx context.Context, itemID dto.UUID) ([]identify.ImageProviderInfo, error)
	DownloadImage(ctx context.Context, itemID dto.UUID, imageType item.ImageType, imageURL string) error
}

// RegisterRemoteImages registers the RemoteImages routes (WP-P8-05):
// GET /Items/{itemId}/RemoteImages, GET /Items/{itemId}/RemoteImages/Providers,
// POST /Items/{itemId}/RemoteImages/Download.
func RegisterRemoteImages(rt *router.Router, reader ItemReader, service RemoteImagesService) {
	h := remoteImagesHandler{reader: reader, service: service}

	rt.Handle(http.MethodGet, "/Items/{itemId}/RemoteImages", router.Route{
		OperationID: "GetRemoteImages",
		Policy:      string(auth.PolicyDefaultAuthorization),
	}, h.list)
	rt.Handle(http.MethodGet, "/Items/{itemId}/RemoteImages/Providers", router.Route{
		OperationID: "GetRemoteImageProviders",
		Policy:      string(auth.PolicyDefaultAuthorization),
	}, h.providers)
	rt.Handle(http.MethodPost, "/Items/{itemId}/RemoteImages/Download", router.Route{
		OperationID: "DownloadRemoteImage",
		Policy:      string(auth.PolicyRequiresElevation),
	}, h.download)
}

type remoteImagesHandler struct {
	reader  ItemReader
	service RemoteImagesService
}

// remoteImagesQuery binds the GET /Items/{itemId}/RemoteImages query
// (RemoteImageController.cs:74-78).
type remoteImagesQuery struct {
	Type                string `query:"type"`
	StartIndex          int    `query:"startIndex"`
	Limit               *int   `query:"limit"`
	ProviderName        string `query:"providerName"`
	IncludeAllLanguages bool   `query:"includeAllLanguages"`
}

func (h remoteImagesHandler) list(w http.ResponseWriter, r *http.Request) {
	id, err := dto.ParseUUID(chi.URLParam(r, "itemId"))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	var query remoteImagesQuery
	if err := bind.QueryRequest(r, &query); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	// 404 before work.
	if h.reader == nil || h.service == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	result, err := h.reader.Query(r.Context(), store.ItemQuery{ItemIDs: []dto.UUID{id}, Recursive: true})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if len(result.Items) == 0 {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	var imageType *item.ImageType
	if query.Type != "" {
		parsed, ok := parseImageType(query.Type)
		if !ok {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
		imageType = &parsed
	}

	language := ""
	images, err := h.service.RemoteImages(r.Context(), id, imageType, query.ProviderName, query.IncludeAllLanguages, query.StartIndex, query.Limit, language)
	if err != nil {
		switch {
		case errors.Is(err, identify.ErrItemNotFound):
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}

	out := dto.RemoteImageResult{
		TotalRecordCount: images.TotalRecordCount,
		Providers:        images.Providers,
		Images:           make([]dto.RemoteImageInfo, 0, len(images.Images)),
	}
	for _, img := range images.Images {
		out.Images = append(out.Images, remoteImageInfoToDTO(img))
	}
	_ = render.JSON(w, r, http.StatusOK, out)
}

func (h remoteImagesHandler) providers(w http.ResponseWriter, r *http.Request) {
	id, err := dto.ParseUUID(chi.URLParam(r, "itemId"))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	// 404 before work.
	if h.reader == nil || h.service == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	result, err := h.reader.Query(r.Context(), store.ItemQuery{ItemIDs: []dto.UUID{id}, Recursive: true})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if len(result.Items) == 0 {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	providerInfos, err := h.service.RemoteImageProviders(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, identify.ErrItemNotFound):
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}
	out := make([]dto.ImageProviderInfo, 0, len(providerInfos))
	for _, p := range providerInfos {
		supported := make([]string, 0, len(p.SupportedImages))
		for _, t := range p.SupportedImages {
			supported = append(supported, t.String())
		}
		out = append(out, dto.ImageProviderInfo{
			Name:            p.Name,
			SupportedImages: supported,
		})
	}
	_ = render.JSON(w, r, http.StatusOK, out)
}

// downloadQuery binds the POST /Items/{itemId}/RemoteImages/Download query.
// type is required, imageUrl is optional (RemoteImageController.cs:130-160).
type downloadQuery struct {
	Type     string `query:"type"`
	ImageURL string `query:"imageUrl"`
}

func (h remoteImagesHandler) download(w http.ResponseWriter, r *http.Request) {
	id, err := dto.ParseUUID(chi.URLParam(r, "itemId"))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	var query downloadQuery
	if err := bind.QueryRequest(r, &query); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	if query.Type == "" {
		http.Error(w, "Type is required", http.StatusBadRequest)
		return
	}
	imageType, ok := parseImageType(query.Type)
	if !ok {
		http.Error(w, "Type is not a valid ImageType", http.StatusBadRequest)
		return
	}
	// 404 before work.
	if h.reader == nil || h.service == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	result, err := h.reader.Query(r.Context(), store.ItemQuery{ItemIDs: []dto.UUID{id}, Recursive: true})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if len(result.Items) == 0 {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	if err := h.service.DownloadImage(r.Context(), id, imageType, query.ImageURL); err != nil {
		switch {
		case errors.Is(err, identify.ErrItemNotFound):
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		case errors.Is(err, identify.ErrInvalidImageURL):
			http.Error(w, errorReason(err, identify.ErrInvalidImageURL), http.StatusBadRequest)
		case errors.Is(err, identify.ErrImageRefused):
			http.Error(w, "ImageUrl was refused: "+errorReason(err, identify.ErrImageRefused), http.StatusBadRequest)
		default:
			http.Error(w, "Image download failed: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// errorReason returns the text that follows sentinel in err's message, so a
// response body names the reason without the wrapping call chain.
func errorReason(err, sentinel error) string {
	msg := err.Error()
	prefix := sentinel.Error() + ": "
	if i := strings.LastIndex(msg, prefix); i >= 0 {
		return msg[i+len(prefix):]
	}
	return msg
}

func remoteImageInfoToDTO(img providers.RemoteImageInfo) dto.RemoteImageInfo {
	out := dto.RemoteImageInfo{
		Type:       img.Type.String(),
		RatingType: img.RatingType,
	}
	if img.ProviderName != "" {
		out.ProviderName = &img.ProviderName
	}
	if img.URL != "" {
		out.URL = &img.URL
	}
	if img.ThumbnailURL != "" {
		out.ThumbnailURL = &img.ThumbnailURL
	}
	if img.Height > 0 {
		out.Height = &img.Height
	}
	if img.Width > 0 {
		out.Width = &img.Width
	}
	if img.CommunityRating != 0 {
		out.CommunityRating = &img.CommunityRating
	}
	if img.VoteCount > 0 {
		out.VoteCount = &img.VoteCount
	}
	if img.Language != "" {
		out.Language = &img.Language
	}
	return out
}
