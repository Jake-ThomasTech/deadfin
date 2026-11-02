// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/deadfin-org/deadfin/internal/activitylog"
	"github.com/deadfin-org/deadfin/internal/anime"
	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/auth/playgrant"
	"github.com/deadfin-org/deadfin/internal/buildinfo"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/displaypreferences"
	"github.com/deadfin-org/deadfin/internal/drives"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/dtobuild"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/events"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
	"github.com/deadfin-org/deadfin/internal/identify"
	"github.com/deadfin-org/deadfin/internal/images"
	"github.com/deadfin-org/deadfin/internal/imageserve"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/itemdelete"
	"github.com/deadfin-org/deadfin/internal/itemedit"
	"github.com/deadfin-org/deadfin/internal/keyframes"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/localization"
	"github.com/deadfin-org/deadfin/internal/naming"
	"github.com/deadfin-org/deadfin/internal/path"
	"github.com/deadfin-org/deadfin/internal/people"
	"github.com/deadfin-org/deadfin/internal/playback"
	"github.com/deadfin-org/deadfin/internal/playlists"
	"github.com/deadfin-org/deadfin/internal/playstate"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/providers/anilist"
	"github.com/deadfin-org/deadfin/internal/providers/audiodb"
	"github.com/deadfin-org/deadfin/internal/providers/listenbrainz"
	"github.com/deadfin-org/deadfin/internal/providers/musicbrainz"
	"github.com/deadfin-org/deadfin/internal/providers/remote"
	"github.com/deadfin-org/deadfin/internal/providers/tmdb"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/sessioncontrol"
	"github.com/deadfin-org/deadfin/internal/sessiondto"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
	"github.com/deadfin-org/deadfin/internal/subtitleedit"
	"github.com/deadfin-org/deadfin/internal/subtitles"
	"github.com/deadfin-org/deadfin/internal/tasks"
	"github.com/deadfin-org/deadfin/internal/trickplay"
	"github.com/deadfin-org/deadfin/internal/user"
	"github.com/deadfin-org/deadfin/internal/ws"
)

const databaseFile = "deadfin.db"

const defaultListenAddress = ":8096"

func audioDbRemoteConfig(source clock.Clock) remote.Config {
	return remote.Config{
		Clock:          source,
		UserAgent:      "Deadfin/dev (+https://github.com/deadfin-org/deadfin)",
		MinInterval:    audiodb.DefaultRequestInterval,
		Burst:          1,
		CacheEntries:   512,
		CacheFallback:  48 * time.Hour,
		RequestTimeout: 30 * time.Second,
	}
}

// application owns the services and persistent resources that serve one
// Deadfin process. The command constructs it once, then closes it after the
// HTTP server has stopped accepting requests.
type application struct {
	libraryChanges       *library.ChangeQueue
	router               *router.Router
	manager              *config.Manager
	users                *user.Service
	deviceActivity       *user.DeviceActivity
	stores               store.Stores
	changes              *events.Bus[config.Change]
	userDataChanges      *events.Bus[playstate.UserDataChanged]
	activity             *activitylog.Log
	tasks                *tasks.Catalog
	scheduler            *tasks.Scheduler
	topology             *library.Topology
	scanner              *library.Scanner
	sessions             *session.Manager
	webSockets           *ws.Server
	listenScrobbler      *listenbrainz.Scrobbler
	transcodes           *ffmpeg.TranscodeManager
	keyframes            *lazyKeyframeSource
	progressive          playback.ProgressiveVideoStreamer
	hls                  playback.HLSVideoStreamer
	trickplayRunner      *tasks.TrickplayRunner
	trickplayTask        *tasks.Task
	trickplayReader      *trickplay.ReaderService
	trickplayGenerator   *trickplay.Generator
	trickplayPublication *trickplay.PublicationCoordinator
	configuration        *configurationConsumer
	transcodeEncoding    playback.TranscodeEncoding
	closeTransition      atomic.Pointer[applicationCloseTransition]
}

// applicationCloseTransition is an instance-owned, context-selectable close
// gate. Its lazy atomic construction keeps zero-value application literals
// usable in narrow lifecycle tests.
type applicationCloseTransition struct {
	gate chan struct{}
}

// newApplication constructs Deadfin's runtime service graph.
// programData is the root path for configuration and persistent data.
// Every persistent application path derives from this path.
func newApplication(ctx context.Context, programData string) (*application, error) {
	return newApplicationWithListen(ctx, programData, defaultListenAddress)
}

