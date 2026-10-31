// SPDX-License-Identifier: GPL-2.0-only

package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
)

// ProviderStage identifies the fixed enrichment phase owned by a provider.
// Registry always runs local images before metadata, then optional remote
// metadata, regardless of registration order.
type ProviderStage uint8

const (
	ProviderStageLocalImages ProviderStage = iota
	ProviderStageLocalMetadata
	ProviderStageRemoteMetadata
)

// Provider enriches one resolved item using the scanner's frozen directory
// facts. handled stops only the local metadata reader chain; image and remote
// providers remain independently ordered phases.
type Provider interface {
	Name() string
	Stage() ProviderStage
	Refresh(context.Context, library.RefreshContext, *item.Item) (handled bool, err error)
}

// Registry is an ordered in-memory metadata enrichment pipeline. It owns no
// storage and deliberately performs no filesystem traversal.
type Registry struct {
	providers []Provider
	logger    *slog.Logger
	// disabledImageFetchers returns the server MetadataOptions
	// DisabledImageFetchers for an item type. Nil disables none.
	disabledImageFetchers func(itemType string) []string
}

// NewRegistry constructs a registry in registration order within each stage.
func NewRegistry(providers ...Provider) *Registry {
	return &Registry{providers: append([]Provider(nil), providers...)}
}

// WithLogger returns a copy of the registry that records remote provider
// failures to logger. Without one the registry uses slog.Default.
func (r *Registry) WithLogger(logger *slog.Logger) *Registry {
	clone := *r
	clone.logger = logger
	return &clone
}

// WithDisabledImageFetchers returns a copy of the registry that reads the
// server MetadataOptions DisabledImageFetchers for an item type. They apply
// to a kind without a library TypeOptions entry, as in
// BaseItemManager.IsImageFetcherEnabled.
func (r *Registry) WithDisabledImageFetchers(disabled func(itemType string) []string) *Registry {
	clone := *r
	clone.disabledImageFetchers = disabled
	return &clone
}

func (r *Registry) serverDisabledImageFetchers(kind item.Kind) []string {
	if r.disabledImageFetchers == nil {
		return nil
	}
	return r.disabledImageFetchers(kind.String())
}

// Refresh enriches target through local images, local metadata, and enabled
// remote metadata. It stops local readers after the first handler. Context
// errors always propagate. A local provider error propagates before any later
// provider runs. A remote provider error is logged and the next remote
// provider runs, as MetadataService.ExecuteRemoteProviders does: one failed
// lookup must not fail the whole folder scan.
func (r *Registry) Refresh(ctx context.Context, refresh library.RefreshContext, target *item.Item) error {
	_, err := r.RefreshWithOutcome(ctx, refresh, target)
	return err
}

// RefreshWithOutcome runs Refresh and reports how many remote providers
// failed.
func (r *Registry) RefreshWithOutcome(ctx context.Context, refresh library.RefreshContext, target *item.Item) (library.RefreshOutcome, error) {
	if err := ctx.Err(); err != nil {
		return library.RefreshOutcome{}, err
	}
	if target == nil {
		return library.RefreshOutcome{}, errors.New("provider refresh: item is required")
	}
	if r == nil {
		return library.RefreshOutcome{}, nil
	}
	if _, err := r.run(ctx, refresh, target, ProviderStageLocalImages, false); err != nil {
		return library.RefreshOutcome{}, err
	}
	if _, err := r.run(ctx, refresh, target, ProviderStageLocalMetadata, true); err != nil {
		return library.RefreshOutcome{}, err
	}
	// LibraryOptions.EnableInternetProviders is obsolete upstream ("Disable
	// remote providers in TypeOptions instead") and nothing reads it, so it
	// does not gate this stage. The web client never sends it, so a library
	// created or edited there always holds false. The library's TypeOptions
	// select the remote providers in run.
	//
	// Gate the remote stage on refresh intent, mirroring MetadataService's
	// ExecuteRemoteProviders call site: remote runs only when metadata is being
	// replaced or the mode exceeds ValidationOnly. A plain library scan threads
	// RefreshModeDefault, so remote runs; None/ValidationOnly suppress it and
	// FullRefresh forces it to re-fetch an already-scanned item.
	//
	// The scanner sets SkipRemoteMetadata for a stored item that upstream
	// GetProviders would not refresh: an unchanged item that was refreshed
	// before. The scanner carries its stored metadata instead.
	if !refresh.ReplaceAllMetadata && refresh.Mode <= library.RefreshModeValidationOnly || refresh.SkipRemoteMetadata {
		return library.RefreshOutcome{}, nil
	}
	failures, err := r.run(ctx, refresh, target, ProviderStageRemoteMetadata, false)
	return library.RefreshOutcome{RemoteFailures: failures}, err
}

