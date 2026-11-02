// SPDX-License-Identifier: GPL-2.0-only

package encoding

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// TrickplayExtraction is one trickplay frame extraction
// (MediaEncoder.ExtractVideoImagesOnIntervalAccelerated without keyframe-only
// extraction).
type TrickplayExtraction struct {
	// InputPath is the -i operand.
	InputPath string
	// OutputPattern is the image2 output, for example "%08d.jpg".
	OutputPattern string
	// MaxWidth is the even rendition width.
	MaxWidth             int
	IntervalMilliseconds int64
	// ReferenceFrameRate normalizes timestamps (setpts). Zero uses 30 and
	// rounds the fps filter up.
	ReferenceFrameRate float64
	// Threads is TrickplayOptions.ProcessThreads; QualityScale is Qscale.
	Threads      int
	QualityScale int

	// AllowHardwareAcceleration is TrickplayOptions.EnableHwAcceleration and
	// EnableHardwareEncoding is TrickplayOptions.EnableHwEncoding. Hardware
	// is the encoding.xml selection they apply to.
	AllowHardwareAcceleration bool
	EnableHardwareEncoding    bool
	Hardware                  HardwareOptions
	VideoStream               VideoStream
	// AspectRatio is the display aspect ratio of the video stream, "16:9".
	AspectRatio string
}

