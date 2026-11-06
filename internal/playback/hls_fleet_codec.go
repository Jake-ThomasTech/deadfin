// SPDX-License-Identifier: GPL-2.0-only

package playback

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
)

// Fleet codec choice (#854): with a transcode-worker placement mode other
// than local, the HLS output codec is the first requested codec that the
// server or a ready worker can encode. A codec that only a worker encodes
// makes the rendition remote-only: its argv is built for that worker, and
// the manager never runs it on the server.
//
// The choice is pinned per play session, because the master playlist names
// the codec. When the worker goes away, the pinned rendition waits for a
// worker for the queue timeout and then fails as usual; a new play session
// chooses again from the fleet as it is then.

// HLSRemoteEncoders reports the transcode workers that can encode an HLS
// output codec.
type HLSRemoteEncoders interface {
	// RemotePlacement reports a placement mode other than local.
	RemotePlacement() bool
	// RemoteVideoEncoder returns a ready worker that can encode
	// facts.TargetCodec with options, in the pools where placement sends a
	// job of facts.
	RemoteVideoEncoder(facts ffmpeg.JobFacts, sourcePath string, options encoding.HardwareOptions) (ffmpeg.RemoteEncoder, bool)
}

// hlsCodecPinTTL is how long a play session keeps its codec choice after
// its last request.
const hlsCodecPinTTL = 6 * time.Hour

// hlsCodecChoice is the output codec of one rendition and the worker that
// encodes it when the server cannot. A nil encoder is the server.
type hlsCodecChoice struct {
	codec   string
	encoder *ffmpeg.RemoteEncoder
}

// source names the host whose capabilities chose the codec, for logs.
func (choice hlsCodecChoice) source() string {
	if choice.encoder == nil {
		return "server"
	}
	return "worker:" + choice.encoder.WorkerID
}

type hlsCodecPin struct {
	choice hlsCodecChoice
	used   time.Time
}

// hlsCodecPins holds the codec choice of each play session.
type hlsCodecPins struct {
	clock   clock.Clock
	logger  *slog.Logger
	mu      sync.Mutex
	entries map[string]hlsCodecPin
}

func newHLSCodecPins(clock clock.Clock, logger *slog.Logger) *hlsCodecPins {
	return &hlsCodecPins{clock: clock, logger: logger, entries: make(map[string]hlsCodecPin)}
}

// choose returns the pinned choice of key, or pins and logs the result of
// pick. An empty key pins nothing.
func (pins *hlsCodecPins) choose(key string, pick func() hlsCodecChoice) hlsCodecChoice {
	if key == "" {
		return pick()
	}
	now := pins.clock.Now()
	pins.mu.Lock()
	if pin, ok := pins.entries[key]; ok && now.Sub(pin.used) < hlsCodecPinTTL {
		pin.used = now
		pins.entries[key] = pin
		pins.mu.Unlock()
		return pin.choice
	}
	pins.mu.Unlock()
	choice := pick()
	pins.mu.Lock()
	for other, pin := range pins.entries {
		if now.Sub(pin.used) >= hlsCodecPinTTL {
			delete(pins.entries, other)
		}
	}
	if choice.codec != "" {
		pins.entries[key] = hlsCodecPin{choice: choice, used: now}
	}
	pins.mu.Unlock()
	if choice.codec != "" {
		pins.logger.Info("HLS output codec chosen", "codec", choice.codec, "source", choice.source())
	}
	return choice
}

// hlsVideoCodec chooses the output video codec of a rendition. Without a
// remote placement it is hlsTranscodeVideoCodec on the server's
// capabilities. With one, each codec in the order of
// hlsTranscodeVideoCodec goes to the server when it can encode it, and
// otherwise to a ready worker that can (#854).
func (service *HLSService) hlsVideoCodec(source MediaSourceInfo, video, audio *MediaStream, request HLSRequest, caps encoding.Capabilities, options encoding.HardwareOptions) hlsCodecChoice {
	server := func() hlsCodecChoice {
		return hlsCodecChoice{codec: hlsTranscodeVideoCodec(request.VideoCodecs, caps, options)}
	}
	fleet := service.remoteEncoders
	// Only a file rendition that transcodes its video can go to a worker.
	if fleet == nil || request.AudioOnly || request.LiveStreamID != "" || len(request.VideoCodecs) == 0 ||
		source.Protocol != MediaProtocolFile || source.IsRemote || canCopyHLSVideo(source, video, request.ProgressiveRequest, caps) || !fleet.RemotePlacement() {
		return server()
	}
	key := ""
	if request.PlaySessionID != "" {
		key = strings.Join([]string{request.PlaySessionID, request.DeviceID, request.MediaSourceID, strings.ToLower(strings.Join(request.VideoCodecs, ","))}, "\x00")
	}
	return service.codecPins.choose(key, func() hlsCodecChoice {
		for _, codec := range hlsVideoCodecOrder(request.VideoCodecs, options) {
			if encoding.CanEncodeVideo(codec, caps, options) {
				return hlsCodecChoice{codec: codec}
			}
			job := hlsEncodingJob(source, video, audio, nil, hlsRenditionIdentityOutputPath, request, codec, caps)
			job.Hardware = options
			facts := encoding.PlacementFacts(job, caps)
			if encoder, ok := fleet.RemoteVideoEncoder(ffmpegJobFacts(facts), source.Path, options); ok {
				return hlsCodecChoice{codec: codec, encoder: &encoder}
			}
		}
		return hlsCodecChoice{}
	})
}

// argsFor returns the job and capabilities that build the server-form argv
// of a rendition: for a remote-only rendition, those of its worker.
func (choice hlsCodecChoice) argsFor(job encoding.HLSJob, caps encoding.Capabilities) (encoding.HLSJob, encoding.Capabilities) {
	if choice.encoder == nil {
		return job, caps
	}
	job.Hardware = job.Hardware.ForHost(choice.encoder.Host)
	return job, choice.encoder.Capabilities
}

func ffmpegJobFacts(facts encoding.HLSPlacementFacts) ffmpeg.JobFacts {
	return ffmpeg.JobFacts{
		SourceCodec: facts.SourceCodec, SourceProfile: facts.SourceProfile, BitDepth: facts.BitDepth,
		HDR: facts.HDR, NeedsToneMap: facts.NeedsToneMap, TargetCodec: facts.TargetCodec, Hardware: facts.Hardware,
	}
}
