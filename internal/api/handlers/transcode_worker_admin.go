// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/transcodeworker"
)

// TranscodeWorkerHub is the live side of the transcode worker admin API.
// *transcodeworker.Hub implements it.
type TranscodeWorkerHub interface {
	Workers() []transcodeworker.WorkerInfo
	Disconnect(workerID string)
	SetPools(workerID string, pools []string)
	SetInputMode(workerID, mode string)
	BroadcastPins(pins []string)
	Policy() transcodeworker.Policy
	SetPolicy(policy transcodeworker.Policy)
}

const transcodeWorkerAdminPath = "/deadfin/v1/transcode-workers"

// RegisterTranscodeWorkerAdmin registers the native admin API for transcode
// workers (#615): join tokens, enrolled workers, and the extra server key
// pins, the placement policy (#616), the scaling token (#617), and the
// plain-HTTP opt-in (#664). Every route requires an administrator. The
// route-only composition (the route ledger) passes nil dependencies; the
// routes then answer 404.
func RegisterTranscodeWorkerAdmin(rt *router.Router, workers *store.TranscodeWorkerStore, hub TranscodeWorkerHub, plainHTTP func() dto.TranscodeWorkerPlainHTTP, clk clock.Clock) {
	h := transcodeWorkerAdmin{workers: workers, hub: hub, plainHTTP: plainHTTP, clock: clk}
	handle := func(method, path, operation string, handler http.HandlerFunc) {
		if workers == nil || hub == nil || plainHTTP == nil {
			handler = http.NotFound
		}
		rt.Handle(method, transcodeWorkerAdminPath+path, router.Route{OperationID: "NATIVE:" + operation, Policy: string(auth.PolicyRequiresElevation)}, handler)
	}
	handle(http.MethodGet, "", "GetTranscodeWorkers", h.list)
	handle(http.MethodGet, "/join-tokens", "GetTranscodeWorkerJoinTokens", h.listTokens)
	handle(http.MethodPost, "/join-tokens", "CreateTranscodeWorkerJoinToken", h.createToken)
	handle(http.MethodDelete, "/join-tokens/{tokenId}", "RevokeTranscodeWorkerJoinToken", h.revokeToken)
	handle(http.MethodGet, "/pins", "GetTranscodeWorkerPins", h.pins)
	handle(http.MethodPut, "/pins", "SetTranscodeWorkerPins", h.setPins)
	handle(http.MethodGet, "/policy", "GetTranscodeWorkerPolicy", h.policy)
	handle(http.MethodPut, "/policy", "SetTranscodeWorkerPolicy", h.setPolicy)
	handle(http.MethodGet, "/plain-http", "GetTranscodeWorkerPlainHttp", h.plainHTTPSetting)
	handle(http.MethodPost, "/scaling-token", "CreateTranscodeWorkerScalingToken", h.createScalingToken)
	handle(http.MethodDelete, "/scaling-token", "DeleteTranscodeWorkerScalingToken", h.deleteScalingToken)
	handle(http.MethodPost, "/{workerId}", "UpdateTranscodeWorker", h.update)
	handle(http.MethodDelete, "/{workerId}", "RevokeTranscodeWorker", h.revoke)
}

type transcodeWorkerAdmin struct {
	workers   *store.TranscodeWorkerStore
	hub       TranscodeWorkerHub
	plainHTTP func() dto.TranscodeWorkerPlainHTTP
	clock     clock.Clock
}

// plainHTTPSetting reports the plain-HTTP opt-in (#664). The dashboard
// changes it through the encoding configuration
// (AllowPlainHttpTranscodeWorkers).
func (h transcodeWorkerAdmin) plainHTTPSetting(w http.ResponseWriter, r *http.Request) {
	_ = render.JSON(w, r, http.StatusOK, h.plainHTTP())
}

