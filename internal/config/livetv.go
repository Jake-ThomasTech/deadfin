// SPDX-License-Identifier: GPL-2.0-only

package config

import (
	"context"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"

	"github.com/deadfin-org/deadfin/internal/livetv/secret"
)

// LiveTvOptions is the persisted Live TV configuration (livetv.xml). It
// matches upstream/jellyfin/MediaBrowser.Model/LiveTv/LiveTvOptions.cs, which
// src/Jellyfin.LiveTv/Configuration/LiveTvConfigurationFactory.cs registers
// under the key "livetv". Live TV is off while TunerHosts is empty (D20).
//
// C# reference-type strings are null by default and omitted on the wire;
// they are omitzero here (CONVENTIONS 3.2).
type LiveTvOptions struct {
	GuideDays                                *int                   `json:"GuideDays,omitzero"`
	RecordingPath                            string                 `json:"RecordingPath,omitzero"`
	MovieRecordingPath                       string                 `json:"MovieRecordingPath,omitzero"`
	SeriesRecordingPath                      string                 `json:"SeriesRecordingPath,omitzero"`
	EnableRecordingSubfolders                bool                   `json:"EnableRecordingSubfolders"`
	EnableOriginalAudioWithEncodedRecordings bool                   `json:"EnableOriginalAudioWithEncodedRecordings"`
	TunerHosts                               []TunerHostInfo        `json:"TunerHosts"`
	ListingProviders                         []ListingsProviderInfo `json:"ListingProviders"`
	PrePaddingSeconds                        int                    `json:"PrePaddingSeconds"`
	PostPaddingSeconds                       int                    `json:"PostPaddingSeconds"`
	MediaLocationsCreated                    []string               `json:"MediaLocationsCreated"`
	RecordingPostProcessor                   string                 `json:"RecordingPostProcessor,omitzero"`
	RecordingPostProcessorArguments          string                 `json:"RecordingPostProcessorArguments,omitzero"`
	SaveRecordingNFO                         bool                   `json:"SaveRecordingNFO"`
	SaveRecordingImages                      bool                   `json:"SaveRecordingImages"`
}

// DefaultLiveTvOptions returns the upstream constructor defaults
// (LiveTvOptions.cs:9-15, 49-51).
func DefaultLiveTvOptions() LiveTvOptions {
	return LiveTvOptions{
		TunerHosts:                      []TunerHostInfo{},
		ListingProviders:                []ListingsProviderInfo{},
		MediaLocationsCreated:           []string{},
		RecordingPostProcessorArguments: `"{path}"`,
		SaveRecordingNFO:                true,
		SaveRecordingImages:             true,
	}
}

// TunerHostInfo is one configured tuner. It matches
// upstream/jellyfin/MediaBrowser.Model/LiveTv/TunerHostInfo.cs.
type TunerHostInfo struct {
	Id                            string `json:"Id,omitzero"`
	Url                           string `json:"Url,omitzero"`
	Type                          string `json:"Type,omitzero"`
	DeviceId                      string `json:"DeviceId,omitzero"`
	FriendlyName                  string `json:"FriendlyName,omitzero"`
	ImportFavoritesOnly           bool   `json:"ImportFavoritesOnly"`
	AllowHWTranscoding            bool   `json:"AllowHWTranscoding"`
	AllowFmp4TranscodingContainer bool   `json:"AllowFmp4TranscodingContainer"`
	AllowStreamSharing            bool   `json:"AllowStreamSharing"`
	FallbackMaxStreamingBitrate   int    `json:"FallbackMaxStreamingBitrate"`
	EnableStreamLooping           bool   `json:"EnableStreamLooping"`
	Source                        string `json:"Source,omitzero"`
	TunerCount                    int    `json:"TunerCount"`
	UserAgent                     string `json:"UserAgent,omitzero"`
	IgnoreDts                     bool   `json:"IgnoreDts"`
	ReadAtNativeFramerate         bool   `json:"ReadAtNativeFramerate"`

	// SkipVodEntries and PlaylistRefreshMinutes are Deadfin-only (D-78).
	// Stock clients do not send them; a save that omits one keeps the
	// stored value (KeepDeadfinFields). Whether they stay tuner fields is
	// an open item on #541.

	// SkipVodEntries drops the movie and series entries of an
	// Xtream-style playlist. Nil or false imports every entry (parity).
	SkipVodEntries *bool `json:"SkipVodEntries,omitzero"`
	// PlaylistRefreshMinutes is the shortest time between two playlist
	// downloads. Nil, or a value under the 15-minute floor, uses the floor.
	PlaylistRefreshMinutes *int `json:"PlaylistRefreshMinutes,omitzero"`
	// StreamOpenIntervalMs is the shortest time between two stream
	// connects to the tuner's source, in milliseconds (D-93). Nil uses
	// 1000; 0 turns the wait off.
	StreamOpenIntervalMs *int `json:"StreamOpenIntervalMs,omitzero"`

	// IncludeGroups and ExcludeGroups filter an M3U tuner's channels by
	// group-title, ignoring case (D-91). A non-empty IncludeGroups keeps
	// only its groups; ExcludeGroups then drops its groups. Nil keeps the
	// stored list on a save; an empty list clears it.
	IncludeGroups []string `json:"IncludeGroups,omitzero"`
	ExcludeGroups []string `json:"ExcludeGroups,omitzero"`
}

