// SPDX-License-Identifier: GPL-2.0-only

// Package user owns user identities and local-password compatibility.
package user

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/deadfin-org/deadfin/internal/activitylog"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/store"
)

const (
	defaultHashID           = "PBKDF2-SHA512"
	defaultIterations       = 210000
	defaultSaltLength       = 16
	defaultHashLength       = 64
	activityStaleness       = 3 * time.Minute
	initialUserFallbackName = "MyJellyfinUser"
)

// initialUserPolicy mirrors UserEntityExtensions.AddDefaultPermissions with
// the three elevated permissions UserManager.InitializeAsync applies to its
// first-run administrator.
const initialUserPolicy = `{"IsAdministrator":true,"IsDisabled":false,"IsHidden":true,"EnableAllChannels":true,"EnableAllDevices":true,"EnableAllFolders":true,"EnableContentDeletion":true,"EnableContentDownloading":true,"EnableMediaConversion":true,"EnableMediaPlayback":true,"EnablePlaybackRemuxing":true,"EnablePublicSharing":true,"EnableRemoteAccess":true,"EnableSyncTranscoding":true,"EnableAudioPlaybackTranscoding":true,"EnableLiveTvAccess":true,"EnableLiveTvManagement":false,"EnableSharedDeviceControl":true,"EnableVideoPlaybackTranscoding":true,"ForceRemoteSourceTranscoding":false,"EnableRemoteControlOfOtherUsers":true,"EnableCollectionManagement":false,"EnableSubtitleManagement":false,"EnableLyricManagement":false,"PasswordResetProviderId":"Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider"}`

var (
	ErrInvalidUsername               = errors.New("invalid username")
	ErrLastUser                      = errors.New("cannot delete the last user")
	ErrLastAdministrator             = errors.New("cannot remove the last administrator")
	ErrAdministratorPasswordRequired = errors.New("administrator password cannot be blank")
	ErrStartupPasswordRequired       = errors.New("startup user password cannot be blank")
	ErrStartupUserConfigured         = store.ErrStartupUserConfigured
	ErrBadRequest                    = errors.New("bad user request")
	ErrUnauthorized                  = errors.New("unauthenticated user request")
	ErrForbidden                     = errors.New("forbidden user request")
)

// Service is the public identity seam. Clock and entropy are explicit so token
// issuance, password writes, and authorization activity remain deterministic in
// tests without package-global state.
type Service struct {
	identity *store.Identity
	clock    clock.Clock
	random   io.Reader
	activity activitylog.Recorder
}

// StartupUserUpdate describes the startup wizard's one-way first-user setup.
// Name is nil when the client wants to retain the initialized account name.
type StartupUserUpdate struct {
	Name     *string
	Password string
}

// Actor is the authenticated request identity used for user-management rules.
type Actor struct {
	UserID        dto.UUID
	Administrator bool
	APIKey        bool
	DeviceID      string
	Remote        bool
	Token         string
}

// UserListOptions controls the public-login and policy-aware user projections.
type UserListOptions struct {
	Public          bool
	StartupComplete bool
	IsHidden        *bool
	IsDisabled      *bool
}

// UserDTO is the public identity projection. Password hashes never leave the
// service boundary.
type UserDTO struct {
	ID               dto.UUID
	Name             string
	Policy           json.RawMessage
	Configuration    json.RawMessage
	LastLoginDate    *time.Time
	LastActivityDate *time.Time
}

// PasswordUpdate describes a user credential change. Reset clears a
// non-administrator credential; New is used for ordinary updates.
type PasswordUpdate struct {
	Current string
	New     string
	Reset   bool
}

func New(identity *store.Identity, c clock.Clock, random io.Reader) *Service {
	return &Service{identity: identity, clock: c, random: random}
}

// WithActivity makes the service record user created, deleted, password
// changed and locked out entries in the activity log (the UserManager event
// consumers). Call it before the service handles a request.
func (s *Service) WithActivity(recorder activitylog.Recorder) *Service {
	s.activity = recorder
	return s
}

func (s *Service) record(entry activitylog.Entry) {
	if s.activity != nil {
		s.activity.Record(entry)
	}
}

