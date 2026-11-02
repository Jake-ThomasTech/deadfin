// SPDX-License-Identifier: GPL-2.0-only

package trickplay_test

import (
	"context"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/images"
	"github.com/deadfin-org/deadfin/internal/trickplay"
)

// TestGeneratorRealFFmpegVipsSynthesizedColorSmoke proves that the concrete
// software-only ffmpeg extractor and libvips TileComposer interoperate through
// the public Generator boundary. It deliberately synthesizes its input locally:
// no checked-in media, network access, hardware codec, or timing race is needed.
func TestGeneratorRealFFmpegVipsSynthesizedColorSmoke(t *testing.T) {
	ffmpegPath, vipsPath := requireTrickplayIntegrationTools(t)
	fixture := synthesizeTrickplayColorFixture(t, ffmpegPath)

	runtimeOwner, err := images.OpenRuntime(t.Context())
	if err != nil {
		t.Fatalf("OpenRuntime(): %v", err)
	}
	t.Cleanup(func() {
		if err := runtimeOwner.Close(); err != nil {
			t.Errorf("Runtime.Close(): %v", err)
		}
	})

	root := t.TempDir()
	composer, err := images.NewTileComposer(runtimeOwner, images.TileComposerConfig{Root: root, MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("NewTileComposer(): %v", err)
	}
	extractor, err := trickplay.NewFFmpegFrameExtractor(ffmpegPath, nil)
	if err != nil {
		t.Fatalf("NewFFmpegFrameExtractor(%q): %v", ffmpegPath, err)
	}
	itemID := generatorUUID(t, "102132435465768798a9babcbddcedfe")
	store := &generatorStore{}
	generator := newGenerator(t, root, extractor, composer, store)

	metadata, err := generator.Generate(t.Context(), trickplay.GenerateRequest{
		ItemID: itemID, Source: openPinnedGeneratorSource(t, fixture), SourceRevision: "test-source-revision", VideoWidth: generatorInt(64),
		RunTimeTicks: generatorInt64(20_000_000), RequestedWidth: 64,
		TileWidth: 2, TileHeight: 1, IntervalMilliseconds: 1_000,
	})
	if err != nil {
		t.Fatalf("Generate(real ffmpeg/libvips): %v", err)
	}
	if metadata.Width != 64 || metadata.Height != 36 || metadata.TileWidth != 2 || metadata.TileHeight != 1 || metadata.Interval != 1_000 {
		t.Fatalf("real generation metadata = %#v, want 64x36 2x1 at 1000ms", metadata)
	}

	resolution := generatorResolutionDirectory(root, itemID, 64, 2, 1)
	entries := generatorDirectoryNames(t, resolution)
	if len(entries) == 0 || entries[0] != "0.jpg" {
		t.Fatalf("real generation tiles = %#v, want a zero-based JPEG tile", entries)
	}
	file, err := os.Open(filepath.Join(resolution, "0.jpg"))
	if err != nil {
		t.Fatalf("Open(real generated tile): %v", err)
	}
	decoded, err := jpeg.Decode(file)
	closeErr := file.Close()
	if err != nil {
		if closeErr != nil {
			t.Errorf("Close(real generated tile) after decode failure: %v", closeErr)
		}
		t.Fatalf("Decode(real generated tile): %v", err)
	}
	if closeErr != nil {
		t.Fatalf("Close(real generated tile): %v", closeErr)
	}
	if got, want := decoded.Bounds().Dx(), 128; got != want {
		t.Fatalf("real generated tile width = %d, want %d", got, want)
	}
	if got, want := decoded.Bounds().Dy(), 36; got != want {
		t.Fatalf("real generated tile height = %d, want %d", got, want)
	}
	assertTrickplayIntegrationColor(t, color.NRGBAModel.Convert(decoded.At(32, 18)).(color.NRGBA), color.NRGBA{R: 0xff, A: 0xff})
	if calls := store.recorded(); len(calls) != 1 || calls[0] != metadata {
		t.Fatalf("real generation metadata store calls = %#v, want committed generated metadata %#v", calls, metadata)
	}
	_ = vipsPath // The explicit probe above makes the native capability prerequisite visible in skip output.
}

func requireTrickplayIntegrationTools(t *testing.T) (string, string) {
	t.Helper()
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("real trickplay integration requires ffmpeg: %v", err)
	}
	vipsPath, err := exec.LookPath("vips")
	if err != nil {
		t.Skipf("real trickplay integration requires libvips CLI: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, vipsPath, "--version").Output()
	if err != nil {
		t.Skipf("real trickplay integration requires a runnable libvips CLI %q: %v", vipsPath, err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(version)), "vips-8.") {
		t.Skipf("real trickplay integration requires libvips 8.x, found %q", strings.TrimSpace(string(version)))
	}
	encoders, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-encoders").Output()
	if err != nil {
		t.Skipf("real trickplay integration cannot inspect ffmpeg encoder support at %q: %v", ffmpegPath, err)
	}
	if !trickplayFFmpegAdvertisesEncoder(string(encoders), "mpeg4") {
		t.Skipf("real trickplay integration requires software ffmpeg mpeg4 encoder at %q", ffmpegPath)
	}
	return ffmpegPath, vipsPath
}

func synthesizeTrickplayColorFixture(t *testing.T, ffmpegPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "solid-red.mp4")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=red:s=64x36:r=1:d=2",
		"-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", path,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("synthesize red ffmpeg fixture: %v\n%s", err, output)
	}
	return path
}

func trickplayFFmpegAdvertisesEncoder(encoders, required string) bool {
	for _, line := range strings.Split(encoders, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == required {
			return true
		}
	}
	return false
}

func assertTrickplayIntegrationColor(t *testing.T, got, want color.NRGBA) {
	t.Helper()
	const tolerance = 20
	if absTrickplayDifference(got.R, want.R) > tolerance || absTrickplayDifference(got.G, want.G) > tolerance || absTrickplayDifference(got.B, want.B) > tolerance || got.A != want.A {
		t.Fatalf("real generated pixel = %#v, want near %#v", got, want)
	}
}

func absTrickplayDifference(left, right uint8) uint8 {
	if left >= right {
		return left - right
	}
	return right - left
}
