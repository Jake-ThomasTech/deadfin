// SPDX-License-Identifier: GPL-2.0-only

package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/playback"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
)

// Upstream dynamic image provider names. They are the ImageFetchers and
// ImageFetcherOrder values that select them.
const (
	ScreenGrabberName          = "Screen Grabber"
	EmbeddedImageExtractorName = "Embedded Image Extractor"
	AudioImageExtractorName    = "Image Extractor"
)

const dynamicMetadataPathToken = "%MetadataPath%"

// ImageFrameExtractor extracts one image from a pinned media source
// (ffmpeg.ImageExtractor).
type ImageFrameExtractor interface {
	Extract(ctx context.Context, job encoding.ImageExtraction, source *sourcefile.Source, timeout time.Duration) error
}

// DynamicImageSettings are the server settings the dynamic image providers
// read for one item type at refresh time.
type DynamicImageSettings struct {
	// DisabledImageFetchers and ImageFetcherOrder are the server
	// MetadataOptions for the item type (empty when none is configured).
	DisabledImageFetchers []string
	ImageFetcherOrder     []string
	// Extraction carries the encoder facts and the server encoding settings
	// of every extraction: Resolution, Threads, PerfTradeoff, filter support
	// and encoder version.
	Extraction encoding.ImageExtraction
	// Timeout is ImageExtractionTimeoutMs; zero selects the upstream default.
	Timeout time.Duration
}

// DynamicImagesOptions configures DynamicImages.
type DynamicImagesOptions struct {
	Extractor ImageFrameExtractor
	// MetadataPath is the absolute internal metadata root.
	MetadataPath string
	// Settings returns the current server settings for an item type name.
	Settings func(itemType string) DynamicImageSettings
	Logger   *slog.Logger
}

// DynamicImages runs upstream's IDynamicImageProvider pass: for videos,
// EmbeddedImageProvider ("Embedded Image Extractor", order 99) and
// VideoImageProvider ("Screen Grabber", order 100); for audio,
// AudioImageProvider ("Image Extractor"). It implements
// library.DynamicImageRefresher and library.FolderImageCopier.
type DynamicImages struct {
	options   DynamicImagesOptions
	providers []dynamicImageProvider
}

// NewDynamicImages returns the dynamic image providers in upstream default
// order.
func NewDynamicImages(options DynamicImagesOptions) *DynamicImages {
	return &DynamicImages{options: options, providers: []dynamicImageProvider{audioImageExtractor{}, embeddedImageExtractor{}, screenGrabber{}}}
}

// ImageFetchers lists the dynamic image providers for kind in default order,
// for GET /Libraries/AvailableOptions. A kind that the providers do not
// support has none.
func (d *DynamicImages) ImageFetchers(kind item.Kind) []ImageFetcherInfo {
	if d == nil {
		return nil
	}
	// GetPluginSummary asks about a file-backed dummy item of the kind.
	path := "dummy"
	dummy := item.Item{Kind: kind, Path: &path}
	var fetchers []ImageFetcherInfo
	for _, provider := range d.providers {
		if !provider.Supports(dummy) {
			continue
		}
		fetchers = append(fetchers, ImageFetcherInfo{Name: provider.Name(), SupportedImages: provider.SupportedImages(dummy)})
	}
	return fetchers
}

// dynamicImageProvider is the IDynamicImageProvider + IHasOrder shape.
type dynamicImageProvider interface {
	Name() string
	Order() int
	Supports(item.Item) bool
	SupportedImages(item.Item) []item.ImageType
	// Plan returns the extraction for imageType, or false when the provider
	// has no image (DynamicImageResponse.HasImage == false).
	Plan(target item.Item, imageType item.ImageType, streams []playback.MediaStream, attachments []playback.MediaAttachment) (dynamicImagePlan, bool)
}

// dynamicImagePlan is one extraction and the saved file extension.
type dynamicImagePlan struct {
	job       encoding.ImageExtraction
	extension string
}

