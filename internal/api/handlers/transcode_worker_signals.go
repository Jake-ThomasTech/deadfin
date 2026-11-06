// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"bytes"
	"io"
	"net/http"

	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/transcodeworker"
)

// TranscodeWorkerSignals is the read side of the transcode-worker scaling
// signals (#617). *transcodeworker.Hub implements it.
type TranscodeWorkerSignals interface {
	Scaling() transcodeworker.Scaling
	WriteMetrics(io.Writer) error
}

// RegisterTranscodeWorkerSignals registers the metrics in the Prometheus
// text format and the scaling signals as JSON
// (docs/spikes/transcode-workers.md §3.9). An administrator, an API key or
// a scaling token reads them. The route-only composition passes nil; the
// routes then answer 404.
func RegisterTranscodeWorkerSignals(rt *router.Router, signals TranscodeWorkerSignals) {
	route := func(operation string) router.Route {
		return router.Route{OperationID: "NATIVE:" + operation, Policy: string(auth.PolicyRequiresElevation), ScalingCredential: true}
	}
	metrics, scaling := http.HandlerFunc(http.NotFound), http.HandlerFunc(http.NotFound)
	if signals != nil {
		metrics = func(w http.ResponseWriter, r *http.Request) {
			var body bytes.Buffer
			if err := signals.WriteMetrics(&body); err != nil {
				renderProblem(w, r, http.StatusInternalServerError, "write metrics")
				return
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			_, _ = body.WriteTo(w)
		}
		scaling = func(w http.ResponseWriter, r *http.Request) {
			_ = render.JSON(w, r, http.StatusOK, signals.Scaling())
		}
	}
	rt.Handle(http.MethodGet, "/deadfin/v1/metrics", route("GetMetrics"), metrics)
	rt.Handle(http.MethodGet, "/deadfin/v1/workers/scaling", route("GetTranscodeWorkerScaling"), scaling)
}
