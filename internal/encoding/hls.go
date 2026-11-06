// SPDX-License-Identifier: GPL-2.0-only

package encoding

import (
	"fmt"
	"path"
	"runtime"
	"strconv"
	"strings"
)

const (
	hlsSegmentDurationSeconds = 3
	hlsMaxDelayMicroseconds   = 5_000_000
	hlsMuxingQueueSize        = 2048
)

// HLSJob contains the encoding plan and HLS-specific playlist controls.
type HLSJob struct {
	EncodingJob
	SegmentLengthSeconds int
	StartNumber          int
	// RunTimeTicks bounds the input seek. Zero leaves the seek unclamped.
	RunTimeTicks      int64
	Live              bool
	DirectDolbyVision bool
	// RemoveDynamicHDR removes Dolby Vision or HDR10+ metadata from a video
	// copy with a bitstream filter (EncodingHelper.GetBitStreamArgs,
	// EncodingHelper.cs:1487-1556). The caller decides the plan. The filter
	// applies also to a source with an unknown NAL length size, where
	// upstream adds no bitstream filter (D-84).
	RemoveDynamicHDR DynamicHDRRemoval
	// KeyframeCuts makes a video-copy job cut a segment at every source
	// keyframe (-hls_time 0), so that output segment N is source GOP N
	// whatever the start point. StartOffsetTicks is then the exact input
	// seek, inside the start GOP; the caller chooses it from the keyframes.
	KeyframeCuts bool
	// KeyframeStartTicks is the start of the start GOP of a keyframe-cut
	// job. The audio of a restarted job starts there, like the copied video
	// (deadfin#799).
	KeyframeStartTicks int64
	// PadAudioStart makes an audio transcode start at the job start with
	// silence when the source audio stream starts later than the video
	// (deadfin#681, D-82). Stream copy cannot pad, so it requires an audio
	// transcode.
	PadAudioStart bool
}

// DynamicHDRRemoval ports DynamicHdrMetadataRemovalPlan.
type DynamicHDRRemoval int

const (
	// RemoveNoDynamicHDR keeps the dynamic HDR metadata.
	RemoveNoDynamicHDR DynamicHDRRemoval = iota
	// RemoveDolbyVision strips the Dolby Vision RPU and enhancement layer.
	RemoveDolbyVision
	// RemoveHDR10Plus strips the HDR10+ SEI messages.
	RemoveHDR10Plus
)

// hlsPadSampleRate is the pad filter output rate when the job sets none.
const hlsPadSampleRate = 48_000

// BuildHLSArgs builds logical ffmpeg argv tokens for a video-on-demand HLS
// playlist. The returned tokens are ready for exec; they are not shell escaped.
func BuildHLSArgs(job HLSJob, caps Capabilities) ([]string, error) {
	if err := validateHLSJob(job, caps); err != nil {
		return nil, err
	}

	encodingJob := job.EncodingJob
	var plan videoPlan
	transcodeVideo := !isAudioOnlyHLSJob(encodingJob) && encodingJob.OutputVideoCodec != "copy"
	if transcodeVideo {
		plan = planVideo(encodingJob, caps, func(format string) string { return hlsVideoFilter(encodingJob, caps, format) })
	}
	args := make([]string, 0, 64)
	args = appendHLSInputArgs(args, job, plan)
	args = append(args, "-map_metadata", "-1", "-map_chapters", "-1", "-threads", "0")
	args = appendHLSMapArgs(args, encodingJob)
	if !isAudioOnlyHLSJob(encodingJob) {
		args = appendHLSVideoArgs(args, job, caps, plan)
		if subtitle := encodingJob.SubtitleStream; subtitle != nil && encodingJob.OutputVideoCodec != "copy" && (!subtitle.IsExternal || !isHLSGraphicalSubtitle(subtitle)) {
			args = append(args, "-start_at_zero")
		}
	}
	if job.Live && encodingJob.OutputContainer == "ts" {
		args = append(args, "-flags", "-global_header")
	}
	args = appendHLSAudioArgs(args, encodingJob, caps, job.PadAudioStart, hlsAudioStartTicks(job))
	args = appendHLSTimestampArgs(args)
	return appendHLSOutputArgs(args, job, caps), nil
}

