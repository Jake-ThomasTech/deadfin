// SPDX-License-Identifier: GPL-2.0-only

package middleware

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store"
)

// Resolver resolves the credential identity carried by one HTTP request.
type Resolver interface {
	Resolve(context.Context, *http.Request) (auth.Identity, error)
}

// Evaluator applies the policy metadata of a registered route.
type Evaluator interface {
	Evaluate(context.Context, auth.PolicyRequest) (auth.Decision, error)
}

// Users loads the current persisted user needed by non-API-key policies.
type Users interface {
	Get(context.Context, dto.UUID) (store.User, error)
}

// PlaySessionGrants resolves the item-bound playback credential of D-54.
type PlaySessionGrants interface {
	Authorize(playSessionID string, itemID dto.UUID, mediaSourceID string) (dto.UUID, bool)
}

// Authorizer enforces declared route authorization before a handler runs.
type Authorizer struct {
	resolver  Resolver
	evaluator Evaluator
	users     Users
	grants    PlaySessionGrants
	scaling   func(*http.Request) (bool, error)
}

// NewAuthorizer constructs route authorization from independently supplied
// authentication, policy, and user-loading seams.
func NewAuthorizer(resolver Resolver, evaluator Evaluator, users Users) *Authorizer {
	return &Authorizer{resolver: resolver, evaluator: evaluator, users: users}
}

// WithPlaySessionGrants enables the PlaySessionId credential on routes that
// declare PlaySessionCredential, and returns authorizer.
func (authorizer *Authorizer) WithPlaySessionGrants(grants PlaySessionGrants) *Authorizer {
	authorizer.grants = grants
	return authorizer
}

// WithScalingCredential enables the scaling token on routes that declare
// ScalingCredential (#617), and returns authorizer. check reports whether
// the request carries a valid scaling token.
func (authorizer *Authorizer) WithScalingCredential(check func(*http.Request) (bool, error)) *Authorizer {
	authorizer.scaling = check
	return authorizer
}

// Wrap returns a handler that enforces route metadata before calling next.
func (authorizer *Authorizer) Wrap(route router.Route, next http.Handler) http.Handler {
	if route.Worker {
		// The worker route checks its own credential; no user identity.
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if route.Anonymous {
			if route.OptionalIdentity && authorizer != nil && authorizer.resolver != nil {
				request = request.WithContext(auth.NewRequestCache(request.Context()))
				identity, err := authorizer.resolver.Resolve(request.Context(), request)
				if err != nil {
					writeAuthorizationError(w, http.StatusInternalServerError)
					return
				}
				if identity.Authenticated {
					request = request.WithContext(withIdentity(request.Context(), identity))
				}
			}
			next.ServeHTTP(w, request)
			return
		}
		if authorizer == nil || authorizer.resolver == nil || authorizer.evaluator == nil || authorizer.users == nil {
			writeAuthorizationError(w, http.StatusInternalServerError)
			return
		}
		if route.ScalingCredential && authorizer.scaling != nil {
			allowed, err := authorizer.scaling(request)
			if err != nil {
				writeAuthorizationError(w, http.StatusInternalServerError)
				return
			}
			if allowed {
				next.ServeHTTP(w, request)
				return
			}
		}

		request = request.WithContext(auth.NewRequestCache(request.Context()))
		if !ClientAddressFromContext(request.Context()).IsValid() {
			request = request.WithContext(WithClientAddress(request.Context(), NormalizeClientAddress(request.RemoteAddr)))
		}
		if route.PlaySessionCredential {
			request = aliasMediaElementAPIKey(request)
		}
		identity, err := authorizer.resolver.Resolve(request.Context(), request)
		if err != nil {
			writeAuthorizationError(w, http.StatusInternalServerError)
			return
		}
		if !identity.Authenticated && route.PlaySessionCredential {
			if userID, granted := authorizer.playSessionGrant(request); granted {
				identity = auth.Identity{Authenticated: true, UserID: userID}
			}
		}
		request = request.WithContext(withIdentity(request.Context(), identity))

		policyRequest := auth.PolicyRequest{
			Policy:         route.Policy,
			Identity:       identity,
			RemoteIP:       ClientAddressFromContext(request.Context()),
			SyncPlayActive: SyncPlayActiveFromContext(request.Context()),
		}
		if identity.Authenticated && !identity.APIKey {
			policyRequest.User, err = authorizer.users.Get(request.Context(), identity.UserID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writeAuthorizationError(w, http.StatusUnauthorized)
					return
				}
				writeAuthorizationError(w, http.StatusInternalServerError)
				return
			}
			if route.Policy == string(auth.PolicyAnonymousLANAccess) {
				disabled, err := auth.UserDisabled(policyRequest.User)
				if err != nil {
					writeAuthorizationError(w, http.StatusInternalServerError)
					return
				}
				if disabled {
					writeAuthorizationError(w, http.StatusUnauthorized)
					return
				}
			}
			administrator, err := auth.UserAdministrator(policyRequest.User)
			if err != nil {
				writeAuthorizationError(w, http.StatusInternalServerError)
				return
			}
			identity.Administrator = identity.Administrator || administrator
			policyRequest.Identity = identity
			request = request.WithContext(withIdentity(request.Context(), identity))
		}

		decision, err := authorizer.evaluator.Evaluate(request.Context(), policyRequest)
		if err != nil || decision == auth.DecisionConfigurationError {
			writeAuthorizationError(w, http.StatusInternalServerError)
			return
		}
		switch decision {
		case auth.DecisionAllowed:
			next.ServeHTTP(w, request)
		case auth.DecisionUnauthenticated:
			writeAuthorizationError(w, http.StatusUnauthorized)
		case auth.DecisionForbidden:
			if !identity.Authenticated {
				writeAuthorizationError(w, http.StatusUnauthorized)
				return
			}
			writeAuthorizationError(w, http.StatusForbidden)
		default:
			writeAuthorizationError(w, http.StatusInternalServerError)
		}
	})
}