// RefreshDynamicImages ports ProviderManager.GetImageProviders for the
// dynamic providers and ItemImageProvider.RefreshFromProvider
// (IDynamicImageProvider). The caller has decided that the item's image
// refresh runs non-local providers (MetadataService.GetNonLocalImageProviders).
func (d *DynamicImages) RefreshDynamicImages(ctx context.Context, input library.DynamicImageInput, target *item.Item) error {
	if d == nil || target == nil || d.options.Extractor == nil || d.options.MetadataPath == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !d.supports(*target) {
		return nil
	}
	// ProviderManager.CanRefreshImages: a locked item refreshes remote and
	// dynamic images only on a FullRefresh.
	if target.IsLocked && input.Image != library.RefreshModeFullRefresh {
		return nil
	}
	settings := DynamicImageSettings{}
	if d.options.Settings != nil {
		settings = d.options.Settings(target.Kind.String())
	}
	replacing := input.Image == library.RefreshModeFullRefresh && input.ReplaceAllImages
	// downloaded lists the image types a provider set during this refresh.
	// Deadfin's remote providers set remote image URLs inside the remote
	// metadata stage, which runs before this pass.
	downloaded := make(map[item.ImageType]bool)
	if replacing {
		for _, image := range target.ImageInfos {
			if isRemoteImagePath(image.Path) {
				downloaded[image.Type] = true
			}
		}
	}
	for _, provider := range d.enabledProviders(input.Options, *target, settings) {
		if err := d.refreshFromProvider(ctx, provider, input, target, settings, replacing, downloaded); err != nil {
			return err
		}
	}
	return nil
}

// enabledProviders ports GetImageProvidersInternal: the enabled providers in
// ImageFetcherOrder, then in default order. TypeOptions.ImageFetcherOrder
// wins over the server order when the library has an entry for the type.
func (d *DynamicImages) enabledProviders(options library.LibraryOptions, target item.Item, settings DynamicImageSettings) []dynamicImageProvider {
	typeOptions := options.TypeOptionsFor(target.Kind)
	order := settings.ImageFetcherOrder
	if typeOptions != nil {
		order = typeOptions.ImageFetcherOrder
	}
	enabled := make([]dynamicImageProvider, 0, len(d.providers))
	for _, provider := range d.providers {
		if provider.Supports(target) && imageFetcherEnabled(typeOptions, settings.DisabledImageFetchers, provider.Name()) {
			enabled = append(enabled, provider)
		}
	}
	sort.SliceStable(enabled, func(i, j int) bool {
		left, right := configuredOrder(order, enabled[i].Name()), configuredOrder(order, enabled[j].Name())
		if left != right {
			return left < right
		}
		return enabled[i].Order() < enabled[j].Order()
	})
	return enabled
}

// configuredOrder ports ProviderManager.GetConfiguredOrder, which matches the
// name exactly (Array.IndexOf). An unlisted provider sorts last.
func configuredOrder(order []string, name string) int {
	for index, value := range order {
		if value == name {
			return index
		}
	}
	return len(order) + 1
}

// refreshFromProvider ports ItemImageProvider.RefreshFromProvider for an
// IDynamicImageProvider. A provider failure is logged and ends that
// provider's pass; cancellation propagates.
func (d *DynamicImages) refreshFromProvider(ctx context.Context, provider dynamicImageProvider, input library.DynamicImageInput, target *item.Item, settings DynamicImageSettings, replacing bool, downloaded map[item.ImageType]bool) error {
	for _, imageType := range provider.SupportedImages(*target) {
		if input.Options.ImageLimit(target.Kind, imageType) <= 0 {
			continue
		}
		if hasImageType(target.ImageInfos, imageType) && !(replacing && !downloaded[imageType]) {
			continue
		}
		plan, ok := provider.Plan(*target, imageType, input.Streams, input.Attachments)
		if !ok {
			continue
		}
		if err := d.extractAndSave(ctx, plan, target, imageType, settings); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d.log().LogAttrs(ctx, slog.LevelError, "dynamic image provider failed",
				slog.String("provider", provider.Name()),
				slog.String("item", target.ID.String()),
				slog.String("path", derefString(target.Path)),
				slog.String("error", err.Error()))
			return nil
		}
		downloaded[imageType] = true
	}
	return nil
}

