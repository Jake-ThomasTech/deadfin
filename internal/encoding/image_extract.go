// SPDX-License-Identifier: GPL-2.0-only

package encoding

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Image3DFormat is the item's Video3DFormat as the image extraction scaler
// reads it (MediaBrowser.Model.Entities.Video3DFormat).
type Image3DFormat uint8

const (
	Image3DNone Image3DFormat = iota
	Image3DHalfSideBySide
	Image3DFullSideBySide
	Image3DFullTopAndBottom
	Image3DHalfTopAndBottom
)

// ImageExtraction is one single-frame image extraction
// (MediaEncoder.ExtractImageInternal).
type ImageExtraction struct {
	// InputPath is the -i operand.
	InputPath  string
	Container  string
	OutputPath string
	// Offset is the input seek. Nil starts at the first frame.
	Offset *time.Duration
	// StreamIndex selects one input stream with -map 0:N. Nil lets ffmpeg
	// choose the video stream.
	StreamIndex *int
	// Interlaced, HDR and DolbyVision describe the selected video stream.
	Interlaced  bool
	HDR         bool
	DolbyVision bool
	Video3D     Image3DFormat
	// UseIFrame is true for the first attempt and false for the fallback.
	UseIFrame bool
	// Audio marks an embedded audio image (MediaEncoder.ExtractAudioImage):
	// one standard attempt without the -s resolution.
	Audio bool
	// PerfTradeoff is the FFmpeg:imgExtractPerfTradeoff setting.
	PerfTradeoff bool
	// Resolution is the -s value from ChapterImageResolution, or empty.
	Resolution string
	// Threads is EncodingHelper.GetNumberOfThreads: 0 lets ffmpeg decide.
	Threads int
	// SupportsTonemapX and SupportsZscale are the probed filter facts.
	SupportsTonemapX bool
	SupportsZscale   bool
	// EncoderAtLeast51 selects -fps_mode over the deprecated -vsync.
	EncoderAtLeast51 bool
}

// HDRTonemap reports whether the extraction applies a software tonemap. It
// selects the longer HDR timeout (MediaEncoder.cs enableHdrExtraction).
func (job ImageExtraction) HDRTonemap() bool {
	return job.HDR && (job.SupportsTonemapX || job.SupportsZscale && !job.DolbyVision)
}

var containerValidation = regexp.MustCompile(`^[a-zA-Z0-9\-\._,|]{0,40}$`)

// ImageInputFormat ports EncodingHelper.GetInputFormat: the -f value for an
// input container, or empty when ffmpeg must probe the input.
func ImageInputFormat(container string) string {
	if container == "" || !containerValidation.MatchString(container) {
		return ""
	}
	container = replaceFold(container, "mkv", "matroska")
	switch strings.ToLower(container) {
	case "ts":
		return "mpegts"
	case "m2ts", "wmv", "mts", "vob", "mpg", "mpeg", "rec", "dvr-ms", "ogm", "divx", "tp", "rmvb", "rtp", "m4v", "strm", "iso":
		return ""
	}
	return container
}