// KeepDeadfinFields copies the Deadfin-only members of previous that host
// leaves unset, so a stock client save does not clear them.
func (host *TunerHostInfo) KeepDeadfinFields(previous TunerHostInfo) {
	if host.SkipVodEntries == nil && previous.SkipVodEntries != nil {
		value := *previous.SkipVodEntries
		host.SkipVodEntries = &value
	}
	if host.PlaylistRefreshMinutes == nil && previous.PlaylistRefreshMinutes != nil {
		value := *previous.PlaylistRefreshMinutes
		host.PlaylistRefreshMinutes = &value
	}
	if host.StreamOpenIntervalMs == nil && previous.StreamOpenIntervalMs != nil {
		value := *previous.StreamOpenIntervalMs
		host.StreamOpenIntervalMs = &value
	}
	if host.IncludeGroups == nil && previous.IncludeGroups != nil {
		host.IncludeGroups = append([]string{}, previous.IncludeGroups...)
	}
	if host.ExcludeGroups == nil && previous.ExcludeGroups != nil {
		host.ExcludeGroups = append([]string{}, previous.ExcludeGroups...)
	}
}

func (host TunerHostInfo) clone() TunerHostInfo {
	cloned := host
	cloned.SkipVodEntries, cloned.PlaylistRefreshMinutes, cloned.StreamOpenIntervalMs = nil, nil, nil
	cloned.IncludeGroups, cloned.ExcludeGroups = nil, nil
	cloned.KeepDeadfinFields(host)
	return cloned
}

// keepDeadfinFields applies KeepDeadfinFields to each posted tuner and
// listing provider that matches a stored one by id (ignoring case).
func (options *LiveTvOptions) keepDeadfinFields(stored LiveTvOptions) {
	for index := range options.TunerHosts {
		host := &options.TunerHosts[index]
		for _, previous := range stored.TunerHosts {
			if host.Id != "" && strings.EqualFold(host.Id, previous.Id) {
				host.KeepDeadfinFields(previous)
				break
			}
		}
	}
	for index := range options.ListingProviders {
		provider := &options.ListingProviders[index]
		for _, previous := range stored.ListingProviders {
			if provider.Id != "" && strings.EqualFold(provider.Id, previous.Id) {
				provider.KeepDeadfinFields(previous)
				break
			}
		}
	}
}

// DefaultTunerHostInfo returns the upstream constructor defaults
// (TunerHostInfo.cs:9-17).
func DefaultTunerHostInfo() TunerHostInfo {
	return TunerHostInfo{
		AllowHWTranscoding:          true,
		IgnoreDts:                   true,
		AllowStreamSharing:          true,
		FallbackMaxStreamingBitrate: 30000000,
	}
}

