// SPDX-License-Identifier: GPL-2.0-only

package encoding

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

const (
	// 8K UHD is 7,680×4,320 (33,177,600 pixels). Keep a small headroom
	// for supported 8K sources while rejecting allocator-hostile dimensions.
	maxTranscodeDimension = 8_192
	maxTranscodePixels    = 32 << 20
)

var subtitleFilterPathEscaper = strings.NewReplacer(
	"\\", "/",
	":", "\\:",
	"'", `'\\\''`,
	"\"", "\\\"",
)

// EncodingJob contains the media facts needed to build a progressive ffmpeg invocation.
// It deliberately contains no request, filesystem, or process state.
type EncodingJob struct {
	InputPath string
	// LiveInput replaces InputPath with a live network input (Live TV).
	LiveInput       *LiveInput
	InputContainer  string
	OutputPath      string
	OutputContainer string

	VideoStream    VideoStream
	AudioStream    AudioStream
	SubtitleStream *SubtitleStream

	OutputVideoCodec      string
	OutputAudioCodec      string
	OutputVideoProfile    string
	OutputVideoLevel      int
	OutputWidth           int
	OutputHeight          int
	OutputVideoBitrate    int
	OutputAudioBitrate    int
	OutputAudioChannels   int
	OutputAudioSampleRate int
	OutputFrameRate       float64
	DownmixAudioBoost     *float64

	StartOffsetTicks int64
	CopyTimestamps   bool
	VideoQuality     VideoQuality

	// Hardware carries the encoding.xml acceleration and tone-mapping
	// options. The zero value selects the software path.
	Hardware HardwareOptions

	// MaxWidth, when positive, replaces the fixed software scale with the
	// maximum-width form of GetSwScaleFilter that image extraction requests.
	// OutputWidth and OutputHeight still size a hardware scale.
	MaxWidth int
}

// VideoStream, AudioStream, and SubtitleStream carry ffmpeg input stream
// indexes (EncodingHelper.FindIndex), not MediaStream indexes. External
// sidecars come first in MediaStreams and are not streams of the media input.
type VideoStream struct {
	Index                        int
	Codec                        string
	ColorTransfer                string
	CodecTag                     string
	BitDepth                     int
	NalLengthSize                string
	DolbyVisionProfile           int
	RPUFlag                      int
	BLPresentFlag                int
	DolbyVisionBLCompatibilityID int
	Width                        int
	Height                       int
	PixelFormat                  string
	Profile                      string
	// RangeType is the upstream VideoRangeType name, for example "HDR10".
	RangeType     string
	RealFrameRate float64
	// ReferenceFrameRate is MediaStream.ReferenceFrameRate: the average
	// frame rate, or RealFrameRate when the average is missing or broken.
	ReferenceFrameRate float64
}

type AudioStream struct {
	Index    int
	Codec    string
	Channels int
}

type SubtitleStream struct {
	InputIndex int
	Index      int
	// SubtitleIndex is the position among the subtitle streams of the input.
	// The subtitles filter option si= counts subtitle streams only.
	SubtitleIndex int
	Codec         string
	Path          string
	BurnIn        bool
	IsExternal    bool
	// ExtractedPath is the cached extraction of an embedded text stream
	// (SubtitleEncoder.GetSubtitleFilePath). When it is set, the subtitles
	// filter reads this file and not the media file.
	ExtractedPath string
	// FontsDir is the attachment cache folder of the media source
	// (PathManager.GetAttachmentFolderPath). The subtitles filter loads its
	// fonts (EncodingHelper.GetTextSubtitlesFilter fontsdir).
	FontsDir string
}

type VideoQuality struct {
	Preset                string
	CRF                   int
	ForcedKeyFrameSeconds int
	X264Options           string
}

