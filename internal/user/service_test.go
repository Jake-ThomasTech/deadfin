// SPDX-License-Identifier: GPL-2.0-only

package user_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
	"github.com/deadfin-org/deadfin/internal/store/storetest"
	"github.com/deadfin-org/deadfin/internal/user"
)

const (
	// These literal vectors were generated using the pinned upstream formula in
	// Emby.Server.Implementations/Cryptography/CryptographyProvider.cs:38-64
	// and the PHC-like hex serialization in MediaBrowser.Model/Cryptography/
	// PasswordHash.cs:189-209, for password "correct horse battery staple".
	// The legacy PBKDF2-SHA1 vector is the pre-10 format explicitly rehashed by
	// DefaultAuthenticationProvider.cs:82-88; the SHA-512 vector is the current
	// 210000-iteration default from Constants.cs:19-22.
	legacyPasswordHash  = "$PBKDF2$iterations=1000$00112233445566778899AABBCCDDEEFF$F4E884262AACB8B0F70D28E4CD1E4684B31AB7670657503460C27A24B251C96E"
	currentPasswordHash = "$PBKDF2-SHA512$iterations=210000$102132435465768798A9BACBDCEDFE0F$6F7BCCB2967EC7552DC313FF7C62919D8E40828F8B19E060CB606A59FFD71C1D768E14444F61D6891890250A8FDBF87C477B177D0AE8737971CACF73AAF986AF"
	vectorPassword      = "correct horse battery staple"
)

func TestServiceUserCRUDNormalizedNameAndPolicyConfigurationRoundTrip(t *testing.T) {
	service, _, _ := openUserService(t)
	ctx := t.Context()

	created, err := service.Create(ctx, "Casey", "initial password")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if created.Name != "Casey" {
		t.Fatalf("created name = %q, want Casey", created.Name)
	}

	byName, err := service.GetByName(ctx, "casey")
	if err != nil {
		t.Fatalf("get case-insensitively by name: %v", err)
	}
	if byName.ID != created.ID {
		t.Fatalf("name lookup ID = %s, want %s", byName.ID, created.ID)
	}

	_, err = service.Create(ctx, "CASEY", "another password")
	if !errors.Is(err, store.ErrDuplicateName) {
		t.Fatalf("create case-colliding user error = %v, want ErrDuplicateName", err)
	}

	created.Policy = []byte(`{"IsAdministrator":false,"EnableRemoteAccess":false}`)
	created.Configuration = []byte(`{"SubtitleMode":"Always","PlayDefaultAudioTrack":false}`)
	if err := service.Update(ctx, created); err != nil {
		t.Fatalf("update user policy/configuration: %v", err)
	}
	got, err := service.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get updated user: %v", err)
	}
	if string(got.Policy) != string(created.Policy) {
		t.Fatalf("policy = %q, want %q", got.Policy, created.Policy)
	}
	if string(got.Configuration) != string(created.Configuration) {
		t.Fatalf("configuration = %q, want %q", got.Configuration, created.Configuration)
	}

	if _, err := service.Create(ctx, "delete-survivor", ""); err != nil {
		t.Fatalf("create deletion survivor: %v", err)
	}
	if err := service.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	_, err = service.Get(ctx, created.ID)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get deleted user error = %v, want ErrNotFound", err)
	}
}

// UserPolicy's concrete bool fields must not silently turn a JSON null into
// false. A rejected document leaves the existing persisted policy untouched;
// explicit false remains a valid, persisted permission value.
func TestServiceUpdatePolicyRejectsNullBooleanFieldsAndPersistsFalse(t *testing.T) {
	service, _, _ := openUserService(t)
	ctx := t.Context()
	account, err := service.Create(ctx, "null-policy-target", "initial password")
	if err != nil {
		t.Fatalf("create policy target: %v", err)
	}

	baseline := json.RawMessage(`{"IsHidden":true,"EnableRemoteAccess":true}`)
	if err := service.UpdatePolicy(ctx, account.ID, baseline); err != nil {
		t.Fatalf("persist baseline policy: %v", err)
	}
	assertPolicy := func(want json.RawMessage) {
		t.Helper()
		stored, err := service.Get(ctx, account.ID)
		if err != nil {
			t.Fatalf("get policy target: %v", err)
		}
		if !bytes.Equal(stored.Policy, want) {
			t.Fatalf("persisted policy = %s, want unchanged %s", stored.Policy, want)
		}
	}

	policyType := reflect.TypeFor[dto.UserPolicy]()
	for fieldIndex := range policyType.NumField() {
		field := policyType.Field(fieldIndex)
		if field.Type.Kind() != reflect.Bool {
			continue
		}
		fieldName := strings.Split(field.Tag.Get("json"), ",")[0]
		if fieldName == "" || fieldName == "-" {
			t.Fatalf("boolean UserPolicy field %s has no JSON name", field.Name)
		}
		t.Run(fieldName+" null is rejected", func(t *testing.T) {
			if err := service.UpdatePolicy(ctx, account.ID, baseline); err != nil {
				t.Fatalf("restore baseline before %s=null: %v", fieldName, err)
			}
			document, err := json.Marshal(map[string]any{fieldName: nil})
			if err != nil {
				t.Fatalf("marshal null %s policy: %v", fieldName, err)
			}
			if err := service.UpdatePolicy(ctx, account.ID, document); !errors.Is(err, user.ErrBadRequest) {
				t.Fatalf("UpdatePolicy(%s=null) error = %v, want ErrBadRequest", fieldName, err)
			}
			assertPolicy(baseline)
		})

		t.Run(fieldName+" false persists", func(t *testing.T) {
			document, err := json.Marshal(map[string]any{fieldName: false})
			if err != nil {
				t.Fatalf("marshal false %s policy: %v", fieldName, err)
			}
			if err := service.UpdatePolicy(ctx, account.ID, document); err != nil {
				t.Fatalf("UpdatePolicy(%s=false): %v", fieldName, err)
			}
			assertPolicy(document)
		})
	}

	if err := service.UpdatePolicy(ctx, account.ID, baseline); err != nil {
		t.Fatalf("restore baseline before literal null: %v", err)
	}
	if err := service.UpdatePolicy(ctx, account.ID, json.RawMessage(`null`)); !errors.Is(err, user.ErrBadRequest) {
		t.Fatalf("UpdatePolicy(null) error = %v, want ErrBadRequest", err)
	}
	assertPolicy(baseline)
}

