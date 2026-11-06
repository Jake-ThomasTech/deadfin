// SPDX-License-Identifier: GPL-2.0-only

package config

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/deadfin-org/deadfin/internal/events"
)

const systemFile = "system.xml"

// CastReceiverApplication identifies a Google Cast receiver application.
type CastReceiverApplication struct {
	ID   string `xml:"Id" json:"Id"`
	Name string `xml:"Name" json:"Name"`
}

// ServerConfiguration is the persisted server-wide Jellyfin compatibility
// configuration managed by this package. It mirrors the full public surface of
// upstream/jellyfin/MediaBrowser.Model/Configuration/BaseApplicationConfiguration.cs
// and ServerConfiguration.cs. Fields inherit from BaseApplicationConfiguration
// (LogFileRetentionDays, IsStartupWizardCompleted, CachePath,
// PreviousVersionStr) and continue with every server-specific field. The struct
// serves as both the persisted XML model and the JSON wire model; JSON tags
// follow the DTO rules (value types always emit, nullable/reference
// values use omitzero, nulls are omitted).
type ServerConfiguration struct {
	// Inherited from BaseApplicationConfiguration.cs:25-61.
	LogFileRetentionDays     int     `xml:"LogFileRetentionDays" json:"LogFileRetentionDays"`
	IsStartupWizardCompleted bool    `xml:"IsStartupWizardCompleted" json:"IsStartupWizardCompleted"`
	CachePath                *string `xml:"CachePath" json:"CachePath,omitzero"`
	PreviousVersionStr       *string `xml:"PreviousVersionStr" json:"PreviousVersionStr,omitzero"`

	// ServerConfiguration.cs:70-291.
	EnableMetrics                       bool                      `xml:"EnableMetrics" json:"EnableMetrics"`
	EnableNormalizedItemByNameIds       bool                      `xml:"EnableNormalizedItemByNameIds" json:"EnableNormalizedItemByNameIds"`
	IsPortAuthorized                    bool                      `xml:"IsPortAuthorized" json:"IsPortAuthorized"`
	QuickConnectAvailable               bool                      `xml:"QuickConnectAvailable" json:"QuickConnectAvailable"`
	EnableCaseSensitiveItemIds          bool                      `xml:"EnableCaseSensitiveItemIds" json:"EnableCaseSensitiveItemIds"`
	DisableLiveTvChannelUserDataName    bool                      `xml:"DisableLiveTvChannelUserDataName" json:"DisableLiveTvChannelUserDataName"`
	MetadataPath                        string                    `xml:"MetadataPath" json:"MetadataPath"`
	PreferredMetadataLanguage           string                    `xml:"PreferredMetadataLanguage" json:"PreferredMetadataLanguage"`
	MetadataCountryCode                 string                    `xml:"MetadataCountryCode" json:"MetadataCountryCode"`
	SortReplaceCharacters               []string                  `xml:"SortReplaceCharacters>string" json:"SortReplaceCharacters"`
	SortRemoveCharacters                []string                  `xml:"SortRemoveCharacters>string" json:"SortRemoveCharacters"`
	SortRemoveWords                     []string                  `xml:"SortRemoveWords>string" json:"SortRemoveWords"`
	MinResumePct                        int                       `xml:"MinResumePct" json:"MinResumePct"`
	MaxResumePct                        int                       `xml:"MaxResumePct" json:"MaxResumePct"`
	MinResumeDurationSeconds            int                       `xml:"MinResumeDurationSeconds" json:"MinResumeDurationSeconds"`
	MinAudiobookResume                  int                       `xml:"MinAudiobookResume" json:"MinAudiobookResume"`
	MaxAudiobookResume                  int                       `xml:"MaxAudiobookResume" json:"MaxAudiobookResume"`
	InactiveSessionThreshold            int                       `xml:"InactiveSessionThreshold" json:"InactiveSessionThreshold"`
	LibraryMonitorDelay                 int                       `xml:"LibraryMonitorDelay" json:"LibraryMonitorDelay"`
	LibraryUpdateDuration               int                       `xml:"LibraryUpdateDuration" json:"LibraryUpdateDuration"`
	CacheSize                           int                       `xml:"CacheSize" json:"CacheSize"`
	ImageSavingConvention               ImageSavingConvention     `xml:"ImageSavingConvention" json:"ImageSavingConvention"`
	MetadataOptions                     []MetadataOptions         `xml:"MetadataOptions>MetadataOptions" json:"MetadataOptions"`
	SkipDeserializationForBasicTypes    bool                      `xml:"SkipDeserializationForBasicTypes" json:"SkipDeserializationForBasicTypes"`
	ServerName                          string                    `xml:"ServerName" json:"ServerName"`
	UICulture                           string                    `xml:"UICulture" json:"UICulture"`
	SaveMetadataHidden                  bool                      `xml:"SaveMetadataHidden" json:"SaveMetadataHidden"`
	ContentTypes                        []NameValuePair           `xml:"ContentTypes>NameValuePair" json:"ContentTypes"`
	RemoteClientBitrateLimit            int                       `xml:"RemoteClientBitrateLimit" json:"RemoteClientBitrateLimit"`
	EnableFolderView                    bool                      `xml:"EnableFolderView" json:"EnableFolderView"`
	EnableGroupingMoviesIntoCollections bool                      `xml:"EnableGroupingMoviesIntoCollections" json:"EnableGroupingMoviesIntoCollections"`
	EnableGroupingShowsIntoCollections  bool                      `xml:"EnableGroupingShowsIntoCollections" json:"EnableGroupingShowsIntoCollections"`
	DisplaySpecialsWithinSeasons        bool                      `xml:"DisplaySpecialsWithinSeasons" json:"DisplaySpecialsWithinSeasons"`
	CodecsUsed                          []string                  `xml:"CodecsUsed>string" json:"CodecsUsed"`
	PluginRepositories                  []RepositoryInfo          `xml:"PluginRepositories>RepositoryInfo" json:"PluginRepositories"`
	EnableExternalContentInSuggestions  bool                      `xml:"EnableExternalContentInSuggestions" json:"EnableExternalContentInSuggestions"`
	ImageExtractionTimeoutMs            int                       `xml:"ImageExtractionTimeoutMs" json:"ImageExtractionTimeoutMs"`
	PathSubstitutions                   []PathSubstitution        `xml:"PathSubstitutions>PathSubstitution" json:"PathSubstitutions"`
	EnableSlowResponseWarning           bool                      `xml:"EnableSlowResponseWarning" json:"EnableSlowResponseWarning"`
	SlowResponseThresholdMs             int64                     `xml:"SlowResponseThresholdMs" json:"SlowResponseThresholdMs"`
	CorsHosts                           []string                  `xml:"CorsHosts>string" json:"CorsHosts"`
	ActivityLogRetentionDays            *int                      `xml:"ActivityLogRetentionDays" json:"ActivityLogRetentionDays,omitzero"`
	LibraryScanFanoutConcurrency        int                       `xml:"LibraryScanFanoutConcurrency" json:"LibraryScanFanoutConcurrency"`
	LibraryMetadataRefreshConcurrency   int                       `xml:"LibraryMetadataRefreshConcurrency" json:"LibraryMetadataRefreshConcurrency"`
	AllowClientLogUpload                bool                      `xml:"AllowClientLogUpload" json:"AllowClientLogUpload"`
	DummyChapterDuration                int                       `xml:"DummyChapterDuration" json:"DummyChapterDuration"`
	ChapterImageResolution              ImageResolution           `xml:"ChapterImageResolution" json:"ChapterImageResolution"`
	ParallelImageEncodingLimit          int                       `xml:"ParallelImageEncodingLimit" json:"ParallelImageEncodingLimit"`
	CastReceiverApplications            []CastReceiverApplication `xml:"CastReceiverApplications>CastReceiverApplication" json:"CastReceiverApplications"`
	TrickplayOptions                    TrickplayOptions          `xml:"TrickplayOptions" json:"TrickplayOptions"`
	EnableLegacyAuthorization           bool                      `xml:"EnableLegacyAuthorization" json:"EnableLegacyAuthorization"`
}

