// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"strings"

	"github.com/deadfin-org/deadfin/internal/encoding"
)

// EncodingCapabilities projects the immutable ffmpeg and NVIDIA device
// probes into the pure encoding facts consumed by both progressive and HLS
// argument builders. The NVENC encoders that the device rejects, and the
// cuda hwaccel when no device opens, are removed, so the builders select
// only what the GPU can run (D-37).
func EncodingCapabilities(probed Capabilities, nvidia NvidiaCapabilities) encoding.Capabilities {
	capabilities := encoding.Capabilities{
		EncoderMajorVersion:            probed.Version.Major,
		EncoderMinorVersion:            probed.Version.Minor,
		EncoderPatchVersion:            probed.Version.Patch,
		SupportsSubtitleExtraction:     containsName(probed.Filters, "subtitles"),
		SupportsTonemapX:               containsName(probed.Filters, "tonemapx"),
		SupportsOverlayCudaAlphaFormat: nvidia.OverlayCudaAlphaFormat,
		SupportsPKeyPause:              probed.PKeyPause,
		Decoders:                       append([]string(nil), probed.Decoders...),
		Filters:                        append([]string(nil), probed.Filters...),
		BSFOptions:                     append([]string(nil), probed.BSFOptions...),
	}
	for _, name := range probed.Encoders {
		if !strings.HasSuffix(name, "_nvenc") || containsName(nvidia.Encoders, name) {
			capabilities.Encoders = append(capabilities.Encoders, name)
		}
	}
	for _, name := range probed.HWAccels {
		if name != "cuda" || nvidia.Device {
			capabilities.HWAccels = append(capabilities.HWAccels, name)
		}
	}
	if containsName(probed.Encoders, "libx264") {
		capabilities.H264Encoder = "libx264"
	}
	for _, name := range []string{"aac_at", "libfdk_aac", "aac"} {
		if containsName(probed.Encoders, name) {
			capabilities.AACEncoders = append(capabilities.AACEncoders, name)
		}
	}
	return capabilities
}
