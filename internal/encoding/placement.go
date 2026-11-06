// SPDX-License-Identifier: GPL-2.0-only

package encoding

import "strings"

// HLSPlacementFacts are the facts of an HLS job that transcode-worker
// placement rules match (#616, docs/spikes/transcode-workers.md §3.8).
type HLSPlacementFacts struct {
	SourceCodec   string
	SourceProfile string
	BitDepth      int
	// HDR is "sdr", "hdr10", "hdr10plus", "hlg" or "dovi".
	HDR          string
	NeedsToneMap bool
	TargetCodec  string
	// Hardware is the hardware family of the argv; empty for software.
	Hardware string
}

// PlacementFacts derives the placement facts of job. It is pure.
func PlacementFacts(job HLSJob, caps Capabilities) HLSPlacementFacts {
	if isAudioOnlyHLSJob(job.EncodingJob) {
		return HLSPlacementFacts{}
	}
	stream := job.VideoStream
	rangeType := videoRangeType(stream)
	facts := HLSPlacementFacts{
		SourceCodec:   strings.ToLower(stream.Codec),
		SourceProfile: strings.ToLower(strings.ReplaceAll(stream.Profile, " ", "")),
		BitDepth:      stream.BitDepth,
		HDR:           placementRange(rangeType),
		TargetCodec:   strings.ToLower(job.OutputVideoCodec),
	}
	if facts.TargetCodec != "copy" {
		facts.NeedsToneMap = isHDRRange(rangeType) && !job.DirectDolbyVision
		if hardware := hardwareType(job.Hardware, caps); hardware != HardwareAccelerationNone {
			facts.Hardware = hardware
		}
	}
	return facts
}

func placementRange(rangeType string) string {
	switch {
	case strings.HasPrefix(rangeType, "DOVI"):
		return "dovi"
	case strings.EqualFold(rangeType, "HDR10Plus"):
		return "hdr10plus"
	case strings.EqualFold(rangeType, "HDR10"):
		return "hdr10"
	case strings.EqualFold(rangeType, "HLG"):
		return "hlg"
	}
	return "sdr"
}

// HostHardware is the hardware of the host that runs a job (#622): a
// transcode worker reports it at registration. The zero value is a host
// with no hardware family.
type HostHardware struct {
	// Type is HardwareAccelerationQSV, HardwareAccelerationVAAPI,
	// HardwareAccelerationNVENC, or empty for software.
	Type string `json:"type,omitempty"`
	// VaapiDevice and QsvDevice are the host's render node paths.
	VaapiDevice string `json:"vaapiDevice,omitempty"`
	QsvDevice   string `json:"qsvDevice,omitempty"`
	// DecodingCodecs replaces the server's hardware decoding codecs when
	// it is not empty.
	DecodingCodecs []string `json:"decodingCodecs,omitempty"`
}

// ForHost returns options for a job on host: the host's hardware family,
// render nodes and decoding codecs, and the server's other options (tone
// mapping, low power, codec permissions, preset). It is pure.
func (options HardwareOptions) ForHost(host HostHardware) HardwareOptions {
	options.Type = host.Type
	if options.Type == "" {
		options.Type = HardwareAccelerationNone
	}
	options.VaapiDevice = host.VaapiDevice
	options.QsvDevice = host.QsvDevice
	if len(host.DecodingCodecs) > 0 {
		options.HardwareDecodingCodecs = append([]string(nil), host.DecodingCodecs...)
	}
	return options
}
