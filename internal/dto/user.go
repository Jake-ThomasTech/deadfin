// SPDX-License-Identifier: GPL-2.0-only

package dto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/deadfin-org/deadfin/internal/jsonnum"
)

// AuthenticationResult is returned after a successful user authentication.
type AuthenticationResult struct {
	User        UserDto        `json:"User"`
	SessionInfo SessionInfoDto `json:"SessionInfo"`
	AccessToken string         `json:"AccessToken"`
	ServerID    string         `json:"ServerId"`
}

// UserDto is Jellyfin's public user representation.
type UserDto struct {
	Name                      string            `json:"Name"`
	ServerID                  string            `json:"ServerId"`
	ServerName                string            `json:"ServerName,omitempty"`
	ID                        UUID              `json:"Id"`
	PrimaryImageTag           string            `json:"PrimaryImageTag,omitempty"`
	HasPassword               *bool             `json:"HasPassword"`
	HasConfiguredPassword     *bool             `json:"HasConfiguredPassword"`
	HasConfiguredEasyPassword *bool             `json:"HasConfiguredEasyPassword"`
	EnableAutoLogin           *bool             `json:"EnableAutoLogin"`
	LastLoginDate             *Time             `json:"LastLoginDate,omitempty"`
	LastActivityDate          *Time             `json:"LastActivityDate,omitempty"`
	Configuration             UserConfiguration `json:"Configuration"`
	Policy                    UserPolicy        `json:"Policy"`
	PrimaryImageAspectRatio   *float64          `json:"PrimaryImageAspectRatio,omitempty"`
}

// UserConfiguration is the persisted per-user playback and library-view state.
// UserConfiguration is the persisted per-user playback and library-view state.
type UserConfiguration struct {
	AudioLanguagePreference    *string      `json:"AudioLanguagePreference,omitempty"`
	PlayDefaultAudioTrack      bool         `json:"PlayDefaultAudioTrack"`
	SubtitleLanguagePreference *string      `json:"SubtitleLanguagePreference"`
	DisplayMissingEpisodes     bool         `json:"DisplayMissingEpisodes"`
	GroupedFolders             []UUID       `json:"GroupedFolders"`
	SubtitleMode               SubtitleMode `json:"SubtitleMode"`
	DisplayCollectionsView     bool         `json:"DisplayCollectionsView"`
	EnableLocalPassword        bool         `json:"EnableLocalPassword"`
	OrderedViews               []UUID       `json:"OrderedViews"`
	LatestItemsExcludes        []UUID       `json:"LatestItemsExcludes"`
	MyMediaExcludes            []UUID       `json:"MyMediaExcludes"`
	HidePlayedInLatest         bool         `json:"HidePlayedInLatest"`
	RememberAudioSelections    bool         `json:"RememberAudioSelections"`
	RememberSubtitleSelections bool         `json:"RememberSubtitleSelections"`
	EnableNextEpisodeAutoPlay  bool         `json:"EnableNextEpisodeAutoPlay"`
	CastReceiverID             string       `json:"CastReceiverId"`
}

// SubtitleMode is Jellyfin's string-backed subtitle preference enum.
type SubtitleMode string

const (
	SubtitleModeDefault    SubtitleMode = "Default"
	SubtitleModeAlways     SubtitleMode = "Always"
	SubtitleModeOnlyForced SubtitleMode = "OnlyForced"
	SubtitleModeNone       SubtitleMode = "None"
	SubtitleModeSmart      SubtitleMode = "Smart"
	SubtitleModeOriginal   SubtitleMode = "Original"
)

// Valid reports whether value names a supported persisted subtitle preference.
func (mode SubtitleMode) Valid() bool {
	switch mode {
	case SubtitleModeDefault, SubtitleModeAlways, SubtitleModeOnlyForced, SubtitleModeNone, SubtitleModeSmart, SubtitleModeOriginal:
		return true
	default:
		return false
	}
}

// UnmarshalJSON rejects values outside the source enum rather than preserving
// an invalid value that later request paths cannot interpret.
func (mode *SubtitleMode) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed := SubtitleMode(value)
	if !parsed.Valid() {
		return fmt.Errorf("invalid SubtitleMode %q", value)
	}
	*mode = parsed
	return nil
}

