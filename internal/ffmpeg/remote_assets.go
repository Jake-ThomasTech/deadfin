// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/deadfin-org/deadfin/internal/encoding"
)

// Remote burn-in (#624): a text subtitle that the server already pinned (an
// external file or an extraction) goes to the remote host as a job asset,
// with the fonts of its fontsdir. The argv names them under
// RemoteAssetsPlaceholder.

// Bounds of the assets of one remote job. A burn-in past them runs locally.
const (
	maxRemoteFonts      = 256
	maxRemoteAssetBytes = 256 << 20
)

// remoteAssets returns the assets of job and the argv rewrite that names
// them. ok is false when the burn-in must run locally.
func (manager *TranscodeManager) remoteAssets(job *transcodeJob) (assets []RemoteAsset, rewrite func([]string) []string, ok bool) {
	identity := func(args []string) []string { return args }
	if job.hls.externalSubtitleSource == "" {
		return nil, identity, true
	}
	manager.mu.Lock()
	subtitle := job.subtitle
	manager.mu.Unlock()
	if subtitle == nil || subtitle.File() == nil {
		return nil, nil, false
	}
	subtitleName := "subtitle" + assetExtension(subtitle.Path())
	assets = append(assets, RemoteAsset{Name: subtitleName, File: subtitle.File(), Size: subtitle.Fingerprint().Size})
	total := subtitle.Fingerprint().Size

	fontsDir := job.hls.fontsDir
	var fonts []*os.File
	closeFonts := func() {
		for _, font := range fonts {
			_ = font.Close()
		}
	}
	if fontsDir != "" {
		entries, err := os.ReadDir(fontsDir)
		if err != nil {
			return nil, nil, false
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				continue
			}
			if len(fonts) == maxRemoteFonts {
				closeFonts()
				return nil, nil, false
			}
			font, err := os.OpenFile(filepath.Join(fontsDir, entry.Name()), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				closeFonts()
				return nil, nil, false
			}
			info, err := font.Stat()
			if err != nil || !info.Mode().IsRegular() {
				_ = font.Close()
				closeFonts()
				return nil, nil, false
			}
			fonts = append(fonts, font)
			total += info.Size()
			if total > maxRemoteAssetBytes {
				closeFonts()
				return nil, nil, false
			}
			name := fmt.Sprintf("font%03d%s", len(fonts), assetExtension(entry.Name()))
			assets = append(assets, RemoteAsset{Name: name, File: font, Size: info.Size()})
		}
	}
	if total > maxRemoteAssetBytes {
		closeFonts()
		return nil, nil, false
	}
	manager.mu.Lock()
	job.fonts = append(job.fonts, fonts...)
	manager.mu.Unlock()

	subtitleOperand := "subtitles=f='" + RemoteAssetsPlaceholder + "/" + subtitleName + "'"
	fontsOption := ""
	if fontsDir != "" {
		fontsOption = ":fontsdir='" + encoding.EscapeSubtitleFilterPath(fontsDir) + "'"
	}
	rewrite = func(argv []string) []string {
		args := append([]string(nil), argv...)
		for index := 0; index+1 < len(args); index++ {
			if args[index] != "-vf" && args[index] != "-filter_complex" {
				continue
			}
			filter := strings.ReplaceAll(args[index+1], job.hls.externalSubtitleSource, subtitleOperand)
			if fontsOption != "" {
				filter = strings.ReplaceAll(filter, fontsOption, ":fontsdir='"+RemoteAssetsPlaceholder+"/fonts'")
			}
			args[index+1] = filter
		}
		return args
	}
	return assets, rewrite, true
}

// assetExtension is the lower-case extension of name when it is short and
// alphanumeric, else empty.
func assetExtension(name string) string {
	extension := strings.ToLower(filepath.Ext(name))
	if len(extension) < 2 || len(extension) > 9 {
		return ""
	}
	for _, r := range extension[1:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return extension
}
