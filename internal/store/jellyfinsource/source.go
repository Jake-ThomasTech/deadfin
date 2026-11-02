// SPDX-License-Identifier: GPL-2.0-only

// Package jellyfinsource reads identity and user state from a stock Jellyfin
// database (jellyfin.db, Jellyfin 10.11 and later) for the one-way importer
// (D3, docs/02-storage.md §Importer). It opens the database read-only and
// never writes it: the untouched original is the rollback.
package jellyfinsource

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"
)

// PlaceholderItemID is the item Jellyfin attaches user data to when the
// original item no longer exists (the 10.11 retention migration).
var PlaceholderItemID = dto.UUID{15: 1}

// Source is an open, read-only Jellyfin database. path is the database file;
// it locates Jellyfin's data and metadata directories for virtual paths.
type Source struct {
	db   *sql.DB
	path string
}

// Open opens path read-only. The import guide requires a copy made while
// Jellyfin is stopped, so the file does not change during the import. Open
// therefore opens it immutable: SQLite then creates no -shm or -wal file,
// and a Jellyfin database in WAL mode opens from a read-only directory
// (deadfin#697). A non-empty -wal file holds committed changes that an
// immutable open would not see, so Open refuses it.
func Open(ctx context.Context, path string) (*Source, error) {
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() > 0 {
		return nil, fmt.Errorf("jellyfinsource: open %s: %s-wal is not empty; stop Jellyfin and copy the database again", path, filepath.Base(path))
	}
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?mode=ro&immutable=1&_pragma=query_only(1)")
	if err != nil {
		return nil, fmt.Errorf("jellyfinsource: open %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("jellyfinsource: open %s: %w", path, err)
	}
	return &Source{db: db, path: path}, nil
}

// Close closes the database.
func (s *Source) Close() error { return s.db.Close() }

// User is one Jellyfin user with its permission and preference rows.
type User struct {
	ID                         dto.UUID
	Username                   string
	PasswordHash               string
	AuthenticationProviderID   string
	PasswordResetProviderID    string
	AudioLanguagePreference    string
	SubtitleLanguagePreference string
	SubtitleMode               int
	CastReceiverID             string
	DisplayCollectionsView     bool
	DisplayMissingEpisodes     bool
	EnableLocalPassword        bool
	EnableNextEpisodeAutoPlay  bool
	EnableUserPreferenceAccess bool
	HidePlayedInLatest         bool
	PlayDefaultAudioTrack      bool
	RememberAudioSelections    bool
	RememberSubtitleSelections bool
	InvalidLoginAttemptCount   int
	LoginAttemptsBeforeLockout *int
	MaxActiveSessions          int
	MaxParentalRatingScore     *int
	MaxParentalRatingSubScore  *int
	RemoteClientBitrateLimit   *int
	SyncPlayAccess             int
	LastLoginDate              *time.Time
	LastActivityDate           *time.Time
	// Permissions maps Jellyfin PermissionKind to its value.
	Permissions map[int]bool
	// Preferences maps Jellyfin PreferenceKind to its delimited value.
	Preferences map[int]string
}

// Users returns every user with permissions and preferences.
func (s *Source) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT Id, Username, COALESCE(Password,''), AuthenticationProviderId, PasswordResetProviderId,
		COALESCE(AudioLanguagePreference,''), COALESCE(SubtitleLanguagePreference,''), SubtitleMode, COALESCE(CastReceiverId,''),
		DisplayCollectionsView, DisplayMissingEpisodes, EnableLocalPassword, EnableNextEpisodeAutoPlay, EnableUserPreferenceAccess,
		HidePlayedInLatest, PlayDefaultAudioTrack, RememberAudioSelections, RememberSubtitleSelections,
		InvalidLoginAttemptCount, LoginAttemptsBeforeLockout, MaxActiveSessions, MaxParentalRatingScore, MaxParentalRatingSubScore,
		RemoteClientBitrateLimit, SyncPlayAccess, LastLoginDate, LastActivityDate
		FROM Users ORDER BY InternalId`)
	if err != nil {
		return nil, fmt.Errorf("jellyfinsource: read users: %w", err)
	}
	defer rows.Close()
	var users []User
	byID := map[dto.UUID]int{}
	for rows.Next() {
		var u User
		var id string
		var lockout, rating, subRating, bitrate sql.NullInt64
		var lastLogin, lastActivity sql.NullString
		if err := rows.Scan(&id, &u.Username, &u.PasswordHash, &u.AuthenticationProviderID, &u.PasswordResetProviderID,
			&u.AudioLanguagePreference, &u.SubtitleLanguagePreference, &u.SubtitleMode, &u.CastReceiverID,
			&u.DisplayCollectionsView, &u.DisplayMissingEpisodes, &u.EnableLocalPassword, &u.EnableNextEpisodeAutoPlay, &u.EnableUserPreferenceAccess,
			&u.HidePlayedInLatest, &u.PlayDefaultAudioTrack, &u.RememberAudioSelections, &u.RememberSubtitleSelections,
			&u.InvalidLoginAttemptCount, &lockout, &u.MaxActiveSessions, &rating, &subRating,
			&bitrate, &u.SyncPlayAccess, &lastLogin, &lastActivity); err != nil {
			return nil, fmt.Errorf("jellyfinsource: scan user: %w", err)
		}
		if u.ID, err = dto.ParseUUID(id); err != nil {
			return nil, fmt.Errorf("jellyfinsource: user %q: %w", u.Username, err)
		}
		u.LoginAttemptsBeforeLockout = nullInt(lockout)
		u.MaxParentalRatingScore = nullInt(rating)
		u.MaxParentalRatingSubScore = nullInt(subRating)
		u.RemoteClientBitrateLimit = nullInt(bitrate)
		u.LastLoginDate = parseOptionalTime(lastLogin)
		u.LastActivityDate = parseOptionalTime(lastActivity)
		u.Permissions = map[int]bool{}
		u.Preferences = map[int]string{}
		byID[u.ID] = len(users)
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jellyfinsource: read users: %w", err)
	}

	perms, err := s.db.QueryContext(ctx, `SELECT UserId, Kind, Value FROM Permissions`)
	if err != nil {
		return nil, fmt.Errorf("jellyfinsource: read permissions: %w", err)
	}
	defer perms.Close()
	for perms.Next() {
		var id string
		var kind int
		var value bool
		if err := perms.Scan(&id, &kind, &value); err != nil {
			return nil, fmt.Errorf("jellyfinsource: scan permission: %w", err)
		}
		if index, ok := lookupUser(byID, id); ok {
			users[index].Permissions[kind] = value
		}
	}
	if err := perms.Err(); err != nil {
		return nil, fmt.Errorf("jellyfinsource: read permissions: %w", err)
	}

	prefs, err := s.db.QueryContext(ctx, `SELECT UserId, Kind, Value FROM Preferences`)
	if err != nil {
		return nil, fmt.Errorf("jellyfinsource: read preferences: %w", err)
	}
	defer prefs.Close()
	for prefs.Next() {
		var id, value string
		var kind int
		if err := prefs.Scan(&id, &kind, &value); err != nil {
			return nil, fmt.Errorf("jellyfinsource: scan preference: %w", err)
		}
		if index, ok := lookupUser(byID, id); ok {
			users[index].Preferences[kind] = value
		}
	}
	if err := prefs.Err(); err != nil {
		return nil, fmt.Errorf("jellyfinsource: read preferences: %w", err)
	}
	return users, nil
}

// Device is one device session (an access token).
type Device struct {
	UserID           dto.UUID
	AccessToken      string
	AppName          string
	AppVersion       string
	DeviceName       string
	DeviceID         string
	DateCreated      time.Time
	DateModified     time.Time
	DateLastActivity time.Time
}

// Devices returns every device session. Jellyfin does not maintain IsActive.
func (s *Source) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT UserId, AccessToken, AppName, AppVersion, DeviceName, DeviceId, DateCreated, DateModified, DateLastActivity
		FROM Devices ORDER BY Id`)
	if err != nil {
		return nil, fmt.Errorf("jellyfinsource: read devices: %w", err)
	}
	defer rows.Close()
	var devices []Device
	for rows.Next() {
		var d Device
		var userID, created, modified, activity string
		if err := rows.Scan(&userID, &d.AccessToken, &d.AppName, &d.AppVersion, &d.DeviceName, &d.DeviceID, &created, &modified, &activity); err != nil {
			return nil, fmt.Errorf("jellyfinsource: scan device: %w", err)
		}
		if d.UserID, err = dto.ParseUUID(userID); err != nil {
			return nil, fmt.Errorf("jellyfinsource: device %q: %w", d.DeviceName, err)
		}
		d.DateCreated, d.DateModified, d.DateLastActivity = parseTime(created), parseTime(modified), parseTime(activity)
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

// APIKey is one server API key.
type APIKey struct {
	Name             string
	AccessToken      string
	DateCreated      time.Time
	DateLastActivity time.Time
}

// APIKeys returns the server API keys.
func (s *Source) APIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT Name, AccessToken, DateCreated, DateLastActivity FROM ApiKeys ORDER BY Id`)
	if err != nil {
		return nil, fmt.Errorf("jellyfinsource: read api keys: %w", err)
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		var k APIKey
		var created, activity string
		if err := rows.Scan(&k.Name, &k.AccessToken, &created, &activity); err != nil {
			return nil, fmt.Errorf("jellyfinsource: scan api key: %w", err)
		}
		k.DateCreated, k.DateLastActivity = parseTime(created), parseTime(activity)
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// UserData is one per-user item state row with the facts of its item.
type UserData struct {
	UserID                dto.UUID
	ItemID                dto.UUID
	CustomDataKey         string
	Played                bool
	PlayCount             int
	PlaybackPositionTicks int64
	IsFavorite            bool
	Rating                *float64
	Likes                 *bool
	LastPlayedDate        *time.Time
	AudioStreamIndex      *int
	SubtitleStreamIndex   *int
	// ItemPath and ItemType are empty for a placeholder (orphaned) row.
	ItemPath string
	ItemType string
}

// Orphaned reports whether the row's item no longer exists in Jellyfin.
func (u UserData) Orphaned() bool { return u.ItemID == PlaceholderItemID }

// UserData returns every user data row.
func (s *Source) UserData(ctx context.Context) ([]UserData, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.UserId, u.ItemId, u.CustomDataKey, u.Played, u.PlayCount, u.PlaybackPositionTicks, u.IsFavorite,
		u.Rating, u.Likes, u.LastPlayedDate, u.AudioStreamIndex, u.SubtitleStreamIndex, COALESCE(b.Path,''), COALESCE(b.Type,'')
		FROM UserData u LEFT JOIN BaseItems b ON b.Id = u.ItemId ORDER BY u.UserId, u.CustomDataKey`)
	if err != nil {
		return nil, fmt.Errorf("jellyfinsource: read user data: %w", err)
	}
	defer rows.Close()
	var out []UserData
	for rows.Next() {
		var d UserData
		var userID, itemID string
		var rating sql.NullFloat64
		var likes sql.NullBool
		var lastPlayed sql.NullString
		var audio, subtitle sql.NullInt64
		if err := rows.Scan(&userID, &itemID, &d.CustomDataKey, &d.Played, &d.PlayCount, &d.PlaybackPositionTicks, &d.IsFavorite,
			&rating, &likes, &lastPlayed, &audio, &subtitle, &d.ItemPath, &d.ItemType); err != nil {
			return nil, fmt.Errorf("jellyfinsource: scan user data: %w", err)
		}
		if d.UserID, err = dto.ParseUUID(userID); err != nil {
			return nil, fmt.Errorf("jellyfinsource: user data user: %w", err)
		}
		if d.ItemID, err = dto.ParseUUID(itemID); err != nil {
			return nil, fmt.Errorf("jellyfinsource: user data item: %w", err)
		}
		if rating.Valid {
			value := rating.Float64
			d.Rating = &value
		}
		if likes.Valid {
			value := likes.Bool
			d.Likes = &value
		}
		d.LastPlayedDate = parseOptionalTime(lastPlayed)
		d.AudioStreamIndex = nullInt(audio)
		d.SubtitleStreamIndex = nullInt(subtitle)
		if d.ItemType == "PLACEHOLDER" {
			d.ItemPath, d.ItemType = "", ""
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Library is one Jellyfin library (CollectionFolder).
type Library struct {
	ID   dto.UUID
	Name string
}

// Libraries returns the Jellyfin libraries, so folder ids in user policies can
// be mapped to Deadfin libraries by name.
func (s *Source) Libraries(ctx context.Context) ([]Library, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT Id, COALESCE(Name,'') FROM BaseItems WHERE Type = 'MediaBrowser.Controller.Entities.CollectionFolder' ORDER BY Name`)
	if err != nil {
		return nil, fmt.Errorf("jellyfinsource: read libraries: %w", err)
	}
	defer rows.Close()
	var out []Library
	for rows.Next() {
		var l Library
		var id string
		if err := rows.Scan(&id, &l.Name); err != nil {
			return nil, fmt.Errorf("jellyfinsource: scan library: %w", err)
		}
		if l.ID, err = dto.ParseUUID(id); err != nil {
			return nil, fmt.Errorf("jellyfinsource: library %q: %w", l.Name, err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func lookupUser(byID map[dto.UUID]int, raw string) (int, bool) {
	id, err := dto.ParseUUID(raw)
	if err != nil {
		return 0, false
	}
	index, ok := byID[id]
	return index, ok
}

func nullInt(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	v := int(value.Int64)
	return &v
}

// EF Core stores DateTime as TEXT in UTC without an offset.
var timeLayouts = []string{"2006-01-02 15:04:05.9999999", "2006-01-02T15:04:05.9999999", "2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00"}

func parseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range timeLayouts {
		if parsed, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func parseOptionalTime(value sql.NullString) *time.Time {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	parsed := parseTime(value.String)
	if parsed.IsZero() {
		return nil
	}
	return &parsed
}
