// SPDX-License-Identifier: GPL-2.0-only

package providers_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/providers"
)

func TestRegistryRefreshAppliesOrderedEnabledProviders(t *testing.T) {
	locked := []item.MetadataField{
		item.MetadataFieldCast,
		item.MetadataFieldGenres,
		item.MetadataFieldProductionLocations,
		item.MetadataFieldStudios,
		item.MetadataFieldTags,
		item.MetadataFieldName,
		item.MetadataFieldOverview,
		item.MetadataFieldRuntime,
		item.MetadataFieldOfficialRating,
	}
	contextValue := library.RefreshContext{
		Directory:   library.Entry{Path: "/media/films", IsDir: true},
		Entries:     []library.Entry{{Path: "/media/films/movie.mkv"}, {Path: "/media/films/movie.jpg"}, {Path: "/media/films/movie.nfo"}},
		LibraryRoot: "/media",
		Options: library.LibraryOptions{
			DisabledLocalMetadataReaders: []string{"disabled-nfo"},
			LocalMetadataReaderOrder:     []string{"nfo", "fallback-nfo"},
			EnableInternetProviders:      true,
		},
	}
	var calls []string
	assertContext := func(got library.RefreshContext) {
		t.Helper()
		if !reflect.DeepEqual(got, contextValue) {
			t.Fatalf("provider refresh context = %#v, want discovered directory facts %#v", got, contextValue)
		}
	}
	registry := providers.NewRegistry(
		providerFunc{name: "local-image", stage: providers.ProviderStageLocalImages, refresh: func(_ context.Context, got library.RefreshContext, target *item.Item) (bool, error) {
			assertContext(got)
			calls = append(calls, "local-image")
			target.ImageInfos = []item.ImageInfo{{Type: item.ImageTypePrimary, Path: "/media/films/movie.jpg"}}
			return true, nil
		}},
		providerFunc{name: "disabled-nfo", stage: providers.ProviderStageLocalMetadata, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) {
			calls = append(calls, "disabled-nfo")
			return true, nil
		}},
		providerFunc{name: "fallback-nfo", stage: providers.ProviderStageLocalMetadata, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) {
			calls = append(calls, "fallback-nfo")
			return true, nil
		}},
		providerFunc{name: "remote", stage: providers.ProviderStageRemoteMetadata, refresh: func(_ context.Context, got library.RefreshContext, target *item.Item) (bool, error) {
			assertContext(got)
			calls = append(calls, "remote")
			target.ProviderIDs.Set("Remote", "future")
			return true, nil
		}},
		providerFunc{name: "nfo", stage: providers.ProviderStageLocalMetadata, refresh: func(_ context.Context, got library.RefreshContext, target *item.Item) (bool, error) {
			assertContext(got)
			calls = append(calls, "nfo")
			target.IsLocked = true
			target.LockedFields = append([]item.MetadataField(nil), locked...)
			return true, nil
		}},
	)

	target := item.Item{Name: providerString("resolver title")}
	if err := registry.Refresh(t.Context(), contextValue, &target); err != nil {
		t.Fatalf("refresh provider registry: %v", err)
	}
	if want := []string{"local-image", "nfo"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("provider calls = %v, want local image then LockData metadata with remote skipped %v", calls, want)
	}
	if !target.IsLocked || !reflect.DeepEqual(target.LockedFields, locked) {
		t.Fatalf("NFO lock facts = locked %t fields %v, want locked with %v", target.IsLocked, target.LockedFields, locked)
	}
	if !reflect.DeepEqual(target.ImageInfos, []item.ImageInfo{{Type: item.ImageTypePrimary, Path: "/media/films/movie.jpg"}}) {
		t.Fatalf("local image enrichment = %#v", target.ImageInfos)
	}
}

