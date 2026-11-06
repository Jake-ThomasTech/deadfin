// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"context"
	"crypto/sha1" // #nosec G505 -- upstream stores the Schedules Direct password as its SHA-1 hex.
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/livetv"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
	"github.com/go-chi/chi/v5"
)

// LiveTvAdmin is the Live TV administration seam. *livetv.Service
// implements it.
type LiveTvAdmin interface {
	GuideInfo() (start, end time.Time)
	TunerHostTypes() []tuner.TypeInfo
	SaveTunerHost(ctx context.Context, info config.TunerHostInfo) (config.TunerHostInfo, error)
	DeleteTunerHost(ctx context.Context, id string) error
	SaveListingProvider(ctx context.Context, info config.ListingsProviderInfo) (config.ListingsProviderInfo, error)
	DeleteListingProvider(ctx context.Context, id string) error
	Lineups(ctx context.Context, providerType, providerID string) ([]livetv.NameID, error)
	ChannelMappingOptions(ctx context.Context, providerID string) (livetv.ChannelMappingOptions, error)
	SetChannelMapping(ctx context.Context, providerID, tunerChannelID, providerChannelID string) (livetv.TunerChannelMapping, error)
	TunerHostGroups(id string) (livetv.TunerHostGroups, error)
}

func registerLiveTvAdmin(rt *router.Router, admin LiveTvAdmin, access, elevated string) {
	h := liveTvAdminHandler{admin: admin}
	rt.Handle(http.MethodGet, "/LiveTv/GuideInfo", router.Route{OperationID: "GetGuideInfo", Policy: access}, h.guideInfo)
	rt.Handle(http.MethodGet, "/LiveTv/TunerHosts/Types", router.Route{OperationID: "GetTunerHostTypes", Policy: access}, h.tunerHostTypes)
	rt.Handle(http.MethodPost, "/LiveTv/TunerHosts", router.Route{OperationID: "AddTunerHost", Policy: elevated}, h.addTunerHost)
	rt.Handle(http.MethodDelete, "/LiveTv/TunerHosts", router.Route{OperationID: "DeleteTunerHost", Policy: elevated}, h.deleteTunerHost)
	rt.Handle(http.MethodPost, "/LiveTv/ListingProviders", router.Route{OperationID: "AddListingProvider", Policy: elevated}, h.addListingProvider)
	rt.Handle(http.MethodDelete, "/LiveTv/ListingProviders", router.Route{OperationID: "DeleteListingProvider", Policy: elevated}, h.deleteListingProvider)
	rt.Handle(http.MethodGet, "/LiveTv/ListingProviders/Lineups", router.Route{OperationID: "GetLineups", Policy: access}, h.lineups)
	rt.Handle(http.MethodGet, "/LiveTv/ChannelMappingOptions", router.Route{OperationID: "GetChannelMappingOptions", Policy: elevated}, h.channelMappingOptions)
	rt.Handle(http.MethodPost, "/LiveTv/ChannelMappings", router.Route{OperationID: "SetChannelMapping", Policy: elevated}, h.setChannelMapping)
	rt.Handle(http.MethodGet, "/deadfin/v1/livetv/tunerhosts/{id}/groups", router.Route{OperationID: "NATIVE:GetTunerHostGroups", Policy: elevated}, h.tunerHostGroups)
}

type liveTvAdminHandler struct {
	admin LiveTvAdmin
}

// guideInfoDTO is MediaBrowser.Model.LiveTv.GuideInfo.
type guideInfoDTO struct {
	StartDate dto.Time `json:"StartDate"`
	EndDate   dto.Time `json:"EndDate"`
}

// nameIDPair is MediaBrowser.Model.Dto.NameIdPair.
type nameIDPair struct {
	Name string `json:"Name,omitzero"`
	ID   string `json:"Id,omitzero"`
}

