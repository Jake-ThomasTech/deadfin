// SPDX-License-Identifier: GPL-2.0-only

package dto_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/dto"
)

func TestAuthenticationResultRendersNestedUserAndSessionWithBothProfiles(t *testing.T) {
	id, err := dto.ParseUUID("8a1b4a2f-9dbe-4f37-9f6b-2b1c3d4e5f60")
	if err != nil {
		t.Fatal(err)
	}
	name := "Ada"
	capabilities := dto.ClientCapabilities{
		PlayableMediaTypes:   dto.CommaDelimitedStrings{"Video", "Audio"},
		SupportedCommands:    dto.CommaDelimitedStrings{"Play", "Stop"},
		SupportsMediaControl: true,
	}
	value := dto.AuthenticationResult{
		User: dto.UserDto{
			Name:       name,
			ServerID:   "server-1",
			ServerName: "Deadfin",
			ID:         id,
			Configuration: dto.UserConfiguration{
				AudioLanguagePreference: &name,
				GroupedFolders:          []dto.UUID{id},
			},
			Policy: dto.UserPolicy{
				BlockedTags:        []string{"Kids", "Spoilers"},
				EnabledDevices:     []string{"Roku"},
				EnabledChannels:    []dto.UUID{id},
				EnableRemoteAccess: true,
			},
		},
		SessionInfo: dto.SessionInfoDto{
			ID:                  &name,
			UserID:              id,
			UserName:            &name,
			LastActivityDate:    dto.NewTime(time.Date(2026, 7, 26, 1, 2, 3, 0, time.UTC)),
			LastPlaybackCheckIn: dto.NewTime(time.Date(2026, 7, 26, 1, 2, 4, 0, time.UTC)),
			Capabilities:        &capabilities,
			PlayableMediaTypes:  dto.CommaDelimitedStrings{"Video", "Audio"},
			SupportedCommands:   dto.CommaDelimitedStrings{"Play", "Stop"},
		},
		AccessToken: "token",
		ServerID:    "server-1",
	}

	for _, tc := range []struct {
		name   string
		accept string
		keys   []string
	}{
		{name: "Pascal", keys: []string{"User", "SessionInfo", "AccessToken", "ServerId"}},
		{name: "Camel", accept: `application/json; profile="CamelCase"`, keys: []string{"user", "sessionInfo", "accessToken", "serverId"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept", tc.accept)
			rec := httptest.NewRecorder()
			if err := render.JSON(rec, req, http.StatusOK, value); err != nil {
				t.Fatal(err)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			for _, key := range tc.keys {
				if _, ok := body[key]; !ok {
					t.Errorf("response missing %q: %s", key, rec.Body.String())
				}
			}
			var session map[string]json.RawMessage
			if err := json.Unmarshal(body[tc.keys[1]], &session); err != nil {
				t.Fatalf("decode nested session: %v", err)
			}
			capabilityKey := "Capabilities"
			if tc.accept != "" {
				capabilityKey = "capabilities"
			}
			var capability map[string]json.RawMessage
			if err := json.Unmarshal(session[capabilityKey], &capability); err != nil {
				t.Fatalf("decode capabilities: %v", err)
			}
			playableKey := "PlayableMediaTypes"
			if tc.accept != "" {
				playableKey = "playableMediaTypes"
			}
			if got, want := string(capability[playableKey]), `["Video","Audio"]`; got != want {
				t.Errorf("capabilities %s = %s, want array %s", playableKey, got, want)
			}
		})
	}
}

func TestClientCapabilitiesAcceptsStockCommaDelimitedAndArrayJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{name: "comma delimited", body: `{"PlayableMediaTypes":"Video,Audio","SupportedCommands":"Play,Stop"}`, want: []string{"Video", "Audio"}},
		{name: "array forward compatibility", body: `{"PlayableMediaTypes":["Video","Audio"],"SupportedCommands":["Play","Stop"]}`, want: []string{"Video", "Audio"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var capabilities dto.ClientCapabilities
			if err := json.Unmarshal([]byte(tc.body), &capabilities); err != nil {
				t.Fatalf("unmarshal capabilities: %v", err)
			}
			if got := []string(capabilities.PlayableMediaTypes); !equalStrings(got, tc.want) {
				t.Fatalf("PlayableMediaTypes = %#v, want %#v", got, tc.want)
			}
			encoded, err := json.Marshal(capabilities)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &body); err != nil {
				t.Fatal(err)
			}
			if got, want := string(body["PlayableMediaTypes"]), `["Video","Audio"]`; got != want {
				t.Errorf("PlayableMediaTypes wire value = %s, want %s", got, want)
			}
		})
	}
}