// Initialize creates the initial administrator only for an empty identity
// store. Startup remains incomplete until an administrator has a password.
func (s *Service) Initialize(ctx context.Context) (configured bool, err error) {
	users, err := s.identity.ListUsers(ctx)
	if err != nil {
		return false, err
	}
	if len(users) == 0 {
		name := initialUserName()
		id, err := s.newUUID()
		if err != nil {
			return false, err
		}
		if _, err := s.identity.CreateUserIfEmpty(ctx, store.User{
			ID:             id,
			Name:           name,
			NormalizedName: NormalizeName(name),
			Policy:         []byte(initialUserPolicy),
			Configuration:  []byte("{}"),
		}); err != nil {
			return false, err
		}
		users, err = s.identity.ListUsers(ctx)
		if err != nil {
			return false, err
		}
	}
	return startupConfigured(users)
}

func startupConfigured(users []store.User) (bool, error) {
	for _, user := range users {
		admin, err := isAdministrator(user)
		if err != nil {
			return false, err
		}
		if admin && user.PasswordHash != "" {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) Create(ctx context.Context, name, password string) (store.User, error) {
	if !ValidName(name) {
		return store.User{}, ErrInvalidUsername
	}
	id, err := s.newUUID()
	if err != nil {
		return store.User{}, err
	}
	passwordHash, err := s.hashPassword(password)
	if err != nil {
		return store.User{}, err
	}
	created, err := s.identity.CreateUser(ctx, store.User{
		ID:             id,
		Name:           name,
		NormalizedName: NormalizeName(name),
		PasswordHash:   passwordHash,
		Policy:         []byte("{}"),
		Configuration:  []byte("{}"),
	})
	if err != nil {
		return store.User{}, err
	}
	s.record(activitylog.UserCreated(created.ID, created.Name))
	return created, nil
}

// CreateByName ports UserController.CreateUserByName: it creates the user
// and, when the request carries a password, sets it through ChangePassword,
// which writes the password-changed entry as upstream does.
func (s *Service) CreateByName(ctx context.Context, name string, password *string) (store.User, error) {
	value := ""
	if password != nil {
		value = *password
	}
	created, err := s.Create(ctx, name, value)
	if err != nil {
		return store.User{}, err
	}
	if password != nil {
		s.record(activitylog.UserPasswordChanged(created.ID, created.Name))
	}
	return created, nil
}

func (s *Service) Get(ctx context.Context, id dto.UUID) (store.User, error) {
	return s.identity.User(ctx, id)
}

func (s *Service) GetByName(ctx context.Context, name string) (store.User, error) {
	return s.identity.UserByNormalizedName(ctx, NormalizeName(name))
}

// List returns policy-filtered user projections ordered by display name.
func (s *Service) List(ctx context.Context, actor Actor, options UserListOptions) ([]UserDTO, error) {
	users, err := s.identity.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]UserDTO, 0, len(users))
	for _, candidate := range users {
		policy, err := userPolicy(candidate)
		if err != nil {
			return nil, err
		}
		if options.IsHidden != nil && policy.IsHidden != *options.IsHidden {
			continue
		}
		if options.IsDisabled != nil && policy.IsDisabled != *options.IsDisabled {
			continue
		}
		if options.Public {
			if policy.IsHidden || policy.IsDisabled {
				continue
			}
			if options.StartupComplete && !policy.visibleTo(actor) {
				continue
			}
		}
		result = append(result, userDTO(candidate))
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Name == result[right].Name {
			return result[left].ID.String() < result[right].ID.String()
		}
		return result[left].Name < result[right].Name
	})
	return result, nil
}

// GetDTO returns a password-free projection of one persisted identity.
func (s *Service) GetDTO(ctx context.Context, id dto.UUID) (UserDTO, error) {
	user, err := s.identity.User(ctx, id)
	if err != nil {
		return UserDTO{}, err
	}
	return userDTO(user), nil
}

// Me resolves the authenticated caller's own public user projection.
func (s *Service) Me(ctx context.Context, actor Actor) (UserDTO, error) {
	if actor.UserID == dto.NilUUID {
		return UserDTO{}, ErrBadRequest
	}
	result, err := s.GetDTO(ctx, actor.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return UserDTO{}, ErrBadRequest
	}
	return result, err
}