// Capabilities describes the local ffmpeg features selected by composition code.
type Capabilities struct {
	H264Encoder                string
	EncoderMajorVersion        int
	EncoderMinorVersion        int
	EncoderPatchVersion        int
	SupportsSubtitleExtraction bool
	AACEncoders                []string
	SupportsTonemapX           bool

	// Encoders, Decoders, HWAccels, and Filters are the probed ffmpeg names
	// that select the hardware pipelines.
	Encoders []string
	Decoders []string
	HWAccels []string
	Filters  []string
	// VaapiDriver is "iHD", "i965", or empty, from the startup probe of the
	// configured VAAPI render node (MediaEncoder.cs:239-249).
	VaapiDriver string
	// SupportsOverlayCudaAlphaFormat is the OverlayCudaAlphaFormat filter
	// option probe (EncoderValidator.cs:167).
	SupportsOverlayCudaAlphaFormat bool
	// SupportsPKeyPause is the "p" runtime-key probe
	// (MediaEncoder.cs:236). The transcode throttler pauses ffmpeg with "p"
	// when it is true and with "c" otherwise.
	SupportsPKeyPause bool
	// BSFOptions lists the supported bitstream filter options as
	// "filter=option" (MediaEncoder.SupportsBitStreamFilterWithOption).
	BSFOptions []string
}

// SupportsBSFOption reports whether ffmpeg supports the bitstream filter
// option, given as "filter=option", for example "dovi_rpu=strip".
func (caps Capabilities) SupportsBSFOption(option string) bool {
	return slices.Contains(caps.BSFOptions, option)
}

// BuildProgressiveArgs builds logical ffmpeg argv tokens for a progressive video
// response. The returned tokens are ready for exec; they are not shell escaped.
func BuildProgressiveArgs(job EncodingJob, caps Capabilities) ([]string, error) {
	if err := validateProgressiveJob(job, caps); err != nil {
		return nil, err
	}

	args := make([]string, 0, 36)
	args = appendInputArgs(args, job)
	args = appendMapArgs(args, job)
	args = appendVideoArgs(args, job, caps)
	args = append(args, "-map_metadata", "-1", "-map_chapters", "-1", "-threads", "0")
	args = appendAudioArgs(args, job, caps)
	args = appendOutputArgs(args, job)
	return args, nil
}

func validateProgressiveJob(job EncodingJob, caps Capabilities) error {
	return validateVideoJob(job, caps, false)
}

// validateVideoJob validates a progressive or HLS job. Only the HLS
// builder drives the hardware encoders.
func validateVideoJob(job EncodingJob, caps Capabilities, hardware bool) error {
	if err := validateEncodingInput(job); err != nil {
		return err
	}
	if job.OutputContainer != "ts" && job.OutputContainer != "mp4" && job.OutputContainer != "fmp4" && (hardware || job.OutputContainer != "mkv") {
		return fmt.Errorf("unsupported progressive output container %q", job.OutputContainer)
	}
	if job.VideoStream.Index < 0 || job.AudioStream.Index < 0 {
		return fmt.Errorf("stream indexes must be non-negative")
	}
	if err := validateEncodingSharedFacts(job); err != nil {
		return err
	}
	if job.OutputVideoCodec == "copy" {
		if job.SubtitleStream != nil && job.SubtitleStream.BurnIn {
			return fmt.Errorf("video stream copy cannot burn subtitles")
		}
	} else {
		if !supportedVideoEncoder(job, caps, hardware) {
			return fmt.Errorf("unsupported progressive video encoder %q", job.OutputVideoCodec)
		}
		if err := validateTranscodeDimensions(job.OutputWidth, job.OutputHeight); err != nil {
			return err
		}
		// A live request may carry no bitrate and the source may have no
		// probed rate; the encoder then runs on CRF alone.
		if job.OutputVideoBitrate < 0 || job.OutputVideoBitrate > int(^uint(0)>>1)/2 {
			return fmt.Errorf("video transcode requires a non-negative non-overflowing bitrate")
		}
		if isHDRRange(videoRangeType(job.VideoStream)) && !CanTonemap(job.VideoStream, caps, job.Hardware) {
			return fmt.Errorf("HDR tone mapping requires 10-bit input and tonemapx or hardware tone mapping support")
		}
		if !isX264Preset(job.VideoQuality.Preset) || job.VideoQuality.CRF < 0 || job.VideoQuality.CRF > 51 || job.VideoQuality.ForcedKeyFrameSeconds <= 0 || !safeX264Options(job.VideoQuality.X264Options) {
			return fmt.Errorf("invalid H.264 quality settings")
		}
	}
	return validateAudioOutput(job, caps)
}