// HLSSubtitleFilterSource returns the exact opaque subtitles-filter operand for
// a text subtitle burn. An empty result means that no subtitle descriptor is
// needed.
func HLSSubtitleFilterSource(job HLSJob) string {
	if isAudioOnlyHLSJob(job.EncodingJob) || job.OutputVideoCodec == "copy" || job.SubtitleStream == nil || !job.SubtitleStream.BurnIn || isHLSGraphicalSubtitle(job.SubtitleStream) {
		return ""
	}
	return textSubtitleFileSource(job.EncodingJob)
}

// HLSEmbeddedSubtitleFilterSource returns the text subtitle operand only when
// it names the primary media source.
func HLSEmbeddedSubtitleFilterSource(job HLSJob) string {
	if job.SubtitleStream != nil && (job.SubtitleStream.IsExternal || job.SubtitleStream.ExtractedPath != "") {
		return ""
	}
	return HLSSubtitleFilterSource(job)
}

func validateHLSJob(job HLSJob, caps Capabilities) error {
	if job.SegmentLengthSeconds <= 0 {
		return fmt.Errorf("hls segment length must be positive")
	}
	if job.KeyframeCuts && (job.Live || job.OutputVideoCodec != "copy") {
		return fmt.Errorf("hls keyframe cuts require a video-on-demand video copy")
	}
	if job.StartNumber < 0 {
		return fmt.Errorf("hls start number must not be negative")
	}
	if job.PadAudioStart && (job.AudioStream.Codec == "" || job.OutputAudioCodec == "" || job.OutputAudioCodec == "copy") {
		return fmt.Errorf("hls audio start padding requires an audio transcode")
	}
	if isHLSDolbyAudioEncoder(job.OutputAudioCodec) {
		// A late AC3 or E-AC3 track of a client that accepts no AAC is
		// re-encoded to its own codec with ffmpeg's native encoder (D-82).
		// No other HLS job encodes these codecs (D-50).
		if !job.PadAudioStart {
			return fmt.Errorf("hls %s encoding requires audio start padding", job.OutputAudioCodec)
		}
		if job.OutputAudioBitrate <= 0 {
			return fmt.Errorf("audio transcode requires a positive bitrate")
		}
		// The shared audio validator knows only copy and AAC. job is a
		// copy, so the rest of the checks see the stream as a copy.
		job.OutputAudioCodec = "copy"
	}
	if job.OutputContainer != "ts" && job.OutputContainer != "fmp4" {
		return fmt.Errorf("unsupported HLS segment container %q", job.OutputContainer)
	}
	if (job.VideoStream.Codec == "") != (job.OutputVideoCodec == "") {
		return fmt.Errorf("hls video stream and output codec must be both set or both empty")
	}
	if isAudioOnlyHLSJob(job.EncodingJob) {
		return validateAudioOnlyHLSJob(job.EncodingJob, caps)
	}
	if job.OutputVideoCodec == "copy" && requiresDirectDolbyVision(job.VideoStream) && !job.DirectDolbyVision && job.RemoveDynamicHDR != RemoveDolbyVision {
		return fmt.Errorf("hls Dolby Vision copy without a compatible base layer requires a direct Dolby Vision request")
	}
	if subtitle := job.SubtitleStream; subtitle != nil && subtitle.BurnIn && isHLSGraphicalSubtitle(subtitle) {
		if subtitle.InputIndex > 0 {
			return fmt.Errorf("graphical subtitle burn-in from a secondary input is unsupported for HLS")
		}
		if subtitle.IsExternal || !isHLSEmbeddedBurnSubtitle(subtitle) {
			return fmt.Errorf("external or unsupported graphical subtitle burn-in is unsupported for HLS")
		}
	}
	return validateVideoJob(job.EncodingJob, caps, true)
}

func isHLSDolbyAudioEncoder(codec string) bool {
	return codec == "ac3" || codec == "eac3"
}

func isAudioOnlyHLSJob(job EncodingJob) bool {
	return job.VideoStream.Codec == "" && job.OutputVideoCodec == ""
}