func TestServiceVerifiesPinnedUpstreamPBKDF2VectorsAndRehashesLegacy(t *testing.T) {
	service, identity, _ := openUserService(t)
	ctx := t.Context()

	current, err := service.Create(ctx, "current-vector", "")
	if err != nil {
		t.Fatalf("create current-vector user: %v", err)
	}
	if err := identity.SetPasswordHash(ctx, current.ID, currentPasswordHash); err != nil {
		t.Fatalf("store current upstream hash: %v", err)
	}
	verified, err := service.VerifyPassword(ctx, current.ID, vectorPassword)
	if err != nil {
		t.Fatalf("verify current upstream hash: %v", err)
	}
	if !verified {
		t.Fatal("verify current upstream hash = false, want true")
	}
	wrong, err := service.VerifyPassword(ctx, current.ID, "wrong password")
	if err != nil {
		t.Fatalf("verify wrong current password: %v", err)
	}
	if wrong {
		t.Fatal("verify wrong current password = true, want false")
	}

	legacy, err := service.Create(ctx, "legacy-vector", "")
	if err != nil {
		t.Fatalf("create legacy-vector user: %v", err)
	}
	if err := identity.SetPasswordHash(ctx, legacy.ID, legacyPasswordHash); err != nil {
		t.Fatalf("store legacy upstream hash: %v", err)
	}
	verified, err = service.VerifyPassword(ctx, legacy.ID, vectorPassword)
	if err != nil {
		t.Fatalf("verify legacy upstream hash: %v", err)
	}
	if !verified {
		t.Fatal("verify legacy upstream hash = false, want true")
	}
	migrated, err := service.Get(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("get legacy user after verification: %v", err)
	}
	if migrated.PasswordHash == legacyPasswordHash || !strings.HasPrefix(migrated.PasswordHash, "$PBKDF2-SHA512$iterations=210000$") {
		t.Fatalf("legacy rehash = %q, want a current PBKDF2-SHA512 210000 hash", migrated.PasswordHash)
	}
	verified, err = service.VerifyPassword(ctx, legacy.ID, vectorPassword)
	if err != nil {
		t.Fatalf("verify migrated legacy hash: %v", err)
	}
	if !verified {
		t.Fatal("verify migrated legacy hash = false, want true")
	}
}

func TestServiceChangePasswordInvalidatesOldPassword(t *testing.T) {
	service, _, _ := openUserService(t)
	ctx := t.Context()
	created, err := service.Create(ctx, "password-change", "old password")
	if err != nil {
		t.Fatalf("create password-change user: %v", err)
	}

	if err := service.ChangePassword(ctx, created.ID, "new password"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	oldVerified, err := service.VerifyPassword(ctx, created.ID, "old password")
	if err != nil {
		t.Fatalf("verify old password: %v", err)
	}
	if oldVerified {
		t.Fatal("verify old password = true after change, want false")
	}
	newVerified, err := service.VerifyPassword(ctx, created.ID, "new password")
	if err != nil {
		t.Fatalf("verify new password: %v", err)
	}
	if !newVerified {
		t.Fatal("verify new password = false after change, want true")
	}
}

func TestServiceDeviceTokensIssueLookupLogoutAndRevokeExceptCurrent(t *testing.T) {
	service, _, _ := openUserService(t)
	ctx := t.Context()
	owner, err := service.Create(ctx, "device-owner", "")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := service.Create(ctx, "device-other", "")
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}

	current := issueServiceDevice(t, service, owner.ID, "owner-current")
	revoked := issueServiceDevice(t, service, owner.ID, "owner-revoked")
	unrelated := issueServiceDevice(t, service, other.ID, "other-device")
	if current.AccessToken == "" {
		t.Fatal("issue token returned an empty token")
	}
	lookedUp, err := service.Token(ctx, current.AccessToken)
	if err != nil {
		t.Fatalf("lookup issued token: %v", err)
	}
	if lookedUp.UserID != owner.ID || lookedUp.DeviceID != current.DeviceID {
		t.Fatalf("token lookup = %#v, want current device %#v", lookedUp, current)
	}

	if err := service.RevokeTokens(ctx, owner.ID, current.AccessToken); err != nil {
		t.Fatalf("revoke owner tokens except current: %v", err)
	}
	if _, err := service.Token(ctx, current.AccessToken); err != nil {
		t.Fatalf("lookup retained current token: %v", err)
	}
	if _, err := service.Token(ctx, revoked.AccessToken); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lookup revoked token error = %v, want ErrNotFound", err)
	}
	if _, err := service.Token(ctx, unrelated.AccessToken); err != nil {
		t.Fatalf("lookup unrelated user token after revoke: %v", err)
	}

	if err := service.Logout(ctx, current.AccessToken); err != nil {
		t.Fatalf("logout current token: %v", err)
	}
	if _, err := service.Token(ctx, current.AccessToken); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lookup logged-out token error = %v, want ErrNotFound", err)
	}
}