// supportedVideoEncoder reports whether the job's output codec has an
// encoder: libx264, or a hardware encoder for H.264, HEVC, or AV1.
func supportedVideoEncoder(job EncodingJob, caps Capabilities, hardware bool) bool {
	if !hardware {
		return caps.H264Encoder == "libx264" && (job.OutputVideoCodec == "h264" || job.OutputVideoCodec == "libx264")
	}
	switch job.OutputVideoCodec {
	case "libx264":
		return caps.H264Encoder == "libx264"
	case "h264":
		return caps.H264Encoder == "libx264" || hardwareVideoEncoder("h264", job.Hardware, caps) != ""
	case "hevc", "av1":
		return hardwareVideoEncoder(job.OutputVideoCodec, job.Hardware, caps) != ""
	}
	return false
}

// validateTranscodeDimensions checks output dimensions. A zero dimension is
// unknown, not invalid: a live source that probed without width or height
// encodes at the source size (GetSwScaleFilter returns no filter when neither
// is requested, EncodingHelper.cs:3566-3572).
func validateTranscodeDimensions(width, height int) error {
	if width < 0 || height < 0 {
		return fmt.Errorf("video transcode dimensions must not be negative")
	}
	if width > maxTranscodeDimension || height > maxTranscodeDimension {
		return fmt.Errorf("video transcode dimensions exceed 8192 pixels per axis or 33554432 pixels total")
	}
	if width > 0 && height > 0 && width > maxTranscodePixels/height {
		return fmt.Errorf("video transcode dimensions exceed 8192 pixels per axis or 33554432 pixels total")
	}
	return nil
}

func validateEncodingInput(job EncodingJob) error {
	if job.LiveInput != nil {
		if err := validateLiveInput(*job.LiveInput); err != nil {
			return err
		}
		if !safePath(job.OutputPath) {
			return fmt.Errorf("encoding paths must be non-empty local paths without control characters or leading dashes")
		}
	} else if !safePath(job.InputPath) || !safePath(job.OutputPath) {
		return fmt.Errorf("encoding paths must be non-empty local paths without control characters or leading dashes")
	}
	if !safeContainer(job.InputContainer) {
		return fmt.Errorf("unsupported input container %q", job.InputContainer)
	}
	if job.OutputFrameRate < 0 || math.IsNaN(job.OutputFrameRate) || math.IsInf(job.OutputFrameRate, 0) {
		return fmt.Errorf("output frame rate must be finite and non-negative")
	}
	return nil
}

func validateEncodingSharedFacts(job EncodingJob) error {
	if job.SubtitleStream != nil {
		if job.SubtitleStream.InputIndex < 0 || job.SubtitleStream.Index < 0 || job.SubtitleStream.SubtitleIndex < 0 {
			return fmt.Errorf("subtitle input and stream indexes must be non-negative")
		}
		if job.SubtitleStream.BurnIn && !isPGSSubtitle(job.SubtitleStream) && job.SubtitleStream.IsExternal && !safePath(job.SubtitleStream.Path) {
			return fmt.Errorf("external text subtitle burn-in requires a resolved local subtitle path")
		}
	}
	if job.StartOffsetTicks < 0 {
		return fmt.Errorf("start offset must not be negative")
	}
	return nil
}

func validateAudioOutput(job EncodingJob, caps Capabilities) error {
	if job.AudioStream.Codec == "" {
		if job.OutputAudioCodec != "" || job.OutputAudioBitrate != 0 || job.OutputAudioChannels != 0 || job.OutputAudioSampleRate != 0 {
			return fmt.Errorf("audio output settings require an input audio stream")
		}
		return nil
	}
	if job.OutputAudioCodec != "copy" && job.OutputAudioCodec != "aac" {
		return fmt.Errorf("unsupported progressive audio encoder %q", job.OutputAudioCodec)
	}
	if job.DownmixAudioBoost != nil && (math.IsNaN(*job.DownmixAudioBoost) || math.IsInf(*job.DownmixAudioBoost, 0)) {
		return fmt.Errorf("downmix audio boost must be finite")
	}
	if job.OutputAudioSampleRate < 0 {
		return fmt.Errorf("output audio sample rate must not be negative")
	}
	if job.OutputAudioCodec == "aac" && aacEncoder(caps) == "" {
		return fmt.Errorf("no supported AAC encoder is available")
	}
	if job.OutputAudioCodec != "copy" && job.OutputAudioBitrate <= 0 {
		return fmt.Errorf("audio transcode requires a positive bitrate")
	}
	return nil
}

