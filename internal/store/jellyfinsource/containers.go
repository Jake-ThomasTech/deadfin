// SPDX-License-Identifier: GPL-2.0-only

package jellyfinsource

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
)

// Jellyfin type names of the linked-children containers the importer reads.
const (
	PlaylistType = "MediaBrowser.Controller.Playlists.Playlist"
	BoxSetType   = "MediaBrowser.Controller.Entities.Movies.BoxSet"
)

// Share is one user's share of a Jellyfin playlist.
type Share struct {
	UserID  dto.UUID
	CanEdit bool
}

// Container is one Jellyfin playlist or collection with its ordered
// entries. ChildPaths holds each resolvable entry's media path, in order.
type Container struct {
	ID          dto.UUID
	Name        string
	Path        string
	DateCreated *time.Time
	OwnerUserID dto.UUID
	OpenAccess  bool
	Shares      []Share
	// MediaType is the playlist's PlaylistMediaType name, empty if unset.
	MediaType  string
	ChildPaths []string
	// UnresolvedChildren counts entries whose item has no path in Jellyfin.
	UnresolvedChildren int
	// ProviderIDs holds the item's BaseItemProviders rows by provider name.
	ProviderIDs map[string]string
	// Images holds the item's BaseItemImageInfos rows, in Jellyfin order.
	Images []Image
}

// Image is one Jellyfin image of an item. Type is the ImageInfoImageType
// value, which is also the ImageType value. Path is a file path, with
// Jellyfin's %AppDataPath% and %MetadataPath% expanded, or a URL.
type Image struct {
	Type int
	Path string
}

// containerData is the part of BaseItems.Data the importer needs. Jellyfin
// 10.11 serializes these Playlist and Folder members into the JSON column.
type containerData struct {
	OwnerUserID       string            `json:"OwnerUserId"`
	OpenAccess        bool              `json:"OpenAccess"`
	Shares            []containerShare  `json:"Shares"`
	PlaylistMediaType json.RawMessage   `json:"PlaylistMediaType"`
	LinkedChildren    []containerLinked `json:"LinkedChildren"`
}

type containerShare struct {
	UserID  string `json:"UserId"`
	CanEdit bool   `json:"CanEdit"`
}

type containerLinked struct {
	Path          string          `json:"Path"`
	ItemID        string          `json:"ItemId"`
	LibraryItemID string          `json:"LibraryItemId"`
	Type          json.RawMessage `json:"Type"`
}

// Containers returns every item of the Jellyfin type typeName with its
// Manual entries. Entries come from the LinkedChildren table when the
// database has one (Jellyfin after MigrateLinkedChildren), else from the
// Data JSON.
func (s *Source) Containers(ctx context.Context, typeName string) ([]Container, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT Id, COALESCE(Name,''), COALESCE(Path,''), COALESCE(Data,''), DateCreated FROM BaseItems WHERE Type = ? ORDER BY Name, Id`, typeName)
	if err != nil {
		return nil, fmt.Errorf("jellyfinsource: read %s items: %w", typeName, err)
	}
	type raw struct {
		container Container
		data      string
	}
	var found []raw
	for rows.Next() {
		var id, data string
		var created sql.NullString
		var c Container
		if err := rows.Scan(&id, &c.Name, &c.Path, &data, &created); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("jellyfinsource: scan %s item: %w", typeName, err)
		}
		if c.ID, err = dto.ParseUUID(id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("jellyfinsource: %s item %q: %w", typeName, c.Name, err)
		}
		c.DateCreated = parseOptionalTime(created)
		found = append(found, raw{container: c, data: data})
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	linkedTable, err := s.hasTable(ctx, "LinkedChildren")
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(found))
	for _, entry := range found {
		c := entry.container
		var data containerData
		if strings.TrimSpace(entry.data) != "" {
			if err := json.Unmarshal([]byte(entry.data), &data); err != nil {
				return nil, fmt.Errorf("jellyfinsource: %s item %q data: %w", typeName, c.Name, err)
			}
		}
		if owner, err := dto.ParseUUID(data.OwnerUserID); err == nil {
			c.OwnerUserID = owner
		}
		c.OpenAccess = data.OpenAccess
		for _, share := range data.Shares {
			if userID, err := dto.ParseUUID(share.UserID); err == nil {
				c.Shares = append(c.Shares, Share{UserID: userID, CanEdit: share.CanEdit})
			}
		}
		c.MediaType = enumName(data.PlaylistMediaType, []string{"Unknown", "Video", "Audio", "Photo", "Book"})
		if linkedTable {
			err = s.tableChildren(ctx, &c)
		} else {
			err = s.dataChildren(ctx, &c, data.LinkedChildren)
		}
		if err != nil {
			return nil, err
		}
		if err := s.itemProviders(ctx, &c); err != nil {
			return nil, err
		}
		if err := s.itemImages(ctx, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// itemProviders reads the BaseItemProviders rows of c. A database without
// the table has no provider IDs.
func (s *Source) itemProviders(ctx context.Context, c *Container) error {
	exists, err := s.hasTable(ctx, "BaseItemProviders")
	if err != nil || !exists {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ProviderId, ProviderValue FROM BaseItemProviders WHERE ItemId = ? ORDER BY ProviderId`, sourceGUID(c.ID))
	if err != nil {
		return fmt.Errorf("jellyfinsource: read provider ids of %q: %w", c.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return fmt.Errorf("jellyfinsource: scan provider id of %q: %w", c.Name, err)
		}
		if strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		if c.ProviderIDs == nil {
			c.ProviderIDs = map[string]string{}
		}
		c.ProviderIDs[name] = value
	}
	return rows.Err()
}

