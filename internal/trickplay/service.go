// SPDX-License-Identifier: GPL-2.0-only

package trickplay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store"
	"golang.org/x/sys/unix"
)

var (
	// ErrNotFound reports a missing trickplay rendition or tile artifact.
	ErrNotFound = errors.New("trickplay: not found")
	// ErrInvalidRequest reports an invalid reader configuration, request, or
	// persisted rendition description.
	ErrInvalidRequest = errors.New("trickplay: invalid request")
	// ErrUnsafeArtifact reports a trickplay path that cannot be opened safely
	// beneath the configured root.
	ErrUnsafeArtifact = errors.New("trickplay: unsafe artifact")
)

// MetadataSource resolves exactly one persisted trickplay rendition.
type MetadataSource interface {
	Trickplay(context.Context, dto.UUID, int) (Metadata, error)
}

// Access determines whether an actor may discover a stored media source.
// Returning false deliberately maps to a missing trickplay resource.
type Access interface {
	Visible(context.Context, dto.UUID, dto.UUID, bool) (bool, error)
}

// MediaPaths resolves the media file of an item.
type MediaPaths interface {
	MediaPath(context.Context, dto.UUID) (string, error)
}

// ReaderServiceConfig supplies the persisted-metadata source and the owned
// trickplay artifact root. Root must be absolute so artifact lookup cannot be
// affected by the process working directory. Media, when set, lets the reader
// serve a rendition whose tiles are saved with the media (see MediaDirectory)
// when the root has none.
type ReaderServiceConfig struct {
	Metadata    MetadataSource
	Access      Access
	Publication *PublicationCoordinator
	Root        string
	Media       MediaPaths
}

// ReadRequest is the transport-independent identity of a trickplay read.
// MediaSourceID, when supplied, owns both metadata and artifact lookup.
type ReadRequest struct {
	ItemID        dto.UUID
	MediaSourceID *dto.UUID
	UserID        dto.UUID
	Administrator bool
	Width         int
	APIKey        string
}

// Artifact is an opened trickplay tile. Body is seekable and caller-owned;
// Size and Modified describe that exact opened descriptor.
type Artifact struct {
	Body            io.ReadSeekCloser
	ContentType     string
	Size            int64
	Modified        time.Time
	GenerationToken string
}

// ReaderService reads persisted trickplay playlists and tiles from its owned
// artifact root.
type ReaderService struct {
	metadata    MetadataSource
	access      Access
	publication *PublicationCoordinator
	media       MediaPaths
	root        string
	rootMu      sync.RWMutex
	rootFD      int
	rootDevice  uint64
	rootInode   uint64
	configErr   error
	closeErr    error
}

// NewReaderService constructs a reader. Invalid configuration is retained and
// reported by operations so construction remains usable by application wiring.
func NewReaderService(config ReaderServiceConfig) *ReaderService {
	service := &ReaderService{metadata: config.Metadata, access: config.Access, publication: config.Publication, media: config.Media, root: config.Root, rootFD: -1}
	if readerNilMetadataSource(config.Metadata) {
		service.configErr = fmt.Errorf("%w: metadata source is required", ErrInvalidRequest)
	} else if readerNilAccess(config.Access) || config.Publication == nil {
		service.configErr = ErrNotFound
	} else if config.Root == "" || !filepath.IsAbs(config.Root) {
		service.configErr = fmt.Errorf("%w: absolute artifact root is required", ErrInvalidRequest)
	} else {
		root, rootInfo, err := readerOpenRoot(config.Root)
		if err != nil {
			service.configErr = err
		} else {
			service.rootFD = root
			service.rootDevice = uint64(rootInfo.Dev)
			service.rootInode = uint64(rootInfo.Ino)
		}
	}
	return service
}

// Close releases the root descriptor retained from construction. It is safe to
// call multiple times, including on an invalid or nil service.
func (service *ReaderService) Close() error {
	if service == nil {
		return nil
	}
	service.rootMu.Lock()
	defer service.rootMu.Unlock()
	if service.rootFD < 0 {
		return service.closeErr
	}
	rootFD := service.rootFD
	service.rootFD = -1
	service.closeErr = unix.Close(rootFD)
	return service.closeErr
}