type serverConfigurationXML struct {
	// BaseApplicationConfiguration.
	LogFileRetentionDays     *int    `xml:"LogFileRetentionDays"`
	IsStartupWizardCompleted *bool   `xml:"IsStartupWizardCompleted"`
	CachePath                *string `xml:"CachePath"`
	PreviousVersionStr       *string `xml:"PreviousVersionStr"`

	// ServerConfiguration.
	EnableMetrics                       *bool                        `xml:"EnableMetrics"`
	EnableNormalizedItemByNameIds       *bool                        `xml:"EnableNormalizedItemByNameIds"`
	IsPortAuthorized                    *bool                        `xml:"IsPortAuthorized"`
	QuickConnectAvailable               *bool                        `xml:"QuickConnectAvailable"`
	EnableCaseSensitiveItemIds          *bool                        `xml:"EnableCaseSensitiveItemIds"`
	DisableLiveTvChannelUserDataName    *bool                        `xml:"DisableLiveTvChannelUserDataName"`
	MetadataPath                        *string                      `xml:"MetadataPath"`
	PreferredMetadataLanguage           *string                      `xml:"PreferredMetadataLanguage"`
	MetadataCountryCode                 *string                      `xml:"MetadataCountryCode"`
	SortReplaceCharacters               *stringArrayXML              `xml:"SortReplaceCharacters"`
	SortRemoveCharacters                *stringArrayXML              `xml:"SortRemoveCharacters"`
	SortRemoveWords                     *stringArrayXML              `xml:"SortRemoveWords"`
	MinResumePct                        *int                         `xml:"MinResumePct"`
	MaxResumePct                        *int                         `xml:"MaxResumePct"`
	MinResumeDurationSeconds            *int                         `xml:"MinResumeDurationSeconds"`
	MinAudiobookResume                  *int                         `xml:"MinAudiobookResume"`
	MaxAudiobookResume                  *int                         `xml:"MaxAudiobookResume"`
	InactiveSessionThreshold            *int                         `xml:"InactiveSessionThreshold"`
	LibraryMonitorDelay                 *int                         `xml:"LibraryMonitorDelay"`
	LibraryUpdateDuration               *int                         `xml:"LibraryUpdateDuration"`
	CacheSize                           *int                         `xml:"CacheSize"`
	ImageSavingConvention               *ImageSavingConvention       `xml:"ImageSavingConvention"`
	MetadataOptions                     *metadataOptionsArrayXML     `xml:"MetadataOptions"`
	SkipDeserializationForBasicTypes    *bool                        `xml:"SkipDeserializationForBasicTypes"`
	ServerName                          *string                      `xml:"ServerName"`
	UICulture                           *string                      `xml:"UICulture"`
	SaveMetadataHidden                  *bool                        `xml:"SaveMetadataHidden"`
	ContentTypes                        *nameValuePairArrayXML       `xml:"ContentTypes"`
	RemoteClientBitrateLimit            *int                         `xml:"RemoteClientBitrateLimit"`
	EnableFolderView                    *bool                        `xml:"EnableFolderView"`
	EnableGroupingMoviesIntoCollections *bool                        `xml:"EnableGroupingMoviesIntoCollections"`
	EnableGroupingShowsIntoCollections  *bool                        `xml:"EnableGroupingShowsIntoCollections"`
	DisplaySpecialsWithinSeasons        *bool                        `xml:"DisplaySpecialsWithinSeasons"`
	CodecsUsed                          *stringArrayXML              `xml:"CodecsUsed"`
	PluginRepositories                  *repositoryInfoArrayXML      `xml:"PluginRepositories"`
	EnableExternalContentInSuggestions  *bool                        `xml:"EnableExternalContentInSuggestions"`
	ImageExtractionTimeoutMs            *int                         `xml:"ImageExtractionTimeoutMs"`
	PathSubstitutions                   *pathSubstitutionArrayXML    `xml:"PathSubstitutions"`
	EnableSlowResponseWarning           *bool                        `xml:"EnableSlowResponseWarning"`
	SlowResponseThresholdMs             *int64                       `xml:"SlowResponseThresholdMs"`
	CorsHosts                           *stringArrayXML              `xml:"CorsHosts"`
	ActivityLogRetentionDays            *int                         `xml:"ActivityLogRetentionDays"`
	LibraryScanFanoutConcurrency        *int                         `xml:"LibraryScanFanoutConcurrency"`
	LibraryMetadataRefreshConcurrency   *int                         `xml:"LibraryMetadataRefreshConcurrency"`
	AllowClientLogUpload                *bool                        `xml:"AllowClientLogUpload"`
	DummyChapterDuration                *int                         `xml:"DummyChapterDuration"`
	ChapterImageResolution              *ImageResolution             `xml:"ChapterImageResolution"`
	ParallelImageEncodingLimit          *int                         `xml:"ParallelImageEncodingLimit"`
	CastReceiverApplications            *castReceiverApplicationsXML `xml:"CastReceiverApplications"`
	TrickplayOptions                    *TrickplayOptions            `xml:"TrickplayOptions"`
	EnableLegacyAuthorization           *bool                        `xml:"EnableLegacyAuthorization"`
}

