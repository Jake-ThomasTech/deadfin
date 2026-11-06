// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/dtobuild"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/sessiondto"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/user"
	"github.com/go-chi/chi/v5"
)

func RegisterAuth(rt *router.Router, login *auth.Login, users *user.Service, sessions *session.Manager, items *store.ItemStore, itemBuilder *dtobuild.Builder, identity config.ServerIdentity, startup interface{ StartupWizardCompleted() bool }, network auth.LocalNetwork, transcoding sessiondto.TranscodingSource, peers SessionPeers) {
	h := authHandler{login: login, users: users, sessions: sessions, items: items, itemBuilder: itemBuilder, identity: identity, startup: startup, network: network, transcoding: transcoding, peers: peers}
	rt.Handle(http.MethodPost, "/Users/AuthenticateByName", router.Route{OperationID: "AuthenticateUserByName", Anonymous: true}, h.loginByName)
	rt.Handle(http.MethodPost, "/Users/{userId}/Authenticate", router.Route{OperationID: "SHADOW:AuthenticateUser", Anonymous: true}, h.loginLegacy)
	rt.Handle(http.MethodGet, "/Users/Me", router.Route{OperationID: "GetCurrentUser", Policy: string(auth.PolicyDefaultAuthorization)}, h.me)
	rt.Handle(http.MethodGet, "/Users/Public", router.Route{OperationID: "GetPublicUsers", Anonymous: true}, h.public)
	rt.Handle(http.MethodGet, "/Users/{userId}", router.Route{OperationID: "GetUserById", Policy: string(auth.PolicyIgnoreParentalControl)}, h.get)
	rt.Handle(http.MethodGet, "/Users", router.Route{OperationID: "GetUsers", Policy: string(auth.PolicyDefaultAuthorization)}, h.list)
	rt.Handle(http.MethodPost, "/Users/Configuration", router.Route{OperationID: "UpdateUserConfiguration", Policy: string(auth.PolicyDefaultAuthorization)}, h.configuration)
	rt.Handle(http.MethodPost, "/Users/{userId}/Configuration", router.Route{OperationID: "SHADOW:UpdateUserConfigurationLegacy", Policy: string(auth.PolicyDefaultAuthorization)}, h.configuration)
	rt.Handle(http.MethodPost, "/Users/Password", router.Route{OperationID: "UpdateUserPassword", Policy: string(auth.PolicyDefaultAuthorization)}, h.password)
	rt.Handle(http.MethodPost, "/Users/{userId}/Password", router.Route{OperationID: "SHADOW:UpdateUserPasswordLegacy", Policy: string(auth.PolicyDefaultAuthorization)}, h.password)
	rt.Handle(http.MethodPost, "/Users/{userId}/Policy", router.Route{OperationID: "UpdateUserPolicy", Policy: string(auth.PolicyRequiresElevation)}, h.policy)
	rt.Handle(http.MethodPost, "/Users/New", router.Route{OperationID: "CreateUserByName", Policy: string(auth.PolicyRequiresElevation)}, h.create)
	rt.Handle(http.MethodDelete, "/Users/{userId}", router.Route{OperationID: "DeleteUser", Policy: string(auth.PolicyRequiresElevation)}, h.delete)
	rt.Handle(http.MethodPost, "/Users", router.Route{OperationID: "UpdateUser", Policy: string(auth.PolicyDefaultAuthorization)}, h.update)
	rt.Handle(http.MethodPost, "/Users/{userId}", router.Route{OperationID: "SHADOW:UpdateUserLegacy", Policy: string(auth.PolicyDefaultAuthorization)}, h.update)
	rt.Handle(http.MethodGet, "/Sessions", router.Route{OperationID: "GetSessions", Policy: string(auth.PolicyDefaultAuthorization)}, h.sessionsList)
	rt.Handle(http.MethodPost, "/Sessions/Capabilities/Full", router.Route{OperationID: "PostFullCapabilities", Policy: string(auth.PolicyDefaultAuthorization)}, h.capabilities)
	rt.Handle(http.MethodPost, "/Sessions/Capabilities", router.Route{OperationID: "PostCapabilities", Policy: string(auth.PolicyDefaultAuthorization)}, h.postCapabilities)
}

