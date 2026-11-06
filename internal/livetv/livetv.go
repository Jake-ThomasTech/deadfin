// SPDX-License-Identifier: GPL-2.0-only

// Package livetv runs the Live TV guide: it stores tuner channels and guide
// programmes as items and manages the configured tuners and listing
// providers. It ports GuideManager, ListingsManager and TunerHostManager
// (src/Jellyfin.LiveTv). The subpackages hold the parts: tuner lists
// channels, guide reads listings, catalog maps both to items.
//
// Live TV is off while no tuner is configured (D20); then RefreshGuide
// writes nothing unless an earlier refresh stored a Live TV folder.
package livetv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/livetv/catalog"
	"github.com/deadfin-org/deadfin/internal/livetv/guide"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

// ErrUnknownType reports a tuner or listing provider type that no
// registered implementation handles. The API maps it to 404, as upstream
// throws ResourceNotFoundException.
var ErrUnknownType = errors.New("livetv: unknown type")

// ErrNotFound reports a configured tuner, provider or channel that does not
// exist.
var ErrNotFound = errors.New("livetv: not found")

// Config is the Live TV half of the configuration manager.
type Config interface {
	LiveTv() config.LiveTvOptions
	UpdateLiveTv(ctx context.Context, update func(*config.LiveTvOptions) error) error
}

// Items is the item storage the guide writes. cmd/deadfin adapts
// store.ItemStore to it.
type Items interface {
	// Item returns a stored item; ok is false when it does not exist.
	Item(ctx context.Context, id dto.UUID) (value item.Item, ok bool, err error)
	// Upsert writes one item.
	Upsert(ctx context.Context, value item.Item) error
	// Children returns the direct children of a parent.
	Children(ctx context.Context, parentID dto.UUID) ([]item.Item, error)
	// ReplaceChildren replaces the complete direct-child set of a parent.
	// It deletes removed children with their subtrees and writes only
	// changed rows.
	ReplaceChildren(ctx context.Context, parentID dto.UUID, desired []item.Item) (Changes, error)
	// MergeChildren writes the changed children of a parent and keeps
	// every stored child it omits.
	MergeChildren(ctx context.Context, parentID dto.UUID, desired []item.Item) (Changes, error)
}

// Changes counts the rows one ReplaceChildren call wrote.
type Changes struct {
	Inserted int
	Updated  int
	Deleted  int
}

func (c *Changes) add(other Changes) {
	c.Inserted += other.Inserted
	c.Updated += other.Updated
	c.Deleted += other.Deleted
}

// ListingsProvider is one guide source type: the members of
// IListingsProvider that XMLTV implements. guide.XMLTV satisfies it.
type ListingsProvider interface {
	Type() string
	Name() string
	Validate(info config.ListingsProviderInfo) error
	Channels(ctx context.Context, info config.ListingsProviderInfo, language string) ([]guide.Channel, error)
	Programs(ctx context.Context, info config.ListingsProviderInfo, language string, channelIDs []string, start, end time.Time, yield func(guide.Program) error) error
}

// validator is a tuner type that checks a tuner before it is saved
// (IConfigurableTunerHost).
type validator interface {
	Validate(ctx context.Context, tuner config.TunerHostInfo) error
}

// forgetter is a tuner type that caches channels per tuner.
type forgetter interface {
	Forget(tuner config.TunerHostInfo)
}

// Paths are the server directories the guide uses.
type Paths struct {
	// ProgramData is the server data root; item ids of paths under it are
	// relative (BaseItem.GetNewItemId).
	ProgramData string
	// InternalMetadata is the metadata root; the Live TV folder is
	// <InternalMetadata>/views/livetv.
	InternalMetadata string
	// Cache is the cache root; XMLTV copies are <Cache>/xmltv/<id>.xml.
	Cache string
}

// Options are the dependencies of a Service.
type Options struct {
	Config   Config
	Tuners   *tuner.Registry
	Listings []ListingsProvider
	Items    Items
	Clock    clock.Clock
	Logger   *slog.Logger
	Paths    Paths
	// MetadataLanguage returns the server PreferredMetadataLanguage, the
	// guide language when a provider sets none.
	MetadataLanguage func() string
	// Queue cancels a running Refresh Guide and queues a new run
	// (ITaskManager.CancelIfRunningAndQueue). Nil does nothing.
	Queue func()
	// NewID returns a new configuration id. Nil uses a random Guid "N".
	NewID func() string
}

// Service is the Live TV guide service. It is safe for concurrent use;
// RefreshGuide runs one at a time.
type Service struct {
	options Options
	refresh sync.Mutex
}

// New returns a Service.
func New(options Options) (*Service, error) {
	if options.Config == nil || options.Tuners == nil || options.Items == nil || options.Clock == nil {
		return nil, errors.New("livetv: config, tuners, items and clock are required")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.MetadataLanguage == nil {
		options.MetadataLanguage = func() string { return "" }
	}
	if options.NewID == nil {
		options.NewID = newGuidN
	}
	return &Service{options: options}, nil
}

// newGuidN returns a random id formatted as Guid.ToString("N").
func newGuidN() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

func (s *Service) queue() {
	if s.options.Queue != nil {
		s.options.Queue()
	}
}

// listings returns the configured providers that have a registered type, in
// configuration order (ListingsManager.GetListingProviders).
func (s *Service) listings(options config.LiveTvOptions) []configuredListings {
	out := make([]configuredListings, 0, len(options.ListingProviders))
	for _, info := range options.ListingProviders {
		if provider, ok := s.listingsProvider(info.Type); ok {
			out = append(out, configuredListings{provider: provider, info: info})
		}
	}
	return out
}

type configuredListings struct {
	provider ListingsProvider
	info     config.ListingsProviderInfo
}

func (s *Service) listingsProvider(providerType string) (ListingsProvider, bool) {
	for _, provider := range s.options.Listings {
		if equalFold(provider.Type(), providerType) {
			return provider, true
		}
	}
	return nil, false
}

func (s *Service) language(info config.ListingsProviderInfo) string {
	return guide.Language(info, s.options.MetadataLanguage())
}

// FolderID returns the id of the Live TV folder that holds the guide items
// (LiveTvManager.GetInternalLiveTvFolder).
func (s *Service) FolderID() dto.UUID {
	return catalog.Folder(s.options.Paths.InternalMetadata, s.options.Paths.ProgramData, s.options.Clock.Now()).ID
}

// LiveTvFolder returns the Live TV folder while a tuner is configured, and
// stores it when no refresh has stored it yet
// (LiveTvManager.GetInternalLiveTvFolder). ok is false while no tuner is
// configured (LiveTvManager.IsLiveTvEnabled).
func (s *Service) LiveTvFolder(ctx context.Context) (folder item.Item, ok bool, err error) {
	if len(s.options.Config.LiveTv().TunerHosts) == 0 {
		return item.Item{}, false, nil
	}
	folder = catalog.Folder(s.options.Paths.InternalMetadata, s.options.Paths.ProgramData, s.options.Clock.Now())
	stored, exists, err := s.options.Items.Item(ctx, folder.ID)
	if err != nil {
		return item.Item{}, false, err
	}
	if exists {
		return stored, true, nil
	}
	if err := s.options.Items.Upsert(ctx, folder); err != nil {
		return item.Item{}, false, err
	}
	return folder, true, nil
}