func validateAudioOnlyHLSJob(job EncodingJob, caps Capabilities) error {
	if err := validateEncodingInput(job); err != nil {
		return err
	}
	if job.AudioStream.Index < 0 {
		return fmt.Errorf("stream indexes must be non-negative")
	}
	if err := validateEncodingSharedFacts(job); err != nil {
		return err
	}
	if job.SubtitleStream != nil && job.SubtitleStream.BurnIn {
		return fmt.Errorf("audio-only HLS cannot burn subtitles")
	}
	if job.AudioStream.Codec == "" {
		return fmt.Errorf("audio-only HLS requires an input audio stream")
	}
	return validateAudioOutput(job, caps)
}

func appendHLSMapArgs(args []string, job EncodingJob) []string {
	if isAudioOnlyHLSJob(job) {
		return append(args, "-map", streamSpecifier(job.AudioStream.Index))
	}
	if job.AudioStream.Codec == "" {
		args = append(args, "-map", streamSpecifier(job.VideoStream.Index))
		args = append(args, "-map", "-0:a")
		if job.SubtitleStream == nil {
			return append(args, "-map", "-0:s")
		}
		return args
	}
	return appendMapArgs(args, job)
}

// appendHLSInputArgs places the seek, the input flags, the hardware device
// and decoder arguments, and the input, in the order of the pinned
// GetInputModifier and GetInputArgument (EncodingHelper.cs:1245-1349,
// 7288-7400).
func appendHLSInputArgs(args []string, job HLSJob, plan videoPlan) []string {
	if job.LiveInput != nil {
		args = appendLiveInputArgs(args, *job.LiveInput, job.OutputVideoCodec == "copy", plan.inputArgs)
		if plan.hwDecoder {
			args = append(args, "-noautoscale")
		}
		return args
	}
	if job.StartOffsetTicks > 0 {
		if job.KeyframeCuts {
			// The input seek lands on the start keyframe, and the copied
			// video starts there. Accurate seek would discard the decoded
			// audio before the seek point, so a restarted job would leave an
			// audio gap of up to 0.5 s at its first segment. Without it the
			// audio starts at the keyframe on the frame grid of a job that
			// played through (deadfin#799).
			args = append(args, "-noaccurate_seek")
		}
		args = append(args, "-ss", formatTicks(hlsSeekTicks(job)))
	}
	// Stream copy regenerates missing presentation timestamps
	// (EncodingHelper.cs:7382-7385).
	if job.OutputVideoCodec == "copy" {
		args = append(args, "-fflags", "+genpts")
	}
	args = append(args, plan.inputArgs...)
	args = append(args, "-i", "file:"+job.InputPath)
	if plan.hwDecoder {
		args = append(args, "-noautoscale")
	}
	return args
}

// hlsSeekTicks ports EncodingHelper.GetFastSeekCommandLineParameter
// (EncodingHelper.cs:3046-3072). A remux segment starts on a keyframe, and an
// input seek to that exact time lands on the previous keyframe, so video copy
// seeks 0.5 s later. The seek stays at least 5 s before the end of the media.
func hlsSeekTicks(job HLSJob) int64 {
	const (
		remuxSeekOffsetTicks = 5_000_000
		endMarginTicks       = 50_000_000
	)
	seek := job.StartOffsetTicks
	if job.KeyframeCuts {
		return seek
	}
	if !isAudioOnlyHLSJob(job.EncodingJob) && job.OutputVideoCodec == "copy" {
		seek += remuxSeekOffsetTicks
	}
	if job.RunTimeTicks > 0 {
		seek = min(seek, max(job.RunTimeTicks-endMarginTicks, 0))
	}
	return max(seek, 0)
}