type castReceiverApplicationsXML struct {
	Applications []CastReceiverApplication `xml:"CastReceiverApplication"`
}

type stringArrayXML struct {
	Values []string `xml:"string"`
}

type metadataOptionsArrayXML struct {
	Options []MetadataOptions `xml:"MetadataOptions"`
}

type nameValuePairArrayXML struct {
	Entries []NameValuePair `xml:"NameValuePair"`
}

type repositoryInfoArrayXML struct {
	Repositories []RepositoryInfo `xml:"RepositoryInfo"`
}

type pathSubstitutionArrayXML struct {
	Substitutions []PathSubstitution `xml:"PathSubstitution"`
}

// BrandingOptions is the persisted Jellyfin branding configuration.
type BrandingOptions struct {
	LoginDisclaimer      string
	CustomCss            string
	SplashscreenEnabled  bool
	SplashscreenLocation string
}

// DefaultBrandingOptions returns Jellyfin's empty branding configuration.
func DefaultBrandingOptions() BrandingOptions {
	return BrandingOptions{}
}

// MarshalXML writes the document root used by Jellyfin's branding.xml.
func (options BrandingOptions) MarshalXML(encoder *xml.Encoder, start xml.StartElement) error {
	start.Name.Local = "BrandingOptions"
	type fields BrandingOptions
	return encoder.EncodeElement(fields(options), start)
}

// UnmarshalXML accepts only the branding configuration document root.
func (options *BrandingOptions) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	if start.Name.Local != "BrandingOptions" {
		return fmt.Errorf("config: unexpected branding configuration root %q", start.Name.Local)
	}
	type fields BrandingOptions
	return decoder.DecodeElement((*fields)(options), &start)
}

// DefaultServerConfiguration returns the upstream-compatible values used for a
// new server configuration. Every default matches the upstream constructor
// (BaseApplicationConfiguration.cs:16-19, ServerConfiguration.cs:20-65) and
// field initializers (ServerConfiguration.cs:70-291).
func DefaultServerConfiguration() ServerConfiguration {
	cacheSize := runtime.NumCPU() * 100
	activityLogRetention := 30
	return ServerConfiguration{
		LogFileRetentionDays:               3,
		EnableNormalizedItemByNameIds:      true,
		QuickConnectAvailable:              true,
		EnableCaseSensitiveItemIds:         true,
		DisableLiveTvChannelUserDataName:   true,
		PreferredMetadataLanguage:          "en",
		MetadataCountryCode:                "US",
		SortReplaceCharacters:              []string{".", "+", "%"},
		SortRemoveCharacters:               []string{",", "&", "-", "{", "}", "'"},
		SortRemoveWords:                    []string{"the", "a", "an"},
		MinResumePct:                       5,
		MaxResumePct:                       90,
		MinResumeDurationSeconds:           300,
		MinAudiobookResume:                 5,
		MaxAudiobookResume:                 5,
		LibraryMonitorDelay:                60,
		LibraryUpdateDuration:              30,
		CacheSize:                          cacheSize,
		ImageSavingConvention:              ImageSavingConventionLegacy,
		MetadataOptions:                    defaultMetadataOptions(),
		SkipDeserializationForBasicTypes:   true,
		UICulture:                          "en-US",
		DisplaySpecialsWithinSeasons:       true,
		EnableExternalContentInSuggestions: true,
		EnableSlowResponseWarning:          true,
		SlowResponseThresholdMs:            500,
		CorsHosts:                          []string{"*"},
		ActivityLogRetentionDays:           &activityLogRetention,
		AllowClientLogUpload:               true,
		ChapterImageResolution:             ImageResolutionMatchSource,
		CastReceiverApplications: []CastReceiverApplication{
			{ID: "F007D354", Name: "Stable"},
			{ID: "6F511C87", Name: "Unstable"},
		},
		ContentTypes:       []NameValuePair{},
		CodecsUsed:         []string{},
		PluginRepositories: []RepositoryInfo{},
		PathSubstitutions:  []PathSubstitution{},
		TrickplayOptions:   DefaultTrickplayOptions(),
	}
}