func readerNilMetadataSource(source MetadataSource) bool {
	if source == nil {
		return true
	}
	value := reflect.ValueOf(source)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func readerNilAccess(access Access) bool {
	if access == nil {
		return true
	}
	value := reflect.ValueOf(access)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (service *ReaderService) readerConfigError() error {
	if service == nil {
		return fmt.Errorf("%w: reader service is required", ErrInvalidRequest)
	}
	if service.configErr != nil {
		return service.configErr
	}
	service.rootMu.RLock()
	closed := service.rootFD < 0
	service.rootMu.RUnlock()
	if closed {
		return fmt.Errorf("%w: reader service is closed", ErrInvalidRequest)
	}
	return nil
}

// Playlist renders the tiles playlist for one requested rendition. An absent
// or empty rendition has no playlist and is not an operation error.
func (service *ReaderService) Playlist(ctx context.Context, request ReadRequest) (string, bool, error) {
	if err := readerContextError(ctx); err != nil {
		return "", false, err
	}
	if err := service.readerConfigError(); err != nil {
		return "", false, err
	}
	service.publication.RLock()
	defer service.publication.RUnlock()
	metadata, err := service.metadataFor(ctx, request)
	if err != nil {
		return "", false, err
	}
	if err := readerContextError(ctx); err != nil {
		return "", false, err
	}
	if err := service.validateRootIdentity(); err != nil {
		return "", false, err
	}
	playlist, found := (Playlist{Metadata: metadata, APIKey: request.APIKey}).Render()
	return playlist, found, nil
}

// Tile opens the exact, regular JPEG tile for one requested rendition. It
// never reads the artifact; the returned body remains caller-owned.
func (service *ReaderService) Tile(ctx context.Context, request ReadRequest, index int) (Artifact, error) {
	if err := readerContextError(ctx); err != nil {
		return Artifact{}, err
	}
	if index < 0 {
		return Artifact{}, ErrNotFound
	}
	if err := service.readerConfigError(); err != nil {
		return Artifact{}, err
	}
	service.publication.RLock()
	defer service.publication.RUnlock()
	for attempt := 0; attempt != 2; attempt++ {
		metadata, err := service.metadataFor(ctx, request)
		if err != nil {
			return Artifact{}, err
		}
		if metadata.ThumbnailCount <= 0 {
			return Artifact{}, ErrNotFound
		}
		capacity, err := tileCapacity(metadata.TileWidth, metadata.TileHeight)
		if err != nil {
			return Artifact{}, fmt.Errorf("%w: invalid tile dimensions", ErrInvalidRequest)
		}
		if index >= ceilingDivide(metadata.ThumbnailCount, capacity) {
			return Artifact{}, ErrNotFound
		}
		if err := readerContextError(ctx); err != nil {
			return Artifact{}, err
		}

		file, err := service.openTile(metadata, index)
		if errors.Is(err, ErrNotFound) && service.media != nil {
			file, err = service.openMediaTile(ctx, metadata, index)
		}
		if err == nil {
			artifact, err := readerArtifact(file, metadata.GenerationToken)
			if err == nil {
				return artifact, nil
			}
			return Artifact{}, err
		}
		if !errors.Is(err, ErrNotFound) || attempt == 1 {
			return Artifact{}, err
		}
	}
	return Artifact{}, ErrNotFound
}

func (service *ReaderService) metadataFor(ctx context.Context, request ReadRequest) (Metadata, error) {
	if err := service.readerConfigError(); err != nil {
		return Metadata{}, err
	}
	if request.Width <= 0 {
		return Metadata{}, ErrNotFound
	}
	itemID := request.ItemID
	if request.MediaSourceID != nil {
		itemID = *request.MediaSourceID
	}
	if itemID.IsNil() {
		return Metadata{}, ErrNotFound
	}
	if err := readerContextError(ctx); err != nil {
		return Metadata{}, err
	}
	if service.access != nil {
		visible, err := service.access.Visible(ctx, request.UserID, itemID, request.Administrator)
		if err != nil {
			return Metadata{}, err
		}
		if !visible {
			return Metadata{}, ErrNotFound
		}
	}

	metadata, err := service.metadata.Trickplay(ctx, itemID, request.Width)
	if err != nil {
		if errors.Is(err, ErrNotFound) || store.IsNotFound(err) {
			return Metadata{}, ErrNotFound
		}
		return Metadata{}, err
	}
	if metadata.ItemID != itemID || metadata.Width != request.Width {
		return Metadata{}, fmt.Errorf("%w: metadata does not match requested rendition", ErrInvalidRequest)
	}
	return metadata, nil
}

func (service *ReaderService) openTile(metadata Metadata, index int) (*os.File, error) {
	root, err := service.openRoot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(root) }()

	id := metadata.ItemID.Dashed()
	parent, err := readerOpenDirectory(root, id[:2])
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(parent) }()
	item, err := readerOpenDirectory(parent, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(item) }()
	resolution := strconv.Itoa(metadata.Width) + " - " + strconv.Itoa(metadata.TileWidth) + "x" + strconv.Itoa(metadata.TileHeight)
	directory, err := readerOpenDirectory(item, resolution)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(directory) }()

	return readerOpenTileFile(directory, strconv.Itoa(index)+".jpg")
}