type authHandler struct {
	login       *auth.Login
	users       *user.Service
	sessions    *session.Manager
	items       *store.ItemStore
	itemBuilder *dtobuild.Builder
	transcoding sessiondto.TranscodingSource

	identity config.ServerIdentity
	startup  interface{ StartupWizardCompleted() bool }
	network  auth.LocalNetwork
	peers    SessionPeers
}

// SessionPeers returns the other server replicas' parts of a GET /Sessions
// answer (#663); nil without replicas.
type SessionPeers interface {
	PeerSessions(request *http.Request) []dto.SessionInfoDto
}

func (h authHandler) loginByName(w http.ResponseWriter, r *http.Request) { h.loginRequest(w, r, "") }
func (h authHandler) loginLegacy(w http.ResponseWriter, r *http.Request) {
	h.loginRequest(w, r, chi.URLParam(r, "userId"))
}

func (h authHandler) loginRequest(w http.ResponseWriter, r *http.Request, target string) {
	var body dto.AuthenticateUserByName
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	if target != "" && body.Username == nil {
		id, err := dto.ParseUUID(target)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
		stored, err := h.users.Get(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		name, password := stored.Name, bind.QueryFirst(r, "pw")
		body.Username, body.Pw = &name, &password
	}
	if body.Username == nil || body.Pw == nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	c := auth.Extract(r, h.login.LegacyAuthorizationEnabled())
	result, err := h.login.Authenticate(r.Context(), auth.LoginRequest{Username: *body.Username, Password: *body.Pw, Client: c.Client, Device: c.Device, DeviceID: c.DeviceID, Version: c.Version, RemoteIP: requestRemoteIP(r), RemoteEndPoint: requestRemoteHost(r)})
	if err != nil {
		status := http.StatusInternalServerError
		var loginError *auth.LoginError
		if errors.As(err, &loginError) {
			status = loginError.Status
		}
		if target == "" && errors.Is(err, auth.ErrLoginDenied) {
			account, lookupErr := h.users.GetByName(r.Context(), *body.Username)
			if lookupErr == nil {
				disabled, policyErr := auth.UserDisabled(account)
				if policyErr == nil && disabled {
					status = http.StatusUnauthorized
				}
			}
		}
		writeLoginError(w, status)
		return
	}
	projection, err := h.users.GetDTO(r.Context(), result.User.ID)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	user := h.projectUser(projection)
	user.Policy.InvalidLoginAttemptCount = loginInvalidAttemptCount(result.User.Policy)
	_ = render.JSON(w, r, http.StatusOK, dto.AuthenticationResult{User: user, SessionInfo: h.projectSession(r.Context(), result.Session, result.User.Name, h.identity.ID.String()), AccessToken: result.AccessToken, ServerID: result.ServerID})
}

func writeLoginError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = w.Write([]byte("Error processing request."))
}

// loginInvalidAttemptCount preserves the entity snapshot returned by the
// successful authentication pipeline. Jellyfin resets the stored count after
// that snapshot has been loaded, so /Users/Me observes zero while the login
// response still exposes the preceding failed attempt.
func loginInvalidAttemptCount(policy json.RawMessage) int {
	value := dto.DefaultUserPolicy()
	if dto.DecodeUserPolicy(policy, &value) != nil {
		return 0
	}
	return value.InvalidLoginAttemptCount
}

func (h authHandler) me(w http.ResponseWriter, r *http.Request) {
	p, err := h.users.Me(r.Context(), actor(r))
	if err != nil {
		writeUserError(w, err)
		return
	}
	_ = render.JSON(w, r, http.StatusOK, h.projectUser(p))
}

