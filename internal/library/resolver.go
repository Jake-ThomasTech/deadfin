// SPDX-License-Identifier: GPL-2.0-only

package library

import (
	"context"
	"crypto/md5" // #nosec G501 -- Jellyfin item identity uses upstream MD5 GUID semantics.
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
)

// ResolverPriority is the upstream resolver ordering. Equal priorities retain
// registration order.
type ResolverPriority uint8

const (
	ResolverPriorityPlugin ResolverPriority = iota
	ResolverPriorityFirst
	ResolverPrioritySecond
	ResolverPriorityThird
	ResolverPriorityFourth
	ResolverPriorityFifth
	ResolverPriorityLast
)

// FileIdentity is the scanner-owned identity for a regular file or a
// directory. The scanner follows links, so a link entry carries the identity
// of its target.
// Valid is false on platforms whose filesystem metadata cannot provide it.
type FileIdentity struct {
	Device  uint64
	Inode   uint64
	Changed time.Time
	Valid   bool
}

// Entry is the filesystem fact gathered once by a future scanner. Resolvers do
// not inspect the filesystem themselves.
type Entry struct {
	Path     string
	IsDir    bool
	Size     int64
	Created  time.Time
	Modified time.Time
	Identity FileIdentity
}

// ResolveContext contains the scanner facts shared by every resolution pass.
type ResolveContext struct {
	Entry            Entry
	Children         []Entry
	ChildEntries     map[string][]Entry
	Parent           *item.Item
	Ancestors        []*item.Item
	CollectionType   item.CollectionType
	Options          LibraryOptions
	LibraryRoot      string
	ProgramDataRoot  string
	CaseSensitiveIDs bool
	PhysicalRoot     bool
	// IsPhysicalLibraryRoot distinguishes a configured media path from the
	// aggregate application root represented by PhysicalRoot.
	IsPhysicalLibraryRoot bool
	IsVirtualFolder       bool
}

// RefreshMode mirrors upstream MetadataRefreshMode
// (MediaBrowser.Controller/Providers/MetadataRefreshOptions.cs). The numeric
// ordering is load-bearing: the provider registry compares the mode against
// RefreshModeValidationOnly to gate the remote-metadata stage, exactly as
// MetadataService gates ExecuteRemoteProviders on
// options.MetadataRefreshMode > MetadataRefreshMode.ValidationOnly.
type RefreshMode uint8

const (
	// RefreshModeNone performs no provider work (upstream None = 0).
	RefreshModeNone RefreshMode = iota
	// RefreshModeValidationOnly revalidates local facts without remote fetch.
	RefreshModeValidationOnly
	// RefreshModeDefault runs providers under default rules; this is the mode a
	// plain library refresh threads, preserving the scanner's remote behavior.
	RefreshModeDefault
	// RefreshModeFullRefresh forces every provider (including remote) to run,
	// re-fetching an item that already has metadata.
	RefreshModeFullRefresh
)

var refreshModeNames = map[string]RefreshMode{
	"none":           RefreshModeNone,
	"validationonly": RefreshModeValidationOnly,
	"default":        RefreshModeDefault,
	"fullrefresh":    RefreshModeFullRefresh,
}

// UnmarshalText binds a refresh mode case-insensitively by upstream enum name
// (ASP.NET model binding) or by its integer value, so api/bind decodes the
// metadataRefreshMode/imageRefreshMode query parameters without bespoke parsing.
func (m *RefreshMode) UnmarshalText(text []byte) error {
	value := strings.TrimSpace(string(text))
	if value == "" {
		*m = RefreshModeNone
		return nil
	}
	if mode, ok := refreshModeNames[strings.ToLower(value)]; ok {
		*m = mode
		return nil
	}
	switch value {
	case "0":
		*m = RefreshModeNone
	case "1":
		*m = RefreshModeValidationOnly
	case "2":
		*m = RefreshModeDefault
	case "3":
		*m = RefreshModeFullRefresh
	default:
		return fmt.Errorf("invalid metadata refresh mode %q", value)
	}
	return nil
}

// RefreshOptions is the per-invocation refresh intent threaded through the scan
// and refresh path. It mirrors the subset of upstream MetadataRefreshOptions the
// scanner acts on: the metadata/image modes and the replace-all flags.
type RefreshOptions struct {
	Metadata                   RefreshMode
	Image                      RefreshMode
	ReplaceAllMetadata         bool
	ReplaceAllImages           bool
	RegenerateTrickplayItemIDs []dto.UUID
	// Targets limits the scan to the libraries that contain these items.
	// Upstream ProviderManager.RefreshItem refreshes only the target item and
	// its descendants. An empty Targets scans every library. A target that no
	// library contains also scans every library.
	Targets []RefreshTarget
}