// extractAndSave runs one extraction into the item's internal metadata
// directory and records it as the item's image.
func (d *DynamicImages) extractAndSave(ctx context.Context, plan dynamicImagePlan, target *item.Item, imageType item.ImageType, settings DynamicImageSettings) error {
	if target.Path == nil {
		return errors.New("item has no path")
	}
	source, err := sourcefile.Open(*target.Path, sourcefile.Expected{})
	if err != nil {
		return fmt.Errorf("open media source: %w", err)
	}
	defer func() { _ = source.Close() }()
	return d.saveImage(target, imageType, plan.extension, func(temporaryPath string) error {
		job := mergeExtractionSettings(plan.job, settings.Extraction)
		job.OutputPath = temporaryPath
		return d.options.Extractor.Extract(ctx, job, source, settings.Timeout)
	})
}

// CopyFolderImage ports BaseDynamicImageProvider.CreateSingleImage and
// FetchToFileInternal for one image type: it copies the local image of
// source into the internal metadata directory of target and records it as
// the image of target. It reports false when source has no local image of
// the type with a file extension.
func (d *DynamicImages) CopyFolderImage(ctx context.Context, source item.Item, target *item.Item, imageType item.ImageType) (bool, error) {
	if d == nil || target == nil || d.options.MetadataPath == "" {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	position := imagePositionOf(source.ImageInfos, imageType, 0)
	if position < 0 {
		return false, nil
	}
	path := source.ImageInfos[position].Path
	if path == "" || isRemoteImagePath(path) || filepath.Ext(path) == "" {
		return false, nil
	}
	if rest, found := strings.CutPrefix(path, dynamicMetadataPathToken+"/"); found {
		path = filepath.Join(d.options.MetadataPath, filepath.FromSlash(rest))
	}
	err := d.saveImage(target, imageType, strings.ToLower(filepath.Ext(path)), func(temporaryPath string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(temporaryPath, data, 0o644)
	})
	return err == nil, err
}

// saveImage writes one image through write into the item's internal
// metadata directory and records it as the item's image. Images save there
// with a content tag in the file name, as uploads do (D-44).
func (d *DynamicImages) saveImage(target *item.Item, imageType item.ImageType, extension string, write func(temporaryPath string) error) error {
	relativeDirectory := internalMetadataRelative(target.ID)
	directory := filepath.Join(d.options.MetadataPath, relativeDirectory)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".extract-*"+extension)
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	_ = temporary.Close()
	defer func() { _ = os.Remove(temporaryPath) }()

	if err := write(temporaryPath); err != nil {
		return err
	}
	data, err := os.ReadFile(temporaryPath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	// ImageSaver.SaveImage: a backdrop without an index takes the next one
	// (the image count); a single image replaces the image at index 0.
	index := 0
	if imageType == item.ImageTypeBackdrop {
		index = len(imagesOfType(target.ImageInfos, imageType))
	}
	position := imagePositionOf(target.ImageInfos, imageType, index)
	name := dynamicImageSaveName(target.Kind, imageType, index) + "-" + hex.EncodeToString(sum[:4]) + extension
	absolute := filepath.Join(directory, name)
	if err := os.Chmod(temporaryPath, 0o644); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, absolute); err != nil {
		return err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return err
	}
	modified := info.ModTime().UTC()
	stored := filepath.ToSlash(filepath.Join(dynamicMetadataPathToken, relativeDirectory, name))
	if position >= 0 {
		previous := target.ImageInfos[position].Path
		target.ImageInfos[position] = item.ImageInfo{Type: imageType, Index: target.ImageInfos[position].Index, Path: stored, DateModified: &modified}
		d.removeReplacedImage(previous, stored)
		return nil
	}
	target.ImageInfos = append(target.ImageInfos, item.ImageInfo{Type: imageType, Index: index, Path: stored, DateModified: &modified})
	return nil
}

// removeReplacedImage deletes a replaced image file only when it lives in the
// internal metadata directory, as SetImage does for an upload.
func (d *DynamicImages) removeReplacedImage(previous, stored string) {
	if previous == "" || previous == stored || !strings.HasPrefix(previous, dynamicMetadataPathToken+"/") {
		return
	}
	expanded := filepath.Join(d.options.MetadataPath, filepath.FromSlash(strings.TrimPrefix(previous, dynamicMetadataPathToken+"/")))
	if err := os.Remove(expanded); err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.log().Warn("remove replaced dynamic image", "path", expanded, "error", err)
	}
}

