// SPDX-License-Identifier: GPL-2.0-only

package providers

import (
	"context"
	"math"
	"net/url"
	"sort"
	"strings"

	"github.com/deadfin-org/deadfin/internal/item"
)

// Lookup is the plain-data search input for remote-search providers, mirroring
// the pinned *Info schemas (jellyport/docs/07-extension-seams.md:29-53). It is
// marshalable — no live server objects — so a future loader can expose the same
// seam to out-of-process plugins.
type Lookup struct {
	Kind        item.Kind
	Name        string
	Year        *int
	ProviderIDs map[string]string
	Language    string
	Country     string
	// Per-kind extras (AlbumInfo, ArtistInfo, BookInfo).
	AlbumArtists []string
	SongInfos    []SongInfo
	SeriesName   string
	ArtistIDs    map[string]string
}

// SongInfo is a track-level lookup for album/artist search, mirroring the
// pinned SongInfo schema. ProviderIDs carries the song's provider IDs
// (e.g. MusicBrainzAlbum, MusicBrainzAlbumArtist) so upstream's
// AlbumInfoExtensions fallback paths work (AlbumInfoExtensions.cs).
type SongInfo struct {
	Name         string
	AlbumArtists []string
	Album        string
	Artists      []string
	ProviderIDs  map[string]string
}

// RemoteSearchResult is one remote-search hit, mirroring the pinned
// RemoteSearchResult field set (upstream/jellyfin-openapi-stable.json).
type RemoteSearchResult struct {
	Name               *string
	ProviderIDs        map[string]string
	ProductionYear     *int
	IndexNumber        *int
	IndexNumberEnd     *int
	ParentIndexNumber  *int
	PremiereDate       *string
	ImageURL           string
	SearchProviderName string
	Overview           *string
	AlbumArtist        *RemoteSearchResult
	Artists            []RemoteSearchResult
}

// RemoteImageInfo is one remote image record, mirroring the pinned
// RemoteImageInfo field set.
type RemoteImageInfo struct {
	ProviderName    string
	URL             string
	ThumbnailURL    string
	Height          int
	Width           int
	CommunityRating float64
	VoteCount       int
	Language        string
	Type            item.ImageType
	RatingType      string
}

// Searcher is the remote-search capability. Providers implement it to answer
// POST /Items/RemoteSearch/{Type}. A provider that does not support a kind
// returns false from SearchSupports and is excluded from search.
type Searcher interface {
	Name() string
	SearchSupports(item.Kind) bool
	Search(ctx context.Context, lookup Lookup) ([]RemoteSearchResult, error)
}

// ImageLookup is the input for remote-image listing.
type ImageLookup struct {
	Kind     item.Kind
	Language string
	// ProviderIDs carries the item's existing provider IDs for image providers
	// that resolve by external ID (for example, a TMDb poster by Tmdb id).
	ProviderIDs map[string]string
}

// ImageLister is the remote-image capability. Providers implement it to
// answer GET /Items/{itemId}/RemoteImages.
type ImageLister interface {
	Name() string
	ImageSupports(item.Kind) bool
	ImageTypes(item.Kind) []item.ImageType
	Images(ctx context.Context, lookup ImageLookup) ([]RemoteImageInfo, error)
}

// LookupRegistry holds ordered lists of search and image-lister capabilities.
// It is built in the composition root (no init() self-registration, CONVENTIONS
// 1.1) from the same provider instances the refresh Registry uses.
type LookupRegistry struct {
	searchers []Searcher
	listers   []ImageLister
}

// NewLookupRegistry constructs a capability registry from ordered provider
// lists. The order mirrors registration order; per-library MetadataFetcherOrder
// is applied at search time by the identify service.
func NewLookupRegistry(searchers []Searcher, listers []ImageLister) *LookupRegistry {
	return &LookupRegistry{
		searchers: append([]Searcher(nil), searchers...),
		listers:   append([]ImageLister(nil), listers...),
	}
}

// Searchers returns the ordered searchers. The identify service applies the
// enabled-provider filter and per-library ordering before calling them.
func (r *LookupRegistry) Searchers() []Searcher {
	if r == nil {
		return nil
	}
	return r.searchers
}

// Listers returns the ordered image listers.
func (r *LookupRegistry) Listers() []ImageLister {
	if r == nil {
		return nil
	}
	return r.listers
}

// ListerByName returns the image lister with the given name (case-insensitive),
// or nil.
func (r *LookupRegistry) ListerByName(name string) ImageLister {
	if r == nil {
		return nil
	}
	for _, lister := range r.listers {
		if strings.EqualFold(lister.Name(), name) {
			return lister
		}
	}
	return nil
}

