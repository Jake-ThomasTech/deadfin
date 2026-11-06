// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"errors"
	"net/http"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/sessioncontrol"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/user"
	"github.com/go-chi/chi/v5"
)

// SessionOwners sends a request for another client's session to the server
// replica that owns the session (#663). ServeOnOwner reports whether it
// answered the request.
type SessionOwners interface {
	ServeOnOwner(w http.ResponseWriter, request *http.Request, sessionID string) bool
}

// RegisterSessionControl registers jellyfin-web's remote-control controller
// surface. owners is nil without server replicas.
func RegisterSessionControl(rt *router.Router, service *sessioncontrol.Service, owners SessionOwners) {
	handler := sessionControlHandler{service: service, owners: owners}
	policy := string(auth.PolicyDefaultAuthorization)
	rt.Handle(http.MethodPost, "/Sessions/{sessionId}/Playing", router.Route{OperationID: "Play", Policy: policy}, handler.onOwner(handler.play))
	rt.Handle(http.MethodPost, "/Sessions/{sessionId}/Playing/{command}", router.Route{OperationID: "SendPlaystateCommand", Policy: policy}, handler.onOwner(handler.playstate))
	rt.Handle(http.MethodPost, "/Sessions/{sessionId}/Command/{command}", router.Route{OperationID: "SendGeneralCommand", Policy: policy}, handler.onOwner(handler.general))
	rt.Handle(http.MethodPost, "/Sessions/{sessionId}/Message", router.Route{OperationID: "SendMessageCommand", Policy: policy}, handler.onOwner(handler.message))
	rt.Handle(http.MethodPost, "/Sessions/{sessionId}/Viewing", router.Route{OperationID: "DisplayContent", Policy: policy}, handler.onOwner(handler.view))
}

type sessionControlHandler struct {
	service *sessioncontrol.Service
	owners  SessionOwners
}

// onOwner serves a command on the replica that holds the target session.
func (handler sessionControlHandler) onOwner(next http.HandlerFunc) http.HandlerFunc {
	if handler.owners == nil {
		return next
	}
	return func(w http.ResponseWriter, request *http.Request) {
		if handler.owners.ServeOnOwner(w, request, chi.URLParam(request, "sessionId")) {
			return
		}
		next(w, request)
	}
}

type playCommandQuery struct {
	PlayCommand         string     `query:"playCommand"`
	ItemIDs             []dto.UUID `query:"itemIds"`
	StartPositionTicks  *int64     `query:"startPositionTicks"`
	MediaSourceID       *string    `query:"mediaSourceId"`
	AudioStreamIndex    *int       `query:"audioStreamIndex"`
	SubtitleStreamIndex *int       `query:"subtitleStreamIndex"`
	StartIndex          *int       `query:"startIndex"`
}

func (handler sessionControlHandler) play(w http.ResponseWriter, request *http.Request) {
	var query playCommandQuery
	if handler.service == nil || bind.QueryRequest(request, &query) != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	err := handler.service.Play(request.Context(), actor(request), chi.URLParam(request, "sessionId"), dto.PlayRequest{
		ItemIDs: query.ItemIDs, StartPositionTicks: query.StartPositionTicks, PlayCommand: query.PlayCommand,
		MediaSourceID: query.MediaSourceID, AudioStreamIndex: query.AudioStreamIndex,
		SubtitleStreamIndex: query.SubtitleStreamIndex, StartIndex: query.StartIndex,
	})
	writeSessionControlResult(w, err)
}

type playstateCommandQuery struct {
	SeekPositionTicks *int64  `query:"seekPositionTicks"`
	ControllingUserID *string `query:"controllingUserId"`
}

func (handler sessionControlHandler) playstate(w http.ResponseWriter, request *http.Request) {
	var query playstateCommandQuery
	if handler.service == nil || bind.QueryRequest(request, &query) != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	err := handler.service.Playstate(request.Context(), actor(request), chi.URLParam(request, "sessionId"), dto.PlaystateRequest{
		Command: chi.URLParam(request, "command"), SeekPositionTicks: query.SeekPositionTicks,
		ControllingUserID: query.ControllingUserID,
	})
	writeSessionControlResult(w, err)
}

func (handler sessionControlHandler) general(w http.ResponseWriter, request *http.Request) {
	if handler.service == nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	err := handler.service.General(request.Context(), actor(request), chi.URLParam(request, "sessionId"), dto.GeneralCommand{
		Name: chi.URLParam(request, "command"),
	})
	writeSessionControlResult(w, err)
}

func (handler sessionControlHandler) message(w http.ResponseWriter, request *http.Request) {
	var command dto.MessageCommand
	if handler.service == nil || bind.JSONBody(request, &command) != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	writeSessionControlResult(w, handler.service.Message(request.Context(), actor(request), chi.URLParam(request, "sessionId"), command))
}

type viewCommandQuery struct {
	ItemType string `query:"itemType"`
	ItemID   string `query:"itemId"`
	ItemName string `query:"itemName"`
}

func (handler sessionControlHandler) view(w http.ResponseWriter, request *http.Request) {
	var query viewCommandQuery
	if handler.service == nil || bind.QueryRequest(request, &query) != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	writeSessionControlResult(w, handler.service.View(
		request.Context(), actor(request), chi.URLParam(request, "sessionId"),
		query.ItemType, query.ItemID, query.ItemName,
	))
}

func writeSessionControlResult(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, sessioncontrol.ErrBadRequest):
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
	case errors.Is(err, sessioncontrol.ErrForbidden), errors.Is(err, user.ErrForbidden), errors.Is(err, user.ErrUnauthorized):
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
	case errors.Is(err, session.ErrNotFound), errors.Is(err, store.ErrNotFound):
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
	default:
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}