// UpdateConfiguration replaces configuration bytes only when the actor is
// authenticated and authorized for the target account.
func (s *Service) UpdateConfiguration(ctx context.Context, actor Actor, target dto.UUID, configuration json.RawMessage) error {
	if actor.UserID == dto.NilUUID {
		return ErrUnauthorized
	}
	return s.identity.ReplaceUserConfigurationIf(ctx, target, configuration, func(user store.User) error {
		policy, err := userPolicy(user)
		if err != nil {
			return err
		}
		if actor.Administrator || (actor.UserID == user.ID && policy.enableUserPreferenceAccess()) {
			return nil
		}
		return ErrForbidden
	})
}

// UpdatePassword changes a password after applying caller authorization.
// Non-reset changes revoke every target token except the caller's current token.
func (s *Service) UpdatePassword(ctx context.Context, actor Actor, target dto.UUID, update PasswordUpdate) error {
	if actor.UserID == dto.NilUUID {
		return ErrUnauthorized
	}
	var changed store.User
	if update.Reset {
		err := s.identity.SetPasswordHashIf(ctx, target, "", func(user store.User) error {
			changed = user
			policy, err := userPolicy(user)
			if err != nil {
				return err
			}
			if policy.IsAdministrator {
				return ErrAdministratorPasswordRequired
			}
			if actor.UserID == user.ID || actor.Administrator {
				return nil
			}
			return ErrForbidden
		})
		if err != nil {
			return err
		}
		s.record(activitylog.UserPasswordChanged(changed.ID, changed.Name))
		return nil
	}
	passwordHash, err := s.hashPassword(update.New)
	if err != nil {
		return err
	}
	err = s.identity.SetPasswordHashAndRevokeTokensIf(ctx, target, passwordHash, actor.Token, func(user store.User) error {
		changed = user
		if actor.UserID == user.ID {
			if update.Current == "" {
				return ErrForbidden
			}
			verified, err := passwordMatches(user.PasswordHash, update.Current)
			if err != nil {
				return err
			}
			if !verified {
				return ErrForbidden
			}
		} else if !actor.Administrator {
			return ErrForbidden
		}
		if passwordHash == "" {
			administrator, err := isAdministrator(user)
			if err != nil {
				return err
			}
			if administrator {
				return ErrAdministratorPasswordRequired
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.record(activitylog.UserPasswordChanged(changed.ID, changed.Name))
	return nil
}

// FirstUser returns the deterministic initialized user without exposing the
// password hash to callers that only need startup identity metadata.
func (s *Service) FirstUser(ctx context.Context) (store.User, error) {
	return s.identity.FirstUser(ctx)
}

// UpdateStartupUser atomically configures the initial blank-password account.
// State is checked before request validation so an established account always
// reports its terminal state rather than the loser request's malformed input.
// The store repeats that condition with the write, closing the read-to-write
// race without splitting the credential mutation.
func (s *Service) UpdateStartupUser(ctx context.Context, update StartupUserUpdate) error {
	first, err := s.identity.FirstUser(ctx)
	if err != nil {
		return err
	}
	if first.PasswordHash != "" {
		return ErrStartupUserConfigured
	}
	if whitespaceOnly(update.Password) {
		return ErrStartupPasswordRequired
	}
	if update.Name != nil && !ValidName(*update.Name) {
		return ErrInvalidUsername
	}
	passwordHash, err := s.hashPassword(update.Password)
	if err != nil {
		return err
	}
	storeUpdate := store.FirstUserUpdate{PasswordHash: passwordHash}
	if update.Name != nil {
		storeUpdate.Name = *update.Name
		storeUpdate.NormalizedName = NormalizeName(*update.Name)
		storeUpdate.Rename = true
	}
	if err := s.identity.ConfigureFirstUser(ctx, storeUpdate); err != nil {
		return err
	}
	// StartupController.UpdateStartupUser sets the password through
	// UserManager.ChangePassword, which writes the password-changed entry.
	name := first.Name
	if update.Name != nil {
		name = *update.Name
	}
	s.record(activitylog.UserPasswordChanged(first.ID, name))
	return nil
}

func (s *Service) Update(ctx context.Context, user store.User) error {
	if !ValidName(user.Name) {
		return ErrInvalidUsername
	}
	user.NormalizedName = NormalizeName(user.Name)
	return s.identity.UpdateUserIf(ctx, user, func(old store.User, users []store.User) error {
		wasAdmin, err := isAdministrator(old)
		if err != nil {
			return err
		}
		willBeAdmin, err := isAdministrator(user)
		if err != nil {
			return err
		}
		adminCount, err := administratorCount(users)
		if err != nil {
			return err
		}
		if !wasAdmin && willBeAdmin && old.PasswordHash == "" {
			return ErrAdministratorPasswordRequired
		}
		if wasAdmin && !willBeAdmin && adminCount <= 1 {
			return ErrLastAdministrator
		}
		return nil
	})
}

// UpdatePolicy validates a source UserPolicy document and persists it through
// the same invariant-preserving update path used by every user mutation.
func (s *Service) UpdatePolicy(ctx context.Context, userID dto.UUID, document json.RawMessage) error {
	var policy dto.UserPolicy
	if err := dto.DecodeUserPolicy(document, &policy); err != nil {
		return ErrBadRequest
	}
	account, err := s.identity.User(ctx, userID)
	if err != nil {
		return err
	}
	document, err = keepDeadfinPolicyFields(account.Policy, document)
	if err != nil {
		return ErrBadRequest
	}
	account.Policy = append(account.Policy[:0], document...)
	return s.Update(ctx, account)
}

// keepDeadfinPolicyFields copies each Deadfin-only UserPolicy member of
// previous that document omits into document. Stock clients do not send
// these members, so their saves keep the stored values (D-105).
func keepDeadfinPolicyFields(previous, document json.RawMessage) (json.RawMessage, error) {
	if len(previous) == 0 {
		return document, nil
	}
	var stored map[string]json.RawMessage
	if json.Unmarshal(previous, &stored) != nil {
		return document, nil
	}
	var next map[string]json.RawMessage
	if err := json.Unmarshal(document, &next); err != nil {
		return nil, err
	}
	changed := false
	for _, name := range []string{"LimitLiveTvToSharedLists"} {
		value, ok := stored[name]
		if _, sent := next[name]; ok && !sent {
			next[name] = value
			changed = true
		}
	}
	if !changed {
		return document, nil
	}
	return json.Marshal(next)
}

// UpdateUser ports UserController.UpdateUser: it authorizes like
// RequestHelpers.AssertCanUpdateUser(restrictUserPreferences: true), renames
// when name differs (UserManager.RenameUser), then replaces the configuration.
func (s *Service) UpdateUser(ctx context.Context, actor Actor, target dto.UUID, name *string, configuration json.RawMessage) error {
	if actor.UserID == dto.NilUUID {
		return ErrUnauthorized
	}
	account, err := s.identity.User(ctx, target)
	if err != nil {
		return err
	}
	policy, err := userPolicy(account)
	if err != nil {
		return err
	}
	if !actor.Administrator && (actor.UserID != account.ID || !policy.enableUserPreferenceAccess()) {
		return ErrForbidden
	}
	if name == nil || *name != account.Name {
		if name == nil || !ValidName(*name) {
			return ErrInvalidUsername
		}
		account.Name = *name
		if err := s.Update(ctx, account); err != nil {
			return err
		}
	}
	if len(configuration) == 0 || string(configuration) == "null" {
		return ErrBadRequest
	}
	return s.UpdateConfiguration(ctx, actor, target, configuration)
}

func (s *Service) Delete(ctx context.Context, id dto.UUID) error {
	var deleted store.User
	err := s.identity.DeleteUserIf(ctx, id, func(users []store.User, target store.User) error {
		deleted = target
		if len(users) <= 1 {
			return ErrLastUser
		}
		admin, err := isAdministrator(target)
		if err != nil {
			return err
		}
		adminCount, err := administratorCount(users)
		if err != nil {
			return err
		}
		if admin && adminCount <= 1 {
			return ErrLastAdministrator
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.record(activitylog.UserDeleted(deleted.Name))
	return nil
}

func (s *Service) ChangePassword(ctx context.Context, id dto.UUID, password string) error {
	passwordHash, err := s.hashPassword(password)
	if err != nil {
		return err
	}
	var changed store.User
	err = s.identity.SetPasswordHashIf(ctx, id, passwordHash, func(user store.User) error {
		changed = user
		admin, err := isAdministrator(user)
		if err != nil {
			return err
		}
		if whitespaceOnly(password) && admin {
			return ErrAdministratorPasswordRequired
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.record(activitylog.UserPasswordChanged(changed.ID, changed.Name))
	return nil
}

// VerifyPassword performs Jellyfin-compatible PBKDF2 verification. A verified
// legacy PBKDF2-SHA1 or non-default iteration hash is immediately rehashed using
// the current SHA512 default, matching DefaultAuthenticationProvider lines 82-88.
func (s *Service) VerifyPassword(ctx context.Context, id dto.UUID, password string) (bool, error) {
	user, err := s.identity.User(ctx, id)
	if err != nil {
		return false, err
	}
	if user.PasswordHash == "" {
		return password == "", nil
	}
	parsed, err := parsePasswordHash(user.PasswordHash)
	if err != nil {
		return false, err
	}
	verified, err := parsed.verify(password)
	if err != nil || !verified {
		return verified, err
	}
	if parsed.id != defaultHashID || parsed.iterations != defaultIterations {
		newHash, err := s.hashPassword(password)
		if err != nil {
			return false, err
		}
		if _, err := s.identity.CompareAndSwapPasswordHash(ctx, id, user.PasswordHash, newHash); err != nil {
			return false, err
		}
	}
	return true, nil
}

// RecordSuccessfulLogin persists the two user-level timestamps that upstream
// updates for an admitted user session authentication.
func (s *Service) RecordSuccessfulLogin(ctx context.Context, id dto.UUID) error {
	return s.identity.RecordSuccessfulLogin(ctx, id, s.clock.Now().UTC())
}

// RecordFailedLogin persists failed authentication accounting without issuing
// a session or token.
func (s *Service) RecordFailedLogin(ctx context.Context, id dto.UUID) error {
	lockedOut, err := s.identity.RecordFailedLogin(ctx, id)
	if err != nil || !lockedOut {
		return err
	}
	if locked, err := s.identity.User(ctx, id); err == nil {
		s.record(activitylog.UserLockedOut(locked.ID, locked.Name))
	}
	return nil
}

// IssueToken creates the device row that owns an authorization token. Existing
// rows for this user/device id are removed first, as SessionManager does before
// creating a new Device in GetAuthorizationToken.
func (s *Service) IssueToken(ctx context.Context, device store.Device) (store.Device, error) {
	if device.DeviceID == "" {
		return store.Device{}, errors.New("device ID is required")
	}
	token, err := s.newToken()
	if err != nil {
		return store.Device{}, err
	}
	now := s.clock.Now().UTC()
	device.AccessToken = token
	device.DateCreated = now
	device.DateModified = now
	device.DateLastActivity = now
	return s.identity.ReplaceDeviceToken(ctx, device)
}

func (s *Service) Token(ctx context.Context, token string) (store.Device, error) {
	return s.identity.DeviceByToken(ctx, token)
}

func (s *Service) Logout(ctx context.Context, token string) error {
	err := s.identity.DeleteToken(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// AuthorizeDevice enforces the stored device-access policy during login
// admission, before token replacement or session creation can occur.
func (s *Service) AuthorizeDevice(_ context.Context, account store.User, deviceID string) error {
	policy, err := userPolicy(account)
	if err != nil {
		return err
	}
	if !policy.allowsDevice(deviceID) {
		return ErrForbidden
	}
	return nil
}

// MaxActiveSessions resolves the persisted session limit for an account.
func (s *Service) MaxActiveSessions(ctx context.Context, userID dto.UUID) (int, error) {
	account, err := s.identity.User(ctx, userID)
	if err != nil {
		return 0, err
	}
	policy, err := userPolicy(account)
	if err != nil {
		return 0, err
	}
	return policy.MaxActiveSessions, nil
}

// FilterSessions applies the persisted caller, target-user, and device facts
// that govern the GET /Sessions projection. The session manager remains the
// registry owner; this service owns all user-policy interpretation.
func (s *Service) FilterSessions(ctx context.Context, actor Actor, controllable *dto.UUID, snapshots []session.Snapshot) ([]session.Snapshot, error) {
	if actor.APIKey {
		return filterSessionsForAPIKey(controllable, snapshots, s, ctx)
	}
	if actor.UserID == dto.NilUUID {
		return nil, ErrUnauthorized
	}
	account, err := s.identity.User(ctx, actor.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return []session.Snapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	policy, err := userPolicy(account)
	if err != nil {
		return nil, err
	}
	if controllable == nil {
		if policy.IsAdministrator {
			return snapshots, nil
		}
		return filterVisibleSessions(snapshots, actor.UserID), nil
	}

	target, err := s.identity.User(ctx, *controllable)
	if errors.Is(err, store.ErrNotFound) {
		return []session.Snapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	targetPolicy, err := userPolicy(target)
	if err != nil {
		return nil, err
	}
	canControlOthers := policy.IsAdministrator || policy.EnableRemoteControlOfOtherUsers
	if !canControlOthers && *controllable != actor.UserID {
		return nil, ErrForbidden
	}
	result := make([]session.Snapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if !snapshot.Capabilities.SupportsMediaControl {
			continue
		}
		if !targetPolicy.sharedDeviceControlEnabled() && snapshot.UserID == dto.NilUUID {
			continue
		}
		if !canControlOthers && !sessionContainsUser(snapshot, actor.UserID) {
			continue
		}
		if snapshot.DeviceID != "" && !policy.allowsDevice(snapshot.DeviceID) {
			continue
		}
		result = append(result, snapshot)
	}
	return result, nil
}

func filterSessionsForAPIKey(controllable *dto.UUID, snapshots []session.Snapshot, users *Service, ctx context.Context) ([]session.Snapshot, error) {
	if controllable == nil {
		return snapshots, nil
	}
	target, err := users.identity.User(ctx, *controllable)
	if errors.Is(err, store.ErrNotFound) {
		return []session.Snapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	policy, err := userPolicy(target)
	if err != nil {
		return nil, err
	}
	result := make([]session.Snapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if !snapshot.Capabilities.SupportsMediaControl || (!policy.sharedDeviceControlEnabled() && snapshot.UserID == dto.NilUUID) {
			continue
		}
		result = append(result, snapshot)
	}
	return result, nil
}

func filterVisibleSessions(snapshots []session.Snapshot, userID dto.UUID) []session.Snapshot {
	result := make([]session.Snapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if sessionContainsUser(snapshot, userID) {
			result = append(result, snapshot)
		}
	}
	return result
}

func sessionContainsUser(snapshot session.Snapshot, userID dto.UUID) bool {
	if snapshot.UserID == dto.NilUUID || snapshot.UserID == userID {
		return true
	}
	for _, additionalUserID := range snapshot.AdditionalUserIDs {
		if additionalUserID == userID {
			return true
		}
	}
	return false
}

func (s *Service) RevokeTokens(ctx context.Context, userID dto.UUID, currentToken string) error {
	return s.identity.RevokeUserTokens(ctx, userID, currentToken)
}

// RefreshDevice implements the mutable device fields and strictly-greater-than
// three-minute activity update that AuthorizationContext applies after a token
// lookup. Calling policy (including Chromecast suppression) stays in P1-03.
func (s *Service) RefreshDevice(ctx context.Context, device store.Device) (store.Device, bool, error) {
	return s.identity.UpsertDeviceActivity(ctx, device, activityStaleness, s.clock.Now().UTC())
}

// RefreshDeviceIfCurrent updates a device only if the lookup snapshot is still
// current, so authorization cannot recreate a logged-out token or overwrite a
// newer request's metadata.
func (s *Service) RefreshDeviceIfCurrent(ctx context.Context, device store.Device) (store.Device, bool, error) {
	return s.identity.RefreshDeviceIfCurrent(ctx, device, activityStaleness, s.clock.Now().UTC())
}

// NormalizeName uses the repository's pinned .NET invariant uppercase table.
func NormalizeName(name string) string { return item.InvariantUpper(name) }

// ValidName reproduces UserManager.ValidUsernameRegex and its explicit dot
// exclusions: no leading/trailing whitespace; Unicode letters/numbers and the
// upstream punctuation set are accepted.
func ValidName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.TrimSpace(name) != name {
		return false
	}
	for _, scalar := range name {
		if unicode.IsLetter(scalar) || unicode.Is(unicode.Mn, scalar) || unicode.IsDigit(scalar) || unicode.Is(unicode.Pc, scalar) || (item.IsInvariantLetterOrNumber(scalar) && !unicode.IsNumber(scalar)) {
			continue
		}
		switch scalar {
		case ' ', '-', '\'', '.', '_', '@', '+':
			continue
		default:
			return false
		}
	}
	return true
}

// initialUserName follows UserManager.InitializeAsync: use the process
// username when it is a valid Jellyfin username, otherwise use its stable
// fallback. USER and USERNAME let the process environment express the same
// identity deterministically on Unix and Windows respectively.
func initialUserName() string {
	name := os.Getenv("USER")
	if runtime.GOOS == "windows" {
		name = os.Getenv("USERNAME")
	}
	if !ValidName(name) {
		return initialUserFallbackName
	}
	return name
}

type policyFacts struct {
	IsAdministrator                 bool
	IsHidden                        bool
	IsDisabled                      bool
	EnableAllDevices                *bool
	EnabledDevices                  []string
	EnableRemoteAccess              *bool
	EnableUserPreferenceAccess      *bool
	EnableRemoteControlOfOtherUsers bool
	EnableSharedDeviceControl       *bool
	MaxActiveSessions               int
}

func userPolicy(user store.User) (policyFacts, error) {
	var policy policyFacts
	if len(user.Policy) == 0 {
		return policy, nil
	}
	if err := dto.DecodeUserPolicy(user.Policy, &policy); err != nil {
		return policyFacts{}, err
	}
	return policy, nil
}

func (policy policyFacts) enableUserPreferenceAccess() bool {
	return policy.EnableUserPreferenceAccess == nil || *policy.EnableUserPreferenceAccess
}

func (policy policyFacts) allowsDevice(deviceID string) bool {
	if policy.IsAdministrator || policy.EnableAllDevices == nil || *policy.EnableAllDevices {
		return true
	}
	for _, enabledDeviceID := range policy.EnabledDevices {
		if strings.EqualFold(enabledDeviceID, deviceID) {
			return true
		}
	}
	return false
}

func (policy policyFacts) sharedDeviceControlEnabled() bool {
	return policy.EnableSharedDeviceControl == nil || *policy.EnableSharedDeviceControl
}

func (policy policyFacts) visibleTo(actor Actor) bool {
	if actor.Remote && policy.EnableRemoteAccess != nil && !*policy.EnableRemoteAccess {
		return false
	}
	if actor.DeviceID == "" || policy.EnableAllDevices == nil || *policy.EnableAllDevices {
		return true
	}
	for _, deviceID := range policy.EnabledDevices {
		if strings.EqualFold(deviceID, actor.DeviceID) {
			return true
		}
	}
	return false
}

func userDTO(user store.User) UserDTO {
	return UserDTO{
		ID:               user.ID,
		Name:             user.Name,
		Policy:           json.RawMessage(user.Policy),
		Configuration:    json.RawMessage(user.Configuration),
		LastLoginDate:    user.LastLoginDate,
		LastActivityDate: user.LastActivityDate,
	}
}

func isAdministrator(user store.User) (bool, error) {
	policy, err := userPolicy(user)
	if err != nil {
		return false, err
	}
	return policy.IsAdministrator, nil
}

func passwordMatches(stored, password string) (bool, error) {
	if stored == "" {
		return password == "", nil
	}
	parsed, err := parsePasswordHash(stored)
	if err != nil {
		return false, err
	}
	return parsed.verify(password)
}

func administratorCount(users []store.User) (int, error) {
	count := 0
	for _, user := range users {
		admin, err := isAdministrator(user)
		if err != nil {
			return 0, err
		}
		if admin {
			count++
		}
	}
	return count, nil
}

func (s *Service) newUUID() (dto.UUID, error) {
	var id dto.UUID
	if _, err := io.ReadFull(s.random, id[:]); err != nil {
		return dto.UUID{}, fmt.Errorf("read identity randomness: %w", err)
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return id, nil
}

func (s *Service) newToken() (string, error) {
	id, err := s.newUUID()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func passwordEmpty(password string) bool { return password == "" }

func whitespaceOnly(password string) bool { return strings.TrimSpace(password) == "" }

func (s *Service) hashPassword(password string) (string, error) {
	if passwordEmpty(password) {
		return "", nil
	}
	salt, err := nonZeroBytes(s.random, defaultSaltLength)
	if err != nil {
		return "", err
	}
	digest := pbkdf2(sha512.New, []byte(password), salt, defaultIterations, defaultHashLength)
	return "$" + defaultHashID + "$iterations=" + strconv.Itoa(defaultIterations) + "$" + strings.ToUpper(hex.EncodeToString(salt)) + "$" + strings.ToUpper(hex.EncodeToString(digest)), nil
}

type passwordHash struct {
	id         string
	iterations int
	salt       []byte
	hash       []byte
}

func parsePasswordHash(value string) (passwordHash, error) {
	if value == "" || value[0] != '$' {
		return passwordHash{}, errors.New("password hash must start with a $")
	}
	parts := strings.Split(value[1:], "$")
	if len(parts) != 4 || parts[0] == "" || !strings.HasPrefix(parts[1], "iterations=") || parts[2] == "" || parts[3] == "" {
		return passwordHash{}, errors.New("malformed password hash")
	}
	iterations, err := strconv.Atoi(strings.TrimPrefix(parts[1], "iterations="))
	if err != nil || iterations <= 0 {
		return passwordHash{}, errors.New("password hash has invalid iterations")
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return passwordHash{}, fmt.Errorf("decode password salt: %w", err)
	}
	digest, err := hex.DecodeString(parts[3])
	if err != nil {
		return passwordHash{}, fmt.Errorf("decode password hash: %w", err)
	}
	expectedLength := 0
	switch parts[0] {
	case "PBKDF2":
		expectedLength = 32
	case defaultHashID:
		expectedLength = defaultHashLength
	default:
		return passwordHash{}, fmt.Errorf("unsupported password hash ID %q", parts[0])
	}
	if len(digest) != expectedLength {
		return passwordHash{}, fmt.Errorf("password hash ID %q has digest length %d, want %d", parts[0], len(digest), expectedLength)
	}
	return passwordHash{id: parts[0], iterations: iterations, salt: salt, hash: digest}, nil
}

func (p passwordHash) verify(password string) (bool, error) {
	var h func() hash.Hash
	switch p.id {
	case "PBKDF2":
		h = sha1.New
	case defaultHashID:
		h = sha512.New
	default:
		return false, fmt.Errorf("unsupported password hash ID %q", p.id)
	}
	derived := pbkdf2(h, []byte(password), p.salt, p.iterations, len(p.hash))
	return subtle.ConstantTimeCompare(derived, p.hash) == 1, nil
}

func nonZeroBytes(random io.Reader, size int) ([]byte, error) {
	result := make([]byte, size)
	for i := range result {
		for result[i] == 0 {
			if _, err := io.ReadFull(random, result[i:i+1]); err != nil {
				return nil, fmt.Errorf("read password salt randomness: %w", err)
			}
		}
	}
	return result, nil
}

// pbkdf2 is RFC 8018 PBKDF2. It is kept local to avoid making the password
// format depend on a package-global or version-varying external implementation.
func pbkdf2(newHash func() hash.Hash, password, salt []byte, iterations, size int) []byte {
	mac := hmac.New(newHash, password)
	hashSize := mac.Size()
	result := make([]byte, 0, ((size+hashSize-1)/hashSize)*hashSize)
	var counter [4]byte
	for block := 1; len(result) < size; block++ {
		counter[0] = byte(block >> 24)
		counter[1] = byte(block >> 16)
		counter[2] = byte(block >> 8)
		counter[3] = byte(block)
		mac.Reset()
		_, _ = mac.Write(salt)
		_, _ = mac.Write(counter[:])
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iterations; i++ {
			mac.Reset()
			_, _ = mac.Write(u)
			u = mac.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		result = append(result, t...)
	}
	return result[:size]
}
