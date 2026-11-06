// SPDX-License-Identifier: GPL-2.0-only

// Command deadfin runs the Deadfin server.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/buildinfo"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/discovery"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
	"github.com/deadfin-org/deadfin/internal/images"
	"github.com/deadfin-org/deadfin/internal/server"
	"github.com/deadfin-org/deadfin/internal/trickplay"
)

const (
	gracefulShutdownTimeout = 15 * time.Second
	shutdownRetryTimeout    = 30 * time.Second
)

func main() {
	trickplay.MaybeRunFrameSinkTrampoline()
	configureLogging(os.Getenv("DEADFIN_LOG_LEVEL"))
	listen := flag.String("listen", defaultListenAddress, "TCP address to listen on")
	programData := flag.String("program-data", "", programDataUsage)
	databaseURL := flag.String("database-url", "", databaseURLUsage)
	flag.Parse()

	args := flag.Args()
	if len(args) > 0 && args[0] == "version" {
		printVersion(os.Stdout)
		return
	}
	if len(args) > 0 && args[0] == "routes" {
		exportRoutes(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "add-library" {
		addLibrary(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "import-jellyfin" {
		importJellyfin(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "remap-user-ids" {
		remapUserIDs(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "anime-identify" {
		runAnimeIdentify(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "worker" {
		runWorker(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "edge" {
		runEdge(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "backfill" {
		runBackfill(args[1:])
		return
	}
	logBuildIdentity(context.Background(), slog.Default())
	programDataRoot := resolveProgramData(*programData, os.Getenv)
	warnUnusedJellyfinDirs(context.Background(), slog.Default(), os.Getenv, programDataRoot)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopProfiling, err := startProfiling(ctx, os.Getenv("DEADFIN_PPROF_ADDR"), slog.Default())
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stopProfiling(shutdownCtx); err != nil {
			slog.Warn("profiling endpoint shutdown", slog.String("error", err.Error()))
		}
	}()
	if err := runServer(ctx, *listen, programDataRoot, nil, withDatabaseURL(resolveDatabaseURL(*databaseURL, os.Getenv(databaseURLEnv)))); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

// parseLogLevel maps a DEADFIN_LOG_LEVEL value to a slog.Level. The named
// levels (debug, info, warn, error) are case-insensitive. An empty or
// unrecognized value returns LevelInfo, so the default output stays quiet.
func parseLogLevel(value string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// configureLogging installs the slog default handler at the requested level.
// Providers that log through slog.Default (for example the TMDb provider at
// LevelDebug) become visible when the operator sets DEADFIN_LOG_LEVEL=debug.
func configureLogging(level string) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLogLevel(level),
	})))
}

// printVersion writes the one-line display form of the build identity:
// "deadfin-server <Version> (<Channel>, <rev12>[, dirty])".
func printVersion(w io.Writer) {
	revision := buildinfo.Revision
	if revision != buildinfo.UnknownRevision {
		revision = revision[:12]
	}
	dirty := ""
	if buildinfo.Dirty && buildinfo.Revision != buildinfo.UnknownRevision {
		dirty = ", dirty"
	}
	fmt.Fprintf(w, "%s %s (%s, %s%s)\n", buildinfo.Component, buildinfo.Version, buildinfo.Channel, revision, dirty)
}

// logBuildIdentity emits the build identity as structured attributes once at
// server startup. Subcommands do not log it.
func logBuildIdentity(ctx context.Context, logger *slog.Logger) {
	logger.LogAttrs(ctx, slog.LevelInfo, "deadfin build",
		slog.String("component", buildinfo.Component),
		slog.String("version", buildinfo.Version),
		slog.String("channel", buildinfo.Channel),
		slog.String("revision", buildinfo.Revision),
		slog.Bool("dirty", buildinfo.Dirty),
		slog.String("build_time", buildinfo.BuildTime),
		slog.String("compatibility_target", buildinfo.CompatibilityTarget),
		slog.Int("api_version", buildinfo.APIVersion),
		slog.Any("capabilities", buildinfo.Capabilities()),
	)
}

func exportRoutes(args []string) {
	flags := flag.NewFlagSet("routes", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "emit routes as JSON")
	if err := flags.Parse(args); err != nil {
		log.Fatal(err)
	}
	if !*jsonOutput || flags.NArg() != 0 {
		log.Fatal("usage: deadfin routes --json")
	}

	if err := json.NewEncoder(os.Stdout).Encode(routeLedger().Routes()); err != nil {
		log.Fatal(err)
	}
}

func runServer(ctx context.Context, listen, programData string, ready chan<- string, options ...applicationOption) (err error) {
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}

	// The libvips runtime is a process-wide, non-restartable singleton, so it is
	// opened here in the real entrypoint rather than in newApplicationWithListen
	// (which construction tests call repeatedly).
	imageRuntime, err := images.OpenRuntime(ctx)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("start image runtime: %w", err)
	}
	defer func() { err = errors.Join(err, imageRuntime.Close()) }()
	processor, err := newImageProcessor(imageRuntime, programData)
	if err != nil {
		_ = listener.Close()
		return err
	}

	paths, err := config.NewPaths(programData)
	if err != nil {
		_ = listener.Close()
		return err
	}
	tileComposer, err := images.NewTileComposer(imageRuntime, images.TileComposerConfig{
		Root: paths.Trickplay, MaxConcurrent: max(1, runtime.GOMAXPROCS(0)),
	})
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("start trickplay tile composer: %w", err)
	}
	defer func() { err = errors.Join(err, tileComposer.Close()) }()
	probedCapabilities, err := ffmpeg.ProbeCapabilities(ctx, "ffmpeg")
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("probe ffmpeg capabilities: %w", err)
	}
	nvidia, err := ffmpeg.ProbeNvidia(ctx, "ffmpeg", probedCapabilities)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("probe NVIDIA device: %w", err)
	}
	if nvidia.Device {
		log.Printf("deadfin: CUDA device 0 accepts NVENC encoders %v", nvidia.Encoders)
	}
	options = append([]applicationOption{withImageProcessor(processor), withTileComposer(tileComposer), withEncodingCapabilities(encodingCapabilities(probedCapabilities, nvidia))}, options...)
	app, err := newApplicationWithListen(ctx, programData, listener.Addr().String(), options...)
	if err != nil {
		_ = listener.Close()
		return err
	}
	defer func() { err = errors.Join(err, closeApplicationWithRetry(app)) }()

	// Read the normalized network snapshot once after the application exists and
	// wrap the router with the BaseUrl mount before passing it to the server.
	network := app.ConfigManager().Network()
	base := network.NormalizedBaseURL()
	mountedHandler := app.AcceptForwards(router.Mount(base, app.Router()))

	// Start the optional UDP discovery responder when enabled. Construction
	// binds the socket and starts the read goroutine. A bind failure is
	// logged and ignored so HTTP startup continues. Shutdown is deferred
	// after app.Close so LIFO order closes discovery first.
	if network.AutoDiscovery {
		// Load the same persisted installation identity that the HTTP
		// handlers advertise. application.go loads data/device.txt through
		// config.LoadOrCreateServerIdentity. Replaying that load here ensures
		// the UDP discovery ID matches /System/Info/Public instead of the
		// default process constant.
		hostname, hostErr := os.Hostname()
		if hostErr != nil {
			return fmt.Errorf("resolve hostname for discovery identity: %w", hostErr)
		}
		persisted, idErr := config.LoadOrCreateServerIdentity(paths, "", hostname, rand.Reader)
		if idErr != nil {
			return fmt.Errorf("load installation identity for discovery: %w", idErr)
		}
		manager := app.ConfigManager()
		identity := configuredServerIdentity(persisted, manager)
		httpPort := listenPort(listener.Addr().String())
		responder, dErr := discovery.New(ctx, discovery.Config{
			Port:       7359,
			ServerID:   identity.ID.String(),
			ServerName: func() string { return configuredServerIdentity(persisted, manager).ServerName },
			Address:    discovery.LocalAddressFunc(httpPort, base),
		})
		if dErr != nil {
			log.Printf("discovery: %v", dErr)
		} else {
			defer func() {
				shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				err = errors.Join(err, responder.Close(shutdown))
			}()
		}
	}

	httpServer, err := server.New(server.Deps{
		Clock:   clock.NewReal(),
		Handler: mountedHandler,
		Addr:    listener.Addr().String(),
	})
	if err != nil {
		_ = listener.Close()
		return err
	}
	// Signal readiness with the actual listen address. The listener is
	// already accepting connections at this point. Serve blocks below.
	if ready != nil {
		select {
		case ready <- listener.Addr().String():
		default:
		}
	}
	return httpServer.Serve(drainBeforeStop(ctx, app), listener)
}

// replicaDrainer hands a replica's work to its peers (#652).
type replicaDrainer interface {
	DrainReplica(context.Context) error
}

// drainBeforeStop returns the context that stops the HTTP server: it ends
// after ctx, once the replica drain is over, so that the drained replica
// still answers the requests that reach it during the handover. Without
// replica mode the drain returns at once. The goroutine ends with ctx.
func drainBeforeStop(ctx context.Context, drainer replicaDrainer) context.Context {
	serve, stop := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		defer stop()
		<-ctx.Done()
		drain, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
		defer cancel()
		_ = drainer.DrainReplica(drain)
	}()
	return serve
}

type applicationCloser interface {
	Close(context.Context) error
}

// closeApplicationWithRetry gives a terminating ffmpeg process enough time to
// pass its grace period and reap. An incomplete first close receives exactly
// one fresh bounded retry so application.Close can continue dependent cleanup.
func closeApplicationWithRetry(app applicationCloser) error {
	if app == nil {
		return nil
	}
	first := closeApplicationWithin(app, gracefulShutdownTimeout)
	if first == nil {
		return nil
	}
	second := closeApplicationWithin(app, shutdownRetryTimeout)
	return errors.Join(first, second)
}

func closeApplicationWithin(app applicationCloser, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return app.Close(ctx)
}

// newImageProcessor builds the resized-image processor over runtime, publishing
// to the program-data image cache. ffmpeg, when present on PATH, converts source
// formats libvips cannot decode natively (upstream GetSupportedImage); when
// absent, such sources fail to serve while jpeg/png/webp/gif/bmp continue to work.
func newImageProcessor(imageRuntime *images.Runtime, programData string) (*images.Processor, error) {
	paths, err := config.NewPaths(programData)
	if err != nil {
		return nil, err
	}
	return images.NewProcessor(imageRuntime, images.ProcessorConfig{
		CacheRoot:       filepath.Join(paths.Cache, "images", "resized"),
		MaxConcurrent:   max(1, runtime.GOMAXPROCS(0)),
		SourceConverter: imageSourceConverter(),
	})
}

func imageSourceConverter() images.SourceConverter {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil
	}
	return images.FFmpegConverter{Path: path}
}