// ListingsProviderInfo is one configured guide source. It matches
// upstream/jellyfin/MediaBrowser.Model/LiveTv/ListingsProviderInfo.cs.
type ListingsProviderInfo struct {
	Id                string          `json:"Id,omitzero"`
	Type              string          `json:"Type,omitzero"`
	Username          string          `json:"Username,omitzero"`
	Password          string          `json:"Password,omitzero"`
	ListingsId        string          `json:"ListingsId,omitzero"`
	ZipCode           string          `json:"ZipCode,omitzero"`
	Country           string          `json:"Country,omitzero"`
	Path              string          `json:"Path,omitzero"`
	EnabledTuners     []string        `json:"EnabledTuners"`
	EnableAllTuners   bool            `json:"EnableAllTuners"`
	NewsCategories    []string        `json:"NewsCategories"`
	SportsCategories  []string        `json:"SportsCategories"`
	KidsCategories    []string        `json:"KidsCategories"`
	MovieCategories   []string        `json:"MovieCategories"`
	ChannelMappings   []NameValuePair `json:"ChannelMappings"`
	MoviePrefix       string          `json:"MoviePrefix,omitzero"`
	PreferredLanguage string          `json:"PreferredLanguage,omitzero"`
	UserAgent         string          `json:"UserAgent,omitzero"`

	// ParseDescriptionEpisodes is Deadfin-only (D-123). Nil or true reads
	// "SxxEyy Episode title" from the first description line and the "ᴺᵉʷ"
	// title marker of a programme that has no <episode-num> or
	// <sub-title>. False takes the text literally (parity). A save that
	// omits it keeps the stored value (KeepDeadfinFields).
	ParseDescriptionEpisodes *bool `json:"ParseDescriptionEpisodes,omitzero"`
}

// DescriptionEpisodes reports whether the guide reads episode facts from
// the programme text (D-123). It is on unless set to false.
func (provider ListingsProviderInfo) DescriptionEpisodes() bool {
	return provider.ParseDescriptionEpisodes == nil || *provider.ParseDescriptionEpisodes
}

// KeepDeadfinFields copies the Deadfin-only members of previous that
// provider leaves unset, so a stock client save does not clear them.
func (provider *ListingsProviderInfo) KeepDeadfinFields(previous ListingsProviderInfo) {
	if provider.ParseDescriptionEpisodes == nil && previous.ParseDescriptionEpisodes != nil {
		value := *previous.ParseDescriptionEpisodes
		provider.ParseDescriptionEpisodes = &value
	}
}

// DefaultListingsProviderInfo returns the upstream constructor defaults
// (ListingsProviderInfo.cs:10-19). GET /LiveTv/ListingProviders/Default
// returns this value.
func DefaultListingsProviderInfo() ListingsProviderInfo {
	return ListingsProviderInfo{
		NewsCategories:   []string{"news", "journalism", "documentary", "current affairs"},
		SportsCategories: []string{"sports", "basketball", "baseball", "football"},
		KidsCategories:   []string{"kids", "family", "children", "childrens", "disney"},
		MovieCategories:  []string{"movie"},
		EnabledTuners:    []string{},
		EnableAllTuners:  true,
		ChannelMappings:  []NameValuePair{},
	}
}

// LiveTv returns an immutable value snapshot of the current Live TV options.
// The array fields are copies.
func (manager *Manager) LiveTv() LiveTvOptions {
	manager.livetv.mu.RLock()
	defer manager.livetv.mu.RUnlock()
	return manager.livetv.value.clone()
}

// UpdateLiveTv serializes a durable mutation of livetv.xml.
func (manager *Manager) UpdateLiveTv(ctx context.Context, update func(*LiveTvOptions) error) error {
	return updateConfiguration(ctx, manager, &manager.livetv, LiveTvFile, KeyLiveTv, func(options *LiveTvOptions) error {
		if err := update(options); err != nil {
			return err
		}
		options.normalizeArrays()
		return nil
	})
}

// Redacted returns a copy for a caller who is not an administrator. Upstream
// returns the stored document to every authenticated user
// (ConfigurationController.cs:77-83). Tuner URLs and guide paths of IPTV
// panels carry the account credentials, so Deadfin masks them, and drops the
// listing provider user name and password (D-69).
func (options LiveTvOptions) Redacted() LiveTvOptions {
	redacted := options.clone()
	for index := range redacted.TunerHosts {
		host := &redacted.TunerHosts[index]
		host.Url = secret.Collect([]string{host.Url}).Redact(host.Url)
	}
	for index := range redacted.ListingProviders {
		provider := &redacted.ListingProviders[index]
		set := secret.Collect([]string{provider.Path}, provider.Username, provider.Password)
		provider.Path = set.Redact(provider.Path)
		provider.Username = ""
		provider.Password = ""
	}
	return redacted
}

