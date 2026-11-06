// SPDX-License-Identifier: GPL-2.0-only

package encoding

import (
	"fmt"
	"strconv"
	"strings"
)

// Hardware acceleration types. The values are the upstream
// HardwareAccelerationType member names (EncodingOptions.cs).
const (
	HardwareAccelerationNone  = "none"
	HardwareAccelerationQSV   = "qsv"
	HardwareAccelerationVAAPI = "vaapi"
	HardwareAccelerationNVENC = "nvenc"
)

// Device aliases match EncodingHelper.cs:52-57.
const (
	qsvAlias    = "qs"
	vaapiAlias  = "va"
	openclAlias = "ocl"
	cudaAlias   = "cu"
)

// Tonemapping holds the tone-mapping options of EncodingOptions.cs. The
// string values are the upstream enum member names.
type Tonemapping struct {
	Algorithm string
	Mode      string
	Range     string
	Desat     float64
	Peak      float64
	Param     float64
}

// DefaultTonemapping returns the upstream EncodingOptions defaults
// (EncodingOptions.cs:31-37).
func DefaultTonemapping() Tonemapping {
	return Tonemapping{Algorithm: "bt2390", Mode: "auto", Range: "auto", Peak: 100}
}

// HardwareOptions is the subset of EncodingOptions that selects the video
// decoder, encoder, and filter chain. The zero value selects software.
type HardwareOptions struct {
	Type                               string
	EnableHardwareEncoding             bool
	HardwareDecodingCodecs             []string
	EnableDecodingColorDepth10Hevc     bool
	EnableDecodingColorDepth10Vp9      bool
	EnableDecodingColorDepth10HevcRext bool
	EnableDecodingColorDepth12HevcRext bool
	PreferSystemNativeHwDecoder        bool
	EnableEnhancedNvdecDecoder         bool
	AllowHevcEncoding                  bool
	AllowAv1Encoding                   bool
	EnableIntelLowPowerH264HwEncoder   bool
	EnableTonemapping                  bool
	EnableVppTonemapping               bool
	Tonemapping                        Tonemapping
	VppTonemappingBrightness           float64
	VppTonemappingContrast             float64
	// VaapiDevice and QsvDevice are render node paths. The composition root
	// clears a path that does not exist, as EncodingHelper.GetVaapiDeviceArgs
	// does with File.Exists.
	VaapiDevice   string
	QsvDevice     string
	EncoderPreset string
}

func (options HardwareOptions) tonemapping() Tonemapping {
	if options.Tonemapping == (Tonemapping{}) {
		return DefaultTonemapping()
	}
	return options.Tonemapping
}

// videoPlan is the per-job result of the hardware decisions in
// EncodingHelper.cs: the encoder, the input device and decoder arguments,
// and the main/sub/overlay filter lists of Get*VidFilterChain.
type videoPlan struct {
	encoder      string
	inputArgs    []string
	hwDecoder    bool
	mainFilters  []string
	subFilters   []string
	overlay      []string
	textSubtitle bool
}

func (caps Capabilities) hasEncoder(name string) bool { return containsString(caps.Encoders, name) }
func (caps Capabilities) hasDecoder(name string) bool { return containsString(caps.Decoders, name) }
func (caps Capabilities) hasHWAccel(name string) bool { return containsString(caps.HWAccels, name) }
func (caps Capabilities) hasFilter(name string) bool  { return containsString(caps.Filters, name) }

func (caps Capabilities) versionAtLeast(major, minor, patch int) bool {
	if caps.EncoderMajorVersion != major {
		return caps.EncoderMajorVersion > major
	}
	if caps.EncoderMinorVersion != minor {
		return caps.EncoderMinorVersion > minor
	}
	return caps.EncoderPatchVersion >= patch
}

// isOpenclFullSupported ports EncodingHelper.IsOpenclFullSupported. Deadfin
// probes filter names, not filter options, so the named filters stand in for
// the TonemapOpenclBt2390 and OverlayOpenclFrameSync option probes.
func (caps Capabilities) isOpenclFullSupported() bool {
	return caps.hasHWAccel("opencl") && caps.hasFilter("scale_opencl") && caps.hasFilter("tonemap_opencl") && caps.hasFilter("overlay_opencl")
}

// isVaapiFullSupported ports EncodingHelper.IsVaapiFullSupported.
func (caps Capabilities) isVaapiFullSupported() bool {
	if !caps.hasHWAccel("drm") || !caps.hasHWAccel("vaapi") {
		return false
	}
	for _, filter := range []string{"scale_vaapi", "deinterlace_vaapi", "tonemap_vaapi", "procamp_vaapi", "overlay_vaapi", "transpose_vaapi", "hwupload_vaapi"} {
		if !caps.hasFilter(filter) {
			return false
		}
	}
	return true
}

// isCudaFullSupported ports EncodingHelper.IsCudaFullSupported. As with
// OpenCL, the named filters stand in for the ScaleCudaFormat and
// TonemapCudaName option probes; jellyfin-ffmpeg has both options.
func (caps Capabilities) isCudaFullSupported() bool {
	if !caps.hasHWAccel("cuda") {
		return false
	}
	for _, filter := range []string{"scale_cuda", "yadif_cuda", "tonemap_cuda", "overlay_cuda", "hwupload_cuda"} {
		if !caps.hasFilter(filter) {
			return false
		}
	}
	return true
}

// HardwareSummary describes the probed facts that select a hardware
// pipeline. The composition root logs it once at startup.
func (caps Capabilities) HardwareSummary() string {
	driver := caps.VaapiDriver
	if driver == "" {
		driver = "unknown"
	}
	return fmt.Sprintf("hwaccels=%v filters=%d tonemapx=%t alphasrc=%t opencl_full=%t vaapi_full=%t cuda_full=%t qsv_full=%t vaapi_driver=%s",
		caps.HWAccels, len(caps.Filters), caps.SupportsTonemapX, caps.hasFilter("alphasrc"),
		caps.isOpenclFullSupported(), caps.isVaapiFullSupported(), caps.isCudaFullSupported(),
		caps.hasHWAccel("qsv") && caps.hasHWAccel("vaapi") && caps.isOpenclFullSupported() && caps.hasFilter("alphasrc"), driver)
}

// isVaapiSupported ports EncodingHelper.IsVaapiSupported.
func isVaapiSupported(stream VideoStream, caps Capabilities) bool {
	return !strings.EqualFold(stream.Codec, "mpeg4") && caps.hasHWAccel("vaapi")
}

// hardwareType returns the configured type when Deadfin can drive it.
// VAAPI is honored only for the Intel iHD driver; see D-37.
func hardwareType(options HardwareOptions, caps Capabilities) string {
	switch options.Type {
	case HardwareAccelerationQSV, HardwareAccelerationNVENC:
		return options.Type
	case HardwareAccelerationVAAPI:
		if caps.VaapiDriver == "iHD" {
			return HardwareAccelerationVAAPI
		}
	}
	return HardwareAccelerationNone
}