func TestServiceRefreshDeviceActivityUsesStrictThreeMinuteThresholdWithFakeClock(t *testing.T) {
	service, _, fake := openUserService(t)
	ctx := t.Context()
	owner, err := service.Create(ctx, "activity-owner", "")
	if err != nil {
		t.Fatalf("create activity owner: %v", err)
	}
	issued := issueServiceDevice(t, service, owner.ID, "activity-device")
	start := issued.DateLastActivity

	device, changed, err := service.RefreshDevice(ctx, issued)
	if err != nil {
		t.Fatalf("refresh immediately after issue: %v", err)
	}
	if changed || !device.DateLastActivity.Equal(start) {
		t.Fatalf("immediate refresh = (%s, changed=%t), want (%s, false)", device.DateLastActivity, changed, start)
	}

	fake.Advance(3*time.Minute - time.Nanosecond)
	device, changed, err = service.RefreshDevice(ctx, device)
	if err != nil {
		t.Fatalf("refresh below three minutes: %v", err)
	}
	if changed || !device.DateLastActivity.Equal(start) {
		t.Fatalf("below threshold = (%s, changed=%t), want (%s, false)", device.DateLastActivity, changed, start)
	}

	fake.Advance(time.Nanosecond)
	device, changed, err = service.RefreshDevice(ctx, device)
	if err != nil {
		t.Fatalf("refresh at three minutes: %v", err)
	}
	if changed || !device.DateLastActivity.Equal(start) {
		t.Fatalf("equal threshold = (%s, changed=%t), want (%s, false)", device.DateLastActivity, changed, start)
	}

	fake.Advance(time.Nanosecond)
	device, changed, err = service.RefreshDevice(ctx, device)
	if err != nil {
		t.Fatalf("refresh above three minutes: %v", err)
	}
	if !changed || !device.DateLastActivity.Equal(fake.Now()) {
		t.Fatalf("above threshold = (%s, changed=%t), want (%s, true)", device.DateLastActivity, changed, fake.Now())
	}
}

func TestServiceRejectsPinnedUpstreamInvalidUsernamesAndAcceptsValidVectors(t *testing.T) {
	valid := []string{
		"this_is_valid",
		"this is also valid",
		"0@_-' .",
		"Aa0@_-' .+",
		"thisisa+testemail@test.foo",
		"------@@@--+++----@@--abcdefghijklmn---------@----_-_-___-_ .9foo+",
	}
	for _, name := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			service, _, _ := openUserService(t)
			if _, err := service.Create(t.Context(), name, "password"); err != nil {
				t.Fatalf("create valid upstream username %q: %v", name, err)
			}
		})
	}

	service, identity, _ := openUserService(t)
	invalid := []string{" ", "", "special characters like & $ ? are not allowed", "thishasaspaceontheend ", " thishasaspaceatthestart", " thishasaspaceatbothends ", " this has a space at both ends and inbetween ", ".", ".."}
	for _, name := range invalid {
		if _, err := service.Create(t.Context(), name, "password"); err == nil {
			t.Fatalf("create invalid upstream username %q succeeded", name)
		}
	}
	users, err := identity.ListUsers(t.Context())
	if err != nil {
		t.Fatalf("list users after rejected usernames: %v", err)
	}
	if len(users) != 0 {
		t.Fatalf("rejected username attempts persisted %d users, want none", len(users))
	}
}

func TestServiceNormalizesPinnedDotNetInvariantSupplementaryCasing(t *testing.T) {
	service, _, _ := openUserService(t)
	ctx := t.Context()
	const lower = "\u1c8a"
	const upper = "\u1c89"
	if got := user.NormalizeName(lower); got != upper {
		t.Fatalf("NormalizeName(U+1C8A) = %U, want U+1C89 (.NET ToUpperInvariant)", []rune(got))
	}
	created, err := service.Create(ctx, lower, "password")
	if err != nil {
		t.Fatalf("create U+1C8A username: %v", err)
	}
	lookedUp, err := service.GetByName(ctx, upper)
	if err != nil {
		t.Fatalf("lookup U+1C89 normalized username: %v", err)
	}
	if lookedUp.ID != created.ID {
		t.Fatalf("U+1C89 lookup ID = %s, want %s", lookedUp.ID, created.ID)
	}
	if _, err := service.Create(ctx, upper, "other password"); !errors.Is(err, store.ErrDuplicateName) {
		t.Fatalf("create U+1C89 normalized collision error = %v, want ErrDuplicateName", err)
	}
}

func TestServiceRejectsWrongPBKDF2DigestLengths(t *testing.T) {
	service, identity, _ := openUserService(t)
	ctx := t.Context()
	for _, test := range []struct {
		name string
		hash string
	}{
		{name: "legacy truncated", hash: legacyPasswordHash[:len(legacyPasswordHash)-2]},
		{name: "legacy widened", hash: legacyPasswordHash + "00"},
		{name: "current truncated", hash: currentPasswordHash[:len(currentPasswordHash)-2]},
		{name: "current widened", hash: currentPasswordHash + "00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			created, err := service.Create(ctx, "digest-"+strings.ReplaceAll(test.name, " ", "-"), "")
			if err != nil {
				t.Fatalf("create digest user: %v", err)
			}
			if err := identity.SetPasswordHash(ctx, created.ID, test.hash); err != nil {
				t.Fatalf("set malformed digest: %v", err)
			}
			verified, err := service.VerifyPassword(ctx, created.ID, vectorPassword)
			if err == nil || verified {
				t.Fatalf("verify %s = (verified=%t, err=%v), want false and a format error", test.name, verified, err)
			}
		})
	}
}

