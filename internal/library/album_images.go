// SPDX-License-Identifier: GPL-2.0-only

package library

import (
	"context"
	"fmt"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
)

// FolderImageCopier copies the image of a child into a folder
// (BaseDynamicImageProvider.CreateSingleImage). It reports false when the
// child has no local image of the type.
type FolderImageCopier interface {
	CopyFolderImage(ctx context.Context, source item.Item, target *item.Item, imageType item.ImageType) (bool, error)
}

// albumImageTask names one scanned MusicAlbum for the album image pass.
type albumImageTask struct {
	albumID            dto.UUID
	collectionFolderID dto.UUID
}

// refreshAlbumImages runs the album image pass for each scanned album, after
// the scan has stored the album subtree and the track images. Upstream
// MusicAlbumImageProvider runs as a custom metadata provider of the album.
func (s *Scanner) refreshAlbumImages(ctx context.Context, tasks []albumImageTask) []RefreshProgress {
	copier, ok := s.options.DynamicImages.(FolderImageCopier)
	if !ok {
		return nil
	}
	var failed []RefreshProgress
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			return failed
		}
		if err := s.refreshAlbumImage(ctx, copier, task); err != nil {
			failed = append(failed, RefreshProgress{ItemID: task.albumID, Status: "failed", Error: fmt.Errorf("album image: %w", err)})
		}
	}
	return failed
}

// refreshAlbumImage ports MusicAlbumImageProvider for an album without a
// Primary image (BaseDynamicImageProvider.HasChanged): it copies the Primary
// image of the first non-folder descendant that has one, in SortName order
// (BaseFolderImageProvider.GetItemsWithImages).
func (s *Scanner) refreshAlbumImage(ctx context.Context, copier FolderImageCopier, task albumImageTask) error {
	loaded, err := s.items.GetBatch(ctx, []dto.UUID{task.albumID})
	if err != nil {
		return err
	}
	if len(loaded) == 0 {
		return nil
	}
	album := loaded[0]
	if album.Kind != item.KindMusicAlbum || hasImage(album.ImageInfos, item.ImageTypePrimary) {
		return nil
	}
	notFolder, limit := false, 1
	children, err := s.items.Query(ctx, store.ItemQuery{
		ParentID:   &album.ID,
		Recursive:  true,
		IsFolder:   &notFolder,
		ImageTypes: []item.ImageType{item.ImageTypePrimary},
		SortBy:     []store.ItemSortBy{store.ItemSortBySortName},
		Limit:      &limit,
	})
	if err != nil {
		return err
	}
	if len(children.Items) == 0 {
		return nil
	}
	copied, err := copier.CopyFolderImage(ctx, children.Items[0], &album, item.ImageTypePrimary)
	if err != nil || !copied {
		return err
	}
	saved := scannerLifecycleTimestamp(s.options.Now)
	album.DateLastSaved = &saved
	if err := s.storeWrite(ctx, func() error { return s.items.EditItems(ctx, []item.Item{album}, nil) }); err != nil {
		return err
	}
	if album.ParentID != nil {
		s.changes.publish(*album.ParentID, task.collectionFolderID, store.ReconcileChildrenResult{Updated: []dto.UUID{album.ID}})
	}
	return nil
}

func hasImage(images []item.ImageInfo, imageType item.ImageType) bool {
	for _, image := range images {
		if image.Type == imageType {
			return true
		}
	}
	return false
}