// LocalMetadataChanged reports whether an enabled local metadata reader saw a
// source of stored change after stored was last saved. It mirrors the
// IHasItemChangeMonitor check in MetadataService.GetProviders.
func (r *Registry) LocalMetadataChanged(refresh library.RefreshContext, stored item.Item) bool {
	if r == nil {
		return false
	}
	for _, provider := range r.ordered(ProviderStageLocalMetadata, typeProviderSelection(refresh.Options, stored.Kind, ProviderStageLocalMetadata)) {
		if disabled(refresh.Options.DisabledLocalMetadataReaders, provider.Name()) {
			continue
		}
		if monitor, ok := provider.(changeMonitor); ok && monitor.HasChanged(refresh, stored) {
			return true
		}
	}
	return false
}

// changeMonitor is the provider side of IHasItemChangeMonitor: HasChanged
// reports whether the provider's source for stored changed after stored was
// last saved.
type changeMonitor interface {
	HasChanged(refresh library.RefreshContext, stored item.Item) bool
}

func (r *Registry) log() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}
	return slog.Default()
}

// run executes one stage. It returns the number of remote providers that
// failed; any other provider error stops the stage.
func (r *Registry) run(ctx context.Context, refresh library.RefreshContext, target *item.Item, stage ProviderStage, stopOnHandled bool) (int, error) {
	failures := 0
	for _, provider := range r.ordered(stage, typeProviderSelection(refresh.Options, target.Kind, stage)) {
		if err := ctx.Err(); err != nil {
			return failures, err
		}
		if stage != ProviderStageLocalImages && target.IsLocked {
			return failures, nil
		}
		if stage == ProviderStageLocalMetadata && disabled(refresh.Options.DisabledLocalMetadataReaders, provider.Name()) {
			continue
		}
		// An image fetcher the library turned off for this kind: a local one
		// does not run, and a remote one keeps its metadata without its images.
		imagesOff := len(supportedImages(provider, target.Kind)) != 0 && !imageFetcherEnabled(refresh.Options.TypeOptionsFor(target.Kind), r.serverDisabledImageFetchers(target.Kind), provider.Name())
		if imagesOff && stage == ProviderStageLocalImages {
			continue
		}
		var images []item.ImageInfo
		if imagesOff {
			images = slices.Clone(target.ImageInfos)
		}
		var locked lockedMetadata
		if stage != ProviderStageLocalImages {
			locked = snapshotLockedMetadata(*target)
		}
		handled, err := provider.Refresh(ctx, refresh, target)
		if imagesOff {
			target.ImageInfos = images
		}
		if err != nil {
			// Only the caller's own cancellation stops a remote stage. A
			// provider's request timeout or rate-limit wait carries its own
			// deadline and fails this provider for this item only (#749).
			if stage != ProviderStageRemoteMetadata || ctx.Err() != nil {
				return failures, fmt.Errorf("refresh provider %q: %w", provider.Name(), err)
			}
			failures++
			locked.restore(target)
			r.log().LogAttrs(ctx, slog.LevelWarn, "remote metadata provider failed",
				slog.String("provider", provider.Name()),
				slog.String("item", target.ID.String()),
				slog.String("kind", target.Kind.String()),
				slog.String("error", err.Error()))
			continue
		}
		if stage != ProviderStageLocalImages {
			locked.restore(target)
		}
		if stage == ProviderStageRemoteMetadata {
			r.log().LogAttrs(ctx, slog.LevelDebug, "remote metadata provider ran",
				slog.String("provider", provider.Name()),
				slog.String("item", target.ID.String()),
				slog.String("kind", target.Kind.String()),
				slog.Bool("handled", handled))
		}
		if stopOnHandled && handled {
			return failures, nil
		}
	}
	return failures, nil
}

