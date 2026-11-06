// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/config"
)

// RegisterConfiguration registers the system configuration slice on rt,
// ported from upstream/jellyfin/Jellyfin.Api/Controllers/
// ConfigurationController.cs. The controller requires authentication
// ([Authorize], :21-24). Reads use PolicyDefaultAuthorization. Writes require
// elevation (Policies.RequiresElevation, :62, :92, :122).
func RegisterConfiguration(rt *router.Router, manager *config.Manager) {
	h := configuration{manager: manager}

	rt.Handle(http.MethodGet, "/System/Configuration", router.Route{
		OperationID: "GetConfiguration",
		Policy:      string(auth.PolicyDefaultAuthorization),
	}, h.getConfiguration)
	rt.Handle(http.MethodPost, "/System/Configuration", router.Route{
		OperationID: "UpdateConfiguration",
		Policy:      string(auth.PolicyRequiresElevation),
	}, h.updateConfiguration)
	rt.Handle(http.MethodPost, "/System/Configuration/Branding", router.Route{
		OperationID: "UpdateBrandingConfiguration",
		Policy:      string(auth.PolicyRequiresElevation),
	}, h.updateBrandingConfiguration)
	rt.Handle(http.MethodGet, "/System/Configuration/{key}", router.Route{
		OperationID: "GetNamedConfiguration",
		Policy:      string(auth.PolicyDefaultAuthorization),
	}, h.getNamedConfiguration)
	rt.Handle(http.MethodPost, "/System/Configuration/{key}", router.Route{
		OperationID: "UpdateNamedConfiguration",
		Policy:      string(auth.PolicyRequiresElevation),
	}, h.updateNamedConfiguration)
}

type configuration struct {
	manager *config.Manager
}

// getConfiguration returns the complete upstream ServerConfiguration document.
// It matches ConfigurationController.cs:49-54, which returns
// _configurationManager.Configuration.
func (h configuration) getConfiguration(w http.ResponseWriter, r *http.Request) {
	_ = render.JSON(w, r, http.StatusOK, h.manager.System())
}

// updateConfiguration replaces the server configuration
// (ConfigurationController.cs:62-68). The body decodes over the upstream
// defaults, so a member that the client omits keeps its default.
func (h configuration) updateConfiguration(w http.ResponseWriter, r *http.Request) {
	body, ok := configurationBody(w, r)
	if !ok {
		return
	}
	var posted config.ServerConfiguration
	if err := posted.UnmarshalJSON(body); err != nil {
		writeConfigurationError(w, r, err)
		return
	}
	if err := h.manager.ReplaceSystem(r.Context(), posted); err != nil {
		writeConfigurationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getNamedConfiguration returns a registered named configuration
// (ConfigurationController.cs:77-83). An unknown key keeps the standard
// not-found response.
func (h configuration) getNamedConfiguration(w http.ResponseWriter, r *http.Request) {
	value, err := h.manager.NamedConfiguration(chi.URLParam(r, "key"))
	if err != nil {
		renderProblem(w, r, http.StatusNotFound, "Named configuration not found.")
		return
	}
	// Tuner and guide URLs carry provider credentials; only an administrator
	// sees them unmasked (D-69).
	if options, ok := value.(config.LiveTvOptions); ok {
		if identity, _ := middleware.IdentityFromContext(r.Context()); !identity.Administrator {
			value = options.Redacted()
		}
	}
	_ = render.JSON(w, r, http.StatusOK, value)
}

// updateNamedConfiguration decodes the body as the configuration type
// registered for key and saves it (ConfigurationController.cs:92-106).
func (h configuration) updateNamedConfiguration(w http.ResponseWriter, r *http.Request) {
	body, ok := configurationBody(w, r)
	if !ok {
		return
	}
	if err := h.manager.SaveNamedConfiguration(r.Context(), chi.URLParam(r, "key"), body); err != nil {
		writeConfigurationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateBrandingConfiguration applies a BrandingOptionsDto and keeps the
// current SplashscreenLocation (ConfigurationController.cs:122-137).
func (h configuration) updateBrandingConfiguration(w http.ResponseWriter, r *http.Request) {
	body, ok := configurationBody(w, r)
	if !ok {
		return
	}
	if err := h.manager.SaveNamedConfiguration(r.Context(), config.KeyBranding, body); err != nil {
		writeConfigurationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// configurationBody reads a required JSON body. A missing body fails the
// [FromBody, Required] binding with 400.
func configurationBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	var raw rawJSONBody
	present, err := bind.OptionalJSONBody(r, &raw)
	switch {
	case errors.Is(err, bind.ErrUnsupportedMediaType):
		renderProblem(w, r, http.StatusUnsupportedMediaType, "Unsupported Media Type")
		return nil, false
	case err != nil || !present:
		renderProblem(w, r, http.StatusBadRequest, "A non-empty request body is required.")
		return nil, false
	}
	return raw, true
}

// rawJSONBody keeps the validated JSON document for type-directed decoding.
type rawJSONBody []byte

func (body *rawJSONBody) UnmarshalJSON(data []byte) error {
	*body = append((*body)[:0], data...)
	return nil
}

// writeConfigurationError maps the configuration manager failures to the
// status codes that upstream ExceptionMiddleware assigns to the exceptions
// the ported code throws (ExceptionMiddleware.cs:123-136): ArgumentException
// 400, DirectoryNotFoundException and ResourceNotFoundException 404, and any
// other exception 500. The body is the production text/plain message.
func writeConfigurationError(w http.ResponseWriter, _ *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, config.ErrInvalidConfiguration):
		status = http.StatusBadRequest
	case errors.Is(err, config.ErrDirectoryNotFound), errors.Is(err, config.ErrUnknownConfiguration):
		status = http.StatusNotFound
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "Error processing request.")
}
