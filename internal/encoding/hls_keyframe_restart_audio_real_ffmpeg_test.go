// SPDX-License-Identifier: GPL-2.0-only

package encoding

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestHLSKeyframeCutRestartAudioMatchesContinuousJobWithRealFFmpeg reproduces
// deadfin#799: a remux with an audio encode, restarted mid-file at a GOP. The
// first segment of the restarted job must carry audio from the keyframe like
// the job that played through. With accurate seek the restarted audio
// started 0.5 s late, at the seek point inside the GOP, once per restart.
func TestHLSKeyframeCutRestartAudioMatchesContinuousJobWithRealFFmpeg(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("requires ffmpeg: %v", err)
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skipf("requires ffprobe: %v", err)
	}
	encoders, err := exec.CommandContext(t.Context(), ffmpegPath, "-hide_banner", "-encoders").Output()
	if err != nil || !bytes.Contains(encoders, []byte(" libx264 ")) {
		t.Skipf("requires ffmpeg with libx264: %v", err)
	}
	source := filepath.Join(t.TempDir(), "gop.mkv")
	// 5.5 s GOPs, longer than the 3 s segments, as in the production source.
	runFFmpeg(t, ffmpegPath,
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24:duration=40",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=40",
		"-map", "0", "-map", "1", "-c:v", "libx264", "-preset", "ultrafast",
		"-x264-params", "keyint=600:min-keyint=1:scenecut=0", "-force_key_frames", "expr:gte(t,n_forced*5.5)",
		"-c:a", "ac3", "-ac", "6", "-b:a", "384k", "-y", source)

	const restartGOP = 4
	gopStart := int64(restartGOP) * 55_000_000
	run := func(start, seek int64, number int) string {
		dir := t.TempDir()
		job := hlsUnitJob()
		job.InputPath = source
		job.InputContainer = "matroska,webm"
		job.OutputPath = filepath.Join(dir, "main.m3u8")
		job.OutputContainer = "fmp4"
		job.OutputVideoCodec = "copy"
		job.VideoStream = VideoStream{Index: 0, Codec: "h264", NalLengthSize: "4"}
		job.SubtitleStream = nil
		job.AudioStream = AudioStream{Index: 1, Codec: "ac3", Channels: 6}
		job.OutputAudioCodec = "aac"
		job.OutputAudioBitrate = 192_000
		job.OutputAudioChannels = 2
		job.OutputAudioSampleRate = 48_000
		job.KeyframeCuts = true
		job.KeyframeStartTicks = start
		job.StartOffsetTicks = seek
		job.StartNumber = number
		// The native AAC encoder runs on every ffmpeg build.
		caps := progressiveTestCapabilities()
		caps.AACEncoders = []string{"aac"}
		args, err := BuildHLSArgs(job, caps)
		if err != nil {
			t.Fatal(err)
		}
		runFFmpeg(t, ffmpegPath, args...)
		return dir
	}
	continuous := run(0, 0, 0)
	restarted := run(gopStart, gopStart+5_000_000, restartGOP)

	for _, gop := range []int{restartGOP, restartGOP + 1} {
		want := firstAudioSeconds(t, ffprobePath, continuous, gop)
		got := firstAudioSeconds(t, ffprobePath, restarted, gop)
		if math.Abs(got-want) > 0.04 {
			t.Errorf("GOP %d first audio after restart = %.4f s, continuous job = %.4f s", gop, got, want)
		}
	}
}

// firstAudioSeconds joins the init segment and segment file gop and returns
// the first audio presentation time.
func firstAudioSeconds(t *testing.T, ffprobePath, dir string, gop int) float64 {
	t.Helper()
	body := readTestFile(t, filepath.Join(dir, "main-1.mp4"))
	body = append(body, readTestFile(t, filepath.Join(dir, fmt.Sprintf("main%d.mp4", gop)))...)
	joined := filepath.Join(dir, "joined.mp4")
	if err := os.WriteFile(joined, body, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(t.Context(), ffprobePath, "-v", "error", "-select_streams", "a",
		"-show_entries", "packet=pts_time", "-read_intervals", "%+#1", "-of", "csv=p=0", joined).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	value, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(string(output)), ","), 64)
	if err != nil {
		t.Fatalf("no audio packet in GOP %d (%q): %v", gop, output, err)
	}
	return value
}