func (options LiveTvOptions) clone() LiveTvOptions {
	cloned := options
	if options.GuideDays != nil {
		days := *options.GuideDays
		cloned.GuideDays = &days
	}
	cloned.TunerHosts = make([]TunerHostInfo, len(options.TunerHosts))
	for index, host := range options.TunerHosts {
		cloned.TunerHosts[index] = host.clone()
	}
	cloned.ListingProviders = make([]ListingsProviderInfo, len(options.ListingProviders))
	for index, provider := range options.ListingProviders {
		cloned.ListingProviders[index] = provider.clone()
	}
	cloned.MediaLocationsCreated = append([]string{}, options.MediaLocationsCreated...)
	return cloned
}

func (provider ListingsProviderInfo) clone() ListingsProviderInfo {
	cloned := provider
	cloned.EnabledTuners = append([]string{}, provider.EnabledTuners...)
	cloned.NewsCategories = append([]string{}, provider.NewsCategories...)
	cloned.SportsCategories = append([]string{}, provider.SportsCategories...)
	cloned.KidsCategories = append([]string{}, provider.KidsCategories...)
	cloned.MovieCategories = append([]string{}, provider.MovieCategories...)
	cloned.ChannelMappings = append([]NameValuePair{}, provider.ChannelMappings...)
	cloned.ParseDescriptionEpisodes = nil
	cloned.KeepDeadfinFields(provider)
	return cloned
}

// normalizeArrays keeps the upstream Array.Empty contract: arrays are empty,
// never absent, on the wire.
func (options *LiveTvOptions) normalizeArrays() {
	options.TunerHosts = nonNilSlice(options.TunerHosts)
	options.ListingProviders = nonNilSlice(options.ListingProviders)
	options.MediaLocationsCreated = nonNilStrings(options.MediaLocationsCreated)
	for index := range options.ListingProviders {
		options.ListingProviders[index].normalizeArrays()
	}
}

func (provider *ListingsProviderInfo) normalizeArrays() {
	provider.EnabledTuners = nonNilStrings(provider.EnabledTuners)
	provider.NewsCategories = nonNilStrings(provider.NewsCategories)
	provider.SportsCategories = nonNilStrings(provider.SportsCategories)
	provider.KidsCategories = nonNilStrings(provider.KidsCategories)
	provider.MovieCategories = nonNilStrings(provider.MovieCategories)
	provider.ChannelMappings = nonNilNameValuePairs(provider.ChannelMappings)
}