func appendHLSVideoArgs(args []string, job HLSJob, caps Capabilities, plan videoPlan) []string {
	if (isHLSEmbeddedBurnSubtitle(job.SubtitleStream) && job.SubtitleStream.BurnIn) || (job.OutputVideoCodec != "copy" && len(plan.overlay) != 0) {
		args = append(args, "-map", "-0:"+strconv.Itoa(job.VideoStream.Index))
	}
	if job.OutputVideoCodec == "copy" {
		args = append(args, "-codec:v:0", "copy")
		switch strings.ToLower(job.VideoStream.Codec) {
		case "h264":
			if job.VideoStream.NalLengthSize != "0" {
				args = append(args, "-bsf:v", "h264_mp4toannexb")
			}
		case "hevc", "h265":
			if isDOVIVideo(job.VideoStream) && job.DirectDolbyVision && job.RemoveDynamicHDR != RemoveDolbyVision {
				tag := "dvh1"
				if isDOVIWithHLG(job.VideoStream) {
					tag = "hvc1"
				}
				args = append(args, "-tag:v:0", tag, "-strict", "-2")
			} else {
				args = append(args, "-tag:v:0", "hvc1")
			}
			var filters []string
			if job.VideoStream.NalLengthSize != "0" {
				filters = append(filters, "hevc_mp4toannexb")
			}
			switch job.RemoveDynamicHDR {
			case RemoveDolbyVision:
				if caps.SupportsBSFOption("hevc_metadata=remove_dovi") {
					filters = append(filters, "hevc_metadata=remove_dovi=1")
				} else {
					filters = append(filters, "dovi_rpu=strip=1")
				}
			case RemoveHDR10Plus:
				filters = append(filters, "hevc_metadata=remove_hdr10plus=1")
			}
			if len(filters) != 0 {
				args = append(args, "-bsf:v", strings.Join(filters, ","))
			}
		case "av1":
			if isDOVIVideo(job.VideoStream) && job.DirectDolbyVision && job.RemoveDynamicHDR != RemoveDolbyVision {
				args = append(args, "-tag:v:0", "dav1", "-strict", "-2")
			}
			switch job.RemoveDynamicHDR {
			case RemoveDolbyVision:
				if caps.SupportsBSFOption("av1_metadata=remove_dovi") {
					args = append(args, "-bsf:v", "av1_metadata=remove_dovi=1")
				} else {
					args = append(args, "-bsf:v", "dovi_rpu=strip=1")
				}
			case RemoveHDR10Plus:
				args = append(args, "-bsf:v", "av1_metadata=remove_hdr10plus=1")
			}
		}
		return append(args, "-start_at_zero")
	}

	if plan.encoder != "" && plan.encoder != "libx264" && plan.encoder != "h264" {
		args = append(args, "-codec:v:0", plan.encoder)
		if normalizeVideoCodec(job.OutputVideoCodec) == "hevc" {
			// Prefer hvc1 to hev1 (DynamicHlsController.GetVideoArguments).
			args = append(args, "-tag:v:0", "hvc1")
		}
		args = append(args, hardwareEncoderArgs(job, caps, plan.encoder)...)
		return appendHLSFilterArgs(args, job.EncodingJob, plan)
	}
	args = append(args, "-codec:v:0", h264Encoder(job.EncodingJob, caps))
	if job.OutputVideoProfile != "" {
		args = append(args, "-profile:v:0", h264OutputProfile(job.OutputVideoProfile))
	}
	if job.OutputVideoLevel > 0 {
		args = append(args, "-level:v:0", formatH264Level(job.OutputVideoLevel))
	}
	if job.OutputFrameRate > 0 {
		args = append(args, "-r", formatFrameRate(job.OutputFrameRate))
	}
	args = append(args, "-preset", job.VideoQuality.Preset, "-crf", strconv.Itoa(job.VideoQuality.CRF))
	if job.OutputVideoBitrate > 0 {
		args = append(args, "-maxrate", strconv.Itoa(job.OutputVideoBitrate), "-bufsize", strconv.Itoa(job.OutputVideoBitrate*2))
	}
	args = append(args,
		"-x264opts:0", job.VideoQuality.X264Options,
		"-force_key_frames:0", "expr:gte(t,n_forced*"+strconv.Itoa(job.SegmentLengthSeconds)+")",
		"-sc_threshold:v:0", "0",
	)
	return appendHLSFilterArgs(args, job.EncodingJob, plan)
}