// FilteredSearchers returns searchers enabled for the given kind and library
// options, in the correct order. When IncludeDisabledProviders is set, the
// enabled filter is bypassed (ProviderManager.cs:571-574). The enabled filter
// distinguishes three cases: enabled == nil means no TypeOptions exist for the
// kind, so all providers are enabled (BaseItemManager.cs:26-47); enabled ==
// []string{} means TypeOptions exist with an explicit empty MetadataFetchers
// list, so no providers are enabled; enabled is non-empty means only providers
// named in the list are enabled (case-insensitive). Ordering uses
// MetadataFetcherOrder when present, then registration order
// (ProviderManager.cs:597-607; same shape as registry.go:218-240).
func FilteredSearchers(searchers []Searcher, kind item.Kind, order, enabled []string, includeDisabled bool) []Searcher {
	kindSearchers := make([]Searcher, 0, len(searchers))
	for _, s := range searchers {
		if s.SearchSupports(kind) {
			kindSearchers = append(kindSearchers, s)
		}
	}
	if !includeDisabled && enabled != nil {
		filtered := make([]Searcher, 0, len(kindSearchers))
		for _, s := range kindSearchers {
			if nameInFold(s.Name(), enabled) {
				filtered = append(filtered, s)
			}
		}
		kindSearchers = filtered
	}
	if len(order) == 0 {
		return kindSearchers
	}
	ordered := make([]Searcher, 0, len(kindSearchers))
	used := make([]bool, len(kindSearchers))
	for _, name := range order {
		for i, s := range kindSearchers {
			if used[i] || !strings.EqualFold(s.Name(), name) {
				continue
			}
			used[i] = true
			ordered = append(ordered, s)
		}
	}
	for i, s := range kindSearchers {
		if !used[i] {
			ordered = append(ordered, s)
		}
	}
	return ordered
}

// FilteredListers returns image listers supporting the given kind. The disabled
// filter is always bypassed for image listing (RemoteImageController.cs:85 sets
// IncludeDisabledProviders = true).
func FilteredListers(listers []ImageLister, kind item.Kind) []ImageLister {
	result := make([]ImageLister, 0, len(listers))
	for _, l := range listers {
		if l.ImageSupports(kind) {
			result = append(result, l)
		}
	}
	return result
}

// MergeSearchResults iterates provider results in order, tagging each result
// with SearchProviderName, and dedupes by any case-insensitive provider-id match,
// merging missing provider IDs and a missing ImageURL into the first match
// (ProviderManager.cs:967-988). Per-provider errors are logged and swallowed
// (ProviderManager.cs:990-996). Deterministic: provider order × provider result
// order × sorted provider-id keys (no Go map iteration), no re-sort.
func MergeSearchResults(perProvider [][]RemoteSearchResult) []RemoteSearchResult {
	var merged []RemoteSearchResult
	seen := make(map[string]int) // lowercase provider-id -> index in merged
	for _, results := range perProvider {
		for _, result := range results {
			// Find the earliest existing match across ALL provider IDs.
			// Iterating sorted keys makes the scan deterministic, but the
			// candidate merges into the lowest-indexed existing result, not
			// the first key that hits.
			mergedIdx := -1
			for _, key := range sortedProviderIDKeys(result.ProviderIDs) {
				value := result.ProviderIDs[key]
				if value == "" {
					continue
				}
				lookupKey := strings.ToLower(key) + ":" + strings.ToLower(value)
				if idx, ok := seen[lookupKey]; ok && (mergedIdx < 0 || idx < mergedIdx) {
					mergedIdx = idx
				}
			}
			if mergedIdx >= 0 {
				mergeResultInto(&merged[mergedIdx], result)
				for _, key := range sortedProviderIDKeys(result.ProviderIDs) {
					value := result.ProviderIDs[key]
					if value == "" {
						continue
					}
					seen[strings.ToLower(key)+":"+strings.ToLower(value)] = mergedIdx
				}
			} else {
				tagged := result
				merged = append(merged, tagged)
				idx := len(merged) - 1
				for _, key := range sortedProviderIDKeys(result.ProviderIDs) {
					value := result.ProviderIDs[key]
					if value == "" {
						continue
					}
					seen[strings.ToLower(key)+":"+strings.ToLower(value)] = idx
				}
			}
		}
	}
	return merged
}