// ImageExtractionArgs ports MediaEncoder.ExtractImageInternal. The filter
// chain is deinterlace, scale, thumbnail, tonemap. -nostdin is added because a
// one-shot job reads no input (CONVENTIONS 5.4).
func ImageExtractionArgs(job ImageExtraction) []string {
	filters := make([]string, 0, 4)
	if job.Interlaced {
		filters = append(filters, "bwdif=0:-1:0")
	}
	filters = append(filters, imageScaler(job.Video3D))
	if !job.PerfTradeoff && job.UseIFrame && !strings.EqualFold(job.Container, "wtv") {
		filters = append(filters, "thumbnail=n=24")
	}
	if job.HDR {
		switch {
		case job.SupportsTonemapX:
			peak := "100"
			if job.DolbyVision {
				peak = "400"
			}
			filters = append(filters, "tonemapx=tonemap=bt2390:desat=0:peak="+peak+":t=bt709:m=bt709:p=bt709:format=yuv420p:range=full")
		case job.SupportsZscale && !job.DolbyVision:
			filters = append(filters, "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0:peak=100,zscale=t=bt709:m=bt709:out_range=full,format=yuv420p")
		}
	}

	args := []string{"-nostdin"}
	if format := ImageInputFormat(job.Container); format != "" {
		args = append(args, "-f", format)
	}
	// The mpegts demuxer cannot seek to keyframes, so the decoder discards
	// non-keyframes, which may hold corrupt images.
	seekMpegTS := job.Offset != nil && strings.EqualFold(job.Container, "mpegts")
	if job.UseIFrame && (job.PerfTradeoff || seekMpegTS) {
		args = append(args, "-skip_frame", "nokey")
	}
	if job.Offset != nil {
		args = append(args, "-ss", imageTimeParameter(*job.Offset))
	}
	args = append(args, "-i", job.InputPath)
	if job.StreamIndex != nil {
		args = append(args, "-map", "0:"+strconv.Itoa(*job.StreamIndex))
	}
	args = append(args, "-threads", strconv.Itoa(job.Threads), "-v", "quiet", "-vframes", "1", "-vf", strings.Join(filters, ","))
	if job.Resolution != "" && !job.Audio {
		args = append(args, "-s", job.Resolution)
	}
	// EncodingHelper.GetVideoSyncOption("-1", EncoderVersion).
	if job.EncoderAtLeast51 {
		args = append(args, "-fps_mode", "auto")
	} else {
		args = append(args, "-vsync", "-1")
	}
	return append(args, "-f", "image2", job.OutputPath)
}

// imageScaler crops a 3D frame to one eye and keeps the display aspect.
func imageScaler(format Image3DFormat) string {
	switch format {
	case Image3DHalfSideBySide:
		return `crop=iw/2:ih:0:0,scale=(iw*2):ih,setdar=dar=a,crop=min(iw\,ih*dar):min(ih\,iw/dar):(iw-min(iw\,iw*sar))/2:(ih - min (ih\,ih/sar))/2,setsar=sar=1`
	case Image3DFullSideBySide:
		return `crop=iw/2:ih:0:0,setdar=dar=a,crop=min(iw\,ih*dar):min(ih\,iw/dar):(iw-min(iw\,iw*sar))/2:(ih - min (ih\,ih/sar))/2,setsar=sar=1`
	case Image3DHalfTopAndBottom:
		return `crop=iw:ih/2:0:0,scale=(iw*2):ih),setdar=dar=a,crop=min(iw\,ih*dar):min(ih\,iw/dar):(iw-min(iw\,iw*sar))/2:(ih - min (ih\,ih/sar))/2,setsar=sar=1`
	case Image3DFullTopAndBottom:
		return `crop=iw:ih/2:0:0,setdar=dar=a,crop=min(iw\,ih*dar):min(ih\,iw/dar):(iw-min(iw\,iw*sar))/2:(ih - min (ih\,ih/sar))/2,setsar=sar=1`
	default:
		return "scale=round(iw*sar/2)*2:round(ih/2)*2"
	}
}

// imageTimeParameter ports MediaEncoder.GetTimeParameter: hh:mm:ss.fff.
func imageTimeParameter(offset time.Duration) string {
	if offset < 0 {
		offset = 0
	}
	milliseconds := offset.Milliseconds()
	hours := milliseconds / 3_600_000
	minutes := milliseconds / 60_000 % 60
	seconds := milliseconds / 1000 % 60
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, seconds, milliseconds%1000)
}

func replaceFold(value, old, replacement string) string {
	lower := strings.ToLower(value)
	var builder strings.Builder
	for {
		index := strings.Index(lower, old)
		if index < 0 {
			builder.WriteString(value)
			return builder.String()
		}
		builder.WriteString(value[:index])
		builder.WriteString(replacement)
		value, lower = value[index+len(old):], lower[index+len(old):]
	}
}
