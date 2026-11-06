// SPDX-License-Identifier: GPL-2.0-only

package edge

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/hls"
)

// PathUpload is the edge route a worker mirrors a family file to.
const PathUpload = "/deadfin/v1/edge/files/"

const (
	defaultWaitTimeout   = 15 * time.Second
	defaultTTL           = time.Hour
	defaultMaxBytes      = 20 << 30
	defaultMaxFileBytes  = 512 << 20
	defaultSweepInterval = time.Minute
)

// segmentPath matches an HLS segment route of the server, below any base
// URL: /videos/{itemId}/hls1/{playlistId}/{segmentId}.{container}, and the
// same for audio.
var segmentPath = regexp.MustCompile(`(?i)/(?:videos|audio)/([0-9a-f-]{32,36})/hls1/[^/]+/(-?[0-9]{1,9})\.([a-z0-9]{1,8})$`)

// Config configures an Edge.
type Config struct {
	Signer *Signer
	// Upstream is the server's base URL.
	Upstream *url.URL
	// StoreDirectory holds the mirrored family files.
	StoreDirectory string
	Clock          clock.Clock
	// WaitTimeout is how long a segment request with a valid grant waits
	// for the segment while the server does not answer.
	WaitTimeout time.Duration
	// TTL removes a file this long after its upload; MaxBytes bounds the
	// store, oldest files first. SweepInterval is the period of both.
	TTL           time.Duration
	MaxBytes      int64
	MaxFileBytes  int64
	SweepInterval time.Duration
	// Transport reaches the server; nil uses http.DefaultTransport.
	Transport http.RoundTripper
	Logger    *slog.Logger
	// LibraryRoots are the media library directories the edge mounts
	// read-only (#621). With none, the edge serves no direct streams.
	LibraryRoots []string
	// PathMappings map the server's library paths to the edge's mounts.
	PathMappings []PathMapping
	// MaxLearned bounds the direct grants learned from PlaybackInfo.
	MaxLearned int
}

// Edge is the stream edge handler. It owns one sweep goroutine; Close
// stops it.
type Edge struct {
	config  Config
	store   *segmentStore
	library library
	learned *learned
	proxy   *httputil.ReverseProxy
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
}

type fallbackKey struct{}

// fallback is the stored segment a request falls back to while the server
// does not answer.
type fallback struct {
	stem, name, container string
}

var errUpstreamUnavailable = errors.New("server unavailable")

// New constructs an Edge and starts its sweep.
func New(config Config) (*Edge, error) {
	if config.Signer == nil || config.Upstream == nil || config.StoreDirectory == "" || config.Clock == nil {
		return nil, errors.New("edge: signer, upstream, store directory and clock are required")
	}
	if config.WaitTimeout <= 0 {
		config.WaitTimeout = defaultWaitTimeout
	}
	if config.TTL <= 0 {
		config.TTL = defaultTTL
	}
	if config.MaxBytes <= 0 {
		config.MaxBytes = defaultMaxBytes
	}
	if config.MaxFileBytes <= 0 {
		config.MaxFileBytes = defaultMaxFileBytes
	}
	if config.SweepInterval <= 0 {
		config.SweepInterval = defaultSweepInterval
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	if config.MaxLearned <= 0 {
		config.MaxLearned = defaultMaxLearned
	}
	lib, err := newLibrary(config.LibraryRoots, config.PathMappings)
	if err != nil {
		return nil, err
	}
	store, err := newSegmentStore(config.StoreDirectory)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Edge{config: config, store: store, library: lib, learned: newLearned(config.MaxLearned), cancel: cancel, done: make(chan struct{})}
	upstream := config.Upstream
	e.proxy = &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(upstream)
			request.SetXForwarded()
			if forwarded := request.In.Header.Get("X-Forwarded-Proto"); forwarded != "" {
				request.Out.Header.Set("X-Forwarded-Proto", forwarded)
			}
		},
		Transport: config.Transport,
		ModifyResponse: func(response *http.Response) error {
			e.learnResponse(response)
			if _, ok := response.Request.Context().Value(fallbackKey{}).(fallback); ok {
				switch response.StatusCode {
				case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
					_ = response.Body.Close()
					return errUpstreamUnavailable
				}
			}
			return nil
		},
		ErrorHandler: e.upstreamFailed,
		ErrorLog:     slog.NewLogLogger(config.Logger.Handler(), slog.LevelDebug),
	}
	go e.sweep(ctx)
	return e, nil
}