// tunerChannelMappingDTO is MediaBrowser.Model.LiveTv.TunerChannelMapping.
type tunerChannelMappingDTO struct {
	Name                string `json:"Name,omitzero"`
	ProviderChannelName string `json:"ProviderChannelName,omitzero"`
	ProviderChannelID   string `json:"ProviderChannelId,omitzero"`
	ID                  string `json:"Id,omitzero"`
}

// channelMappingOptionsDTO is Jellyfin.Api.Models.LiveTvDtos.ChannelMappingOptionsDto.
type channelMappingOptionsDTO struct {
	TunerChannels    []tunerChannelMappingDTO `json:"TunerChannels"`
	ProviderChannels []nameIDPair             `json:"ProviderChannels"`
	Mappings         []config.NameValuePair   `json:"Mappings"`
	ProviderName     string                   `json:"ProviderName,omitzero"`
}

// tunerHostGroupsDTO lists the group-titles of a tuner's cached playlist
// with their channel counts (D-91).
type tunerHostGroupsDTO struct {
	Groups []tunerHostGroupDTO `json:"Groups"`
	Loaded bool                `json:"Loaded"`
}

type tunerHostGroupDTO struct {
	Name  string `json:"Name"`
	Count int    `json:"Count"`
}

func (h liveTvAdminHandler) tunerHostGroups(w http.ResponseWriter, r *http.Request) {
	result, err := h.admin.TunerHostGroups(chi.URLParam(r, "id"))
	if err != nil {
		writeLiveTvError(w, err)
		return
	}
	out := tunerHostGroupsDTO{Groups: make([]tunerHostGroupDTO, 0, len(result.Groups)), Loaded: result.Loaded}
	for _, group := range result.Groups {
		out.Groups = append(out.Groups, tunerHostGroupDTO{Name: group.Name, Count: group.Count})
	}
	_ = render.JSON(w, r, http.StatusOK, out)
}

func (h liveTvAdminHandler) guideInfo(w http.ResponseWriter, r *http.Request) {
	start, end := h.admin.GuideInfo()
	_ = render.JSON(w, r, http.StatusOK, guideInfoDTO{StartDate: dto.NewTime(start), EndDate: dto.NewTime(end)})
}

func (h liveTvAdminHandler) tunerHostTypes(w http.ResponseWriter, r *http.Request) {
	types := h.admin.TunerHostTypes()
	out := make([]nameIDPair, 0, len(types))
	for _, value := range types {
		out = append(out, nameIDPair{Name: value.Name, ID: value.ID})
	}
	_ = render.JSON(w, r, http.StatusOK, out)
}

// addTunerHost ports AddTunerHost (LiveTvController.cs:950-954).
func (h liveTvAdminHandler) addTunerHost(w http.ResponseWriter, r *http.Request) {
	info := config.DefaultTunerHostInfo()
	if err := bind.JSONBody(r, &info); err != nil {
		writeBindError(w, err)
		return
	}
	saved, err := h.admin.SaveTunerHost(r.Context(), info)
	if err != nil {
		writeLiveTvError(w, err)
		return
	}
	_ = render.JSON(w, r, http.StatusOK, saved)
}

