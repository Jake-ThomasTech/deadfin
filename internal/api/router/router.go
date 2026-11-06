// SPDX-License-Identifier: GPL-2.0-only

// Package router owns route registration and ASP.NET-compatible
// case-insensitive path matching.
//
// ASP.NET matches routes case-insensitively and real clients depend on it
// (legacy jellyfin-apiclient builds "Users/..." pairs with SDK "/users/..."
// variants). chi matches case-sensitively, so a middleware rewrites literal
// path segments to their canonical registered casing before dispatch.
// Parameter segments pass through untouched — item IDs and filenames are
// never case-mangled.
package router

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Route describes one registered operation; the table feeds both the
// normalizer trie and the future `deadfin routes --json` ledger export.
type Route struct {
	Method      string `json:"Method"`
	Pattern     string `json:"Pattern"`
	OperationID string `json:"OperationId"`         // spec operationId or SHADOW:<...>
	Policy      string `json:"Policy,omitempty"`    // authz policy name (03-api-surface / WP-P1-03)
	Anonymous   bool   `json:"Anonymous,omitempty"` // no auth required
	// PlaySessionCredential lets a request without a credential use the
	// PlaySessionId grant of an earlier authenticated PlaybackInfo for the
	// route's {itemId} (D-54). Policy still applies to the grant's user.
	PlaySessionCredential bool `json:"PlaySessionCredential,omitempty"`
	// OptionalIdentity resolves the credential of a request on an Anonymous
	// route. A valid credential installs its identity; a missing or invalid
	// credential leaves the request anonymous. Upstream resolves claims on
	// every request, so an anonymous action can still read User.GetUserId().
	OptionalIdentity bool `json:"OptionalIdentity,omitempty"`
	// Worker marks a transcode-worker route (/deadfin/v1/workers/, #614).
	// The route authenticates the worker itself: the control handshake
	// proves a join token and the data plane checks a per-job bearer token.
	// User tokens and API keys are never resolved on it, so they authorize
	// nothing there, and a worker credential is unknown to every other route.
	Worker bool `json:"Worker,omitempty"`
	// ScalingCredential also admits a transcode scaling token (#617) on a
	// read-only route, so a metrics scraper or an autoscaler needs no
	// administrator credential. Any other request meets Policy.
	ScalingCredential bool `json:"ScalingCredential,omitempty"`
}

// RouteWrapper applies behavior to an individual registered route using its
// complete ledger metadata.
type RouteWrapper func(Route, http.Handler) http.Handler

// Router wraps chi with a route table and the case normalizer.
type Router struct {
	mux      *chi.Mux
	routes   []Route
	trie     *node
	wrappers []RouteWrapper
}

// New constructs a router. Wrappers apply to each route at registration time,
// after metadata is completed and before the handler is registered.
func New(wrappers ...RouteWrapper) *Router {
	return &Router{
		mux:      chi.NewRouter(),
		trie:     newNode(),
		wrappers: append([]RouteWrapper(nil), wrappers...),
	}
}

// Handle registers a handler with route metadata. Pattern syntax is chi's
// (mid-segment params like "stream.{container}" are supported — the reason
// chi was chosen; see docs/01-architecture.md §HTTP stack).
func (rt *Router) Handle(method, pattern string, meta Route, h http.HandlerFunc) {
	meta.Method = method
	meta.Pattern = pattern
	rt.routes = append(rt.routes, meta)
	rt.trie.insert(pattern)

	var handler http.Handler = h
	for index := len(rt.wrappers) - 1; index >= 0; index-- {
		handler = rt.wrappers[index](meta, handler)
	}
	rt.mux.Method(method, pattern, handler)
}

// Routes returns the registered route table (ledger input).
func (rt *Router) Routes() []Route { return rt.routes }

// ServeHTTP normalizes literal segment casing, applies the CORS default policy,
// then dispatches via chi. CORS runs before routing and before authentication,
// matching upstream's UseCors middleware ordering (Startup.cs:191); see cors.go.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if normalized, changed := rt.trie.normalize(r.URL.Path); changed {
		// Clone the request with the canonical path; RawPath is dropped on
		// purpose (chi routes on the decoded path).
		r2 := r.Clone(r.Context())
		r2.URL.Path = normalized
		r2.URL.RawPath = ""
		r = r2
	}
	if applyCORS(w, r) {
		return
	}
	rt.mux.ServeHTTP(w, r)
}

// --- normalization trie ---

type node struct {
	// literal children keyed by lowercase segment -> canonical casing + child.
	literals map[string]*literalChild
	// mixed/param segment templates at this level (for example, "stream.{container}",
	// "{segmentId}.{container}", "{itemId}").
	templates []*segTemplate
}

