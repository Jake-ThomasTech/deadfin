// SPDX-License-Identifier: GPL-2.0-only

package playback_test

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
	"github.com/deadfin-org/deadfin/internal/playback"
)

// fakeFleet is the transcode-worker hub as the HLS codec choice sees it.
type fakeFleet struct {
	mu       sync.Mutex
	remote   bool
	encoders map[string]ffmpeg.RemoteEncoder
	asked    []ffmpeg.JobFacts
	paths    []string
}

func (fleet *fakeFleet) RemotePlacement() bool {
	fleet.mu.Lock()
	defer fleet.mu.Unlock()
	return fleet.remote
}

func (fleet *fakeFleet) RemoteVideoEncoder(facts ffmpeg.JobFacts, sourcePath string, _ encoding.HardwareOptions) (ffmpeg.RemoteEncoder, bool) {
	fleet.mu.Lock()
	defer fleet.mu.Unlock()
	fleet.asked = append(fleet.asked, facts)
	fleet.paths = append(fleet.paths, sourcePath)
	encoder, ok := fleet.encoders[facts.TargetCodec]
	return encoder, ok
}

func (fleet *fakeFleet) remove(codec string) {
	fleet.mu.Lock()
	defer fleet.mu.Unlock()
	delete(fleet.encoders, codec)
}

// adaWorker is worker A of #854: a GPU whose ffmpeg encodes AV1.
func adaWorker() ffmpeg.RemoteEncoder {
	return ffmpeg.RemoteEncoder{WorkerID: "a", Capabilities: nvencServiceCapabilities("av1_nvenc", "h264_nvenc", "hevc_nvenc"), Host: encoding.HostHardware{Type: encoding.HardwareAccelerationNVENC}}
}

func fleetHardware(allowAv1 bool) func() encoding.HardwareOptions {
	return func() encoding.HardwareOptions {
		return encoding.HardwareOptions{
			Type: encoding.HardwareAccelerationNVENC, EnableHardwareEncoding: true,
			HardwareDecodingCodecs: []string{"h264", "hevc"}, EnableEnhancedNvdecDecoder: true,
			AllowHevcEncoding: true, AllowAv1Encoding: allowAv1,
		}
	}
}

// fleetService is a server without an AV1 encoder (Ampere) and an HEVC
// source. fleet nil configures no transcode workers.
func fleetService(fleet playback.HLSRemoteEncoders, allowAv1 bool) (*playback.HLSService, *hlsFakeManager) {
	source := progressiveCopySource("/media/anime.mkv")
	profile := "Main"
	source.MediaStreams[0].Codec, source.MediaStreams[0].Profile = "hevc", &profile
	manager := &hlsFakeManager{playlist: "#EXTM3U\n"}
	config := hlsServiceConfig(&progressiveFakeSource{source: source}, manager)
	config.Capabilities = nvencServiceCapabilities("h264_nvenc", "hevc_nvenc")
	config.Hardware = fleetHardware(allowAv1)
	if fleet != nil {
		config.RemoteEncoders = fleet
	}
	return playback.NewHLSService(config), manager
}

func fleetRequest(session string, codecs ...string) playback.HLSRequest {
	request := hlsTranscodeRequest()
	request.SegmentContainer = "mp4"
	request.PlaySessionID = session
	request.VideoCodec = strings.Join(codecs, ",")
	request.VideoCodecs = codecs
	return request
}

// startArgs returns the master playlist codecs and the job request that a
// live start of request sends to the manager.
func startArgs(t *testing.T, service *playback.HLSService, manager *hlsFakeManager, request playback.HLSRequest) (string, playback.HLSJobRequest) {
	t.Helper()
	response, err := service.Master(t.Context(), request)
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}
	playlist := readHLSResponse(t, response)
	request.Live = true
	request.MinSegments = 1
	if _, err := service.Live(t.Context(), request); err != nil {
		t.Fatalf("Live(): %v", err)
	}
	return playlist, manager.lastStart
}

// TestHLSServiceChoosesTheOutputCodecFromTheFleet: acceptance 3 and 4 of
// #854. The server has no AV1 encoder and worker A has one: the HEVC
// source gets AV1, remote-only, with the argv built for A. With A gone,
// the same play session keeps AV1 and waits for a worker; a new play
// session gets HEVC from the server.
func TestHLSServiceChoosesTheOutputCodecFromTheFleet(t *testing.T) {
	fleet := &fakeFleet{remote: true, encoders: map[string]ffmpeg.RemoteEncoder{"av1": adaWorker()}}
	service, manager := fleetService(fleet, true)
	playlist, job := startArgs(t, service, manager, fleetRequest("session-1", "av1", "hevc", "h264"))
	if !strings.Contains(playlist, `CODECS="av01.0.15M.08,mp4a`) {
		t.Fatalf("Master() playlist = %q, want AV1", playlist)
	}
	if args := strings.Join(job.Args, " "); !job.RemoteOnly || !strings.Contains(args, "-codec:v:0 av1_nvenc ") {
		t.Fatalf("job remote-only %v, args %q; want a remote-only av1_nvenc job", job.RemoteOnly, args)
	}
	if job.BuildArgs == nil {
		t.Fatal("the remote-only job has no worker argv builder")
	}
	if len(fleet.asked) == 0 || fleet.asked[0].TargetCodec != "av1" || fleet.asked[0].SourceCodec != "hevc" || fleet.paths[0] != "/media/anime.mkv" {
		t.Fatalf("fleet asked %+v for %q", fleet.asked, fleet.paths)
	}

	fleet.remove("av1")
	_, job = startArgs(t, service, manager, fleetRequest("session-1", "av1", "hevc", "h264"))
	if !job.RemoteOnly || !strings.Contains(strings.Join(job.Args, " "), "av1_nvenc") {
		t.Fatalf("pinned session lost its codec: remote-only %v, args %q", job.RemoteOnly, job.Args)
	}
	playlist, job = startArgs(t, service, manager, fleetRequest("session-2", "av1", "hevc", "h264"))
	if !strings.Contains(playlist, `CODECS="hvc1.`) {
		t.Fatalf("new session playlist = %q, want HEVC", playlist)
	}
	if args := strings.Join(job.Args, " "); job.RemoteOnly || !strings.Contains(args, "-codec:v:0 hevc_nvenc ") {
		t.Fatalf("new session remote-only %v, args %q; want a server hevc_nvenc job", job.RemoteOnly, args)
	}
}