// BuildTrickplayArgs builds the ffmpeg argv tokens of one trickplay frame
// extraction. Without hardware acceleration, or when no hardware type can
// run, it builds the software extraction. With it, the decoder, the filter
// chain, and the MJPEG encoder come from the hardware plan, as
// ExtractVideoImagesOnIntervalAccelerated takes them from EncodingHelper.
func BuildTrickplayArgs(job TrickplayExtraction, caps Capabilities) ([]string, error) {
	if job.InputPath == "" || job.OutputPattern == "" {
		return nil, errors.New("trickplay extraction input and output are required")
	}
	if job.MaxWidth <= 0 {
		return nil, errors.New("trickplay extraction width must be positive")
	}
	if job.IntervalMilliseconds <= 0 {
		return nil, errors.New("trickplay extraction interval must be positive")
	}
	threads := strconv.Itoa(max(job.Threads, 1))
	quality := min(max(job.QualityScale, 1), 31)
	if job.QualityScale == 0 {
		quality = 4
	}
	timing := trickplayTimingFilters(job)

	hardware := job.Hardware
	stream := trickplayDisplayStream(job.VideoStream, job.AspectRatio)
	if !job.AllowHardwareAcceleration || hardwareType(hardware, caps) == HardwareAccelerationNone || stream.Width <= 0 || stream.Height <= 0 {
		return []string{
			"-hide_banner", "-loglevel", "error", "-nostdin",
			"-hwaccel", "none", "-threads", threads,
			"-i", job.InputPath,
			"-an", "-sn",
			"-vf", strings.Join(append(timing, TrickplayScaleFilter(job.MaxWidth)), ","),
			"-threads", threads, "-strict", "unofficial", "-c:v", "mjpeg", "-q:v", strconv.Itoa(quality),
			"-fps_mode", "passthrough", "-f", "image2", job.OutputPattern,
		}, nil
	}

	hardware.EnableHardwareEncoding = hardware.EnableHardwareEncoding && job.EnableHardwareEncoding
	encoder := trickplayMjpegEncoder(hardware, caps)
	width, height := trickplayOutputSize(stream.Width, stream.Height, job.MaxWidth)
	encodingJob := EncodingJob{
		VideoStream: stream, OutputVideoCodec: encoder,
		OutputWidth: width, OutputHeight: height, MaxWidth: job.MaxWidth, Hardware: hardware,
	}
	plan := planVideo(encodingJob, caps, func(format string) string {
		return overwriteColorProperties(stream, false) + "," + TrickplayScaleFilter(job.MaxWidth) + ",format=" + format
	})
	filters := timing
	for _, filter := range plan.mainFilters {
		if filter != "" {
			filters = append(filters, filter)
		}
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	args = append(args, plan.inputArgs...)
	args = append(args, "-i", job.InputPath, "-an", "-sn", "-vf", strings.Join(filters, ","), "-threads", threads)
	switch {
	case strings.Contains(encoder, "qsv") || strings.Contains(encoder, "vaapi"):
		// vaapi and qsv's mjpeg encoder use jpeg quality as input
		// (ExtractVideoImagesOnIntervalInternal).
		args = append(args, "-c:v", encoder, "-global_quality:v", strconv.Itoa(100-(quality-1)*(100/30)))
	default:
		args = append(args, "-strict", "unofficial", "-c:v", encoder, "-q:v", strconv.Itoa(quality))
	}
	return append(args, "-fps_mode", "passthrough", "-f", "image2", job.OutputPattern), nil
}

// TrickplayScaleFilter is the maximum-width form of GetSwScaleFilter for an
// MJPEG encoder: the display aspect is computed from the sample aspect.
func TrickplayScaleFilter(maxWidth int) string {
	return fmt.Sprintf("scale=trunc(min(max(iw\\,ih*(a*sar))\\,%d)/2)*2:trunc(ow/(a*sar)/2)*2", maxWidth)
}

// trickplayTimingFilters normalizes invalid container timestamps and samples
// one frame per interval: setpts before the fps filter.
func trickplayTimingFilters(job TrickplayExtraction) []string {
	rate, explicit := job.ReferenceFrameRate, true
	if rate <= 0 || rate != rate {
		rate, explicit = 30, false
	}
	fps := fmt.Sprintf("fps=1000/%d", job.IntervalMilliseconds)
	if !explicit {
		fps += ":round=up"
	}
	return []string{fmt.Sprintf("setpts=N/%.3f/TB", rate), fps}
}

// trickplayMjpegEncoder ports EncodingHelper.GetMjpegEncoder. VAAPI is
// already limited to the iHD driver by hardwareType.
func trickplayMjpegEncoder(options HardwareOptions, caps Capabilities) string {
	if !options.EnableHardwareEncoding {
		return "mjpeg"
	}
	var name string
	switch hardwareType(options, caps) {
	case HardwareAccelerationQSV:
		name = "mjpeg_qsv"
	case HardwareAccelerationVAAPI:
		name = "mjpeg_vaapi"
	default:
		return "mjpeg"
	}
	if caps.hasEncoder(name) {
		return name
	}
	return "mjpeg"
}

// trickplayDisplayStream recalculates the height of a stream stored
// stretched, because the hardware scalers use fixed output sizes
// (ExtractVideoImagesOnIntervalAccelerated).
func trickplayDisplayStream(stream VideoStream, aspectRatio string) VideoStream {
	parts := strings.Split(aspectRatio, ":")
	if stream.Width <= 0 || stream.Height <= 0 || len(parts) != 2 {
		return stream
	}
	wa, errW := strconv.ParseFloat(parts[0], 64)
	ha, errH := strconv.ParseFloat(parts[1], 64)
	if errW != nil || errH != nil || wa <= 0 || ha <= 0 {
		return stream
	}
	if math.Abs(float64(stream.Width)*ha-float64(stream.Height)*wa) > .05 {
		stream.Height = int(math.RoundToEven(float64(stream.Width) * ha / wa))
	}
	return stream
}

// trickplayOutputSize ports EncodingHelper.GetFixedOutputSize with only a
// maximum width.
func trickplayOutputSize(inputWidth, inputHeight, maxWidth int) (int, int) {
	width, height := inputWidth, inputHeight
	maximumWidth, maximumHeight := min(maxWidth, 4096), min(height, 4096)
	if width > maximumWidth || height > maximumHeight {
		scale := min(float64(maximumWidth)/float64(width), float64(maximumHeight)/float64(height))
		width = min(maximumWidth, int(math.RoundToEven(float64(width)*scale)))
		height = min(maximumHeight, int(math.RoundToEven(float64(height)*scale)))
	}
	return evenDimension(width), evenDimension(height)
}