// defaultMetadataOptions returns the per-type metadata options initialized in
// the upstream ServerConfiguration constructor (ServerConfiguration.cs:22-64).
func defaultMetadataOptions() []MetadataOptions {
	return []MetadataOptions{
		{ItemType: "Book"},
		{ItemType: "Movie"},
		{ItemType: "MusicVideo", DisabledMetadataFetchers: []string{"The Open Movie Database"}, DisabledImageFetchers: []string{"The Open Movie Database"}},
		{ItemType: "Series"},
		{ItemType: "MusicAlbum", DisabledMetadataFetchers: []string{"TheAudioDB"}},
		{ItemType: "MusicArtist", DisabledMetadataFetchers: []string{"TheAudioDB"}},
		{ItemType: "BoxSet"},
		{ItemType: "Season"},
		{ItemType: "Episode"},
	}
}

// MarshalXML writes the document root used by Jellyfin's system.xml. Every
// array member writes its wrapper element, empty or not, as XmlSerializer
// does, so an array that a client clears does not revert to its default on
// the next load.
func (configuration ServerConfiguration) MarshalXML(encoder *xml.Encoder, start xml.StartElement) error {
	start.Name.Local = "ServerConfiguration"
	c := configuration
	document := serverConfigurationXML{
		LogFileRetentionDays:                &c.LogFileRetentionDays,
		IsStartupWizardCompleted:            &c.IsStartupWizardCompleted,
		CachePath:                           c.CachePath,
		PreviousVersionStr:                  c.PreviousVersionStr,
		EnableMetrics:                       &c.EnableMetrics,
		EnableNormalizedItemByNameIds:       &c.EnableNormalizedItemByNameIds,
		IsPortAuthorized:                    &c.IsPortAuthorized,
		QuickConnectAvailable:               &c.QuickConnectAvailable,
		EnableCaseSensitiveItemIds:          &c.EnableCaseSensitiveItemIds,
		DisableLiveTvChannelUserDataName:    &c.DisableLiveTvChannelUserDataName,
		MetadataPath:                        &c.MetadataPath,
		PreferredMetadataLanguage:           &c.PreferredMetadataLanguage,
		MetadataCountryCode:                 &c.MetadataCountryCode,
		SortReplaceCharacters:               &stringArrayXML{Values: c.SortReplaceCharacters},
		SortRemoveCharacters:                &stringArrayXML{Values: c.SortRemoveCharacters},
		SortRemoveWords:                     &stringArrayXML{Values: c.SortRemoveWords},
		MinResumePct:                        &c.MinResumePct,
		MaxResumePct:                        &c.MaxResumePct,
		MinResumeDurationSeconds:            &c.MinResumeDurationSeconds,
		MinAudiobookResume:                  &c.MinAudiobookResume,
		MaxAudiobookResume:                  &c.MaxAudiobookResume,
		InactiveSessionThreshold:            &c.InactiveSessionThreshold,
		LibraryMonitorDelay:                 &c.LibraryMonitorDelay,
		LibraryUpdateDuration:               &c.LibraryUpdateDuration,
		CacheSize:                           &c.CacheSize,
		ImageSavingConvention:               &c.ImageSavingConvention,
		MetadataOptions:                     &metadataOptionsArrayXML{Options: c.MetadataOptions},
		SkipDeserializationForBasicTypes:    &c.SkipDeserializationForBasicTypes,
		ServerName:                          &c.ServerName,
		UICulture:                           &c.UICulture,
		SaveMetadataHidden:                  &c.SaveMetadataHidden,
		ContentTypes:                        &nameValuePairArrayXML{Entries: c.ContentTypes},
		RemoteClientBitrateLimit:            &c.RemoteClientBitrateLimit,
		EnableFolderView:                    &c.EnableFolderView,
		EnableGroupingMoviesIntoCollections: &c.EnableGroupingMoviesIntoCollections,
		EnableGroupingShowsIntoCollections:  &c.EnableGroupingShowsIntoCollections,
		DisplaySpecialsWithinSeasons:        &c.DisplaySpecialsWithinSeasons,
		CodecsUsed:                          &stringArrayXML{Values: c.CodecsUsed},
		PluginRepositories:                  &repositoryInfoArrayXML{Repositories: c.PluginRepositories},
		EnableExternalContentInSuggestions:  &c.EnableExternalContentInSuggestions,
		ImageExtractionTimeoutMs:            &c.ImageExtractionTimeoutMs,
		PathSubstitutions:                   &pathSubstitutionArrayXML{Substitutions: c.PathSubstitutions},
		EnableSlowResponseWarning:           &c.EnableSlowResponseWarning,
		SlowResponseThresholdMs:             &c.SlowResponseThresholdMs,
		CorsHosts:                           &stringArrayXML{Values: c.CorsHosts},
		ActivityLogRetentionDays:            c.ActivityLogRetentionDays,
		LibraryScanFanoutConcurrency:        &c.LibraryScanFanoutConcurrency,
		LibraryMetadataRefreshConcurrency:   &c.LibraryMetadataRefreshConcurrency,
		AllowClientLogUpload:                &c.AllowClientLogUpload,
		DummyChapterDuration:                &c.DummyChapterDuration,
		ChapterImageResolution:              &c.ChapterImageResolution,
		ParallelImageEncodingLimit:          &c.ParallelImageEncodingLimit,
		CastReceiverApplications:            &castReceiverApplicationsXML{Applications: c.CastReceiverApplications},
		TrickplayOptions:                    &c.TrickplayOptions,
		EnableLegacyAuthorization:           &c.EnableLegacyAuthorization,
	}
	return encoder.EncodeElement(document, start)
}