func nonNilSlice[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

// UnmarshalJSON decodes a posted Live TV configuration over the upstream
// defaults. Each tuner and listing provider element decodes over its own
// constructor defaults, as System.Text.Json constructs each element.
func (options *LiveTvOptions) UnmarshalJSON(data []byte) error {
	type fields LiveTvOptions
	decoded := fields(DefaultLiveTvOptions())
	if err := decodeJSONOverDefaults(data, &decoded); err != nil {
		return err
	}
	*options = LiveTvOptions(decoded)
	options.normalizeArrays()
	return nil
}

// UnmarshalJSON decodes one tuner over the upstream constructor defaults.
func (host *TunerHostInfo) UnmarshalJSON(data []byte) error {
	type fields TunerHostInfo
	decoded := fields(DefaultTunerHostInfo())
	if err := decodeJSONOverDefaults(data, &decoded); err != nil {
		return err
	}
	*host = TunerHostInfo(decoded)
	return nil
}

// UnmarshalJSON decodes one listing provider over the upstream constructor
// defaults. A present array replaces the default array.
func (provider *ListingsProviderInfo) UnmarshalJSON(data []byte) error {
	type fields ListingsProviderInfo
	decoded := fields(DefaultListingsProviderInfo())
	if err := decodeJSONOverDefaults(data, &decoded); err != nil {
		return err
	}
	*provider = ListingsProviderInfo(decoded)
	provider.normalizeArrays()
	return nil
}

// The XML documents mirror XmlSerializer output: arrays are wrapped
// (<TunerHosts><TunerHostInfo>…), null strings are absent elements, and a
// null GuideDays is written as xsi:nil.

type liveTvOptionsXML struct {
	GuideDays                                *nullableIntXML               `xml:"GuideDays"`
	RecordingPath                            *string                       `xml:"RecordingPath"`
	MovieRecordingPath                       *string                       `xml:"MovieRecordingPath"`
	SeriesRecordingPath                      *string                       `xml:"SeriesRecordingPath"`
	EnableRecordingSubfolders                *bool                         `xml:"EnableRecordingSubfolders"`
	EnableOriginalAudioWithEncodedRecordings *bool                         `xml:"EnableOriginalAudioWithEncodedRecordings"`
	TunerHosts                               *tunerHostInfoArrayXML        `xml:"TunerHosts"`
	ListingProviders                         *listingsProviderInfoArrayXML `xml:"ListingProviders"`
	PrePaddingSeconds                        *int                          `xml:"PrePaddingSeconds"`
	PostPaddingSeconds                       *int                          `xml:"PostPaddingSeconds"`
	MediaLocationsCreated                    *stringArrayXML               `xml:"MediaLocationsCreated"`
	RecordingPostProcessor                   *string                       `xml:"RecordingPostProcessor"`
	RecordingPostProcessorArguments          *string                       `xml:"RecordingPostProcessorArguments"`
	SaveRecordingNFO                         *bool                         `xml:"SaveRecordingNFO"`
	SaveRecordingImages                      *bool                         `xml:"SaveRecordingImages"`
}

type tunerHostInfoArrayXML struct {
	Hosts []TunerHostInfo `xml:"TunerHostInfo"`
}

type listingsProviderInfoArrayXML struct {
	Providers []ListingsProviderInfo `xml:"ListingsProviderInfo"`
}

// nullableIntXML reads and writes a C# int? element. XmlSerializer writes a
// null value as <GuideDays xsi:nil="true" />; encoding/xml would read that
// as 0.
type nullableIntXML struct {
	Nil   string `xml:"http://www.w3.org/2001/XMLSchema-instance nil,attr,omitempty"`
	Value string `xml:",chardata"`
}

func (value *nullableIntXML) decode() (*int, error) {
	if value == nil || value.Nil == "true" || strings.TrimSpace(value.Value) == "" {
		return nil, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value.Value))
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func assignString(target *string, value *string) {
	if value != nil {
		*target = *value
	}
}

// MarshalXML writes the document root used by Jellyfin's livetv.xml.
func (options LiveTvOptions) MarshalXML(encoder *xml.Encoder, start xml.StartElement) error {
	start.Name.Local = "LiveTvOptions"
	o := options
	document := liveTvOptionsXML{
		RecordingPath:                            optionalString(o.RecordingPath),
		MovieRecordingPath:                       optionalString(o.MovieRecordingPath),
		SeriesRecordingPath:                      optionalString(o.SeriesRecordingPath),
		EnableRecordingSubfolders:                &o.EnableRecordingSubfolders,
		EnableOriginalAudioWithEncodedRecordings: &o.EnableOriginalAudioWithEncodedRecordings,
		TunerHosts:                               &tunerHostInfoArrayXML{Hosts: o.TunerHosts},
		ListingProviders:                         &listingsProviderInfoArrayXML{Providers: o.ListingProviders},
		PrePaddingSeconds:                        &o.PrePaddingSeconds,
		PostPaddingSeconds:                       &o.PostPaddingSeconds,
		MediaLocationsCreated:                    &stringArrayXML{Values: o.MediaLocationsCreated},
		RecordingPostProcessor:                   optionalString(o.RecordingPostProcessor),
		RecordingPostProcessorArguments:          optionalString(o.RecordingPostProcessorArguments),
		SaveRecordingNFO:                         &o.SaveRecordingNFO,
		SaveRecordingImages:                      &o.SaveRecordingImages,
	}
	if o.GuideDays != nil {
		document.GuideDays = &nullableIntXML{Value: strconv.Itoa(*o.GuideDays)}
	}
	return encoder.EncodeElement(document, start)
}

// UnmarshalXML accepts only the Live TV configuration document root. Fields
// omitted by the source keep the defaults supplied by the caller; a present
// array replaces the default array.
func (options *LiveTvOptions) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	if start.Name.Local != "LiveTvOptions" {
		return fmt.Errorf("config: unexpected live tv configuration root %q", start.Name.Local)
	}
	var document liveTvOptionsXML
	if err := decoder.DecodeElement(&document, &start); err != nil {
		return err
	}
	decoded := options.clone()
	if document.GuideDays != nil {
		days, err := document.GuideDays.decode()
		if err != nil {
			return fmt.Errorf("config: GuideDays: %w", err)
		}
		decoded.GuideDays = days
	}
	assignString(&decoded.RecordingPath, document.RecordingPath)
	assignString(&decoded.MovieRecordingPath, document.MovieRecordingPath)
	assignString(&decoded.SeriesRecordingPath, document.SeriesRecordingPath)
	assignBool(&decoded.EnableRecordingSubfolders, document.EnableRecordingSubfolders)
	assignBool(&decoded.EnableOriginalAudioWithEncodedRecordings, document.EnableOriginalAudioWithEncodedRecordings)
	if document.TunerHosts != nil {
		decoded.TunerHosts = append([]TunerHostInfo{}, document.TunerHosts.Hosts...)
	}
	if document.ListingProviders != nil {
		decoded.ListingProviders = append([]ListingsProviderInfo{}, document.ListingProviders.Providers...)
	}
	assignInt(&decoded.PrePaddingSeconds, document.PrePaddingSeconds)
	assignInt(&decoded.PostPaddingSeconds, document.PostPaddingSeconds)
	decoded.MediaLocationsCreated = replaceStrings(decoded.MediaLocationsCreated, document.MediaLocationsCreated)
	assignString(&decoded.RecordingPostProcessor, document.RecordingPostProcessor)
	assignString(&decoded.RecordingPostProcessorArguments, document.RecordingPostProcessorArguments)
	assignBool(&decoded.SaveRecordingNFO, document.SaveRecordingNFO)
	assignBool(&decoded.SaveRecordingImages, document.SaveRecordingImages)
	decoded.normalizeArrays()
	*options = decoded
	return nil
}