func TestUserPolicyPublishesEveryPinnedWireField(t *testing.T) {
	want := map[string]struct{}{
		"IsAdministrator": {}, "IsHidden": {}, "EnableCollectionManagement": {}, "EnableSubtitleManagement": {}, "EnableLyricManagement": {}, "IsDisabled": {}, "MaxParentalRating": {}, "MaxParentalSubRating": {}, "BlockedTags": {}, "AllowedTags": {}, "EnableUserPreferenceAccess": {}, "AccessSchedules": {}, "BlockUnratedItems": {}, "EnableRemoteControlOfOtherUsers": {}, "EnableSharedDeviceControl": {}, "EnableRemoteAccess": {}, "EnableLiveTvManagement": {}, "EnableLiveTvAccess": {}, "EnableMediaPlayback": {}, "EnableAudioPlaybackTranscoding": {}, "EnableVideoPlaybackTranscoding": {}, "EnablePlaybackRemuxing": {}, "ForceRemoteSourceTranscoding": {}, "EnableContentDeletion": {}, "EnableContentDeletionFromFolders": {}, "EnableContentDownloading": {}, "EnableSyncTranscoding": {}, "EnableMediaConversion": {}, "EnabledDevices": {}, "EnableAllDevices": {}, "EnabledChannels": {}, "EnableAllChannels": {}, "EnabledFolders": {}, "EnableAllFolders": {}, "InvalidLoginAttemptCount": {}, "LoginAttemptsBeforeLockout": {}, "MaxActiveSessions": {}, "EnablePublicSharing": {}, "BlockedMediaFolders": {}, "BlockedChannels": {}, "RemoteClientBitrateLimit": {}, "AuthenticationProviderId": {}, "PasswordResetProviderId": {}, "SyncPlayAccess": {},
	}
	// Deadfin-only fields, each with its divergence row.
	for _, name := range []string{"LimitLiveTvToSharedLists"} { // D-105
		want[name] = struct{}{}
	}
	typ := reflect.TypeOf(dto.UserPolicy{})
	if typ.NumField() != len(want) {
		t.Fatalf("UserPolicy has %d fields, want %d pinned and Deadfin fields", typ.NumField(), len(want))
	}
	for index := range typ.NumField() {
		field := typ.Field(index)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if _, ok := want[name]; !ok {
			t.Errorf("UserPolicy field %q has unexpected wire name %q", field.Name, name)
			continue
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("UserPolicy missing pinned wire field %q", name)
	}
}

func TestLoginAndPasswordBodiesPreserveNullVersusEmpty(t *testing.T) {
	var login dto.AuthenticateUserByName
	if err := json.Unmarshal([]byte(`{"Username":"","Pw":null}`), &login); err != nil {
		t.Fatal(err)
	}
	if login.Username == nil || *login.Username != "" || login.Pw != nil {
		t.Fatalf("login body did not preserve empty/null distinction: %#v", login)
	}

	var password dto.UpdateUserPassword
	if err := json.Unmarshal([]byte(`{"CurrentPassword":null,"CurrentPw":"","NewPw":"new","ResetPassword":true}`), &password); err != nil {
		t.Fatal(err)
	}
	if password.CurrentPassword != nil || password.CurrentPw == nil || *password.CurrentPw != "" || password.NewPw == nil || *password.NewPw != "new" || !password.ResetPassword {
		t.Fatalf("password body did not preserve nullable request values: %#v", password)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
