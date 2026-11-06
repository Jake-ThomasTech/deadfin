// SPDX-License-Identifier: GPL-2.0-only

package playback

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
)

const (
	// hlsAudioStartWindow is the part of the source that the first-packet
	// probe reads.
	hlsAudioStartWindow = 3 * time.Second
	// hlsLateAudioTicks is the first-packet time from which an audio stream
	// counts as late.
	hlsLateAudioTicks = 10_000_000
	// hlsAudioStartCacheLimit bounds the remembered probe results.
	hlsAudioStartCacheLimit = 4096
)

// AudioStartProber reads the first packet time of one ffmpeg input stream
// within the first window of a pinned source.
type AudioStartProber interface {
	FirstPacketTicks(ctx context.Context, source *sourcefile.Source, streamIndex int, window time.Duration) (int64, bool, error)
}

// hlsAudioStarts remembers, per source revision and stream, whether an audio
// stream starts late. The HLS service owns it.
type hlsAudioStarts struct {
	prober AudioStartProber
	mu     sync.Mutex
	late   map[string]bool
}

// startsLate reports whether the first packet of audio is later than
// hlsLateAudioTicks, or missing from the probe window. A probe error counts
// as not late and is not remembered: the job then copies as before.
func (starts *hlsAudioStarts) startsLate(ctx context.Context, source MediaSourceInfo, audio *MediaStream) bool {
	if starts == nil || starts.prober == nil || audio == nil || audio.IsExternal {
		return false
	}
	index := ffmpegStreamIndex(source.MediaStreams, audio)
	if index < 0 {
		return false
	}
	key := source.Path + "\x00" + source.SourceRevision + "\x00" + strconv.Itoa(index)
	starts.mu.Lock()
	late, known := starts.late[key]
	starts.mu.Unlock()
	if known {
		return late
	}
	pinned, err := openPinnedSource(source)
	if err != nil {
		return false
	}
	defer func() { _ = pinned.Close() }()
	first, found, err := starts.prober.FirstPacketTicks(ctx, pinned, index, hlsAudioStartWindow)
	if err != nil {
		return false
	}
	late = !found || first > hlsLateAudioTicks
	starts.mu.Lock()
	if starts.late == nil || len(starts.late) >= hlsAudioStartCacheLimit {
		starts.late = make(map[string]bool)
	}
	starts.late[key] = late
	starts.mu.Unlock()
	return late
}

// padLateAudio makes an HLS job start its audio with silence when the source
// audio stream starts late (deadfin#681, D-82). Stream copy cannot fill the
// gap: the first fMP4 segments then carry no audio samples, and AVPlayer
// fails the item (CoreMediaErrorDomain -16172). A copy becomes an encode to
// AAC when the client accepts AAC, else to the source codec when it is AC3 or
// E-AC3. Any other copy stays a copy.
func (service *HLSService) padLateAudio(ctx context.Context, source MediaSourceInfo, audio *MediaStream, request HLSRequest, job *encoding.HLSJob) {
	if audio == nil || request.Live || request.LiveStreamID != "" || job.OutputAudioCodec == "" || !service.audioStarts.startsLate(ctx, source, audio) {
		return
	}
	if job.OutputAudioCodec == "copy" {
		codec := hlsLateAudioCodec(audio, request.ProgressiveRequest)
		if codec == "" {
			return
		}
		job.OutputAudioCodec = codec
	}
	job.PadAudioStart = true
}

func hlsLateAudioCodec(audio *MediaStream, request ProgressiveRequest) string {
	if len(request.AudioCodecs) == 0 || containsProgressiveCodec(request.AudioCodecs, "aac") {
		return "aac"
	}
	switch codec := strings.ToLower(audioCodec(audio)); codec {
	case "ac3", "eac3":
		if containsProgressiveCodec(request.AudioCodecs, codec) {
			return codec
		}
	}
	return ""
}