func (h liveTvAdminHandler) deleteTunerHost(w http.ResponseWriter, r *http.Request) {
	if err := h.admin.DeleteTunerHost(r.Context(), bind.QueryFirst(r, "id")); err != nil {
		writeLiveTvError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// addListingProvider ports AddListingProvider (LiveTvController.cs:993-1009):
// a pw query value replaces Password with its lower-case SHA-1 hex.
func (h liveTvAdminHandler) addListingProvider(w http.ResponseWriter, r *http.Request) {
	info := config.DefaultListingsProviderInfo()
	if err := bind.JSONBody(r, &info); err != nil {
		writeBindError(w, err)
		return
	}
	if pw := bind.QueryFirst(r, "pw"); pw != "" {
		sum := sha1.Sum([]byte(pw)) // #nosec G401 -- upstream wire format.
		info.Password = hex.EncodeToString(sum[:])
	}
	saved, err := h.admin.SaveListingProvider(r.Context(), info)
	if err != nil {
		writeLiveTvError(w, err)
		return
	}
	_ = render.JSON(w, r, http.StatusOK, saved)
}

func (h liveTvAdminHandler) deleteListingProvider(w http.ResponseWriter, r *http.Request) {
	if err := h.admin.DeleteListingProvider(r.Context(), bind.QueryFirst(r, "id")); err != nil {
		writeLiveTvError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h liveTvAdminHandler) lineups(w http.ResponseWriter, r *http.Request) {
	lineups, err := h.admin.Lineups(r.Context(), bind.QueryFirst(r, "type"), bind.QueryFirst(r, "id"))
	if err != nil {
		writeLiveTvError(w, err)
		return
	}
	out := make([]nameIDPair, 0, len(lineups))
	for _, value := range lineups {
		out = append(out, nameIDPair{Name: value.Name, ID: value.ID})
	}
	_ = render.JSON(w, r, http.StatusOK, out)
}

func (h liveTvAdminHandler) channelMappingOptions(w http.ResponseWriter, r *http.Request) {
	options, err := h.admin.ChannelMappingOptions(r.Context(), bind.QueryFirst(r, "providerId"))
	if err != nil {
		writeLiveTvError(w, err)
		return
	}
	out := channelMappingOptionsDTO{
		TunerChannels:    make([]tunerChannelMappingDTO, 0, len(options.TunerChannels)),
		ProviderChannels: make([]nameIDPair, 0, len(options.ProviderChannels)),
		Mappings:         options.Mappings,
		ProviderName:     options.ProviderName,
	}
	for _, value := range options.TunerChannels {
		out.TunerChannels = append(out.TunerChannels, tunerChannelMappingDTO(value))
	}
	for _, value := range options.ProviderChannels {
		out.ProviderChannels = append(out.ProviderChannels, nameIDPair{Name: value.Name, ID: value.ID})
	}
	if out.Mappings == nil {
		out.Mappings = []config.NameValuePair{}
	}
	_ = render.JSON(w, r, http.StatusOK, out)
}

// setChannelMappingDTO is Jellyfin.Api.Models.LiveTvDtos.SetChannelMappingDto;
// every member is [Required].
type setChannelMappingDTO struct {
	ProviderID        *string `json:"ProviderId"`
	TunerChannelID    *string `json:"TunerChannelId"`
	ProviderChannelID *string `json:"ProviderChannelId"`
}

func (h liveTvAdminHandler) setChannelMapping(w http.ResponseWriter, r *http.Request) {
	var body setChannelMappingDTO
	present, err := bind.OptionalJSONBody(r, &body)
	if err != nil {
		writeBindError(w, err)
		return
	}
	if !present || body.ProviderID == nil || body.TunerChannelID == nil || body.ProviderChannelID == nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	mapping, err := h.admin.SetChannelMapping(r.Context(), *body.ProviderID, *body.TunerChannelID, *body.ProviderChannelID)
	if err != nil {
		writeLiveTvError(w, err)
		return
	}
	_ = render.JSON(w, r, http.StatusOK, tunerChannelMappingDTO(mapping))
}

func writeBindError(w http.ResponseWriter, err error) {
	if errors.Is(err, bind.ErrUnsupportedMediaType) {
		http.Error(w, http.StatusText(http.StatusUnsupportedMediaType), http.StatusUnsupportedMediaType)
		return
	}
	http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
}

// writeLiveTvError maps service errors as upstream ExceptionMiddleware maps
// the matching exceptions: ResourceNotFoundException and
// FileNotFoundException give 404, ArgumentException gives 400, the rest 500
// (ExceptionMiddleware.cs:124-135).
func writeLiveTvError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, livetv.ErrUnknownType), errors.Is(err, livetv.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
	case errors.Is(err, tuner.ErrNoTunerURL):
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
	case errors.Is(err, context.Canceled):
		// The client is gone; nothing reads the response.
	default:
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}