func TestIdentityPasswordHashCASCannotOverwriteNewPassword(t *testing.T) {
	service, identity, _ := openUserService(t)
	ctx := t.Context()
	created, err := service.Create(ctx, "password-cas", "")
	if err != nil {
		t.Fatalf("create CAS user: %v", err)
	}
	if err := identity.SetPasswordHash(ctx, created.ID, legacyPasswordHash); err != nil {
		t.Fatalf("set legacy hash: %v", err)
	}
	if err := service.ChangePassword(ctx, created.ID, "concurrent new password"); err != nil {
		t.Fatalf("concurrent password change: %v", err)
	}
	changed, err := identity.CompareAndSwapPasswordHash(ctx, created.ID, legacyPasswordHash, currentPasswordHash)
	if err != nil {
		t.Fatalf("stale legacy rehash CAS: %v", err)
	}
	if changed {
		t.Fatal("stale legacy rehash CAS succeeded after ChangePassword, overwriting the newer credential")
	}
	verified, err := service.VerifyPassword(ctx, created.ID, "concurrent new password")
	if err != nil || !verified {
		t.Fatalf("verify concurrent password after stale CAS = (verified=%t, err=%v), want true nil", verified, err)
	}
}

func TestServiceRejectsDeletingLastUserOrAdministratorAndBlankAdminPassword(t *testing.T) {
	service, _, _ := openUserService(t)
	ctx := t.Context()
	admin, err := service.Create(ctx, "sole-admin", "admin password")
	if err != nil {
		t.Fatalf("create sole admin: %v", err)
	}
	admin.Policy = []byte(`{"IsAdministrator":true}`)
	if err := service.Update(ctx, admin); err != nil {
		t.Fatalf("make sole user administrator: %v", err)
	}
	if err := service.Delete(ctx, admin.ID); !errors.Is(err, user.ErrLastUser) {
		t.Fatalf("delete last user error = %v, want ErrLastUser", err)
	}
	if err := service.ChangePassword(ctx, admin.ID, ""); !errors.Is(err, user.ErrAdministratorPasswordRequired) {
		t.Fatalf("clear sole admin password error = %v, want ErrAdministratorPasswordRequired", err)
	}
	verified, err := service.VerifyPassword(ctx, admin.ID, "admin password")
	if err != nil || !verified {
		t.Fatalf("verify admin password after rejected clear = (verified=%t, err=%v), want true nil", verified, err)
	}

	member, err := service.Create(ctx, "member", "member password")
	if err != nil {
		t.Fatalf("create non-admin member: %v", err)
	}
	if err := service.Delete(ctx, admin.ID); !errors.Is(err, user.ErrLastAdministrator) {
		t.Fatalf("delete sole administrator error = %v, want ErrLastAdministrator", err)
	}
	member.Policy = []byte(`{"IsAdministrator":true}`)
	if err := service.Update(ctx, member); err != nil {
		t.Fatalf("promote member to administrator: %v", err)
	}
	if err := service.Delete(ctx, admin.ID); err != nil {
		t.Fatalf("delete administrator after safe replacement: %v", err)
	}
}

func TestServiceConcurrentIssueLeavesOneDeviceAndFailedReplacementKeepsPriorToken(t *testing.T) {
	service, identity, fake := openUserService(t)
	ctx := t.Context()
	owner, err := service.Create(ctx, "concurrent-owner", "")
	if err != nil {
		t.Fatalf("create concurrent owner: %v", err)
	}
	start := make(chan struct{})
	type result struct {
		device store.Device
		err    error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			device, err := service.IssueToken(ctx, store.Device{UserID: owner.ID, AppName: "Deadfin Test", AppVersion: "1.0", DeviceName: "Concurrent", DeviceID: "one-physical-device"})
			results <- result{device: device, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	issued := make([]store.Device, 0, 2)
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent token issue: %v", result.err)
		}
		issued = append(issued, result.device)
	}
	usable := 0
	for _, device := range issued {
		if _, err := service.Token(ctx, device.AccessToken); err == nil {
			usable++
		} else if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("lookup concurrently issued token: %v", err)
		}
	}
	if usable != 1 {
		t.Fatalf("concurrent issue left %d usable tokens for one user/device, want exactly one", usable)
	}

	prior := issueServiceDevice(t, service, owner.ID, "failure-device")
	const collidingToken = "ffffffffffff4fffbfffffffffffffff"
	other, err := service.Create(ctx, "collision-owner", "")
	if err != nil {
		t.Fatalf("create collision owner: %v", err)
	}
	if _, err := identity.CreateDevice(ctx, store.Device{UserID: other.ID, AccessToken: collidingToken, AppName: "Deadfin Test", AppVersion: "1.0", DeviceName: "Collision", DeviceID: "collision-device", DateCreated: fake.Now(), DateModified: fake.Now(), DateLastActivity: fake.Now()}); err != nil {
		t.Fatalf("create collision token: %v", err)
	}
	replacement := user.New(identity, fake, &lockedBytesReader{reader: bytes.NewReader(bytes.Repeat([]byte{0xff}, 32))})
	if _, err := replacement.IssueToken(ctx, store.Device{UserID: owner.ID, AppName: "Deadfin Test", AppVersion: "1.0", DeviceName: "Failure", DeviceID: prior.DeviceID}); err == nil {
		t.Fatal("issue colliding replacement token succeeded, want insertion failure")
	}
	if _, err := service.Token(ctx, prior.AccessToken); err != nil {
		t.Fatalf("lookup prior token after failed replacement: %v", err)
	}
}

func TestServiceLogoutIsIdempotentAfterRevoke(t *testing.T) {
	service, _, _ := openUserService(t)
	ctx := t.Context()
	owner, err := service.Create(ctx, "idempotent-logout", "")
	if err != nil {
		t.Fatalf("create logout owner: %v", err)
	}
	issued := issueServiceDevice(t, service, owner.ID, "logout-device")
	if err := service.RevokeTokens(ctx, owner.ID, ""); err != nil {
		t.Fatalf("revoke issued token: %v", err)
	}
	if err := service.Logout(ctx, issued.AccessToken); err != nil {
		t.Fatalf("logout revoked token: %v", err)
	}
	if err := service.Logout(ctx, issued.AccessToken); err != nil {
		t.Fatalf("repeat logout revoked token: %v", err)
	}
}