// sortedProviderIDKeys returns the keys of a provider-id map sorted
// alphabetically, so the dedup scan in MergeSearchResults is deterministic
// regardless of Go map iteration order.
func sortedProviderIDKeys(ids map[string]string) []string {
	keys := make([]string, 0, len(ids))
	for k := range ids {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func mergeResultInto(existing *RemoteSearchResult, candidate RemoteSearchResult) {
	if existing.ImageURL == "" && candidate.ImageURL != "" {
		existing.ImageURL = candidate.ImageURL
	}
	// Merge provider IDs case-insensitively: a candidate "tmdb" key merges
	// into an existing "Tmdb" key so no case-variant duplicates remain.
	for name, value := range candidate.ProviderIDs {
		if value == "" {
			continue
		}
		if _, has := existing.ProviderIDs[name]; !has {
			// Check for a case-variant of the same key.
			canonical := existingCanonicalKey(existing.ProviderIDs, name)
			if canonical == "" {
				if existing.ProviderIDs == nil {
					existing.ProviderIDs = make(map[string]string)
				}
				existing.ProviderIDs[name] = value
			} else if existing.ProviderIDs[canonical] == "" {
				existing.ProviderIDs[canonical] = value
			}
		}
	}
}

// existingCanonicalKey returns the existing map key that case-insensitively
// matches name, or "" if none.
func existingCanonicalKey(ids map[string]string, name string) string {
	lower := strings.ToLower(name)
	for k := range ids {
		if strings.ToLower(k) == lower {
			return k
		}
	}
	return ""
}

// SortRemoteImages orders remote images per upstream's OrderByLanguageDescending
// (MediaBrowser.Model/Extensions/EnumerableExtensions.cs:19-56): score 4 =
// preferred language (default "en" when unset), 3 = "en", 2 = no language, 0 =
// other; then CommunityRating (rounded to 1 decimal) desc, then VoteCount desc.
// Uses sort.SliceStable (divergence D-09 pattern).
func SortRemoteImages(images []RemoteImageInfo, preferredLanguage string) {
	if preferredLanguage == "" {
		preferredLanguage = "en"
	}
	preferredLanguage = strings.ToLower(preferredLanguage)
	sort.SliceStable(images, func(i, j int) bool {
		si := imageLanguageScore(images[i].Language, preferredLanguage)
		sj := imageLanguageScore(images[j].Language, preferredLanguage)
		if si != sj {
			return si > sj
		}
		ri := roundTo1(images[i].CommunityRating)
		rj := roundTo1(images[j].CommunityRating)
		if ri != rj {
			return ri > rj
		}
		return images[i].VoteCount > images[j].VoteCount
	})
}

func imageLanguageScore(language, preferred string) int {
	lang := strings.ToLower(strings.TrimSpace(language))
	if lang == "" {
		return 2
	}
	if lang == preferred {
		return 4
	}
	if lang == "en" {
		return 3
	}
	return 0
}

// roundTo1 rounds a rating to one decimal using midpoint-to-even, matching
// upstream's Math.Round(value, 1) (EnumerableExtensions.cs:53), which uses
// MidpointRounding.ToEven. The previous int(x*10+0.5)/10 form rounded
// midpoint values upward instead of to even.
func roundTo1(value float64) float64 {
	return math.RoundToEven(value*10) / 10
}

// FilterImagesByType keeps only images matching the given type. The caller
// is responsible for deciding whether to filter; this function always applies
// the type match (Primary is 0 and is a valid filter value).
func FilterImagesByType(images []RemoteImageInfo, imageType item.ImageType) []RemoteImageInfo {
	result := make([]RemoteImageInfo, 0, len(images))
	for _, img := range images {
		if img.Type == imageType {
			result = append(result, img)
		}
	}
	return result
}

// FilterImagesByLanguage keeps languageless, exact preferred-language, or "en"
// images unless includeAllLanguages is set (ProviderManager.cs:331-356).
func FilterImagesByLanguage(images []RemoteImageInfo, preferredLanguage string, includeAllLanguages bool) []RemoteImageInfo {
	if includeAllLanguages {
		return images
	}
	preferredLanguage = strings.ToLower(strings.TrimSpace(preferredLanguage))
	if preferredLanguage == "" {
		preferredLanguage = "en"
	}
	result := make([]RemoteImageInfo, 0, len(images))
	for _, img := range images {
		lang := strings.ToLower(strings.TrimSpace(img.Language))
		if lang == "" || lang == preferredLanguage || lang == "en" {
			result = append(result, img)
		}
	}
	return result
}

// IsAllowedImageURL reports whether urlStr is an http(s) URL, the scheme check
// for a remote image download.
func IsAllowedImageURL(urlStr string) bool {
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return scheme == "http" || scheme == "https"
}

func nameInFold(name string, list []string) bool {
	for _, entry := range list {
		if strings.EqualFold(name, entry) {
			return true
		}
	}
	return false
}

// ItemToMap converts an item.ProviderIDs to a plain map for lookups and image
// queries. Entries with empty values are dropped.
func ItemToMap(ids item.ProviderIDs) map[string]string {
	snapshot := ids.Snapshot()
	if len(snapshot) == 0 {
		return nil
	}
	m := make(map[string]string, len(snapshot))
	for _, id := range snapshot {
		if id.Value != "" {
			m[id.Name] = id.Value
		}
	}
	return m
}

// ItemFromResultIDs builds an item.ProviderIDs from a result's provider-id map,
// for wholesale replacement during Apply (ItemLookupController.cs:266).
func ItemFromResultIDs(ids map[string]string) item.ProviderIDs {
	var result item.ProviderIDs
	for name, value := range ids {
		if value == "" {
			continue
		}
		result.Set(name, value)
	}
	return result
}
