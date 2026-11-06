// SPDX-License-Identifier: GPL-2.0-only

package encoding

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// LiveInput is a live network input: a Live TV broker stream or a channel
// URL. It carries the media source facts that the pinned GetInputModifier
// reads for infinite sources (EncodingHelper.cs:7289-7420).
type LiveInput struct {
	// URL is the http or https input.
	URL string
	// AnalyzeDurationMs is MediaSource.AnalyzeDurationMs; 0 omits
	// -analyzeduration.
	AnalyzeDurationMs int
	// IgnoreDts adds +igndts (MediaSource.IgnoreDts).
	IgnoreDts bool
	// ReadAtNativeFramerate adds -re.
	ReadAtNativeFramerate bool
	// UserAgent is sent with -user_agent when set.
	UserAgent string
	// Loop is MediaSource.RequiresLooping.
	Loop bool
}

func validateLiveInput(input LiveInput) error {
	parsed, err := url.Parse(input.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("live input must be an http or https URL")
	}
	for _, value := range []string{input.URL, input.UserAgent} {
		for _, r := range value {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("live input must not contain control characters")
			}
		}
	}
	if input.AnalyzeDurationMs < 0 {
		return fmt.Errorf("live analyze duration must not be negative")
	}
	return nil
}

// appendLiveInputArgs ports GetInputModifier for a live input, in its order:
// -analyzeduration, -user_agent, -re, -fflags, the loop flags; then the
// decoder arguments and the input. Live inputs never seek.
func appendLiveInputArgs(args []string, input LiveInput, copyVideo bool, decoder []string) []string {
	if input.AnalyzeDurationMs > 0 {
		args = append(args, "-analyzeduration", strconv.Itoa(input.AnalyzeDurationMs*1000))
	}
	if strings.TrimSpace(input.UserAgent) != "" {
		args = append(args, "-user_agent", input.UserAgent)
	}
	if input.ReadAtNativeFramerate {
		args = append(args, "-re")
	}
	flags := ""
	if input.IgnoreDts {
		flags += "+igndts"
	}
	if copyVideo {
		flags += "+genpts"
	}
	if flags != "" {
		args = append(args, "-fflags", flags)
	}
	if input.Loop {
		args = append(args, "-stream_loop", "-1", "-reconnect_at_eof", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "2")
	}
	args = append(args, decoder...)
	return append(args, "-i", input.URL)
}

// BuildLiveRemuxArgs builds the ffmpeg arguments that read a live HLS
// source once and copy it to MPEG-TS on stdout, for the Live TV stream
// broker. ffmpeg's default stream selection keeps the best video and
// audio rendition; subtitles and data streams are dropped because
// WebVTT and timed ID3 cannot be copied into MPEG-TS. +genpts fills
// missing timestamps across segment boundaries.
func BuildLiveRemuxArgs(input LiveInput) ([]string, error) {
	if err := validateLiveInput(input); err != nil {
		return nil, err
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error"}
	args = appendLiveInputArgs(args, LiveInput{
		URL:               input.URL,
		AnalyzeDurationMs: input.AnalyzeDurationMs,
		IgnoreDts:         input.IgnoreDts,
		UserAgent:         input.UserAgent,
	}, true, nil)
	return append(args, "-sn", "-dn", "-c", "copy", "-f", "mpegts", "pipe:1"), nil
}
