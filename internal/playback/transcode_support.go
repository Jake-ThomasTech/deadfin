// SPDX-License-Identifier: GPL-2.0-only

package playback

import "github.com/deadfin-org/deadfin/internal/encoding"

// TranscodeEncoding supplies the encoder facts that both PlaybackInfo
// negotiation and the HLS job builder consult: the probed ffmpeg
// capabilities and the current encoding.xml acceleration options.
type TranscodeEncoding struct {
	Capabilities encoding.Capabilities
	// CurrentCapabilities, when set, returns the capabilities that apply to
	// the current encoding.xml and replaces Capabilities. The composition
	// root re-probes the device facts when the encoding configuration
	// changes, so a dashboard save applies without a restart.
	CurrentCapabilities func() encoding.Capabilities
	// Options returns the current hardware and tone-mapping options. Nil
	// selects the software path.
	Options func() encoding.HardwareOptions
	// SubtitleExtraction returns EncodingOptions.EnableSubtitleExtraction.
	// Nil selects the upstream default, true.
	SubtitleExtraction func() bool
}

func (support TranscodeEncoding) capabilities() encoding.Capabilities {
	if support.CurrentCapabilities != nil {
		return support.CurrentCapabilities()
	}
	return support.Capabilities
}

func (support TranscodeEncoding) hardware() encoding.HardwareOptions {
	if support.Options == nil {
		return encoding.HardwareOptions{}
	}
	return support.Options()
}

// CanTranscodeVideo reports whether the encoder can produce SDR output from
// video. Pinned stock always attempts the transcode; Deadfin declines a
// non-SDR source only when no tone mapper is available (D-32).
func (support TranscodeEncoding) CanTranscodeVideo(video MediaStream) bool {
	if video.VideoRangeType() == VideoRangeSDR {
		return true
	}
	return encoding.CanTonemap(encodingVideoStream(&video), support.capabilities(), support.hardware())
}

// CanExtractSubtitles ports MediaEncoder.CanExtractSubtitles: every codec
// is extractable while EnableSubtitleExtraction is set.
func (support TranscodeEncoding) CanExtractSubtitles(string) bool {
	if support.SubtitleExtraction == nil {
		return true
	}
	return support.SubtitleExtraction()
}

// EncodingVideoStream projects the probed stream facts that the pure
// encoding builders use, for callers outside playback (trickplay).
func EncodingVideoStream(video MediaStream) encoding.VideoStream {
	return encodingVideoStream(&video)
}

// encodingVideoStream projects the probed stream facts that the pure
// encoding builders use.
func encodingVideoStream(video *MediaStream) encoding.VideoStream {
	stream := encoding.VideoStream{
		Index:                        video.Index,
		Codec:                        video.Codec,
		ColorTransfer:                stringPointerValue(video.ColorTransfer),
		CodecTag:                     stringPointerValue(video.CodecTag),
		BitDepth:                     intPointerValue(video.BitDepth),
		NalLengthSize:                nalLengthSize(video.NalLengthSize),
		DolbyVisionProfile:           intPointerValue(video.DvProfile),
		RPUFlag:                      intPointerValue(video.RpuPresentFlag),
		BLPresentFlag:                intPointerValue(video.BlPresentFlag),
		DolbyVisionBLCompatibilityID: intPointerValue(video.DvBlSignalCompatibilityID),
		Width:                        intPointerValue(video.Width),
		Height:                       intPointerValue(video.Height),
		PixelFormat:                  stringPointerValue(video.PixelFormat),
		Profile:                      stringPointerValue(video.Profile),
		RangeType:                    video.VideoRangeType().String(),
	}
	if video.RealFrameRate != nil {
		stream.RealFrameRate = float64(*video.RealFrameRate)
	}
	if rate := video.ReferenceFrameRate(); rate != nil {
		stream.ReferenceFrameRate = float64(*rate)
	}
	return stream
}