type literalChild struct {
	canonical string
	child     *node
}

type segTemplate struct {
	// matcher captures param spans; rebuild re-emits the segment with
	// canonical literal casing around the captured params.
	matcher    *regexp.Regexp
	rebuild    func(params []string) string
	child      *node
	isPure     bool   // pure single-param segment ("{x}"): match anything, keep as-is
	rebuildKey string // the original template, for dedupe on insert
}

func newNode() *node { return &node{literals: map[string]*literalChild{}} }

var paramRe = regexp.MustCompile(`\{[^}]*\}`)

func (n *node) insert(pattern string) {
	cur := n
	for _, seg := range strings.Split(strings.Trim(pattern, "/"), "/") {
		if seg == "" {
			continue
		}
		if !strings.Contains(seg, "{") {
			key := strings.ToLower(seg)
			lc, ok := cur.literals[key]
			if !ok {
				lc = &literalChild{canonical: seg, child: newNode()}
				cur.literals[key] = lc
			}
			cur = lc.child
			continue
		}
		// Param or mixed segment: find an equivalent existing template.
		// Param NAMES are irrelevant to normalization — merge by shape, so
		// "{itemId}" and "{routeItemId}" share one child (otherwise sibling
		// routes with different param names split the trie and segments
		// below the fork stop normalizing).
		tmplKey := paramRe.ReplaceAllString(seg, "{}")
		var found *segTemplate
		for _, t := range cur.templates {
			if t.rebuildKey == tmplKey {
				found = t
				break
			}
		}
		if found == nil {
			found = compileSegTemplate(seg)
			found.rebuildKey = tmplKey // dedupe by shape, not by param name
			cur.templates = append(cur.templates, found)
		}
		cur = found.child
	}
}

// compileSegTemplate builds a case-insensitive matcher for a segment template
// containing params, for example, "stream.{container}" => ^(?i:stream\.)(.+)$ with a
// rebuild that re-emits "stream." canonically around the captured value.
func compileSegTemplate(seg string) *segTemplate {
	t := &segTemplate{child: newNode(), rebuildKey: seg}
	if paramRe.MatchString(seg) && paramRe.FindString(seg) == seg {
		t.isPure = true
		return t
	}
	var (
		pattern  strings.Builder
		literals []string
	)
	pattern.WriteString("^")
	rest := seg
	for {
		loc := paramRe.FindStringIndex(rest)
		if loc == nil {
			literals = append(literals, rest)
			pattern.WriteString("(?i)" + regexp.QuoteMeta(rest))
			break
		}
		lit := rest[:loc[0]]
		literals = append(literals, lit)
		pattern.WriteString("(?i)" + regexp.QuoteMeta(lit))
		pattern.WriteString(`(.+?)`)
		rest = rest[loc[1]:]
		if rest == "" {
			literals = append(literals, "")
			break
		}
	}
	pattern.WriteString("$")
	t.matcher = regexp.MustCompile(pattern.String())
	t.rebuild = func(params []string) string {
		var b strings.Builder
		for i, lit := range literals {
			b.WriteString(lit)
			if i < len(params) {
				b.WriteString(params[i])
			}
		}
		return b.String()
	}
	return t
}

// normalize rewrites literal segments (and literal parts of mixed segments)
// to canonical casing. Unknown paths are returned unchanged.
func (n *node) normalize(path string) (string, bool) {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return path, false
	}
	segs := strings.Split(trimmed, "/")
	out := make([]string, len(segs))
	cur := n
	changed := false
	for i, seg := range segs {
		if cur == nil {
			out[i] = seg
			continue
		}
		if lc, ok := cur.literals[strings.ToLower(seg)]; ok {
			if lc.canonical != seg {
				changed = true
			}
			out[i] = lc.canonical
			cur = lc.child
			continue
		}
		// Try mixed templates, then pure params.
		var matched *segTemplate
		var rebuilt string
		for _, t := range cur.templates {
			if t.isPure {
				continue
			}
			if m := t.matcher.FindStringSubmatch(seg); m != nil {
				matched, rebuilt = t, t.rebuild(m[1:])
				break
			}
		}
		if matched == nil {
			for _, t := range cur.templates {
				if t.isPure {
					matched, rebuilt = t, seg
					break
				}
			}
		}
		if matched == nil {
			// No route knowledge below this point; pass the rest through.
			copy(out[i:], segs[i:])
			cur = nil
			continue
		}
		if rebuilt != seg {
			changed = true
		}
		out[i] = rebuilt
		cur = matched.child
	}
	result := "/" + strings.Join(out, "/")
	if strings.HasSuffix(path, "/") && !strings.HasSuffix(result, "/") {
		result += "/"
	}
	return result, changed
}
