// SPDX-License-Identifier: GPL-2.0-only

package encoding

import "testing"

func TestPlacementFacts(t *testing.T) {
	software := hevcHDR10Job()
	qsv := hevcHDR10Job()
	qsv.Hardware = qsvOptions()
	sdr := h264SDRJob()
	remux := hevcHDR10Job()
	remux.OutputVideoCodec = "copy"
	dovi := hevcHDR10Job()
	dovi.VideoStream.RangeType = "DOVIWithHDR10"
	dovi.DirectDolbyVision = true
	doviSDR := hevcHDR10Job()
	doviSDR.VideoStream.RangeType = "DOVIWithSDR"

	for _, test := range []struct {
		name string
		job  HLSJob
		want HLSPlacementFacts
	}{
		{"hdr10 software", software, HLSPlacementFacts{SourceCodec: "hevc", SourceProfile: "main10", BitDepth: 10, HDR: "hdr10", NeedsToneMap: true, TargetCodec: "h264"}},
		{"hdr10 qsv", qsv, HLSPlacementFacts{SourceCodec: "hevc", SourceProfile: "main10", BitDepth: 10, HDR: "hdr10", NeedsToneMap: true, TargetCodec: "h264", Hardware: HardwareAccelerationQSV}},
		{"sdr", sdr, HLSPlacementFacts{SourceCodec: "h264", SourceProfile: "high", BitDepth: 8, HDR: "sdr", TargetCodec: "h264"}},
		{"remux keeps hdr without a tone map", remux, HLSPlacementFacts{SourceCodec: "hevc", SourceProfile: "main10", BitDepth: 10, HDR: "hdr10", TargetCodec: "copy"}},
		{"direct dolby vision", dovi, HLSPlacementFacts{SourceCodec: "hevc", SourceProfile: "main10", BitDepth: 10, HDR: "dovi", TargetCodec: "h264"}},
		{"dolby vision with an sdr base", doviSDR, HLSPlacementFacts{SourceCodec: "hevc", SourceProfile: "main10", BitDepth: 10, HDR: "dovi", TargetCodec: "h264"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := PlacementFacts(test.job, jellyfinFFmpegCapabilities()); got != test.want {
				t.Fatalf("PlacementFacts = %+v\nwant %+v", got, test.want)
			}
		})
	}
}
