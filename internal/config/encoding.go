// SPDX-License-Identifier: GPL-2.0-only

package config

import (
	"encoding/xml"
	"fmt"
)

// EncodingOptions is the persisted Jellyfin transcoding configuration
// (encoding.xml). It matches upstream/jellyfin/MediaBrowser.Model/
// Configuration/EncodingOptions.cs. Enum values keep the upstream member
// names, which are also their JSON and XML spellings.
type EncodingOptions struct {
	EncodingThreadCount                                       int
	TranscodingTempPath                                       string
	FallbackFontPath                                          string
	EnableFallbackFont                                        bool
	EnableAudioVbr                                            bool
	DownMixAudioBoost                                         float64
	DownMixStereoAlgorithm                                    string
	MaxMuxingQueueSize                                        int
	EnableThrottling                                          bool
	ThrottleDelaySeconds                                      int
	EnableSegmentDeletion                                     bool
	SegmentKeepSeconds                                        int
	HardwareAccelerationType                                  string
	EncoderAppPath                                            string
	EncoderAppPathDisplay                                     string
	VaapiDevice                                               string
	QsvDevice                                                 string
	EnableTonemapping                                         bool
	EnableVppTonemapping                                      bool
	EnableVideoToolboxTonemapping                             bool
	TonemappingAlgorithm                                      string
	TonemappingMode                                           string
	TonemappingRange                                          string
	TonemappingDesat                                          float64
	TonemappingPeak                                           float64
	TonemappingParam                                          float64
	VppTonemappingBrightness                                  float64
	VppTonemappingContrast                                    float64
	H264Crf                                                   int
	H265Crf                                                   int
	EncoderPreset                                             string
	DeinterlaceDoubleRate                                     bool
	DeinterlaceMethod                                         string
	EnableDecodingColorDepth10Hevc                            bool
	EnableDecodingColorDepth10Vp9                             bool
	EnableDecodingColorDepth10HevcRext                        bool
	EnableDecodingColorDepth12HevcRext                        bool
	EnableEnhancedNvdecDecoder                                bool
	PreferSystemNativeHwDecoder                               bool
	EnableIntelLowPowerH264HwEncoder                          bool
	EnableIntelLowPowerHevcHwEncoder                          bool
	EnableHardwareEncoding                                    bool
	AllowHevcEncoding                                         bool
	AllowAv1Encoding                                          bool
	EnableSubtitleExtraction                                  bool
	SubtitleExtractionTimeoutMinutes                          int
	HardwareDecodingCodecs                                    []string
	AllowOnDemandMetadataBasedKeyframeExtractionForExtensions []string
	HlsAudioSeekStrategy                                      string

	// AllowPlainHttpTranscodeWorkers is Deadfin-only (D-118, #664): true
	// admits transcode workers over plain HTTP from a non-loopback address.
	// DEADFIN_TRANSCODE_WORKERS_ALLOW_HTTP wins when it is set. Stock
	// clients do not send it; a save that omits it keeps the stored value.
	AllowPlainHttpTranscodeWorkers *bool `json:",omitzero" xml:",omitempty"`
}

// DefaultEncodingOptions returns the upstream constructor defaults
// (EncodingOptions.cs:21-74).
func DefaultEncodingOptions() EncodingOptions {
	return EncodingOptions{
		EncodingThreadCount:              -1,
		DownMixAudioBoost:                2,
		DownMixStereoAlgorithm:           "None",
		MaxMuxingQueueSize:               2048,
		ThrottleDelaySeconds:             180,
		SegmentKeepSeconds:               720,
		HardwareAccelerationType:         "none",
		VaapiDevice:                      "/dev/dri/renderD128",
		TonemappingAlgorithm:             "bt2390",
		TonemappingMode:                  "auto",
		TonemappingRange:                 "auto",
		TonemappingPeak:                  100,
		VppTonemappingBrightness:         16,
		VppTonemappingContrast:           1,
		H264Crf:                          23,
		H265Crf:                          28,
		EncoderPreset:                    "auto",
		DeinterlaceMethod:                "yadif",
		EnableDecodingColorDepth10Hevc:   true,
		EnableDecodingColorDepth10Vp9:    true,
		EnableEnhancedNvdecDecoder:       true,
		PreferSystemNativeHwDecoder:      true,
		EnableHardwareEncoding:           true,
		EnableSubtitleExtraction:         true,
		SubtitleExtractionTimeoutMinutes: 30,
		HardwareDecodingCodecs:           []string{"h264", "vc1"},
		HlsAudioSeekStrategy:             "TrimCopiedAudio",
		AllowOnDemandMetadataBasedKeyframeExtractionForExtensions: []string{"mkv"},
	}
}