// aliasMediaElementAPIKey lets an api_key query token count as ApiKey on a
// route that declares PlaySessionCredential, even when
// EnableLegacyAuthorization is off (D-54). These routes are anonymous
// upstream, and a media element can carry its token only in the URL; the web
// client puts it in api_key. The resolver's precedence still prefers an
// Authorization header and an explicit ApiKey.
func aliasMediaElementAPIKey(request *http.Request) *http.Request {
	var query struct {
		APIKey       *string `query:"ApiKey"`
		LegacyAPIKey *string `query:"api_key"`
	}
	if err := bind.QueryRequest(request, &query); err != nil || query.APIKey != nil || query.LegacyAPIKey == nil || *query.LegacyAPIKey == "" {
		return request
	}
	aliased := request.Clone(request.Context())
	aliased.URL.RawQuery += "&" + url.Values{"ApiKey": {*query.LegacyAPIKey}}.Encode()
	return aliased
}

// playSessionGrant reads the PlaySessionId and MediaSourceId query values and
// the route item, and asks the grant store for the user they may stream as
// (D-54). The grant comes from an earlier authenticated PlaybackInfo.
func (authorizer *Authorizer) playSessionGrant(request *http.Request) (dto.UUID, bool) {
	if authorizer.grants == nil {
		return dto.NilUUID, false
	}
	var query struct {
		PlaySessionID *string `query:"playSessionId"`
		MediaSourceID *string `query:"mediaSourceId"`
	}
	if err := bind.QueryRequest(request, &query); err != nil || query.PlaySessionID == nil {
		return dto.NilUUID, false
	}
	itemID, err := dto.ParseUUID(chi.URLParam(request, "itemId"))
	if err != nil {
		return dto.NilUUID, false
	}
	mediaSourceID := ""
	if query.MediaSourceID != nil {
		mediaSourceID = *query.MediaSourceID
	}
	return authorizer.grants.Authorize(*query.PlaySessionID, itemID, mediaSourceID)
}

func writeAuthorizationError(w http.ResponseWriter, status int) {
	if status == http.StatusUnauthorized {
		w.WriteHeader(status)
		return
	}
	http.Error(w, http.StatusText(status), status)
}

// NormalizeClientAddress derives the transport peer address from a
// RemoteAddr value. It reads no forwarded header; ClientAddress applies
// X-Forwarded-For from KnownProxies peers.
func NormalizeClientAddress(remoteAddress string) netip.Addr {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		host = remoteAddress
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return address.Unmap()
}

type clientAddressContextKey struct{}

// WithClientAddress installs a normalized client address at the HTTP
// middleware boundary (ClientAddresses).
func WithClientAddress(ctx context.Context, address netip.Addr) context.Context {
	if address.IsValid() {
		address = address.Unmap()
	}
	return context.WithValue(ctx, clientAddressContextKey{}, address)
}

// ClientAddressFromContext returns the normalized client address.
// Callers receive an invalid address when no transport middleware installed it.
func ClientAddressFromContext(ctx context.Context) netip.Addr {
	address, _ := ctx.Value(clientAddressContextKey{}).(netip.Addr)
	return address
}

type identityContextKey struct{}

func withIdentity(ctx context.Context, identity auth.Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// IdentityFromContext returns the resolved request identity installed by
// Authorizer after credential resolution.
func IdentityFromContext(ctx context.Context) (auth.Identity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(auth.Identity)
	return identity, ok
}

type syncPlayActiveContextKey struct{}

// WithSyncPlayActive annotates a request context with its SyncPlay membership.
func WithSyncPlayActive(ctx context.Context, active bool) context.Context {
	return context.WithValue(ctx, syncPlayActiveContextKey{}, active)
}

// SyncPlayActiveFromContext reports the request's SyncPlay membership fact.
func SyncPlayActiveFromContext(ctx context.Context) bool {
	active, _ := ctx.Value(syncPlayActiveContextKey{}).(bool)
	return active
}