// RefreshTarget identifies the item a refresh request names. Path is the
// item's stored path; it is empty for an item without a path.
type RefreshTarget struct {
	ItemID dto.UUID
	Path   string
}

// DefaultRefreshOptions is the intent a plain library scan/refresh runs under.
// Default mode preserves the scanner's existing remote behavior (remote runs
// when the library enables internet providers).
func DefaultRefreshOptions() RefreshOptions {
	return RefreshOptions{Metadata: RefreshModeDefault, Image: RefreshModeDefault}
}

// RefreshContext is the scanner's frozen directory snapshot for metadata
// enrichment. Providers must use these facts rather than enumerate again.
type RefreshContext struct {
	Directory    Entry
	Entries      []Entry
	ChildEntries map[string][]Entry
	Options      LibraryOptions
	LibraryRoot  string
	// Mode and Image carry the metadata and image refresh intent. Remote
	// providers run only when Mode exceeds RefreshModeValidationOnly (or a
	// replace-all flag forces them), matching upstream MetadataService.
	Mode               RefreshMode
	Image              RefreshMode
	ReplaceAllMetadata bool
	ReplaceAllImages   bool
	// SkipRemoteMetadata suppresses the remote metadata stage. The scanner
	// sets it for a stored item that upstream would not send to remote
	// providers: MetadataService.GetProviders runs every provider only on a
	// first refresh, a required refresh, FullRefresh, or ReplaceAllMetadata.
	// The zero value keeps the mode gate alone.
	SkipRemoteMetadata bool
	// SkipLocalMetadata suppresses the local metadata stage (NFO readers).
	// Identify sets it: upstream MetadataService does not run local metadata
	// providers when the refresh carries a SearchResult.
	SkipLocalMetadata bool
	// RunRemoteImages runs the remote image providers (ImageFetchers) after
	// the remote metadata stage. The scanner sets it when upstream
	// MetadataService.GetNonLocalImageProviders runs all of them: the image
	// mode is FullRefresh, or the item has no refresh yet.
	RunRemoteImages bool
	// StoredImages is a copy of the stored item's images. The remote image
	// stage does not fetch a type the stored item has, unless the refresh
	// replaces all images.
	StoredImages []item.ImageInfo
	// Ancestors is a copy of the target's ancestor identity facts, nearest
	// first. Hierarchy-aware providers read the parent series from it, as
	// upstream SeasonInfo/EpisodeInfo carry SeriesProviderIds and
	// SeriesDisplayOrder. It holds no reference to a scanner item.
	Ancestors []RefreshAncestor
}

// RefreshAncestor is an immutable copy of one ancestor's identity facts.
type RefreshAncestor struct {
	ID           dto.UUID
	Kind         item.Kind
	Name         string
	IndexNumber  *int
	ProviderIDs  item.ProviderIDs
	DisplayOrder string
}

// NewRefreshAncestor copies the identity facts of ancestor. Later changes to
// ancestor do not reach the copy.
func NewRefreshAncestor(ancestor *item.Item) RefreshAncestor {
	snapshot := RefreshAncestor{ID: ancestor.ID, Kind: ancestor.Kind}
	if ancestor.Name != nil {
		snapshot.Name = *ancestor.Name
	}
	if ancestor.IndexNumber != nil {
		index := *ancestor.IndexNumber
		snapshot.IndexNumber = &index
	}
	for _, id := range ancestor.ProviderIDs.Snapshot() {
		snapshot.ProviderIDs.Set(id.Name, id.Value)
	}
	if ancestor.Extra.Series != nil && ancestor.Extra.Series.DisplayOrder != nil {
		snapshot.DisplayOrder = *ancestor.Extra.Series.DisplayOrder
	}
	return snapshot
}

// NearestAncestor returns the nearest ancestor of kind.
func (refresh RefreshContext) NearestAncestor(kind item.Kind) (RefreshAncestor, bool) {
	for _, ancestor := range refresh.Ancestors {
		if ancestor.Kind == kind {
			return ancestor, true
		}
	}
	return RefreshAncestor{}, false
}

// Resolver maps one entry to an item. A nil item means it does not claim it.
type Resolver interface {
	Priority() ResolverPriority
	Resolve(context.Context, ResolveContext) (*item.Item, error)
}