// UnmarshalJSON decodes a posted server configuration over the upstream
// constructor defaults (ServerConfiguration.cs:20-291). A member that the
// client omits keeps its upstream default and never becomes a Go zero value.
func (configuration *ServerConfiguration) UnmarshalJSON(data []byte) error {
	type fields ServerConfiguration
	decoded := fields(DefaultServerConfiguration())
	if err := decodeJSONOverDefaults(data, &decoded); err != nil {
		return err
	}
	*configuration = ServerConfiguration(decoded)
	configuration.normalizeArrays()
	return nil
}

// normalizeArrays replaces nil top-level arrays with empty ones, matching the
// upstream array initializers and the GET wire shape.
func (configuration *ServerConfiguration) normalizeArrays() {
	configuration.SortReplaceCharacters = nonNilStrings(configuration.SortReplaceCharacters)
	configuration.SortRemoveCharacters = nonNilStrings(configuration.SortRemoveCharacters)
	configuration.SortRemoveWords = nonNilStrings(configuration.SortRemoveWords)
	configuration.MetadataOptions = nonNilMetadataOptions(configuration.MetadataOptions)
	configuration.ContentTypes = nonNilNameValuePairs(configuration.ContentTypes)
	configuration.CodecsUsed = nonNilStrings(configuration.CodecsUsed)
	configuration.PluginRepositories = nonNilRepositories(configuration.PluginRepositories)
	configuration.PathSubstitutions = nonNilPathSubstitutions(configuration.PathSubstitutions)
	configuration.CorsHosts = nonNilStrings(configuration.CorsHosts)
	configuration.CastReceiverApplications = nonNilCastReceivers(configuration.CastReceiverApplications)
}