func assignBool(target *bool, value *bool) {
	if value != nil {
		*target = *value
	}
}

func assignInt(target *int, value *int) {
	if value != nil {
		*target = *value
	}
}

type tunerHostInfoXML struct {
	Id                            *string `xml:"Id"`
	Url                           *string `xml:"Url"`
	Type                          *string `xml:"Type"`
	DeviceId                      *string `xml:"DeviceId"`
	FriendlyName                  *string `xml:"FriendlyName"`
	ImportFavoritesOnly           *bool   `xml:"ImportFavoritesOnly"`
	AllowHWTranscoding            *bool   `xml:"AllowHWTranscoding"`
	AllowFmp4TranscodingContainer *bool   `xml:"AllowFmp4TranscodingContainer"`
	AllowStreamSharing            *bool   `xml:"AllowStreamSharing"`
	FallbackMaxStreamingBitrate   *int    `xml:"FallbackMaxStreamingBitrate"`
	EnableStreamLooping           *bool   `xml:"EnableStreamLooping"`
	Source                        *string `xml:"Source"`
	TunerCount                    *int    `xml:"TunerCount"`
	UserAgent                     *string `xml:"UserAgent"`
	IgnoreDts                     *bool   `xml:"IgnoreDts"`
	ReadAtNativeFramerate         *bool   `xml:"ReadAtNativeFramerate"`
	SkipVodEntries                *bool   `xml:"SkipVodEntries,omitempty"`
	PlaylistRefreshMinutes        *int    `xml:"PlaylistRefreshMinutes,omitempty"`
	StreamOpenIntervalMs          *int    `xml:"StreamOpenIntervalMs,omitempty"`
	// A nil group list writes no element; an empty list writes an empty
	// element, so the reload keeps the difference.
	IncludeGroups *stringArrayXML `xml:"IncludeGroups,omitempty"`
	ExcludeGroups *stringArrayXML `xml:"ExcludeGroups,omitempty"`
}