func (h authHandler) public(w http.ResponseWriter, r *http.Request) {
	credentials := auth.Extract(r, true)
	remote := requestRemoteIP(r)
	values, err := h.users.List(r.Context(), user.Actor{DeviceID: credentials.DeviceID, Remote: remote.IsValid() && !h.network.IsInLocalNetwork(remote)}, user.UserListOptions{Public: true, StartupComplete: h.startup.StartupWizardCompleted()})
	if err != nil {
		writeUserError(w, err)
		return
	}
	out := make([]dto.UserDto, len(values))
	for i := range values {
		out[i] = h.projectUser(values[i])
	}
	_ = render.JSON(w, r, http.StatusOK, out)
}

func (h authHandler) get(w http.ResponseWriter, r *http.Request) {
	id, err := dto.ParseUUID(chi.URLParam(r, "userId"))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	p, err := h.users.GetDTO(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	_ = render.JSON(w, r, http.StatusOK, h.projectUser(p))
}

func (h authHandler) list(w http.ResponseWriter, r *http.Request) {
	var q struct {
		IsHidden   *bool `query:"IsHidden"`
		IsDisabled *bool `query:"IsDisabled"`
	}
	if err := bind.QueryRequest(r, &q); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	values, err := h.users.List(r.Context(), actor(r), user.UserListOptions{IsHidden: q.IsHidden, IsDisabled: q.IsDisabled})
	if err != nil {
		writeUserError(w, err)
		return
	}
	out := make([]dto.UserDto, len(values))
	for i := range values {
		out[i] = h.projectUser(values[i])
	}
	_ = render.JSON(w, r, http.StatusOK, out)
}

func (h authHandler) configuration(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := bind.JSONBody(r, &raw); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	var request struct {
		SubtitleMode *dto.SubtitleMode `json:"SubtitleMode"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	id, ok := targetUser(w, r)
	if !ok {
		return
	}
	if err := h.users.UpdateConfiguration(r.Context(), actor(r), id, raw); err != nil {
		writeUserError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h authHandler) password(w http.ResponseWriter, r *http.Request) {
	var body dto.UpdateUserPassword
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	id, ok := targetUser(w, r)
	if !ok {
		return
	}
	current := ""
	if body.CurrentPw != nil {
		current = *body.CurrentPw
	} else if body.CurrentPassword != nil {
		current = *body.CurrentPassword
	}
	next := ""
	if body.NewPw != nil {
		next = *body.NewPw
	}
	if err := h.users.UpdatePassword(r.Context(), actor(r), id, user.PasswordUpdate{Current: current, New: next, Reset: body.ResetPassword}); err != nil {
		writeUserError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// create ports UserController.CreateUserByName.
func (h authHandler) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     *string `json:"Name"`
		Password *string `json:"Password"`
	}
	if err := bind.JSONBody(r, &body); err != nil || body.Name == nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	created, err := h.users.CreateByName(r.Context(), *body.Name, body.Password)
	if err != nil {
		writeUserError(w, err)
		return
	}
	p, err := h.users.GetDTO(r.Context(), created.ID)
	if err != nil {
		writeUserError(w, err)
		return
	}
	_ = render.JSON(w, r, http.StatusOK, h.projectUser(p))
}

// delete ports UserController.DeleteUser: revoke the user's tokens, then
// delete (UserManager.DeleteUserAsync keeps the last user and last admin).
func (h authHandler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := dto.ParseUUID(chi.URLParam(r, "userId"))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	if _, err := h.users.Get(r.Context(), id); err != nil {
		writeUserError(w, err)
		return
	}
	if err := h.users.Delete(r.Context(), id); err != nil {
		writeUserError(w, err)
		return
	}
	if err := h.users.RevokeTokens(r.Context(), id, ""); err != nil && !errors.Is(err, store.ErrNotFound) {
		writeUserError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// update ports UserController.UpdateUser and its legacy route.
func (h authHandler) update(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          *string         `json:"Name"`
		Configuration json.RawMessage `json:"Configuration"`
	}
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	id, ok := targetUser(w, r)
	if !ok {
		return
	}
	if err := h.users.UpdateUser(r.Context(), actor(r), id, body.Name, body.Configuration); err != nil {
		writeUserError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h authHandler) policy(w http.ResponseWriter, r *http.Request) {
	var document json.RawMessage
	if err := bind.JSONBody(r, &document); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	id, ok := targetUser(w, r)
	if !ok {
		return
	}
	if err := h.users.UpdatePolicy(r.Context(), id, document); err != nil {
		writeUserError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h authHandler) sessionsList(w http.ResponseWriter, r *http.Request) {
	var q struct {
		DeviceID     string    `query:"DeviceId"`
		Active       *int      `query:"ActiveWithinSeconds"`
		Controllable *dto.UUID `query:"ControllableByUserId"`
	}
	if bind.QueryRequest(r, &q) != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	s, err := h.users.FilterSessions(r.Context(), actor(r), q.Controllable, h.sessions.Sessions(session.Query{DeviceID: q.DeviceID, ActiveWithinSeconds: q.Active}))
	if err != nil {
		writeUserError(w, err)
		return
	}
	out := make([]dto.SessionInfoDto, len(s))
	for i := range s {
		name := ""
		if user, err := h.users.GetDTO(r.Context(), s[i].UserID); err == nil {
			name = user.Name
		}
		out[i] = h.projectSession(r.Context(), s[i], name, h.identity.ID.String())
	}
	if h.peers != nil {
		out = sessiondto.Merge(out, h.peers.PeerSessions(r))
	}
	_ = render.JSON(w, r, http.StatusOK, out)
}

func (h authHandler) capabilities(w http.ResponseWriter, r *http.Request) {
	var body dto.ClientCapabilities
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	var q struct {
		ID *string `query:"Id"`
	}
	if bind.QueryRequest(r, &q) != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	identity, _ := middleware.IdentityFromContext(r.Context())
	err := h.sessions.ReportCapabilities(r.Context(), session.CapabilityReport{
		ID:        q.ID,
		CurrentID: sessionIDFor(h.sessions, identity.Client, identity.DeviceID, identity.UserID),
		Owner:     capabilityOwner(identity),
		Capabilities: session.Capabilities{
			PlayableMediaTypes:           []string(body.PlayableMediaTypes),
			SupportedCommands:            []string(body.SupportedCommands),
			SupportsMediaControl:         body.SupportsMediaControl,
			SupportsPersistentIdentifier: body.SupportsPersistentIdentifier,
			DeviceProfile:                deviceProfileBytes(body.DeviceProfile),
		},
	})
	if errors.Is(err, session.ErrNotFound) {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// postCapabilities handles POST /Sessions/Capabilities, the query-parameter
// variant of the capability report. Upstream binds the same fields from the
// query string (SessionController.cs:348-366). SupportsPersistentIdentifier
// defaults to true when absent (jellyfin-openapi-stable.json:34457-34464).
func (h authHandler) postCapabilities(w http.ResponseWriter, r *http.Request) {
	var q struct {
		ID                           *string  `query:"Id"`
		PlayableMediaTypes           []string `query:"PlayableMediaTypes"`
		SupportedCommands            []string `query:"SupportedCommands"`
		SupportsMediaControl         bool     `query:"SupportsMediaControl"`
		SupportsPersistentIdentifier *bool    `query:"SupportsPersistentIdentifier"`
	}
	if bind.QueryRequest(r, &q) != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	supportsPersistentIdentifier := true
	if q.SupportsPersistentIdentifier != nil {
		supportsPersistentIdentifier = *q.SupportsPersistentIdentifier
	}
	identity, _ := middleware.IdentityFromContext(r.Context())
	err := h.sessions.ReportCapabilities(r.Context(), session.CapabilityReport{
		ID:        q.ID,
		CurrentID: sessionIDFor(h.sessions, identity.Client, identity.DeviceID, identity.UserID),
		Owner:     capabilityOwner(identity),
		Capabilities: session.Capabilities{
			PlayableMediaTypes:           q.PlayableMediaTypes,
			SupportedCommands:            q.SupportedCommands,
			SupportsMediaControl:         q.SupportsMediaControl,
			SupportsPersistentIdentifier: supportsPersistentIdentifier,
			DeviceProfile:                nil,
		},
	})
	if errors.Is(err, session.ErrNotFound) {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func deviceProfileBytes(profile *json.RawMessage) []byte {
	if profile == nil {
		return nil
	}
	return append([]byte(nil), (*profile)...)
}

func requestRemoteHost(r *http.Request) string {
	return middleware.RemoteEndpoint(r)
}

func requestRemoteIP(r *http.Request) netip.Addr {
	if address := middleware.ClientAddressFromContext(r.Context()); address.IsValid() {
		return address
	}
	return middleware.NormalizeClientAddress(r.RemoteAddr)
}

// capabilityOwner restricts a non-administrator capability report to the
// caller's own sessions. Pinned SessionController.PostCapabilities and
// PostFullCapabilities accept any session ID (SessionController.cs:355-386);
// Deadfin answers another user's ID as an unknown session (divergence D-67).
func capabilityOwner(identity auth.Identity) *dto.UUID {
	if identity.Administrator || identity.APIKey {
		return nil
	}
	return &identity.UserID
}

func sessionIDFor(m *session.Manager, client, device string, userID dto.UUID) string {
	snapshot, found := m.Session(client, device)
	if !found || snapshot.UserID != userID {
		return ""
	}
	return snapshot.ID
}

func targetUser(w http.ResponseWriter, r *http.Request) (dto.UUID, bool) {
	v := chi.URLParam(r, "userId")
	if v == "" {
		var err error
		v, err = bind.QueryScalar(r, "userId")
		if err != nil {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return dto.NilUUID, false
		}
	}
	if v == "" {
		i, ok := middleware.IdentityFromContext(r.Context())
		if !ok {
			return dto.NilUUID, false
		}
		return i.UserID, true
	}
	id, err := dto.ParseUUID(v)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return dto.NilUUID, false
	}
	return id, true
}

func actor(r *http.Request) user.Actor {
	i, _ := middleware.IdentityFromContext(r.Context())
	return user.Actor{UserID: i.UserID, Administrator: i.Administrator, APIKey: i.APIKey, DeviceID: i.DeviceID, Token: i.Token}
}

func writeUserError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, user.ErrBadRequest), errors.Is(err, user.ErrInvalidUsername), errors.Is(err, user.ErrAdministratorPasswordRequired),
		errors.Is(err, store.ErrDuplicateName), errors.Is(err, user.ErrLastAdministrator):
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
	case errors.Is(err, user.ErrUnauthorized):
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
	case errors.Is(err, user.ErrForbidden):
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
	default:
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

func (h authHandler) projectUser(v user.UserDTO) dto.UserDto {
	p := dto.DefaultUserPolicy()
	c := dto.DefaultUserConfiguration()
	_ = dto.DecodeUserPolicy(v.Policy, &p)
	_ = json.Unmarshal(v.Configuration, &c)
	autoLogin := false
	hasPassword := true
	hasConfiguredPassword := true
	hasConfiguredEasyPassword := false
	projected := dto.UserDto{
		ID:                        v.ID,
		Name:                      v.Name,
		ServerID:                  h.identity.ID.String(),
		HasPassword:               &hasPassword,
		HasConfiguredPassword:     &hasConfiguredPassword,
		HasConfiguredEasyPassword: &hasConfiguredEasyPassword,
		EnableAutoLogin:           &autoLogin,
		Policy:                    p,
		Configuration:             c,
	}
	if v.LastLoginDate != nil {
		date := dto.NewTime(*v.LastLoginDate)
		projected.LastLoginDate = &date
	}
	if v.LastActivityDate != nil {
		date := dto.NewTime(*v.LastActivityDate)
		projected.LastActivityDate = &date
	}
	return projected
}

func (h authHandler) projectSession(ctx context.Context, snapshot session.Snapshot, userName, serverID string) dto.SessionInfoDto {
	return sessiondto.New(h.users, h.items, h.itemBuilder, serverID).WithTranscoding(h.transcoding).Project(ctx, snapshot, userName)
}
