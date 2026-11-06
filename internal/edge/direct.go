// SPDX-License-Identifier: GPL-2.0-only

package edge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/dto"
)

// DirectHeader carries the direct grants of a PlaybackInfo response from
// the server to the edge, one token per media source. The edge removes it
// before the response reaches the client.
const DirectHeader = "X-Deadfin-Edge-Direct"

const defaultMaxLearned = 4096

var (
	// staticPath matches the static stream routes of the server, below any
	// base URL: /videos/{itemId}/stream(.{container}) and the same for audio.
	staticPath = regexp.MustCompile(`(?i)/(?:videos|audio)/([0-9a-f-]{32,36})/stream(?:\.([a-z0-9]{1,8}))?$`)
	// playbackInfoPath matches the PlaybackInfo route of the server.
	playbackInfoPath = regexp.MustCompile(`(?i)/items/([0-9a-f-]{32,36})/playbackinfo$`)
	logoutPath       = regexp.MustCompile(`(?i)/sessions/logout$`)
)

// PathMapping maps a server path prefix to the edge path of the same files.
type PathMapping struct {
	Server string
	Edge   string
}

// library resolves the source file of a direct grant inside the library
// roots that the edge mounts.
type library struct {
	roots    []string
	mappings []PathMapping
}

var errOutsideLibrary = errors.New("path is outside the library roots")

func newLibrary(roots []string, mappings []PathMapping) (library, error) {
	var lib library
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			return library{}, errors.New("edge: library root " + root + " is not absolute")
		}
		resolved, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			return library{}, err
		}
		lib.roots = append(lib.roots, resolved)
	}
	for _, mapping := range mappings {
		if !filepath.IsAbs(mapping.Server) || !filepath.IsAbs(mapping.Edge) {
			return library{}, errors.New("edge: path mapping " + mapping.Server + "=" + mapping.Edge + " is not absolute")
		}
		lib.mappings = append(lib.mappings, PathMapping{Server: filepath.Clean(mapping.Server), Edge: filepath.Clean(mapping.Edge)})
	}
	return lib, nil
}

// resolve maps a server path to the edge, follows its links, and accepts it
// only below a library root.
func (lib library) resolve(serverPath string) (string, error) {
	if len(lib.roots) == 0 || !filepath.IsAbs(serverPath) {
		return "", errOutsideLibrary
	}
	local := filepath.Clean(serverPath)
	for _, mapping := range lib.mappings {
		if within(local, mapping.Server) {
			local = filepath.Join(mapping.Edge, strings.TrimPrefix(local, mapping.Server))
			break
		}
	}
	resolved, err := filepath.EvalSymlinks(local)
	if err != nil {
		return "", err
	}
	for _, root := range lib.roots {
		if within(resolved, root) {
			return resolved, nil
		}
	}
	return "", errOutsideLibrary
}