func TestServiceHashesNonAdminWhitespacePasswordsAndRejectsAdminWhitespace(t *testing.T) {
	service, _, _ := openUserService(t)
	ctx := t.Context()
	const whitespacePassword = " \t\n "
	member, err := service.Create(ctx, "whitespace-member", "initial password")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	if err := service.ChangePassword(ctx, member.ID, whitespacePassword); err != nil {
		t.Fatalf("set whitespace-only member password: %v", err)
	}
	emptyVerified, err := service.VerifyPassword(ctx, member.ID, "")
	if err != nil || emptyVerified {
		t.Fatalf("verify empty after whitespace-only change = (verified=%t, err=%v), want false nil", emptyVerified, err)
	}
	whitespaceVerified, err := service.VerifyPassword(ctx, member.ID, whitespacePassword)
	if err != nil || !whitespaceVerified {
		t.Fatalf("verify whitespace after whitespace-only change = (verified=%t, err=%v), want true nil", whitespaceVerified, err)
	}
	createdWithWhitespace, err := service.Create(ctx, "whitespace-created", whitespacePassword)
	if err != nil {
		t.Fatalf("create member with whitespace-only password: %v", err)
	}
	whitespaceVerified, err = service.VerifyPassword(ctx, createdWithWhitespace.ID, whitespacePassword)
	if err != nil || !whitespaceVerified {
		t.Fatalf("verify created whitespace password = (verified=%t, err=%v), want true nil", whitespaceVerified, err)
	}

	admin, err := service.Create(ctx, "whitespace-admin", "admin password")
	if err != nil {
		t.Fatalf("create administrator: %v", err)
	}
	admin.Policy = []byte(`{"IsAdministrator":true}`)
	if err := service.Update(ctx, admin); err != nil {
		t.Fatalf("promote administrator: %v", err)
	}
	if err := service.ChangePassword(ctx, admin.ID, "\t "); !errors.Is(err, user.ErrAdministratorPasswordRequired) {
		t.Fatalf("clear whitespace-only administrator password error = %v, want ErrAdministratorPasswordRequired", err)
	}
	adminVerified, err := service.VerifyPassword(ctx, admin.ID, "admin password")
	if err != nil || !adminVerified {
		t.Fatalf("verify administrator password after rejected whitespace clear = (verified=%t, err=%v), want true nil", adminVerified, err)
	}
}

func TestServiceMatchesDotNetWordCharacterCategories(t *testing.T) {
	service, _, _ := openUserService(t)
	ctx := t.Context()
	for _, name := range []string{
		"bad-\u00b2", // No, other number
		"bad-\u2160", // Nl, letter number
		"bad-\u0903", // Mc, spacing combining mark
		"bad-\u20dd", // Me, enclosing mark
	} {
		if _, err := service.Create(ctx, name, "password"); !errors.Is(err, user.ErrInvalidUsername) {
			t.Fatalf("create name containing excluded .NET \\w category %q error = %v, want ErrInvalidUsername", name, err)
		}
	}
	for _, name := range []string{
		"accepted-\u0660",  // Nd, decimal digit
		"accepted-\u203f",  // Pc, connector punctuation
		"accepted-e\u0301", // Mn, nonspacing mark
	} {
		if _, err := service.Create(ctx, name, "password"); err != nil {
			t.Fatalf("create name containing accepted .NET \\w boundary %q: %v", name, err)
		}
	}
}

func TestServiceConcurrentAdminPromotionAndPasswordClearNeverLeavesBlankAdmin(t *testing.T) {
	service, _, _ := openUserService(t)
	ctx := t.Context()
	member, err := service.Create(ctx, "promotion-race", "initial password")
	if err != nil {
		t.Fatalf("create promotion-race user: %v", err)
	}
	promoted := member
	promoted.Policy = []byte(`{"IsAdministrator":true}`)
	start := make(chan struct{})
	errorsByOperation := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		errorsByOperation <- service.Update(ctx, promoted)
	}()
	go func() {
		defer workers.Done()
		<-start
		errorsByOperation <- service.ChangePassword(ctx, member.ID, " \t ")
	}()
	close(start)
	workers.Wait()
	close(errorsByOperation)
	for err := range errorsByOperation {
		if err != nil && !errors.Is(err, user.ErrAdministratorPasswordRequired) {
			t.Fatalf("concurrent promotion/password clear error = %v", err)
		}
	}
	got, err := service.Get(ctx, member.ID)
	if err != nil {
		t.Fatalf("get user after promotion/password race: %v", err)
	}
	if strings.Contains(string(got.Policy), `"IsAdministrator":true`) {
		emptyVerified, err := service.VerifyPassword(ctx, member.ID, "")
		if err != nil {
			t.Fatalf("verify final administrator empty password: %v", err)
		}
		if emptyVerified {
			t.Fatal("concurrent promotion and password clear left an administrator with an empty password")
		}
	}
}