func (d *DynamicImages) log() *slog.Logger {
	if d.options.Logger != nil {
		return d.options.Logger
	}
	return slog.Default()
}

// mergeExtractionSettings adds the server and encoder facts to a provider's
// extraction.
func mergeExtractionSettings(job, settings encoding.ImageExtraction) encoding.ImageExtraction {
	job.Resolution = settings.Resolution
	job.Threads = settings.Threads
	job.PerfTradeoff = settings.PerfTradeoff
	job.SupportsTonemapX = settings.SupportsTonemapX
	job.SupportsZscale = settings.SupportsZscale
	job.EncoderAtLeast51 = settings.EncoderAtLeast51
	return job
}

// supports reports whether any dynamic image provider supports target.
func (d *DynamicImages) supports(target item.Item) bool {
	for _, provider := range d.providers {
		if provider.Supports(target) {
			return true
		}
	}
	return false
}

// supportsVideoImages ports VideoImageProvider.Supports and
// EmbeddedImageProvider.Supports: a complete, non-placeholder Video on the
// file protocol that is not a shortcut. Library videos are never shortcuts in
// Deadfin, and a virtual item has no file.
func supportsVideoImages(target item.Item) bool {
	switch target.Kind {
	case item.KindMovie, item.KindEpisode, item.KindVideo, item.KindMusicVideo, item.KindTrailer:
	default:
		return false
	}
	return isLocalFileItem(target)
}

// isLocalFileItem reports a non-virtual, non-folder item with a file on the
// file protocol (BaseItem.IsFileProtocol).
func isLocalFileItem(target item.Item) bool {
	if target.IsPlaceHolder || target.IsVirtualItem || target.IsFolder || target.Path == nil || *target.Path == "" {
		return false
	}
	return !isRemoteImagePath(*target.Path)
}

// noDynamicImageSource ports the shared GetImage guard: placeholders and DVD
// folders have no image.
func noDynamicImageSource(target item.Item) bool {
	return target.IsPlaceHolder || target.VideoType != nil && *target.VideoType == item.VideoTypeDvd
}

// screenGrabber ports VideoImageProvider.
type screenGrabber struct{}

func (screenGrabber) Name() string { return ScreenGrabberName }

// Order is 100: after internet image providers.
func (screenGrabber) Order() int { return 100 }

func (screenGrabber) Supports(target item.Item) bool { return supportsVideoImages(target) }

func (screenGrabber) SupportedImages(item.Item) []item.ImageType {
	return []item.ImageType{item.ImageTypePrimary}
}

func (screenGrabber) Plan(target item.Item, _ item.ImageType, streams []playback.MediaStream, _ []playback.MediaAttachment) (dynamicImagePlan, bool) {
	if noDynamicImageSource(target) {
		return dynamicImagePlan{}, false
	}
	// DefaultVideoStreamIndex is the first video stream
	// (FFProbeVideoInfo.cs:263-268); without one there is nothing to grab.
	var videoStream *playback.MediaStream
	for index := range streams {
		if streams[index].Type == playback.StreamTypeVideo {
			videoStream = &streams[index]
			break
		}
	}
	if videoStream == nil {
		return dynamicImagePlan{}, false
	}
	// 10% into the video when the duration is known, otherwise 10 seconds.
	offset := 10 * time.Second
	if target.RunTimeTicks != nil && *target.RunTimeTicks > 0 {
		offset = time.Duration(*target.RunTimeTicks/10) * 100
	}
	job := encoding.ImageExtraction{
		Container: derefString(target.Container),
		Offset:    &offset,
		Video3D:   image3DFormat(target.Video3DFormat),
	}
	applyStreamFacts(&job, videoStream)
	return dynamicImagePlan{job: job, extension: ".jpg"}, true
}

// embeddedImageExtractor ports EmbeddedImageProvider.
type embeddedImageExtractor struct{}

func (embeddedImageExtractor) Name() string { return EmbeddedImageExtractorName }

// Order is 99: after internet image providers, before the Screen Grabber.
func (embeddedImageExtractor) Order() int { return 99 }

