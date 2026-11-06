// SPDX-License-Identifier: GPL-2.0-only

package edge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
)

const movieBody = "0123456789abcdefghij"

// directRig is an edge with a media library in front of a server whose
// PlaybackInfo responses carry direct grants.
type directRig struct {
	edge    *Edge
	front   *httptest.Server
	signer  *Signer
	clock   *clock.Fake
	item    dto.UUID
	root    string
	movie   string
	down    atomic.Bool
	hits    atomic.Int32
	grants  atomic.Value // []string for the next PlaybackInfo response
	logouts atomic.Int32
}

func newDirectRig(t *testing.T, roots bool) *directRig {
	t.Helper()
	before := goleak.IgnoreCurrent()
	t.Cleanup(func() {
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		goleak.VerifyNone(t, before)
	})
	rig := &directRig{signer: testSigner(t, "shared-edge-key-0123456789"), clock: clock.NewFake(testEpoch), item: dto.UUID{0xcd, 0x02}}
	rig.root = filepath.Join(t.TempDir(), "media")
	rig.movie = filepath.Join(rig.root, "movies", "Film (2020)", "film.mkv")
	if err := os.MkdirAll(filepath.Dir(rig.movie), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rig.movie, []byte(movieBody), 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.hits.Add(1)
		if rig.down.Load() {
			http.Error(w, "restarting", http.StatusBadGateway)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/PlaybackInfo"):
			if grants, ok := rig.grants.Load().([]string); ok {
				for _, grant := range grants {
					w.Header().Add(DirectHeader, grant)
				}
			}
			_, _ = io.WriteString(w, `{"MediaSources":[]}`)
		case strings.HasSuffix(r.URL.Path, "/Sessions/Logout"):
			rig.logouts.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = io.WriteString(w, "server:"+r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	upstream, _ := url.Parse(server.URL)
	config := Config{Signer: rig.signer, Upstream: upstream, StoreDirectory: t.TempDir(), Clock: rig.clock}
	if roots {
		config.LibraryRoots = []string{rig.root}
	}
	e, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	rig.edge = e
	rig.front = httptest.NewServer(e)
	t.Cleanup(func() {
		rig.front.Close()
		e.Close()
	})
	return rig
}

func (rig *directRig) grant(path string, size int64, playSessionID string) string {
	return rig.signer.SignDirect(DirectGrant{ItemID: rig.item, MediaSourceID: rig.item.String(), PlaySessionID: playSessionID,
		Path: path, Size: size, Container: "mkv", Expires: testEpoch.Add(time.Hour)})
}

func (rig *directRig) do(t *testing.T, method, path string, header http.Header) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(method, rig.front.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, values := range header {
		request.Header[key] = values
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	return response, string(body)
}

func (rig *directRig) streamPath(query string) string {
	return "/Videos/" + rig.item.String() + "/stream.mkv?" + query
}

// fromLibrary asserts that a request was served from the library, with no
// server request.
func (rig *directRig) fromLibrary(t *testing.T, path string, header http.Header, want string) {
	t.Helper()
	hits := rig.hits.Load()
	response, body := rig.do(t, http.MethodGet, path, header)
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent || body != want {
		t.Fatalf("GET %s = %d %q, want the library bytes %q", path, response.StatusCode, body, want)
	}
	if rig.hits.Load() != hits {
		t.Fatalf("GET %s reached the server", path)
	}
}

// fromServer asserts that a request was forwarded to the server.
func (rig *directRig) fromServer(t *testing.T, path string, header http.Header) {
	t.Helper()
	hits := rig.hits.Load()
	_, body := rig.do(t, http.MethodGet, path, header)
	if !strings.HasPrefix(body, "server:") || rig.hits.Load() != hits+1 {
		t.Fatalf("GET %s = %q, want the server's answer", path, body)
	}
}

// TestEdgeServesARangedStaticStreamWithAGrant: a static stream request with
// a valid direct grant is served from the library by range, also while the
// server does not answer.
func TestEdgeServesARangedStaticStreamWithAGrant(t *testing.T) {
	rig := newDirectRig(t, true)
	grant := url.QueryEscape(rig.grant(rig.movie, int64(len(movieBody)), "play-1"))
	response, body := rig.do(t, http.MethodGet, rig.streamPath("Static=true&DeadfinEdge="+grant), http.Header{"Range": {"bytes=2-5"}})
	if response.StatusCode != http.StatusPartialContent || body != "2345" {
		t.Fatalf("ranged GET = %d %q, want 206 \"2345\"", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Range"); got != "bytes 2-5/20" {
		t.Fatalf("Content-Range = %q", got)
	}
	if got := response.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q", got)
	}
	if rig.hits.Load() != 0 {
		t.Fatal("the granted request reached the server")
	}
	rig.down.Store(true)
	rig.fromLibrary(t, rig.streamPath("static=true&mediaSourceId="+rig.item.String()+"&DeadfinEdge="+grant), nil, movieBody)
	rig.fromLibrary(t, "/Audio/"+rig.item.String()+"/stream?static=true&DeadfinEdge="+grant, nil, movieBody)
}

// TestEdgeRefusesInvalidDirectGrants: an expired, foreign, or mismatched
// grant, and a request that is not static, go to the server.
func TestEdgeRefusesInvalidDirectGrants(t *testing.T) {
	rig := newDirectRig(t, true)
	size := int64(len(movieBody))
	foreign := testSigner(t, "another-edge-key-0123456789")
	other := dto.UUID{0xee}
	cases := map[string]string{
		"no grant":      rig.streamPath("static=true"),
		"not static":    rig.streamPath("DeadfinEdge=" + url.QueryEscape(rig.grant(rig.movie, size, ""))),
		"live stream":   rig.streamPath("static=true&LiveStreamId=x&DeadfinEdge=" + url.QueryEscape(rig.grant(rig.movie, size, ""))),
		"foreign key":   rig.streamPath("static=true&DeadfinEdge=" + url.QueryEscape(foreign.SignDirect(DirectGrant{ItemID: rig.item, MediaSourceID: rig.item.String(), Path: rig.movie, Expires: testEpoch.Add(time.Hour)}))),
		"other item":    rig.streamPath("static=true&DeadfinEdge=" + url.QueryEscape(rig.signer.SignDirect(DirectGrant{ItemID: other, MediaSourceID: other.String(), Path: rig.movie, Expires: testEpoch.Add(time.Hour)}))),
		"other source":  rig.streamPath("static=true&mediaSourceId=ffff&DeadfinEdge=" + url.QueryEscape(rig.grant(rig.movie, size, ""))),
		"size mismatch": rig.streamPath("static=true&DeadfinEdge=" + url.QueryEscape(rig.grant(rig.movie, size+1, ""))),
		"read grant":    rig.streamPath("static=true&DeadfinEdge=" + url.QueryEscape(rig.signer.SignRead(ReadGrant{ItemID: rig.item, Family: Family{Stem: "a", Pattern: "a%d.ts"}, Expires: testEpoch.Add(time.Hour)}))),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) { rig.fromServer(t, path, nil) })
	}
	valid := rig.streamPath("static=true&DeadfinEdge=" + url.QueryEscape(rig.grant(rig.movie, size, "")))
	rig.fromLibrary(t, valid, nil, movieBody)
	rig.clock.Advance(time.Hour)
	rig.fromServer(t, valid, nil)
}

// TestEdgeRefusesPathsOutsideTheLibrary: a grant cannot name a file outside
// the library roots, by a dot-dot path or by a link.
func TestEdgeRefusesPathsOutsideTheLibrary(t *testing.T) {
	rig := newDirectRig(t, true)
	outside := filepath.Join(filepath.Dir(rig.root), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(rig.root, "movies", "link.mkv")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"outside":    outside,
		"dot-dot":    filepath.Join(rig.root, "movies") + "/../../secret.txt",
		"link":       link,
		"directory":  filepath.Join(rig.root, "movies"),
		"missing":    filepath.Join(rig.root, "missing.mkv"),
		"root alias": rig.root + "-other/film.mkv",
	} {
		t.Run(name, func(t *testing.T) {
			rig.fromServer(t, rig.streamPath("static=true&DeadfinEdge="+url.QueryEscape(rig.grant(path, 0, ""))), nil)
		})
	}
}

// TestEdgeServesNoDirectStreamsWithoutLibraryRoots: with no roots, every
// static stream request goes to the server.
func TestEdgeServesNoDirectStreamsWithoutLibraryRoots(t *testing.T) {
	rig := newDirectRig(t, false)
	rig.fromServer(t, rig.streamPath("static=true&DeadfinEdge="+url.QueryEscape(rig.grant(rig.movie, 0, ""))), nil)
}

// TestEdgeLearnsDirectGrantsFromPlaybackInfo: the edge removes the direct
// grants from a PlaybackInfo response and serves the static URL that a
// client builds itself with the same token or PlaySessionId. A logout
// forgets the token.
func TestEdgeLearnsDirectGrantsFromPlaybackInfo(t *testing.T) {
	rig := newDirectRig(t, true)
	rig.grants.Store([]string{rig.grant(rig.movie, int64(len(movieBody)), "play-7"), rig.grant("/elsewhere/x.mkv", 0, "play-7") + ",junk"})
	authorization := http.Header{"Authorization": {`MediaBrowser Client="Infuse", Token="user-token"`}}
	response, _ := rig.do(t, http.MethodPost, "/Items/"+rig.item.String()+"/PlaybackInfo?UserId=u", authorization)
	if response.StatusCode != http.StatusOK || response.Header.Get(DirectHeader) != "" {
		t.Fatalf("PlaybackInfo = %d with %s %q; the edge must remove the header", response.StatusCode, DirectHeader, response.Header.Get(DirectHeader))
	}
	rig.fromLibrary(t, rig.streamPath("Static=true&mediaSourceId="+strings.ReplaceAll(rig.item.String(), "-", "")+"&api_key=user-token"), nil, movieBody)
	rig.fromLibrary(t, rig.streamPath("static=true"), authorization, movieBody)
	rig.fromLibrary(t, rig.streamPath("static=true&PlaySessionId=play-7"), nil, movieBody)
	rig.fromServer(t, rig.streamPath("static=true&api_key=other-token"), nil)
	rig.fromServer(t, rig.streamPath("static=true&PlaySessionId=play-8"), nil)
	rig.fromServer(t, "/Videos/"+dto.UUID{0x99}.String()+"/stream?static=true&api_key=user-token", nil)

	rig.down.Store(true)
	rig.fromLibrary(t, rig.streamPath("static=true&api_key=user-token"), nil, movieBody)
	rig.down.Store(false)

	if response, _ := rig.do(t, http.MethodPost, "/Sessions/Logout", authorization); response.StatusCode != http.StatusNoContent || rig.logouts.Load() != 1 {
		t.Fatalf("logout = %d", response.StatusCode)
	}
	rig.fromServer(t, rig.streamPath("static=true&api_key=user-token"), nil)
}

func TestLearnedGrantsAreBounded(t *testing.T) {
	l := newLearned(2)
	grant := []DirectGrant{{Expires: testEpoch.Add(time.Hour)}}
	for index := range 4 {
		l.put(learnKey{credential: "s:" + string(rune('a'+index))}, grant, testEpoch.Add(time.Duration(index)*time.Second))
	}
	if len(l.entries) != 2 || l.get(learnKey{credential: "s:a"}) != nil || l.get(learnKey{credential: "s:d"}) == nil {
		t.Fatalf("entries = %v, want the two newest", l.entries)
	}
}

func TestLibraryResolvesMappedPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.mkv"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := newLibrary([]string{root}, []PathMapping{{Server: "/media", Edge: root}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := lib.resolve("/media/a.mkv")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(root, "a.mkv"))
	if resolved != want {
		t.Fatalf("resolve = %q, want %q", resolved, want)
	}
	if _, err := lib.resolve("/media/../etc/passwd"); err == nil {
		t.Fatal("a dot-dot path resolved")
	}
	if _, err := lib.resolve("/mediaX/a.mkv"); err == nil {
		t.Fatal("a prefix that is not a path component was mapped")
	}
	if _, err := newLibrary([]string{"relative"}, nil); err == nil {
		t.Fatal("a relative root was accepted")
	}
}