// encodingOptionsXML replaces the string arrays with wrappers. Upstream's
// XmlSerializer writes <HardwareDecodingCodecs><string>h264</string>...;
// decoding into a pre-filled slice would append to the defaults.
type encodingOptionsXML struct {
	fieldsEncodingOptions
	HardwareDecodingCodecs                                    *stringArrayXML `xml:"HardwareDecodingCodecs"`
	AllowOnDemandMetadataBasedKeyframeExtractionForExtensions *stringArrayXML `xml:"AllowOnDemandMetadataBasedKeyframeExtractionForExtensions"`
}

type fieldsEncodingOptions EncodingOptions

// MarshalXML writes the document root used by Jellyfin's encoding.xml.
func (options EncodingOptions) MarshalXML(encoder *xml.Encoder, start xml.StartElement) error {
	start.Name.Local = "EncodingOptions"
	document := encodingOptionsXML{
		fieldsEncodingOptions:  fieldsEncodingOptions(options),
		HardwareDecodingCodecs: &stringArrayXML{Values: options.HardwareDecodingCodecs},
		AllowOnDemandMetadataBasedKeyframeExtractionForExtensions: &stringArrayXML{
			Values: options.AllowOnDemandMetadataBasedKeyframeExtractionForExtensions,
		},
	}
	document.fieldsEncodingOptions.HardwareDecodingCodecs = nil
	document.fieldsEncodingOptions.AllowOnDemandMetadataBasedKeyframeExtractionForExtensions = nil
	return encoder.EncodeElement(document, start)
}

// UnmarshalXML accepts only the encoding configuration document root.
// Fields omitted by the source retain the defaults supplied by the caller.
// A present array element replaces the default array.
func (options *EncodingOptions) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	if start.Name.Local != "EncodingOptions" {
		return fmt.Errorf("config: unexpected encoding configuration root %q", start.Name.Local)
	}
	document := encodingOptionsXML{fieldsEncodingOptions: fieldsEncodingOptions(*options)}
	document.fieldsEncodingOptions.HardwareDecodingCodecs = nil
	document.fieldsEncodingOptions.AllowOnDemandMetadataBasedKeyframeExtractionForExtensions = nil
	if err := decoder.DecodeElement(&document, &start); err != nil {
		return err
	}
	decoded := EncodingOptions(document.fieldsEncodingOptions)
	decoded.HardwareDecodingCodecs = options.HardwareDecodingCodecs
	decoded.AllowOnDemandMetadataBasedKeyframeExtractionForExtensions = options.AllowOnDemandMetadataBasedKeyframeExtractionForExtensions
	if document.HardwareDecodingCodecs != nil {
		decoded.HardwareDecodingCodecs = append([]string{}, document.HardwareDecodingCodecs.Values...)
	}
	if document.AllowOnDemandMetadataBasedKeyframeExtractionForExtensions != nil {
		decoded.AllowOnDemandMetadataBasedKeyframeExtractionForExtensions = append([]string{}, document.AllowOnDemandMetadataBasedKeyframeExtractionForExtensions.Values...)
	}
	*options = decoded
	return nil
}

// UnmarshalJSON decodes a posted encoding configuration over the upstream
// constructor defaults, so a member that the client omits keeps its upstream
// default and never becomes a Go zero value.
func (options *EncodingOptions) UnmarshalJSON(data []byte) error {
	decoded := fieldsEncodingOptions(DefaultEncodingOptions())
	if err := decodeJSONOverDefaults(data, &decoded); err != nil {
		return err
	}
	*options = EncodingOptions(decoded)
	options.HardwareDecodingCodecs = nonNilStrings(options.HardwareDecodingCodecs)
	options.AllowOnDemandMetadataBasedKeyframeExtractionForExtensions = nonNilStrings(options.AllowOnDemandMetadataBasedKeyframeExtractionForExtensions)
	return nil
}
