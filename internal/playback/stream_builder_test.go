// SPDX-License-Identifier: GPL-2.0-only

package playback

// These tests port the observable fixture matrix from
// tests/Jellyfin.Model.Tests/Dlna/StreamBuilderTests.cs @ b04614e
// (notably BuildVideoItemSimple, BuildVideoItemWithFirstExplicitStream, and
// BuildVideoItemWithDirectPlayExplicitStreams). The committed Goldenrec corpus
// was generated from that same pinned revision.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type noTranscoderSupport struct{}

func (noTranscoderSupport) CanEncodeToAudioCodec(string) bool    { return false }
func (noTranscoderSupport) CanEncodeToSubtitleCodec(string) bool { return false }
func (noTranscoderSupport) CanExtractSubtitles(string) bool      { return false }

type streamBuilderAssertions struct {
	Cases []streamBuilderAssertionCase `json:"cases"`
}

type streamBuilderAssertionCase struct {
	ID      string                       `json:"id"`
	Method  string                       `json:"method"`
	Profile string                       `json:"profile"`
	Source  string                       `json:"source"`
	Options streamBuilderFixtureOptions  `json:"options"`
	Expect  streamBuilderFixtureExpected `json:"expect"`
}

type streamBuilderFixtureOptions struct {
	AudioStreamIndex     json.RawMessage `json:"audioStreamIndex"`
	SubtitleStreamIndex  json.RawMessage `json:"subtitleStreamIndex"`
	EnableDirectStream   bool            `json:"enableDirectStream"`
	AllowAudioStreamCopy bool            `json:"allowAudioStreamCopy"`
	AllowVideoStreamCopy bool            `json:"allowVideoStreamCopy"`
}

type streamBuilderFixtureExpected struct {
	PlayMethod       *string  `json:"playMethod"`
	TranscodeReasons []string `json:"transcodeReasons"`
	Kind             *string  `json:"kind"`
	Protocol         *string  `json:"protocol"`
}

type streamBuilderGolden struct {
	Cases []streamBuilderGoldenCase `json:"cases"`
}

type streamBuilderGoldenCase struct {
	ID      string                        `json:"id"`
	Profile string                        `json:"profile"`
	Source  string                        `json:"source"`
	Video   streamBuilderGoldenStreamData `json:"video"`
	Audio   streamBuilderGoldenStreamData `json:"audio"`
}

type streamBuilderGoldenStreamData struct {
	StreamInfo    *streamBuilderGoldenDecision
	StreamInfoRaw json.RawMessage
	Error         string `json:"error"`
}

