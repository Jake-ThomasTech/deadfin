// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
)

// TestRemoteBurnInSendsSubtitleAndFontsAsAssets covers #624: a burn-in of
// a pinned subtitle goes to the remote runner with the subtitle and its
// fonts as assets, and the argv names them under the asset placeholder.
func TestRemoteBurnInSendsSubtitleAndFontsAsAssets(t *testing.T) {
	fixture := newHLSFixture(t, "hold")
	runner := newFakeRemoteRunner(nil)
	manager := remoteHLSManager(t, fixture, runner)
	defer closeHLSManager(t, manager)

	input := writeHLSSource(t, fixture.root, "burn-input", "primary source bytes")
	subtitle := writeHLSSource(t, fixture.root, "burn.ASS", "[Script Info]\n")
	fonts := filepath.Join(fixture.root, "fonts: dir")
	if err := os.MkdirAll(fonts, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"A.ttf": "font a", "b.otf": "font bb"} {
		if err := os.WriteFile(filepath.Join(fonts, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(subtitle, filepath.Join(fonts, "link.ttf")); err != nil {
		t.Fatal(err)
	}
	token := "subtitles=f='" + encoding.EscapeSubtitleFilterPath(subtitle) + "'"
	fontsOption := ":fontsdir='" + encoding.EscapeSubtitleFilterPath(fonts) + "'"
	request := fixture.request("burn", 4)
	request.Args = []string{"--fixture-label=burn", "-i", "file:" + input, "-vf", "scale=1280:720," + token + fontsOption}
	request.SourceRevision = "burn-source"
	request.OpenSource = newHLSSourceProbe(input, sourcefile.Expected{}).Open
	request.ExternalSubtitleSource = token
	request.ExternalSubtitleRevision = "burn-subtitle"
	request.OpenExternalSubtitle = newHLSSourceProbe(subtitle, sourcefile.Expected{}).Open
	request.SubtitleFontsDir = fonts
	if err := manager.StartHLS(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	remote := runner.receive(t)
	filter := remote.job.Args[len(remote.job.Args)-1]
	want := "scale=1280:720,subtitles=f='" + RemoteAssetsPlaceholder + "/subtitle.ass':fontsdir='" + RemoteAssetsPlaceholder + "/fonts'"
	if filter != want {
		t.Fatalf("remote filter = %q, want %q", filter, want)
	}
	got := map[string]string{}
	for _, asset := range remote.job.Assets {
		body, err := io.ReadAll(io.NewSectionReader(asset.File, 0, asset.Size))
		if err != nil {
			t.Fatal(err)
		}
		got[asset.Name] = string(body)
	}
	if len(got) != 3 || got["subtitle.ass"] != "[Script Info]\n" || got["font001.ttf"] != "font a" || got["font002.otf"] != "font bb" {
		t.Fatalf("assets = %v, want the subtitle and the two regular fonts", got)
	}
	for _, argument := range remote.job.Args {
		if strings.Contains(argument, fixture.root) && !strings.HasPrefix(argument, "file:") {
			t.Fatalf("remote argv names a server path: %q", argument)
		}
	}
	remote.end(nil)
}

// TestRemoteEmbeddedSubtitleBurnInStaysLocal: a burn-in that reads the
// media file itself never moves.
func TestRemoteEmbeddedSubtitleBurnInStaysLocal(t *testing.T) {
	fixture := newHLSFixture(t, "hold")
	runner := newFakeRemoteRunner(nil)
	manager := remoteHLSManager(t, fixture, runner)
	defer closeHLSManager(t, manager)
	input := writeHLSSource(t, fixture.root, "embedded-input", "primary source bytes")
	token := "subtitles=f='" + encoding.EscapeSubtitleFilterPath(input) + "'"
	request := fixture.request("embedded", 4)
	request.Args = []string{"--fixture-label=embedded", "-i", "file:" + input, "-vf", "scale=1280:720," + token + ":si=0"}
	request.SourceRevision = "embedded-source"
	request.OpenSource = newHLSSourceProbe(input, sourcefile.Expected{}).Open
	request.EmbeddedSubtitleSource = token
	if err := manager.StartHLS(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 0 {
		t.Fatal("an embedded burn-in went to the remote runner")
	}
	if err := manager.StopPlaySession(context.Background(), request.PlaySessionID, request.DeviceID); err != nil {
		t.Fatal(err)
	}
}
