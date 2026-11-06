// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/edge"
	"github.com/deadfin-org/deadfin/internal/playback"
	"github.com/deadfin-org/deadfin/internal/transcodeworker"
)

const (
	// envEdgeKey is the key that the server and the stream edge share
	// (#619). On the server it turns on the read grants in segment URLs.
	envEdgeKey = "DEADFIN_EDGE_KEY"
	// envEdgeURL is the edge base URL as transcode workers reach it.
	envEdgeURL = "DEADFIN_EDGE_URL"
	// edgeReadGrantLifetime covers the segment requests of one playback.
	edgeReadGrantLifetime = 24 * time.Hour
)

// serverEdge is the server side of the stream edge.
type serverEdge struct {
	signer *edge.Signer
	url    string
}

// edgeEnvironment reads the server's edge settings. With no key the edge is
// off. A key that is too short is an error; a URL without a key is ignored
// with a warning.
func edgeEnvironment(getenv func(string) string, logger *slog.Logger) (serverEdge, error) {
	key, target := strings.TrimSpace(getenv(envEdgeKey)), strings.TrimSpace(getenv(envEdgeURL))
	if key == "" {
		if target != "" {
			logger.Warn("stream edge URL is set without a key; the edge is off", "setting", envEdgeURL, "missing", envEdgeKey)
		}
		return serverEdge{}, nil
	}
	signer, err := edge.NewSigner(key)
	if err != nil {
		return serverEdge{}, errors.New(envEdgeKey + ": the key is shorter than 16 bytes")
	}
	if target != "" {
		parsed, err := url.Parse(target)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return serverEdge{}, errors.New(envEdgeURL + ": " + target + " is not an http or https URL")
		}
	}
	return serverEdge{signer: signer, url: target}, nil
}

// hubTarget is the edge that workers mirror segments to.
func (e serverEdge) hubTarget() transcodeworker.EdgeTarget {
	if e.signer == nil || e.url == "" {
		return transcodeworker.EdgeTarget{}
	}
	return transcodeworker.EdgeTarget{URL: e.url, Signer: e.signer}
}

// readGrant signs the read grant of one HLS family; nil when the edge is
// off.
func (e serverEdge) readGrant(c clock.Clock) func(dto.UUID, string, string, string) string {
	if e.signer == nil {
		return nil
	}
	return func(itemID dto.UUID, playSessionID, playlistPath, segmentPattern string) string {
		playlist := filepath.Base(playlistPath)
		return e.signer.SignRead(edge.ReadGrant{
			ItemID: itemID, PlaySessionID: playSessionID,
			Family:  edge.Family{Stem: strings.TrimSuffix(playlist, filepath.Ext(playlist)), Pattern: filepath.Base(segmentPattern)},
			Expires: c.Now().Add(edgeReadGrantLifetime),
		})
	}
}

// directGrants signs the direct grants of PlaybackInfo responses (#621);
// nil when the edge is off.
func (e serverEdge) directGrants(c clock.Clock) handlers.PlaybackInfoEdge {
	if e.signer == nil {
		return nil
	}
	return edgeDirectGrants{signer: e.signer, clock: c}
}

type edgeDirectGrants struct {
	signer *edge.Signer
	clock  clock.Clock
}

// DirectGrants signs one grant for each source that the static stream
// route serves from a library file.
func (g edgeDirectGrants) DirectGrants(itemID dto.UUID, playSessionID string, sources []playback.MediaSourceInfo) []string {
	expires := g.clock.Now().Add(edgeReadGrantLifetime)
	var tokens []string
	for _, source := range sources {
		if source.Protocol != playback.MediaProtocolFile || !filepath.IsAbs(source.Path) || source.ID == "" ||
			source.LiveStreamID != "" || source.IsInfiniteStream || !(source.SupportsDirectPlay || source.SupportsDirectStream) ||
			(source.VideoType != nil && (*source.VideoType == playback.VideoTypeDVD || *source.VideoType == playback.VideoTypeBluRay)) {
			continue
		}
		var size int64
		if source.Size != nil && *source.Size > 0 {
			size = *source.Size
		}
		tokens = append(tokens, g.signer.SignDirect(edge.DirectGrant{
			ItemID: itemID, MediaSourceID: source.ID, PlaySessionID: playSessionID,
			Path: source.Path, Size: size, Container: source.Container, Expires: expires,
		}))
	}
	return tokens
}

// runEdge is `deadfin edge`: the stream edge process.
func runEdge(args []string) {
	flags := flag.NewFlagSet("edge", flag.ExitOnError)
	listen := flags.String("listen", ":8097", "TCP address to listen on")
	upstream := flags.String("upstream", "", "server base URL, including any base path")
	storeDir := flags.String("store", filepath.Join(os.TempDir(), "deadfin-edge"), "directory for mirrored segments")
	wait := flags.Duration("wait", 15*time.Second, "how long a granted segment request waits while the server does not answer")
	ttl := flags.Duration("ttl", time.Hour, "how long a mirrored file is kept")
	maxBytes := flags.Int64("max-bytes", 20<<30, "store size bound; the oldest files go first")
	var roots, maps repeatedFlag
	flags.Var(&roots, "library-root", "media library directory mounted read-only for direct streams; repeat for more roots")
	flags.Var(&maps, "path-map", "SERVER=EDGE: the edge path of a server library path prefix; repeat for more prefixes")
	_ = flags.Parse(args)
	mappings, err := pathMappings(maps)
	if err != nil {
		log.Fatal(err)
	}
	server, err := url.Parse(*upstream)
	if err != nil || (server.Scheme != "http" && server.Scheme != "https") || server.Host == "" {
		log.Fatalf("-upstream %q is not an http or https URL", *upstream)
	}
	signer, err := edge.NewSigner(strings.TrimSpace(os.Getenv(envEdgeKey)))
	if err != nil {
		log.Fatalf("%s: %v", envEdgeKey, err)
	}
	handler, err := edge.New(edge.Config{
		Signer: signer, Upstream: server, StoreDirectory: *storeDir, Clock: clock.NewReal(),
		WaitTimeout: *wait, TTL: *ttl, MaxBytes: *maxBytes, Logger: slog.Default(),
		LibraryRoots: roots, PathMappings: mappings,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer handler.Close()
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	slog.Info("stream edge listening", "address", listener.Addr().String(), "upstream", server.String())
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// repeatedFlag collects the values of a flag given more than once.
type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }

func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

// pathMappings parses SERVER=EDGE path prefixes.
func pathMappings(values []string) ([]edge.PathMapping, error) {
	mappings := make([]edge.PathMapping, 0, len(values))
	for _, value := range values {
		server, local, ok := strings.Cut(value, "=")
		if !ok || !filepath.IsAbs(server) || !filepath.IsAbs(local) {
			return nil, errors.New("-path-map " + value + " is not SERVER=EDGE with absolute paths")
		}
		mappings = append(mappings, edge.PathMapping{Server: server, Edge: local})
	}
	return mappings, nil
}