func TestServiceConcurrentAdministratorDeletesLeaveAdministrator(t *testing.T) {
	service, _, _ := openUserService(t)
	ctx := t.Context()
	adminOne, err := service.Create(ctx, "concurrent-admin-one", "password")
	if err != nil {
		t.Fatalf("create first administrator: %v", err)
	}
	adminTwo, err := service.Create(ctx, "concurrent-admin-two", "password")
	if err != nil {
		t.Fatalf("create second administrator: %v", err)
	}
	member, err := service.Create(ctx, "concurrent-member", "password")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	for _, administrator := range []store.User{adminOne, adminTwo} {
		administrator.Policy = []byte(`{"IsAdministrator":true}`)
		if err := service.Update(ctx, administrator); err != nil {
			t.Fatalf("promote %q: %v", administrator.Name, err)
		}
	}
	start := make(chan struct{})
	errorsByDelete := make(chan error, 2)
	var workers sync.WaitGroup
	for _, administrator := range []store.User{adminOne, adminTwo} {
		workers.Add(1)
		go func(id dto.UUID) {
			defer workers.Done()
			<-start
			errorsByDelete <- service.Delete(ctx, id)
		}(administrator.ID)
	}
	close(start)
	workers.Wait()
	close(errorsByDelete)
	succeeded := 0
	protected := 0
	for err := range errorsByDelete {
		if err == nil {
			succeeded++
		} else if errors.Is(err, user.ErrLastAdministrator) {
			protected++
		} else {
			t.Fatalf("concurrent administrator delete error = %v", err)
		}
	}
	if succeeded != 1 || protected != 1 {
		t.Fatalf("concurrent administrator deletes = (%d succeeded, %d protected), want exactly one each", succeeded, protected)
	}
	if _, err := service.Get(ctx, member.ID); err != nil {
		t.Fatalf("concurrent delete altered unrelated member: %v", err)
	}
}

func TestServicePostgresTokenLifecycleConcurrencyAndActivity(t *testing.T) {
	stores := storetest.OpenPostgres(t)
	identity := store.NewIdentity(stores)
	startTime := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	fake := clock.NewFake(startTime)
	random := make([]byte, 4096)
	for index := range random {
		random[index] = byte(index)
	}
	service := user.New(identity, fake, &lockedBytesReader{reader: bytes.NewReader(random)})
	ctx := t.Context()
	owner, err := service.Create(ctx, "pg-token-owner", "password")
	if err != nil {
		t.Fatalf("create PostgreSQL token owner: %v", err)
	}
	current := issueServiceDevice(t, service, owner.ID, "pg-current-device")
	revoked := issueServiceDevice(t, service, owner.ID, "pg-revoked-device")
	if err := service.RevokeTokens(ctx, owner.ID, current.AccessToken); err != nil {
		t.Fatalf("revoke PostgreSQL tokens except current: %v", err)
	}
	if _, err := service.Token(ctx, current.AccessToken); err != nil {
		t.Fatalf("lookup retained PostgreSQL current token: %v", err)
	}
	if _, err := service.Token(ctx, revoked.AccessToken); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lookup revoked PostgreSQL token error = %v, want ErrNotFound", err)
	}
	if err := service.Logout(ctx, revoked.AccessToken); err != nil {
		t.Fatalf("idempotent logout of revoked PostgreSQL token: %v", err)
	}

	start := make(chan struct{})
	type result struct {
		device store.Device
		err    error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			device, err := service.IssueToken(ctx, store.Device{UserID: owner.ID, AppName: "Deadfin Test", AppVersion: "1.0", DeviceName: "PostgreSQL Concurrent", DeviceID: "pg-one-device"})
			results <- result{device: device, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	usable := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent PostgreSQL token issue: %v", result.err)
		}
		if _, err := service.Token(ctx, result.device.AccessToken); err == nil {
			usable++
		} else if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("lookup concurrently issued PostgreSQL token: %v", err)
		}
	}
	if usable != 1 {
		t.Fatalf("concurrent PostgreSQL issue left %d usable tokens, want exactly one", usable)
	}

	fake.Advance(3*time.Minute + time.Nanosecond)
	updated, changed, err := service.RefreshDevice(ctx, current)
	if err != nil || !changed {
		t.Fatalf("refresh PostgreSQL current device = (%#v, changed=%t, err=%v), want changed", updated, changed, err)
	}
	if !updated.DateLastActivity.Equal(fake.Now()) {
		t.Fatalf("PostgreSQL activity = %s, want fake now %s", updated.DateLastActivity, fake.Now())
	}
}

func TestServicePostgresConcurrentAdministratorDeletesLeaveAdministrator(t *testing.T) {
	stores := storetest.OpenPostgres(t)
	identity := store.NewIdentity(stores)
	fake := clock.NewFake(time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC))
	random := make([]byte, 4096)
	for index := range random {
		random[index] = byte(index)
	}
	service := user.New(identity, fake, &lockedBytesReader{reader: bytes.NewReader(random)})
	ctx := t.Context()
	adminOne, err := service.Create(ctx, "pg-admin-one", "password")
	if err != nil {
		t.Fatalf("create first PostgreSQL administrator: %v", err)
	}
	adminTwo, err := service.Create(ctx, "pg-admin-two", "password")
	if err != nil {
		t.Fatalf("create second PostgreSQL administrator: %v", err)
	}
	if _, err := service.Create(ctx, "pg-member", "password"); err != nil {
		t.Fatalf("create PostgreSQL member: %v", err)
	}
	for _, administrator := range []store.User{adminOne, adminTwo} {
		administrator.Policy = []byte(`{"IsAdministrator":true}`)
		if err := service.Update(ctx, administrator); err != nil {
			t.Fatalf("promote PostgreSQL administrator %q: %v", administrator.Name, err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, administrator := range []store.User{adminOne, adminTwo} {
		workers.Add(1)
		go func(id dto.UUID) {
			defer workers.Done()
			<-start
			results <- service.Delete(ctx, id)
		}(administrator.ID)
	}
	close(start)
	workers.Wait()
	close(results)
	succeeded := 0
	protected := 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, user.ErrLastAdministrator) {
			protected++
		} else {
			t.Fatalf("concurrent PostgreSQL administrator delete error = %v", err)
		}
	}
	if succeeded != 1 || protected != 1 {
		t.Fatalf("concurrent PostgreSQL administrator deletes = (%d succeeded, %d protected), want one each", succeeded, protected)
	}
}