func readerOpenTileFile(directory int, name string) (*os.File, error) {
	var targetInfo unix.Stat_t
	if err := unix.Fstatat(directory, name, &targetInfo, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, readerOpenError(err)
	}
	if targetInfo.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("%w: tile is not a regular file", ErrUnsafeArtifact)
	}
	fd, err := unix.Openat(directory, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, readerOpenError(err)
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		_ = unix.Close(fd)
		return nil, readerOpenError(err)
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%w: tile is not a regular file", ErrUnsafeArtifact)
	}
	return os.NewFile(uintptr(fd), "trickplay tile"), nil
}

// openMediaTile opens a tile saved with the media
// (TrickplayManager.GetTrickplayTilePathAsync with saveWithMedia). The media
// tree may contain symbolic links up to the ".trickplay" directory; the
// resolution directory and the tile must not be links.
func (service *ReaderService) openMediaTile(ctx context.Context, metadata Metadata, index int) (*os.File, error) {
	mediaPath, err := service.media.MediaPath(ctx, metadata.ItemID)
	if err != nil {
		if errors.Is(err, ErrNotFound) || store.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if mediaPath == "" || !filepath.IsAbs(mediaPath) {
		return nil, ErrNotFound
	}
	root, err := unix.Open(MediaDirectory(mediaPath), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, readerOpenError(err)
	}
	defer func() { _ = unix.Close(root) }()
	directory, err := readerOpenDirectory(root, trickplayResolutionName(metadata.Width, metadata.TileWidth, metadata.TileHeight))
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(directory) }()
	return readerOpenTileFile(directory, strconv.Itoa(index)+".jpg")
}

func readerArtifact(file *os.File, generationToken string) (Artifact, error) {
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return Artifact{}, fmt.Errorf("%w: stat tile descriptor: %v", ErrUnsafeArtifact, err)
	}
	return Artifact{
		Body:            file,
		ContentType:     "image/jpeg",
		Size:            info.Size(),
		Modified:        info.ModTime(),
		GenerationToken: generationToken,
	}, nil
}

func (service *ReaderService) validateRootIdentity() error {
	root, err := service.openRoot()
	if err != nil {
		return err
	}
	return unix.Close(root)
}

func (service *ReaderService) openRoot() (int, error) {
	service.rootMu.RLock()
	defer service.rootMu.RUnlock()
	if service.rootFD < 0 {
		return -1, fmt.Errorf("%w: reader service is closed", ErrInvalidRequest)
	}
	root, rootInfo, err := readerOpenRoot(service.root)
	if err != nil {
		return -1, err
	}
	if uint64(rootInfo.Dev) != service.rootDevice || uint64(rootInfo.Ino) != service.rootInode {
		_ = unix.Close(root)
		return -1, fmt.Errorf("%w: artifact root identity changed", ErrUnsafeArtifact)
	}
	return root, nil
}

func readerOpenRoot(rootPath string) (int, unix.Stat_t, error) {
	var pathInfo unix.Stat_t
	if err := unix.Lstat(rootPath, &pathInfo); err != nil {
		return -1, unix.Stat_t{}, readerOpenError(err)
	}
	if pathInfo.Mode&unix.S_IFMT == unix.S_IFLNK {
		return -1, unix.Stat_t{}, fmt.Errorf("%w: artifact root is a symbolic link", ErrUnsafeArtifact)
	}
	if pathInfo.Mode&unix.S_IFMT != unix.S_IFDIR {
		return -1, unix.Stat_t{}, fmt.Errorf("%w: artifact root is not a directory", ErrUnsafeArtifact)
	}
	root, err := unix.Open(rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, unix.Stat_t{}, readerOpenError(err)
	}
	var rootInfo unix.Stat_t
	if err := unix.Fstat(root, &rootInfo); err != nil {
		_ = unix.Close(root)
		return -1, unix.Stat_t{}, readerOpenError(err)
	}
	if rootInfo.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(root)
		return -1, unix.Stat_t{}, fmt.Errorf("%w: artifact root is not a directory", ErrUnsafeArtifact)
	}
	return root, rootInfo, nil
}

func readerOpenDirectory(parent int, name string) (int, error) {
	var info unix.Stat_t
	if err := unix.Fstatat(parent, name, &info, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return -1, readerOpenError(err)
	}
	if info.Mode&unix.S_IFMT == unix.S_IFLNK {
		return -1, fmt.Errorf("%w: symbolic link", ErrUnsafeArtifact)
	}
	if info.Mode&unix.S_IFMT != unix.S_IFDIR {
		return -1, fmt.Errorf("%w: path component is not a directory", ErrUnsafeArtifact)
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, readerOpenError(err)
	}
	return fd, nil
}

func readerOpenError(err error) error {
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
		return ErrNotFound
	}
	if errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("%w: symbolic link", ErrUnsafeArtifact)
	}
	return fmt.Errorf("%w: open artifact: %v", ErrUnsafeArtifact, err)
}

func readerContextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", ErrInvalidRequest)
	}
	return ctx.Err()
}