// MarshalXML writes one <TunerHostInfo> element.
func (host TunerHostInfo) MarshalXML(encoder *xml.Encoder, start xml.StartElement) error {
	start.Name.Local = "TunerHostInfo"
	h := host
	return encoder.EncodeElement(tunerHostInfoXML{
		Id:                            optionalString(h.Id),
		Url:                           optionalString(h.Url),
		Type:                          optionalString(h.Type),
		DeviceId:                      optionalString(h.DeviceId),
		FriendlyName:                  optionalString(h.FriendlyName),
		ImportFavoritesOnly:           &h.ImportFavoritesOnly,
		AllowHWTranscoding:            &h.AllowHWTranscoding,
		AllowFmp4TranscodingContainer: &h.AllowFmp4TranscodingContainer,
		AllowStreamSharing:            &h.AllowStreamSharing,
		FallbackMaxStreamingBitrate:   &h.FallbackMaxStreamingBitrate,
		EnableStreamLooping:           &h.EnableStreamLooping,
		Source:                        optionalString(h.Source),
		TunerCount:                    &h.TunerCount,
		UserAgent:                     optionalString(h.UserAgent),
		IgnoreDts:                     &h.IgnoreDts,
		ReadAtNativeFramerate:         &h.ReadAtNativeFramerate,
		SkipVodEntries:                h.SkipVodEntries,
		PlaylistRefreshMinutes:        h.PlaylistRefreshMinutes,
		StreamOpenIntervalMs:          h.StreamOpenIntervalMs,
		IncludeGroups:                 groupsXML(h.IncludeGroups),
		ExcludeGroups:                 groupsXML(h.ExcludeGroups),
	}, start)
}

// UnmarshalXML reads one <TunerHostInfo> element over the upstream
// constructor defaults, as XmlSerializer constructs each element.
func (host *TunerHostInfo) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var document tunerHostInfoXML
	if err := decoder.DecodeElement(&document, &start); err != nil {
		return err
	}
	decoded := DefaultTunerHostInfo()
	assignString(&decoded.Id, document.Id)
	assignString(&decoded.Url, document.Url)
	assignString(&decoded.Type, document.Type)
	assignString(&decoded.DeviceId, document.DeviceId)
	assignString(&decoded.FriendlyName, document.FriendlyName)
	assignBool(&decoded.ImportFavoritesOnly, document.ImportFavoritesOnly)
	assignBool(&decoded.AllowHWTranscoding, document.AllowHWTranscoding)
	assignBool(&decoded.AllowFmp4TranscodingContainer, document.AllowFmp4TranscodingContainer)
	assignBool(&decoded.AllowStreamSharing, document.AllowStreamSharing)
	assignInt(&decoded.FallbackMaxStreamingBitrate, document.FallbackMaxStreamingBitrate)
	assignBool(&decoded.EnableStreamLooping, document.EnableStreamLooping)
	assignString(&decoded.Source, document.Source)
	assignInt(&decoded.TunerCount, document.TunerCount)
	assignString(&decoded.UserAgent, document.UserAgent)
	assignBool(&decoded.IgnoreDts, document.IgnoreDts)
	assignBool(&decoded.ReadAtNativeFramerate, document.ReadAtNativeFramerate)
	decoded.SkipVodEntries = document.SkipVodEntries
	decoded.PlaylistRefreshMinutes = document.PlaylistRefreshMinutes
	decoded.StreamOpenIntervalMs = document.StreamOpenIntervalMs
	decoded.IncludeGroups = groupsFromXML(document.IncludeGroups)
	decoded.ExcludeGroups = groupsFromXML(document.ExcludeGroups)
	*host = decoded
	return nil
}

func groupsXML(groups []string) *stringArrayXML {
	if groups == nil {
		return nil
	}
	return &stringArrayXML{Values: groups}
}

func groupsFromXML(document *stringArrayXML) []string {
	if document == nil {
		return nil
	}
	return append([]string{}, document.Values...)
}