func TestRegistryRefreshRunsRemoteAfterUnlockedLocalMetadata(t *testing.T) {
	var calls []string
	registry := providers.NewRegistry(
		providerFunc{name: "nfo", stage: providers.ProviderStageLocalMetadata, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) {
			calls = append(calls, "nfo")
			return true, nil
		}},
		providerFunc{name: "remote", stage: providers.ProviderStageRemoteMetadata, refresh: func(_ context.Context, _ library.RefreshContext, target *item.Item) (bool, error) {
			calls = append(calls, "remote")
			target.ProviderIDs.Set("Remote", "future")
			return true, nil
		}},
	)
	target := item.Item{}
	if err := registry.Refresh(t.Context(), library.RefreshContext{Mode: library.RefreshModeDefault, Options: library.LibraryOptions{EnableInternetProviders: true}}, &target); err != nil {
		t.Fatalf("refresh unlocked local metadata: %v", err)
	}
	if want := []string{"nfo", "remote"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("provider calls = %v, want unlocked local metadata then remote %v", calls, want)
	}
	if value, ok := target.ProviderIDs.Get("Remote"); !ok || value != "future" {
		t.Fatalf("remote enrichment = %q, %t; want future, true", value, ok)
	}
}

func TestRegistryRefreshAbortsAtProviderFailure(t *testing.T) {
	providerErr := errors.New("NFO is malformed")
	calledAfterFailure := false
	registry := providers.NewRegistry(
		providerFunc{name: "image", stage: providers.ProviderStageLocalImages, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) { return true, nil }},
		providerFunc{name: "nfo", stage: providers.ProviderStageLocalMetadata, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) { return false, providerErr }},
		providerFunc{name: "remote", stage: providers.ProviderStageRemoteMetadata, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) {
			calledAfterFailure = true
			return true, nil
		}},
	)
	if err := registry.Refresh(t.Context(), library.RefreshContext{}, &item.Item{}); !errors.Is(err, providerErr) {
		t.Fatalf("provider error = %v, want %v", err, providerErr)
	}
	if calledAfterFailure {
		t.Fatal("registry invoked a later provider after an enrichment failure")
	}
}

func TestRegistryRefreshRejectsCanceledContextBeforeProviderWork(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	registry := providers.NewRegistry(providerFunc{name: "image", stage: providers.ProviderStageLocalImages, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) {
		called = true
		return true, nil
	}})
	if err := registry.Refresh(ctx, library.RefreshContext{}, &item.Item{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("registry invoked a provider after cancellation")
	}
}

type providerFunc struct {
	name    string
	stage   providers.ProviderStage
	refresh func(context.Context, library.RefreshContext, *item.Item) (bool, error)
}

func (provider providerFunc) Name() string { return provider.name }

func (provider providerFunc) Stage() providers.ProviderStage { return provider.stage }

func (provider providerFunc) Refresh(ctx context.Context, refresh library.RefreshContext, target *item.Item) (bool, error) {
	return provider.refresh(ctx, refresh, target)
}

func providerString(value string) *string { return &value }

func remoteRefreshContext() library.RefreshContext {
	return library.RefreshContext{Mode: library.RefreshModeDefault, Options: library.LibraryOptions{EnableInternetProviders: true}}
}

// TestRegistryRemoteProviderErrorIsLoggedAndNextProviderRuns mirrors
// MetadataService.ExecuteRemoteProviders: a failed remote provider is logged
// and the next remote provider still runs.
func TestRegistryRemoteProviderErrorIsLoggedAndNextProviderRuns(t *testing.T) {
	var logs bytes.Buffer
	var calls []string
	registry := providers.NewRegistry(
		providerFunc{name: "failing", stage: providers.ProviderStageRemoteMetadata, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) {
			calls = append(calls, "failing")
			return false, errors.New("tmdb: 503")
		}},
		providerFunc{name: "working", stage: providers.ProviderStageRemoteMetadata, refresh: func(_ context.Context, _ library.RefreshContext, target *item.Item) (bool, error) {
			calls = append(calls, "working")
			target.ProviderIDs.Set("Working", "1")
			return true, nil
		}},
	).WithLogger(slog.New(slog.NewTextHandler(&logs, nil)))
	target := item.Item{}
	if err := registry.Refresh(t.Context(), remoteRefreshContext(), &target); err != nil {
		t.Fatalf("Refresh = %v, want the remote error logged, not returned", err)
	}
	if want := []string{"failing", "working"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	if id, _ := target.ProviderIDs.Get("Working"); id != "1" {
		t.Error("the provider after the failure did not enrich the item")
	}
	if !strings.Contains(logs.String(), "provider=failing") || !strings.Contains(logs.String(), "tmdb: 503") {
		t.Errorf("log = %q, want the provider name and cause", logs.String())
	}
}