func appendInputArgs(args []string, job EncodingJob) []string {
	if job.StartOffsetTicks != 0 {
		args = append(args, "-ss", formatTicks(job.StartOffsetTicks))
	}
	if job.OutputVideoCodec == "copy" {
		args = append(args, "-fflags", "+genpts")
	}
	return append(args, "-i", "file:"+job.InputPath)
}

func appendMapArgs(args []string, job EncodingJob) []string {
	args = append(args, "-map", streamSpecifier(job.VideoStream.Index))
	if job.AudioStream.Codec != "" {
		args = append(args, "-map", streamSpecifier(job.AudioStream.Index))
	}
	if job.SubtitleStream == nil {
		return append(args, "-map", "-0:s")
	}
	return args
}

func appendVideoArgs(args []string, job EncodingJob, caps Capabilities) []string {
	if job.OutputVideoCodec == "copy" {
		args = append(args, "-codec:v:0", "copy")
		if job.OutputContainer == "ts" && strings.EqualFold(job.VideoStream.Codec, "h264") && job.VideoStream.NalLengthSize != "0" {
			args = append(args, "-bsf:v", "h264_mp4toannexb")
		}
		return appendTimestampArgs(args, job.CopyTimestamps)
	}

	if isPGSSubtitle(job.SubtitleStream) && job.SubtitleStream.BurnIn {
		args = append(args, "-map", "-0:"+strconv.Itoa(job.VideoStream.Index))
	}
	args = append(args, "-codec:v:0", h264Encoder(job, caps))
	if job.OutputFrameRate > 0 {
		args = append(args, "-r", formatFrameRate(job.OutputFrameRate))
	}
	args = append(args, "-force_key_frames", "expr:gte(t,n_forced*"+strconv.Itoa(job.VideoQuality.ForcedKeyFrameSeconds)+")")
	args = appendFilterArgs(args, job, caps)
	if job.CopyTimestamps {
		args = appendTimestampArgs(args, true)
	}
	args = append(args, "-preset", job.VideoQuality.Preset, "-crf", strconv.Itoa(job.VideoQuality.CRF))
	if job.OutputVideoBitrate > 0 {
		args = append(args, "-maxrate", strconv.Itoa(job.OutputVideoBitrate), "-bufsize", strconv.Itoa(job.OutputVideoBitrate*2))
	}
	return append(args, "-x264opts:0", job.VideoQuality.X264Options)
}

func appendFilterArgs(args []string, job EncodingJob, caps Capabilities) []string {
	filter := progressiveVideoFilter(job, caps)
	if isPGSSubtitle(job.SubtitleStream) && job.SubtitleStream.BurnIn {
		return append(args, "-filter_complex", embeddedBitmapOverlayFilter(job, filter))
	}
	return append(args, "-vf", filter)
}

func progressiveVideoFilter(job EncodingJob, caps Capabilities) string {
	filter := colorFilter(job.VideoStream) + "," + swScaleFilter(job)
	if isSwTonemapAvailable(job.VideoStream, caps) {
		filter += "," + softwareTonemapFilter(job.Hardware, "yuv420p")
	} else {
		filter += ",format=yuv420p"
	}
	if job.SubtitleStream != nil && job.SubtitleStream.BurnIn && !isPGSSubtitle(job.SubtitleStream) {
		filter += "," + subtitleFilterFileSource(subtitleSourcePath(job)) + ",setpts=PTS -" + formatRoundedSeconds(job.StartOffsetTicks) + "/TB"
	}
	return filter
}

