// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
)

// TestBurnInAssetsReachTheWorkerBeforeFFmpeg covers #624: the worker
// fetches the subtitle and the fonts of a burn-in into its asset directory
// before ffmpeg starts, and the argv names that directory.
func TestBurnInAssetsReachTheWorkerBeforeFFmpeg(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	worker := startTestAgent(t, hub, token, "w", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")
	assets := map[string]string{"subtitle.ass": "[Script Info]\n", "font001.ttf": "font one"}
	job := f.job("-vf", "subtitles=f='"+ffmpeg.RemoteAssetsPlaceholder+"/subtitle.ass':fontsdir='"+ffmpeg.RemoteAssetsPlaceholder+"/fonts'")
	for name, body := range assets {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		job.Assets = append(job.Assets, ffmpeg.RemoteAsset{Name: name, File: file, Size: int64(len(body))})
	}
	process := startJob(t, hub, job)
	ffmpegProcess := worker.process(t)
	assetDir := filepath.Join(ffmpegProcess.options.Dir, "assets")
	for name, body := range assets {
		path := filepath.Join(assetDir, name)
		if strings.HasPrefix(name, "font") {
			path = filepath.Join(assetDir, "fonts", name)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != body {
			t.Fatalf("asset %s on the worker = %q, %v", name, got, err)
		}
	}
	escaped := encoding.EscapeSubtitleFilterPath(assetDir)
	want := "subtitles=f='" + escaped + "/subtitle.ass':fontsdir='" + escaped + "/fonts'"
	if args := ffmpegProcess.options.Args; args[indexOf(args, "-vf")+1] != want {
		t.Fatalf("worker filter = %q, want %q", args[indexOf(args, "-vf")+1], want)
	}
	ffmpegProcess.end(nil)
	if err := waitResult(t, process); err != nil {
		t.Fatal(err)
	}
}

// TestJobAssetNamesAreChecked: the hub refuses an asset name outside the
// subtitle and font forms, so the job runs locally.
func TestJobAssetNamesAreChecked(t *testing.T) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	for _, name := range []string{"../subtitle.ass", "fonts/font001.ttf", "evil.sh", "subtitle.ASS"} {
		if _, _, err := checkAssets([]ffmpeg.RemoteAsset{{Name: name, File: file}}); err == nil {
			t.Errorf("checkAssets accepted %q", name)
		}
	}
	if _, specs, err := checkAssets([]ffmpeg.RemoteAsset{{Name: "subtitle.ass", File: file, Size: 3}, {Name: "font001.ttf", File: file}}); err != nil || len(specs) != 2 {
		t.Fatalf("valid assets = %v, %v", specs, err)
	}
}