type listingsProviderInfoXML struct {
	Id                *string                `xml:"Id"`
	Type              *string                `xml:"Type"`
	Username          *string                `xml:"Username"`
	Password          *string                `xml:"Password"`
	ListingsId        *string                `xml:"ListingsId"`
	ZipCode           *string                `xml:"ZipCode"`
	Country           *string                `xml:"Country"`
	Path              *string                `xml:"Path"`
	EnabledTuners     *stringArrayXML        `xml:"EnabledTuners"`
	EnableAllTuners   *bool                  `xml:"EnableAllTuners"`
	NewsCategories    *stringArrayXML        `xml:"NewsCategories"`
	SportsCategories  *stringArrayXML        `xml:"SportsCategories"`
	KidsCategories    *stringArrayXML        `xml:"KidsCategories"`
	MovieCategories   *stringArrayXML        `xml:"MovieCategories"`
	ChannelMappings   *nameValuePairArrayXML `xml:"ChannelMappings"`
	MoviePrefix       *string                `xml:"MoviePrefix"`
	PreferredLanguage *string                `xml:"PreferredLanguage"`
	UserAgent         *string                `xml:"UserAgent"`

	ParseDescriptionEpisodes *bool `xml:"ParseDescriptionEpisodes,omitempty"`
}

// MarshalXML writes one <ListingsProviderInfo> element.
func (provider ListingsProviderInfo) MarshalXML(encoder *xml.Encoder, start xml.StartElement) error {
	start.Name.Local = "ListingsProviderInfo"
	p := provider
	return encoder.EncodeElement(listingsProviderInfoXML{
		Id:                optionalString(p.Id),
		Type:              optionalString(p.Type),
		Username:          optionalString(p.Username),
		Password:          optionalString(p.Password),
		ListingsId:        optionalString(p.ListingsId),
		ZipCode:           optionalString(p.ZipCode),
		Country:           optionalString(p.Country),
		Path:              optionalString(p.Path),
		EnabledTuners:     &stringArrayXML{Values: p.EnabledTuners},
		EnableAllTuners:   &p.EnableAllTuners,
		NewsCategories:    &stringArrayXML{Values: p.NewsCategories},
		SportsCategories:  &stringArrayXML{Values: p.SportsCategories},
		KidsCategories:    &stringArrayXML{Values: p.KidsCategories},
		MovieCategories:   &stringArrayXML{Values: p.MovieCategories},
		ChannelMappings:   &nameValuePairArrayXML{Entries: p.ChannelMappings},
		MoviePrefix:       optionalString(p.MoviePrefix),
		PreferredLanguage: optionalString(p.PreferredLanguage),
		UserAgent:         optionalString(p.UserAgent),

		ParseDescriptionEpisodes: p.ParseDescriptionEpisodes,
	}, start)
}

// UnmarshalXML reads one <ListingsProviderInfo> element over the upstream
// constructor defaults. A present array replaces the default array.
func (provider *ListingsProviderInfo) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var document listingsProviderInfoXML
	if err := decoder.DecodeElement(&document, &start); err != nil {
		return err
	}
	decoded := DefaultListingsProviderInfo()
	assignString(&decoded.Id, document.Id)
	assignString(&decoded.Type, document.Type)
	assignString(&decoded.Username, document.Username)
	assignString(&decoded.Password, document.Password)
	assignString(&decoded.ListingsId, document.ListingsId)
	assignString(&decoded.ZipCode, document.ZipCode)
	assignString(&decoded.Country, document.Country)
	assignString(&decoded.Path, document.Path)
	decoded.EnabledTuners = replaceStrings(decoded.EnabledTuners, document.EnabledTuners)
	assignBool(&decoded.EnableAllTuners, document.EnableAllTuners)
	decoded.NewsCategories = replaceStrings(decoded.NewsCategories, document.NewsCategories)
	decoded.SportsCategories = replaceStrings(decoded.SportsCategories, document.SportsCategories)
	decoded.KidsCategories = replaceStrings(decoded.KidsCategories, document.KidsCategories)
	decoded.MovieCategories = replaceStrings(decoded.MovieCategories, document.MovieCategories)
	if document.ChannelMappings != nil {
		decoded.ChannelMappings = append([]NameValuePair{}, document.ChannelMappings.Entries...)
	}
	assignString(&decoded.MoviePrefix, document.MoviePrefix)
	assignString(&decoded.PreferredLanguage, document.PreferredLanguage)
	assignString(&decoded.UserAgent, document.UserAgent)
	decoded.ParseDescriptionEpisodes = document.ParseDescriptionEpisodes
	decoded.normalizeArrays()
	*provider = decoded
	return nil
}