func TestServicePostgresSameDeviceReplacementSerializesConcurrentIssues(t *testing.T) {
	stores := storetest.OpenPostgres(t)
	identity := store.NewIdentity(stores)
	fake := clock.NewFake(time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC))
	random := make([]byte, 4096)
	for index := range random {
		random[index] = byte(index)
	}
	service := user.New(identity, fake, &lockedBytesReader{reader: bytes.NewReader(random)})
	ctx := t.Context()
	owner, err := service.Create(ctx, "pg-replacement-owner", "password")
	if err != nil {
		t.Fatalf("create PostgreSQL replacement owner: %v", err)
	}
	prior := issueServiceDevice(t, service, owner.ID, "pg-replacement-device")
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	type result struct {
		device store.Device
		err    error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ready <- struct{}{}
			<-start
			device, err := service.IssueToken(ctx, store.Device{UserID: owner.ID, AppName: "Deadfin Test", AppVersion: "1.0", DeviceName: "PostgreSQL Replacement", DeviceID: prior.DeviceID})
			results <- result{device: device, err: err}
		}()
	}
	<-ready
	<-ready
	close(start)
	workers.Wait()
	close(results)
	issued := make([]store.Device, 0, 2)
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent PostgreSQL same-device replacement error = %v, want serialized success rather than a unique failure", result.err)
		}
		issued = append(issued, result.device)
	}
	if _, err := service.Token(ctx, prior.AccessToken); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lookup prior replaced token error = %v, want ErrNotFound", err)
	}
	usable := 0
	for _, device := range issued {
		if _, err := service.Token(ctx, device.AccessToken); err == nil {
			usable++
		} else if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("lookup issued replacement token error = %v", err)
		}
	}
	if usable != 1 {
		t.Fatalf("same-device replacement left %d usable tokens, want exactly one", usable)
	}
}

func TestServiceUpdateStartupUserValidatesAndPreservesCaseInsensitiveName(t *testing.T) {
	service, _, _ := openUserService(t)
	ctx := t.Context()

	if err := service.UpdateStartupUser(ctx, user.StartupUserUpdate{Password: "missing user password"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("configure absent startup user error = %v, want ErrNotFound", err)
	}
	if _, err := service.Initialize(ctx); err != nil {
		t.Fatalf("initialize startup user: %v", err)
	}
	initial, err := service.FirstUser(ctx)
	if err != nil {
		t.Fatalf("read initialized startup user: %v", err)
	}
	if err := service.UpdateStartupUser(ctx, user.StartupUserUpdate{Password: " \t "}); !errors.Is(err, user.ErrStartupPasswordRequired) {
		t.Fatalf("configure blank startup password error = %v, want ErrStartupPasswordRequired", err)
	}
	unchanged, err := service.FirstUser(ctx)
	if err != nil {
		t.Fatalf("read startup user after rejected password: %v", err)
	}
	if unchanged.Name != initial.Name || unchanged.NormalizedName != initial.NormalizedName || unchanged.PasswordHash != "" {
		t.Fatalf("blank-password rejection mutated startup user = %#v, want unchanged blank-password account", unchanged)
	}

	sameNameDifferentCase := strings.ToUpper(initial.Name)
	if err := service.UpdateStartupUser(ctx, user.StartupUserUpdate{Name: &sameNameDifferentCase, Password: "configured password"}); err != nil {
		t.Fatalf("configure startup user with case-only rename: %v", err)
	}
	configured, err := service.FirstUser(ctx)
	if err != nil {
		t.Fatalf("read configured startup user: %v", err)
	}
	if configured.Name != initial.Name || configured.NormalizedName != initial.NormalizedName {
		t.Fatalf("case-only startup rename changed stored spelling = (%q, %q), want (%q, %q)", configured.Name, configured.NormalizedName, initial.Name, initial.NormalizedName)
	}
	verified, err := service.VerifyPassword(ctx, configured.ID, "configured password")
	if err != nil || !verified {
		t.Fatalf("verify configured startup password = (verified=%t, err=%v), want (true, nil)", verified, err)
	}

	changedName := "Renamed Startup User"
	if err := service.UpdateStartupUser(ctx, user.StartupUserUpdate{Name: &changedName, Password: "replacement password"}); !errors.Is(err, user.ErrStartupUserConfigured) {
		t.Fatalf("reconfigure established startup user error = %v, want ErrStartupUserConfigured", err)
	}
	afterRetry, err := service.FirstUser(ctx)
	if err != nil {
		t.Fatalf("read startup user after configured retry: %v", err)
	}
	if afterRetry.Name != initial.Name {
		t.Fatalf("configured retry renamed startup user to %q, want original spelling %q", afterRetry.Name, initial.Name)
	}
	oldVerified, err := service.VerifyPassword(ctx, afterRetry.ID, "configured password")
	if err != nil || !oldVerified {
		t.Fatalf("configured retry replaced winning password = (verified=%t, err=%v), want (true, nil)", oldVerified, err)
	}
}

func TestServiceUpdateStartupUserConcurrentCandidatesLeaveOneUsableCredential(t *testing.T) {
	service, _, _ := openUserService(t)
	if _, err := service.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize startup user: %v", err)
	}
	candidates := []struct {
		name     string
		password string
	}{
		{name: "First Startup Candidate", password: "first concurrent password"},
		{name: "Second Startup Candidate", password: "second concurrent password"},
	}
	start := make(chan struct{})
	ready := make(chan struct{}, len(candidates))
	results := make(chan error, len(candidates))
	var group sync.WaitGroup
	for _, candidate := range candidates {
		group.Add(1)
		go func(candidate struct{ name, password string }) {
			defer group.Done()
			ready <- struct{}{}
			<-start
			results <- service.UpdateStartupUser(t.Context(), user.StartupUserUpdate{Name: &candidate.name, Password: candidate.password})
		}(candidate)
	}
	for range candidates {
		<-ready
	}
	close(start)
	group.Wait()
	close(results)

	succeeded := 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, user.ErrStartupUserConfigured):
		default:
			t.Fatalf("concurrent startup credential mutation error = %v, want success or ErrStartupUserConfigured", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent startup credential mutation successes = %d, want exactly one", succeeded)
	}

	configured, err := service.FirstUser(t.Context())
	if err != nil {
		t.Fatalf("read concurrently configured startup user: %v", err)
	}
	for _, candidate := range candidates {
		verified, err := service.VerifyPassword(t.Context(), configured.ID, candidate.password)
		if err != nil {
			t.Fatalf("verify concurrent candidate %q: %v", candidate.name, err)
		}
		if configured.Name == candidate.name && verified {
			return
		}
		if (configured.Name == candidate.name) != verified {
			t.Fatalf("concurrent startup write mixed name %q and password candidate %q", configured.Name, candidate.name)
		}
	}
	t.Fatalf("concurrent startup credential mutation persisted unexpected name %q", configured.Name)
}