func (s *streamBuilderGoldenStreamData) UnmarshalJSON(data []byte) error {
	var raw struct {
		StreamInfo json.RawMessage `json:"streamInfo"`
		Error      string          `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Error = raw.Error
	s.StreamInfoRaw = raw.StreamInfo
	if len(raw.StreamInfo) == 0 || string(raw.StreamInfo) == "null" {
		s.StreamInfo = nil
		return nil
	}
	var decision streamBuilderGoldenDecision
	if err := json.Unmarshal(raw.StreamInfo, &decision); err != nil {
		return err
	}
	s.StreamInfo = &decision
	return nil
}

type streamBuilderGoldenDecision struct {
	PlayMethod          string   `json:"PlayMethod"`
	TranscodeReasons    []string `json:"TranscodeReasons"`
	Container           *string  `json:"Container"`
	SubProtocol         string   `json:"SubProtocol"`
	MediaSourceID       string   `json:"MediaSourceId"`
	AudioStreamIndex    *int     `json:"AudioStreamIndex"`
	SubtitleStreamIndex *int     `json:"SubtitleStreamIndex"`
	AudioCodecs         []string `json:"AudioCodecs"`
	VideoCodecs         []string `json:"VideoCodecs"`
	TargetVideoCodec    []string `json:"TargetVideoCodec"`
	TargetAudioCodec    []string `json:"TargetAudioCodec"`
}

// TestStreamBuilderUpstreamFixtureMatrix runs every committed upstream
// InlineData outcome (176 simple, 81 first-explicit, 23 direct-play-explicit)
// through the same exported StreamBuilder entry point PlaybackInfo will use.
func TestStreamBuilderUpstreamFixtureMatrix(t *testing.T) {
	var matrix streamBuilderAssertions
	loadJSONFixture(t, "streambuilder-assertions.yaml", &matrix)
	if got, want := len(matrix.Cases), 280; got != want {
		t.Fatalf("fixture matrix case count changed: got %d want %d", got, want)
	}

	builder := NewStreamBuilder(noTranscoderSupport{})
	for _, tc := range matrix.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			options, source := streamBuilderOptions(t, tc.Profile, tc.Source)
			applyFixtureOptions(t, &options, source, tc.Options)

			got, err := builder.GetOptimalVideoStream(&options)
			if err != nil {
				t.Fatalf("GetOptimalVideoStream: %v", err)
			}
			if got == nil {
				t.Fatal("GetOptimalVideoStream returned nil")
			}
			assertFixtureDecision(t, got, tc.Expect)

			if tc.Method == "BuildVideoItemWithFirstExplicitStream" || tc.Method == "BuildVideoItemWithDirectPlayExplicitStreams" {
				if !equalIntPointers(got.AudioStreamIndex, options.AudioStreamIndex) {
					t.Errorf("AudioStreamIndex = %v, want explicitly selected %v", got.AudioStreamIndex, options.AudioStreamIndex)
				}
				if !equalIntPointers(got.SubtitleStreamIndex, options.SubtitleStreamIndex) {
					t.Errorf("SubtitleStreamIndex = %v, want explicitly selected %v", got.SubtitleStreamIndex, options.SubtitleStreamIndex)
				}
			}
		})
	}
}

// TestStreamBuilderGoldenDecisionMatrix consumes all 627 profile×source Goldenrec
// cases. StreamInfo JSON and ToURL byte-level serialization belong to P4-01c;
// this test deliberately pins only StreamBuilder's public decision output.
func TestStreamBuilderGoldenDecisionMatrix(t *testing.T) {
	var golden streamBuilderGolden
	loadJSONFixture(t, filepath.Join("golden", "streambuilder.json"), &golden)
	if got, want := len(golden.Cases), 627; got != want {
		t.Fatalf("golden case count changed: got %d want %d", got, want)
	}

	builder := NewStreamBuilder(noTranscoderSupport{})
	for _, tc := range golden.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			videoOptions, _ := streamBuilderOptions(t, tc.Profile, tc.Source)
			video, videoErr := builder.GetOptimalVideoStream(&videoOptions)

			// Goldenrec constructs video, then audio from a new options object that
			// aliases the same source, and serializes video only afterward.
			audioOptions, _ := streamBuilderOptions(t, tc.Profile, tc.Source)
			audioOptions.MediaSources = videoOptions.MediaSources
			audio, audioErr := builder.GetOptimalAudioStream(&audioOptions)

			assertGoldenDecision(t, "video", tc.Video, video, videoErr)
			assertGoldenDecision(t, "audio", tc.Audio, audio, audioErr)
		})
	}
}

// Stable ordering is observable because upstream's LINQ OrderBy preserves input
// order on equal ranking keys (StreamBuilder.cs:260-302); D-09 requires Go to do
// the same. This uses only the public builder decision seam.
func TestStreamBuilderKeepsFirstSourceOnEqualRank(t *testing.T) {
	options, source := streamBuilderOptions(t, "Chrome", "mp4-h264-aac-vtt-2600k")
	first, second := source, source
	first.ID = "first-source"
	second.ID = "second-source"
	options.MediaSourceID = ""
	options.MediaSources = []MediaSourceInfo{first, second}

	got, err := NewStreamBuilder(noTranscoderSupport{}).GetOptimalVideoStream(&options)
	if err != nil {
		t.Fatalf("GetOptimalVideoStream: %v", err)
	}
	if got == nil {
		t.Fatal("GetOptimalVideoStream returned nil")
	}
	if got.MediaSourceID() != first.ID {
		t.Errorf("equal-rank source selection = %q, want first input %q", got.MediaSourceID(), first.ID)
	}
}

// NewMediaOptions is the public construction seam. Its defaults are material:
// direct play/direct stream are enabled and negotiation is streaming context.
func TestNewMediaOptionsUsesUpstreamDefaults(t *testing.T) {
	options := NewMediaOptions()
	if !options.EnableDirectPlay {
		t.Error("EnableDirectPlay default = false, want true")
	}
	if !options.EnableDirectStream {
		t.Error("EnableDirectStream default = false, want true")
	}
	if options.Context != ContextStreaming {
		t.Errorf("Context = %v, want streaming", options.Context)
	}
}

// Pinned upstream StreamBuilder.cs:1288-1296 returns a nil DirectPlayProfile
// for forced modes. The public build must retain that forced decision without
// dereferencing the absent profile.
func TestStreamBuilderForcedModesWithoutDirectPlayProfile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*MediaOptions)
		want   PlayMethod
	}{
		{"direct-play", func(o *MediaOptions) { o.ForceDirectPlay = true }, PlayMethodDirectPlay},
		{"direct-stream", func(o *MediaOptions) { o.ForceDirectStream = true }, PlayMethodDirectStream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options, _ := streamBuilderOptions(t, "Chrome", "mp4-h264-aac-vtt-2600k")
			options.Profile.DirectPlayProfiles = nil
			tc.mutate(&options)

			got, err := NewStreamBuilder(noTranscoderSupport{}).GetOptimalVideoStream(&options)
			if err != nil {
				t.Fatalf("GetOptimalVideoStream: %v", err)
			}
			if got == nil {
				t.Fatal("GetOptimalVideoStream returned nil")
			}
			if got.PlayMethod != tc.want {
				t.Errorf("PlayMethod = %v, want %v", got.PlayMethod, tc.want)
			}
		})
	}
}

// StreamBuilder.cs:686-700 compares language values, not object identity.
// Distinct Go string pointers holding "eng" must remain one candidate pool.
func TestStreamBuilderReselectsSameLanguageAudioByValue(t *testing.T) {
	defaultLanguage := testPtr("eng")
	candidateLanguage := testPtr("eng")
	defaultAudio := 1
	source := MediaSourceInfo{
		ID:                      "language-selection",
		Container:               "mp4",
		Protocol:                MediaProtocolFile,
		DefaultAudioStreamIndex: &defaultAudio,
		DefaultAudioIndexSource: AudioIndexLanguage,
		MediaStreams: []MediaStream{
			{Index: 0, Type: StreamTypeVideo, Codec: "h264"},
			{Index: 1, Type: StreamTypeAudio, Codec: "ac3", IsDefault: true, Language: defaultLanguage},
			{Index: 2, Type: StreamTypeAudio, Codec: "aac", Language: candidateLanguage},
		},
	}
	profile := &DeviceProfile{DirectPlayProfiles: []DirectPlayProfile{{Type: DlnaVideo, Container: "mp4", VideoCodec: "h264", AudioCodec: "aac"}}}
	options := NewMediaOptions()
	options.ItemID = "11d229b72d484b959f9b49f6ab75e613"
	options.MediaSourceID = source.ID
	options.MediaSources = []MediaSourceInfo{source}
	options.Profile = profile
	options.AllowAudioStreamCopy = true
	options.AllowVideoStreamCopy = true
	options.EnableDirectStream = false

	got, err := NewStreamBuilder(noTranscoderSupport{}).GetOptimalVideoStream(&options)
	if err != nil {
		t.Fatalf("GetOptimalVideoStream: %v", err)
	}
	if got == nil || got.AudioStreamIndex == nil {
		t.Fatalf("audio selection = %#v, want AAC index 2", got)
	}
	if got.PlayMethod != PlayMethodDirectPlay {
		t.Errorf("PlayMethod = %v, want DirectPlay", got.PlayMethod)
	}
	if want := 2; *got.AudioStreamIndex != want {
		t.Errorf("AudioStreamIndex = %d, want %d", *got.AudioStreamIndex, want)
	}
}

// StreamBuilder.cs:1741-2139 applies typed conditions after a transcode choice.
// Values such as profiles, range types, tags, rotations, and bools are not
// integers and must not be discarded by numeric pre-parsing.
func TestStreamBuilderAppliesTypedTranscodeConditions(t *testing.T) {
	source := MediaSourceInfo{
		ID: "typed-conditions", Container: "mp4", Protocol: MediaProtocolFile,
		MediaStreams: []MediaStream{
			{Index: 0, Type: StreamTypeVideo, Codec: "h264", Profile: testPtr("High"), CodecTag: testPtr("avc1"), Rotation: testPtr(90), RefFrames: testPtr(6), IsAVC: testPtr(false), IsInterlaced: true},
			{Index: 1, Type: StreamTypeAudio, Codec: "aac", IsDefault: true, Channels: testPtr(6)},
		},
	}
	profile := &DeviceProfile{
		DirectPlayProfiles:  []DirectPlayProfile{{Type: DlnaVideo, Container: "mp4", VideoCodec: "hevc", AudioCodec: "aac"}},
		TranscodingProfiles: []TranscodingProfile{{Type: DlnaVideo, Container: "mkv", VideoCodec: "h264", AudioCodec: "aac", Protocol: StreamProtocolHTTP}},
		CodecProfiles: []CodecProfile{{Type: CodecVideo, Container: "mkv", Codec: "h264", Conditions: []ProfileCondition{
			{Property: PropVideoProfile, Condition: ConditionEquals, Value: "High|Main"},
			{Property: PropVideoRangeType, Condition: ConditionEquals, Value: "HDR10|SDR"},
			{Property: PropVideoCodecTag, Condition: ConditionEquals, Value: " hvc1 | avc1 "},
			{Property: PropVideoRotation, Condition: ConditionEquals, Value: " 0 | 90 "},
			{Property: PropRefFrames, Condition: ConditionLessThanEqual, Value: "4"},
			{Property: PropIsAvc, Condition: ConditionEquals, Value: "true"},
			{Property: PropIsAnamorphic, Condition: ConditionNotEquals, Value: "false"},
			{Property: PropIsInterlaced, Condition: ConditionEquals, Value: "false"},
		}}},
	}
	options := NewMediaOptions()
	options.ItemID = "11d229b72d484b959f9b49f6ab75e613"
	options.MediaSourceID, options.MediaSources, options.Profile = source.ID, []MediaSourceInfo{source}, profile
	options.AllowAudioStreamCopy, options.AllowVideoStreamCopy, options.EnableDirectStream = true, true, false

	got, err := NewStreamBuilder(noTranscoderSupport{}).GetOptimalVideoStream(&options)
	if err != nil || got == nil {
		t.Fatalf("GetOptimalVideoStream = %#v, %v", got, err)
	}
	if got.PlayMethod != PlayMethodTranscode {
		t.Fatalf("PlayMethod = %v, want Transcode", got.PlayMethod)
	}
	for key, want := range map[string]string{"profile": "High,Main", "rangetype": "HDR10,SDR", "codectag": "hvc1,avc1", "rotation": "0,90", "maxrefframes": "4", "deinterlace": "true"} {
		if value := got.GetOption("h264", key); value != want {
			t.Errorf("h264 %s = %q, want %q", key, value, want)
		}
	}
	if !got.RequireAvc {
		t.Error("RequireAvc = false, want true")
	}
	if !got.RequireNonAnamorphic {
		t.Error("RequireNonAnamorphic = false, want true")
	}
	if target := got.TargetRefFrames(); target == nil || *target != 4 {
		t.Errorf("TargetRefFrames = %v, want 4", target)
	}
}

// StreamBuilder.cs:2077-2139 deliberately emits tag and rotation options only
// for Equals/EqualsAny; NotEquals is a compatibility check, never a setter.
func TestStreamBuilderDoesNotApplyNotEqualsTagOrRotation(t *testing.T) {
	options := audioDecisionOptions("h264", "h264", "aac")
	video := &options.MediaSources[0].MediaStreams[0]
	video.CodecTag, video.Rotation = testPtr("avc1"), testPtr(90)
	options.Profile.CodecProfiles = []CodecProfile{{Type: CodecVideo, Container: "ts", Codec: "h264", Conditions: []ProfileCondition{
		{Property: PropVideoCodecTag, Condition: ConditionNotEquals, Value: "hvc1"},
		{Property: PropVideoRotation, Condition: ConditionNotEquals, Value: "0"},
	}}}
	got := buildAudioDecision(t, options)
	if got.GetOption("h264", "codectag") != "" {
		t.Errorf("NotEquals CodecTag set codectag=%q", got.GetOption("h264", "codectag"))
	}
	if got.GetOption("h264", "rotation") != "" {
		t.Errorf("NotEquals Rotation set rotation=%q", got.GetOption("h264", "rotation"))
	}
}

// StreamInfo.cs:1343-1355 and BuildStreamVideo use the current target option,
// not source facts, as a GreaterThanEqual setter baseline. A source value does
// not seed output until a prior condition has created that target option.
func TestStreamBuilderGreaterThanEqualConditionsUseTargetOptionBaseline(t *testing.T) {
	options := audioDecisionOptions("h264", "h264", "aac")
	video := &options.MediaSources[0].MediaStreams[0]
	video.BitRate, video.RefFrames = testPtr(5000000), testPtr(6)
	options.Profile.CodecProfiles = []CodecProfile{{Type: CodecVideo, Container: "ts", Codec: "h264", Conditions: []ProfileCondition{
		{Property: PropVideoBitrate, Condition: ConditionGreaterThanEqual, Value: "1000000"},
		{Property: PropRefFrames, Condition: ConditionGreaterThanEqual, Value: "4"},
	}}}
	got := buildAudioDecision(t, options)
	if got.VideoBitrate == nil || *got.VideoBitrate != 1000000 {
		t.Errorf("VideoBitrate = %v, want request 1000000", got.VideoBitrate)
	}
	if target := got.TargetRefFrames(); target == nil || *target != 4 {
		t.Errorf("TargetRefFrames = %v, want request 4", target)
	}

	priorTarget := audioDecisionOptions("h264", "h264", "aac")
	priorTarget.MediaSources[0].MediaStreams[0].RefFrames = testPtr(1)
	priorTarget.Profile.CodecProfiles = []CodecProfile{{Type: CodecVideo, Container: "ts", Codec: "h264", Conditions: []ProfileCondition{
		{Property: PropRefFrames, Condition: ConditionGreaterThanEqual, Value: "12"},
		{Property: PropRefFrames, Condition: ConditionGreaterThanEqual, Value: "4"},
	}}}
	priorResult := buildAudioDecision(t, priorTarget)
	if target := priorResult.TargetRefFrames(); target == nil || *target != 12 {
		t.Errorf("TargetRefFrames with prior target option = %v, want 12", target)
	}
}

// StreamBuilder.cs:197-210 reclamps a requested pure-audio transcode bitrate
// to the configured cap and narrows only after the Int32 maximum is applied.
func TestStreamBuilderClampsPureAudioTranscodeBitrate(t *testing.T) {
	t.Run("configured cap overrides request", func(t *testing.T) {
		options := pureAudioTranscodeOptions(testPtr(500000), testPtr(200000))
		got, err := NewStreamBuilder(audioEncodingSupport{}).GetOptimalAudioStream(&options)
		if err != nil || got == nil {
			t.Fatalf("GetOptimalAudioStream = %#v, %v", got, err)
		}
		if got.AudioBitrate == nil || *got.AudioBitrate != 200000 {
			t.Errorf("AudioBitrate = %v, want configured cap 200000", got.AudioBitrate)
		}
	})
	t.Run("large request narrows at Int32 maximum", func(t *testing.T) {
		options := pureAudioTranscodeOptions(testPtr(int(^uint(0)>>1)), testPtr(int(^uint(0)>>1)))
		options.MediaSources[0].MediaStreams[0].BitRate = nil
		got, err := NewStreamBuilder(audioEncodingSupport{}).GetOptimalAudioStream(&options)
		if err != nil || got == nil {
			t.Fatalf("GetOptimalAudioStream = %#v, %v", got, err)
		}
		if got.AudioBitrate == nil || *got.AudioBitrate != int(^uint32(0)>>1) {
			t.Errorf("AudioBitrate = %v, want Int32 max", got.AudioBitrate)
		}
	})
}

func pureAudioTranscodeOptions(request, configured *int) MediaOptions {
	zero := 0
	o := NewMediaOptions()
	o.DeviceID, o.MediaSourceID = "audio-transcode", "audio-transcode"
	o.EnableDirectPlay, o.EnableDirectStream = false, false
	o.AudioTranscodingBitrate, o.MaxBitrate = request, configured
	o.MediaSources = []MediaSourceInfo{{ID: "audio-transcode", Container: "mp3", Protocol: MediaProtocolFile, SupportsTranscoding: true, MediaStreams: []MediaStream{{Index: 0, Type: StreamTypeAudio, Codec: "aac", IsDefault: true, BitRate: testPtr(1000000)}}, DefaultAudioStreamIndex: &zero}}
	o.Profile = &DeviceProfile{TranscodingProfiles: []TranscodingProfile{{Type: DlnaAudio, Container: "mp3", AudioCodec: "aac", Protocol: StreamProtocolHTTP}}}
	return o
}

// testPtr returns a pointer to a copy of value.
func testPtr[T any](value T) *T { return &value }

type audioEncodingSupport struct{}

func (audioEncodingSupport) CanEncodeToAudioCodec(string) bool    { return true }
func (audioEncodingSupport) CanEncodeToSubtitleCodec(string) bool { return false }
func (audioEncodingSupport) CanExtractSubtitles(string) bool      { return false }

// StreamBuilder.cs:958-1125: audio decisions are observable through the
// returned StreamInfo and its MediaSource, rather than private candidate logic.
func TestStreamBuilderAudioDecisionRegressions(t *testing.T) {
	t.Run("failed direct audio candidate is not copied", func(t *testing.T) {
		o := audioDecisionOptions("h264", "h264", "aac,ac3")
		o.Profile.CodecProfiles = []CodecProfile{{Type: CodecVideoAudio, Container: "ts", Codec: "aac", Conditions: []ProfileCondition{{Property: PropAudioChannels, Condition: ConditionLessThanEqual, Value: "2", IsRequired: false}}}}
		got := buildAudioDecision(t, o)
		if got.TranscodeReasons&ReasonAudioChannelsNotSupported == 0 {
			t.Error("missing AudioChannelsNotSupported")
		}
		if !reflect.DeepEqual(got.AudioCodecs, []string{"aac", "ac3"}) {
			t.Errorf("AudioCodecs = %q, want uncopied [aac ac3]", got.AudioCodecs)
		}
		if got.AudioSampleRate != nil {
			t.Errorf("AudioSampleRate = %v, want nil without copied direct candidate", got.AudioSampleRate)
		}
	})

	t.Run("failed direct candidate keeps channel option audio qualified", func(t *testing.T) {
		o := audioDecisionOptions("hevc", "h264", "aac")
		o.Profile.CodecProfiles = []CodecProfile{{Type: CodecVideoAudio, Container: "ts", Codec: "aac", Conditions: []ProfileCondition{{Property: PropAudioChannels, Condition: ConditionLessThanEqual, Value: "2", IsRequired: false}}}}
		got := buildAudioDecision(t, o)
		if got.GetTargetAudioChannels("aac") != 2 {
			t.Errorf("aac target channels = %d, want 2", got.GetTargetAudioChannels("aac"))
		}
		if got.GetOption("hevc", "audiochannels") != "" {
			t.Errorf("source video qualifier copied after failed direct candidate: %q", got.GetOption("hevc", "audiochannels"))
		}
	})

	// StreamBuilder.cs:891 and :988 call audio compatibility with
	// isSecondary=false while ranking video-transcode profiles. The selected
	// stream may be physically secondary without acquiring that reason here.
	t.Run("video transcode ignores source secondary audio flag", func(t *testing.T) {
		o := audioDecisionOptions("h264", "h264", "aac")
		o.MediaSources[0].MediaStreams[1].IsDefault = false
		o.MediaSources[0].DefaultAudioStreamIndex = testPtr(2)
		o.MediaSources[0].MediaStreams = append(o.MediaSources[0].MediaStreams, MediaStream{Index: 2, Type: StreamTypeAudio, Codec: "aac", Channels: testPtr(6), IsDefault: true})
		o.Profile.CodecProfiles = []CodecProfile{{Type: CodecVideoAudio, Container: "ts", Codec: "aac", Conditions: []ProfileCondition{{Property: PropIsSecondaryAudio, Condition: ConditionEquals, Value: "false", IsRequired: false}}}}
		got := buildAudioDecision(t, o)
		if got.TranscodeReasons&ReasonSecondaryAudioNotSupported != 0 {
			t.Errorf("TranscodeReasons = %v, must not include SecondaryAudioNotSupported", got.TranscodeReasons)
		}
	})

	t.Run("channel limit mutates target source stream", func(t *testing.T) {
		o := audioDecisionOptions("h264", "h264", "aac,ac3")
		o.Profile.TranscodingProfiles[0].MaxAudioChannels = "2"
		got := buildAudioDecision(t, o)
		if got.TranscodeReasons&ReasonAudioChannelsNotSupported == 0 {
			t.Error("missing AudioChannelsNotSupported")
		}
		stream := got.MediaSource.MediaStream(StreamTypeAudio, 1)
		if stream == nil || stream.Channels == nil || *stream.Channels != 2 {
			t.Errorf("target source audio channels = %#v, want 2", stream)
		}
	})

	t.Run("audio qualifier remains source video codec", func(t *testing.T) {
		o := audioDecisionOptions("hevc", "h264", "aac")
		got := buildAudioDecision(t, o)
		if got.GetOption("hevc", "audiochannels") != "6" {
			t.Errorf("hevc audiochannels = %q, want 6", got.GetOption("hevc", "audiochannels"))
		}
		if got.GetOption("h264", "audiochannels") != "" {
			t.Errorf("target h264 qualifier unexpectedly set to %q", got.GetOption("h264", "audiochannels"))
		}
	})

	t.Run("bitrate precedes channel condition and zero max remains explicit", func(t *testing.T) {
		conditioned := audioDecisionOptions("h264", "h264", "aac")
		conditioned.Profile.CodecProfiles = []CodecProfile{{Type: CodecVideoAudio, Container: "ts", Codec: "aac", Conditions: []ProfileCondition{{Property: PropAudioChannels, Condition: ConditionLessThanEqual, Value: "2", IsRequired: false}}}}
		got := buildAudioDecision(t, conditioned)
		if got.AudioBitrate == nil || *got.AudioBitrate != 640000 {
			t.Errorf("AudioBitrate = %v, want 640000 before channel reduction", got.AudioBitrate)
		}
		if got.GetTargetAudioChannels("aac") != 2 {
			t.Errorf("TargetAudioChannels = %d, want 2", got.GetTargetAudioChannels("aac"))
		}

		// StreamBuilder.cs:1087 forwards the chosen target audio codec, not the
		// first profile codec, into bitrate defaults (StreamInfo.cs:545-567).
		defaultRate := audioDecisionOptions("h264", "h264", "flac,aac")
		defaultRate.MediaSources[0].MediaStreams[1].BitRate = nil
		defaultRate.Profile.CodecProfiles = []CodecProfile{{Type: CodecVideoAudio, Container: "ts", Codec: "aac", Conditions: []ProfileCondition{{Property: PropAudioChannels, Condition: ConditionLessThanEqual, Value: "2", IsRequired: false}}}}
		defaultResult := buildAudioDecision(t, defaultRate)
		if defaultResult.AudioBitrate == nil || *defaultResult.AudioBitrate != 128000 {
			t.Errorf("AudioBitrate with target flac,aac = %v, want AAC default 128000", defaultResult.AudioBitrate)
		}

		zero := audioDecisionOptions("h264", "h264", "aac")
		zero.MaxAudioChannels = testPtr(0)
		zeroResult := buildAudioDecision(t, zero)
		if zeroResult.GlobalMaxAudioChannels == nil || *zeroResult.GlobalMaxAudioChannels != 0 {
			t.Errorf("GlobalMaxAudioChannels = %v, want explicit 0", zeroResult.GlobalMaxAudioChannels)
		}
		if zeroResult.AudioBitrate == nil || *zeroResult.AudioBitrate != 128000 {
			t.Errorf("AudioBitrate with explicit zero channels = %v, want 128000", zeroResult.AudioBitrate)
		}
	})

	t.Run("mkv does not direct play through webm-only audio profile", func(t *testing.T) {
		one := 1
		o := NewMediaOptions()

		o.DeviceID = "audio-mkv"
		o.MediaSourceID = "audio-mkv"
		o.MediaSources = []MediaSourceInfo{{ID: "audio-mkv", Container: "mkv,webm", Protocol: MediaProtocolFile, SupportsDirectPlay: true, SupportsDirectStream: true, MediaStreams: []MediaStream{{Index: 1, Type: StreamTypeAudio, Codec: "opus", IsDefault: true}}, DefaultAudioStreamIndex: &one}}
		o.Profile = &DeviceProfile{DirectPlayProfiles: []DirectPlayProfile{{Type: DlnaAudio, Container: "webm", AudioCodec: "opus"}}}
		got, err := NewStreamBuilder(audioEncodingSupport{}).GetOptimalAudioStream(&o)
		if err != nil || got == nil {
			t.Fatalf("GetOptimalAudioStream = %#v, %v", got, err)
		}
		if got.PlayMethod != PlayMethodDirectStream {
			t.Errorf("PlayMethod = %v, want DirectStream", got.PlayMethod)
		}
		if got.TranscodeReasons&ReasonContainerNotSupported == 0 {
			t.Errorf("TranscodeReasons = %v, want ContainerNotSupported", got.TranscodeReasons)
		}
	})
}

// StreamBuilder.cs:89-104 applies forced audio methods before resolving an
// audio stream. A forced request must not fail merely because no stream exists.
func TestStreamBuilderForcesAudioMethodBeforeStreamLookup(t *testing.T) {
	o := NewMediaOptions()
	o.DeviceID, o.MediaSourceID, o.ForceDirectPlay = "forced-audio", "forced-audio", true
	o.Profile = &DeviceProfile{}
	o.MediaSources = []MediaSourceInfo{{ID: "forced-audio", Container: "mp3", Protocol: MediaProtocolFile, SupportsDirectPlay: true}}
	got, err := NewStreamBuilder(audioEncodingSupport{}).GetOptimalAudioStream(&o)
	if err != nil || got == nil {
		t.Fatalf("GetOptimalAudioStream = %#v, %v", got, err)
	}
	if got.PlayMethod != PlayMethodDirectPlay {
		t.Errorf("PlayMethod = %v, want DirectPlay", got.PlayMethod)
	}
}

func audioDecisionOptions(sourceVideo, targetVideo, targetAudio string) MediaOptions {
	one := 1
	o := NewMediaOptions()
	o.DeviceID, o.MediaSourceID = "audio-decision", "audio-decision"
	o.AllowAudioStreamCopy, o.AllowVideoStreamCopy, o.EnableDirectStream = true, true, false
	o.MediaSources = []MediaSourceInfo{{ID: "audio-decision", Container: "mp4", Protocol: MediaProtocolFile, SupportsTranscoding: true, SupportsDirectPlay: true, SupportsDirectStream: true, MediaStreams: []MediaStream{{Index: 0, Type: StreamTypeVideo, Codec: sourceVideo, Width: testPtr(1920), Height: testPtr(1080), BitRate: testPtr(2000000)}, {Index: 1, Type: StreamTypeAudio, Codec: "aac", Channels: testPtr(6), BitRate: testPtr(640000), SampleRate: testPtr(48000), IsDefault: true}}, DefaultAudioStreamIndex: &one}}
	o.Profile = &DeviceProfile{TranscodingProfiles: []TranscodingProfile{{Type: DlnaVideo, Context: ContextStreaming, Container: "ts", Protocol: StreamProtocolHTTP, VideoCodec: targetVideo, AudioCodec: targetAudio}}}
	return o
}

func buildAudioDecision(t *testing.T, options MediaOptions) *StreamInfo {
	t.Helper()
	got, err := NewStreamBuilder(audioEncodingSupport{}).GetOptimalVideoStream(&options)
	if err != nil || got == nil {
		t.Fatalf("GetOptimalVideoStream = %#v, %v", got, err)
	}
	if got.PlayMethod != PlayMethodTranscode {
		t.Fatalf("PlayMethod = %v, want Transcode", got.PlayMethod)
	}
	return got
}

type subtitleSupport struct{ extract bool }

func (s subtitleSupport) CanEncodeToAudioCodec(string) bool    { return false }
func (s subtitleSupport) CanEncodeToSubtitleCodec(string) bool { return false }

func (s subtitleSupport) CanExtractSubtitles(string) bool { return s.extract }

// Ports StreamBuilderTests.cs:626-776 @ b04614e. GetSubtitleProfile is the
// exported decision seam; these cases pin delivery precedence, output
// container/protocol gating, extraction capability, and VobSub/MKS eligibility.
func TestGetSubtitleProfileCompatibility(t *testing.T) {
	media := &MediaSourceInfo{}
	profiles := []SubtitleProfile{
		{Format: "srt", Method: SubtitleEmbed, Container: "mkv"},
		{Format: "srt", Method: SubtitleExternal},
	}
	internalSRT := &MediaStream{Type: StreamTypeSubtitle, Codec: "srt"}
	externalSRT := &MediaStream{Type: StreamTypeSubtitle, Codec: "srt", IsExternal: true, SupportsExternalStream: true}

	for _, tc := range []struct {
		name      string
		subtitle  *MediaStream
		profiles  []SubtitleProfile
		method    PlayMethod
		support   subtitleSupport
		container string
		protocol  MediaStreamProtocol
		want      SubtitleDeliveryMethod
	}{
		{"internal text embeds in mkv transcode", internalSRT, profiles, PlayMethodTranscode, subtitleSupport{}, "mkv", StreamProtocolHTTP, SubtitleEmbed},
		{"external text embeds in mkv http transcode", externalSRT, profiles, PlayMethodTranscode, subtitleSupport{}, "mkv", StreamProtocolHTTP, SubtitleEmbed},
		{"external text is not embedded in hls", externalSRT, profiles, PlayMethodTranscode, subtitleSupport{}, "mp4", StreamProtocolHLS, SubtitleExternal},
		{"internal text embeds for direct play", internalSRT, profiles, PlayMethodDirectPlay, subtitleSupport{}, "mkv", StreamProtocolHTTP, SubtitleEmbed},
		{"internal text requires extraction for external delivery", internalSRT, []SubtitleProfile{{Format: "srt", Method: SubtitleExternal}}, PlayMethodTranscode, subtitleSupport{extract: false}, "mp4", StreamProtocolHLS, SubtitleEncode},
		{"internal text externally delivered when extractable", internalSRT, []SubtitleProfile{{Format: "srt", Method: SubtitleExternal}}, PlayMethodTranscode, subtitleSupport{extract: true}, "mp4", StreamProtocolHLS, SubtitleExternal},
		{"internal graphical subtitle externally delivered when extractable", &MediaStream{Type: StreamTypeSubtitle, Codec: "pgssub"}, []SubtitleProfile{{Format: "pgssub", Method: SubtitleExternal}}, PlayMethodTranscode, subtitleSupport{extract: true}, "mp4", StreamProtocolHLS, SubtitleExternal},
		{"external idx vobsub cannot use mks", &MediaStream{Type: StreamTypeSubtitle, Codec: "vobsub", IsExternal: true, Path: "/media/sub.idx"}, []SubtitleProfile{{Format: "vobsub", Container: "mks", Method: SubtitleExternal}}, PlayMethodTranscode, subtitleSupport{extract: true}, "mkv", StreamProtocolHTTP, SubtitleEncode},
		{"external mks vobsub uses mks profile", &MediaStream{Type: StreamTypeSubtitle, Codec: "vobsub", IsExternal: true, Path: "/media/sub.mks"}, []SubtitleProfile{{Format: "vobsub", Container: "mks", Method: SubtitleExternal}}, PlayMethodTranscode, subtitleSupport{extract: true}, "mkv", StreamProtocolHTTP, SubtitleExternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := GetSubtitleProfile(media, tc.subtitle, tc.profiles, tc.method, tc.support, tc.container, tc.protocol)
			if got.Method != tc.want {
				t.Errorf("delivery = %v, want %v", got.Method, tc.want)
			}
		})
	}
}

// StreamBuilder.cs:549-589 first ranks by Score, then optimizes tied streams
// with the VobSub-in-MKS predicate. That predicate intentionally ignores
// profile language; an external .idx is not equivalent to deliverable .mks.
func TestStreamBuilderDefaultSubtitleTieUsesVobSubMksPredicate(t *testing.T) {
	options, source := streamBuilderOptions(t, "Chrome", "mp4-h264-aac-vtt-2600k")
	score := 6421
	defaultIndex := 40
	source.DefaultSubtitleStreamIndex = &defaultIndex
	source.MediaStreams = append(source.MediaStreams,
		MediaStream{Index: 40, Type: StreamTypeSubtitle, Codec: "vobsub", IsExternal: true, Path: "/media/sub.idx", Score: &score},
		MediaStream{Index: 41, Type: StreamTypeSubtitle, Codec: "srt", Language: testPtr("eng"), Score: &score},
	)
	options.MediaSources = []MediaSourceInfo{source}
	options.SubtitleStreamIndex = nil
	options.Profile.SubtitleProfiles = []SubtitleProfile{
		{Format: "vobsub", Container: "mks", Method: SubtitleExternal},
		{Format: "srt", Language: "fra", Method: SubtitleExternal},
	}

	got, err := NewStreamBuilder(noTranscoderSupport{}).GetOptimalVideoStream(&options)
	if err != nil {
		t.Fatalf("GetOptimalVideoStream: %v", err)
	}
	if got == nil || got.SubtitleStreamIndex == nil {
		t.Fatalf("default subtitle decision = %#v, want stream 41", got)
	}
	if want := 41; *got.SubtitleStreamIndex != want {
		t.Errorf("default SubtitleStreamIndex = %d, want %d", *got.SubtitleStreamIndex, want)
	}
}

func assertGoldenDecision(t *testing.T, name string, want streamBuilderGoldenStreamData, got *StreamInfo, err error) {
	t.Helper()
	if want.Error != "" {
		if err == nil {
			t.Fatalf("%s: expected %s, got nil error", name, want.Error)
		}
		if got != nil {
			t.Fatalf("%s: expected no StreamInfo with %s, got %+v", name, want.Error, got)
		}
		return
	}
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", name, err)
	}
	if got == nil {
		t.Fatalf("%s: got nil StreamInfo", name)
	}
	assertGoldenStreamInfo(t, got, *want.StreamInfo)
	assertGoldenFullStreamInfo(t, got, want.StreamInfoRaw)
}

func assertGoldenStreamInfo(t *testing.T, got *StreamInfo, want streamBuilderGoldenDecision) {
	t.Helper()
	if got.PlayMethod != parsePlayMethod(t, want.PlayMethod) {
		t.Errorf("PlayMethod = %v, want %s", got.PlayMethod, want.PlayMethod)
	}
	if got.TranscodeReasons != parseReasons(t, want.TranscodeReasons) {
		t.Errorf("TranscodeReasons = %v, want %v", got.TranscodeReasons, want.TranscodeReasons)
	}
	if !equalStringPointers(got.Container, want.Container) {
		t.Errorf("Container = %v, want %v", got.Container, want.Container)
	}
	if got.SubProtocol != parseStreamProtocol(t, want.SubProtocol) {
		t.Errorf("SubProtocol = %v, want %s", got.SubProtocol, want.SubProtocol)
	}
	if got.MediaSourceID() != want.MediaSourceID {
		t.Errorf("MediaSourceID = %q, want %q", got.MediaSourceID(), want.MediaSourceID)
	}
	if !equalIntPointers(got.AudioStreamIndex, want.AudioStreamIndex) {
		t.Errorf("AudioStreamIndex = %v, want %v", got.AudioStreamIndex, want.AudioStreamIndex)
	}
	if !equalIntPointers(got.SubtitleStreamIndex, want.SubtitleStreamIndex) {
		t.Errorf("SubtitleStreamIndex = %v, want %v", got.SubtitleStreamIndex, want.SubtitleStreamIndex)
	}
	if !reflect.DeepEqual(got.AudioCodecs, want.AudioCodecs) {
		t.Errorf("AudioCodecs = %q, want %q", got.AudioCodecs, want.AudioCodecs)
	}
	if !reflect.DeepEqual(got.VideoCodecs, want.VideoCodecs) {
		t.Errorf("VideoCodecs = %q, want %q", got.VideoCodecs, want.VideoCodecs)
	}
	if !reflect.DeepEqual(got.TargetVideoCodec(), want.TargetVideoCodec) {
		t.Errorf("TargetVideoCodec = %q, want %q", got.TargetVideoCodec(), want.TargetVideoCodec)
	}
	if !reflect.DeepEqual(got.TargetAudioCodec(), want.TargetAudioCodec) {
		t.Errorf("TargetAudioCodec = %q, want %q", got.TargetAudioCodec(), want.TargetAudioCodec)
	}
}

// Goldenrec persists the full pinned upstream StreamInfo object. Decode it
// through the same wire model, then compare every builder-owned field,
// including nullable state, StreamOptions, and MediaSource/MediaStreams. The
// DeviceProfile pointer is an input identity, not builder-produced state.
func assertGoldenFullStreamInfo(t *testing.T, got *StreamInfo, wantRaw json.RawMessage) {
	t.Helper()
	var want StreamInfo
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		t.Fatalf("decode golden StreamInfo: %v", err)
	}
	actual := completeStreamBuilderProjection(got)
	expected := completeStreamBuilderProjection(&want)
	if !reflect.DeepEqual(actual, expected) {
		fields := streamInfoProjectionDiffFields(actual, expected)
		t.Errorf("complete StreamInfo projection differs in %s; %s", strings.Join(fields, ","), streamInfoProjectionDifferenceDetail(actual, expected))
	}
}

func streamInfoProjectionDiffFields(actual, expected StreamInfo) []string {
	typeOf := reflect.TypeOf(actual)
	actualValue := reflect.ValueOf(actual)
	expectedValue := reflect.ValueOf(expected)
	fields := make([]string, 0, typeOf.NumField())
	for i := range typeOf.NumField() {
		field := typeOf.Field(i)
		if field.Name == "DeviceProfile" || !field.IsExported() || reflect.DeepEqual(actualValue.Field(i).Interface(), expectedValue.Field(i).Interface()) {
			continue
		}
		if field.Name == "MediaSource" {
			fields = append(fields, mediaSourceProjectionDiffFields(actual.MediaSource, expected.MediaSource)...)
			continue
		}
		fields = append(fields, field.Name)
	}
	return fields
}

func mediaSourceProjectionDiffFields(actual, expected *MediaSourceInfo) []string {
	if actual == nil || expected == nil {
		return []string{"MediaSource"}
	}
	typeOf := reflect.TypeOf(*actual)
	actualValue := reflect.ValueOf(*actual)
	expectedValue := reflect.ValueOf(*expected)
	fields := make([]string, 0, typeOf.NumField())
	for i := range typeOf.NumField() {
		field := typeOf.Field(i)
		if !field.IsExported() || reflect.DeepEqual(actualValue.Field(i).Interface(), expectedValue.Field(i).Interface()) {
			continue
		}
		if field.Name == "MediaStreams" {
			fields = append(fields, mediaStreamsProjectionDiffFields(actual.MediaStreams, expected.MediaStreams)...)
			continue
		}
		fields = append(fields, "MediaSource."+field.Name)
	}
	return fields
}

func streamInfoProjectionDifferenceDetail(actual, expected StreamInfo) string {
	actualOptions, _ := json.Marshal(actual.StreamOptions)
	expectedOptions, _ := json.Marshal(expected.StreamOptions)
	actualContainer, expectedContainer := "", ""
	actualProtocol, expectedProtocol := "", ""
	if actual.MediaSource != nil {
		actualContainer = actual.MediaSource.TranscodingContainer
		actualProtocol = strconv.Itoa(int(actual.MediaSource.TranscodingSubProtocol))
	}
	if expected.MediaSource != nil {
		expectedContainer = expected.MediaSource.TranscodingContainer
		expectedProtocol = strconv.Itoa(int(expected.MediaSource.TranscodingSubProtocol))
	}
	return "EnableAudioVbrEncoding got=" + strconv.FormatBool(actual.EnableAudioVbrEncoding) + " want=" + strconv.FormatBool(expected.EnableAudioVbrEncoding) + "; TranscodingMaxAudioChannels got=" + optionalIntString(actual.TranscodingMaxAudioChannels) + " want=" + optionalIntString(expected.TranscodingMaxAudioChannels) + "; AudioBitrate got=" + optionalIntString(actual.AudioBitrate) + " want=" + optionalIntString(expected.AudioBitrate) + "; VideoBitrate got=" + optionalIntString(actual.VideoBitrate) + " want=" + optionalIntString(expected.VideoBitrate) + "; TranscodingContainer got=" + actualContainer + " want=" + expectedContainer + "; TranscodingSubProtocol got=" + actualProtocol + " want=" + expectedProtocol + "; StreamOptions got=" + string(actualOptions) + " want=" + string(expectedOptions)
}

func optionalIntString(value *int) string {
	if value == nil {
		return "nil"
	}
	return strconv.Itoa(*value)
}

func completeStreamBuilderProjection(info *StreamInfo) StreamInfo {
	projection := *info
	projection.DeviceProfile = nil
	if projection.MediaSource == nil {
		return projection
	}
	source := *projection.MediaSource
	source.MediaStreams = append([]MediaStream(nil), source.MediaStreams...)
	for i := range source.MediaStreams {
		stream := &source.MediaStreams[i]
		stream.ReferenceFrameRateValue = nil
		stream.LocalizedUndefined = ""
		stream.LocalizedDefault = ""
		stream.LocalizedForced = ""
		stream.LocalizedExternal = ""
		stream.LocalizedHearingImpaired = ""
		stream.LocalizedLanguage = ""
		stream.LocalizedOriginal = ""
	}
	projection.MediaSource = &source
	return projection
}

func mediaStreamsProjectionDiffFields(actual, expected []MediaStream) []string {
	if len(actual) != len(expected) {
		return []string{"MediaSource.MediaStreams.length"}
	}
	streamType := reflect.TypeOf(MediaStream{})
	fields := make([]string, 0)
	for streamIndex := range len(actual) {
		actualStream := reflect.ValueOf(actual[streamIndex])
		expectedStream := reflect.ValueOf(expected[streamIndex])
		for fieldIndex := range streamType.NumField() {
			field := streamType.Field(fieldIndex)
			if !field.IsExported() || reflect.DeepEqual(actualStream.Field(fieldIndex).Interface(), expectedStream.Field(fieldIndex).Interface()) {
				continue
			}
			fields = append(fields, "MediaSource.MediaStreams["+strconv.Itoa(streamIndex)+"]."+field.Name)
		}
	}
	return fields
}

func assertFixtureDecision(t *testing.T, got *StreamInfo, want streamBuilderFixtureExpected) {
	t.Helper()
	if want.PlayMethod != nil && got.PlayMethod != parsePlayMethod(t, *want.PlayMethod) {
		t.Errorf("PlayMethod = %v, want %s", got.PlayMethod, *want.PlayMethod)
	}
	if got.TranscodeReasons != parseReasons(t, want.TranscodeReasons) {
		t.Errorf("TranscodeReasons = %v, want %v", got.TranscodeReasons, want.TranscodeReasons)
	}
	if want.Protocol != nil && (want.PlayMethod == nil || *want.PlayMethod == "Transcode") {
		if got.SubProtocol != parseFixtureProtocol(t, *want.Protocol) {
			t.Errorf("SubProtocol = %v, want %s", got.SubProtocol, *want.Protocol)
		}
	}
	assertFixtureKind(t, got, want.Kind)
}

// The upstream fixture's "kind" is not an implementation type: it describes
// the public output codec relationship. Assert that relationship instead of a
// private StreamBuilder branch.
func assertFixtureKind(t *testing.T, got *StreamInfo, want *string) {
	t.Helper()
	if want == nil || *want == "Transcode" {
		return
	}
	if len(got.TargetVideoCodec()) != 1 {
		t.Errorf("%s target video codec count = %d, want one copied codec", *want, len(got.TargetVideoCodec()))
	}
	if *want == "Remux" && len(got.TargetAudioCodec()) != 1 {
		t.Errorf("Remux target audio codec count = %d, want one copied codec", len(got.TargetAudioCodec()))
	}
}

func streamBuilderOptions(t *testing.T, profileName, sourceName string) (MediaOptions, MediaSourceInfo) {
	t.Helper()
	var profile DeviceProfile
	loadJSONFixture(t, "DeviceProfile-"+profileName+".json", &profile)
	var source MediaSourceInfo
	loadJSONFixture(t, "MediaSourceInfo-"+sourceName+".json", &source)

	options := NewMediaOptions()
	options.ItemID = "11d229b72d484b959f9b49f6ab75e613"
	options.MediaSourceID = source.ID
	options.MediaSources = []MediaSourceInfo{source}
	options.DeviceID = "goldenrec-device"
	options.Profile = &profile
	options.AllowAudioStreamCopy = true
	options.AllowVideoStreamCopy = true
	options.EnableDirectStream = false
	return options, source
}

func applyFixtureOptions(t *testing.T, options *MediaOptions, source MediaSourceInfo, fixture streamBuilderFixtureOptions) {
	t.Helper()
	options.EnableDirectStream = fixture.EnableDirectStream
	options.AllowAudioStreamCopy = fixture.AllowAudioStreamCopy
	options.AllowVideoStreamCopy = fixture.AllowVideoStreamCopy
	options.AudioStreamIndex = fixtureIndex(t, fixture.AudioStreamIndex, len(source.MediaStreams))
	options.SubtitleStreamIndex = fixtureIndex(t, fixture.SubtitleStreamIndex, len(source.MediaStreams))
}

func fixtureIndex(t *testing.T, raw json.RawMessage, streamCount int) *int {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		t.Fatalf("invalid fixture stream index %s: %v", raw, err)
	}
	var value int
	switch text {
	case "last":
		value = streamCount - 1
	case "penultimate":
		value = streamCount - 2
	default:
		parsed, err := strconv.Atoi(text)
		if err != nil {
			t.Fatalf("invalid fixture stream index %q: %v", text, err)
		}
		value = parsed
	}
	return &value
}

func loadJSONFixture(t *testing.T, name string, target any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

func parsePlayMethod(t *testing.T, value string) PlayMethod {
	t.Helper()
	switch value {
	case "DirectPlay":
		return PlayMethodDirectPlay
	case "DirectStream":
		return PlayMethodDirectStream
	case "Transcode":
		return PlayMethodTranscode
	default:
		t.Fatalf("unknown play method %q", value)
		return 0
	}
}

func parseStreamProtocol(t *testing.T, value string) MediaStreamProtocol {
	t.Helper()
	switch strings.ToLower(value) {
	case "http":
		return StreamProtocolHTTP
	case "hls":
		return StreamProtocolHLS
	default:
		t.Fatalf("unknown stream protocol %q", value)
		return 0
	}
}

func parseFixtureProtocol(t *testing.T, value string) MediaStreamProtocol {
	t.Helper()
	if strings.EqualFold(value, "http") {
		return StreamProtocolHTTP
	}
	if strings.HasPrefix(strings.ToLower(value), "hls.") {
		return StreamProtocolHLS
	}
	t.Fatalf("unknown fixture protocol %q", value)
	return 0
}

func parseReasons(t *testing.T, names []string) TranscodeReason {
	t.Helper()
	known := map[string]TranscodeReason{
		"ContainerNotSupported":        ReasonContainerNotSupported,
		"VideoCodecNotSupported":       ReasonVideoCodecNotSupported,
		"AudioCodecNotSupported":       ReasonAudioCodecNotSupported,
		"SubtitleCodecNotSupported":    ReasonSubtitleCodecNotSupported,
		"AudioIsExternal":              ReasonAudioIsExternal,
		"SecondaryAudioNotSupported":   ReasonSecondaryAudioNotSupported,
		"VideoProfileNotSupported":     ReasonVideoProfileNotSupported,
		"VideoRangeTypeNotSupported":   ReasonVideoRangeTypeNotSupported,
		"VideoCodecTagNotSupported":    ReasonVideoCodecTagNotSupported,
		"VideoLevelNotSupported":       ReasonVideoLevelNotSupported,
		"VideoResolutionNotSupported":  ReasonVideoResolutionNotSupported,
		"VideoBitDepthNotSupported":    ReasonVideoBitDepthNotSupported,
		"VideoFramerateNotSupported":   ReasonVideoFramerateNotSupported,
		"VideoRotationNotSupported":    ReasonVideoRotationNotSupported,
		"RefFramesNotSupported":        ReasonRefFramesNotSupported,
		"AnamorphicVideoNotSupported":  ReasonAnamorphicVideoNotSupported,
		"InterlacedVideoNotSupported":  ReasonInterlacedVideoNotSupported,
		"AudioChannelsNotSupported":    ReasonAudioChannelsNotSupported,
		"AudioProfileNotSupported":     ReasonAudioProfileNotSupported,
		"AudioSampleRateNotSupported":  ReasonAudioSampleRateNotSupported,
		"AudioBitDepthNotSupported":    ReasonAudioBitDepthNotSupported,
		"ContainerBitrateExceedsLimit": ReasonContainerBitrateExceedsLimit,
		"VideoBitrateNotSupported":     ReasonVideoBitrateNotSupported,
		"AudioBitrateNotSupported":     ReasonAudioBitrateNotSupported,
		"UnknownVideoStreamInfo":       ReasonUnknownVideoStreamInfo,
		"UnknownAudioStreamInfo":       ReasonUnknownAudioStreamInfo,
		"DirectPlayError":              ReasonDirectPlayError,
		"StreamCountExceedsLimit":      ReasonStreamCountExceedsLimit,
	}
	var result TranscodeReason
	for _, name := range names {
		reason, ok := known[name]
		if !ok {
			t.Fatalf("unknown transcode reason %q", name)
		}
		result |= reason
	}
	return result
}

func equalIntPointers(got, want *int) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

func equalStringPointers(got, want *string) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

// TestStreamBuilderCopyAudioSubtractsSourceBitrateFromTotal asserts that a
// copy-eligible audio stream subtracts its own bitrate (not a default) from
// the total. The measurement on the pinned stock image (12.0-rc2) confirmed
// the upstream StreamBuilder.cs:1118-1133 rule: it subtracts
// playlistItem.AudioBitrate from the total unconditionally, and
// GetAudioBitrate returns the source stream's own bitrate when the source
// codec matches the target codec.
func TestStreamBuilderCopyAudioSubtractsSourceBitrateFromTotal(t *testing.T) {
	one := 1
	sourceAudioBitrate := 96_000
	totalBitrate := 420_000
	o := NewMediaOptions()
	o.DeviceID, o.MediaSourceID = "copy-bitrate", "copy-bitrate"
	o.AllowAudioStreamCopy, o.AllowVideoStreamCopy, o.EnableDirectStream = true, true, false
	o.MaxBitrate = testPtr(totalBitrate)
	o.MediaSources = []MediaSourceInfo{{
		ID: "copy-bitrate", Container: "mp4", Protocol: MediaProtocolFile,
		SupportsTranscoding: true, SupportsDirectPlay: true, SupportsDirectStream: true,
		MediaStreams: []MediaStream{
			{Index: 0, Type: StreamTypeVideo, Codec: "h264", Width: testPtr(1920), Height: testPtr(1080), BitRate: testPtr(2_000_000)},
			{Index: 1, Type: StreamTypeAudio, Codec: "aac", Channels: testPtr(2), BitRate: testPtr(sourceAudioBitrate), SampleRate: testPtr(48_000), IsDefault: true},
		},
		DefaultAudioStreamIndex: &one,
	}}
	o.Profile = &DeviceProfile{
		MaxStreamingBitrate: testPtr(totalBitrate),
		TranscodingProfiles: []TranscodingProfile{{
			Type: DlnaVideo, Context: ContextStreaming, Container: "ts",
			Protocol: StreamProtocolHLS, VideoCodec: "h264", AudioCodec: "aac",
		}},
	}

	got, err := NewStreamBuilder(audioEncodingSupport{}).GetOptimalVideoStream(&o)
	if err != nil || got == nil {
		t.Fatalf("GetOptimalVideoStream = %#v, %v", got, err)
	}
	if got.PlayMethod != PlayMethodTranscode {
		t.Fatalf("PlayMethod = %v, want Transcode", got.PlayMethod)
	}
	// The audio bitrate must be the source stream's own bitrate (96000),
	// not a default such as 192000 or 128000.
	if got.AudioBitrate == nil || *got.AudioBitrate != sourceAudioBitrate {
		t.Errorf("AudioBitrate = %v, want source stream bitrate %d (not a default)", got.AudioBitrate, sourceAudioBitrate)
	}
	// The video bitrate must equal total minus the source audio bitrate.
	wantVideoBitrate := totalBitrate - sourceAudioBitrate
	if got.VideoBitrate == nil || *got.VideoBitrate != wantVideoBitrate {
		t.Errorf("VideoBitrate = %v, want %d (total %d - audio %d)", got.VideoBitrate, wantVideoBitrate, totalBitrate, sourceAudioBitrate)
	}
}

// A source with UseMostCompatibleTranscodingProfile (an M3U tuner without
// AllowFmp4TranscodingContainer) transcodes with a ts profile when the client
// sends one, and with the client's other profiles when it sends none (D-89).
func TestStreamBuilderMostCompatibleFallsBackWithoutTS(t *testing.T) {
	mp4 := TranscodingProfile{Container: "mp4", Type: DlnaVideo, VideoCodec: "h264", AudioCodec: "aac", Protocol: StreamProtocolHLS, Context: ContextStreaming}
	ts := mp4
	ts.Container = "ts"
	for _, tc := range []struct {
		name     string
		profiles []TranscodingProfile
		want     string
	}{
		{"fmp4 only", []TranscodingProfile{mp4}, "mp4"},
		{"ts offered", []TranscodingProfile{mp4, ts}, "ts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options, source := streamBuilderOptions(t, "Chrome", "mp4-h264-aac-vtt-2600k")
			source.UseMostCompatibleTranscodingProfile = true
			options.MediaSources = []MediaSourceInfo{source}
			options.EnableDirectPlay, options.EnableDirectStream = false, false
			options.Profile.TranscodingProfiles = tc.profiles
			got, err := NewStreamBuilder(noTranscoderSupport{}).GetOptimalVideoStream(&options)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.Container == nil || *got.Container != tc.want {
				t.Fatalf("container = %v, want %s", got, tc.want)
			}
		})
	}
}