type lockedMetadata struct {
	locked       bool
	fields       []item.MetadataField
	name         *string
	sortName     *string
	forcedName   *string
	originalName *string
	overview     *string
	tagline      *string
	official     *string
	custom       *string
	runtime      *int64
	genres       []string
	studios      []string
	tags         []string
	locations    []string
	people       []item.PersonCredit
}

func snapshotLockedMetadata(value item.Item) lockedMetadata {
	return lockedMetadata{
		locked:       value.IsLocked,
		fields:       append([]item.MetadataField(nil), value.LockedFields...),
		name:         cloneProviderPointer(value.Name),
		sortName:     cloneProviderPointer(value.SortName),
		forcedName:   cloneProviderPointer(value.ForcedSortName),
		originalName: cloneProviderPointer(value.OriginalTitle),
		overview:     cloneProviderPointer(value.Overview),
		tagline:      cloneProviderPointer(value.Tagline),
		official:     cloneProviderPointer(value.OfficialRating),
		custom:       cloneProviderPointer(value.CustomRating),
		runtime:      cloneProviderPointer(value.RunTimeTicks),
		genres:       append([]string(nil), value.Values.Genres...),
		studios:      append([]string(nil), value.Values.Studios...),
		tags:         append([]string(nil), value.Values.Tags...),
		locations:    append([]string(nil), value.Values.ProductionLocations...),
		people:       cloneProviderPeople(value.People),
	}
}

func (locked lockedMetadata) restore(target *item.Item) {
	if locked.locked {
		target.IsLocked = true
	}
	if len(locked.fields) != 0 {
		target.LockedFields = mergeLockedFields(locked.fields, target.LockedFields)
	}
	if locked.has(item.MetadataFieldName) {
		target.Name = cloneProviderPointer(locked.name)
		target.SortName = cloneProviderPointer(locked.sortName)
		target.ForcedSortName = cloneProviderPointer(locked.forcedName)
		target.OriginalTitle = cloneProviderPointer(locked.originalName)
	}
	if locked.has(item.MetadataFieldOverview) {
		target.Overview = cloneProviderPointer(locked.overview)
		target.Tagline = cloneProviderPointer(locked.tagline)
	}
	if locked.has(item.MetadataFieldOfficialRating) {
		target.OfficialRating = cloneProviderPointer(locked.official)
		target.CustomRating = cloneProviderPointer(locked.custom)
	}
	if locked.has(item.MetadataFieldRuntime) {
		target.RunTimeTicks = cloneProviderPointer(locked.runtime)
	}
	if locked.has(item.MetadataFieldGenres) {
		target.Values.Genres = append([]string(nil), locked.genres...)
	}
	if locked.has(item.MetadataFieldStudios) {
		target.Values.Studios = append([]string(nil), locked.studios...)
	}
	if locked.has(item.MetadataFieldTags) {
		target.Values.Tags = append([]string(nil), locked.tags...)
	}
	if locked.has(item.MetadataFieldProductionLocations) {
		target.Values.ProductionLocations = append([]string(nil), locked.locations...)
	}
	if locked.has(item.MetadataFieldCast) {
		target.People = cloneProviderPeople(locked.people)
	}
}