func embeddedBitmapOverlayFilter(job EncodingJob, videoFilter string) string {
	subtitle := strconv.Itoa(job.SubtitleStream.Index)
	subtitleInput := strconv.Itoa(job.SubtitleStream.InputIndex)
	video := strconv.Itoa(job.VideoStream.Index)
	source := "[" + subtitleInput + ":" + subtitle + "]"
	if job.OutputWidth <= 0 || job.OutputHeight <= 0 {
		// Without an output size crop rejects zero dimensions. Keep the
		// subtitle canvas and center it on the encoded frame.
		return source + "scale[sub];[0:" + video + "]" + videoFilter + "[main];[main][sub]overlay=(main_w-overlay_w)/2:(main_h-overlay_h)/2:eof_action=pass:repeatlast=0"
	}
	width := strconv.Itoa(job.OutputWidth)
	height := strconv.Itoa(job.OutputHeight)
	return source + "scale,scale=-1:" + height + ":fast_bilinear,crop,pad=max(" + width + "\\,iw):max(" + height + "\\,ih):(ow-iw)/2:(oh-ih)/2:black@0,crop=" + width + ":" + height + "[sub];[0:" + video + "]" + videoFilter + "[main];[main][sub]overlay=eof_action=pass:repeatlast=0"
}

func appendTimestampArgs(args []string, copyTimestamps bool) []string {
	if !copyTimestamps {
		return args
	}
	return append(args, "-copyts", "-avoid_negative_ts", "disabled", "-start_at_zero")
}

func appendAudioArgs(args []string, job EncodingJob, caps Capabilities) []string {
	if job.AudioStream.Codec == "" {
		return args
	}
	codec := job.OutputAudioCodec
	if codec == "aac" {
		codec = aacEncoder(caps)
	}
	args = append(args, "-codec:a:0", codec)
	if codec == "copy" {
		return args
	}
	if job.OutputAudioChannels > 0 {
		args = append(args, "-ac", strconv.Itoa(job.OutputAudioChannels))
	}
	args = append(args, "-ab", strconv.Itoa(job.OutputAudioBitrate))
	if job.OutputAudioChannels == 2 && job.AudioStream.Channels > 2 {
		filter := ""
		if job.DownmixAudioBoost != nil && *job.DownmixAudioBoost != 1 {
			filter = "volume=" + strconv.FormatFloat(*job.DownmixAudioBoost, 'f', -1, 64)
		}
		if job.SubtitleStream != nil && job.SubtitleStream.BurnIn && !isPGSSubtitle(job.SubtitleStream) {
			if filter != "" {
				filter += ","
			}
			filter += "asetpts=PTS-" + formatRoundedSeconds(job.StartOffsetTicks) + "/TB"
		}
		if filter != "" {
			return append(args, "-af", filter)
		}
	}
	if job.SubtitleStream != nil && job.SubtitleStream.BurnIn && !isPGSSubtitle(job.SubtitleStream) {
		return append(args, "-af", "asetpts=PTS-"+formatRoundedSeconds(job.StartOffsetTicks)+"/TB")
	}
	return args
}

func appendOutputArgs(args []string, job EncodingJob) []string {
	if job.OutputContainer == "mp4" || job.OutputContainer == "fmp4" {
		args = append(args, "-f", "mp4", "-movflags", "frag_keyframe+empty_moov+delay_moov")
	}
	return append(args, "-y", job.OutputPath)
}

func colorFilter(stream VideoStream) string {
	if isHDR(stream) {
		transfer := strings.ToLower(stream.ColorTransfer)
		if stream.DolbyVisionProfile != 0 || transfer == "" || transfer == "dv" || transfer == "dovi" {
			transfer = "smpte2084"
		}
		return "setparams=color_primaries=bt2020:color_trc=" + transfer + ":colorspace=bt2020nc"
	}
	return "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709"
}

func isHDR(stream VideoStream) bool {
	transfer := strings.ToLower(stream.ColorTransfer)
	return transfer == "smpte2084" || transfer == "arib-std-b67" || transfer == "hlg" || stream.DolbyVisionProfile != 0 || strings.Contains(strings.ToLower(stream.CodecTag), "dv")
}

func isPGSSubtitle(stream *SubtitleStream) bool {
	return stream != nil && (strings.EqualFold(stream.Codec, "pgs") || strings.EqualFold(stream.Codec, "pgssub") || strings.EqualFold(stream.Codec, "hdmv_pgs_subtitle"))
}