func TestRegistryRemoteCancellationAndLocalErrorsPropagate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancelling := providers.NewRegistry(providerFunc{name: "remote", stage: providers.ProviderStageRemoteMetadata, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) {
		cancel()
		return false, context.Canceled
	}})
	if err := cancelling.Refresh(ctx, remoteRefreshContext(), &item.Item{}); !errors.Is(err, context.Canceled) {
		t.Errorf("remote cancellation = %v, want context.Canceled", err)
	}

	// A provider whose own deadline fires while the caller cancels still
	// aborts: the caller's cancellation wins.
	callerCtx, callerCancel := context.WithCancel(t.Context())
	callerGone := providers.NewRegistry(providerFunc{name: "remote", stage: providers.ProviderStageRemoteMetadata, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) {
		callerCancel()
		return false, context.DeadlineExceeded
	}})
	if err := callerGone.Refresh(callerCtx, remoteRefreshContext(), &item.Item{}); err == nil {
		t.Error("provider deadline after caller cancellation = nil, want the stage aborted")
	}

	local := errors.New("nfo unreadable")
	failingLocal := providers.NewRegistry(providerFunc{name: "nfo", stage: providers.ProviderStageLocalMetadata, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) {
		return false, local
	}})
	if err := failingLocal.Refresh(t.Context(), remoteRefreshContext(), &item.Item{}); !errors.Is(err, local) {
		t.Errorf("local error = %v, want it returned", err)
	}
}

// TestRegistryRemoteProviderOwnDeadlineIsItemFailure covers #749: a remote
// provider whose own request timeout or rate-limit wait expires, while the
// caller's context is still live, fails for this item only. The registry logs
// it, restores locked metadata, and runs the next provider.
func TestRegistryRemoteProviderOwnDeadlineIsItemFailure(t *testing.T) {
	for _, cause := range []error{
		context.DeadlineExceeded,
		fmt.Errorf("musicbrainz: release: %w", context.DeadlineExceeded),
		context.Canceled,
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			var logs bytes.Buffer
			var calls []string
			registry := providers.NewRegistry(
				providerFunc{name: "MusicBrainz", stage: providers.ProviderStageRemoteMetadata, refresh: func(_ context.Context, _ library.RefreshContext, target *item.Item) (bool, error) {
					calls = append(calls, "MusicBrainz")
					target.Overview = providerString("partial")
					return false, cause
				}},
				providerFunc{name: "TheAudioDB", stage: providers.ProviderStageRemoteMetadata, refresh: func(_ context.Context, _ library.RefreshContext, target *item.Item) (bool, error) {
					calls = append(calls, "TheAudioDB")
					target.ProviderIDs.Set("AudioDbAlbum", "1")
					return true, nil
				}},
			).WithLogger(slog.New(slog.NewTextHandler(&logs, nil)))
			target := item.Item{Kind: item.KindMusicAlbum, Overview: providerString("kept"), LockedFields: []item.MetadataField{item.MetadataFieldOverview}}
			if err := registry.Refresh(t.Context(), remoteRefreshContext(), &target); err != nil {
				t.Fatalf("Refresh = %v, want the provider's own deadline logged, not returned", err)
			}
			if want := []string{"MusicBrainz", "TheAudioDB"}; !reflect.DeepEqual(calls, want) {
				t.Errorf("calls = %v, want %v", calls, want)
			}
			if target.Overview == nil || *target.Overview != "kept" {
				t.Errorf("Overview = %v, want the locked value restored", target.Overview)
			}
			if id, _ := target.ProviderIDs.Get("AudioDbAlbum"); id != "1" {
				t.Error("the provider after the failure did not enrich the item")
			}
			if !strings.Contains(logs.String(), "remote metadata provider failed") || !strings.Contains(logs.String(), "provider=MusicBrainz") {
				t.Errorf("log = %q, want an item-level warning for MusicBrainz", logs.String())
			}
		})
	}
}