// DefaultUserConfiguration returns UserConfiguration's source defaults.
func DefaultUserConfiguration() UserConfiguration {
	empty := ""
	return UserConfiguration{
		CastReceiverID:             "F007D354",
		SubtitleLanguagePreference: &empty,
		SubtitleMode:               SubtitleModeDefault,
		PlayDefaultAudioTrack:      true,
		HidePlayedInLatest:         true,
		RememberAudioSelections:    true,
		RememberSubtitleSelections: true,
		EnableNextEpisodeAutoPlay:  true,
		GroupedFolders:             []UUID{},
		OrderedViews:               []UUID{},
		LatestItemsExcludes:        []UUID{},
		MyMediaExcludes:            []UUID{},
	}
}

// UserPolicy is Jellyfin's access policy for one user.
type UserPolicy struct {
	IsAdministrator                  bool           `json:"IsAdministrator"`
	IsHidden                         bool           `json:"IsHidden"`
	EnableCollectionManagement       bool           `json:"EnableCollectionManagement"`
	EnableSubtitleManagement         bool           `json:"EnableSubtitleManagement"`
	EnableLyricManagement            bool           `json:"EnableLyricManagement"`
	IsDisabled                       bool           `json:"IsDisabled"`
	MaxParentalRating                *int           `json:"MaxParentalRating,omitempty"`
	MaxParentalSubRating             *int           `json:"MaxParentalSubRating,omitempty"`
	BlockedTags                      []string       `json:"BlockedTags"`
	AllowedTags                      []string       `json:"AllowedTags"`
	EnableUserPreferenceAccess       bool           `json:"EnableUserPreferenceAccess"`
	AccessSchedules                  []any          `json:"AccessSchedules"`
	BlockUnratedItems                []int          `json:"BlockUnratedItems"`
	EnableRemoteControlOfOtherUsers  bool           `json:"EnableRemoteControlOfOtherUsers"`
	EnableSharedDeviceControl        bool           `json:"EnableSharedDeviceControl"`
	EnableRemoteAccess               bool           `json:"EnableRemoteAccess"`
	EnableLiveTvManagement           bool           `json:"EnableLiveTvManagement"`
	EnableLiveTvAccess               bool           `json:"EnableLiveTvAccess"`
	EnableMediaPlayback              bool           `json:"EnableMediaPlayback"`
	EnableAudioPlaybackTranscoding   bool           `json:"EnableAudioPlaybackTranscoding"`
	EnableVideoPlaybackTranscoding   bool           `json:"EnableVideoPlaybackTranscoding"`
	EnablePlaybackRemuxing           bool           `json:"EnablePlaybackRemuxing"`
	ForceRemoteSourceTranscoding     bool           `json:"ForceRemoteSourceTranscoding"`
	EnableContentDeletion            bool           `json:"EnableContentDeletion"`
	EnableContentDeletionFromFolders []string       `json:"EnableContentDeletionFromFolders"`
	EnableContentDownloading         bool           `json:"EnableContentDownloading"`
	EnableSyncTranscoding            bool           `json:"EnableSyncTranscoding"`
	EnableMediaConversion            bool           `json:"EnableMediaConversion"`
	EnabledDevices                   []string       `json:"EnabledDevices"`
	EnableAllDevices                 bool           `json:"EnableAllDevices"`
	EnabledChannels                  []UUID         `json:"EnabledChannels"`
	EnableAllChannels                bool           `json:"EnableAllChannels"`
	EnabledFolders                   []UUID         `json:"EnabledFolders"`
	EnableAllFolders                 bool           `json:"EnableAllFolders"`
	InvalidLoginAttemptCount         int            `json:"InvalidLoginAttemptCount"`
	LoginAttemptsBeforeLockout       int            `json:"LoginAttemptsBeforeLockout"`
	MaxActiveSessions                int            `json:"MaxActiveSessions"`
	EnablePublicSharing              bool           `json:"EnablePublicSharing"`
	BlockedMediaFolders              []UUID         `json:"BlockedMediaFolders"`
	BlockedChannels                  []UUID         `json:"BlockedChannels"`
	RemoteClientBitrateLimit         int            `json:"RemoteClientBitrateLimit"`
	AuthenticationProviderID         string         `json:"AuthenticationProviderId"`
	PasswordResetProviderID          string         `json:"PasswordResetProviderId"`
	SyncPlayAccess                   SyncPlayAccess `json:"SyncPlayAccess"`
	// LimitLiveTvToSharedLists is Deadfin-only (D-105): when true, the user
	// sees a Live TV channel only if a playlist the user can see links it.
	// Absent is false. A policy save that omits it keeps the stored value.
	LimitLiveTvToSharedLists *bool `json:"LimitLiveTvToSharedLists,omitempty"`
}