// appendHLSFilterArgs joins the planned filter lists as
// GetVideoProcessingFilterParam does (EncodingHelper.cs:6231-6344).
func appendHLSFilterArgs(args []string, job EncodingJob, plan videoPlan) []string {
	main := joinFilters(plan.mainFilters)
	if len(plan.overlay) == 0 {
		if isHLSEmbeddedBurnSubtitle(job.SubtitleStream) && job.SubtitleStream.BurnIn {
			return append(args, "-filter_complex", embeddedBitmapOverlayFilter(job, main))
		}
		return append(args, "-vf", main)
	}
	video := "[0:" + strconv.Itoa(job.VideoStream.Index) + "]"
	sub := joinFilters(plan.subFilters) + "[sub];"
	if !plan.textSubtitle {
		sub = "[" + strconv.Itoa(job.SubtitleStream.InputIndex) + ":" + strconv.Itoa(job.SubtitleStream.Index) + "]" + sub
	}
	graph := sub + video
	if main != "" {
		graph += main + "[main];[main]"
	}
	return append(args, "-filter_complex", graph+"[sub]"+joinFilters(plan.overlay))
}

func joinFilters(filters []string) string {
	kept := make([]string, 0, len(filters))
	for _, filter := range filters {
		if filter != "" {
			kept = append(kept, filter)
		}
	}
	return strings.Join(kept, ",")
}

func hlsVideoFilter(job EncodingJob, caps Capabilities, format string) string {
	filter := colorFilter(job.VideoStream) + "," + swScaleFilter(job)
	if isSwTonemapAvailable(job.VideoStream, caps) {
		tonemapFormat := format
		// tonemapx needs yuv420p10 input for Dolby Vision reshaping.
		if videoRangeType(job.VideoStream) == "DOVI" {
			tonemapFormat = "yuv420p"
		}
		filter += "," + softwareTonemapFilter(job.Hardware, tonemapFormat)
	} else {
		filter += ",format=" + format
	}
	if subtitle := job.SubtitleStream; subtitle != nil && subtitle.BurnIn && !isHLSGraphicalSubtitle(subtitle) {
		filter += "," + textSubtitleBurnFilter(job, false)
	}
	return filter
}

func h264OutputProfile(profile string) string {
	switch strings.ToLower(strings.ReplaceAll(profile, " ", "")) {
	case "constrainedbaseline", "baseline", "extended":
		return "baseline"
	case "main":
		return "main"
	default:
		return "high"
	}
}

func formatH264Level(level int) string {
	return strconv.Itoa(level/10) + "." + strconv.Itoa(level%10)
}

func isDOVIVideo(stream VideoStream) bool {
	return stream.DolbyVisionProfile != 0 || strings.Contains(strings.ToLower(stream.CodecTag), "dv")
}

func requiresDirectDolbyVision(stream VideoStream) bool {
	return isDOVIVideo(stream) && (stream.DolbyVisionProfile == 5 || stream.DolbyVisionBLCompatibilityID == 0)
}

func isDOVIWithHLG(stream VideoStream) bool {
	transfer := strings.ToLower(stream.ColorTransfer)
	if transfer == "hlg" || transfer == "arib-std-b67" {
		return true
	}
	return (stream.DolbyVisionProfile == 8 || stream.DolbyVisionProfile == 10) && stream.RPUFlag != 0 && stream.BLPresentFlag != 0 && stream.DolbyVisionBLCompatibilityID == 4
}

func isHLSGraphicalSubtitle(stream *SubtitleStream) bool {
	if stream == nil {
		return false
	}
	codec := strings.ToLower(stream.Codec)
	return isPGSSubtitle(stream) || strings.Contains(codec, "dvdsub") || strings.Contains(codec, "vobsub") || strings.Contains(codec, "dvbsub") || codec == "sup" || codec == "sub"
}

// isHLSEmbeddedBurnSubtitle reports whether the stream is an embedded
// graphical subtitle that the HLS bitmap-overlay path can burn in. PGS,
// DVD, and VobSub share the same overlay graph. External graphical
// subtitles and other graphical codecs stay rejected by validateHLSJob.
func isHLSEmbeddedBurnSubtitle(stream *SubtitleStream) bool {
	return isPGSSubtitle(stream) || isDVDSubtitle(stream)
}

func isDVDSubtitle(stream *SubtitleStream) bool {
	if stream == nil {
		return false
	}
	codec := strings.ToLower(stream.Codec)
	return strings.Contains(codec, "dvdsub") || strings.Contains(codec, "vobsub")
}