// hardwareVideoEncoder ports the hardware branch of
// EncodingHelper.GetH26xOrAv1Encoder. codec is "h264", "hevc", or "av1".
// The result is empty when the configured type has no listed encoder for
// codec. The composition root removes the NVENC encoders that the device
// rejects, so an encoder in caps.Encoders is one the GPU can run.
func hardwareVideoEncoder(codec string, options HardwareOptions, caps Capabilities) string {
	if !options.EnableHardwareEncoding {
		return ""
	}
	hwType := hardwareType(options, caps)
	if hwType == HardwareAccelerationNone {
		return ""
	}
	name := codec + "_" + hwType
	if caps.hasEncoder(name) {
		return name
	}
	return ""
}

// CanEncodeVideo reports whether a transcode can produce codec. H.264 has
// the libx264 fallback. HEVC and AV1 need a hardware encoder, because
// Deadfin has no libx265 or libsvtav1 path (D-37).
func CanEncodeVideo(codec string, caps Capabilities, options HardwareOptions) bool {
	switch normalizeVideoCodec(codec) {
	case "h264":
		return caps.H264Encoder != "" || hardwareVideoEncoder("h264", options, caps) != ""
	case "hevc", "av1":
		return hardwareVideoEncoder(normalizeVideoCodec(codec), options, caps) != ""
	}
	return false
}

func normalizeVideoCodec(codec string) string {
	codec = strings.ToLower(codec)
	if codec == "h265" {
		return "hevc"
	}
	return codec
}

// videoEncoderName ports EncodingHelper.GetH264Encoder, GetH265Encoder, and
// GetAv1Encoder.
func videoEncoderName(job EncodingJob, caps Capabilities) string {
	codec := normalizeVideoCodec(job.OutputVideoCodec)
	switch codec {
	case "h264", "hevc", "av1":
	default:
		return job.OutputVideoCodec
	}
	if encoder := hardwareVideoEncoder(codec, job.Hardware, caps); encoder != "" {
		return encoder
	}
	if codec == "h264" {
		return caps.H264Encoder
	}
	return ""
}

// videoColorBitDepth ports EncodingHelper.GetVideoColorBitDepth.
func videoColorBitDepth(stream VideoStream) int {
	if stream.BitDepth > 0 {
		return stream.BitDepth
	}
	switch strings.ToLower(stream.PixelFormat) {
	case "yuv420p10le", "yuv422p10le", "yuv444p10le":
		return 10
	case "yuv420p12le", "yuv422p12le", "yuv444p12le":
		return 12
	default:
		return 8
	}
}

// videoRangeType returns the probed range type. A job built without one
// derives it from the transfer and Dolby Vision facts.
func videoRangeType(stream VideoStream) string {
	if stream.RangeType != "" {
		return stream.RangeType
	}
	switch {
	case stream.DolbyVisionProfile == 5:
		return "DOVI"
	case isDOVIVideo(stream) && stream.DolbyVisionBLCompatibilityID == 1:
		return "DOVIWithHDR10"
	case isDOVIVideo(stream) && stream.DolbyVisionBLCompatibilityID == 4:
		return "DOVIWithHLG"
	case isDOVIVideo(stream):
		return "DOVI"
	}
	switch strings.ToLower(stream.ColorTransfer) {
	case "smpte2084":
		return "HDR10"
	case "arib-std-b67", "hlg":
		return "HLG"
	}
	return "SDR"
}

func isHDRRange(rangeType string) bool {
	switch rangeType {
	case "", "Unknown", "SDR", "DOVIWithSDR":
		return false
	default:
		return true
	}
}

func isDoviWithHdr10Bl(rangeType string) bool {
	switch rangeType {
	case "DOVIWithHDR10", "DOVIWithEL", "DOVIWithHDR10Plus", "DOVIWithELHDR10Plus", "DOVIInvalid":
		return true
	}
	return false
}

func isHdr10Plus(rangeType string) bool {
	return rangeType == "HDR10Plus" || rangeType == "DOVIWithHDR10Plus" || rangeType == "DOVIWithELHDR10Plus"
}

// isSwTonemapAvailable ports EncodingHelper.IsSwTonemapAvailable.
func isSwTonemapAvailable(stream VideoStream, caps Capabilities) bool {
	return videoColorBitDepth(stream) >= 10 && caps.SupportsTonemapX && isHDRRange(videoRangeType(stream))
}

// isHwTonemapAvailable ports EncodingHelper.IsHwTonemapAvailable for the
// Intel decoders, which all parse the Dolby Vision RPU.
func isHwTonemapAvailable(stream VideoStream, options HardwareOptions) bool {
	return options.EnableTonemapping && videoColorBitDepth(stream) >= 10 && isHDRRange(videoRangeType(stream))
}

// isHwTonemapAvailableFor ports IsHwTonemapAvailable with the decoder
// check: only the software decoder and the hwaccel decoders (NVDEC,
// VAAPI) parse the Dolby Vision RPU. A cuvid decoder alone does not.
func isHwTonemapAvailableFor(stream VideoStream, options HardwareOptions, decoder []string) bool {
	if !isHwTonemapAvailable(stream, options) {
		return false
	}
	if videoRangeType(stream) != "DOVI" {
		return true
	}
	joined := strings.Join(decoder, " ")
	return len(decoder) == 0 || strings.Contains(joined, "cuda") || strings.Contains(joined, "vaapi")
}

// isIntelVppTonemapAvailable ports EncodingHelper.IsIntelVppTonemapAvailable
// for Linux.
func isIntelVppTonemapAvailable(stream VideoStream, options HardwareOptions) bool {
	if !options.EnableVppTonemapping || videoColorBitDepth(stream) < 10 {
		return false
	}
	rangeType := videoRangeType(stream)
	return rangeType == "HDR10" || isHdr10Plus(rangeType) || isDoviWithHdr10Bl(rangeType)
}

// CanTonemap reports whether a job for stream can map HDR to SDR with the
// given capabilities and options. Playback negotiation uses it to decide
// whether a non-SDR source can transcode.
func CanTonemap(stream VideoStream, caps Capabilities, options HardwareOptions) bool {
	if isSwTonemapAvailable(stream, caps) {
		return true
	}
	switch hardwareType(options, caps) {
	case HardwareAccelerationQSV:
		if !caps.hasHWAccel("qsv") || !isVaapiSupported(stream, caps) {
			return false
		}
	case HardwareAccelerationVAAPI:
		if !caps.isVaapiFullSupported() {
			return false
		}
	case HardwareAccelerationNVENC:
		// The NVDEC hwaccel and the software decoder both parse the Dolby
		// Vision RPU for tonemap_cuda.
		return caps.isCudaFullSupported() && isHwTonemapAvailable(stream, options)
	default:
		return false
	}
	return isIntelVppTonemapAvailable(stream, options) || (isHwTonemapAvailable(stream, options) && caps.isOpenclFullSupported())
}