// UnmarshalXML accepts only the system configuration document root. Fields
// omitted by an older source retain the defaults supplied by the caller.
func (configuration *ServerConfiguration) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	if start.Name.Local != "ServerConfiguration" {
		return fmt.Errorf("config: unexpected system configuration root %q", start.Name.Local)
	}

	var decoded serverConfigurationXML
	if err := decoder.DecodeElement(&decoded, &start); err != nil {
		return err
	}
	if decoded.LogFileRetentionDays != nil {
		configuration.LogFileRetentionDays = *decoded.LogFileRetentionDays
	}
	if decoded.IsStartupWizardCompleted != nil {
		configuration.IsStartupWizardCompleted = *decoded.IsStartupWizardCompleted
	}
	if decoded.CachePath != nil {
		configuration.CachePath = decoded.CachePath
	}
	if decoded.PreviousVersionStr != nil {
		configuration.PreviousVersionStr = decoded.PreviousVersionStr
	}
	if decoded.EnableMetrics != nil {
		configuration.EnableMetrics = *decoded.EnableMetrics
	}
	if decoded.EnableNormalizedItemByNameIds != nil {
		configuration.EnableNormalizedItemByNameIds = *decoded.EnableNormalizedItemByNameIds
	}
	if decoded.IsPortAuthorized != nil {
		configuration.IsPortAuthorized = *decoded.IsPortAuthorized
	}
	if decoded.QuickConnectAvailable != nil {
		configuration.QuickConnectAvailable = *decoded.QuickConnectAvailable
	}
	if decoded.EnableCaseSensitiveItemIds != nil {
		configuration.EnableCaseSensitiveItemIds = *decoded.EnableCaseSensitiveItemIds
	}
	if decoded.DisableLiveTvChannelUserDataName != nil {
		configuration.DisableLiveTvChannelUserDataName = *decoded.DisableLiveTvChannelUserDataName
	}
	if decoded.MetadataPath != nil {
		configuration.MetadataPath = *decoded.MetadataPath
	}
	if decoded.PreferredMetadataLanguage != nil {
		configuration.PreferredMetadataLanguage = *decoded.PreferredMetadataLanguage
	}
	if decoded.MetadataCountryCode != nil {
		configuration.MetadataCountryCode = *decoded.MetadataCountryCode
	}
	if decoded.SortReplaceCharacters != nil {
		configuration.SortReplaceCharacters = nonNilStrings(decoded.SortReplaceCharacters.Values)
	}
	if decoded.SortRemoveCharacters != nil {
		configuration.SortRemoveCharacters = nonNilStrings(decoded.SortRemoveCharacters.Values)
	}
	if decoded.SortRemoveWords != nil {
		configuration.SortRemoveWords = nonNilStrings(decoded.SortRemoveWords.Values)
	}
	if decoded.MinResumePct != nil {
		configuration.MinResumePct = *decoded.MinResumePct
	}
	if decoded.MaxResumePct != nil {
		configuration.MaxResumePct = *decoded.MaxResumePct
	}
	if decoded.MinResumeDurationSeconds != nil {
		configuration.MinResumeDurationSeconds = *decoded.MinResumeDurationSeconds
	}
	if decoded.MinAudiobookResume != nil {
		configuration.MinAudiobookResume = *decoded.MinAudiobookResume
	}
	if decoded.MaxAudiobookResume != nil {
		configuration.MaxAudiobookResume = *decoded.MaxAudiobookResume
	}
	if decoded.InactiveSessionThreshold != nil {
		configuration.InactiveSessionThreshold = *decoded.InactiveSessionThreshold
	}
	if decoded.LibraryMonitorDelay != nil {
		configuration.LibraryMonitorDelay = *decoded.LibraryMonitorDelay
	}
	if decoded.LibraryUpdateDuration != nil {
		configuration.LibraryUpdateDuration = *decoded.LibraryUpdateDuration
	}
	if decoded.CacheSize != nil {
		configuration.CacheSize = *decoded.CacheSize
	}
	if decoded.ImageSavingConvention != nil {
		configuration.ImageSavingConvention = *decoded.ImageSavingConvention
	}
	if decoded.MetadataOptions != nil {
		configuration.MetadataOptions = nonNilMetadataOptions(decoded.MetadataOptions.Options)
	}
	if decoded.SkipDeserializationForBasicTypes != nil {
		configuration.SkipDeserializationForBasicTypes = *decoded.SkipDeserializationForBasicTypes
	}
	if decoded.ServerName != nil {
		configuration.ServerName = *decoded.ServerName
	}
	if decoded.UICulture != nil {
		configuration.UICulture = *decoded.UICulture
	}
	if decoded.SaveMetadataHidden != nil {
		configuration.SaveMetadataHidden = *decoded.SaveMetadataHidden
	}
	if decoded.ContentTypes != nil {
		configuration.ContentTypes = nonNilNameValuePairs(decoded.ContentTypes.Entries)
	}
	if decoded.RemoteClientBitrateLimit != nil {
		configuration.RemoteClientBitrateLimit = *decoded.RemoteClientBitrateLimit
	}
	if decoded.EnableFolderView != nil {
		configuration.EnableFolderView = *decoded.EnableFolderView
	}
	if decoded.EnableGroupingMoviesIntoCollections != nil {
		configuration.EnableGroupingMoviesIntoCollections = *decoded.EnableGroupingMoviesIntoCollections
	}
	if decoded.EnableGroupingShowsIntoCollections != nil {
		configuration.EnableGroupingShowsIntoCollections = *decoded.EnableGroupingShowsIntoCollections
	}
	if decoded.DisplaySpecialsWithinSeasons != nil {
		configuration.DisplaySpecialsWithinSeasons = *decoded.DisplaySpecialsWithinSeasons
	}
	if decoded.CodecsUsed != nil {
		configuration.CodecsUsed = nonNilStrings(decoded.CodecsUsed.Values)
	}
	if decoded.PluginRepositories != nil {
		configuration.PluginRepositories = nonNilRepositories(decoded.PluginRepositories.Repositories)
	}
	if decoded.EnableExternalContentInSuggestions != nil {
		configuration.EnableExternalContentInSuggestions = *decoded.EnableExternalContentInSuggestions
	}
	if decoded.ImageExtractionTimeoutMs != nil {
		configuration.ImageExtractionTimeoutMs = *decoded.ImageExtractionTimeoutMs
	}
	if decoded.PathSubstitutions != nil {
		configuration.PathSubstitutions = nonNilPathSubstitutions(decoded.PathSubstitutions.Substitutions)
	}
	if decoded.EnableSlowResponseWarning != nil {
		configuration.EnableSlowResponseWarning = *decoded.EnableSlowResponseWarning
	}
	if decoded.SlowResponseThresholdMs != nil {
		configuration.SlowResponseThresholdMs = *decoded.SlowResponseThresholdMs
	}
	if decoded.CorsHosts != nil {
		configuration.CorsHosts = nonNilStrings(decoded.CorsHosts.Values)
	}
	if decoded.ActivityLogRetentionDays != nil {
		configuration.ActivityLogRetentionDays = decoded.ActivityLogRetentionDays
	}
	if decoded.LibraryScanFanoutConcurrency != nil {
		configuration.LibraryScanFanoutConcurrency = *decoded.LibraryScanFanoutConcurrency
	}
	if decoded.LibraryMetadataRefreshConcurrency != nil {
		configuration.LibraryMetadataRefreshConcurrency = *decoded.LibraryMetadataRefreshConcurrency
	}
	if decoded.AllowClientLogUpload != nil {
		configuration.AllowClientLogUpload = *decoded.AllowClientLogUpload
	}
	if decoded.DummyChapterDuration != nil {
		configuration.DummyChapterDuration = *decoded.DummyChapterDuration
	}
	if decoded.ChapterImageResolution != nil {
		configuration.ChapterImageResolution = *decoded.ChapterImageResolution
	}
	if decoded.ParallelImageEncodingLimit != nil {
		configuration.ParallelImageEncodingLimit = *decoded.ParallelImageEncodingLimit
	}
	if decoded.CastReceiverApplications != nil {
		configuration.CastReceiverApplications = nonNilCastReceivers(decoded.CastReceiverApplications.Applications)
	}
	if decoded.TrickplayOptions != nil {
		configuration.TrickplayOptions = *decoded.TrickplayOptions
	}
	if decoded.EnableLegacyAuthorization != nil {
		configuration.EnableLegacyAuthorization = *decoded.EnableLegacyAuthorization
	}
	return nil
}