func (h transcodeWorkerAdmin) list(w http.ResponseWriter, r *http.Request) {
	stored, err := h.workers.Workers(r.Context())
	if err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "list transcode workers")
		return
	}
	live := make(map[string]transcodeworker.WorkerInfo)
	for _, info := range h.hub.Workers() {
		live[info.ID] = info
	}
	result := make([]dto.TranscodeWorker, 0, len(stored))
	for _, worker := range stored {
		value := dto.TranscodeWorker{
			ID: worker.ID, Name: worker.Name, JoinTokenID: worker.JoinTokenID, InputMode: worker.InputMode,
			Pools: append([]string{}, worker.Pools...), Created: dto.NewTime(worker.Created),
			LastSeen: dto.NewTime(worker.LastSeen), Revoked: optionalTime(worker.Revoked),
		}
		if info, ok := live[worker.ID]; ok {
			setLiveWorker(&value, info)
			delete(live, worker.ID)
		}
		result = append(result, value)
	}
	// The rest are static workers from the server environment.
	static := make([]dto.TranscodeWorker, 0, len(live))
	for _, info := range live {
		value := dto.TranscodeWorker{ID: info.ID, Name: info.Name, Static: true, InputMode: info.InputMode, Pools: append([]string{}, info.Pools...)}
		setLiveWorker(&value, info)
		static = append(static, value)
	}
	sort.Slice(static, func(i, j int) bool { return static[i].ID < static[j].ID })
	_ = render.JSON(w, r, http.StatusOK, append(result, static...))
}

func (h transcodeWorkerAdmin) listTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := h.workers.JoinTokens(r.Context())
	if err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "list join tokens")
		return
	}
	now := h.clock.Now()
	result := make([]dto.TranscodeWorkerJoinToken, 0, len(tokens))
	for _, token := range tokens {
		result = append(result, joinTokenDTO(token, now))
	}
	_ = render.JSON(w, r, http.StatusOK, result)
}

func (h transcodeWorkerAdmin) createToken(w http.ResponseWriter, r *http.Request) {
	var request dto.CreateTranscodeWorkerJoinToken
	if err := bind.JSONBody(r, &request); err != nil {
		renderProblem(w, r, http.StatusBadRequest, "invalid join token request")
		return
	}
	request.Fingerprint = strings.TrimSpace(request.Fingerprint)
	if request.ExpiresInSeconds < 0 || request.Uses < 0 {
		renderProblem(w, r, http.StatusBadRequest, "ExpiresInSeconds and Uses must not be negative")
		return
	}
	if request.Fingerprint != "" && !transcodeworker.ValidFingerprint(request.Fingerprint) {
		renderProblem(w, r, http.StatusBadRequest, "Fingerprint is not a base64url SHA-256 of a certificate key")
		return
	}
	value, err := transcodeworker.NewToken(request.Fingerprint)
	if err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "create join token")
		return
	}
	token, err := transcodeworker.ParseToken(value)
	if err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "create join token")
		return
	}
	now := h.clock.Now().UTC()
	stored := store.TranscodeWorkerJoinToken{
		ID: token.ID, Name: strings.TrimSpace(request.Name), DerivedKey: token.Key(),
		Fingerprint: token.Fingerprint, Created: now,
	}
	if request.ExpiresInSeconds > 0 {
		stored.Expires = now.Add(time.Duration(request.ExpiresInSeconds) * time.Second)
	}
	if request.Uses > 0 {
		uses := request.Uses
		stored.UsesLeft = &uses
	}
	if err := h.workers.CreateJoinToken(r.Context(), stored); err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "store join token")
		return
	}
	_ = render.JSON(w, r, http.StatusOK, dto.CreatedTranscodeWorkerJoinToken{TranscodeWorkerJoinToken: joinTokenDTO(stored, now), Token: value})
}

func (h transcodeWorkerAdmin) revokeToken(w http.ResponseWriter, r *http.Request) {
	err := h.workers.RevokeJoinToken(r.Context(), chi.URLParam(r, "tokenId"), h.clock.Now())
	h.writeChange(w, r, err, "join token")
}

func (h transcodeWorkerAdmin) pins(w http.ResponseWriter, r *http.Request) {
	pins, err := h.workers.Pins(r.Context())
	if err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "read pins")
		return
	}
	_ = render.JSON(w, r, http.StatusOK, dto.TranscodeWorkerPins{Fingerprints: append([]string{}, pins...)})
}

func (h transcodeWorkerAdmin) setPins(w http.ResponseWriter, r *http.Request) {
	var request dto.TranscodeWorkerPins
	if err := bind.JSONBody(r, &request); err != nil {
		renderProblem(w, r, http.StatusBadRequest, "invalid pins request")
		return
	}
	for _, pin := range request.Fingerprints {
		if !transcodeworker.ValidFingerprint(pin) {
			renderProblem(w, r, http.StatusBadRequest, "a fingerprint is not a base64url SHA-256 of a certificate key")
			return
		}
	}
	if err := h.workers.SetPins(r.Context(), request.Fingerprints, h.clock.Now()); err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "store pins")
		return
	}
	pins, err := h.workers.Pins(r.Context())
	if err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "read pins")
		return
	}
	h.hub.BroadcastPins(pins)
	w.WriteHeader(http.StatusNoContent)
}