func isVideoStreamHevcRext(stream VideoStream) bool {
	if !strings.EqualFold(stream.Codec, "hevc") {
		return false
	}
	if strings.EqualFold(stream.Profile, "Rext") {
		return true
	}
	switch strings.ToLower(stream.PixelFormat) {
	case "yuv420p12le", "yuv422p", "yuv422p10le", "yuv422p12le", "yuv444p", "yuv444p10le", "yuv444p12le":
		return true
	}
	return false
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// hwaccelArgs ports EncodingHelper.GetHwaccelType for the Intel types. The
// boolean result is false when the decoder must stay in software.
func hwaccelArgs(stream VideoStream, options HardwareOptions, caps Capabilities, hwType, codec string, bitDepth int, outputHwSurface bool) ([]string, bool) {
	codecAvailable := containsFold(options.HardwareDecodingCodecs, codec)
	if codecAvailable {
		if codec == "hevc" && containsFold(options.HardwareDecodingCodecs, "hevc") {
			if isVideoStreamHevcRext(stream) {
				if bitDepth <= 10 && !options.EnableDecodingColorDepth10HevcRext {
					return nil, false
				}
				if bitDepth == 12 && !options.EnableDecodingColorDepth12HevcRext {
					return nil, false
				}
			} else if bitDepth == 10 && !options.EnableDecodingColorDepth10Hevc {
				return nil, false
			}
		}
		if codec == "vp9" && containsFold(options.HardwareDecodingCodecs, "vp9") && bitDepth == 10 && !options.EnableDecodingColorDepth10Vp9 {
			return nil, false
		}
	}
	if !codecAvailable {
		return nil, false
	}
	isAv1 := codec == "av1" && !caps.versionAtLeast(6, 0, 0)
	profileMismatch := codec == "h264" && strings.EqualFold(stream.Profile, "baseline")
	vaapiArgs := func() []string {
		args := []string{"-hwaccel", "vaapi"}
		if outputHwSurface {
			args = append(args, "-hwaccel_output_format", "vaapi", "-noautorotate")
		}
		if profileMismatch {
			args = append(args, "-hwaccel_flags", "+allow_profile_mismatch")
		}
		if isAv1 {
			args = append(args, "-c:v", "av1")
		}
		return args
	}
	switch hwType {
	case HardwareAccelerationQSV:
		if options.PreferSystemNativeHwDecoder {
			if isVaapiSupported(stream, caps) {
				return vaapiArgs(), true
			}
			return nil, false
		}
		if caps.hasHWAccel("qsv") {
			args := []string{"-hwaccel", "qsv"}
			if outputHwSurface {
				args = append(args, "-hwaccel_output_format", "qsv", "-noautorotate")
			}
			return args, true
		}
	case HardwareAccelerationVAAPI:
		if isVaapiSupported(stream, caps) {
			return vaapiArgs(), true
		}
	case HardwareAccelerationNVENC:
		if caps.isCudaFullSupported() {
			args := []string{"-hwaccel", "cuda"}
			if outputHwSurface {
				args = append(args, "-hwaccel_output_format", "cuda", "-noautorotate")
			}
			if options.EnableEnhancedNvdecDecoder {
				// NVDEC does not implement threading; cuvid has no threading issue.
				if caps.versionAtLeast(6, 0, 0) {
					args = append(args, "-hwaccel_flags", "+unsafe_output")
				}
				args = append(args, "-threads", "1")
				if isAv1 {
					args = append(args, "-c:v", "av1")
				}
			}
			return args, true
		}
	}
	return nil, false
}

// hwDecoderName ports EncodingHelper.GetHwDecoderName for the "qsv" and
// "cuvid" suffixes. It returns the -c:v decoder, or nothing.
func hwDecoderName(options HardwareOptions, caps Capabilities, prefix, suffix, codec string, bitDepth int) []string {
	name := prefix + "_" + suffix
	if !caps.hasDecoder(name) || !containsFold(options.HardwareDecodingCodecs, codec) {
		return nil
	}
	if bitDepth == 10 {
		if codec == "hevc" && !options.EnableDecodingColorDepth10Hevc {
			return nil
		}
		if codec == "vp9" && !options.EnableDecodingColorDepth10Vp9 {
			return nil
		}
	}
	if suffix == "cuvid" && options.EnableEnhancedNvdecDecoder {
		return nil
	}
	if suffix == "qsv" && options.PreferSystemNativeHwDecoder {
		return nil
	}
	return []string{"-c:v", name}
}

// hardwareVideoDecoder ports EncodingHelper.GetHardwareVideoDecoder with
// GetQsvHwVidDecoder and GetVaapiVidDecoder. An empty result selects the
// software decoder.
func hardwareVideoDecoder(job EncodingJob, caps Capabilities, hwType string) []string {
	stream := job.VideoStream
	if job.OutputVideoCodec == "copy" || stream.Codec == "" || hwType == HardwareAccelerationNone {
		return nil
	}
	codec := strings.ToLower(stream.Codec)
	bitDepth := videoColorBitDepth(stream)
	// Only HEVC, VP9, and AV1 have 10-bit Intel decoders.
	if bitDepth == 10 && codec == "h264" {
		return nil
	}
	if codec == "h264" && (strings.Contains(stream.Profile, "4:2:2") || strings.Contains(stream.Profile, "4:4:4")) {
		return nil
	}

	pixelFormat := strings.ToLower(stream.PixelFormat)
	if hwType == HardwareAccelerationNVENC {
		return nvdecVideoDecoder(job, caps, codec, pixelFormat, bitDepth)
	}
	is8bit := pixelFormat == "yuv420p" || pixelFormat == "yuvj420p"
	is8or10bit := is8bit || pixelFormat == "yuv420p10le"
	is8to12bit := is8or10bit
	switch pixelFormat {
	case "yuv422p", "yuv444p", "yuv422p10le", "yuv444p10le", "yuv420p12le", "yuv422p12le", "yuv444p12le":
		is8to12bit = true
	}

	var hwSurface bool
	var name, prefix string
	switch {
	case is8bit && (codec == "avc" || codec == "h264"):
		name, prefix = "h264", "h264"
	case is8bit && codec == "vc1":
		name, prefix = "vc1", "vc1"
	case is8bit && codec == "vp8":
		name, prefix = "vp8", "vp8"
	case is8bit && codec == "mpeg2video":
		name, prefix = "mpeg2video", "mpeg2"
	case is8or10bit && codec == "vp9":
		name, prefix = "vp9", "vp9"
	case is8or10bit && codec == "av1":
		name, prefix = "av1", "av1"
	case is8to12bit && (codec == "hevc" || codec == "h265"):
		name, prefix = "hevc", "hevc"
	default:
		return nil
	}

	options := job.Hardware
	switch hwType {
	case HardwareAccelerationQSV:
		hwSurface = caps.hasHWAccel("qsv") && caps.isOpenclFullSupported() && isVaapiSupported(stream, caps) && caps.hasFilter("alphasrc")
		accel, ok := hwaccelArgs(stream, options, caps, hwType, name, bitDepth, hwSurface)
		decoder := hwDecoderName(options, caps, prefix, "qsv", name, bitDepth)
		if !ok {
			accel = nil
		}
		return append(accel, decoder...)
	case HardwareAccelerationVAAPI:
		hwSurface = isVaapiSupported(stream, caps) && caps.isVaapiFullSupported() && caps.isOpenclFullSupported() && caps.hasFilter("alphasrc")
		if accel, ok := hwaccelArgs(stream, options, caps, hwType, name, bitDepth, hwSurface); ok {
			return accel
		}
	}
	return nil
}

// nvdecVideoDecoder ports EncodingHelper.GetNvdecVidDecoder. NVDEC has
// wider format support than the Intel decoders: MPEG-4 Part 2, and 4:4:4
// and 12-bit HEVC.
func nvdecVideoDecoder(job EncodingJob, caps Capabilities, codec, pixelFormat string, bitDepth int) []string {
	stream := job.VideoStream
	options := job.Hardware
	hwSurface := caps.isCudaFullSupported() && caps.hasFilter("alphasrc")
	is8bit := pixelFormat == "yuv420p" || pixelFormat == "yuvj420p"
	is8or10bit := is8bit || pixelFormat == "yuv420p10le"
	is8to12bit := is8or10bit
	switch pixelFormat {
	case "yuv444p", "yuv444p10le", "yuv420p12le", "yuv444p12le":
		is8to12bit = true
	}
	var name, prefix string
	switch {
	case is8bit && (codec == "avc" || codec == "h264"):
		name, prefix = "h264", "h264"
	case is8bit && codec == "mpeg2video":
		name, prefix = "mpeg2video", "mpeg2"
	case is8bit && codec == "vc1":
		name, prefix = "vc1", "vc1"
	case is8bit && codec == "mpeg4":
		name, prefix = "mpeg4", "mpeg4"
	case is8bit && codec == "vp8":
		name, prefix = "vp8", "vp8"
	case is8or10bit && codec == "vp9":
		name, prefix = "vp9", "vp9"
	case is8or10bit && codec == "av1":
		name, prefix = "av1", "av1"
	case is8to12bit && (codec == "hevc" || codec == "h265"):
		name, prefix = "hevc", "hevc"
	default:
		return nil
	}
	accel, ok := hwaccelArgs(stream, options, caps, HardwareAccelerationNVENC, name, bitDepth, hwSurface)
	if !ok {
		accel = nil
	}
	return append(accel, hwDecoderName(options, caps, prefix, "cuvid", name, bitDepth)...)
}

func decoderKind(decoder []string) string {
	joined := strings.Join(decoder, " ")
	switch {
	case strings.Contains(joined, "cuda"):
		return "cuda"
	case strings.Contains(joined, "cuvid"):
		return "cuvid"
	case strings.Contains(joined, "vaapi"):
		return "vaapi"
	case strings.Contains(joined, "qsv"):
		return "qsv"
	}
	return ""
}

// vaapiDeviceArgs ports EncodingHelper.GetVaapiDeviceArgs.
func vaapiDeviceArgs(caps Capabilities, renderNode, driver, kernelDriver, vendorID string) []string {
	var options string
	switch {
	case renderNode != "":
		options = renderNode
	case vendorID != "" && caps.versionAtLeast(7, 0, 1):
		options = ",vendor_id=" + vendorID
	case kernelDriver != "":
		options = ",kernel_driver=" + kernelDriver
	}
	if driver != "" {
		options += ",driver=" + driver
	}
	device := "vaapi=" + vaapiAlias
	if options != "" {
		device += ":" + options
	}
	return []string{"-init_hw_device", device}
}

// inputVideoHwaccelArgs ports EncodingHelper.GetInputVideoHwaccelArgs for
// the Intel QSV and iHD VAAPI types on Linux.
func inputVideoHwaccelArgs(job EncodingJob, caps Capabilities, hwType, encoder string, decoder []string) []string {
	if encoder == "copy" {
		return nil
	}
	stream := job.VideoStream
	options := job.Hardware
	kind := decoderKind(decoder)
	hwTonemap := isHwTonemapAvailable(stream, options)
	var args []string
	switch hwType {
	case HardwareAccelerationVAAPI:
		if !caps.hasHWAccel("vaapi") || (kind != "vaapi" && !strings.Contains(encoder, "vaapi")) {
			return nil
		}
		args = append(args, vaapiDeviceArgs(caps, options.VaapiDevice, "iHD", "", "")...)
		if hwTonemap && caps.isOpenclFullSupported() && kind != "vaapi" {
			args = append(args, "-init_hw_device", "opencl="+openclAlias+"@"+vaapiAlias, "-filter_hw_device", openclAlias)
		}
	case HardwareAccelerationQSV:
		if !caps.hasHWAccel("qsv") || (kind == "" && !strings.Contains(encoder, "qsv")) {
			return nil
		}
		args = append(args, vaapiDeviceArgs(caps, options.QsvDevice, "iHD", "i915", "0x8086")...)
		args = append(args, "-init_hw_device", "qsv="+qsvAlias+"@"+vaapiAlias)
		filterDevice := qsvAlias
		if caps.hasHWAccel("vaapi") && hwTonemap && caps.isOpenclFullSupported() {
			args = append(args, "-init_hw_device", "opencl="+openclAlias+"@"+vaapiAlias)
			if kind == "" {
				filterDevice = openclAlias
			}
		}
		args = append(args, "-filter_hw_device", filterDevice)
	case HardwareAccelerationNVENC:
		if !caps.isCudaFullSupported() || (kind == "" && !strings.Contains(encoder, "nvenc")) {
			return nil
		}
		args = append(args, "-init_hw_device", "cuda="+cudaAlias+":0", "-filter_hw_device", cudaAlias)
	default:
		return nil
	}
	return append(args, decoder...)
}

// planVideo selects the encoder, the input arguments, and the filter chain
// for a video transcode. It ports GetVideoProcessingFilterParam's dispatch
// for software, Intel QSV, and Intel iHD VAAPI.
func planVideo(job EncodingJob, caps Capabilities, software softwareFilter) videoPlan {
	hwType := hardwareType(job.Hardware, caps)
	encoder := videoEncoderName(job, caps)
	plan := videoPlan{encoder: encoder}
	if hwType == HardwareAccelerationNone {
		plan.mainFilters = []string{software("yuv420p")}
		return plan
	}
	decoder := hardwareVideoDecoder(job, caps, hwType)
	plan.inputArgs = inputVideoHwaccelArgs(job, caps, hwType, encoder, decoder)
	plan.hwDecoder = len(decoder) != 0
	switch hwType {
	case HardwareAccelerationQSV:
		planIntelQSV(&plan, job, caps, decoder, software)
	case HardwareAccelerationVAAPI:
		planIntelVAAPI(&plan, job, caps, decoder, software)
	case HardwareAccelerationNVENC:
		planNvidia(&plan, job, caps, decoder, software)
	}
	return plan
}

// softwareFilter builds the software main filter chain with the given output
// pixel format.
type softwareFilter func(format string) string

// softwareFallback ports GetSwVidFilterChain as used by the legacy
// copy-back pipelines. Frames from a hardware decoder without a hardware
// surface and frames for a VAAPI encoder use nv12.
func softwareFallback(plan *videoPlan, software softwareFilter, nv12 bool) {
	format := "yuv420p"
	if nv12 {
		format = "nv12"
	}
	plan.mainFilters = []string{software(format)}
}

func hasGraphicalBurn(job EncodingJob) bool {
	return job.SubtitleStream != nil && job.SubtitleStream.BurnIn && isHLSGraphicalSubtitle(job.SubtitleStream)
}

func hasTextBurn(job EncodingJob) bool {
	return job.SubtitleStream != nil && job.SubtitleStream.BurnIn && !isHLSGraphicalSubtitle(job.SubtitleStream)
}

// planIntelQSV ports GetIntelVidFilterChain and
// GetIntelQsvVaapiVidFiltersPrefered (EncodingHelper.cs:4409-4455,
// 4747-5007). Deinterlace and transpose branches are not used. The MJPEG
// branches serve trickplay extraction.
func planIntelQSV(plan *videoPlan, job EncodingJob, caps Capabilities, decoder []string, software softwareFilter) {
	stream := job.VideoStream
	options := job.Hardware
	kind := decoderKind(decoder)
	isVaapiDecoder, isQsvDecoder := kind == "vaapi", kind == "qsv"
	isHwDecoder := isVaapiDecoder || isQsvDecoder
	isSwDecoder := !isHwDecoder
	isQsvEncoder := strings.Contains(plan.encoder, "qsv")
	isSwEncoder := !isQsvEncoder
	isMjpegEncoder := strings.Contains(plan.encoder, "mjpeg")
	isIntelVaapiOclSupported := isVaapiSupported(stream, caps) && caps.hasHWAccel("qsv") && caps.isOpenclFullSupported()

	if (isSwDecoder && isSwEncoder) || !isIntelVaapiOclSupported || !caps.hasFilter("alphasrc") {
		softwareFallback(plan, software, isHwDecoder)
		return
	}

	doVaVppTonemap := isIntelVppTonemapAvailable(stream, options)
	doOclTonemap := !doVaVppTonemap && isHwTonemapAvailable(stream, options)
	doTonemap := doVaVppTonemap || doOclTonemap
	hasGraphicalSubs := hasGraphicalBurn(job)
	hasTextSubs := hasTextBurn(job)
	width, height := evenDimension(job.OutputWidth), evenDimension(job.OutputHeight)

	main := []string{overwriteColorProperties(stream, doTonemap)}
	if isSwDecoder {
		outFormat := "nv12"
		if doOclTonemap {
			outFormat = "yuv420p10le"
		} else if hasGraphicalSubs {
			outFormat = "yuv420p"
		}
		main = append(main, mjpegSwScaleFilter(job, isMjpegEncoder && !doOclTonemap), "format="+outFormat)
		if doOclTonemap {
			main = append(main, "hwupload=derive_device=opencl")
		}
	} else {
		suffix, prefix := "vaapi", "scale"
		if isQsvDecoder {
			suffix, prefix = "qsv", "vpp"
		}
		outFormat := "nv12"
		if doTonemap {
			outFormat = ""
			if isVideoStreamHevcRext(stream) {
				outFormat = "p010"
			}
		}
		scale := hwScaleFilter(prefix, suffix, outFormat, stream, width, height)
		if scale != "" && isMjpegEncoder {
			// _minFFmpegQsvVppOutRangeOption and _minFFmpegQsvVppScaleModeOption.
			doVppFullRangeOut := caps.versionAtLeast(7, 0, 1)
			if !(isQsvDecoder && !doVppFullRangeOut) && !doOclTonemap {
				scale += ":out_range=pc"
			}
			if !isQsvDecoder {
				scale += ":mode=hq"
			} else if caps.versionAtLeast(6, 0, 0) {
				scale += ":scale_mode=hq"
			}
		}
		if scale != "" && isVaapiDecoder {
			scale += ":extra_hw_frames=24"
		}
		main = append(main, scale)
	}

	if doVaVppTonemap && isHwDecoder {
		if isQsvDecoder {
			main = append(main, "hwmap=derive_device=vaapi", "format=vaapi")
		}
		main = append(main, vaapiTonemapFilter(options))
		if isQsvDecoder {
			main = append(main, "hwmap=derive_device=qsv", "format=qsv")
		}
	}
	if doOclTonemap && isHwDecoder {
		main = append(main, "hwmap=derive_device=opencl:mode=read")
	}
	if doOclTonemap {
		main = append(main, openclTonemapFilter(fullRangeTonemap(options, isMjpegEncoder), caps))
	}

	memoryOutput := false
	if (isHwDecoder && isSwEncoder) || (isSwDecoder && doOclTonemap) {
		memoryOutput = true
		if isSwEncoder && (doOclTonemap || isVaapiDecoder) {
			main = append(main, "hwmap=mode=read")
		} else {
			main = append(main, "hwdownload")
		}
		main = append(main, "format=nv12")
	}
	if isSwDecoder && isQsvEncoder {
		memoryOutput = true
	}
	if memoryOutput && hasTextSubs {
		main = append(main, textSubtitleBurnFilter(job, false))
	}

	isQsvInQsvOut := isHwDecoder && isQsvEncoder
	if isQsvInQsvOut {
		if doOclTonemap {
			main = append(main, "hwmap=derive_device=qsv:mode=write:reverse=1:extra_hw_frames=16", "format=qsv")
		} else if isVaapiDecoder {
			main = append(main, "hwmap=derive_device=qsv", "format=qsv")
		}
	}
	plan.mainFilters = main

	switch {
	case isQsvInQsvOut && (hasGraphicalSubs || hasTextSubs):
		plan.subFilters, plan.textSubtitle = hardwareSubtitleSource(job, width, 1080)
		plan.subFilters = append(plan.subFilters, "hwupload=derive_device=qsv:extra_hw_frames=64")
		plan.overlay = []string{"overlay_qsv=eof_action=pass:repeatlast=0" + overlaySize(width, height)}
	case memoryOutput && hasGraphicalSubs:
		plan.subFilters = []string{graphicalSubPreProcess(width, height)}
		plan.overlay = []string{"overlay=eof_action=pass:repeatlast=0"}
	}
}

// planIntelVAAPI ports GetVaapiVidFilterChain and
// GetIntelVaapiFullVidFiltersPrefered (EncodingHelper.cs:5017-5313).
func planIntelVAAPI(plan *videoPlan, job EncodingJob, caps Capabilities, decoder []string, software softwareFilter) {
	stream := job.VideoStream
	options := job.Hardware
	isVaapiDecoder := decoderKind(decoder) == "vaapi"
	isVaapiEncoder := strings.Contains(plan.encoder, "vaapi")
	isSwDecoder := !isVaapiDecoder
	isSwEncoder := !isVaapiEncoder
	isMjpegEncoder := strings.Contains(plan.encoder, "mjpeg")
	isVaapiOclSupported := isVaapiSupported(stream, caps) && caps.isVaapiFullSupported() && caps.isOpenclFullSupported()

	if (isSwDecoder && isSwEncoder) || !isVaapiOclSupported || !caps.hasFilter("alphasrc") {
		softwareFallback(plan, software, isVaapiDecoder || isVaapiEncoder)
		if !isSwEncoder {
			if hasGraphicalBurn(job) {
				plan.subFilters = []string{graphicalSubPreProcess(evenDimension(job.OutputWidth), evenDimension(job.OutputHeight))}
				plan.overlay = []string{"overlay=eof_action=pass:repeatlast=0", "hwupload=derive_device=vaapi"}
			} else {
				plan.mainFilters = append(plan.mainFilters, "hwupload=derive_device=vaapi")
			}
		}
		return
	}

	doVaVppTonemap := isVaapiDecoder && isIntelVppTonemapAvailable(stream, options)
	doOclTonemap := !doVaVppTonemap && isHwTonemapAvailable(stream, options)
	doTonemap := doVaVppTonemap || doOclTonemap
	hasGraphicalSubs := hasGraphicalBurn(job)
	hasTextSubs := hasTextBurn(job)
	width, height := evenDimension(job.OutputWidth), evenDimension(job.OutputHeight)
	isVaInVaOut := isVaapiDecoder && isVaapiEncoder

	main := []string{overwriteColorProperties(stream, doTonemap)}
	if isSwDecoder {
		outFormat := "nv12"
		if doOclTonemap {
			outFormat = "yuv420p10le"
		}
		main = append(main, mjpegSwScaleFilter(job, isMjpegEncoder && !doOclTonemap), "format="+outFormat)
		if doOclTonemap {
			main = append(main, "hwupload=derive_device=opencl")
		}
	} else {
		outFormat := "nv12"
		if doTonemap {
			outFormat = ""
			if isVideoStreamHevcRext(stream) {
				outFormat = "p010"
			}
		}
		scale := hwScaleFilter("scale", "vaapi", outFormat, stream, width, height)
		if scale != "" && isMjpegEncoder {
			if !doOclTonemap {
				scale += ":out_range=pc"
			}
			scale += ":mode=hq"
		}
		if scale != "" {
			scale += ":extra_hw_frames=24"
		}
		main = append(main, scale)
	}
	if doVaVppTonemap {
		main = append(main, vaapiTonemapFilter(options))
	}
	if doOclTonemap && isVaapiDecoder {
		main = append(main, "hwmap=derive_device=opencl:mode=read")
	}
	if doOclTonemap {
		main = append(main, openclTonemapFilter(fullRangeTonemap(options, isMjpegEncoder), caps))
	}
	if doOclTonemap && isVaInVaOut {
		main = append(main, "hwmap=derive_device=vaapi:mode=write:reverse=1", "format=vaapi")
	}

	memoryOutput := false
	isUploadForOclTonemap := isSwDecoder && doOclTonemap
	if (isVaapiDecoder && isSwEncoder) || isUploadForOclTonemap {
		memoryOutput = true
		if isUploadForOclTonemap && isVaapiEncoder {
			main = append(main, "hwdownload")
		} else {
			main = append(main, "hwmap=mode=read")
		}
		main = append(main, "format=nv12")
	}
	if isSwDecoder && isVaapiEncoder {
		memoryOutput = true
	}
	if memoryOutput && hasTextSubs {
		main = append(main, textSubtitleBurnFilter(job, false))
	}
	if memoryOutput && isVaapiEncoder && !hasGraphicalSubs {
		main = append(main, "hwupload_vaapi")
	}
	plan.mainFilters = main

	switch {
	case isVaInVaOut && (hasGraphicalSubs || hasTextSubs):
		plan.subFilters, plan.textSubtitle = hardwareSubtitleSource(job, width, 1080)
		plan.subFilters = append(plan.subFilters, "hwupload=derive_device=vaapi")
		plan.overlay = []string{"overlay_vaapi=eof_action=pass:repeatlast=0" + overlaySize(width, height)}
	case memoryOutput && hasGraphicalSubs:
		plan.subFilters = []string{graphicalSubPreProcess(width, height)}
		plan.overlay = []string{"overlay=eof_action=pass:repeatlast=0"}
		if isVaapiEncoder {
			plan.overlay = append(plan.overlay, "hwupload_vaapi")
		}
	}
}

// planNvidia ports GetNvidiaVidFilterChain and GetNvidiaVidFiltersPrefered
// (EncodingHelper.cs:3975-4174). Deinterlace and transpose branches are not
// used: Deadfin does not record interlace or rotation (D-37).
func planNvidia(plan *videoPlan, job EncodingJob, caps Capabilities, decoder []string, software softwareFilter) {
	stream := job.VideoStream
	isNvDecoder := decoderKind(decoder) == "cuda"
	isSwDecoder := len(decoder) == 0
	isNvencEncoder := strings.Contains(plan.encoder, "nvenc")
	isSwEncoder := !isNvencEncoder
	isMjpegEncoder := strings.Contains(plan.encoder, "mjpeg")

	if (isSwDecoder && isSwEncoder) || !caps.isCudaFullSupported() || !caps.hasFilter("alphasrc") {
		softwareFallback(plan, software, !isSwDecoder)
		return
	}

	doCuTonemap := isHwTonemapAvailableFor(stream, job.Hardware, decoder)
	hasGraphicalSubs := hasGraphicalBurn(job)
	hasTextSubs := hasTextBurn(job)
	hasSubs := hasGraphicalSubs || hasTextSubs
	width, height := evenDimension(job.OutputWidth), evenDimension(job.OutputHeight)
	isCuInCuOut := isNvDecoder && isNvencEncoder

	main := []string{overwriteColorProperties(stream, doCuTonemap)}
	if isSwDecoder {
		outFormat := "yuv420p"
		if doCuTonemap {
			outFormat = "p010le"
		}
		main = append(main, mjpegSwScaleFilter(job, false), "format="+outFormat)
		if doCuTonemap {
			main = append(main, "hwupload=derive_device=cuda")
		}
	}
	if isNvDecoder {
		outFormat := "yuv420p"
		if doCuTonemap {
			outFormat = ""
			if isVideoStreamHevcRext(stream) {
				outFormat = "p010"
			}
		}
		main = append(main, hwScaleFilter("scale", "cuda", outFormat, stream, width, height))
	}
	if doCuTonemap {
		main = append(main, hwTonemapFilter("cuda", "yuv420p", fullRangeTonemap(job.Hardware, isMjpegEncoder), caps))
	}

	memoryOutput := false
	isUploadForCuTonemap := isSwDecoder && doCuTonemap
	if (isNvDecoder && isSwEncoder) || (isUploadForCuTonemap && hasSubs) {
		memoryOutput = true
		main = append(main, "hwdownload", "format=yuv420p")
	}
	if isSwDecoder && isNvencEncoder && !isUploadForCuTonemap {
		memoryOutput = true
	}
	if memoryOutput && hasTextSubs {
		main = append(main, textSubtitleBurnFilter(job, false))
	}
	plan.mainFilters = main

	switch {
	case isCuInCuOut && hasSubs:
		var alphaFormat string
		if hasGraphicalSubs {
			plan.subFilters = []string{graphicalSubPreProcess(width, height), "format=yuva420p"}
		} else {
			plan.subFilters, plan.textSubtitle = textSubtitleSource(job, width, height, "yuva420p")
			if caps.SupportsOverlayCudaAlphaFormat {
				alphaFormat = ":alpha_format=premultiplied"
			}
		}
		plan.subFilters = append(plan.subFilters, "hwupload=derive_device=cuda")
		plan.overlay = []string{"overlay_cuda=eof_action=pass:repeatlast=0" + alphaFormat}
	case !isCuInCuOut && hasGraphicalSubs:
		plan.subFilters = []string{graphicalSubPreProcess(width, height)}
		plan.overlay = []string{"overlay=eof_action=pass:repeatlast=0"}
	}
}

// hardwareSubtitleSource builds the subtitle branch for an in-VRAM overlay.
// Graphical subtitles scale to at most maxHeight to reduce transfer cost;
// text subtitles render onto a transparent alphasrc canvas.
func hardwareSubtitleSource(job EncodingJob, width, maxHeight int) ([]string, bool) {
	subWidth, subHeight := width, evenDimension(job.OutputHeight)
	if subWidth <= 0 || subHeight <= 0 {
		// An unknown output size cannot size alphasrc; render the
		// subtitle canvas at 1080p and let the overlay stretch it.
		subWidth, subHeight = 1920, maxHeight
	}
	if subHeight > maxHeight {
		subWidth = evenDimension(subWidth * maxHeight / subHeight)
		subHeight = maxHeight
	}
	if hasGraphicalBurn(job) {
		return []string{graphicalSubPreProcess(subWidth, subHeight), "format=bgra"}, false
	}
	return textSubtitleSource(job, subWidth, subHeight, "bgra")
}

// textSubtitleSource ports GetAlphaSrcFilter with the subtitle renderer:
// text subtitles render onto a transparent alphasrc canvas of the given
// size and pixel format.
func textSubtitleSource(job EncodingJob, subWidth, subHeight int, format string) ([]string, bool) {
	rate := 10.0
	codec := strings.ToLower(job.SubtitleStream.Codec)
	if codec == "ass" || codec == "ssa" {
		rate = 25
		if job.VideoStream.RealFrameRate > 0 {
			rate = job.VideoStream.RealFrameRate
		}
		if rate > 60 {
			rate = 60
		}
	}
	start := "0"
	if job.StartOffsetTicks > 0 {
		start = strings.NewReplacer(":", "\\:", ".", "\\.").Replace(formatTicks(job.StartOffsetTicks))
	}
	alpha := "alphasrc=s=" + strconv.Itoa(subWidth) + "x" + strconv.Itoa(subHeight) + ":r=" + strconv.FormatFloat(rate, 'f', -1, 64) + ":start='" + start + "'"
	return []string{alpha, "format=" + format, textSubtitleBurnFilter(job, true)}, true
}

// overwriteColorProperties ports GetOverwriteColorPropertiesParam.
func overwriteColorProperties(stream VideoStream, tonemap bool) string {
	if !tonemap {
		return "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709"
	}
	if strings.EqualFold(stream.ColorTransfer, "arib-std-b67") {
		return "setparams=color_primaries=bt2020:color_trc=arib-std-b67:colorspace=bt2020nc"
	}
	return "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc"
}

// mjpegSwScaleFilter is swScaleFilter with GetSwScaleFilter's maximum-width
// form for image extraction, and full range output for a hardware MJPEG
// encoder ("sw decoder + hw mjpeg encoder").
func mjpegSwScaleFilter(job EncodingJob, fullRange bool) string {
	filter := swScaleFilter(job)
	if job.MaxWidth > 0 {
		filter = TrickplayScaleFilter(job.MaxWidth)
	}
	if fullRange {
		filter += ":out_range=pc"
	}
	return filter
}

// fullRangeTonemap ports the forceFullRange argument of GetHwTonemapFilter:
// an MJPEG encoder tone-maps to the pc range.
func fullRangeTonemap(options HardwareOptions, mjpeg bool) HardwareOptions {
	if mjpeg {
		options.Tonemapping = options.tonemapping()
		options.Tonemapping.Range = "pc"
	}
	return options
}

// swScaleFilter ports GetSwScaleFilter for a fixed output size. An unknown
// dimension (zero) keeps the source size on that axis: the aspect ratio comes
// from -2, or from the input size when both dimensions are unknown. Upstream
// emits no scale filter at all in the last case; a no-op scale keeps the
// filter chain non-empty for callers that concatenate it.
func swScaleFilter(job EncodingJob) string {
	width, height := job.OutputWidth, job.OutputHeight
	switch {
	case width > 0 && height > 0:
		return "scale=trunc(" + strconv.Itoa(width) + "/2)*2:trunc(" + strconv.Itoa(height) + "/2)*2"
	case width > 0:
		return "scale=trunc(" + strconv.Itoa(width) + "/2)*2:-2"
	case height > 0:
		return "scale=-2:trunc(" + strconv.Itoa(height) + "/2)*2"
	default:
		return "scale=0:0"
	}
}

func evenDimension(value int) int { return 2 * (value / 2) }

// hwScaleFilter ports EncodingHelper.GetHwScaleFilter. The size is fixed
// when the output differs from the input or the input size is unknown.
func hwScaleFilter(prefix, suffix, format string, stream VideoStream, width, height int) string {
	sizeFixed := stream.Width <= 0 || stream.Height <= 0 || width != stream.Width || height != stream.Height
	if !sizeFixed && format == "" {
		return ""
	}
	filter := prefix + "_" + suffix
	separator := "="
	if sizeFixed {
		filter += "=w=" + strconv.Itoa(width) + ":h=" + strconv.Itoa(height)
		separator = ":"
	}
	if format != "" {
		filter += separator + "format=" + format
	}
	return filter
}

// graphicalSubPreProcess ports GetGraphicalSubPreProcessFilters for a
// subtitle whose canvas size is unknown, which always pads. Without an output
// size the graph keeps the subtitle canvas as it is: crop rejects zero
// dimensions. Upstream returns no filter in that case
// (EncodingHelper.cs:3399-3405).
func graphicalSubPreProcess(width, height int) string {
	if width <= 0 || height <= 0 {
		return "scale"
	}
	w, h := strconv.Itoa(width), strconv.Itoa(height)
	return "scale,scale=-1:" + h + ":fast_bilinear,crop,pad=max(" + w + "\\,iw):max(" + h + "\\,ih):(ow-iw)/2:(oh-ih)/2:black@0,crop=" + w + ":" + h
}

func overlaySize(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	return ":w=" + strconv.Itoa(width) + ":h=" + strconv.Itoa(height)
}

// vaapiTonemapFilter ports GetHwTonemapFilter for the "vaapi" suffix.
func vaapiTonemapFilter(options HardwareOptions) string {
	var procamp string
	if b := options.VppTonemappingBrightness; b != 0 && b >= -100 && b <= 100 {
		procamp = "procamp_vaapi=b=" + formatFloat(b)
	}
	if c := options.VppTonemappingContrast; c > 1 && c <= 10 {
		if procamp == "" {
			procamp = "procamp_vaapi=c=" + formatFloat(c)
		} else {
			procamp += ":c=" + formatFloat(c)
		}
	}
	if procamp != "" {
		procamp += ","
	}
	return procamp + "tonemap_vaapi=format=nv12:p=bt709:t=bt709:m=bt709:extra_hw_frames=32"
}

// openclTonemapFilter ports GetHwTonemapFilter for the "opencl" suffix.
func openclTonemapFilter(options HardwareOptions, caps Capabilities) string {
	return hwTonemapFilter("opencl", "nv12", options, caps)
}

// hwTonemapFilter ports GetHwTonemapFilter for the "opencl" and "cuda"
// suffixes.
func hwTonemapFilter(suffix, format string, options HardwareOptions, caps Capabilities) string {
	tonemap := options.tonemapping()
	filter := "tonemap_" + suffix + "=format=" + format + ":p=bt709:t=bt709:m=bt709:tonemap=" + strings.ToLower(tonemap.Algorithm) + ":peak=" + formatFloat(tonemap.Peak) + ":desat=" + formatFloat(tonemap.Desat)
	mode := strings.ToLower(tonemap.Mode)
	legacyMode := (mode == "max" || mode == "rgb") && caps.versionAtLeast(5, 1, 3)
	advancedMode := (mode == "lum" || mode == "itp") && caps.versionAtLeast(7, 0, 1)
	if legacyMode || advancedMode {
		filter += ":tonemap_mode=" + mode
	}
	if tonemap.Param != 0 {
		filter += ":param=" + formatFloat(tonemap.Param)
	}
	if r := strings.ToLower(tonemap.Range); r == "tv" || r == "pc" {
		filter += ":range=" + r
	}
	return filter
}

// softwareTonemapFilter ports the tonemapx branch of GetSwVidFilterChain.
func softwareTonemapFilter(options HardwareOptions, format string) string {
	tonemap := options.tonemapping()
	filter := "tonemapx=tonemap=" + tonemap.Algorithm + ":desat=" + formatFloat(tonemap.Desat) + ":peak=" + formatFloat(tonemap.Peak) + ":t=bt709:m=bt709:p=bt709:format=" + format
	if tonemap.Param != 0 {
		filter += ":param=" + formatFloat(tonemap.Param)
	}
	if r := strings.ToLower(tonemap.Range); r == "tv" || r == "pc" {
		filter += ":range=" + r
	}
	return filter
}

func formatFloat(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }

// hardwareEncoderArgs ports GetVideoQualityParam, GetEncoderParam,
// GetVideoBitrateParam, and GetHlsVideoKeyFrameArguments for h264_qsv,
// h264_vaapi, and the NVENC encoders.
func hardwareEncoderArgs(job HLSJob, caps Capabilities, encoder string) []string {
	if strings.HasSuffix(encoder, "_nvenc") {
		return nvencEncoderArgs(job, encoder)
	}
	options := job.Hardware
	var args []string
	if options.EnableIntelLowPowerH264HwEncoder {
		args = append(args, "-low_power", "1")
	}
	bitrate := job.OutputVideoBitrate
	switch encoder {
	case "h264_qsv":
		preset := strings.ToLower(options.EncoderPreset)
		switch preset {
		case "veryslow", "slower", "slow", "medium", "fast", "faster", "veryfast":
		default:
			preset = "veryfast"
		}
		args = append(args, "-preset", preset)
		if bitrate < 1000 {
			bitrate = 1000
		}
		factor := 2
		if job.OutputVideoLevel > 0 && job.OutputVideoLevel < 51 {
			factor = 1
		}
		args = append(args,
			"-mbbrc", "1",
			"-b:v", strconv.Itoa(bitrate),
			"-maxrate", strconv.Itoa(clampInt(int64(bitrate)+1)),
			"-rc_init_occupancy", strconv.Itoa(clampInt(int64(bitrate)*int64(factor))),
			"-bufsize", strconv.Itoa(clampInt(int64(bitrate)*2*int64(factor))),
		)
	case "h264_vaapi":
		args = append(args, "-rc_mode", "VBR", "-b:v", strconv.Itoa(bitrate), "-maxrate", strconv.Itoa(bitrate), "-bufsize", strconv.Itoa(clampInt(int64(bitrate)*2)))
	}
	if job.OutputFrameRate > 0 {
		args = append(args, "-r", formatFrameRate(job.OutputFrameRate))
	}
	if job.OutputVideoProfile != "" {
		args = append(args, "-profile:v:0", h264OutputProfile(job.OutputVideoProfile))
	}
	if level := job.OutputVideoLevel; level > 0 {
		// NormalizeTranscodingLevel caps H.264 at 5.1.
		if level >= 51 {
			level = 51
		}
		args = append(args, "-level", strconv.Itoa(level))
	}
	if encoder == "h264_vaapi" && caps.versionAtLeast(6, 0, 0) {
		args = append(args, "-sei", "-a53_cc")
	}
	switch encoder {
	case "h264_qsv":
		args = append(args, hlsGOPArgs(job)...)
	case "h264_vaapi":
		args = append(args, "-force_key_frames:0", "expr:gte(t,n_forced*"+strconv.Itoa(job.SegmentLengthSeconds)+")")
	}
	return args
}

// nvencPreset ports the NVENC branch of GetEncoderParam.
func nvencPreset(preset string) string {
	switch strings.ToLower(preset) {
	case "veryslow":
		return "p7"
	case "slower":
		return "p6"
	case "slow":
		return "p5"
	case "medium":
		return "p4"
	case "fast":
		return "p3"
	case "faster":
		return "p2"
	}
	return "p1"
}

// nvencEncoderArgs ports GetVideoQualityParam and
// GetHlsVideoKeyFrameArguments for h264_nvenc, hevc_nvenc, and av1_nvenc.
// NVENC uses the default bitrate control. It takes no level, because NVENC
// cannot adjust a given level and fails instead. av1_nvenc takes no
// profile. NVENC cannot force key frames, so the GOP is fixed to the
// segment length.
func nvencEncoderArgs(job HLSJob, encoder string) []string {
	bitrate := job.OutputVideoBitrate
	args := []string{
		"-preset", nvencPreset(job.Hardware.EncoderPreset),
		"-b:v", strconv.Itoa(bitrate),
		"-maxrate", strconv.Itoa(bitrate),
		"-bufsize", strconv.Itoa(clampInt(int64(bitrate) * 2)),
	}
	if job.OutputFrameRate > 0 {
		args = append(args, "-r", formatFrameRate(job.OutputFrameRate))
	}
	if profile := job.OutputVideoProfile; profile != "" {
		switch encoder {
		case "h264_nvenc":
			args = append(args, "-profile:v:0", h264OutputProfile(profile))
		case "hevc_nvenc":
			// Deadfin transcodes to HEVC 8-bit only, as upstream: Main.
			args = append(args, "-profile:v:0", "main")
		}
	}
	return append(args, hlsGOPArgs(job)...)
}

// hlsGOPArgs ports the gopArg of GetHlsVideoKeyFrameArguments
// (EncodingHelper.cs:2016-2030) for the encoders that cannot force key
// frames: the GOP is one segment long. Upstream counts the GOP at the
// source RealFrameRate. Deadfin counts it at the rate the encoder receives
// frames: the -r output rate when one is set (Framerate, or MaxFramerate
// below the source rate), else the source ReferenceFrameRate (the average
// rate), else RealFrameRate. RealFrameRate can be a multiple of the frame
// cadence (a TS VC-1 source probes 47.952 for 23.976 frames). A GOP counted
// at a rate above the cadence lasts longer than one segment, so the
// segments are longer than the playlist EXTINF values (D-62).
func hlsGOPArgs(job HLSJob) []string {
	rate := job.OutputFrameRate
	if rate <= 0 {
		rate = job.VideoStream.ReferenceFrameRate
	}
	if rate <= 0 {
		rate = job.VideoStream.RealFrameRate
	}
	if rate <= 0 {
		return nil
	}
	gop := strconv.FormatFloat(ceilFloat(float64(job.SegmentLengthSeconds)*rate), 'f', -1, 64)
	return []string{"-g:v:0", gop, "-keyint_min:v:0", gop}
}

func clampInt(value int64) int {
	const maxInt = int64(^uint32(0) >> 1)
	if value > maxInt {
		return int(maxInt)
	}
	return int(value)
}

func ceilFloat(value float64) float64 {
	truncated := float64(int64(value))
	if truncated < value {
		return truncated + 1
	}
	return truncated
}