// Change identifies a persisted configuration file that was successfully
// replaced.
type Change struct {
	File string
}

// Manager owns serialized configuration mutations and immutable value snapshots
// for readers.
type Manager struct {
	paths   Paths
	changes *events.Bus[Change]

	updates  sync.Mutex
	system   configurationSnapshot[ServerConfiguration]
	branding configurationSnapshot[BrandingOptions]
	network  configurationSnapshot[NetworkConfiguration]
	metadata configurationSnapshot[MetadataConfiguration]
	encoding configurationSnapshot[EncodingOptions]
	livetv   configurationSnapshot[LiveTvOptions]

	xbmcMetadata configurationSnapshot[XbmcMetadataOptions]
}

type configurationSnapshot[T any] struct {
	mu    sync.RWMutex
	value T
}

// Open loads system.xml or creates it with the compatibility defaults. A
// malformed source is preserved as system.xml.bak before the canonical default
// configuration replaces it.
func Open(ctx context.Context, paths Paths, changes *events.Bus[Change]) (*Manager, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if changes == nil {
		return nil, errors.New("config: change bus is required")
	}
	if paths.Config == "" {
		return nil, errors.New("config: configuration directory is required")
	}
	if err := os.MkdirAll(paths.Config, 0o700); err != nil {
		return nil, fmt.Errorf("config: create configuration directory: %w", err)
	}

	system, err := loadConfiguration(filepath.Join(paths.Config, systemFile), DefaultServerConfiguration, true)
	if err != nil {
		return nil, err
	}
	branding, err := loadConfiguration(filepath.Join(paths.Config, "branding.xml"), DefaultBrandingOptions, false)
	if err != nil {
		return nil, err
	}
	network, err := loadConfiguration(filepath.Join(paths.Config, "network.xml"), DefaultNetworkConfiguration, false)
	if err != nil {
		return nil, err
	}
	metadata, err := loadConfiguration(filepath.Join(paths.Config, "metadata.xml"), DefaultMetadataConfiguration, false)
	if err != nil {
		return nil, err
	}
	encodingOptions, err := loadConfiguration(filepath.Join(paths.Config, "encoding.xml"), DefaultEncodingOptions, false)
	if err != nil {
		return nil, err
	}
	livetv, err := loadConfiguration(filepath.Join(paths.Config, LiveTvFile), DefaultLiveTvOptions, false)
	xbmcMetadata, err := loadConfiguration(filepath.Join(paths.Config, XbmcMetadataFile), DefaultXbmcMetadataOptions, false)
	if err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	return &Manager{
		paths:    paths,
		changes:  changes,
		system:   configurationSnapshot[ServerConfiguration]{value: system},
		branding: configurationSnapshot[BrandingOptions]{value: branding},
		network:  configurationSnapshot[NetworkConfiguration]{value: network},
		metadata: configurationSnapshot[MetadataConfiguration]{value: metadata},
		encoding: configurationSnapshot[EncodingOptions]{value: encodingOptions},
		livetv:   configurationSnapshot[LiveTvOptions]{value: livetv},

		xbmcMetadata: configurationSnapshot[XbmcMetadataOptions]{value: xbmcMetadata},
	}, nil
}

// Paths returns the application paths the manager reads and writes.
func (manager *Manager) Paths() Paths {
	return manager.paths
}

// System returns an immutable value snapshot of the current system
// configuration.
func (manager *Manager) System() ServerConfiguration {
	manager.system.mu.RLock()
	defer manager.system.mu.RUnlock()
	return manager.system.value
}

// StartupWizardCompleted reports whether the current system configuration has
// completed first-run setup.
func (manager *Manager) StartupWizardCompleted() bool {
	manager.system.mu.RLock()
	defer manager.system.mu.RUnlock()
	return manager.system.value.IsStartupWizardCompleted
}

// Branding returns an immutable value snapshot of the current branding options.
func (manager *Manager) Branding() BrandingOptions {
	manager.branding.mu.RLock()
	defer manager.branding.mu.RUnlock()
	return manager.branding.value
}

// Network returns an immutable value snapshot of the current network
// configuration. The array fields are copies.
func (manager *Manager) Network() NetworkConfiguration {
	manager.network.mu.RLock()
	defer manager.network.mu.RUnlock()
	return manager.network.value.clone()
}

// Metadata returns an immutable value snapshot of the current metadata
// configuration.
func (manager *Manager) Metadata() MetadataConfiguration {
	manager.metadata.mu.RLock()
	defer manager.metadata.mu.RUnlock()
	return manager.metadata.value
}

// Encoding returns an immutable value snapshot of the current encoding
// options. The array fields are copies.
func (manager *Manager) Encoding() EncodingOptions {
	manager.encoding.mu.RLock()
	defer manager.encoding.mu.RUnlock()
	options := manager.encoding.value
	options.HardwareDecodingCodecs = append([]string{}, options.HardwareDecodingCodecs...)
	options.AllowOnDemandMetadataBasedKeyframeExtractionForExtensions = append([]string{}, options.AllowOnDemandMetadataBasedKeyframeExtractionForExtensions...)
	return options
}

// UpdateSystem serializes a durable mutation of system.xml.
func (manager *Manager) UpdateSystem(ctx context.Context, update func(*ServerConfiguration) error) error {
	return updateConfiguration(ctx, manager, &manager.system, systemFile, "system", update)
}