func (locked lockedMetadata) has(field item.MetadataField) bool {
	for _, current := range locked.fields {
		if current == field {
			return true
		}
	}
	return false
}

func cloneProviderPointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneProviderPeople(values []item.PersonCredit) []item.PersonCredit {
	if values == nil {
		return nil
	}
	clone := make([]item.PersonCredit, len(values))
	copy(clone, values)
	for index := range clone {
		clone[index].Role = cloneProviderPointer(clone[index].Role)
		clone[index].SortOrder = cloneProviderPointer(clone[index].SortOrder)
	}
	return clone
}

// ordered returns the providers of one stage that the type options enable,
// in configured order. It mirrors ProviderManager.GetOrCreateOrderedProviders:
// an exact selection keeps only the providers its enabled list names, so an
// empty list keeps none; otherwise a non-empty enabled list keeps only the
// providers it names and an empty one keeps all. Order ranks
// the kept providers by their index in it; a provider absent from order runs
// after the listed ones, in registration order. A stale order list therefore
// never disables a provider that the enabled list names.
func (r *Registry) ordered(stage ProviderStage, selection providerSelection) []Provider {
	stageProviders := make([]Provider, 0, len(r.providers))
	for _, provider := range r.providers {
		if provider == nil || provider.Stage() != stage {
			continue
		}
		if (selection.exact || len(selection.enabled) != 0) && !containsFold(selection.enabled, provider.Name()) {
			continue
		}
		stageProviders = append(stageProviders, provider)
	}
	if len(selection.order) == 0 {
		return stageProviders
	}
	rank := func(provider Provider) int {
		for index, name := range selection.order {
			if strings.EqualFold(provider.Name(), name) {
				return index
			}
		}
		return len(selection.order)
	}
	sort.SliceStable(stageProviders, func(i, j int) bool { return rank(stageProviders[i]) < rank(stageProviders[j]) })
	return stageProviders
}

// providerSelection is the enabled set and the order of one provider stage.
// exact reports that enabled is authoritative even when it is empty.
type providerSelection struct {
	enabled []string
	order   []string
	exact   bool
}

func containsFold(names []string, name string) bool {
	for _, current := range names {
		if strings.EqualFold(current, name) {
			return true
		}
	}
	return false
}

func disabled(names []string, name string) bool {
	for _, disabledName := range names {
		if strings.EqualFold(disabledName, name) {
			return true
		}
	}
	return false
}

// typeProviderSelection returns the enabled list and order of one stage for
// kind. Local metadata readers keep LocalMetadataReaderOrder as both: the
// registry runs only the readers it names, as before.
//
// Remote metadata follows BaseItemManager.IsMetadataFetcherEnabled: when the
// library has a TypeOptions entry for the kind, only the fetchers it names
// run, and an empty MetadataFetchers list runs none. Without an entry every
// remote provider runs; Deadfin does not consult the server MetadataOptions
// fallback.
//
// Local images follow ProviderManager.CanRefreshImages, which enables every
// ILocalImageProvider whatever ImageFetchers says. ImageFetchers names the
// image fetchers that /Libraries/AvailableOptions lists; run applies it to
// each provider that implements ImageFetcher (imageFetcherEnabled).
func typeProviderSelection(options library.LibraryOptions, kind item.Kind, stage ProviderStage) providerSelection {
	switch stage {
	case ProviderStageLocalMetadata:
		return providerSelection{enabled: options.LocalMetadataReaderOrder, order: options.LocalMetadataReaderOrder}
	case ProviderStageRemoteMetadata:
		for _, typeOptions := range options.TypeOptions {
			if strings.EqualFold(typeOptions.Type, kind.String()) {
				return providerSelection{enabled: typeOptions.MetadataFetchers, order: typeOptions.MetadataFetcherOrder, exact: true}
			}
		}
	}
	return providerSelection{}
}
