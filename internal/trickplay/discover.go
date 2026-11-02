// SPDX-License-Identifier: GPL-2.0-only

package trickplay

import (
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
	"golang.org/x/sys/unix"
)

// resolutionDirectoryPattern matches a "<width> - <tileWidth>x<tileHeight>"
// directory (TrickplayManager.TrickplaySubdirRegex).
var resolutionDirectoryPattern = regexp.MustCompile(`^(\d+) - (\d+)x(\d+)$`)

// MediaDirectory returns the directory that holds trickplay saved with the
// media at mediaPath: "<containing folder>/<file name>.trickplay"
// (PathManager.GetTrickplayDirectory with saveWithMedia, which uses
// Path.ChangeExtension on the file name).
func MediaDirectory(mediaPath string) string {
	name := filepath.Base(mediaPath)
	if extension := filepath.Ext(name); extension != "" {
		name = strings.TrimSuffix(name, extension)
	}
	return filepath.Join(filepath.Dir(mediaPath), name+".trickplay")
}

// MediaResolutionHasTiles reports whether the media-side directory of one
// resolution holds at least one JPEG tile (TrickplayManager.HasTrickplayTiles).
func MediaResolutionHasTiles(mediaPath string, width, tileWidth, tileHeight int) (bool, error) {
	tiles, err := jpegTiles(filepath.Join(MediaDirectory(mediaPath), trickplayResolutionName(width, tileWidth, tileHeight)))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return len(tiles) != 0, err
}

// DiscoverRequest names one item whose media-side trickplay is catalogued.
type DiscoverRequest struct {
	ItemID       dto.UUID
	MediaPath    string
	RunTimeTicks *int64
	// IntervalMilliseconds is the configured TrickplayOptions.Interval.
	IntervalMilliseconds int64
	// ExistingWidths are the widths that already have a persisted row.
	ExistingWidths []int
}

// DiscoverMedia ports the folder scan of TrickplayManager
// .DiscoverExistingTrickplayAsync for trickplay saved with the media. It
// returns one rendition for each "<width> - <tileWidth>x<tileHeight>"
// directory that holds JPEG tiles and whose width has no persisted row. The
// returned metadata has no source revision or generation token; the caller
// persists it.
func DiscoverMedia(ctx context.Context, request DiscoverRequest) ([]Metadata, error) {
	if request.ItemID.IsNil() {
		return nil, errors.New("trickplay discovery item id is required")
	}
	if request.MediaPath == "" {
		return nil, errors.New("trickplay discovery media path is required")
	}
	root := MediaDirectory(request.MediaPath)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read trickplay directory %s: %w", root, err)
	}
	existing := make(map[int]struct{}, len(request.ExistingWidths))
	for _, width := range request.ExistingWidths {
		existing[width] = struct{}{}
	}
	var discovered []Metadata
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() {
			continue
		}
		width, tileWidth, tileHeight, ok := parseResolutionDirectory(entry.Name())
		if !ok {
			continue
		}
		if _, ok := existing[width]; ok {
			continue
		}
		directory := filepath.Join(root, entry.Name())
		tiles, err := jpegTiles(directory)
		if err != nil {
			return nil, fmt.Errorf("read trickplay tiles %s: %w", directory, err)
		}
		if len(tiles) == 0 {
			continue
		}
		firstHeight, err := jpegHeight(filepath.Join(directory, tiles[0].name))
		if err != nil {
			return nil, fmt.Errorf("read trickplay tile %s: %w", filepath.Join(directory, tiles[0].name), err)
		}
		sizes := make([]int64, len(tiles))
		for index, tile := range tiles {
			sizes[index] = tile.size
		}
		metadata, err := (Manager{}).Discovered(DiscoveredRequest{
			ItemID: request.ItemID, Width: width, TileWidth: tileWidth, TileHeight: tileHeight,
			RunTimeTicks: request.RunTimeTicks, IntervalMilliseconds: request.IntervalMilliseconds,
			FirstTileHeight: firstHeight, TileByteSizes: sizes,
		})
		if err != nil {
			return nil, fmt.Errorf("discover trickplay %s: %w", directory, err)
		}
		existing[width] = struct{}{}
		discovered = append(discovered, metadata)
	}
	return discovered, nil
}