// MultiResolver resolves sibling groups ahead of normal fallback resolution.
type MultiResolver interface {
	Resolver
	ResolveMany(context.Context, ResolveContext) ([]*item.Item, error)
}

// ResolvedCandidate carries resolver grouping provenance alongside a resolved
// item. Resolvers emit it only for a grouping decision that the scanner must
// materialize rather than reconstruct from display fields.
type ResolvedCandidate struct {
	Item                  *item.Item
	LocalAlternateVersion *LocalAlternateVersion
}

// LocalAlternateVersion records the resolver-selected primary identity and
// stable alternate ordering. It is scanner-plan provenance, not persisted item
// metadata.
type LocalAlternateVersion struct {
	PrimaryID dto.UUID
	Index     int
}

// CandidateMultiResolver extends MultiResolver with scanner-plan provenance.
// Resolvers that do not make a grouping decision continue through ResolveMany.
type CandidateMultiResolver interface {
	ResolveCandidates(context.Context, ResolveContext) ([]ResolvedCandidate, error)
}

// Refresher enriches resolved items from one frozen directory snapshot before
// the scanner validates and atomically reconciles the complete child plan.
// Resolvers intentionally neither receive nor invoke it.
type Refresher interface {
	Refresh(context.Context, RefreshContext, *item.Item) error
}

// RefreshOutcome reports what one provider refresh did.
type RefreshOutcome struct {
	// RemoteFailures counts the remote providers that returned an error.
	// Upstream sets DateLastRefreshed only when no provider failed
	// (MetadataService.RefreshMetadata), so a failed item retries on the
	// next scan.
	RemoteFailures int
}

// OutcomeRefresher is a Refresher that also reports the outcome. The scanner
// uses it when the injected Refresher implements it.
type OutcomeRefresher interface {
	RefreshWithOutcome(context.Context, RefreshContext, *item.Item) (RefreshOutcome, error)
}

// MetadataChangeMonitor reports whether a local metadata source of a stored
// item changed after the item was last saved. It mirrors upstream
// IHasItemChangeMonitor on the local metadata providers, which
// MetadataService.GetProviders asks before it runs them on a Default
// refresh.
type MetadataChangeMonitor interface {
	LocalMetadataChanged(refresh RefreshContext, stored item.Item) bool
}

// ResolverFunc is the focused injection adapter for a single resolver.
type ResolverFunc struct {
	At        ResolverPriority
	ResolveFn func(context.Context, ResolveContext) (*item.Item, error)
}

func (r ResolverFunc) Priority() ResolverPriority { return r.At }
func (r ResolverFunc) Resolve(ctx context.Context, input ResolveContext) (*item.Item, error) {
	if r.ResolveFn == nil {
		return nil, nil
	}
	return r.ResolveFn(ctx, input)
}

// MultiResolverFunc is the focused injection adapter for sibling resolution.
// ResolveCandidatesFn optionally carries explicit alternate topology for the
// scanner; ResolveManyFn remains the fallback when it returns no candidates.
type MultiResolverFunc struct {
	ResolverFunc
	ResolveManyFn       func(context.Context, ResolveContext) ([]*item.Item, error)
	ResolveCandidatesFn func(context.Context, ResolveContext) ([]ResolvedCandidate, error)
}

func (r MultiResolverFunc) ResolveMany(ctx context.Context, input ResolveContext) ([]*item.Item, error) {
	if r.ResolveManyFn == nil {
		return nil, nil
	}
	return r.ResolveManyFn(ctx, input)
}

func (r MultiResolverFunc) ResolveCandidates(ctx context.Context, input ResolveContext) ([]ResolvedCandidate, error) {
	if r.ResolveCandidatesFn == nil {
		return nil, nil
	}
	return r.ResolveCandidatesFn(ctx, input)
}

// Chain applies its registered resolvers in stable upstream priority order.
type Chain struct{ resolvers []Resolver }

// NewChain constructs a custom ordered chain. NewDefaultChain installs the
// standard naming-backed adapters.
func NewChain(resolvers ...Resolver) *Chain {
	ordered := append([]Resolver(nil), resolvers...)
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left] == nil {
			return false
		}
		if ordered[right] == nil {
			return true
		}
		return ordered[left].Priority() < ordered[right].Priority()
	})
	return &Chain{resolvers: ordered}
}

// Resolvers returns a copy of the ordered resolver registration list.
func (c *Chain) Resolvers() []Resolver {
	if c == nil || len(c.resolvers) == 0 {
		return nil
	}
	return append([]Resolver(nil), c.resolvers...)
}