func appendHLSTimestampArgs(args []string) []string {
	return append(args, "-copyts", "-avoid_negative_ts", "disabled")
}

func appendHLSAudioArgs(args []string, job EncodingJob, caps Capabilities, padStart bool, startTicks int64) []string {
	if job.AudioStream.Codec == "" {
		return args
	}

	codec := job.OutputAudioCodec
	if codec == "aac" {
		codec = aacEncoder(caps)
	}
	args = append(args, "-codec:a:0", codec)
	actualCodec := codec
	if codec == "copy" {
		actualCodec = job.AudioStream.Codec
	}
	if requiresHLSFMP4ExperimentalAudio(actualCodec, job, caps) {
		args = append(args, "-strict", "-2")
	}
	args = appendHLSAudioBitstreamArgs(args, job, caps, "-bsf:a")
	if codec == "copy" {
		return args
	}
	if job.OutputAudioChannels > 0 {
		args = append(args, "-ac", strconv.Itoa(job.OutputAudioChannels))
	}
	args = append(args, "-ab", strconv.Itoa(job.OutputAudioBitrate))
	if job.OutputAudioSampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(job.OutputAudioSampleRate))
	}
	filters := make([]string, 0, 2)
	if filter := hlsScopedAudioFilter(job); filter != "" {
		filters = append(filters, filter)
	}
	if padStart {
		filters = append(filters, hlsAudioStartPadFilter(job, startTicks))
	}
	if len(filters) != 0 {
		args = append(args, "-af", strings.Join(filters, ","))
	}
	return args
}

// hlsAudioStartPadFilter fills the time between the job start and the first
// source audio frame with silence. The timestamps are the source timestamps
// (-copyts), so first_pts is the job start in samples of the output rate.
// async=1 also fills later gaps in the source track.
func hlsAudioStartPadFilter(job EncodingJob, startTicks int64) string {
	rate := job.OutputAudioSampleRate
	if rate <= 0 {
		rate = hlsPadSampleRate
	}
	firstPTS := max(startTicks, 0) * int64(rate) / 10_000_000
	return "aresample=osr=" + strconv.Itoa(rate) + ":async=1:first_pts=" + strconv.FormatInt(firstPTS, 10)
}

// hlsAudioStartTicks is the media time where the audio of the job starts: the
// input seek, or the start GOP of a keyframe-cut job, which seeks without
// accurate seek.
func hlsAudioStartTicks(job HLSJob) int64 {
	if job.KeyframeCuts && job.StartOffsetTicks > 0 {
		return job.KeyframeStartTicks
	}
	return job.StartOffsetTicks
}

func appendHLSAudioBitstreamArgs(args []string, job EncodingJob, caps Capabilities, flag string) []string {
	filters := hlsAudioBitstreamFilters(job, caps)
	if filters == "" {
		return args
	}
	return append(args, flag, filters)
}

func hlsAudioBitstreamFilters(job EncodingJob, caps Capabilities) string {
	if job.AudioStream.Codec == "" {
		return ""
	}

	filters := make([]string, 0, 2)
	if job.OutputAudioCodec == "copy" && job.OutputVideoCodec != "copy" && job.StartOffsetTicks > 0 && !strings.EqualFold(job.InputContainer, "wtv") && caps.EncoderMajorVersion >= 5 {
		filters = append(filters, "noise=drop=lt(pts*tb\\,"+formatHLSSeconds(job.StartOffsetTicks)+")")
	}
	if job.OutputContainer == "fmp4" && strings.EqualFold(job.AudioStream.Codec, "aac") && isADTSContainer(job.InputContainer) {
		filters = append(filters, "aac_adtstoasc")
	}
	return strings.Join(filters, ",")
}

func hlsScopedAudioFilter(job EncodingJob) string {
	if job.OutputAudioChannels != 2 || job.AudioStream.Channels <= 2 {
		return ""
	}
	if job.DownmixAudioBoost == nil || *job.DownmixAudioBoost == 1 {
		return ""
	}
	return "volume=" + strconv.FormatFloat(*job.DownmixAudioBoost, 'f', -1, 64)
}