func newApplicationWithListen(ctx context.Context, programData, listen string, options ...applicationOption) (app *application, err error) {
	var appConfig applicationConfig
	for _, option := range options {
		option(&appConfig)
	}
	paths, err := config.NewPaths(programData)
	if err != nil {
		return nil, err
	}
	if err := createApplicationDirectories(paths); err != nil {
		return nil, err
	}

	realClock := clock.NewReal()
	changes := events.NewBus[config.Change](realClock)
	userDataChanges := events.NewBus[playstate.UserDataChanged](realClock)
	var activity *activitylog.Log
	scheduledTasks := tasks.New(realClock)
	var scheduler *tasks.Scheduler
	var stores store.Stores
	var topology *library.Topology
	var scanner *library.Scanner
	var sessions *session.Manager
	var webSockets *ws.Server
	var playstateService *playstate.Service
	var listenScrobbler *listenbrainz.Scrobbler
	var transcodes *ffmpeg.TranscodeManager
	var trickplayRunner *tasks.TrickplayRunner
	var trickplayReader *trickplay.ReaderService
	var trickplayGenerator *trickplay.Generator
	var trickplayTask *tasks.Task
	var lazyKeyframes *lazyKeyframeSource
	var deviceActivity *user.DeviceActivity
	var configurationApplier *configurationConsumer
	defer func() {
		if err == nil {
			return
		}
		if configurationApplier != nil {
			err = errors.Join(err, configurationApplier.Close(context.Background()))
		}
		if scanner != nil {
			err = errors.Join(err, scanner.Close())
		}
		if trickplayRunner != nil {
			err = errors.Join(err, trickplayRunner.Close(context.Background()))
		}
		if trickplayGenerator != nil {
			err = errors.Join(err, trickplayGenerator.Close())
		}
		if trickplayReader != nil {
			err = errors.Join(err, trickplayReader.Close())
		}
		if scheduler != nil {
			err = errors.Join(err, scheduler.Close(context.Background()))
		}
		if transcodes != nil {
			err = errors.Join(err, transcodes.Close(context.Background()))
		}
		if lazyKeyframes != nil {
			err = errors.Join(err, lazyKeyframes.Close(context.Background()))
		}
		if deviceActivity != nil {
			err = errors.Join(err, deviceActivity.Close(context.Background()))
		}
		if webSockets != nil {
			err = errors.Join(err, webSockets.Close(context.Background()))
		}
		if sessions != nil {
			err = errors.Join(err, closeSessions(sessions))
		}
		if listenScrobbler != nil {
			err = errors.Join(err, listenScrobbler.Close(context.Background()))
		}
		if topology != nil {
			err = errors.Join(err, topology.Close())
		}
		err = errors.Join(err, scheduledTasks.Close(context.Background()))
		if activity != nil {
			err = errors.Join(err, activity.Close(context.Background()))
		}
		err = errors.Join(err, userDataChanges.Close(context.Background()))
		if stores != nil {
			err = errors.Join(err, stores.Close())
		}
		err = errors.Join(err, changes.Close(context.Background()))
	}()

	manager, err := config.Open(ctx, paths, changes)
	if err != nil {
		return nil, fmt.Errorf("open configuration: %w", err)
	}
	hardwareProbe := appConfig.hardwareProbe
	if hardwareProbe == nil {
		hardwareProbe = probeVaapiDriver
	}
	hardware := newHardwareCapabilities(ctx, appConfig.encodingCapabilities, manager.Encoding(), hardwareProbe)
	log.Printf("application: encoding %s; configured HardwareAccelerationType %q", hardware.Current().HardwareSummary(), manager.Encoding().HardwareAccelerationType)
	configurationApplier = startConfigurationConsumer(changes, manager, hardware, appConfig.configurationApplied)
	transcodeEncoding := playback.TranscodeEncoding{
		Capabilities:        hardware.Current(),
		CurrentCapabilities: hardware.Current,
		Options:             func() encoding.HardwareOptions { return hardwareOptions(manager.Encoding(), renderNodeExists) },
		SubtitleExtraction:  func() bool { return manager.Encoding().EnableSubtitleExtraction },
	}
	selectedDatabase, err := parseDatabase(appConfig.databaseURL, paths.Data)
	if err != nil {
		return nil, err
	}
	// The statistics refresher and the page cache settings are SQLite
	// features. PostgreSQL autovacuum keeps planner statistics current.
	var sqliteOptions []sqlite.Option
	if selectedDatabase.postgresDSN == "" {
		sqliteCache, err := sqliteCacheFromEnv(os.Getenv)
		if err != nil {
			return nil, err
		}
		sqliteOptions = append(sqliteOptions, sqlite.WithStatistics(sqlite.StatisticsOptions{
			Clock:  realClock,
			Logger: slog.Default(),
		}), sqlite.WithCache(sqliteCache))
	}
	stores, err = selectedDatabase.open(ctx, sqliteOptions...)
	if err != nil {
		return nil, fmt.Errorf("open application database (%s): %w", selectedDatabase, err)
	}
	log.Printf("application: store %s", selectedDatabase)

	activity = activitylog.NewWithStore(realClock, store.NewActivityLogStore(stores), slog.Default())
	identity := store.NewIdentity(stores)
	users := user.New(identity, realClock, rand.Reader).WithActivity(activity)
	if _, err := users.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("initialize users: %w", err)
	}
	topology, err = library.NewTopology(paths.Root, store.NewLibraryStore(stores))
	if err != nil {
		return nil, fmt.Errorf("open library topology: %w", err)
	}
	// Shared, rate-limited, response-caching transport for internet metadata
	// providers. TMDb honors its own Cache-Control; the fallback matches
	// upstream's one-hour in-memory cache (TmdbClientManager.CacheDurationInHours).
	remoteHTTP := remote.NewClient(remote.Config{
		Doer:          appConfig.providerRemoteDoer,
		Clock:         realClock,
		UserAgent:     "Deadfin/dev (+https://github.com/deadfin-org/deadfin)",
		MinInterval:   25 * time.Millisecond,
		Burst:         20,
		CacheEntries:  512,
		CacheFallback: time.Hour,
	})
	tmdbConfig := tmdb.LoadConfig(filepath.Join(paths.Config, "plugins", "Jellyfin.Plugin.Tmdb.xml"))
	tmdbProvider := tmdb.NewProvider(tmdb.Options{HTTP: remoteHTTP, Config: tmdbConfig})
	musicBrainzConfig := musicbrainz.LoadConfig(filepath.Join(paths.Config, "plugins", "Jellyfin.Plugin.MusicBrainz.xml"))
	musicBrainzHTTP := remote.NewClient(remote.Config{
		Doer:           appConfig.providerRemoteDoer,
		UserAgent:      "Deadfin/dev (+https://github.com/deadfin-org/deadfin)",
		MinInterval:    time.Duration(musicBrainzConfig.RateLimit * float64(time.Second)),
		Burst:          1,
		CacheEntries:   512,
		RequestTimeout: 30 * time.Second,
	})
	// Anime identity: offline seed datasets in the metadata
	// directory, refreshed by a scheduled task at most once a day per source,
	// and an AniList transport paced below AniList's 30 requests per minute.
	animeSeedDir := filepath.Join(paths.Metadata, "anime")
	animeSeeds := anime.NewService(anime.ServiceOptions{
		Fetcher: anime.NewFetcher(anime.FetcherOptions{
			Clock:     realClock,
			Dir:       animeSeedDir,
			UserAgent: "Deadfin/dev (+https://github.com/deadfin-org/deadfin)",
		}),
		Dir: animeSeedDir,
	})
	aniListHTTP := remote.NewClient(remote.Config{
		Doer:           appConfig.providerRemoteDoer,
		Clock:          realClock,
		UserAgent:      "Deadfin/dev (+https://github.com/deadfin-org/deadfin)",
		MinInterval:    2500 * time.Millisecond,
		Burst:          1,
		RequestTimeout: 2 * time.Minute,
	})
	audioDbConfig := audiodb.LoadConfig(filepath.Join(paths.Config, "plugins", "Jellyfin.Plugin.AudioDb.xml"))
	audioDbRemote := audioDbRemoteConfig(realClock)
	audioDbRemote.Doer = appConfig.providerRemoteDoer
	audioDbHTTP := remote.NewClient(audioDbRemote)
	audioDbProvider := audiodb.NewProvider(audiodb.Options{HTTP: audioDbHTTP, Config: audioDbConfig})
	refresher := providers.NewRegistry(
		providers.NewLocalImageProvider(),
		providers.NewNFOProvider(providers.NFOProviderOptions{
			Open: func(ctx context.Context, path string) (io.ReadCloser, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return os.Open(path)
			},
		}),
		musicbrainz.NewProvider(musicbrainz.Options{HTTP: musicBrainzHTTP, Config: musicBrainzConfig}),
		audioDbProvider,
		tmdbProvider,
		// AniList acts only in libraries that list it among the series
		// metadata fetchers; order it after TheMovieDb so its titles win.
		anilist.NewProvider(anilist.Options{
			HTTP:       aniListHTTP,
			Crosswalks: animeSeeds,
			Store:      store.NewAnimeStore(stores),
			Clock:      realClock,
			Logger:     slog.Default(),
		}),
	).WithDisabledImageFetchers(func(itemType string) []string {
		for _, options := range manager.System().MetadataOptions {
			if strings.EqualFold(options.ItemType, itemType) {
				return options.DisabledImageFetchers
			}
		}
		return nil
	})
	tmdbLookup := tmdb.NewLookupProvider(tmdb.Options{HTTP: remoteHTTP, Config: tmdbConfig})
	musicbrainzLookup := musicbrainz.NewLookupProvider(musicbrainz.Options{HTTP: musicBrainzHTTP, Config: musicBrainzConfig})
	lookupRegistry := providers.NewLookupRegistry(
		[]providers.Searcher{tmdbLookup, musicbrainzLookup},
		[]providers.ImageLister{tmdbLookup, audioDbProvider},
	)
	virtualPaths := path.NewVirtualPathExpander(paths.Data, paths.Metadata)
	items := store.NewItemStore(stores)
	if err := os.MkdirAll(filepath.Join(paths.Root, "root", "default"), 0o700); err != nil {
		return nil, fmt.Errorf("create user root folder: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(paths.Data, "playlists"), 0o700); err != nil {
		return nil, fmt.Errorf("create playlists folder: %w", err)
	}
	if err := items.BootstrapSystemFolders(ctx, paths.Root); err != nil {
		return nil, fmt.Errorf("bootstrap system folders: %w", err)
	}
	mediaProbe, err := newApplicationMediaProbe(ctx)
	if err != nil {
		return nil, fmt.Errorf("open library media probe: %w", err)
	}
	dynamicImages := newDynamicImages(manager, hardware.Current, paths.Metadata)
	scanner, err = library.NewScanner(topology, items, library.NewDefaultChain(naming.DefaultOptions()), library.ScannerOptions{
		Clock:           realClock,
		Refresher:       refresher,
		MediaProbe:      mediaProbe,
		Logger:          slog.Default(),
		CollectionsPath: filepath.Join(paths.Data, "collections"),
		MetadataPath:    paths.Metadata,
		DynamicImages:   dynamicImages,
		RefreshComplete: func(result library.RefreshResult) {
			requestStatisticsRefresh(stores, result)
			if trickplayRunner == nil {
				return
			}
			dispatchRefreshTrickplay(result, trickplayRunner.TriggerRegenerateTrickplay)
			// TrickplayProvider runs for new and changed items during a scan;
			// the scan-scope task generates only missing renditions, so an
			// unchanged scan does no trickplay work.
			if result.Inserted+result.Updated > 0 {
				if err := trickplayRunner.Trigger(); err != nil {
					log.Printf("schedule scan trickplay refresh: %v", err)
				}
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("open library scanner: %w", err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("resolve hostname for installation identity: %w", err)
	}
	identityConfig, err := config.LoadOrCreateServerIdentity(paths, "", hostname, rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("load installation identity: %w", err)
	}
	localizationService, err := localization.NewService()
	if err != nil {
		return nil, fmt.Errorf("open localization service: %w", err)
	}
	itemBuilder := dtobuild.NewBuilder(items, dtobuild.NewStoreEnricher(items, users, localizationService), identityConfig.ID.String()).WithPaths(virtualPaths)
	views := library.NewViews(items, users)
	playlistService, err := playlists.New(playlists.Options{Items: items, Users: users, Clock: realClock, ProgramData: paths.Root, DataPath: paths.Data})
	if err != nil {
		return nil, fmt.Errorf("open playlists: %w", err)
	}
	displayPreferences, err := displaypreferences.New(identity)
	if err != nil {
		return nil, fmt.Errorf("open display preferences: %w", err)
	}
	// Authorization device refreshes write off the request path, so a request
	// never waits for the SQLite writer that a library scan holds.
	deviceActivity = user.NewDeviceActivity(identity, realClock, slog.Default())
	resolver := auth.NewResolver(auth.ResolverConfig{
		Devices: deviceActivity,
		APIKeys: identity,
		Server:  identityConfig,
		LegacyAuthorizationEnabled: func() bool {
			return manager.System().EnableLegacyAuthorization
		},
	})
	network := configuredNetwork{manager: manager, lan: &lanSubnets{manager: manager}}
	evaluator := auth.NewEvaluator(manager, network, identity, realClock.Now)
	sessions = session.New(realClock)
	playstateService = playstate.NewService(realClock, manager, items, users, sessions, userDataChanges).WithActivity(activity)
	listenBrainzConfig := listenbrainz.LoadConfig(filepath.Join(paths.Config, "plugins", "Jellyfin.Plugin.ListenBrainz.xml"))
	listenBrainzClient := listenbrainz.NewClient(listenbrainz.ClientOptions{
		Doer:      &http.Client{Timeout: 30 * time.Second},
		Config:    listenBrainzConfig,
		UserAgent: "Deadfin/dev (+https://github.com/deadfin-org/deadfin)",
	})
	listenScrobbler = listenbrainz.NewScrobbler(listenbrainz.ScrobblerOptions{
		Client:           listenBrainzClient,
		Items:            items,
		Clock:            realClock,
		SubmitPlayingNow: listenBrainzConfig.SubmitPlayingNow,
		OnError: func(err error) {
			log.Printf("ListenBrainz submission failed: %v", err)
		},
	})
	playstateApplication := listenbrainz.Wrap(playstateService, listenScrobbler)
	transcodes, err = ffmpeg.NewTranscodeManager("ffmpeg", paths.Transcodes, ffmpeg.TranscodeManagerOptions{Clock: realClock, LogDirectory: paths.Log, Logger: slog.Default(), PKeySupported: appConfig.encodingCapabilities.SupportsPKeyPause})
	if err != nil {
		return nil, fmt.Errorf("open progressive transcode manager: %w", err)
	}
	sessionProjector := sessiondto.New(users, items, itemBuilder, identityConfig.ID.String()).WithTranscoding(transcodingState{manager: transcodes})
	webSockets = ws.New(ws.Config{
		Clock:            realClock,
		Sessions:         sessions,
		Users:            users,
		SessionProjector: sessionProjector,
		Items:            items,
		ItemBuilder:      itemBuilder,
		LibraryChanges:   scanner.Changes(),
		RefreshProgress:  scanner.Progress(),
		UserDataChanges:  userDataChanges,
		Activity:         activity,
		Tasks:            scheduledTasks,
	})
	if err := sessions.SetOptions(session.Options{
		InactiveSessionThreshold: func() int {
			return manager.System().InactiveSessionThreshold
		},
		OnIdlePlayback: func(ctx context.Context, snapshot session.Snapshot, state session.PlaybackState) error {
			return playstateApplication.Stop(ctx, playstate.Actor{
				UserID: snapshot.UserID, Client: snapshot.Client, DeviceID: snapshot.DeviceID, Target: snapshot.Target,
			}, playstate.Report{PlaybackState: state})
		},
		OnInactivePlayback: func(ctx context.Context, snapshot session.Snapshot, state session.PlaybackState) error {
			controllingUserID := snapshot.UserID.String()
			return webSockets.Send(ctx, snapshot.ID, ws.MessagePlaystate, dto.PlaystateRequest{
				Command: "Stop", SeekPositionTicks: state.PositionTicks, ControllingUserID: &controllingUserID,
			})
		},
		OnSessionStarted: func(ctx context.Context, snapshot session.Snapshot) {
			recordSessionActivity(ctx, activity, users, snapshot, activitylog.SessionStarted)
		},
		OnSessionEnded: func(ctx context.Context, snapshot session.Snapshot) {
			recordSessionActivity(ctx, activity, users, snapshot, activitylog.SessionEnded)
		},
	}); err != nil {
		return nil, fmt.Errorf("configure session watchdogs: %w", err)
	}
	sessionController := sessioncontrol.New(sessions, users, webSockets)
	login := auth.NewLogin(auth.LoginConfig{
		Users:    users,
		Policy:   loginPolicy{evaluator: evaluator, users: users},
		Network:  network,
		Sessions: loginSessions{manager: sessions, users: users},
		Server:   identityConfig,
		LegacyAuthorizationEnabled: func() bool {
			return manager.System().EnableLegacyAuthorization
		},
		Activity: activity,
	})
	playbackService := playback.NewPlaybackInfoService(playback.PlaybackInfoServiceConfig{
		Sources:        playbackSourceReader{items: items},
		Access:         store.NewPlaybackItemAccessReader(items, users),
		DeviceProfiles: playbackDeviceProfiles{sessions: sessions},
		Permissions:    playbackPolicyReader{users: users, manager: manager},
		Network:        playbackNetworkClassifier{network: network},
		Selection:      store.NewPlaybackSelectionReader(items, users),
		Languages:      localizationService,
		Paths:          virtualPaths,
		Encoding:       transcodeEncoding,
	})
	progressiveService := playback.NewProgressiveVideoService(playback.ProgressiveServiceConfig{
		Sources:      playbackService,
		TranscodeDir: paths.Transcodes,
		Opener:       progressiveTranscodeOpener{manager: transcodes},
		Capabilities: hardware.Current,
		AudioVBR:     func() bool { return manager.Encoding().EnableAudioVbr },
	})
	// On-demand extraction uses only metadata-based extractors, as
	// DynamicHlsPlaylistGenerator.cs does: Matroska Cues, never a full ffprobe scan.
	lazyKeyframes = newLazyKeyframeSource(items, keyframes.MatroskaExtractor{})
	trickplayMetadata := trickplayStoreAdapter{items: items}
	trickplayPublication := trickplay.NewPublicationCoordinator()
	subtitleConverter, err := subtitles.NewConverter("ffmpeg", 30*time.Second, 8<<20)
	if err != nil {
		return nil, fmt.Errorf("open HLS subtitle converter: %w", err)
	}
	mediaExtractor := ffmpeg.Extractor{Path: "ffmpeg"}
	subtitleService := playback.NewSubtitleService(playback.SubtitleServiceConfig{
		Sources: playbackService, Converter: subtitleConverter,
		Extractor: mediaExtractor, ExtractionPath: filepath.Join(paths.Data, "subtitles"),
		ExtractionTimeout: func() time.Duration {
			return time.Duration(manager.Encoding().SubtitleExtractionTimeoutMinutes) * time.Minute
		},
	})
	attachmentService := playback.NewAttachmentService(playback.AttachmentServiceConfig{
		Sources: playbackService, Extractor: mediaExtractor, Path: filepath.Join(paths.Data, "attachments"),
	})
	hlsService := playback.NewHLSService(playback.HLSServiceConfig{
		Sources:             playbackService,
		TranscodeDir:        paths.Transcodes,
		Capabilities:        hardware.Current(),
		CurrentCapabilities: hardware.Current,
		Hardware:            transcodeEncoding.Options,
		Keyframes:           lazyKeyframes,
		Trickplays:          trickplayMetadata,
		Manager:             hlsTranscodeManager{manager: transcodes},
		Subtitles:           subtitleService,
		Attachments:         attachmentService,
		AudioStart:          ffmpeg.Prober{Path: applicationFFProbePath, Timeout: 30 * time.Second},
	})
	trickplayReader = trickplay.NewReaderService(trickplay.ReaderServiceConfig{
		Metadata: trickplayMetadata,
		Access: trickplayVisibilityAdapter{
			access: store.NewPlaybackItemAccessReader(items, users),
		},
		Publication: trickplayPublication,
		Root:        paths.Trickplay,
		Media:       trickplayMediaPaths{items: items},
	})
	if appConfig.tileComposer != nil {
		frameExtractor, extractErr := trickplay.NewFFmpegFrameExtractor("ffmpeg", trickplayAcceleration(manager, hardware.Current, items))
		if extractErr != nil {
			return nil, fmt.Errorf("open trickplay frame extractor: %w", extractErr)
		}
		generator, generateErr := trickplay.NewGenerator(trickplay.GeneratorConfig{
			Root: paths.Trickplay, Extractor: frameExtractor, Composer: appConfig.tileComposer, Store: trickplayMetadata, Reader: trickplayMetadata, Publication: trickplayPublication,
		})
		if generateErr != nil {
			return nil, fmt.Errorf("open trickplay generator: %w", generateErr)
		}
		trickplayGenerator = generator
		if recoverErr := generator.Recover(ctx); recoverErr != nil {
			return nil, fmt.Errorf("recover trickplay publications: %w", recoverErr)
		}
		options := tasks.DefaultOptions()
		frozenOptions := options
		frozenOptions.Widths = append([]int(nil), options.Widths...)
		task := tasks.NewTrickplayTask(
			trickplayTaskSource{items: items, topology: topology, options: frozenOptions},
			trickplayTaskGenerator{generator: generator, discovery: items, interval: options.IntervalMilliseconds},
			trickplayTaskReporter{},
			options,
		)
		trickplayTask = task
		trickplayRunner = tasks.NewTrickplayRunner(trickplayScanExecutable{task: task}, func(runErr error) {
			log.Printf("scan trickplay refresh: %v", runErr)
		})
	}
	scheduler, err = tasks.NewScheduler(realClock, time.Local, scheduledTasks,
		filepath.Join(paths.Config, "ScheduledTasks"),
		filepath.Join(paths.Data, "ScheduledTasks"))
	if err != nil {
		return nil, fmt.Errorf("open task scheduler: %w", err)
	}
	// The person refresh needs a TMDb key; without one the TMDb provider is
	// inert and a refresh would only stamp DateLastRefreshed.
	var peopleService *people.Service
	if tmdbConfig.APIKey != "" {
		peopleService, err = people.NewService(people.Options{
			Items:    items,
			Provider: tmdbProvider,
			Settings: manager,
			Clock:    realClock,
			Logger:   slog.Default(),
		})
		if err != nil {
			return nil, fmt.Errorf("open people service: %w", err)
		}
	}
	builtinDeps := tasks.BuiltinDeps{
		ScanLibrary:        libraryScanAdapter{scanner: scanner},
		LibraryScanRunning: scheduler,
		StoreMaintainer:    storeMaintainerAdapter{stores: stores},
		ActivityLogPruner:  activity,
		ActivityLogRetentionDays: func() *int {
			return manager.System().ActivityLogRetentionDays
		},
		TranscodeCleaner: transcodes,
		UserDataCleaner:  userDataCleanerAdapter{stores: stores},
		Clock:            realClock,
		LogPath:          paths.Log,
		CachePath:        paths.Cache,
		TranscodePath:    paths.Transcodes,
	}
	// Register the RefreshMetadata extension task only in the production
	// server (imageProcessor is injected by main.go) and in composition
	// tests that inject a recorded provider transport (providerRemoteDoer).
	// The stock composition test asserts the 12-task Jellyfin-parity count,
	// so RefreshMetadata stays absent when neither signal is present.
	if appConfig.imageProcessor != nil || appConfig.providerRemoteDoer != nil {
		builtinDeps.MetadataRefresher = metadataRefreshAdapter{scanner: scanner}
	}
	// The anime seed task downloads from the internet, so only the
	// production server (imageProcessor is injected by main.go) registers it.
	if appConfig.imageProcessor != nil {
		builtinDeps.AnimeSeeds = animeSeeds
	}
	if trickplayTask != nil {
		builtinDeps.Trickplay = trickplayTask
	}
	if peopleService != nil {
		builtinDeps.People = peopleService
	}
	if err := tasks.RegisterBuiltins(scheduler, builtinDeps); err != nil {
		return nil, fmt.Errorf("register scheduled tasks: %w", err)
	}
	for _, reg := range appConfig.scheduledTasks {
		if err := scheduler.Register(reg.def, reg.run); err != nil {
			return nil, fmt.Errorf("register scheduled task %s: %w", reg.def.ID, err)
		}
	}
	if err := scheduler.OnCompleted(func(_ context.Context, result tasks.Result) {
		if result.Status == tasks.StatusFailed {
			activity.Record(activitylog.TaskFailed(result.Name, result.ErrorMessage, result.LongErrorMessage, result.EndTimeUTC.Sub(result.StartTimeUTC.Time)))
		}
	}); err != nil {
		return nil, fmt.Errorf("observe task results: %w", err)
	}
	if err := scheduler.Start(ctx); err != nil {
		return nil, fmt.Errorf("start task scheduler: %w", err)
	}
	// Image serving needs the process-owned libvips runtime, which is opened
	// once in runServer and injected; construction tests omit it and the image
	// routes answer 503. Remote provider artwork is downloaded on demand into
	// the image cache. The hardened image downloader enforces http(s) only,
	// dial-time rejection of non-global-unicast addresses, and image content
	// validation (ring security/remote-image-download@v1) for both the serving
	// path and the identify Download endpoint.
	// remoteImages is hoisted out of the imageProcessor != nil branch so the
	// identify download path works without libvips (WP-P8-05 §6.2).
	imageDownloader := identify.NewHardenedDownloader(30 * time.Second)
	remoteImages := imageserve.NewRemoteCache(imageDownloader, filepath.Join(paths.Cache, "images", "remote"))
	var imageRenderer handlers.ImageRenderer
	if appConfig.imageProcessor != nil {
		imageRenderer = imageserve.New(appConfig.imageProcessor, remoteImages)
	}
	identifyService := identify.New(identify.Options{
		Items:      items,
		Topology:   topology,
		Refresher:  refresher,
		Lookups:    lookupRegistry,
		Remote:     remoteImages,
		Downloader: imageDownloader,
		Config:     manager,
		Clock:      realClock,
		Changes:    scanner.Changes(),
		Logger:     identify.SlogLogger{Logger: slog.Default()},
	})
	playGrants := playgrant.New(realClock)
	authorizer := middleware.NewAuthorizer(resolver, evaluator, users).WithPlaySessionGrants(playGrants)
	libraryChanges := library.NewChangeQueue(realClock, func() time.Duration {
		return time.Duration(manager.System().LibraryMonitorDelay) * time.Second
	}, refreshTriggerAdapter{scanner: scanner, scheduler: scheduler}.Trigger, slog.Default())
	setupRandom := appConfig.setupRandom
	if setupRandom == nil {
		setupRandom = rand.Reader
	}
	bundledWebDir := defaultBundledWebDir
	if appConfig.bundledWebDir != nil {
		bundledWebDir = *appConfig.bundledWebDir
	}
	webClient := resolveWebClient(paths.Root, bundledWebDir)
	logWebClient(ctx, slog.Default(), webClient)
	setupGate, err := newSetupGate(manager, network, &knownProxies{manager: manager}, setupRandom, slog.Default())
	if err != nil {
		return nil, err
	}
	rt := router.New(setupGate.Wrap, middleware.NewSlowRequestDiagnostics(realClock, stores, slog.Default(), 2*time.Second).Wrap, middleware.NoCache("GetBitrateTestBytes", "GetEndpointInfo", "NATIVE:GetBuild"), authorizer.Wrap, middleware.NewSessionActivity(sessions).Wrap)
	registerRoutes(rt, routeServices{
		identity:           identityConfig,
		systemIdentity:     func() config.ServerIdentity { return configuredServerIdentity(identityConfig, manager) },
		webSocketPort:      listenPort(listen),
		localAddress:       listen,
		paths:              paths,
		webPath:            webClient.Path,
		webComponent:       webClient.Component,
		startup:            manager,
		manager:            manager,
		clock:              realClock,
		network:            network,
		playGrants:         playGrants,
		transcoding:        transcodingState{manager: transcodes},
		users:              users,
		apiKeys:            identity,
		localization:       localizationService,
		refresh:            refreshTriggerAdapter{scanner: scanner, scheduler: scheduler},
		libraryChanges:     libraryChanges,
		topology:           topology,
		login:              login,
		sessions:           sessions,
		displayPreferences: displayPreferences,
		playback:           playbackService,
		playstate:          playstateApplication,
		directFiles:        playbackService,
		progressive:        progressiveService,
		hls:                hlsService,
		subtitles:          subtitleService,
		attachments:        attachmentService,
		trickplay:          trickplayReader,
		downloadFiles:      playbackService,
		items:              items,
		itemBuilder:        itemBuilder,
		views:              views,
		playlists:          playlistService,
		collections:        scanner,
		images:             imageRenderer,
		webSockets:         webSockets,
		virtualPaths:       virtualPaths,
		sessionControl:     sessionController,
		lookup:             identify.NewLookupAdapter(identifyService),
		remoteImages:       identify.NewRemoteImagesAdapter(identifyService),
		metadataPlugins:    providers.Catalog{Registry: refresher, ImageFetchers: []providers.ImageFetcherSource{dynamicImages}},
		itemDeleter: itemdelete.New(itemdelete.Options{
			Items:         items,
			Users:         users,
			Libraries:     topology,
			Publisher:     scanner.Changes(),
			MetadataPath:  paths.Metadata,
			TrickplayPath: paths.Trickplay,
			Logger:        slog.Default(),
		}),
		people: personRefresher(peopleService),
		itemEditor: itemedit.New(itemedit.Options{
			Items:        items,
			Libraries:    topology,
			Config:       manager,
			Localization: localizationService,
			Publisher:    scanner.Changes(),
			Clock:        realClock,
			MetadataPath: paths.Metadata,
			Paths:        virtualPaths,
		}),
		subtitleEditor: subtitleedit.New(subtitleedit.Options{
			Items:        items,
			Libraries:    topology,
			Access:       store.NewPlaybackItemAccessReader(items, users),
			Publisher:    scanner.Changes(),
			MetadataPath: paths.Metadata,
			Logger:       slog.Default(),
		}),
		scheduler: scheduler,
		activity:  activity,
		setupGate: setupGate,
	})
	return &application{
		libraryChanges:       libraryChanges,
		router:               rt,
		manager:              manager,
		configuration:        configurationApplier,
		transcodeEncoding:    transcodeEncoding,
		users:                users,
		deviceActivity:       deviceActivity,
		stores:               stores,
		changes:              changes,
		userDataChanges:      userDataChanges,
		activity:             activity,
		tasks:                scheduledTasks,
		topology:             topology,
		scanner:              scanner,
		sessions:             sessions,
		webSockets:           webSockets,
		listenScrobbler:      listenScrobbler,
		transcodes:           transcodes,
		progressive:          progressiveService,
		hls:                  hlsService,
		keyframes:            lazyKeyframes,
		trickplayRunner:      trickplayRunner,
		trickplayTask:        trickplayTask,
		trickplayReader:      trickplayReader,
		trickplayGenerator:   trickplayGenerator,
		trickplayPublication: trickplayPublication,
		scheduler:            scheduler,
	}, nil
}

func dispatchRefreshTrickplay(result library.RefreshResult, trigger func(dto.UUID) error) {
	for _, itemID := range result.RegenerateTrickplayItemIDs {
		if err := trigger(itemID); err != nil {
			log.Printf("schedule requested trickplay regeneration for %s: %v", itemID.Dashed(), err)
		}
	}
}

// applicationOption customizes optional, process-owned dependencies that are
// unsafe to construct per application instance. The image processor owns the
// single process-lifetime libvips runtime, so it is opened once in runServer
// and injected here rather than constructed inside newApplicationWithListen
// (which construction tests call repeatedly).
type applicationOption func(*applicationConfig)

type applicationConfig struct {
	imageProcessor       imageserve.Processor
	tileComposer         *images.TileComposer
	encodingCapabilities encoding.Capabilities
	hardwareProbe        func(context.Context, encoding.Capabilities, config.EncodingOptions) encoding.Capabilities
	configurationApplied func(config.Change)
	scheduledTasks       []scheduledTaskRegistration
	providerRemoteDoer   remote.Doer
	setupRandom          io.Reader
	databaseURL          string
	bundledWebDir        *string
}

// scheduledTaskRegistration stores one task definition and its runnable for
// pre-start composition registration.
type scheduledTaskRegistration struct {
	def tasks.Definition
	run tasks.Runnable
}

// withScheduledTask appends one configured task registration in option order.
// The application registers configured tasks after built-ins and before
// scheduler.Start.
func withScheduledTask(def tasks.Definition, run tasks.Runnable) applicationOption {
	return func(config *applicationConfig) {
		config.scheduledTasks = append(config.scheduledTasks, scheduledTaskRegistration{def: def, run: run})
	}
}

// withProviderRemoteDoer injects a test-only HTTP transport for every remote
// metadata provider client. The production value is nil, which retains the
// default HTTP client. Composition tests pass a recorded cassette Doer so
// no live API is contacted.
func withProviderRemoteDoer(doer remote.Doer) applicationOption {
	return func(config *applicationConfig) { config.providerRemoteDoer = doer }
}

// withDatabaseURL selects the store (see parseDatabase). The zero value keeps
// SQLite in the program-data directory.
func withDatabaseURL(raw string) applicationOption {
	return func(config *applicationConfig) { config.databaseURL = raw }
}

// withBundledWebDir replaces defaultBundledWebDir, the web client directory
// that /web/ serves when <data root>/web does not exist. An empty path
// disables the bundled client. Composition tests use it to keep the result
// independent of the host's /usr/share.
func withBundledWebDir(path string) applicationOption {
	return func(config *applicationConfig) {
		config.bundledWebDir = &path
	}
}

// withSetupRandom injects the entropy source of the first-run setup token, so
// a composition test knows the token. Production uses crypto/rand.
func withSetupRandom(random io.Reader) applicationOption {
	return func(config *applicationConfig) { config.setupRandom = random }
}

// progressiveTranscodeOpener adapts playback's request-shaped process seam to
// the application-owned ffmpeg process manager.
type progressiveTranscodeOpener struct{ manager *ffmpeg.TranscodeManager }

func (opener progressiveTranscodeOpener) Open(ctx context.Context, request playback.ProgressiveOpenRequest) (io.ReadCloser, error) {
	if opener.manager == nil {
		return nil, ffmpeg.ErrManagerClosed
	}
	return opener.manager.OpenProgressive(ctx, ffmpeg.ProgressiveRequest{
		OutputPath:     request.OutputPath,
		Args:           request.Args,
		PlaySessionID:  request.PlaySessionID,
		DeviceID:       request.DeviceID,
		SourceRevision: request.SourceRevision,
		OpenSource:     request.OpenSource,
	})
}

// hlsManager is the narrow lifecycle surface shared by the HLS adapter and
// the application-owned transcode manager.
type hlsManager interface {
	StartHLS(context.Context, ffmpeg.HLSRequest) error
	RestartHLSAt(context.Context, ffmpeg.HLSRequest, int) error
	WaitSegment(context.Context, string, int, ...*ffmpeg.HLSSegmentTiming) (io.ReadCloser, error)
	WaitPlaylist(context.Context, string, int) (string, error)
	PingPlaySession(string) bool
	StopPlaySession(context.Context, string, string) error
	OpenLegacyHLS(context.Context, string, string) (io.ReadCloser, error)
}

// hlsTranscodeManager translates playback's process-neutral request into the
// one manager the application owns for both progressive and HLS processes.
type hlsTranscodeManager struct{ manager hlsManager }

func (adapter hlsTranscodeManager) StartHLS(ctx context.Context, request playback.HLSJobRequest) error {
	if adapter.manager == nil {
		return ffmpeg.ErrManagerClosed
	}
	return adapter.manager.StartHLS(ctx, adapter.request(request))
}

func (adapter hlsTranscodeManager) RestartHLSAt(ctx context.Context, request playback.HLSJobRequest, index int) error {
	if adapter.manager == nil {
		return ffmpeg.ErrManagerClosed
	}
	return adapter.manager.RestartHLSAt(ctx, adapter.request(request), index)
}

func (adapter hlsTranscodeManager) WaitSegment(ctx context.Context, playlist string, index int, timing *playback.HLSSegmentTiming) (io.ReadCloser, error) {
	if adapter.manager == nil {
		return nil, ffmpeg.ErrManagerClosed
	}
	var ffmpegTiming *ffmpeg.HLSSegmentTiming
	if timing != nil {
		ffmpegTiming = &ffmpeg.HLSSegmentTiming{
			RuntimeTicks:             timing.RuntimeTicks,
			ActualSegmentLengthTicks: timing.ActualSegmentLengthTicks,
		}
	}
	return adapter.manager.WaitSegment(ctx, playlist, index, ffmpegTiming)
}

func (adapter hlsTranscodeManager) WaitPlaylist(ctx context.Context, playlist string, minimum int) (string, error) {
	if adapter.manager == nil {
		return "", ffmpeg.ErrManagerClosed
	}
	return adapter.manager.WaitPlaylist(ctx, playlist, minimum)
}

func (adapter hlsTranscodeManager) PingPlaySession(playSessionID string) bool {
	return adapter.manager != nil && adapter.manager.PingPlaySession(playSessionID)
}

func (adapter hlsTranscodeManager) StopPlaySession(ctx context.Context, playSessionID, deviceID string) error {
	if adapter.manager == nil {
		return ffmpeg.ErrManagerClosed
	}
	return adapter.manager.StopPlaySession(ctx, playSessionID, deviceID)
}

func (adapter hlsTranscodeManager) OpenLegacyHLS(ctx context.Context, playlistID, artifactName string) (io.ReadCloser, error) {
	if adapter.manager == nil {
		return nil, ffmpeg.ErrManagerClosed
	}
	return adapter.manager.OpenLegacyHLS(ctx, playlistID, artifactName)
}

func (adapter hlsTranscodeManager) request(request playback.HLSJobRequest) ffmpeg.HLSRequest {
	return ffmpeg.HLSRequest{
		PlaylistPath: request.PlaylistPath, Live: request.Live, MinSegments: request.MinSegments,
		SegmentPattern: request.SegmentPattern, Args: request.Args, IdentityArgs: request.IdentityArgs,
		PlaySessionID: request.PlaySessionID,
		DeviceID:      request.DeviceID, MediaSourceID: request.MediaSourceID, StartTimeTicks: request.StartTimeTicks,
		SegmentLengthSeconds: request.SegmentLengthSeconds, MaxForwardGap: request.MaxForwardGap, RunTimeTicks: request.RunTimeTicks,
		SegmentStartTicks: request.SegmentStartTicks,
		SourceRevision:    request.SourceRevision, OpenSource: request.OpenSource,
		EmbeddedSubtitleSource: request.EmbeddedSubtitleSource,
		ExternalSubtitleSource: request.ExternalSubtitleSource, ExternalSubtitleRevision: request.ExternalSubtitleRevision,
		OpenExternalSubtitle: request.OpenExternalSubtitle,
		TranscodingInfo:      request.TranscodingInfo,
	}
}

var errLazyKeyframeSourceClosed = errors.New("lazy keyframe source is closed")

// keyframeFileExtractor reads keyframe data from a borrowed source descriptor.
type keyframeFileExtractor interface {
	Extract(ctx context.Context, source *os.File) (keyframes.Data, error)
}

// lazyKeyframeSource owns coalesced extraction workers for the whole
// application lifetime. An extraction survives the cancellation of every
// caller and persists its result; only Close cancels it.
type lazyKeyframeSource struct {
	items     *store.ItemStore
	extractor keyframeFileExtractor

	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu        sync.Mutex
	closed    bool
	inFlight  map[dto.UUID]*keyframeExtraction
	workers   sync.WaitGroup
	workerCnt int
	closeOnce sync.Once
	closeDone chan struct{}
}

type keyframeExtraction struct {
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	waiters int
	data    keyframes.Data
	found   bool
	err     error
}

func newLazyKeyframeSource(items *store.ItemStore, extractor keyframeFileExtractor) *lazyKeyframeSource {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	return &lazyKeyframeSource{
		items: items, extractor: extractor, rootCtx: rootCtx, rootCancel: rootCancel,
		inFlight: make(map[dto.UUID]*keyframeExtraction), closeDone: make(chan struct{}),
	}
}

// Close rejects new requests, cancels every active worker, and waits until all
// workers have stopped using the store. It is safe to retry after a caller's
// drain context expires.
func (source *lazyKeyframeSource) Close(ctx context.Context) error {
	if source == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	source.closeOnce.Do(func() {
		source.mu.Lock()
		source.closed = true
		source.rootCancel()
		workersLive := source.workerCnt != 0
		source.mu.Unlock()
		if !workersLive {
			source.workers.Wait()
			close(source.closeDone)
			return
		}
		go func() {
			source.workers.Wait()
			close(source.closeDone)
		}()
	})
	select {
	case <-source.closeDone:
		return nil
	default:
	}
	select {
	case <-source.closeDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Keyframes reads persisted data first, then coalesces one extraction per
// selected media source before atomically making that data durable. A source
// without container keyframes is a miss, as in CacheDecorator.cs.
func (source *lazyKeyframeSource) Keyframes(ctx context.Context, itemID dto.UUID, mediaSourceID string) (keyframes.Data, bool, error) {
	selectedID := itemID
	if parsed, err := dto.ParseUUID(mediaSourceID); err == nil && !parsed.IsNil() {
		selectedID = parsed
	}
	if selectedID.IsNil() {
		return keyframes.Data{}, false, nil
	}
	source.mu.Lock()
	closed := source.closed
	source.mu.Unlock()
	if closed {
		return keyframes.Data{}, false, errLazyKeyframeSourceClosed
	}
	stored, err := source.items.Keyframes(ctx, selectedID)
	if err == nil {
		return keyframes.Data{TotalDurationTicks: stored.TotalDurationTicks, KeyframeTicks: stored.KeyframeTicks}, true, nil
	}
	if !store.IsNotFound(err) {
		return keyframes.Data{}, false, err
	}

	source.mu.Lock()
	if source.closed {
		source.mu.Unlock()
		return keyframes.Data{}, false, errLazyKeyframeSourceClosed
	}
	call := source.inFlight[selectedID]
	if call == nil || call.ctx.Err() != nil {
		workerCtx, cancel := context.WithCancel(source.rootCtx)
		call = &keyframeExtraction{done: make(chan struct{}), ctx: workerCtx, cancel: cancel}
		source.inFlight[selectedID] = call
		source.workers.Add(1)
		source.workerCnt++
		go source.extractKeyframes(selectedID, call)
	}
	call.waiters++
	source.mu.Unlock()
	defer source.releaseKeyframeWaiter(call)
	select {
	case <-call.done:
		return call.data, call.found, call.err
	case <-ctx.Done():
		return keyframes.Data{}, false, ctx.Err()
	}
}

func (source *lazyKeyframeSource) extractKeyframes(itemID dto.UUID, call *keyframeExtraction) {
	defer func() {
		call.cancel()
		source.mu.Lock()
		if source.inFlight[itemID] == call {
			delete(source.inFlight, itemID)
		}
		close(call.done)
		source.workerCnt--
		source.mu.Unlock()
		source.workers.Done()
	}()
	snapshot, err := source.items.DerivedSourceSnapshot(call.ctx, itemID)
	if err != nil {
		call.err = err
		return
	}
	if snapshot.FileInode == nil || snapshot.FileChangedAt == nil {
		log.Printf("discard stale keyframe source for %s", itemID.Dashed())
		return
	}
	if snapshot.Path == nil || *snapshot.Path == "" {
		call.err = fmt.Errorf("keyframe source %s has no local path", itemID.Dashed())
		return
	}
	src, err := sourcefile.Open(*snapshot.Path, sourcefile.Expected{Size: snapshot.Size, ModifiedAt: snapshot.DateModified, Inode: snapshot.FileInode, ChangedAt: snapshot.FileChangedAt})
	if err != nil {
		if errors.Is(err, sourcefile.ErrStaleSource) {
			log.Printf("discard stale keyframe source for %s", itemID.Dashed())
			return
		}
		call.err = err
		return
	}
	defer func() {
		if err := src.Close(); err != nil {
			log.Printf("close keyframe source for %s: %v", itemID.Dashed(), err)
		}
	}()

	data, err := source.extractor.Extract(call.ctx, src.File())
	if errors.Is(err, keyframes.ErrNoKeyframes) {
		return
	}
	if err != nil {
		log.Printf("extract keyframes for %s: %v", itemID.Dashed(), err)
		call.err = err
		return
	}
	if err = src.VerifyUnchanged(); err == nil {
		err = src.VerifyPathStillCurrent()
	}
	if err != nil {
		if errors.Is(err, sourcefile.ErrStaleSource) {
			log.Printf("discard stale keyframe source for %s", itemID.Dashed())
			return
		}
		call.err = err
		return
	}
	if err := source.items.ReplaceKeyframes(call.ctx, store.KeyframeInfo{ItemID: itemID, TotalDurationTicks: data.TotalDurationTicks, KeyframeTicks: data.KeyframeTicks, SourceRevision: snapshot.SourceRevision}); err != nil {
		if errors.Is(err, store.ErrStaleSourceRevision) {
			log.Printf("discard stale keyframe extraction for %s", itemID.Dashed())
			return
		}
		call.err = err
		return
	}
	call.data, call.found = data, true
}

// releaseKeyframeWaiter does not cancel the extraction when the last waiter
// leaves: a retry must find the persisted result, not start again.
func (source *lazyKeyframeSource) releaseKeyframeWaiter(call *keyframeExtraction) {
	source.mu.Lock()
	defer source.mu.Unlock()
	call.waiters--
}

type trickplayStoreAdapter struct{ items *store.ItemStore }

func (adapter trickplayStoreAdapter) Trickplay(ctx context.Context, itemID dto.UUID, width int) (trickplay.Metadata, error) {
	value, err := adapter.items.Trickplay(ctx, itemID, width)
	return trickplayMetadata(value), err
}

func (adapter trickplayStoreAdapter) ReplaceTrickplay(ctx context.Context, value trickplay.Metadata) error {
	err := adapter.items.ReplaceTrickplay(ctx, store.TrickplayInfo{
		ItemID: value.ItemID, Width: value.Width, Height: value.Height, TileWidth: value.TileWidth, TileHeight: value.TileHeight,
		ThumbnailCount: value.ThumbnailCount, IntervalMS: value.Interval, Bandwidth: value.Bandwidth, SourceRevision: value.SourceRevision, GenerationToken: value.GenerationToken,
	})
	if errors.Is(err, store.ErrStaleSourceRevision) {
		return fmt.Errorf("replace trickplay metadata: %w: %w", trickplay.ErrStaleSourceRevision, err)
	}
	return err
}

func (adapter trickplayStoreAdapter) TrickplayMetadata(ctx context.Context, itemID dto.UUID) ([]playback.HLSTrickplayMetadata, error) {
	values, err := adapter.items.Trickplays(ctx, itemID)
	if err != nil {
		return nil, err
	}
	result := make([]playback.HLSTrickplayMetadata, len(values))
	for index, value := range values {
		result[index] = playback.HLSTrickplayMetadata{Width: value.Width, Height: value.Height, Bandwidth: int(value.Bandwidth)}
	}
	return result, nil
}

func trickplayMetadata(value store.TrickplayInfo) trickplay.Metadata {
	return trickplay.Metadata{
		ItemID: value.ItemID, Width: value.Width, Height: value.Height, TileWidth: value.TileWidth, TileHeight: value.TileHeight,
		ThumbnailCount: value.ThumbnailCount, Interval: value.IntervalMS, Bandwidth: value.Bandwidth, GenerationToken: value.GenerationToken,
	}
}

type trickplayVisibilityAdapter struct {
	access store.PlaybackItemAccessReader
}

func (adapter trickplayVisibilityAdapter) Visible(ctx context.Context, userID, itemID dto.UUID, administrator bool) (bool, error) {
	if administrator {
		return true, nil
	}
	visible, err := adapter.access.AccessiblePlaybackItemIDs(ctx, userID, []dto.UUID{itemID})
	return len(visible) == 1, err
}

// ReadTrickplay supplies durable-recovery with an exact persisted rendition.
// A missing row is not an error because no post-publication commit occurred.
func (adapter trickplayStoreAdapter) ReadTrickplay(ctx context.Context, itemID dto.UUID, width int) (*trickplay.Metadata, error) {
	value, err := adapter.items.Trickplay(ctx, itemID, width)
	if store.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	metadata := trickplayMetadata(value)
	return &metadata, nil
}

type trickplayTaskSource struct {
	items    *store.ItemStore
	topology *library.Topology
	options  tasks.Options
}

func (source trickplayTaskSource) Snapshot(ctx context.Context) (tasks.Candidates, error) {
	isFolder, isVirtual := false, false
	result, err := source.items.Query(ctx, store.ItemQuery{
		MediaTypes: []item.MediaType{item.MediaTypeVideo}, SourceTypes: []item.SourceType{item.SourceTypeLibrary},
		IsFolder: &isFolder, IsVirtualItem: &isVirtual, Recursive: true, IncludeOwnedItems: true,
	})
	if err != nil {
		return nil, err
	}
	folders, err := source.topology.VirtualFolders(ctx)
	if err != nil {
		return nil, err
	}
	// items.top_parent_id names the library's CollectionFolder item, not the
	// library registry ID (see identify.Service).
	libraries := make(map[dto.UUID]library.LibraryOptions, len(folders))
	for _, folder := range folders {
		libraries[folder.CollectionFolderID] = folder.Options
	}
	values := make([]tasks.Candidate, 0, len(result.Items))
	for _, value := range result.Items {
		candidate, err := source.candidate(ctx, value, libraries)
		if err != nil {
			return nil, err
		}
		if candidate.ID.IsNil() {
			continue
		}
		values = append(values, candidate)
	}
	return trickplayCandidates{values: values}, nil
}

func (source trickplayTaskSource) candidate(ctx context.Context, value item.Item, libraries map[dto.UUID]library.LibraryOptions) (tasks.Candidate, error) {
	snapshot, err := source.items.DerivedSourceSnapshot(ctx, value.ID)
	if err != nil {
		if store.IsNotFound(err) {
			return tasks.Candidate{}, nil
		}
		return tasks.Candidate{}, err
	}
	candidate := tasks.Candidate{ID: value.ID, SourceRevision: snapshot.SourceRevision}
	if value.Name != nil {
		candidate.Name = *value.Name
	}
	if snapshot.Path != nil {
		candidate.SourcePath = *snapshot.Path
	}
	if snapshot.Size != nil {
		size := *snapshot.Size
		candidate.SourceSize = &size
	}
	if snapshot.DateModified != nil {
		modified := *snapshot.DateModified
		candidate.SourceModified = &modified
	}
	if snapshot.FileInode != nil {
		inode := *snapshot.FileInode
		candidate.SourceInode = &inode
	}
	if snapshot.FileChangedAt != nil {
		changedAt := *snapshot.FileChangedAt
		candidate.SourceChangedAt = &changedAt
	}
	if snapshot.Width != nil {
		width := *snapshot.Width
		candidate.VideoWidth = &width
	}
	if snapshot.RunTimeTicks != nil {
		runTimeTicks := *snapshot.RunTimeTicks
		candidate.RunTimeTicks = &runTimeTicks
	}
	if snapshot.ReferenceFrameRate != nil {
		referenceFrameRate := *snapshot.ReferenceFrameRate
		candidate.ReferenceFrameRate = &referenceFrameRate
	}
	if snapshot.FileInode == nil || snapshot.FileChangedAt == nil || value.TopParentID == nil || candidate.SourcePath == "" {
		return candidate, nil
	}
	libraryOptions := libraries[*value.TopParentID]
	canGenerate := (trickplay.Manager{}).CanGenerate(candidate.RunTimeTicks, source.options.IntervalMilliseconds)
	// RefreshTrickplayDataAsync discovers existing trickplay before it
	// checks EnableTrickplayImageExtraction.
	candidate.Discover = canGenerate && libraryOptions.SaveTrickplayWithMedia
	if !libraryOptions.EnableTrickplayImageExtraction {
		return candidate, nil
	}
	candidate.ExtractDuringScan = libraryOptions.ExtractTrickplayImagesDuringLibraryScan
	if !canGenerate {
		return candidate, nil
	}
	persisted, err := source.items.Trickplays(ctx, value.ID)
	if err != nil {
		return tasks.Candidate{}, err
	}
	for _, requestedWidth := range source.options.Widths {
		effectiveWidth := effectiveTrickplayWidth(requestedWidth, candidate.VideoWidth)
		for _, rendition := range persisted {
			if rendition.Width != effectiveWidth {
				continue
			}
			if !hasTrickplayWidth(candidate.ExistingWidths, effectiveWidth) {
				candidate.ExistingWidths = append(candidate.ExistingWidths, effectiveWidth)
			}
			break
		}
	}
	candidate.Generate = true
	return candidate, nil
}

func effectiveTrickplayWidth(requested int, videoWidth *int) int {
	if videoWidth != nil && *videoWidth < requested {
		requested = *videoWidth
	}
	return requested / 2 * 2
}

func hasTrickplayWidth(widths []int, width int) bool {
	for _, existing := range widths {
		if existing == width {
			return true
		}
	}
	return false
}

type trickplayCandidates struct{ values []tasks.Candidate }

func (candidates trickplayCandidates) Count() int { return len(candidates.values) }

func (candidates trickplayCandidates) Page(ctx context.Context, start, limit int) ([]tasks.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if start < 0 || limit < 0 || start >= len(candidates.values) {
		return nil, nil
	}
	end := min(start+limit, len(candidates.values))
	return candidates.values[start:end], nil
}

type trickplayGenerationExecutor interface {
	Generate(context.Context, trickplay.GenerateRequest) (trickplay.Metadata, error)
}

// trickplayDiscoveryStore persists discovered media-side trickplay.
type trickplayDiscoveryStore interface {
	Trickplays(context.Context, dto.UUID) ([]store.TrickplayInfo, error)
	ReplaceTrickplay(context.Context, store.TrickplayInfo) error
}

type trickplayTaskGenerator struct {
	generator trickplayGenerationExecutor
	// discovery, when set, catalogues trickplay saved with the media;
	// interval is the configured TrickplayOptions.Interval.
	discovery trickplayDiscoveryStore
	interval  int64
}

// Discover ports TrickplayManager.DiscoverExistingTrickplayAsync for a
// library that saves trickplay with the media: each media-side resolution
// whose width has no row is persisted, so the task does not generate it
// again and the reader serves its tiles in place.
func (adapter trickplayTaskGenerator) Discover(ctx context.Context, candidate tasks.Candidate) ([]int, error) {
	if adapter.discovery == nil {
		return nil, nil
	}
	persisted, err := adapter.discovery.Trickplays(ctx, candidate.ID)
	if err != nil {
		return nil, fmt.Errorf("read trickplay metadata: %w", err)
	}
	existing := make([]int, len(persisted))
	for index, value := range persisted {
		existing[index] = value.Width
	}
	discovered, err := trickplay.DiscoverMedia(ctx, trickplay.DiscoverRequest{
		ItemID: candidate.ID, MediaPath: candidate.SourcePath, RunTimeTicks: candidate.RunTimeTicks,
		IntervalMilliseconds: adapter.interval, ExistingWidths: existing,
	})
	if err != nil {
		return nil, err
	}
	widths := make([]int, 0, len(discovered))
	for _, value := range discovered {
		token, err := discoveredTrickplayToken()
		if err != nil {
			return widths, err
		}
		if err := adapter.discovery.ReplaceTrickplay(ctx, store.TrickplayInfo{
			ItemID: value.ItemID, SourceRevision: candidate.SourceRevision, Width: value.Width, Height: value.Height,
			TileWidth: value.TileWidth, TileHeight: value.TileHeight, ThumbnailCount: value.ThumbnailCount,
			IntervalMS: value.Interval, Bandwidth: value.Bandwidth, GenerationToken: token,
		}); err != nil {
			return widths, fmt.Errorf("persist discovered trickplay width %d: %w", value.Width, err)
		}
		slog.Info("discovered existing trickplay",
			slog.String("item_id", candidate.ID.Dashed()), slog.String("path", candidate.SourcePath),
			slog.Int("width", value.Width), slog.Int("tile_width", value.TileWidth), slog.Int("tile_height", value.TileHeight),
			slog.Int("thumbnails", value.ThumbnailCount), slog.Int64("interval_ms", value.Interval))
		widths = append(widths, value.Width)
	}
	return widths, nil
}

// discoveredTrickplayToken names a discovered rendition. The tiles are not
// Deadfin's publication, so the token only has to change when the row does.
func discoveredTrickplayToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create trickplay generation token: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// trickplayAcceleration honours TrickplayOptions.EnableHwAcceleration and
// EnableHwEncoding through the encoding.xml hardware selection, as
// MediaEncoder.ExtractVideoImagesOnIntervalAccelerated does. The options and
// capabilities are read for each extraction, so a dashboard save applies to
// the next item.
func trickplayAcceleration(manager *config.Manager, capabilities func() encoding.Capabilities, items *store.ItemStore) trickplay.Acceleration {
	return func(ctx context.Context, itemID dto.UUID) (encoding.TrickplayExtraction, encoding.Capabilities, error) {
		options := manager.System().TrickplayOptions
		if !options.EnableHwAcceleration {
			return encoding.TrickplayExtraction{}, encoding.Capabilities{}, nil
		}
		streams, err := items.Streams(ctx, itemID)
		if err != nil {
			return encoding.TrickplayExtraction{}, encoding.Capabilities{}, fmt.Errorf("read media streams: %w", err)
		}
		job := encoding.TrickplayExtraction{
			AllowHardwareAcceleration: true,
			EnableHardwareEncoding:    options.EnableHwEncoding,
			Hardware:                  hardwareOptions(manager.Encoding(), renderNodeExists),
		}
		for _, stream := range streams {
			if stream.Type != playback.StreamTypeVideo {
				continue
			}
			job.VideoStream = playback.EncodingVideoStream(stream)
			if stream.AspectRatio != nil {
				job.AspectRatio = *stream.AspectRatio
			}
			break
		}
		return job, capabilities(), nil
	}
}

// trickplayMediaPaths resolves an item's media file for the trickplay reader.
type trickplayMediaPaths struct{ items *store.ItemStore }

func (paths trickplayMediaPaths) MediaPath(ctx context.Context, itemID dto.UUID) (string, error) {
	snapshot, err := paths.items.DerivedSourceSnapshot(ctx, itemID)
	if err != nil {
		return "", err
	}
	if snapshot.Path == nil {
		return "", nil
	}
	return *snapshot.Path, nil
}

func (adapter trickplayTaskGenerator) Generate(ctx context.Context, generation tasks.Generation) (err error) {
	if generation.Candidate.SourceInode == nil || generation.Candidate.SourceChangedAt == nil {
		return fmt.Errorf("open trickplay source: %w", trickplay.ErrStaleSourceRevision)
	}
	src, err := sourcefile.Open(generation.Candidate.SourcePath, sourcefile.Expected{
		Size: generation.Candidate.SourceSize, ModifiedAt: generation.Candidate.SourceModified,
		Inode: generation.Candidate.SourceInode, ChangedAt: generation.Candidate.SourceChangedAt,
	})
	if err != nil {
		if errors.Is(err, sourcefile.ErrStaleSource) {
			return fmt.Errorf("open trickplay source: %w: %w", trickplay.ErrStaleSourceRevision, err)
		}
		return fmt.Errorf("open trickplay source: %w", err)
	}
	defer func() { err = errors.Join(err, src.Close()) }()

	_, err = adapter.generator.Generate(ctx, trickplay.GenerateRequest{
		ItemID: generation.Candidate.ID, Source: src, SourceRevision: generation.SourceRevision,
		VideoWidth: generation.Candidate.VideoWidth, RunTimeTicks: generation.Candidate.RunTimeTicks, RequestedWidth: generation.RequestedWidth,
		TileWidth: generation.TileWidth, TileHeight: generation.TileHeight, IntervalMilliseconds: generation.IntervalMilliseconds,
		JPEGQuality: generation.Quality, ReferenceFrameRate: generation.ReferenceFrameRate,
	})
	return err
}

// trickplayScanExecutable gives the scan runner TrickplayProvider scope.
type trickplayScanExecutable struct{ task *tasks.Task }

func (executable trickplayScanExecutable) Execute(ctx context.Context) error {
	return executable.task.ExecuteScan(ctx)
}

func (executable trickplayScanExecutable) RequestRegenerateTrickplay(itemID dto.UUID) error {
	return executable.task.RequestRegenerateTrickplay(itemID)
}

type trickplayTaskReporter struct{}

func (trickplayTaskReporter) Progress(progress float64) {
	log.Printf("trickplay refresh: %.0f%%", progress)
}

func (trickplayTaskReporter) ItemError(candidate tasks.Candidate, err error) {
	slog.Warn("trickplay refresh failed for an item; continuing with the next item",
		slog.String("item_id", candidate.ID.Dashed()), slog.String("name", candidate.Name),
		slog.String("path", candidate.SourcePath), slog.String("error", err.Error()))
}

// libraryScanAdapter adapts the scanner's synchronous Scan method to the
// tasks.ScanLibrary seam. It calls Scan(ctx), not Trigger(), so scheduler
// cancellation propagates (internal/library/scanner.go:314-319). It folds the
// top-level scan error together with every per-folder error in RefreshResult,
// so a folder failure produces a Failed task result with a visible message.
type libraryScanAdapter struct{ scanner *library.Scanner }

func (a libraryScanAdapter) Scan(ctx context.Context, progress func(float64)) (any, error) {
	if a.scanner == nil {
		return nil, nil
	}
	result, err := a.scanner.ScanWithProgress(ctx, library.DefaultRefreshOptions(), progress)
	for _, folderErr := range result.Errors {
		if errors.Is(folderErr, library.ErrItemSkipped) {
			log.Printf("library scan: %v", folderErr)
		}
	}
	return result, foldRefreshErrors(err, result.Errors)
}

// metadataRefreshAdapter adapts the scanner's remote-backfill refresh mode to
// the tasks.MetadataRefresher seam. It runs RefreshExistingMetadata, which
// enumerates and resolves the configured library paths under a full metadata
// and image refresh (no replace-all) so existing locked metadata and local
// images are preserved while provider IDs and remote artwork backfill. The
// adapter joins topology and folder errors, then returns an error when the
// remote stage makes no invocation or persists no provider ID or image. This
// prevents a false-success result that did no provider work.
type metadataRefreshAdapter struct{ scanner *library.Scanner }

func (a metadataRefreshAdapter) RefreshMetadata(ctx context.Context, progress func(float64)) error {
	if a.scanner == nil {
		return nil
	}
	result, err := a.scanner.RefreshExistingMetadata(ctx, progress)
	log.Printf("refresh metadata: provider invocations=%d persisted_provider_ids=%d persisted_images=%d updated=%d skipped=%d",
		result.ProviderInvocations, result.PersistedProviderIDs, result.PersistedImages, result.BackfillUpdated, result.BackfillSkipped)
	for _, folderErr := range result.Errors {
		if errors.Is(folderErr, library.ErrItemSkipped) {
			log.Printf("refresh metadata: %v", folderErr)
		} else {
			log.Printf("refresh metadata: failed path: %v", folderErr)
		}
	}
	return foldRefreshErrors(err, result.Errors)
}

// foldRefreshErrors joins the top-level scan error with every per-folder error.
// A nil error with no folder errors stays nil, so a clean scan records
// Completed. A folder failure alone produces a non-nil error, so the
// scheduler records Failed with the folder path and cause. A skipped item
// (library.ErrItemSkipped) is logged by the caller and does not fail the
// task, as upstream logs a per-item failure and completes.
func foldRefreshErrors(err error, folderErrors []error) error {
	errs := make([]error, 0, 1+len(folderErrors))
	if err != nil {
		errs = append(errs, err)
	}
	for _, folderErr := range folderErrors {
		if !errors.Is(folderErr, library.ErrItemSkipped) {
			errs = append(errs, folderErr)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// refreshTriggerAdapter routes POST /Library/Refresh through the task
// scheduler instead of the scanner's async Trigger. It implements the
// refreshTrigger interface so the handler seam stays unchanged. Trigger
// calls scheduler.Run(tasks.RefreshLibraryID), which coalesces while a scan
// runs. When the scheduler is closed, Run returns tasks.ErrSchedulerClosed;
// the adapter translates that to library.ErrScannerClosed so the handler
// maps it to 503. Any other error returns as-is and maps to 500.
// TriggerRefresh and RefreshStates keep delegating to the scanner, so the
// /Library/VirtualFolders Active/Queued projection and POST
// /Items/{itemId}/Refresh stay unchanged.
type refreshTriggerAdapter struct {
	scanner   *library.Scanner
	scheduler *tasks.Scheduler
}

func (a refreshTriggerAdapter) Trigger() error {
	err := a.scheduler.Run(tasks.RefreshLibraryID)
	if errors.Is(err, tasks.ErrSchedulerClosed) {
		return library.ErrScannerClosed
	}
	return err
}

func (a refreshTriggerAdapter) TriggerRefresh(opts library.RefreshOptions) error {
	return a.scanner.TriggerRefresh(opts)
}

func (a refreshTriggerAdapter) RefreshStates() map[dto.UUID]library.FolderRefreshState {
	return a.scanner.RefreshStates()
}

// statisticsRefreshRows is the number of items a scan must insert, update,
// or delete before it asks the store to refresh its planner statistics.
const statisticsRefreshRows = 1000

// requestStatisticsRefresh asks the store for a planner statistics refresh
// after a scan that changed many items. The periodic refresh covers smaller
// scans.
func requestStatisticsRefresh(stores store.Stores, result library.RefreshResult) {
	if result.Inserted+result.Updated+result.Deleted < statisticsRefreshRows {
		return
	}
	if refresher, ok := stores.(store.StatisticsRefresher); ok {
		refresher.RequestStatisticsRefresh()
	}
}

// storeMaintainerAdapter adapts the store.Stores backend to the
// tasks.StoreMaintainer seam. The SQLite backend implements Maintain directly;
// backends without Maintainer log-skip (the task's Completed behavior) rather
// than running SQLite-specific PRAGMA/VACUUM statements on an unsupported
// backend (spec §4: "Backend without Maintainer → log-skip, Completed").
type storeMaintainerAdapter struct{ stores store.Stores }

func (a storeMaintainerAdapter) Maintain(ctx context.Context) error {
	if m, ok := a.stores.(store.Maintainer); ok {
		return m.Maintain(ctx)
	}
	log.Printf("application: OptimizeDatabase skipped: backend does not implement store.Maintainer")
	return nil
}

// userDataCleanerAdapter adapts the store.Stores backend to the
// tasks.UserDataCleaner seam. It calls store.DeleteDetachedUserData to
// remove orphaned user-data rows (upstream CleanupUserDataTask).
type userDataCleanerAdapter struct{ stores store.Stores }

func (a userDataCleanerAdapter) DeleteDetachedUserData(ctx context.Context) (int64, error) {
	return store.DeleteDetachedUserData(ctx, a.stores)
}

func withImageProcessor(processor imageserve.Processor) applicationOption {
	return func(config *applicationConfig) { config.imageProcessor = processor }
}

func withTileComposer(composer *images.TileComposer) applicationOption {
	return func(config *applicationConfig) { config.tileComposer = composer }
}

func withEncodingCapabilities(capabilities encoding.Capabilities) applicationOption {
	return func(config *applicationConfig) { config.encodingCapabilities = capabilities }
}

// withHardwareProbe replaces the device probe that runs at startup and after
// each encoding configuration save.
func withHardwareProbe(probe func(context.Context, encoding.Capabilities, config.EncodingOptions) encoding.Capabilities) applicationOption {
	return func(config *applicationConfig) { config.hardwareProbe = probe }
}

// withConfigurationApplied observes each configuration change after the
// configuration consumer applies it.
func withConfigurationApplied(applied func(config.Change)) applicationOption {
	return func(config *applicationConfig) { config.configurationApplied = applied }
}

// encodingCapabilities projects the immutable ffmpeg and NVIDIA device
// probes into the pure encoding facts consumed by both progressive and HLS
// argument builders. The NVENC encoders that the device rejects, and the
// cuda hwaccel when no device opens, are removed, so the builders select
// only what the GPU can run (D-37).
func encodingCapabilities(probed ffmpeg.Capabilities, nvidia ffmpeg.NvidiaCapabilities) encoding.Capabilities {
	capabilities := encoding.Capabilities{
		EncoderMajorVersion:            probed.Version.Major,
		EncoderMinorVersion:            probed.Version.Minor,
		EncoderPatchVersion:            probed.Version.Patch,
		SupportsSubtitleExtraction:     containsCapability(probed.Filters, "subtitles"),
		SupportsTonemapX:               containsCapability(probed.Filters, "tonemapx"),
		SupportsOverlayCudaAlphaFormat: nvidia.OverlayCudaAlphaFormat,
		SupportsPKeyPause:              probed.PKeyPause,
		Decoders:                       append([]string(nil), probed.Decoders...),
		Filters:                        append([]string(nil), probed.Filters...),
		BSFOptions:                     append([]string(nil), probed.BSFOptions...),
	}
	for _, name := range probed.Encoders {
		if !strings.HasSuffix(name, "_nvenc") || containsCapability(nvidia.Encoders, name) {
			capabilities.Encoders = append(capabilities.Encoders, name)
		}
	}
	for _, name := range probed.HWAccels {
		if name != "cuda" || nvidia.Device {
			capabilities.HWAccels = append(capabilities.HWAccels, name)
		}
	}
	if containsCapability(probed.Encoders, "libx264") {
		capabilities.H264Encoder = "libx264"
	}
	for _, name := range []string{"aac_at", "libfdk_aac", "aac"} {
		if containsCapability(probed.Encoders, name) {
			capabilities.AACEncoders = append(capabilities.AACEncoders, name)
		}
	}
	return capabilities
}

// probeVaapiDriver records the driver of the configured VAAPI render node
// when the server is configured for VAAPI, as MediaEncoder.SetFFmpegPath
// does at startup (MediaEncoder.cs:239-249). A failed probe leaves the
// driver unknown, which keeps transcodes in software (D-37).
func probeVaapiDriver(ctx context.Context, capabilities encoding.Capabilities, options config.EncodingOptions) encoding.Capabilities {
	if options.HardwareAccelerationType != encoding.HardwareAccelerationVAAPI || options.VaapiDevice == "" || !containsCapability(capabilities.HWAccels, "vaapi") {
		return capabilities
	}
	driver, err := ffmpeg.ProbeVaapiDriver(ctx, "ffmpeg", options.VaapiDevice)
	if err != nil {
		log.Printf("application: VAAPI device %s probe failed: %v", options.VaapiDevice, err)
		return capabilities
	}
	log.Printf("application: VAAPI device %s driver %q", options.VaapiDevice, driver)
	capabilities.VaapiDriver = driver
	return capabilities
}

// hardwareOptions projects encoding.xml into the pure encoder options. A
// render node path that does not exist is cleared, as
// EncodingHelper.GetVaapiDeviceArgs falls back when File.Exists fails.
func hardwareOptions(options config.EncodingOptions, exists func(string) bool) encoding.HardwareOptions {
	device := func(path string) string {
		if path == "" || !exists(path) {
			return ""
		}
		return path
	}
	return encoding.HardwareOptions{
		Type:                               options.HardwareAccelerationType,
		EnableHardwareEncoding:             options.EnableHardwareEncoding,
		HardwareDecodingCodecs:             options.HardwareDecodingCodecs,
		EnableDecodingColorDepth10Hevc:     options.EnableDecodingColorDepth10Hevc,
		EnableDecodingColorDepth10Vp9:      options.EnableDecodingColorDepth10Vp9,
		EnableDecodingColorDepth10HevcRext: options.EnableDecodingColorDepth10HevcRext,
		EnableDecodingColorDepth12HevcRext: options.EnableDecodingColorDepth12HevcRext,
		PreferSystemNativeHwDecoder:        options.PreferSystemNativeHwDecoder,
		EnableEnhancedNvdecDecoder:         options.EnableEnhancedNvdecDecoder,
		AllowHevcEncoding:                  options.AllowHevcEncoding,
		AllowAv1Encoding:                   options.AllowAv1Encoding,
		EnableIntelLowPowerH264HwEncoder:   options.EnableIntelLowPowerH264HwEncoder,
		EnableTonemapping:                  options.EnableTonemapping,
		EnableVppTonemapping:               options.EnableVppTonemapping,
		Tonemapping: encoding.Tonemapping{
			Algorithm: options.TonemappingAlgorithm,
			Mode:      options.TonemappingMode,
			Range:     options.TonemappingRange,
			Desat:     options.TonemappingDesat,
			Peak:      options.TonemappingPeak,
			Param:     options.TonemappingParam,
		},
		VppTonemappingBrightness: options.VppTonemappingBrightness,
		VppTonemappingContrast:   options.VppTonemappingContrast,
		VaapiDevice:              device(options.VaapiDevice),
		QsvDevice:                device(options.QsvDevice),
		EncoderPreset:            options.EncoderPreset,
	}
}

func renderNodeExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func containsCapability(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Router returns the fully registered, authorization-wrapped runtime router.
func (app *application) Router() *router.Router { return app.router }

// UserService returns the runtime identity service. It is a narrow composition
// seam for process-level tests and shares application ownership.
func (app *application) UserService() *user.Service { return app.users }

// ConfigManager returns the runtime system configuration seam.
func (app *application) ConfigManager() *config.Manager { return app.manager }

// Close releases owned process resources in two phases. Phase 1 requests
// shutdown from every directly owned worker (scheduler, websockets, scanner);
// each begins cancellation on entry and returns ctx.Err() without blocking
// when ctx is already done. Phase 2 returns the caller's cancellation if any
// owned drain expired, without closing dependents, so a later Close call can
// safely finish cleanup. Fields for undrained workers stay set for that retry.
func (app *application) Close(ctx context.Context) error {
	if app == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	release, gateErr := app.enterClose(ctx)
	if gateErr != nil {
		return gateErr
	}
	defer release()

	var schedulerErr, webSocketErr, scannerErr error
	// The change queue triggers the scheduler, so it stops first.
	var changesErr error
	if app.libraryChanges != nil {
		if changesErr = app.libraryChanges.Close(ctx); changesErr == nil {
			app.libraryChanges = nil
		}
	}
	if app.scheduler != nil {
		schedulerErr = errors.Join(changesErr, app.scheduler.Close(ctx))
		if schedulerErr == nil {
			app.scheduler = nil
		}
	}
	if app.webSockets != nil {
		webSocketErr = app.webSockets.Close(ctx)
		if webSocketErr == nil {
			app.webSockets = nil
		}
	}
	if app.scanner != nil {
		scannerErr = app.scanner.CloseContext(ctx)
		if scannerErr == nil {
			app.scanner = nil
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		for _, err := range []error{schedulerErr, webSocketErr, scannerErr} {
			if err != nil && errors.Is(err, ctxErr) {
				return errors.Join(schedulerErr, webSocketErr, scannerErr)
			}
		}
	}

	if app.trickplayRunner != nil {
		runnerErr := app.trickplayRunner.Close(ctx)
		if runnerErr != nil {
			return errors.Join(webSocketErr, scannerErr, runnerErr)
		}
		app.trickplayRunner = nil
	}
	var trickplayGeneratorErr, trickplayReaderErr error
	if app.trickplayGenerator != nil {
		trickplayGeneratorErr = app.trickplayGenerator.Close()
		if trickplayGeneratorErr == nil {
			app.trickplayGenerator = nil
		}
	}
	if app.trickplayReader != nil {
		trickplayReaderErr = app.trickplayReader.Close()
		if trickplayReaderErr == nil {
			app.trickplayReader = nil
		}
	}

	var transcodeErr, sessionErr, listenBrainzErr, topologyErr, busErr, storeErr error
	var userDataBusErr, activityErr, tasksErr error
	if app.transcodes != nil {
		transcodeErr = app.transcodes.Close(ctx)
		if transcodeErr == nil {
			app.transcodes = nil
		}
		if transcodeErr != nil && ctx.Err() != nil && errors.Is(transcodeErr, ctx.Err()) {
			return errors.Join(scannerErr, transcodeErr)
		}
	}
	if app.sessions != nil {
		sessionErr = closeSessions(app.sessions)
		app.sessions = nil
	}
	var keyframeErr error
	if app.keyframes != nil {
		keyframeErr = app.keyframes.Close(ctx)
		if keyframeErr != nil && ctx.Err() != nil && errors.Is(keyframeErr, ctx.Err()) {
			return errors.Join(webSocketErr, scannerErr, trickplayGeneratorErr, trickplayReaderErr, transcodeErr, sessionErr, keyframeErr)
		}
	}
	if app.listenScrobbler != nil {
		listenBrainzErr = app.listenScrobbler.Close(ctx)
		if listenBrainzErr == nil {
			app.listenScrobbler = nil
		}
	}
	if app.topology != nil {
		topologyErr = app.topology.Close()
		app.topology = nil
	}
	if app.tasks != nil {
		tasksErr = app.tasks.Close(ctx)
		if tasksErr == nil {
			app.tasks = nil
		}
	}
	if app.deviceActivity != nil {
		deviceActivityErr := app.deviceActivity.Close(ctx)
		if deviceActivityErr != nil {
			return errors.Join(scannerErr, transcodeErr, sessionErr, topologyErr, deviceActivityErr)
		}
		app.deviceActivity = nil
	}
	if app.activity != nil {
		activityErr = app.activity.Close(ctx)
		if activityErr == nil {
			app.activity = nil
		}
	}
	if app.userDataChanges != nil {
		userDataBusErr = app.userDataChanges.Close(ctx)
		if userDataBusErr == nil {
			app.userDataChanges = nil
		}
		if userDataBusErr != nil && ctx.Err() != nil && errors.Is(userDataBusErr, ctx.Err()) {
			return errors.Join(scannerErr, transcodeErr, sessionErr, topologyErr, userDataBusErr)
		}
	}
	if app.configuration != nil {
		configurationErr := app.configuration.Close(ctx)
		if configurationErr != nil && ctx.Err() != nil && errors.Is(configurationErr, ctx.Err()) {
			return errors.Join(scannerErr, transcodeErr, sessionErr, topologyErr, userDataBusErr, configurationErr)
		}
		app.configuration = nil
	}
	if app.changes != nil {
		busErr = app.changes.Close(ctx)
		if busErr == nil {
			app.changes = nil
		}
		if busErr != nil && ctx.Err() != nil && errors.Is(busErr, ctx.Err()) {
			return errors.Join(scannerErr, transcodeErr, sessionErr, topologyErr, userDataBusErr, busErr)
		}
	}
	if app.stores != nil {
		storeErr = app.stores.Close()
		app.stores = nil
	}
	// Fold the scheduler close error into the store error so the existing
	// errors.Join return line stays unchanged (additive-only carve-out).
	storeErr = errors.Join(schedulerErr, storeErr)
	return errors.Join(webSocketErr, scannerErr, trickplayGeneratorErr, trickplayReaderErr, transcodeErr, sessionErr, keyframeErr, listenBrainzErr, topologyErr, tasksErr, activityErr, userDataBusErr, busErr, storeErr)
}

func (app *application) enterClose(ctx context.Context) (func(), error) {
	transition := app.closeGate()
	// A first closer owns shutdown even when its drain context has already
	// expired: scanner CloseContext must still begin cancellation. Context
	// selection applies only while another close transition owns the gate.
	select {
	case <-transition.gate:
		return func() { transition.gate <- struct{}{} }, nil
	default:
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-transition.gate:
		return func() { transition.gate <- struct{}{} }, nil
	}
}

func (app *application) closeGate() *applicationCloseTransition {
	for {
		if transition := app.closeTransition.Load(); transition != nil {
			return transition
		}
		candidate := &applicationCloseTransition{gate: make(chan struct{}, 1)}
		candidate.gate <- struct{}{}
		if app.closeTransition.CompareAndSwap(nil, candidate) {
			return candidate
		}
	}
}

func createApplicationDirectories(paths config.Paths) error {
	for _, path := range []string{
		paths.Root,
		paths.Data,
		paths.Config,
		paths.Cache,
		paths.Log,
		paths.Metadata,
		paths.Transcodes,
		paths.Trickplay,
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create program-data directory %q: %w", path, err)
		}
	}
	return nil
}

// routeServices groups only handler registration dependencies. Registering
// routes does not invoke these services, which permits the pure ledger path to
// use nil services while retaining the exact runtime registration function.
type routeServices struct {
	identity           config.ServerIdentity
	paths              config.Paths
	webPath            string
	webComponent       string
	systemIdentity     func() config.ServerIdentity
	webSocketPort      int
	localAddress       string
	startup            interface{ StartupWizardCompleted() bool }
	network            auth.LocalNetwork
	playGrants         *playgrant.Store
	transcoding        transcodingState
	setupGate          *middleware.SetupGate
	manager            *config.Manager
	users              *user.Service
	apiKeys            handlers.APIKeyStore
	localization       *localization.Service
	clock              clock.Clock
	refresh            refreshTrigger
	libraryChanges     handlers.ChangeReporter
	topology           *library.Topology
	login              *auth.Login
	sessions           *session.Manager
	displayPreferences *displaypreferences.Service
	playback           playback.PlaybackInfoService
	playstate          playstate.Application
	directFiles        playback.DirectFileSourceService
	downloadFiles      playback.DownloadFileSourceService
	progressive        playback.ProgressiveVideoStreamer
	hls                playback.HLSVideoStreamer
	subtitles          playback.SubtitleStreamer
	attachments        handlers.AttachmentReader
	trickplay          *trickplay.ReaderService
	items              *store.ItemStore
	itemBuilder        *dtobuild.Builder
	views              *library.Views
	playlists          *playlists.Service
	collections        *library.Scanner
	images             handlers.ImageRenderer
	virtualPaths       path.VirtualPathExpander
	webSockets         *ws.Server
	sessionControl     *sessioncontrol.Service
	lookup             handlers.ItemLookupSearcher
	remoteImages       handlers.RemoteImagesService
	metadataPlugins    handlers.MetadataPluginCatalog
	itemDeleter        handlers.ItemDeleter
	itemEditor         itemEditor
	people             handlers.PersonRefresher
	subtitleEditor     handlers.SubtitleEditor
	scheduler          scheduledTaskService
	activity           *activitylog.Log
}

// transcodingState answers the session and playstate questions about running
// transcodes. A nil manager has no jobs, so the handlers never see a typed nil
// pointer.
type transcodingState struct{ manager *ffmpeg.TranscodeManager }

func (state transcodingState) HasTranscodingJob(playSessionID string) bool {
	return state.manager != nil && state.manager.HasTranscodingJob(playSessionID)
}

func (state transcodingState) TranscodingInfo(deviceID string) (dto.TranscodingInfo, bool) {
	if state.manager == nil {
		return dto.TranscodingInfo{}, false
	}
	return state.manager.TranscodingInfo(deviceID)
}

// personRefresher keeps a nil service out of the handler seam: a typed nil
// pointer in the interface would look like a configured refresher.
func personRefresher(service *people.Service) handlers.PersonRefresher {
	if service == nil {
		return nil
	}
	return service
}

// itemEditor is the metadata and image edit seam. *itemedit.Service
// satisfies it.
type itemEditor interface {
	handlers.ItemEditor
	handlers.ItemImageEditor
}

type refreshTrigger interface {
	Trigger() error
	TriggerRefresh(library.RefreshOptions) error
	RefreshStates() map[dto.UUID]library.FolderRefreshState
}

// scheduledTaskService is the narrow scheduler seam for the ScheduledTasks
// REST operations. It mirrors handlers.scheduledTaskService so cmd/deadfin
// owns no import of the handler's private interface.
type scheduledTaskService interface {
	List() []tasks.Info
	Get(id string) (tasks.Info, bool)
	Run(id string) error
	Cancel(id string) error
	UpdateTriggers(id string, triggers []tasks.TriggerInfo) error
}

func registerRoutes(rt *router.Router, services routeServices) {
	// Compile-time checks: the lookup and remote-images adapters satisfy
	// their handler-facing interfaces without composition-root glue.
	var _ handlers.ItemLookupSearcher = identify.LookupAdapter{}
	var _ handlers.RemoteImagesService = identify.RemoteImagesAdapter{}
	infoConfig := handlers.SystemInfoConfig{
		Paths:        services.paths,
		WebPath:      services.webPath,
		WebComponent: services.webComponent,
		CastReceiverApplications: func() []config.CastReceiverApplication {
			if services.manager == nil {
				return nil
			}
			return services.manager.System().CastReceiverApplications
		},
		SupportsLibraryMonitor: false,
		WebSocketPortNumber:    services.webSocketPort,
		Identity:               services.systemIdentity,
		LocalAddress:           services.localAddress,
		EnablePublishedServerURIByRequest: func() bool {
			return services.manager != nil && services.manager.Network().EnablePublishedServerUriByRequest
		},
		Network: services.network,
	}
	handlers.RegisterSystemWithConfig(rt, services.identity, services.startup, infoConfig)
	handlers.RegisterBuild(rt, currentBuildInfo)
	handlers.RegisterSetupUnlock(rt, services.setupGate)
	handlers.RegisterSystemStorage(rt, infoConfig, services.topology)
	handlers.RegisterStartup(rt, services.manager, services.users)
	handlers.RegisterBranding(rt, services.manager)
	handlers.RegisterConfiguration(rt, services.manager)
	handlers.RegisterLocalization(rt, services.localization, services.manager)
	handlers.RegisterLibrary(rt, services.refresh, services.topology)
	handlers.RegisterMediaFolders(rt, services.topology, services.items, services.itemBuilder)
	handlers.RegisterLibraryOptionsInfo(rt, services.metadataPlugins, func() []config.MetadataOptions {
		if services.manager == nil {
			return nil
		}
		return services.manager.System().MetadataOptions
	})
	handlers.RegisterEnvironment(rt, drives.System{})
	handlers.RegisterLibraryUpdates(rt, services.libraryChanges, services.topology, services.items)
	handlers.RegisterItemRefresh(rt, services.items, services.refresh)
	var collectionCreator handlers.CollectionCreator
	if services.collections != nil {
		collectionCreator = services.collections
	}
	handlers.RegisterCollections(rt, services.items, collectionCreator)
	var playlistService handlers.PlaylistService
	if services.playlists != nil {
		playlistService = services.playlists
	}
	var playlistUsers handlers.ItemUserResolver
	if services.users != nil {
		playlistUsers = services.users
	}
	var playlistBuilder handlers.ItemDTOBuilder
	if services.itemBuilder != nil {
		playlistBuilder = services.itemBuilder
	}
	handlers.RegisterPlaylists(rt, playlistService, playlistUsers, playlistBuilder, services.clock)
	handlers.RegisterAuth(rt, services.login, services.users, services.sessions, services.items, services.itemBuilder, services.identity, services.startup, services.network, services.transcoding)
	handlers.RegisterSessionLogout(rt, services.users, services.sessions, services.webSockets)
	var deviceService handlers.DeviceService
	if services.users != nil {
		deviceService = services.users
	}
	var deviceSessions handlers.DeviceSessions
	if services.sessions != nil {
		deviceSessions = services.sessions
	}
	var deviceSockets handlers.SessionSockets
	if services.webSockets != nil {
		deviceSockets = services.webSockets
	}
	handlers.RegisterDevices(rt, deviceService, deviceSessions, deviceSockets)
	handlers.RegisterSystemLogs(rt, services.paths.Log)
	handlers.RegisterAPIKeys(rt, services.apiKeys, services.clock, rand.Reader)
	handlers.RegisterClientLog(rt, handlers.ClientLogConfig{
		Enabled: func() bool {
			return services.manager != nil && services.manager.System().AllowClientLogUpload
		},
		LogDirectory: services.paths.Log,
		Clock:        services.clock,
		Random:       rand.Reader,
	})
	handlers.RegisterQuickConnect(rt, services.clock, func() bool { return services.manager != nil && services.manager.System().QuickConnectAvailable }, services.login, services.users, services.sessions, services.items, services.itemBuilder, services.identity)
	handlers.RegisterDisplayPreferences(rt, services.displayPreferences)
	itemDependencies := []any{handlers.ItemServices{Users: services.users, LatestPreferences: handlers.NewLatestUserPreferences(services.users), Languages: services.localization}, services.downloadFiles, services.manager}
	if services.sessions != nil {
		itemDependencies = append(itemDependencies, services.sessions)
	}
	if services.people != nil {
		itemDependencies = append(itemDependencies, services.people)
	}
	handlers.RegisterItems(rt, services.items, services.itemBuilder, services.clock, itemDependencies...)
	handlers.RegisterShows(rt, services.items, services.itemBuilder, services.items, services.users, services.clock, services.manager)
	handlers.RegisterUserViews(rt, services.views, services.itemBuilder)
	handlers.RegisterPlaybackInfo(rt, services.playback, services.users, services.playGrants)
	handlers.RegisterPlaystateWithGrants(rt, services.playstate, services.sessions, services.playGrants, services.transcoding)
	handlers.RegisterSessionControl(rt, services.sessionControl)
	handlers.RegisterDirectVideoStreams(rt, services.directFiles, services.progressive)
	handlers.RegisterDirectAudioStreams(rt, services.directFiles, services.users, services.playback, services.progressive, services.hls)
	handlers.RegisterHLSStreams(rt, services.hls, slog.Default())
	handlers.RegisterSubtitleStreams(rt, services.subtitles)
	handlers.RegisterVideoAttachments(rt, services.attachments)
	handlers.RegisterTrickplay(rt, services.trickplay)
	handlers.RegisterImages(rt, services.items, services.images, services.virtualPaths)
	handlers.RegisterUserImage(rt)
	handlers.RegisterItemLookup(rt, services.items, services.lookup)
	handlers.RegisterRemoteImages(rt, services.items, services.remoteImages)
	handlers.RegisterItemDelete(rt, services.itemDeleter)
	handlers.RegisterItemUpdate(rt, services.itemEditor)
	handlers.RegisterItemImageWrites(rt, services.itemEditor)
	handlers.RegisterSubtitleWrites(rt, services.subtitleEditor)
	handlers.RegisterScheduledTasks(rt, services.scheduler)
	handlers.RegisterActivityLog(rt, services.activity, services.users)
	handlers.RegisterPlugins(rt)
	handlers.RegisterBackup(rt)
	handlers.RegisterLiveTv(rt)
	handlers.RegisterSyncPlay(rt)
	ws.Register(rt, services.webSockets)
}

// currentBuildInfo builds the native build DTO from the compiled constants.
// It runs per request so no response shares a Capabilities slice.
func currentBuildInfo() dto.BuildInfo {
	return dto.BuildInfo{
		Component:           buildinfo.Component,
		Version:             buildinfo.Version,
		Channel:             buildinfo.Channel,
		Revision:            buildinfo.Revision,
		Dirty:               buildinfo.Dirty,
		BuildTime:           buildinfo.BuildTime,
		CompatibilityTarget: buildinfo.CompatibilityTarget,
		APIVersion:          buildinfo.APIVersion,
		Capabilities:        buildinfo.Capabilities(),
	}
}

func configuredServerIdentity(identity config.ServerIdentity, manager *config.Manager) config.ServerIdentity {
	if manager != nil {
		if name := strings.TrimSpace(manager.System().ServerName); name != "" {
			identity.ServerName = name
		}
	}
	return identity
}

func listenPort(listen string) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 0
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 0 || value > 65535 {
		return 0
	}
	return value
}

// routeLedger registers the runtime route table without opening configuration,
// databases, or other persistent resources.
func routeLedger() *router.Router {
	return newRouter(config.DefaultServerIdentity())
}

// newRouter remains the lightweight route-only composition seam used by route
// export and direct route-ledger tests.
func newRouter(identity config.ServerIdentity) *router.Router {
	rt := router.New()
	registerRoutes(rt, routeServices{
		identity: identity,
		clock:    clock.NewReal(),
		startup:  incompleteStartup{},
		network:  localAddressNetwork{},
		users:    &user.Service{},
	})
	return rt
}

type incompleteStartup struct{}

func (incompleteStartup) StartupWizardCompleted() bool { return false }

// loginPolicy adapts the registered default authorization policy to password
// login admission. The evaluator keeps startup, access-schedule, and network
// policy dynamic through the configuration manager.
type loginPolicy struct {
	evaluator *auth.Evaluator
	users     *user.Service
}

func (policy loginPolicy) Authorize(ctx context.Context, account store.User, remote netip.Addr, _ bool) error {
	decision, err := policy.evaluator.Evaluate(ctx, auth.PolicyRequest{
		Policy:   string(auth.PolicyDefaultAuthorization),
		Identity: auth.Identity{Authenticated: true},
		User:     account,
		RemoteIP: remote,
	})
	if err != nil {
		return err
	}
	if decision != auth.DecisionAllowed {
		return auth.ErrLoginDenied
	}
	return nil
}

func (policy loginPolicy) AuthorizeDevice(ctx context.Context, account store.User, deviceID string) error {
	err := policy.users.AuthorizeDevice(ctx, account, deviceID)
	if errors.Is(err, user.ErrForbidden) {
		return auth.ErrLoginDenied
	}
	return err
}

type loginSessions struct {
	manager *session.Manager
	users   *user.Service
}

func (sessions loginSessions) CheckLimit(ctx context.Context, userID dto.UUID) error {
	return sessions.checkLimit(ctx, userID, "", "")
}

func (sessions loginSessions) CheckLimitFor(ctx context.Context, userID dto.UUID, client, deviceID string) error {
	return sessions.checkLimit(ctx, userID, client, deviceID)
}

func (sessions loginSessions) checkLimit(ctx context.Context, userID dto.UUID, client, deviceID string) error {
	maximum, err := sessions.users.MaxActiveSessions(ctx, userID)
	if err != nil {
		return err
	}
	return sessions.manager.CheckLimit(userID, client, deviceID, maximum)
}

// recordSessionActivity writes the SessionStartedLogger or SessionEndedLogger
// entry. A session without a user name writes none, as upstream.
func recordSessionActivity(ctx context.Context, activity activitylog.Recorder, users *user.Service, snapshot session.Snapshot, build func(dto.UUID, string, string, string) activitylog.Entry) {
	if snapshot.UserID.IsNil() {
		return
	}
	owner, err := users.Get(ctx, snapshot.UserID)
	if err != nil || owner.Name == "" {
		return
	}
	activity.Record(build(owner.ID, owner.Name, snapshot.DeviceName, snapshot.RemoteEndPoint))
}

func (sessions loginSessions) LogActivity(ctx context.Context, activity session.Activity) (session.Snapshot, error) {
	return sessions.manager.LogActivity(ctx, activity)
}

func closeSessions(sessions *session.Manager) error {
	err := sessions.Close()
	if errors.Is(err, session.ErrClosed) {
		return nil
	}
	return err
}

// localAddressNetwork recognizes loopback, private, and link-local peers as
// LAN callers. Server-wide remote policy remains separate from locality so it
// can deny public callers without preventing LAN administration.
type localAddressNetwork struct{}

func (localAddressNetwork) IsInLocalNetwork(address netip.Addr) bool {
	return address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast()
}

func (network localAddressNetwork) IsLocal(address netip.Addr) bool {
	return network.IsInLocalNetwork(address)
}

// configuredNetwork supplies the live global remote-access setting alongside
// the shared LAN classifier used by authorization, login, and playback.
type configuredNetwork struct {
	manager *config.Manager
	lan     *lanSubnets
}

// IsInLocalNetwork applies the live LocalNetworkSubnets configuration.
func (network configuredNetwork) IsInLocalNetwork(address netip.Addr) bool {
	if network.lan == nil {
		return localAddressNetwork{}.IsInLocalNetwork(address)
	}
	return network.lan.IsInLocalNetwork(address)
}

func (network configuredNetwork) IsLocal(address netip.Addr) bool {
	return network.IsInLocalNetwork(address)
}

func (network configuredNetwork) RemoteAccessEnabled() bool {
	return network.manager == nil || network.manager.Network().EnableRemoteAccess
}

// programDataUsage is the -program-data flag help shared by the server and
// every subcommand.
const programDataUsage = "program-data root (defaults to DEADFIN_DATA_DIR, then JELLYFIN_DATA_DIR, then ./data)"

// resolveProgramData returns the data root: the flag value, then
// DEADFIN_DATA_DIR, then JELLYFIN_DATA_DIR, then defaultProgramData. Blank
// values do not count as set.
func resolveProgramData(flagValue string, getenv func(string) string) string {
	if value := strings.TrimSpace(flagValue); value != "" {
		return value
	}
	for _, name := range []string{"DEADFIN_DATA_DIR", "JELLYFIN_DATA_DIR"} {
		if value := strings.TrimSpace(getenv(name)); value != "" {
			return value
		}
	}
	return defaultProgramData()
}

// warnUnusedJellyfinDirs logs one warning for each set upstream Jellyfin
// directory variable that Deadfin does not read, with the data-root subfolder
// that Deadfin uses instead.
func warnUnusedJellyfinDirs(ctx context.Context, logger *slog.Logger, getenv func(string) string, programData string) {
	unused := [...]struct{ name, subfolder string }{
		{"JELLYFIN_CONFIG_DIR", "config"},
		{"JELLYFIN_CACHE_DIR", "cache"},
		{"JELLYFIN_LOG_DIR", "log"},
		{"JELLYFIN_WEB_DIR", "web"},
	}
	for _, dir := range unused {
		value := strings.TrimSpace(getenv(dir.name))
		if value == "" {
			continue
		}
		logger.LogAttrs(ctx, slog.LevelWarn, dir.name+" is not used; Deadfin uses the data-root subfolder instead",
			slog.String("variable", dir.name),
			slog.String("value", value),
			slog.String("path", filepath.Join(programData, dir.subfolder)),
		)
	}
}

func defaultProgramData() string { return "data" }