// itemImages reads the BaseItemImageInfos rows of c. A database without the
// table has no images.
func (s *Source) itemImages(ctx context.Context, c *Container) error {
	exists, err := s.hasTable(ctx, "BaseItemImageInfos")
	if err != nil || !exists {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ImageType, COALESCE(Path,'') FROM BaseItemImageInfos WHERE ItemId = ? ORDER BY ImageType, Id`, sourceGUID(c.ID))
	if err != nil {
		return fmt.Errorf("jellyfinsource: read images of %q: %w", c.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var image Image
		if err := rows.Scan(&image.Type, &image.Path); err != nil {
			return fmt.Errorf("jellyfinsource: scan image of %q: %w", c.Name, err)
		}
		if strings.TrimSpace(image.Path) == "" {
			continue
		}
		image.Path = s.expandVirtualPath(image.Path)
		c.Images = append(c.Images, image)
	}
	return rows.Err()
}

// expandVirtualPath ports IServerApplicationHost.ExpandVirtualPath for the
// %AppDataPath% and %MetadataPath% prefixes. Jellyfin keeps jellyfin.db in
// its data directory, and the metadata directory is a sibling of it, so the
// database copy must sit in a copy of the data directory for the paths to
// resolve.
func (s *Source) expandVirtualPath(path string) string {
	data := filepath.Dir(s.path)
	for prefix, root := range map[string]string{
		"%AppDataPath%":  data,
		"%MetadataPath%": filepath.Join(filepath.Dir(data), "metadata"),
	} {
		if rest, ok := strings.CutPrefix(path, prefix); ok {
			return filepath.Join(root, filepath.FromSlash(rest))
		}
	}
	return path
}

func (s *Source) hasTable(ctx context.Context, name string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count); err != nil {
		return false, fmt.Errorf("jellyfinsource: inspect schema: %w", err)
	}
	return count > 0, nil
}

// tableChildren reads Manual (ChildType 0) entries from LinkedChildren.
func (s *Source) tableChildren(ctx context.Context, c *Container) error {
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(b.Path,'') FROM LinkedChildren l LEFT JOIN BaseItems b ON b.Id = l.ChildId
		WHERE l.ParentId = ? AND l.ChildType = 0 ORDER BY l.SortOrder IS NULL, l.SortOrder, l.ChildId`, sourceGUID(c.ID))
	if err != nil {
		return fmt.Errorf("jellyfinsource: read linked children of %q: %w", c.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return fmt.Errorf("jellyfinsource: scan linked child of %q: %w", c.Name, err)
		}
		c.addChild(path)
	}
	return rows.Err()
}

// dataChildren resolves Data JSON entries the way MigrateLinkedChildren
// does: ItemId, else Path, else LibraryItemId.
func (s *Source) dataChildren(ctx context.Context, c *Container, linked []containerLinked) error {
	for _, child := range linked {
		if name := enumName(child.Type, []string{"Manual", "Shortcut", "LocalAlternateVersion", "LinkedAlternateVersion"}); name != "" && name != "Manual" {
			continue
		}
		path := ""
		for _, candidate := range []string{child.ItemID, child.LibraryItemID} {
			id, err := dto.ParseUUID(candidate)
			if err != nil || id.IsNil() {
				continue
			}
			if path, err = s.itemPath(ctx, id); err != nil {
				return err
			}
			if path != "" {
				break
			}
		}
		if path == "" {
			path = child.Path
		}
		c.addChild(path)
	}
	return nil
}

func (c *Container) addChild(path string) {
	if strings.TrimSpace(path) == "" {
		c.UnresolvedChildren++
		return
	}
	c.ChildPaths = append(c.ChildPaths, path)
}

func (s *Source) itemPath(ctx context.Context, id dto.UUID) (string, error) {
	var path sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT Path FROM BaseItems WHERE Id = ?`, sourceGUID(id)).Scan(&path)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("jellyfinsource: read item %s path: %w", id, err)
	}
	return path.String, nil
}

// sourceGUID formats id the way EF Core stores a Guid in SQLite: uppercase
// with dashes.
func sourceGUID(id dto.UUID) string { return strings.ToUpper(id.Dashed()) }

// enumName reads a System.Text.Json enum written as a name or a number.
func enumName(raw json.RawMessage, names []string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		return name
	}
	index, err := strconv.Atoi(string(raw))
	if err != nil || index < 0 || index >= len(names) {
		return ""
	}
	return names[index]
}