// TestRegistryRemoteFetchersEnableAndOrderSeparately mirrors
// ProviderManager.GetOrCreateOrderedProviders and
// BaseItemManager.IsMetadataFetcherEnabled: a TypeOptions entry for the kind
// makes MetadataFetchers the exact enabled set, so an empty list runs no
// remote provider, and MetadataFetcherOrder only ranks it. A provider that the
// enabled set names but a stale order list omits still runs, after the ranked
// ones. Without an entry for the kind every remote provider runs.
func TestRegistryRemoteFetchersEnableAndOrderSeparately(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		entryType string
		enabled   []string
		order     []string
		want      string
	}{
		{name: "order omits an enabled provider", enabled: []string{"C", "A", "B"}, order: []string{"B", "A"}, want: "B,A,C"},
		{name: "enabled set filters", enabled: []string{"A"}, order: []string{"B", "A", "C"}, want: "A"},
		{name: "empty enabled list runs none", order: []string{"C"}, want: ""},
		{name: "empty lists run none", want: ""},
		{name: "entry for another kind runs all in registration order", entryType: "Movie", want: "A,B,C"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var ran []string
			remote := func(name string) providers.Provider {
				return providerFunc{name: name, stage: providers.ProviderStageRemoteMetadata, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) {
					ran = append(ran, name)
					return true, nil
				}}
			}
			registry := providers.NewRegistry(remote("A"), remote("B"), remote("C"))
			entryType := testCase.entryType
			if entryType == "" {
				entryType = "Series"
			}
			refresh := library.RefreshContext{Mode: library.RefreshModeFullRefresh, Options: library.LibraryOptions{
				TypeOptions: []library.TypeOptions{{Type: entryType, MetadataFetchers: testCase.enabled, MetadataFetcherOrder: testCase.order}},
			}}
			target := item.Item{Kind: item.KindSeries}
			if err := registry.Refresh(t.Context(), refresh, &target); err != nil {
				t.Fatalf("registry refresh: %v", err)
			}
			if got := strings.Join(ran, ","); got != testCase.want {
				t.Fatalf("remote providers ran %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestRegistryNoTypeOptionsRunsEveryRemoteProvider pins the fallback of
// BaseItemManager.IsMetadataFetcherEnabled: a library with no TypeOptions runs
// every remote provider.
func TestRegistryNoTypeOptionsRunsEveryRemoteProvider(t *testing.T) {
	var ran []string
	remote := func(name string) providers.Provider {
		return providerFunc{name: name, stage: providers.ProviderStageRemoteMetadata, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) {
			ran = append(ran, name)
			return true, nil
		}}
	}
	registry := providers.NewRegistry(remote("A"), remote("B"))
	target := item.Item{Kind: item.KindMusicAlbum}
	if err := registry.Refresh(t.Context(), library.RefreshContext{Mode: library.RefreshModeDefault}, &target); err != nil {
		t.Fatalf("registry refresh: %v", err)
	}
	if got := strings.Join(ran, ","); got != "A,B" {
		t.Fatalf("remote providers ran %q, want every provider", got)
	}
}

// TestRegistryLocalImagesIgnoreImageFetchers pins ProviderManager.CanRefreshImages:
// every ILocalImageProvider runs whatever the TypeOptions ImageFetchers list
// names. The list names remote image providers only, so an empty list or a
// list of remote names must not drop local artwork.
func TestRegistryLocalImagesIgnoreImageFetchers(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		fetcher []string
	}{
		{name: "empty image fetchers"},
		{name: "remote image fetchers only", fetcher: []string{"TheAudioDB", "MusicBrainz"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ran := false
			registry := providers.NewRegistry(providerFunc{name: "LocalImageProvider", stage: providers.ProviderStageLocalImages, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) {
				ran = true
				return false, nil
			}})
			refresh := library.RefreshContext{Mode: library.RefreshModeDefault, Options: library.LibraryOptions{
				TypeOptions: []library.TypeOptions{{Type: "MusicAlbum", ImageFetchers: testCase.fetcher, ImageFetcherOrder: testCase.fetcher}},
			}}
			target := item.Item{Kind: item.KindMusicAlbum}
			if err := registry.Refresh(t.Context(), refresh, &target); err != nil {
				t.Fatalf("registry refresh: %v", err)
			}
			if !ran {
				t.Fatal("local image provider did not run")
			}
		})
	}
}