func (embeddedImageExtractor) Supports(target item.Item) bool { return supportsVideoImages(target) }

func (embeddedImageExtractor) SupportedImages(target item.Item) []item.ImageType {
	if target.Kind == item.KindEpisode {
		return []item.ImageType{item.ImageTypePrimary}
	}
	return []item.ImageType{item.ImageTypePrimary, item.ImageTypeBackdrop, item.ImageTypeLogo}
}

func embeddedImageFileNames(imageType item.ImageType) []string {
	switch imageType {
	case item.ImageTypePrimary:
		return []string{"poster", "folder", "cover", "default", "movie", "show"}
	case item.ImageTypeBackdrop:
		return []string{"backdrop", "background", "art"}
	case item.ImageTypeLogo:
		return []string{"logo"}
	}
	return nil
}

func (embeddedImageExtractor) Plan(target item.Item, imageType item.ImageType, streams []playback.MediaStream, attachments []playback.MediaAttachment) (dynamicImagePlan, bool) {
	if noDynamicImageSource(target) {
		return dynamicImagePlan{}, false
	}
	names := embeddedImageFileNames(imageType)
	if len(names) == 0 {
		return dynamicImagePlan{}, false
	}
	container := derefString(target.Container)
	// Attachments first.
	for _, attachment := range attachments {
		if attachment.FileName == "" || !containsAnyFold(attachment.FileName, names) {
			continue
		}
		extension := filepath.Ext(attachment.FileName)
		if attachment.MimeType != "" {
			extension = mimeTypeExtension(attachment.MimeType)
		}
		index := attachment.Index
		return dynamicImagePlan{
			job:       encoding.ImageExtraction{Container: container, StreamIndex: &index},
			extension: imageFormatExtension(extension),
		}, true
	}
	// Then EmbeddedImage streams: the first whose comment names the type,
	// or the first stream for Primary.
	var images []playback.MediaStream
	for _, stream := range streams {
		if stream.Type == playback.StreamTypeEmbeddedImage {
			images = append(images, stream)
		}
	}
	if len(images) == 0 {
		return dynamicImagePlan{}, false
	}
	var selected *playback.MediaStream
	for index := range images {
		if images[index].Comment != nil && *images[index].Comment != "" && containsAnyFold(*images[index].Comment, names) {
			selected = &images[index]
			break
		}
	}
	if selected == nil {
		if imageType != item.ImageTypePrimary {
			return dynamicImagePlan{}, false
		}
		selected = &images[0]
	}
	index := selected.Index
	job := encoding.ImageExtraction{Container: container, StreamIndex: &index}
	applyStreamFacts(&job, selected)
	return dynamicImagePlan{job: job, extension: codecImageExtension(selected.Codec)}, true
}

// audioImageExtractor ports AudioImageProvider. It has no IHasOrder, so its
// order is 0.
type audioImageExtractor struct{}

func (audioImageExtractor) Name() string { return AudioImageExtractorName }

func (audioImageExtractor) Order() int { return 0 }

// Supports ports AudioImageProvider.Supports: an Audio item (AudioBook
// derives from Audio upstream) on the file protocol.
func (audioImageExtractor) Supports(target item.Item) bool {
	if target.Kind != item.KindAudio && target.Kind != item.KindAudioBook {
		return false
	}
	return isLocalFileItem(target)
}

func (audioImageExtractor) SupportedImages(item.Item) []item.ImageType {
	return []item.ImageType{item.ImageTypePrimary}
}

// Plan ports AudioImageProvider.GetImage: the first EmbeddedImage stream
// whose comment contains "front", then "cover", then the first one. Upstream
// MediaEncoder.ExtractAudioImage always writes a JPEG.
func (audioImageExtractor) Plan(_ item.Item, _ item.ImageType, streams []playback.MediaStream, _ []playback.MediaAttachment) (dynamicImagePlan, bool) {
	var images []playback.MediaStream
	for _, stream := range streams {
		if stream.Type == playback.StreamTypeEmbeddedImage {
			images = append(images, stream)
		}
	}
	if len(images) == 0 {
		return dynamicImagePlan{}, false
	}
	selected := images[0]
	for _, word := range []string{"front", "cover"} {
		if index := slices.IndexFunc(images, func(stream playback.MediaStream) bool {
			return stream.Comment != nil && containsAnyFold(*stream.Comment, []string{word})
		}); index >= 0 {
			selected = images[index]
			break
		}
	}
	index := selected.Index
	return dynamicImagePlan{job: encoding.ImageExtraction{StreamIndex: &index, Audio: true}, extension: ".jpg"}, true
}