// DiscoveredRequest holds the facts read from one media-side resolution
// directory.
type DiscoveredRequest struct {
	ItemID               dto.UUID
	Width                int
	TileWidth            int
	TileHeight           int
	RunTimeTicks         *int64
	IntervalMilliseconds int64
	FirstTileHeight      int
	// TileByteSizes has one entry for each tile, in ordinal name order.
	TileByteSizes []int64
}

// Discovered derives the metadata of a discovered resolution
// (TrickplayManager.DiscoverExistingTrickplayAsync). The encoder pads the
// last tile to a full grid, so the thumbnail count is bounded by the tile
// count and an interval consistent with the runtime is chosen, the configured
// interval when it fits.
func (Manager) Discovered(request DiscoveredRequest) (Metadata, error) {
	if request.Width <= 0 {
		return Metadata{}, errors.New("trickplay width must be positive")
	}
	capacity, err := tileCapacity(request.TileWidth, request.TileHeight)
	if err != nil {
		return Metadata{}, err
	}
	tiles := len(request.TileByteSizes)
	if tiles == 0 {
		return Metadata{}, errors.New("cannot discover trickplay from zero tiles")
	}
	if tiles > math.MaxInt32/capacity {
		return Metadata{}, errors.New("trickplay thumbnail count overflows")
	}
	maxThumbs := int64(tiles * capacity)
	minThumbs := int64(1)
	if tiles > 1 {
		minThumbs = int64((tiles-1)*capacity) + 1
	}

	var interval, thumbnailCount int64
	if request.RunTimeTicks != nil && *request.RunTimeTicks >= 0 {
		runtimeMs := *request.RunTimeTicks / ticksPerMillisecond
		minInterval := max(1000, int64(math.Ceil(float64(runtimeMs)/float64(maxThumbs))))
		maxInterval := max(minInterval, int64(math.Floor(float64(runtimeMs)/float64(minThumbs))))
		if request.IntervalMilliseconds >= minInterval && request.IntervalMilliseconds <= maxInterval {
			interval = request.IntervalMilliseconds
		} else {
			// Math.Round rounds half to even.
			midpoint := float64(minInterval+maxInterval) / 2
			snapped := int64(math.RoundToEven(midpoint/1000)) * 1000
			interval = min(max(snapped, minInterval), maxInterval)
		}
		thumbnailCount = min(max(int64(math.RoundToEven(float64(runtimeMs)/float64(interval))), minThumbs), maxThumbs)
	} else {
		interval = max(1000, request.IntervalMilliseconds)
		thumbnailCount = maxThumbs
	}

	metadata := Metadata{
		ItemID:         request.ItemID,
		Width:          request.Width,
		Height:         max(1, ceilingDivide(request.FirstTileHeight, request.TileHeight)),
		TileWidth:      request.TileWidth,
		TileHeight:     request.TileHeight,
		ThumbnailCount: int(thumbnailCount),
		Interval:       interval,
	}
	for index, size := range request.TileByteSizes {
		bitrate, err := generatedBandwidth(size, capacity, interval)
		if err != nil {
			return Metadata{}, fmt.Errorf("tile %d bandwidth: %w", index, err)
		}
		metadata.Bandwidth = max(metadata.Bandwidth, bitrate)
	}
	return metadata, nil
}

func parseResolutionDirectory(name string) (width, tileWidth, tileHeight int, ok bool) {
	match := resolutionDirectoryPattern.FindStringSubmatch(name)
	if match == nil {
		return 0, 0, 0, false
	}
	values := [3]int{}
	for index := range values {
		value, err := strconv.ParseInt(match[index+1], 10, 32)
		if err != nil || value <= 0 {
			return 0, 0, 0, false
		}
		values[index] = int(value)
	}
	return values[0], values[1], values[2], true
}

type jpegTile struct {
	name string
	size int64
}

// jpegTiles lists the regular "*.jpg" files of directory in ordinal name
// order, as upstream does (StringComparer.Ordinal).
func jpegTiles(directory string) ([]jpegTile, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var tiles []jpegTile
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jpg") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		tiles = append(tiles, jpegTile{name: entry.Name(), size: info.Size()})
	}
	sort.Slice(tiles, func(left, right int) bool { return tiles[left].name < tiles[right].name })
	return tiles, nil
}

func jpegHeight(path string) (height int, returnErr error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	config, err := jpeg.DecodeConfig(io.LimitReader(file, maxFrameExtractorJPEGHeaderBytes))
	if err != nil {
		return 0, err
	}
	return config.Height, nil
}
