// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// nativePrefix marks Deadfin-native routes (docs/08-control-plane.md D13).
// They are exported in the route snapshot but are not parity operations:
// they never enter the operations list, the Swiftfin set, or any count.
const nativePrefix = "NATIVE:"

const nativeOperationPattern = `^NATIVE:[A-Z][A-Za-z0-9]*$`

// ledger is the complete generated output.
type ledger struct {
	Parity    parityDoc
	WebSocket webSocketDoc
	Summary   summaryData
}

type parityDoc struct {
	Source     paritySource `json:"source"`
	Operations []parityOp   `json:"operations"`
}

type paritySource struct {
	DeadfinRevision string `json:"deadfin_revision"`
	JellyfinPin     string `json:"jellyfin_pin"`
	RouteCount      int    `json:"route_count"`
	OpenAPIVersion  string `json:"openapi_version"`
	// PinReset records an upstream pin reset that cleared ratchets.
	// Populated only when a pin change triggered a reset.
	PinReset *pinResetRecord `json:"pin_reset,omitempty"`
}

type pinResetRecord struct {
	OldPin string `json:"old_pin"`
	NewPin string `json:"new_pin"`
	// Operation is the operation ID that triggered the reset.
	Operation string `json:"operation"`
}

type parityOp struct {
	ID          string   `json:"id"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Subsystem   string   `json:"subsystem"`
	Status      string   `json:"status"`
	Disposition string   `json:"disposition,omitempty"`
	Registered  bool     `json:"registered"`
	Shadow      bool     `json:"shadow"`
	Clients     []string `json:"clients"`
	Reason      string   `json:"reason,omitempty"`
}

type webSocketDoc struct {
	Source   webSocketSource  `json:"source"`
	Messages []webSocketEntry `json:"messages"`
}

type webSocketSource struct {
	JellyfinPin      string `json:"jellyfin_pin"`
	UpstreamCount    int    `json:"upstream_count"`
	ImplementedCount int    `json:"implemented_count"`
}

type webSocketEntry struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Deadfin  bool   `json:"deadfin"`
}
type summaryData struct {
	SwiftfinDone         int
	SwiftfinTotal        int
	SwiftfinPercent      float64
	SwiftfinConveniences []string
	TagCounts            map[string]statusCounts
	SubsystemCounts      map[string]statusCounts
	Unimplemented        []unimplRow
	WebSocketDone        int
	WebSocketTotal       int
	ShadowCoverage       shadowCoverage
	Diagnostics          inputDiagnostics
}

type inputDiagnostics struct {
	OpenAPICount     int
	ShadowCount      int
	RouteCount       int
	NativeRoutes     int
	DuplicateIDs     int
	UnknownRoutes    int
	UnknownResults   int
	MethodMismatches int
	PathMismatches   int
}

type statusCounts struct {
	Done     int
	Partial  int
	Stubbed  int
	Deferred int
	Todo     int
	Total    int
}

type unimplRow struct {
	Subsystem string
	ID        string
	Status    string
	Reason    string
}

type shadowCoverage struct {
	Total      int
	Registered int
}

// buildLedger merges all inputs into the generated output.
func buildLedger(in *inputs) (*ledger, error) {
	// Validate inputs and collect diagnostics.
	diag, err := validateInputs(in)
	if err != nil {
		return nil, err
	}

	// Build route lookup: operation ID → route.
	routeMap := make(map[string]snapshotRoute)
	for _, r := range in.Routes {
		routeMap[r.OperationID] = r
	}

	// Build result lookup: operation ID → all steps.
	resultMap := make(map[string][]resultStep)
	for _, steps := range in.Results {
		for _, step := range steps {
			resultMap[step.OperationID] = append(resultMap[step.OperationID], step)
		}
	}

	// Build shadow lookup: operation ID → shadow operation.
	shadowMap := make(map[string]shadowOperation)
	for _, so := range in.Shadow.Operations {
		shadowMap[so.ID] = so
	}

	// Validate: every SHADOW: route must exist in shadow spec.
	for _, r := range in.Routes {
		if strings.HasPrefix(r.OperationID, "SHADOW:") {
			if _, ok := shadowMap[r.OperationID]; !ok {
				return nil, fmt.Errorf("route %q is a SHADOW route but is missing from shadow-spec.yaml", r.OperationID)
			}
		}
	}

	// Build the operations list from OpenAPI + shadow spec.
	var ops []parityOp

	// OpenAPI operations.
	for _, oa := range in.OpenAPI {
		op := buildOpenAIOp(oa, routeMap, resultMap, in.Annotations.Operations)
		ops = append(ops, op)
	}

	// Shadow operations (not in OpenAPI).
	for _, so := range in.Shadow.Operations {
		if isOpenAIOp(so.ID, in.OpenAPI) {
			continue // Already added from OpenAPI
		}
		op := buildShadowOp(so, routeMap, resultMap, in.Annotations.Operations)
		ops = append(ops, op)
	}

	// Sort by operation ID for stable output.
	sort.Slice(ops, func(i, j int) bool {
		return ops[i].ID < ops[j].ID
	})
	// Ensure all Clients fields are non-nil so JSON serializes [] not null.
	for i := range ops {
		if ops[i].Clients == nil {
			ops[i].Clients = []string{}
		}
	}

	// Validate Swiftfin membership: must contain exactly 95 distinct operations.
	swiftfinOps := filterSwiftfin(ops)
	if len(swiftfinOps) != swiftfinDenominator {
		return nil, fmt.Errorf("Swiftfin REST set contains %d operations; expected exactly %d", len(swiftfinOps), swiftfinDenominator)
	}

	// Apply ratchet checks with pin-reset enforcement.
	pinReset, err := checkRatchets(ops, resultMap, in.Annotations)
	if err != nil {
		return nil, err
	}

	// Build WebSocket entries.
	wsDoc, err := buildWebSocket(in)
	if err != nil {
		return nil, err
	}

	// Build summary.
	summary := buildSummary(in, ops, wsDoc)
	summary.Diagnostics = diag

	source := paritySource{
		DeadfinRevision: in.RouteRevision,
		JellyfinPin:     in.Annotations.Source.JellyfinPin,
		RouteCount:      len(in.Routes),
		OpenAPIVersion:  "12.0.0",
	}
	if pinReset != nil {
		source.PinReset = pinReset
	}

	return &ledger{
		Parity: parityDoc{
			Source:     source,
			Operations: ops,
		},
		WebSocket: *wsDoc,
		Summary:   summary,
	}, nil
}

// validateInputs checks the committed inputs for stale or incomplete inventories.
// It rejects duplicate operation IDs across the complete OpenAPI and shadow
// union, unknown exported routes, unknown result operations, and method or
// path mismatches between the OpenAPI spec, shadow spec, route snapshot, and
// difftool results. It returns diagnostic counts for the generated report.
func validateInputs(in *inputs) (inputDiagnostics, error) {
	var diag inputDiagnostics
	diag.OpenAPICount = len(in.OpenAPI)
	diag.ShadowCount = len(in.Shadow.Operations)
	diag.RouteCount = len(in.Routes)

	// Reject duplicate operation IDs in the OpenAPI spec.
	seenOpenAPI := make(map[string]bool)
	for _, oa := range in.OpenAPI {
		if seenOpenAPI[oa.OperationID] {
			diag.DuplicateIDs++
			return diag, fmt.Errorf("duplicate operation ID %q in OpenAPI spec", oa.OperationID)
		}
		seenOpenAPI[oa.OperationID] = true
	}

	// Reject duplicate operation IDs in the route snapshot.
	seenRoute := make(map[string]bool)
	for _, r := range in.Routes {
		if seenRoute[r.OperationID] {
			return diag, fmt.Errorf("duplicate route operation ID %q in deadfin-routes.json", r.OperationID)
		}
		seenRoute[r.OperationID] = true
	}

	// Reject duplicate IDs in the shadow spec.
	seenShadow := make(map[string]bool)
	for _, so := range in.Shadow.Operations {
		if seenShadow[so.ID] {
			diag.DuplicateIDs++
			return diag, fmt.Errorf("duplicate shadow operation ID %q in shadow-spec.yaml", so.ID)
		}
		seenShadow[so.ID] = true
	}

	// Reject cross-input duplicate IDs: an ID that occurs in both OpenAPI and shadow.
	for id := range seenShadow {
		if seenOpenAPI[id] {
			diag.DuplicateIDs++
			return diag, fmt.Errorf("duplicate operation ID %q appears in both OpenAPI spec and shadow spec", id)
		}
	}

	// Build a set of all known operation IDs: OpenAPI + shadow.
	knownOps := make(map[string]bool)
	for _, oa := range in.OpenAPI {
		knownOps[oa.OperationID] = true
	}
	for _, so := range in.Shadow.Operations {
		knownOps[so.ID] = true
	}

	// Native routes form their own exact class: the ID must match the
	// native pattern, and it is never a parity operation.
	for _, r := range in.Routes {
		if !strings.HasPrefix(r.OperationID, nativePrefix) {
			continue
		}
		if !regexp.MustCompile(nativeOperationPattern).MatchString(r.OperationID) {
			return diag, fmt.Errorf("route %q uses the NATIVE: prefix but is not a valid native operation ID", r.OperationID)
		}
		diag.NativeRoutes++
	}

	// Reject unknown exported routes: routes that match no known operation.
	for _, r := range in.Routes {
		if strings.HasPrefix(r.OperationID, nativePrefix) {
			continue
		}
		if !knownOps[r.OperationID] {
			diag.UnknownRoutes++
			return diag, fmt.Errorf("route %q in deadfin-routes.json has no matching operation in the OpenAPI spec or shadow spec", r.OperationID)
		}
	}

	// Reject unknown result operations: difftool results that match no known operation.
	for _, steps := range in.Results {
		for _, step := range steps {
			if strings.HasPrefix(step.OperationID, nativePrefix) {
				diag.UnknownResults++
				return diag, fmt.Errorf("difftool result %q names a native operation; native routes are not parity operations", step.OperationID)
			}
			if !knownOps[step.OperationID] {
				diag.UnknownResults++
				return diag, fmt.Errorf("difftool result operation %q has no matching operation in the OpenAPI spec or shadow spec", step.OperationID)
			}
		}
	}

	// Reject method or path mismatches between the OpenAPI spec and the route snapshot.
	routeByID := make(map[string]snapshotRoute)
	for _, r := range in.Routes {
		routeByID[r.OperationID] = r
	}
	for _, oa := range in.OpenAPI {
		if r, ok := routeByID[oa.OperationID]; ok {
			if strings.ToUpper(r.Method) != strings.ToUpper(oa.Method) {
				diag.MethodMismatches++
				return diag, fmt.Errorf("operation %q: method mismatch (OpenAPI %s, route %s)", oa.OperationID, oa.Method, r.Method)
			}
			if r.Pattern != oa.Path {
				diag.PathMismatches++
				return diag, fmt.Errorf("operation %q: path mismatch (OpenAPI %s, route %s)", oa.OperationID, oa.Path, r.Pattern)
			}
		}
	}

	// Reject method or path mismatches between the shadow spec and the route snapshot.
	for _, so := range in.Shadow.Operations {
		if r, ok := routeByID[so.ID]; ok {
			if strings.ToUpper(r.Method) != strings.ToUpper(so.Method) {
				diag.MethodMismatches++
				return diag, fmt.Errorf("shadow operation %q: method mismatch (shadow %s, route %s)", so.ID, so.Method, r.Method)
			}
			if r.Pattern != so.Path {
				diag.PathMismatches++
				return diag, fmt.Errorf("shadow operation %q: path mismatch (shadow %s, route %s)", so.ID, so.Path, r.Pattern)
			}
		}
	}

	return diag, nil
}

func isOpenAIOp(id string, openAPI []openAPIOperation) bool {
	for _, oa := range openAPI {
		if oa.OperationID == id {
			return true
		}
	}
	return false
}

func buildOpenAIOp(oa openAPIOperation, routes map[string]snapshotRoute, results map[string][]resultStep, annotations map[string]opAnnotation) parityOp {
	op := parityOp{
		ID:        oa.OperationID,
		Method:    oa.Method,
		Path:      oa.Path,
		Subsystem: oa.Tag,
		Clients:   []string{},
	}

	_, registered := routes[oa.OperationID]
	op.Registered = registered

	// Check for annotation.
	ann, hasAnn := annotations[oa.OperationID]

	// Step 1: deferred annotation with no route.
	if hasAnn && ann.Status == "deferred" && !registered {
		op.Status = "deferred"
		op.Disposition = "deferred"
		op.Reason = ann.Reason
		op.Clients = ann.Clients
		return op
	}

	// Step 2: registered intentional stub (LiveTv/SyncPlay). An
	// implemented annotation marks a route of those subsystems that serves
	// real behavior; it continues to the evidence steps.
	if registered && isStubSubystem(oa.Tag) && !(hasAnn && ann.Status == "implemented") {
		op.Status = "stubbed"
		op.Disposition = "deferred"
		op.Reason = "intentional stub per D2"
		op.Clients = ann.Clients
		return op
	}

	// Step 3: missing unannotated route produces todo.
	if !registered {
		op.Status = "todo"
		op.Clients = ann.Clients
		return op
	}

	// Steps 4-6: registered route — check corpus evidence.
	steps, hasEvidence := results[oa.OperationID]

	if !hasEvidence {
		// Step 4: registered route without corpus evidence produces partial.
		op.Status = "partial"
		if hasAnn {
			op.Reason = ann.Reason
			op.Clients = ann.Clients
		}
		return op
	}

	// Step 5: registered route with any failed step produces partial.
	// Step 6: registered route with evidence produces done when every step passes.
	allPass := true
	for _, step := range steps {
		if step.Status != "pass" {
			allPass = false
			break
		}
	}

	if allPass {
		op.Status = "done"
	} else {
		op.Status = "partial"
	}

	if hasAnn {
		op.Reason = ann.Reason
		op.Clients = ann.Clients
	}

	return op
}

func buildShadowOp(so shadowOperation, routes map[string]snapshotRoute, results map[string][]resultStep, annotations map[string]opAnnotation) parityOp {
	// Only include swiftfin in Clients when the swiftfin REST flag is set.
	// Shadow routes like HLS playlists are used by Swiftfin but are not
	// part of the 95-operation Paths.* REST set.
	clients := so.Clients
	if !so.Swiftfin {
		filtered := make([]string, 0, len(clients))
		for _, c := range clients {
			if c != "swiftfin" {
				filtered = append(filtered, c)
			}
		}
		clients = filtered
	}

	op := parityOp{
		ID:        so.ID,
		Method:    so.Method,
		Path:      so.Path,
		Subsystem: so.Subsystem,
		Shadow:    true,
		Clients:   clients,
	}

	_, registered := routes[so.ID]
	op.Registered = registered

	ann, hasAnn := annotations[so.ID]

	// Step 1: deferred annotation with no route produces deferred.
	if hasAnn && ann.Status == "deferred" && !registered {
		op.Status = "deferred"
		op.Disposition = "deferred"
		op.Reason = ann.Reason
		return op
	}

	// Step 3: missing unannotated route produces todo.
	if !registered {
		op.Status = "todo"
		if hasAnn {
			op.Reason = ann.Reason
		}
		return op
	}

	steps, hasEvidence := results[so.ID]

	if !hasEvidence {
		op.Status = "partial"
		if hasAnn {
			op.Reason = ann.Reason
		}
		return op
	}

	allPass := true
	for _, step := range steps {
		if step.Status != "pass" {
			allPass = false
			break
		}
	}

	if allPass {
		op.Status = "done"
	} else {
		op.Status = "partial"
	}

	if hasAnn {
		op.Reason = ann.Reason
	}

	return op
}

// isStubSubystem returns true for LiveTv and SyncPlay, which are registered
// as intentional stubs per decision D2.
func isStubSubystem(tag string) bool {
	return tag == "LiveTv" || tag == "SyncPlay"
}

// filterSwiftfin returns operations marked with swiftfin client membership.
func filterSwiftfin(ops []parityOp) []parityOp {
	var result []parityOp
	for _, op := range ops {
		for _, c := range op.Clients {
			if c == "swiftfin" {
				result = append(result, op)
				break
			}
		}
	}
	return result
}

// checkRatchets fails generation if a ratcheted done row no longer supports
// done. A ratchet annotation marks a row that was previously done. If the
// current evidence does not support done, generation fails unless the
// annotation has pin-reset: true AND the current source jellyfin_pin differs
// from the previously recorded pin. When a valid pin reset clears ratchets,
// the function returns a pinResetRecord for the generated source metadata.
//
// A pin-reset annotation is valid only on a ratcheted row. A non-ratcheted
// pin-reset fails generation because the reset has no ratchet to clear.
func checkRatchets(ops []parityOp, results map[string][]resultStep, anns annotations) (*pinResetRecord, error) {
	var anyRatchet bool
	var anyPinReset bool
	var recordedPin string
	var resetOpID string
	for _, op := range ops {
		ann, hasAnn := anns.Operations[op.ID]
		if !hasAnn {
			continue
		}

		// A pin-reset annotation is valid only on a ratcheted row.
		// A non-ratcheted pin-reset fails generation because the reset
		// has no ratchet to clear.
		if ann.PinReset && !ann.Ratchet {
			return nil, fmt.Errorf("operation %q has pin-reset: true but is not ratcheted; pin-reset is valid only on a ratcheted row", op.ID)
		}

		if !ann.Ratchet {
			continue
		}
		anyRatchet = true

		// A pin-reset clears the ratchet only when the current pin changed.
		// This check applies to every ratcheted row, including done rows.
		// A done row with an unchanged pin-reset must fail generation.
		if ann.PinReset {
			if anns.Source.JellyfinPin == "" || ann.RecordedPin == "" {
				return nil, fmt.Errorf("ratcheted operation %q has pin-reset: true but the recorded pin or current pin is empty", op.ID)
			}
			if anns.Source.JellyfinPin == ann.RecordedPin {
				return nil, fmt.Errorf("ratcheted operation %q has pin-reset: true but the current pin %q equals the recorded pin; a pin reset requires an actual pin change", op.ID, anns.Source.JellyfinPin)
			}
			anyPinReset = true
			if recordedPin == "" {
				recordedPin = ann.RecordedPin
				resetOpID = op.ID
			}
			continue
		}

		// A ratcheted done row without pin-reset still supports done.
		if op.Status == "done" {
			continue
		}

		// Evidence regression: the ratcheted row no longer supports done.
		return nil, fmt.Errorf("ratcheted operation %q no longer supports done (current status: %s); if this is an intentional upstream pin reset, set pin-reset: true and recorded_pin in annotations.yaml", op.ID, op.Status)
	}

	// Record the pin reset in source metadata only when a ratcheted row
	// was cleared by an actual pin change. The operation field records
	// the operation ID that triggered the reset, not a descriptive phrase.
	if anyRatchet && anyPinReset {
		return &pinResetRecord{
			OldPin:    recordedPin,
			NewPin:    anns.Source.JellyfinPin,
			Operation: resetOpID,
		}, nil
	}

	return nil, nil
}

// buildWebSocket creates the WebSocket document from upstream types and annotations.
// It rejects a parsed type count other than 34, extra WebSocket annotation keys,
// and missing annotations. Each parsed type must have exactly one annotation.
func buildWebSocket(in *inputs) (*webSocketDoc, error) {
	// Parse the 34 upstream WebSocket message types from SessionMessageType.cs.
	types, err := parseSessionMessageTypes(in.Root)
	if err != nil {
		return nil, err
	}

	// Reject a parsed type count other than 34.
	if len(types) != websocketDenominator {
		return nil, fmt.Errorf("parsed %d WebSocket message types from SessionMessageType.cs; expected exactly %d", len(types), websocketDenominator)
	}

	// Build a set of parsed type names.
	parsedNames := make(map[string]bool, len(types))
	for _, name := range types {
		parsedNames[name] = true
	}

	// Reject extra WebSocket annotation keys: annotations for types that do
	// not appear in the parsed enum.
	for name := range in.Annotations.WebSocket {
		if !parsedNames[name] {
			return nil, fmt.Errorf("WebSocket annotation %q does not match a parsed SessionMessageType", name)
		}
	}

	var entries []webSocketEntry
	implementedCount := 0

	for _, name := range types {
		ann, hasAnn := in.Annotations.WebSocket[name]

		entry := webSocketEntry{
			Name: name,
		}

		if hasAnn {
			entry.Status = ann.Status
			entry.Reason = ann.Reason
			entry.Evidence = ann.Evidence
			entry.Deadfin = ann.Status != "todo"

			// A done annotation without implementation evidence downgrades
			// to partial. The spec says a message without evidence remains
			// todo or partial, not done.
			if ann.Status == "done" && strings.TrimSpace(ann.Evidence) == "" {
				entry.Status = "partial"
				entry.Deadfin = true
				if entry.Reason == "" {
					entry.Reason = "done annotation lacks implementation evidence"
				}
			}

			if entry.Status == "done" || entry.Status == "partial" || entry.Status == "stubbed" {
				implementedCount++
			}
		} else {
			// Each parsed type must have exactly one annotation.
			return nil, fmt.Errorf("WebSocket message type %q has no annotation in annotations.yaml", name)
		}

		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})

	return &webSocketDoc{
		Source: webSocketSource{
			JellyfinPin:      in.Annotations.Source.JellyfinPin,
			UpstreamCount:    websocketDenominator,
			ImplementedCount: implementedCount,
		},
		Messages: entries,
	}, nil
}