// DecodeUserPolicy validates a persisted UserPolicy document before decoding it
// into destination. It rejects null for every boolean declared by UserPolicy,
// including fields added later, because encoding/json otherwise silently turns
// null into false.
func DecodeUserPolicy(document []byte, destination any) error {
	if bytes.Equal(bytes.TrimSpace(document), []byte("null")) {
		return fmt.Errorf("decode user policy: policy document cannot be null")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(document, &fields); err != nil {
		return fmt.Errorf("decode user policy: %w", err)
	}
	for name, value := range fields {
		if userPolicyBoolean(name) && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("decode user policy: %s cannot be null", name)
		}
	}
	if err := jsonnum.Unmarshal(document, destination); err != nil {
		return fmt.Errorf("decode user policy: %w", err)
	}
	return nil
}

var userPolicyBooleanNames = func() map[string]struct{} {
	policyType := reflect.TypeFor[UserPolicy]()
	names := make(map[string]struct{}, policyType.NumField())
	for index := range policyType.NumField() {
		field := policyType.Field(index)
		if field.Type.Kind() != reflect.Bool {
			continue
		}
		jsonName, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		names[jsonName] = struct{}{}
	}
	return names
}()

func userPolicyBoolean(name string) bool {
	if _, found := userPolicyBooleanNames[name]; found {
		return true
	}
	for jsonName := range userPolicyBooleanNames {
		if strings.EqualFold(name, jsonName) {
			return true
		}
	}
	return false
}

// SyncPlayAccess is Jellyfin's string-backed SyncPlay policy enum.
type SyncPlayAccess string

const (
	SyncPlayAccessCreateAndJoinGroups SyncPlayAccess = "CreateAndJoinGroups"
	SyncPlayAccessJoinGroups          SyncPlayAccess = "JoinGroups"
	SyncPlayAccessNone                SyncPlayAccess = "None"
)

// Valid reports whether value names a source SyncPlayUserAccessType member.
func (access SyncPlayAccess) Valid() bool {
	switch access {
	case SyncPlayAccessCreateAndJoinGroups, SyncPlayAccessJoinGroups, SyncPlayAccessNone:
		return true
	default:
		return false
	}
}

// UnmarshalJSON rejects values outside the source enum rather than preserving
// an invalid value that later authorization paths cannot interpret.
func (access *SyncPlayAccess) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed := SyncPlayAccess(value)
	if !parsed.Valid() {
		return fmt.Errorf("invalid SyncPlayAccess %q", value)
	}
	*access = parsed
	return nil
}

// DefaultUserPolicy returns UserPolicy's source defaults.
func DefaultUserPolicy() UserPolicy {
	return UserPolicy{
		IsHidden:                         true,
		EnableMediaPlayback:              true,
		EnableAudioPlaybackTranscoding:   true,
		EnableVideoPlaybackTranscoding:   true,
		EnablePlaybackRemuxing:           true,
		EnableSyncTranscoding:            true,
		EnableMediaConversion:            true,
		EnableLiveTvAccess:               true,
		EnableSharedDeviceControl:        true,
		EnableUserPreferenceAccess:       true,
		EnableContentDownloading:         true,
		EnablePublicSharing:              true,
		EnableRemoteAccess:               true,
		EnableAllDevices:                 true,
		EnableAllChannels:                true,
		EnableAllFolders:                 true,
		LoginAttemptsBeforeLockout:       -1,
		AuthenticationProviderID:         "Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider",
		PasswordResetProviderID:          "Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider",
		SyncPlayAccess:                   SyncPlayAccessNone,
		BlockedTags:                      []string{},
		AllowedTags:                      []string{},
		AccessSchedules:                  []any{},
		BlockUnratedItems:                []int{},
		EnableContentDeletionFromFolders: []string{},
		EnabledDevices:                   []string{},
		EnabledChannels:                  []UUID{},
		EnabledFolders:                   []UUID{},
		BlockedMediaFolders:              []UUID{},
		BlockedChannels:                  []UUID{},
	}
}

// AuthenticateUserByName is the username/password authentication request.
type AuthenticateUserByName struct {
	Username *string `json:"Username"`
	Pw       *string `json:"Pw"`
}

// UpdateUserPassword is the password update request.
type UpdateUserPassword struct {
	CurrentPassword *string `json:"CurrentPassword"`
	CurrentPw       *string `json:"CurrentPw"`
	NewPw           *string `json:"NewPw"`
	ResetPassword   bool    `json:"ResetPassword"`
}