// Close stops the sweep.
func (e *Edge) Close() {
	e.once.Do(func() {
		e.cancel()
		<-e.done
	})
}

func (e *Edge) sweep(ctx context.Context) {
	defer close(e.done)
	for {
		timer := e.config.Clock.NewTimer(e.config.SweepInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
		e.store.sweep(e.config.Clock.Now().Add(-e.config.TTL), e.config.MaxBytes)
	}
}

// ServeHTTP serves mirror uploads, granted segments and granted static
// streams, and forwards every other request to the server.
func (e *Edge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, PathUpload) {
		e.upload(w, r)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if target, ok := e.grantedSegment(r); ok {
			if file, err := e.store.open(target.stem, target.name); err == nil {
				e.serveFile(w, r, target, file)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), fallbackKey{}, target))
		} else if grant, container, ok := e.grantedDirect(r); ok {
			if file, info, err := e.openDirect(grant); err == nil {
				serveDirect(w, r, grant, container, file, info)
				return
			}
		}
	}
	e.proxy.ServeHTTP(w, r)
}

// grantedSegment resolves a segment request that carries a valid read
// grant for its item and play session.
func (e *Edge) grantedSegment(r *http.Request) (fallback, bool) {
	match := segmentPath.FindStringSubmatch(r.URL.Path)
	if match == nil {
		return fallback{}, false
	}
	query := r.URL.Query()
	token, playSessionID := "", ""
	for key, values := range query {
		switch {
		case strings.EqualFold(key, GrantQuery) && len(values) > 0:
			token = values[0]
		case strings.EqualFold(key, "PlaySessionId") && len(values) > 0:
			playSessionID = values[0]
		}
	}
	if token == "" {
		return fallback{}, false
	}
	grant, err := e.config.Signer.VerifyRead(token, e.config.Clock.Now())
	if err != nil {
		return fallback{}, false
	}
	item, err := dto.ParseUUID(match[1])
	if err != nil || item != grant.ItemID || (grant.PlaySessionID != "" && grant.PlaySessionID != playSessionID) {
		return fallback{}, false
	}
	index, err := strconv.Atoi(match[2])
	if err != nil {
		return fallback{}, false
	}
	name := grant.Family.FileName(index)
	if !validName(name) {
		return fallback{}, false
	}
	return fallback{stem: grant.Family.Stem, name: name, container: strings.ToLower(match[3])}, true
}

// upstreamFailed answers a request the server did not. A granted segment
// waits for the worker's mirror copy.
func (e *Edge) upstreamFailed(w http.ResponseWriter, r *http.Request, err error) {
	target, ok := r.Context().Value(fallbackKey{}).(fallback)
	if !ok {
		e.config.Logger.Debug("edge upstream request failed", "path", r.URL.Path, "error", err.Error())
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), e.config.WaitTimeout)
	defer cancel()
	file, waitErr := e.store.wait(ctx, target.stem, target.name)
	if waitErr != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "segment is not available yet", http.StatusServiceUnavailable)
		return
	}
	e.serveFile(w, r, target, file)
}

func (e *Edge) serveFile(w http.ResponseWriter, r *http.Request, target fallback, file *os.File) {
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		http.Error(w, "segment is not readable", http.StatusInternalServerError)
		return
	}
	contentType := "video/mp2t"
	switch target.container {
	case "mp4", "m4s":
		contentType = "video/mp4"
	case "aac":
		contentType = "audio/aac"
	}
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, path.Base(target.name), info.ModTime(), file)
}

// upload stores a family file that a worker mirrors.
func (e *Edge) upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	grant, err := e.config.Signer.VerifyUpload(r.Header.Get(GrantHeader), e.config.Clock.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, PathUpload)
	if !grant.Family.Owns(name) {
		http.Error(w, "file is not part of the granted family", http.StatusForbidden)
		return
	}
	if err := e.store.put(grant.Family.Stem, name, r.Body, e.config.MaxFileBytes); err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, errTooLarge):
			status = http.StatusRequestEntityTooLarge
		case errors.Is(err, hls.ErrMalformedFile):
			status = http.StatusUnprocessableEntity
		}
		http.Error(w, "store file", status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