// within reports whether path is root or below it.
func within(path, root string) bool {
	if root == string(filepath.Separator) {
		return true
	}
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

type learnKey struct {
	// credential is "t:" and the SHA-256 of an access token, or "s:" and a
	// PlaySessionId.
	credential string
	item       dto.UUID
}

type learnEntry struct {
	grants []DirectGrant
	added  time.Time
}

// learned holds the direct grants of the PlaybackInfo responses that passed
// through the edge, so that a static URL that a client builds itself, with
// the same token or PlaySessionId, is served from the library.
type learned struct {
	max int

	mu      sync.Mutex
	entries map[learnKey]learnEntry
}

func newLearned(max int) *learned {
	return &learned{max: max, entries: make(map[learnKey]learnEntry)}
}

func tokenCredential(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "t:" + hex.EncodeToString(sum[:])
}

func (l *learned) put(key learnKey, grants []DirectGrant, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[key] = learnEntry{grants: grants, added: now}
	if len(l.entries) <= l.max {
		return
	}
	for k, entry := range l.entries {
		if !now.Before(latestExpiry(entry.grants)) {
			delete(l.entries, k)
		}
	}
	for len(l.entries) > l.max {
		var oldest learnKey
		var oldestAdded time.Time
		first := true
		for k, entry := range l.entries {
			if first || entry.added.Before(oldestAdded) {
				oldest, oldestAdded, first = k, entry.added, false
			}
		}
		delete(l.entries, oldest)
	}
}

func (l *learned) get(key learnKey) []DirectGrant {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entries[key].grants
}

// forget removes every entry of credential.
func (l *learned) forget(credential string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k := range l.entries {
		if k.credential == credential {
			delete(l.entries, k)
		}
	}
}

func latestExpiry(grants []DirectGrant) time.Time {
	var latest time.Time
	for _, grant := range grants {
		if grant.Expires.After(latest) {
			latest = grant.Expires
		}
	}
	return latest
}

// learnResponse records the direct grants of a PlaybackInfo response and
// removes them from the response. A successful logout forgets the grants of
// its token.
func (e *Edge) learnResponse(response *http.Response) {
	tokens := response.Header.Values(DirectHeader)
	response.Header.Del(DirectHeader)
	request := response.Request
	if request == nil || response.StatusCode < 200 || response.StatusCode > 299 {
		return
	}
	if logoutPath.MatchString(request.URL.Path) {
		if token := auth.Extract(request, true).Token; token != "" {
			e.learned.forget(tokenCredential(token))
		}
		return
	}
	if len(tokens) == 0 || len(e.library.roots) == 0 {
		return
	}
	match := playbackInfoPath.FindStringSubmatch(request.URL.Path)
	if match == nil {
		return
	}
	item, err := dto.ParseUUID(match[1])
	if err != nil {
		return
	}
	now := e.config.Clock.Now()
	var grants []DirectGrant
	for _, value := range tokens {
		for _, token := range strings.Split(value, ",") {
			grant, err := e.config.Signer.VerifyDirect(strings.TrimSpace(token), now)
			if err == nil && grant.ItemID == item {
				grants = append(grants, grant)
			}
		}
	}
	if len(grants) == 0 {
		return
	}
	if token := auth.Extract(request, true).Token; token != "" {
		e.learned.put(learnKey{credential: tokenCredential(token), item: item}, grants, now)
	}
	bySession := make(map[string][]DirectGrant)
	for _, grant := range grants {
		if grant.PlaySessionID != "" {
			bySession[grant.PlaySessionID] = append(bySession[grant.PlaySessionID], grant)
		}
	}
	for playSessionID, sessionGrants := range bySession {
		e.learned.put(learnKey{credential: "s:" + playSessionID, item: item}, sessionGrants, now)
	}
}

// grantedDirect resolves a static stream request to a direct grant: one in
// the DeadfinEdge query parameter, or one learned for the request's token
// or PlaySessionId.
func (e *Edge) grantedDirect(r *http.Request) (DirectGrant, string, bool) {
	if len(e.library.roots) == 0 {
		return DirectGrant{}, "", false
	}
	match := staticPath.FindStringSubmatch(r.URL.Path)
	if match == nil {
		return DirectGrant{}, "", false
	}
	item, err := dto.ParseUUID(match[1])
	if err != nil {
		return DirectGrant{}, "", false
	}
	var static, token, playSessionID, sourceID, liveStreamID string
	for key, values := range r.URL.Query() {
		if len(values) == 0 {
			continue
		}
		switch strings.ToLower(key) {
		case "static":
			static = values[0]
		case strings.ToLower(GrantQuery):
			token = values[0]
		case "playsessionid":
			playSessionID = values[0]
		case "mediasourceid":
			sourceID = values[0]
		case "livestreamid":
			liveStreamID = values[0]
		}
	}
	if !strings.EqualFold(static, "true") || liveStreamID != "" {
		return DirectGrant{}, "", false
	}
	now := e.config.Clock.Now()
	var candidates []DirectGrant
	if token != "" {
		if grant, err := e.config.Signer.VerifyDirect(token, now); err == nil && grant.ItemID == item {
			candidates = []DirectGrant{grant}
		}
	} else {
		if access := auth.Extract(r, true).Token; access != "" {
			candidates = e.learned.get(learnKey{credential: tokenCredential(access), item: item})
		}
		if len(candidates) == 0 && playSessionID != "" {
			candidates = e.learned.get(learnKey{credential: "s:" + playSessionID, item: item})
		}
	}
	grant, ok := selectSource(candidates, item, sourceID, now)
	return grant, strings.ToLower(match[2]), ok
}

// selectSource picks the grant of the requested media source; without one,
// the item's default source, or the only source.
func selectSource(grants []DirectGrant, item dto.UUID, sourceID string, now time.Time) (DirectGrant, bool) {
	var live []DirectGrant
	for _, grant := range grants {
		if now.Before(grant.Expires) {
			live = append(live, grant)
		}
	}
	want := normalizeSourceID(sourceID)
	if want == "" {
		want = normalizeSourceID(item.String())
	}
	for _, grant := range live {
		if normalizeSourceID(grant.MediaSourceID) == want {
			return grant, true
		}
	}
	if sourceID == "" && len(live) == 1 {
		return live[0], true
	}
	return DirectGrant{}, false
}

func normalizeSourceID(id string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
}

// openDirect opens the library file of grant. A file that is missing,
// outside the roots, not regular, or of another size than the grant names
// is not served by the edge.
func (e *Edge) openDirect(grant DirectGrant) (*os.File, os.FileInfo, error) {
	local, err := e.library.resolve(grant.Path)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.Open(local)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || (grant.Size > 0 && info.Size() != grant.Size) {
		_ = file.Close()
		return nil, nil, errors.New("library file does not match the grant")
	}
	return file, info, nil
}

// serveDirect serves a library file by range.
func serveDirect(w http.ResponseWriter, r *http.Request, grant DirectGrant, routeContainer string, file *os.File, info os.FileInfo) {
	defer func() { _ = file.Close() }()
	if ranges := r.Header.Values("Range"); len(ranges) > 1 {
		r.Header.Del("Range")
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", directContentType(routeContainer, grant.Container, grant.Path))
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

// directContentType follows the server's static stream: the route
// container, then the source container, then the file extension.
func directContentType(routeContainer, sourceContainer, path string) string {
	container := routeContainer
	if container == "" {
		container, _, _ = strings.Cut(sourceContainer, ",")
	}
	container = strings.TrimPrefix(strings.TrimSpace(container), ".")
	for _, extension := range []string{container, strings.TrimPrefix(filepath.Ext(path), ".")} {
		if extension == "" {
			continue
		}
		if strings.EqualFold(extension, "flac") {
			return "audio/flac"
		}
		if contentType := mime.TypeByExtension("." + extension); contentType != "" {
			return contentType
		}
	}
	return "application/octet-stream"
}