// TestHLSServiceFleetCodecKeepsTheAllowGates: with AllowAv1Encoding off,
// AV1 moves behind HEVC, which the server encodes.
func TestHLSServiceFleetCodecKeepsTheAllowGates(t *testing.T) {
	fleet := &fakeFleet{remote: true, encoders: map[string]ffmpeg.RemoteEncoder{"av1": adaWorker()}}
	service, manager := fleetService(fleet, false)
	_, job := startArgs(t, service, manager, fleetRequest("session-1", "av1", "hevc", "h264"))
	if job.RemoteOnly || !strings.Contains(strings.Join(job.Args, " "), "hevc_nvenc") {
		t.Fatalf("remote-only %v, args %q; want server HEVC", job.RemoteOnly, job.Args)
	}
	if len(fleet.asked) != 0 {
		t.Fatalf("fleet asked %+v before a codec the server encodes", fleet.asked)
	}
}

// TestHLSServiceAsksTheFleetOnlyForACodecTheServerCannot: a codec that
// the server encodes stays on the server.
func TestHLSServiceAsksTheFleetOnlyForACodecTheServerCannot(t *testing.T) {
	fleet := &fakeFleet{remote: true, encoders: map[string]ffmpeg.RemoteEncoder{"av1": adaWorker(), "hevc": adaWorker()}}
	service, manager := fleetService(fleet, true)
	_, job := startArgs(t, service, manager, fleetRequest("session-1", "hevc", "av1"))
	if job.RemoteOnly || !strings.Contains(strings.Join(job.Args, " "), "hevc_nvenc") {
		t.Fatalf("remote-only %v, args %q; want server HEVC", job.RemoteOnly, job.Args)
	}
	if len(fleet.asked) != 0 {
		t.Fatalf("fleet asked %+v", fleet.asked)
	}
}

// TestHLSServiceLocalModeKeepsTheServerCodecChoice: acceptance 5 of #854.
// In local mode the fleet is not asked and the argv is the one of a server
// without workers.
func TestHLSServiceLocalModeKeepsTheServerCodecChoice(t *testing.T) {
	for _, codecs := range [][]string{{"av1", "hevc", "h264"}, {"h264"}, {"av1", "h264"}} {
		fleet := &fakeFleet{encoders: map[string]ffmpeg.RemoteEncoder{"av1": adaWorker()}}
		withFleet, withManager := fleetService(fleet, true)
		without, withoutManager := fleetService(nil, true)
		gotPlaylist, got := startArgs(t, withFleet, withManager, fleetRequest("session-1", codecs...))
		wantPlaylist, want := startArgs(t, without, withoutManager, fleetRequest("session-1", codecs...))
		if gotPlaylist != wantPlaylist || !slices.Equal(got.Args, want.Args) || !slices.Equal(got.IdentityArgs, want.IdentityArgs) || got.RemoteOnly || got.PlaylistPath != want.PlaylistPath {
			t.Fatalf("%v: local mode with workers differs:\n%q\n%q", codecs, got.Args, want.Args)
		}
		if len(fleet.asked) != 0 {
			t.Fatalf("%v: fleet asked %+v in local mode", codecs, fleet.asked)
		}
	}
}

// TestHLSServiceFleetEncodesWhatAServerWithoutAGPUCannot: workers-only
// with a GPU-less server: an HEVC-only client gets HEVC from a worker
// instead of an error.
func TestHLSServiceFleetEncodesWhatAServerWithoutAGPUCannot(t *testing.T) {
	request := fleetRequest("session-1", "hevc")
	source := progressiveCopySource("/media/anime.mkv")
	manager := &hlsFakeManager{playlist: "#EXTM3U\n"}
	config := hlsServiceConfig(&progressiveFakeSource{source: source}, manager)
	config.Capabilities = encoding.Capabilities{H264Encoder: "libx264", EncoderMajorVersion: 8, EncoderMinorVersion: 1, AACEncoders: []string{"aac"}, Encoders: []string{"libx264"}}
	config.Hardware = func() encoding.HardwareOptions {
		return encoding.HardwareOptions{Type: encoding.HardwareAccelerationNone, EnableHardwareEncoding: true, AllowHevcEncoding: true}
	}
	if _, err := playback.NewHLSService(config).Master(t.Context(), request); !errors.Is(err, playback.ErrProgressiveUnsupported) {
		t.Fatalf("Master() without workers = %v, want ErrProgressiveUnsupported", err)
	}
	config.RemoteEncoders = &fakeFleet{remote: true, encoders: map[string]ffmpeg.RemoteEncoder{"hevc": adaWorker()}}
	_, job := startArgs(t, playback.NewHLSService(config), manager, request)
	if !job.RemoteOnly || !strings.Contains(strings.Join(job.Args, " "), "hevc_nvenc") {
		t.Fatalf("remote-only %v, args %q; want a remote-only hevc_nvenc job", job.RemoteOnly, job.Args)
	}
}