// applyStreamFacts copies the stream facts ExtractImageInternal reads.
func applyStreamFacts(job *encoding.ImageExtraction, stream *playback.MediaStream) {
	job.Interlaced = stream.IsInterlaced
	job.HDR = stream.VideoRange() == playback.MediaVideoRangeHDR
	job.DolbyVision = stream.VideoRangeType() == playback.VideoRangeDOVI
}

// codecImageExtension maps an embedded image codec to its ImageFormat
// extension (EmbeddedImageProvider.GetEmbeddedImage).
func codecImageExtension(codec string) string {
	switch codec {
	case "bmp":
		return ".bmp"
	case "gif":
		return ".gif"
	case "png":
		return ".png"
	case "webp":
		return ".webp"
	default:
		return ".jpg"
	}
}

// imageFormatExtension maps an attachment extension to its ImageFormat
// extension (EmbeddedImageProvider.ExtractAttachment); others are JPEG.
func imageFormatExtension(extension string) string {
	switch extension {
	case ".bmp", ".gif", ".png", ".webp":
		return extension
	default:
		return ".jpg"
	}
}

// mimeTypeExtension ports MimeTypes.ToExtension for image media types.
func mimeTypeExtension(mimeType string) string {
	switch strings.ToLower(mimeType) {
	case "image/jpeg", "image/jpg", "image/pjpeg":
		return ".jpg"
	case "image/png", "image/x-png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	default:
		return ""
	}
}

func image3DFormat(value *item.Video3DFormat) encoding.Image3DFormat {
	if value == nil {
		return encoding.Image3DNone
	}
	switch *value {
	case item.Video3DFormatHalfSideBySide:
		return encoding.Image3DHalfSideBySide
	case item.Video3DFormatFullSideBySide:
		return encoding.Image3DFullSideBySide
	case item.Video3DFormatFullTopAndBottom:
		return encoding.Image3DFullTopAndBottom
	case item.Video3DFormatHalfTopAndBottom:
		return encoding.Image3DHalfTopAndBottom
	default:
		return encoding.Image3DNone
	}
}

// dynamicImageSaveName ports the non-local names of
// ImageSaver.GetStandardSavePath for the types the dynamic providers save.
func dynamicImageSaveName(kind item.Kind, imageType item.ImageType, index int) string {
	switch imageType {
	case item.ImageTypePrimary:
		return "poster"
	case item.ImageTypeBackdrop:
		if index == 0 {
			return "backdrop"
		}
		return "backdrop" + strconv.Itoa(index)
	default:
		return strings.ToLower(imageType.String())
	}
}

// internalMetadataRelative ports BaseItem.GetInternalMetadataPath relative to
// the metadata root: library/<first two id characters>/<id>.
func internalMetadataRelative(id dto.UUID) string {
	value := id.String()
	return filepath.Join("library", value[:2], value)
}

func hasImageType(images []item.ImageInfo, imageType item.ImageType) bool {
	return imagePositionOf(images, imageType, -1) >= 0
}

// imagePositionOf returns the position of the image of imageType at index,
// or of any image of imageType when index is -1.
func imagePositionOf(images []item.ImageInfo, imageType item.ImageType, index int) int {
	for position, image := range images {
		if image.Type == imageType && (index < 0 || image.Index == index) {
			return position
		}
	}
	return -1
}

func imagesOfType(images []item.ImageInfo, imageType item.ImageType) []item.ImageInfo {
	var result []item.ImageInfo
	for _, image := range images {
		if image.Type == imageType {
			result = append(result, image)
		}
	}
	return result
}

func containsAnyFold(value string, names []string) bool {
	lower := strings.ToLower(value)
	for _, name := range names {
		if strings.Contains(lower, name) {
			return true
		}
	}
	return false
}

func isRemoteImagePath(path string) bool {
	return strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://")
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