// Resolve returns the first non-nil resolution or its terminating error.
func (c *Chain) Resolve(ctx context.Context, input ResolveContext) (*item.Item, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	for _, resolver := range c.resolvers {
		if err := contextErr(ctx); err != nil {
			return nil, err
		}
		if resolver == nil {
			continue
		}
		resolved, err := resolver.Resolve(ctx, input)
		if err != nil {
			return nil, err
		}
		if resolved != nil {
			return resolved, nil
		}
	}
	return nil, nil
}

// ResolveMany calls multi resolvers in priority order and returns the first
// non-empty result. It deliberately does not fall through to single resolvers.
func (c *Chain) ResolveMany(ctx context.Context, input ResolveContext) ([]*item.Item, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	for _, resolver := range c.resolvers {
		multi, ok := resolver.(MultiResolver)
		if !ok || multi == nil {
			continue
		}
		if err := contextErr(ctx); err != nil {
			return nil, err
		}
		items, err := multi.ResolveMany(ctx, input)
		if err != nil {
			return nil, err
		}
		if len(items) != 0 {
			return items, nil
		}
	}
	return nil, nil
}

// ResolveCandidates preserves scanner-only provenance emitted by a multi
// resolver. Its selection semantics are deliberately identical to ResolveMany:
// the first non-empty multi resolver wins at its configured priority.
func (c *Chain) ResolveCandidates(ctx context.Context, input ResolveContext) ([]ResolvedCandidate, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	for _, resolver := range c.resolvers {
		multi, ok := resolver.(MultiResolver)
		if !ok || multi == nil {
			continue
		}
		if err := contextErr(ctx); err != nil {
			return nil, err
		}
		if candidates, ok := resolver.(CandidateMultiResolver); ok {
			resolved, err := candidates.ResolveCandidates(ctx, input)
			if err != nil {
				return nil, err
			}
			if len(resolved) != 0 {
				return resolved, nil
			}
		}
		items, err := multi.ResolveMany(ctx, input)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			continue
		}
		resolved := make([]ResolvedCandidate, 0, len(items))
		for _, value := range items {
			resolved = append(resolved, ResolvedCandidate{Item: value})
		}
		return resolved, nil
	}
	return nil, nil
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func initialize(entry Entry, kind item.Kind, parent *item.Item) item.Item {
	path := entry.Path
	name := filepath.Base(path)
	if !entry.IsDir {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	if name == "." || name == string(filepath.Separator) {
		name = ""
	}
	result := item.Item{Kind: kind, Path: &path, Name: &name, IsFolder: entry.IsDir}
	if parent != nil {
		parentID := parent.ID
		result.ParentID = &parentID
		if parent.TopParentID != nil {
			top := *parent.TopParentID
			result.TopParentID = &top
		} else {
			top := parent.ID
			result.TopParentID = &top
		}
	}
	if !entry.Created.IsZero() {
		created := entry.Created
		result.DateCreated = &created
	}
	if !entry.Modified.IsZero() {
		modified := entry.Modified
		result.DateModified = &modified
	}
	if !entry.IsDir {
		size := entry.Size
		result.Size = &size
	}
	applyEntryFileIdentity(&result, entry)
	if definition, found := item.Definition(kind); found && definition.DefaultMediaType != item.MediaTypeUnknown {
		mediaType := definition.DefaultMediaType
		result.MediaType = &mediaType
	}
	return result
}

func applyEntryFileIdentity(result *item.Item, entry Entry) {
	result.FileDevice = nil
	result.FileInode = nil
	result.FileChangedAt = nil
	if entry.IsDir || !entry.Identity.Valid {
		return
	}
	device := int64(entry.Identity.Device)
	inode := int64(entry.Identity.Inode)
	changed := entry.Identity.Changed.UTC()
	result.FileDevice = &device
	result.FileInode = &inode
	result.FileChangedAt = &changed
}

func initializeContext(input ResolveContext, kind item.Kind) item.Item {
	result := initialize(input.Entry, kind, input.Parent)
	result.ID = itemID(kind, input.Entry.Path, input.ProgramDataRoot, input.CaseSensitiveIDs)
	// A folder whose parent is the AggregateFolder is the library boundary and
	// the top parent for its descendants, per upstream BaseItem.IsTopParent
	// (BaseItem.cs:755-758: GetParent() is AggregateFolder). Its TopParentID is
	// its own ID, not the AggregateFolder's ID. Descendants then inherit this
	// folder's ID through the initialize chain and stop at the library root.
	if input.Parent != nil && input.Parent.Kind == item.KindAggregateFolder {
		top := result.ID
		result.TopParentID = &top
	}
	result.IsLocked = strings.Contains(strings.ToLower(input.Entry.Path), "[dontfetchmeta]") || input.Parent != nil && input.Parent.IsLocked
	if !result.IsLocked {
		for _, ancestor := range input.Ancestors {
			if ancestor != nil && ancestor.IsLocked {
				result.IsLocked = true
				break
			}
		}
	}
	return result
}

// ItemID returns the item identity LibraryManager.GetNewItemId gives a kind
// at path. A path under programDataRoot uses its portable relative key.
func ItemID(kind item.Kind, path, programDataRoot string) dto.UUID {
	return itemID(kind, path, programDataRoot, false)
}

// itemID ports LibraryManager.GetNewItemId's source type key and path rules.
func itemID(kind item.Kind, path, programDataRoot string, caseSensitive bool) dto.UUID {
	key := path
	if programDataRoot != "" && strings.HasPrefix(key, programDataRoot) {
		key = strings.TrimLeft(key[len(programDataRoot):], "/\\")
		key = strings.ReplaceAll(key, "/", "\\")
	}
	if !caseSensitive {
		key = dotNetInvariantLower(key)
	}
	sum := md5.Sum(utf16LE(itemCLRType(kind) + key)) // #nosec G401 -- compatibility identity, not security.
	return dto.UUID{sum[3], sum[2], sum[1], sum[0], sum[5], sum[4], sum[7], sum[6], sum[8], sum[9], sum[10], sum[11], sum[12], sum[13], sum[14], sum[15]}
}

// PersonEntity constructs the durable metadata item Jellyfin creates for a
// locally credited person. Its path remains portable while its identity uses
// the source's normalized metadata-relative key.
func PersonEntity(name string) item.Item {
	name = strings.TrimSpace(name)
	path := personMetadataPath(name)
	identityPath := "metadata\\" + strings.ReplaceAll(strings.TrimPrefix(path, "%MetadataPath%/"), "/", "\\")
	result := item.Item{
		ID:   itemID(item.KindPerson, identityPath, "", false),
		Kind: item.KindPerson,
		Path: &path,
		Name: &name,
	}
	keys := item.UserDataKeys(result, item.UserDataInputs{})
	if len(keys) != 0 {
		result.PresentationUniqueKey = &keys[0]
	}
	return result
}

func personMetadataPath(name string) string {
	prefix := ""
	for _, character := range name {
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			prefix = string(character)
			break
		}
	}
	if prefix == "" {
		return "%MetadataPath%/People/" + name
	}
	return "%MetadataPath%/People/" + prefix + "/" + name
}