func TestServiceUpdateStartupUserStateErrorsPrecedePasswordHashing(t *testing.T) {
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open identity store: %v", err)
	}
	t.Cleanup(func() {
		if err := stores.Close(); err != nil {
			t.Errorf("close identity store: %v", err)
		}
	})
	identity := store.NewIdentity(stores)
	entropyFailure := errors.New("entropy is unavailable")
	service := user.New(identity, clock.NewFake(time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)), startupFailingEntropy{err: entropyFailure})
	update := user.StartupUserUpdate{Password: "valid password"}

	if err := service.UpdateStartupUser(t.Context(), update); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent startup user with unavailable entropy error = %v, want ErrNotFound", err)
	}
	if _, err := identity.CreateUser(t.Context(), store.User{
		ID:             startupTestUUID(t, "50000000000000000000000000000001"),
		Name:           "Configured Startup User",
		NormalizedName: "CONFIGURED STARTUP USER",
		PasswordHash:   "existing-hash",
		Policy:         []byte(`{"IsAdministrator":true}`),
		Configuration:  []byte(`{}`),
	}); err != nil {
		t.Fatalf("seed configured startup user: %v", err)
	}
	if err := service.UpdateStartupUser(t.Context(), update); !errors.Is(err, user.ErrStartupUserConfigured) {
		t.Fatalf("configured startup user with unavailable entropy error = %v, want ErrStartupUserConfigured", err)
	}
}

type startupFailingEntropy struct{ err error }

func (entropy startupFailingEntropy) Read([]byte) (int, error) { return 0, entropy.err }

func openUserService(t *testing.T) (*user.Service, *store.Identity, *clock.Fake) {
	t.Helper()
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open SQLite store: %v", err)
	}
	t.Cleanup(func() {
		if err := stores.Close(); err != nil {
			t.Errorf("close SQLite store: %v", err)
		}
	})
	identity := store.NewIdentity(stores)
	fake := clock.NewFake(time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC))
	random := make([]byte, 4096)
	for index := range random {
		random[index] = byte(index)
	}
	return user.New(identity, fake, &lockedBytesReader{reader: bytes.NewReader(random)}), identity, fake
}

func issueServiceDevice(t *testing.T, service *user.Service, userID dto.UUID, deviceID string) store.Device {
	t.Helper()
	issued, err := service.IssueToken(t.Context(), store.Device{
		UserID:     userID,
		AppName:    "Deadfin Test",
		AppVersion: "1.0",
		DeviceName: deviceID,
		DeviceID:   deviceID,
	})
	if err != nil {
		t.Fatalf("issue token for %q: %v", deviceID, err)
	}
	return issued
}

type lockedBytesReader struct {
	mu     sync.Mutex
	reader *bytes.Reader
}

func (r *lockedBytesReader) Read(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reader.Read(data)
}

// A policy save that omits LimitLiveTvToSharedLists keeps the stored value,
// so a stock client cannot clear it; a save that sends it replaces it (D-105).
func TestServiceUpdatePolicyKeepsDeadfinFields(t *testing.T) {
	service, _, _ := openUserService(t)
	ctx := t.Context()
	account, err := service.Create(ctx, "shared-lists-target", "initial password")
	if err != nil {
		t.Fatalf("create policy target: %v", err)
	}
	limit := func() *bool {
		t.Helper()
		stored, err := service.Get(ctx, account.ID)
		if err != nil {
			t.Fatalf("get policy target: %v", err)
		}
		var policy dto.UserPolicy
		if err := json.Unmarshal(stored.Policy, &policy); err != nil {
			t.Fatalf("decode stored policy %s: %v", stored.Policy, err)
		}
		return policy.LimitLiveTvToSharedLists
	}
	for _, step := range []struct {
		document string
		want     *bool
	}{
		{`{"IsHidden":true}`, nil},
		{`{"IsHidden":true,"LimitLiveTvToSharedLists":true}`, new(true)},
		{`{"IsHidden":false}`, new(true)},
		{`{"IsHidden":false,"LimitLiveTvToSharedLists":false}`, new(false)},
		{`{"IsHidden":true}`, new(false)},
	} {
		if err := service.UpdatePolicy(ctx, account.ID, json.RawMessage(step.document)); err != nil {
			t.Fatalf("UpdatePolicy(%s): %v", step.document, err)
		}
		if got := limit(); !reflect.DeepEqual(got, step.want) {
			t.Fatalf("after %s LimitLiveTvToSharedLists = %v, want %v", step.document, got, step.want)
		}
	}
}
