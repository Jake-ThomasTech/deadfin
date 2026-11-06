// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"net/http"

	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/transcodeworker"
)

// RegisterTranscodeWorkers registers the transcode-worker routes (#614,
// docs/spikes/transcode-workers.md §3.3). The hub serves all three: the
// control WebSocket and the per-job data plane. The routes carry the worker
// credential marker, so the user authorizer does not run and the hub
// authenticates every request itself. The route-only composition (the route
// ledger) passes a nil hub; the routes then answer 404.
func RegisterTranscodeWorkers(rt *router.Router, hub http.Handler) {
	if hub == nil {
		hub = http.NotFoundHandler()
	}
	worker := func(operation string) router.Route {
		return router.Route{OperationID: "NATIVE:" + operation, Worker: true}
	}
	rt.Handle(http.MethodGet, transcodeworker.PathConnect, worker("TranscodeWorkerConnect"), hub.ServeHTTP)
	rt.Handle(http.MethodGet, transcodeworker.PathSource, worker("TranscodeWorkerSource"), hub.ServeHTTP)
	rt.Handle(http.MethodPut, transcodeworker.PathFile, worker("TranscodeWorkerFile"), hub.ServeHTTP)
	rt.Handle(http.MethodGet, transcodeworker.PathAsset, worker("TranscodeWorkerAsset"), hub.ServeHTTP)
	rt.Handle(http.MethodPost, transcodeworker.PathOutput, worker("TranscodeWorkerOutput"), hub.ServeHTTP)
}