func utf16LE(value string) []byte {
	units := utf16.Encode([]rune(value))
	result := make([]byte, len(units)*2)
	for index, unit := range units {
		result[index*2] = byte(unit)
		result[index*2+1] = byte(unit >> 8)
	}
	return result
}

func dotNetInvariantLower(value string) string {
	return strings.Map(func(character rune) rune {
		if character == '\u0130' {
			return character
		}
		return unicode.ToLower(character)
	}, value)
}

func itemCLRType(kind item.Kind) string {
	switch kind {
	case item.KindMovie:
		return "MediaBrowser.Controller.Entities.Movies.Movie"
	case item.KindBoxSet:
		return "MediaBrowser.Controller.Entities.Movies.BoxSet"
	case item.KindSeries:
		return "MediaBrowser.Controller.Entities.TV.Series"
	case item.KindSeason:
		return "MediaBrowser.Controller.Entities.TV.Season"
	case item.KindEpisode:
		return "MediaBrowser.Controller.Entities.TV.Episode"
	case item.KindAudio:
		return "MediaBrowser.Controller.Entities.Audio.Audio"
	case item.KindAudioBook:
		return "MediaBrowser.Controller.Entities.AudioBook"
	case item.KindMusicArtist:
		return "MediaBrowser.Controller.Entities.Audio.MusicArtist"
	case item.KindMusicAlbum:
		return "MediaBrowser.Controller.Entities.Audio.MusicAlbum"
	case item.KindBook:
		return "MediaBrowser.Controller.Entities.Book"
	case item.KindPlaylist:
		return "MediaBrowser.Controller.Playlists.Playlist"
	default:
		return "MediaBrowser.Controller.Entities." + kind.String()
	}
}

func collectionIs(input ResolveContext, collection item.CollectionType) bool {
	return input.CollectionType == collection
}