func formatHLSSeconds(ticks int64) string {
	return strconv.FormatFloat(float64(ticks)/10_000_000, 'f', 3, 64)
}

func isADTSContainer(container string) bool {
	switch strings.ToLower(container) {
	case "ts", "mpegts", "aac", "hls":
		return true
	default:
		return false
	}
}

// requiresHLSFMP4ExperimentalAudio ports the strictness rule of
// DynamicHlsController.GetAudioArguments (DynamicHlsController.cs:1669-1678):
// the mp4 muxer needs "-strict -2" for Opus, DTS, and TrueHD on every FFmpeg
// version, and for FLAC before FFmpeg 6.
func requiresHLSFMP4ExperimentalAudio(codec string, job EncodingJob, caps Capabilities) bool {
	if job.OutputContainer != "fmp4" {
		return false
	}
	switch strings.ToLower(codec) {
	case "opus", "dts", "truehd":
		return true
	case "flac":
		return caps.EncoderMajorVersion < 6
	default:
		return false
	}
}

func appendHLSOutputArgs(args []string, job HLSJob, caps Capabilities) []string {
	prefix := hlsSegmentPrefix(job.OutputPath)
	segmentType := "mpegts"
	segmentExtension := ".ts"
	if job.OutputContainer == "fmp4" {
		segmentType = "fmp4"
		segmentExtension = ".mp4"
	}

	args = append(args, "-max_muxing_queue_size", strconv.Itoa(hlsMuxingQueueSize))
	if job.PadAudioStart {
		// The pad starts only when the first source audio frame arrives. Until
		// then the muxer holds the video, so the first segments get the
		// leading silence and the init segment gets the audio decoder
		// configuration (dac3 for AC3).
		args = append(args, "-max_interleave_delta", "0")
	}
	args = append(args,
		"-f", "hls",
		"-max_delay", strconv.Itoa(hlsMaxDelayMicroseconds),
		"-hls_time", hlsTimeArg(job),
		"-hls_segment_type", segmentType,
	)
	if job.OutputContainer == "fmp4" {
		args = append(args, "-hls_fmp4_init_filename", hlsFMP4InitFilename(prefix))
	}
	args = append(args, "-start_number", strconv.Itoa(job.StartNumber))
	if job.Live {
		args = append(args, "-hls_base_url", hlsBaseURL(job.OutputPath))
	}
	playlistType := "vod"
	if job.Live {
		playlistType = "event"
	}
	args = append(args,
		"-hls_segment_filename", prefix+"%d"+segmentExtension,
		"-hls_playlist_type", playlistType,
		"-hls_list_size", "0",
	)
	if job.OutputContainer == "fmp4" {
		option := "-hls_segment_options"
		if caps.EncoderMajorVersion > 0 && caps.EncoderMajorVersion < 5 {
			option = "-hls_ts_options"
		}
		args = append(args, option, "movflags=+frag_discont")
	}
	return append(args, "-y", job.OutputPath)
}

// hlsTimeArg returns the muxer's target segment length. The HLS muxer cuts at
// the first keyframe at least hls_time*n after the first packet of the
// process, so a job started mid-file cuts on a grid that the advertised
// keyframe playlist does not use. Keyframe cuts use 0 to cut at every
// keyframe instead (deadfin#629, D-72).
func hlsTimeArg(job HLSJob) string {
	if job.KeyframeCuts {
		return "0"
	}
	return strconv.Itoa(job.SegmentLengthSeconds)
}

func hlsSegmentPrefix(playlistPath string) string {
	extension := path.Ext(playlistPath)
	if extension == "" {
		return playlistPath
	}
	return playlistPath[:len(playlistPath)-len(extension)]
}

func hlsBaseURL(playlistPath string) string {
	return "hls/" + hlsBaseName(hlsSegmentPrefix(playlistPath)) + "/"
}

func hlsBaseName(pathname string) string {
	return path.Base(strings.ReplaceAll(pathname, "\\", "/"))
}

func hlsFMP4InitFilename(prefix string) string {
	if runtime.GOOS == "windows" {
		return prefix + "-1.mp4"
	}
	return path.Base(prefix) + "-1.mp4"
}