func h264Encoder(job EncodingJob, caps Capabilities) string {
	if job.OutputVideoCodec == "h264" {
		return caps.H264Encoder
	}
	return job.OutputVideoCodec
}

func aacEncoder(caps Capabilities) string {
	for _, preferred := range [...]string{"aac_at", "libfdk_aac", "aac"} {
		for _, available := range caps.AACEncoders {
			if available == preferred {
				return preferred
			}
		}
	}
	return ""
}

func safeContainer(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.', r == ',':
			continue
		default:
			return false
		}
	}
	return true
}

func streamSpecifier(index int) string { return "0:" + strconv.Itoa(index) }

func formatTicks(ticks int64) string {
	milliseconds := ticks / 10_000
	hours := milliseconds / 3_600_000
	milliseconds %= 3_600_000
	minutes := milliseconds / 60_000
	milliseconds %= 60_000
	seconds := milliseconds / 1_000
	milliseconds %= 1_000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, seconds, milliseconds)
}

func formatFrameRate(rate float64) string {
	return strconv.FormatFloat(rate, 'f', -1, 64)
}

func formatRoundedSeconds(ticks int64) string {
	const ticksPerSecond int64 = 10_000_000
	const halfSecondTicks int64 = ticksPerSecond / 2

	seconds := ticks / ticksPerSecond
	remainder := ticks % ticksPerSecond
	if remainder > halfSecondTicks || (remainder == halfSecondTicks && seconds%2 != 0) {
		seconds++
	} else if remainder < -halfSecondTicks || (remainder == -halfSecondTicks && seconds%2 != 0) {
		seconds--
	}
	return strconv.FormatInt(seconds, 10)
}

func safePath(path string) bool {
	if path == "" || path[0] == '-' {
		return false
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func safeX264Options(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == ':', r == '=':
			continue
		default:
			return false
		}
	}
	return true
}

func isX264Preset(preset string) bool {
	switch preset {
	case "ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow", "placebo":
		return true
	default:
		return false
	}
}

// EscapeSubtitleFilterPath escapes path for a quoted subtitles filter
// option (f= or fontsdir=).
func EscapeSubtitleFilterPath(path string) string {
	return subtitleFilterPathEscaper.Replace(path)
}

func subtitleFilterFileSource(path string) string {
	return "subtitles=f='" + EscapeSubtitleFilterPath(path) + "'"
}

// subtitleSourcePath names the file a burn-in reads: the subtitle's own path,
// the media file, or a live input's broker URL. A live job has no input path.
func subtitleSourcePath(job EncodingJob) string {
	if job.SubtitleStream != nil && job.SubtitleStream.Path != "" {
		return job.SubtitleStream.Path
	}
	if job.InputPath != "" {
		return job.InputPath
	}
	if job.LiveInput != nil {
		return job.LiveInput.URL
	}
	return ""
}

// textSubtitleFileSource returns the subtitles filter source operand of a
// text subtitle: the external file, the cached extraction, or the media
// source.
func textSubtitleFileSource(job EncodingJob) string {
	subtitle := job.SubtitleStream
	switch {
	case subtitle.IsExternal:
		return subtitleFilterFileSource(subtitle.Path)
	case subtitle.ExtractedPath != "":
		return subtitleFilterFileSource(subtitle.ExtractedPath)
	default:
		return subtitleFilterFileSource(subtitleSourcePath(job))
	}
}

// textSubtitleBurnFilter ports GetTextSubtitlesFilter for the HLS path,
// which always copies timestamps and so never adds setpts. A stream that is
// read from the media file names its subtitle index with si=.
func textSubtitleBurnFilter(job EncodingJob, alpha bool) string {
	subtitle := job.SubtitleStream
	filter := textSubtitleFileSource(job)
	if !subtitle.IsExternal && subtitle.ExtractedPath == "" {
		filter += ":si=" + strconv.Itoa(subtitle.SubtitleIndex)
	}
	if alpha {
		filter += ":alpha=1:sub2video=1"
	}
	if subtitle.FontsDir != "" {
		filter += ":fontsdir='" + EscapeSubtitleFilterPath(subtitle.FontsDir) + "'"
	}
	return filter
}