func (h transcodeWorkerAdmin) update(w http.ResponseWriter, r *http.Request) {
	var request dto.TranscodeWorkerUpdate
	if err := bind.JSONBody(r, &request); err != nil {
		renderProblem(w, r, http.StatusBadRequest, "invalid worker update")
		return
	}
	if request.InputMode != nil && !store.ValidTranscodeWorkerInputMode(*request.InputMode) {
		renderProblem(w, r, http.StatusBadRequest, "InputMode must be shared, stream or auto")
		return
	}
	if request.Name != nil && strings.TrimSpace(*request.Name) == "" {
		renderProblem(w, r, http.StatusBadRequest, "Name must not be empty")
		return
	}
	id := chi.URLParam(r, "workerId")
	update := store.TranscodeWorkerUpdate{Name: request.Name, InputMode: request.InputMode, Pools: request.Pools}
	if err := h.workers.UpdateWorker(r.Context(), id, update); err != nil {
		h.writeChange(w, r, err, "worker")
		return
	}
	if request.Pools != nil {
		h.hub.SetPools(id, store.NormalizeTranscodeWorkerPools(*request.Pools))
	}
	if request.InputMode != nil {
		h.hub.SetInputMode(id, *request.InputMode)
	}
	w.WriteHeader(http.StatusNoContent)
}

// revoke revokes the worker identity and closes its connection. Its running
// jobs are lost and restart on another worker or locally.
func (h transcodeWorkerAdmin) revoke(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "workerId")
	if err := h.workers.RevokeWorker(r.Context(), id, h.clock.Now()); err != nil {
		h.writeChange(w, r, err, "worker")
		return
	}
	h.hub.Disconnect(id)
	w.WriteHeader(http.StatusNoContent)
}

func (h transcodeWorkerAdmin) writeChange(w http.ResponseWriter, r *http.Request, err error, subject string) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		renderProblem(w, r, http.StatusNotFound, "unknown "+subject)
	default:
		renderProblem(w, r, http.StatusInternalServerError, "update "+subject)
	}
}

func setLiveWorker(value *dto.TranscodeWorker, info transcodeworker.WorkerInfo) {
	value.Connected = true
	value.LastSeen = dto.NewTime(info.LastSeen)
	value.ActiveJobs = info.ActiveJobs
	value.Capabilities = &dto.TranscodeWorkerCapabilities{
		HWAccels: append([]string{}, info.Capabilities.HWAccels...), Encoders: append([]string{}, info.Capabilities.Encoders...),
		FFmpegVersion: info.Capabilities.FFmpegVersion, MaxJobs: info.Capabilities.MaxJobs,
	}
}

func joinTokenDTO(token store.TranscodeWorkerJoinToken, now time.Time) dto.TranscodeWorkerJoinToken {
	return dto.TranscodeWorkerJoinToken{
		ID: token.ID, Name: token.Name, Fingerprint: token.Fingerprint, Expires: optionalTime(token.Expires),
		UsesLeft: token.UsesLeft, Created: dto.NewTime(token.Created), Revoked: optionalTime(token.Revoked),
		Usable: token.Usable(now),
	}
}

func optionalTime(value time.Time) *dto.Time {
	if value.IsZero() {
		return nil
	}
	t := dto.NewTime(value)
	return &t
}

// createScalingToken creates or rotates the dashboard scaling token and
// returns it once. It reads only /deadfin/v1/metrics and
// /deadfin/v1/workers/scaling.
func (h transcodeWorkerAdmin) createScalingToken(w http.ResponseWriter, r *http.Request) {
	token, err := transcodeworker.NewScalingToken()
	if err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "create scaling token")
		return
	}
	if err := h.workers.SetScalingTokenHash(r.Context(), transcodeworker.ScalingTokenHash(token), h.clock.Now()); err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "store scaling token")
		return
	}
	_ = render.JSON(w, r, http.StatusOK, dto.TranscodeWorkerScalingToken{Token: token})
}

func (h transcodeWorkerAdmin) deleteScalingToken(w http.ResponseWriter, r *http.Request) {
	if err := h.workers.DeleteScalingToken(r.Context()); err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "delete scaling token")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