// UpdateBranding serializes a durable mutation of branding.xml.
func (manager *Manager) UpdateBranding(ctx context.Context, update func(*BrandingOptions) error) error {
	return updateConfiguration(ctx, manager, &manager.branding, "branding.xml", "branding", update)
}

// UpdateNetwork serializes a durable mutation of network.xml.
func (manager *Manager) UpdateNetwork(ctx context.Context, update func(*NetworkConfiguration) error) error {
	return updateConfiguration(ctx, manager, &manager.network, "network.xml", "network", update)
}

func updateConfiguration[T any](ctx context.Context, manager *Manager, snapshot *configurationSnapshot[T], file, name string, update func(*T) error) error {
	if update == nil {
		return fmt.Errorf("config: %s update is required", name)
	}
	if err := contextError(ctx); err != nil {
		return err
	}

	manager.updates.Lock()
	defer manager.updates.Unlock()
	if err := contextError(ctx); err != nil {
		return err
	}

	snapshot.mu.RLock()
	next := snapshot.value
	snapshot.mu.RUnlock()
	if err := update(&next); err != nil {
		return err
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	committed, err := saveConfiguration(filepath.Join(manager.paths.Config, file), next)
	if committed {
		snapshot.mu.Lock()
		snapshot.value = next
		snapshot.mu.Unlock()
		manager.changes.Publish(Change{File: file})
	}
	return err
}

type corruptConfigurationError struct {
	source []byte
	err    error
}

func (err *corruptConfigurationError) Error() string {
	return fmt.Sprintf("config: decode configuration: %v", err.err)
}

func (err *corruptConfigurationError) Unwrap() error {
	return err.err
}

func loadConfiguration[T any](path string, defaults func() T, createMissing bool) (T, error) {
	var zero T
	configuration, err := readConfiguration(path, defaults)
	if errors.Is(err, os.ErrNotExist) {
		configuration = defaults()
		if !createMissing {
			return configuration, nil
		}
		if _, err := saveConfiguration(path, configuration); err != nil {
			return zero, err
		}
		return configuration, nil
	}
	if err == nil {
		return configuration, nil
	}

	var corrupt *corruptConfigurationError
	if !errors.As(err, &corrupt) {
		return zero, err
	}
	if _, err := writeAtomically(path+".bak", corrupt.source); err != nil {
		return zero, fmt.Errorf("config: preserve corrupt %s: %w", filepath.Base(path), err)
	}
	configuration = defaults()
	if _, err := saveConfiguration(path, configuration); err != nil {
		return zero, err
	}
	return configuration, nil
}

func readConfiguration[T any](path string, defaults func() T) (T, error) {
	var zero T
	source, err := os.ReadFile(path)
	if err != nil {
		return zero, err
	}

	configuration := defaults()
	if err := xml.Unmarshal(source, &configuration); err != nil {
		return zero, &corruptConfigurationError{source: source, err: err}
	}
	return configuration, nil
}

func saveConfiguration[T any](path string, configuration T) (bool, error) {
	encoded, err := encodeConfiguration(configuration)
	if err != nil {
		return false, fmt.Errorf("config: encode %s: %w", filepath.Base(path), err)
	}
	committed, err := writeAtomically(path, encoded)
	if err != nil {
		return committed, fmt.Errorf("config: save %s: %w", filepath.Base(path), err)
	}
	return true, nil
}

func encodeConfiguration[T any](configuration T) ([]byte, error) {
	var encoded bytes.Buffer
	encoded.WriteString(xml.Header)
	encoder := xml.NewEncoder(&encoded)
	encoder.Indent("", "  ")
	if err := encoder.Encode(configuration); err != nil {
		return nil, err
	}
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func writeAtomically(path string, contents []byte) (bool, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return false, err
	}

	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+"-*")
	if err != nil {
		return false, err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporary != nil {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return false, err
	}
	if _, err := temporary.Write(contents); err != nil {
		return false, err
	}
	if err := temporary.Sync(); err != nil {
		return false, err
	}
	if err := temporary.Close(); err != nil {
		return false, err
	}
	temporary = nil
	if err := os.Rename(temporaryPath, path); err != nil {
		return false, err
	}

	directoryHandle, err := os.Open(directory)
	if err != nil {
		return true, err
	}
	if err := directoryHandle.Sync(); err != nil {
		_ = directoryHandle.Close()
		return true, err
	}
	if err := directoryHandle.Close(); err != nil {
		return true, err
	}
	return true, nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("config: context is required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// MarshalJSON writes the GetNamedConfiguration("branding") shape. Upstream
// declares the strings nullable and omits null members
// (BrandingOptions.cs:6-29, JsonDefaults.cs:33). This model stores an absent
// string as empty, so an empty string is omitted.
func (options BrandingOptions) MarshalJSON() ([]byte, error) {
	optional := func(value string) *string {
		if value == "" {
			return nil
		}
		return &value
	}
	return json.Marshal(struct {
		LoginDisclaimer      *string `json:"LoginDisclaimer,omitzero"`
		CustomCss            *string `json:"CustomCss,omitzero"`
		SplashscreenEnabled  bool    `json:"SplashscreenEnabled"`
		SplashscreenLocation *string `json:"SplashscreenLocation,omitzero"`
	}{
		LoginDisclaimer:      optional(options.LoginDisclaimer),
		CustomCss:            optional(options.CustomCss),
		SplashscreenEnabled:  options.SplashscreenEnabled,
		SplashscreenLocation: optional(options.SplashscreenLocation),
	})
}
