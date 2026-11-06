// SPDX-License-Identifier: GPL-2.0-only

package middleware

import (
	"net/http"
	"strings"

	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/session"
)

// SessionActivity records authenticated request activity for the exact
// client/device session after the authorizer has admitted a protected request.
type SessionActivity struct {
	manager *session.Manager
	skip    func(*http.Request) bool
}

// NewSessionActivity constructs the route wrapper used after authorization.
func NewSessionActivity(manager *session.Manager) *SessionActivity {
	return &SessionActivity{manager: manager}
}

// Except returns activity that records nothing for a request that skip
// selects, such as a request that a server replica forwarded for another
// client's session (#663): the replica that owns the caller's session
// records the activity.
func (activity *SessionActivity) Except(skip func(*http.Request) bool) *SessionActivity {
	return &SessionActivity{manager: activity.manager, skip: skip}
}

// Wrap restores a missing session for the authenticated user, refreshes that
// user's existing session, and leaves another user's session untouched.
func (activity *SessionActivity) Wrap(route router.Route, next http.Handler) http.Handler {
	if route.Anonymous || route.Worker || activity == nil || activity.manager == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, authenticated := IdentityFromContext(r.Context())
		if authenticated && identity.Authenticated && sessionActivityRequired(route, r) && (activity.skip == nil || !activity.skip(r)) {
			activity.manager.RecordAuthenticatedActivity(session.Activity{Client: identity.Client, Version: identity.Version, DeviceID: identity.DeviceID, DeviceName: identity.Device, UserID: identity.UserID, RemoteEndPoint: RemoteEndpoint(r)})
		}
		next.ServeHTTP(w, r)
	})
}

// sessionActivityRequired mirrors the controller methods that call
// RequestHelpers.GetSession. Read-only user, display-preference, and session
// listing endpoints do not log session activity upstream. Both capability
// report endpoints log it only when their omitted Id requires resolving
// the caller's current session.
func sessionActivityRequired(route router.Route, r *http.Request) bool {
	switch route.OperationID {
	case "GetCurrentUser", "GetDisplayPreferences", "UpdateDisplayPreferences", "GetSessions":
		return false
	case "PostFullCapabilities", "PostCapabilities":
		for key, values := range r.URL.Query() {
			if strings.EqualFold(key, "id") {
				return len(values) == 0 || strings.TrimSpace(values[0]) == ""
			}
		}
	}
	return true
}
