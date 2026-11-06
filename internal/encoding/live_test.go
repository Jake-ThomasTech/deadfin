// SPDX-License-Identifier: GPL-2.0-only

package encoding

import (
	"strings"
	"testing"
)

var liveCapabilities = Capabilities{H264Encoder: "libx264", AACEncoders: []string{"aac"}}

func liveHLSJob(t *testing.T) HLSJob {
	t.Helper()
	job := hlsJobFromGolden(t, progressiveGoldenCase{
		Fixture:          "mp4-h264-ac3-aac-srt-2600k",
		InputCodec:       "h264",
		Subtitle:         "none",
		ToneMap:          "sdr",
		Audio:            "copy",
		Container:        "ts",
		Seek:             "start",
		NalLengthSize:    "0",
		AudioCodec:       "ac3",
		AudioStreamIndex: 1,
		AudioChannels:    6,
	})
	job.InputPath = ""
	job.InputContainer = "mpegts"
	job.LiveInput = &LiveInput{URL: "http://127.0.0.1:8096/LiveTv/LiveStreamFiles/abc/stream.ts", AnalyzeDurationMs: 3000}
	return job
}

func TestBuildHLSArgsLiveInput(t *testing.T) {
	for _, test := range []struct {
		name  string
		edit  func(*HLSJob)
		input string
	}{
		{
			name:  "broker stream",
			input: "-analyzeduration 3000000 -i http://127.0.0.1:8096/LiveTv/LiveStreamFiles/abc/stream.ts",
		},
		{
			name: "every modifier in upstream order",
			edit: func(job *HLSJob) {
				job.LiveInput.UserAgent = "Deadfin/test"
				job.LiveInput.ReadAtNativeFramerate = true
				job.LiveInput.IgnoreDts = true
				job.LiveInput.Loop = true
			},
			input: "-analyzeduration 3000000 -user_agent Deadfin/test -re -fflags +igndts -stream_loop -1 -reconnect_at_eof 1 -reconnect_streamed 1 -reconnect_delay_max 2 -i http://127.0.0.1:8096/LiveTv/LiveStreamFiles/abc/stream.ts",
		},
		{
			name: "video copy generates timestamps",
			edit: func(job *HLSJob) {
				job.OutputVideoCodec = "copy"
				job.LiveInput.IgnoreDts = true
			},
			input: "-analyzeduration 3000000 -fflags +igndts+genpts -i http://127.0.0.1:8096/LiveTv/LiveStreamFiles/abc/stream.ts",
		},
		{
			name: "no seek on a live input",
			edit: func(job *HLSJob) {
				job.StartOffsetTicks = 600000000
			},
			input: "-analyzeduration 3000000 -i http://127.0.0.1:8096/LiveTv/LiveStreamFiles/abc/stream.ts",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := liveHLSJob(t)
			if test.edit != nil {
				test.edit(&job)
			}
			args, err := BuildHLSArgs(job, liveCapabilities)
			if err != nil {
				t.Fatalf("BuildHLSArgs: %v", err)
			}
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, test.input) {
				t.Fatalf("argv lacks %q:\n%s", test.input, joined)
			}
			input := strings.Index(joined, " -i ")
			if strings.Contains(joined[:input], "-ss ") {
				t.Fatalf("live input seeks:\n%s", joined)
			}
			if strings.Count(joined, " -i ") != 1 {
				t.Fatalf("argv has more than one input:\n%s", joined)
			}
		})
	}
}

func TestBuildHLSArgsRejectsUnsafeLiveInput(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*LiveInput)
	}{
		{name: "file path", edit: func(input *LiveInput) { input.URL = "/media/movie.mkv" }},
		{name: "file scheme", edit: func(input *LiveInput) { input.URL = "file:///etc/passwd" }},
		{name: "option", edit: func(input *LiveInput) { input.URL = "-i" }},
		{name: "control character", edit: func(input *LiveInput) { input.URL = "http://host/a\nb" }},
		{name: "user agent control character", edit: func(input *LiveInput) { input.UserAgent = "a\r\nX-Injected: 1" }},
		{name: "negative analyze duration", edit: func(input *LiveInput) { input.AnalyzeDurationMs = -1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := liveHLSJob(t)
			test.edit(job.LiveInput)
			if _, err := BuildHLSArgs(job, liveCapabilities); err == nil {
				t.Fatal("BuildHLSArgs accepted an unsafe live input")
			}
		})
	}
}

func TestBuildLiveRemuxArgs(t *testing.T) {
	args, err := BuildLiveRemuxArgs(LiveInput{URL: "https://cdn.invalid/live/master.m3u8", UserAgent: "Deadfin/test", IgnoreDts: true, ReadAtNativeFramerate: true, Loop: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "-hide_banner -nostdin -loglevel error -user_agent Deadfin/test -fflags +igndts+genpts -i https://cdn.invalid/live/master.m3u8 -sn -dn -c copy -f mpegts pipe:1"
	if got := strings.Join(args, " "); got != want {
		t.Fatalf("argv:\n got %s\nwant %s", got, want)
	}
	for _, url := range []string{"/srv/a.m3u8", "file:///etc/passwd", "-i", "http://host/a\nb"} {
		if _, err := BuildLiveRemuxArgs(LiveInput{URL: url}); err == nil {
			t.Fatalf("accepted %q", url)
		}
	}
}
