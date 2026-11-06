// SPDX-License-Identifier: GPL-2.0-only

package library_test

import (
	"context"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
)

// remoteImageRecorder records the remote image intent of each movie refresh
// and gives Alpha a remote Primary image on its first refresh.
type remoteImageRecorder struct {
	runs   map[string]bool
	stored map[string]int
}

func (r *remoteImageRecorder) Refresh(_ context.Context, refresh library.RefreshContext, target *item.Item) error {
	if target.Kind != item.KindMovie || target.Name == nil {
		return nil
	}
	name, _, _ := strings.Cut(*target.Name, " (")
	r.runs[name] = refresh.RunRemoteImages
	r.stored[name] = len(refresh.StoredImages)
	if name == "Alpha" && len(refresh.StoredImages) == 0 {
		target.ImageInfos = append(target.ImageInfos, item.ImageInfo{Type: item.ImageTypePrimary, Path: "https://images.example/alpha.jpg"})
	}
	return nil
}

// TestScanRunsRemoteImagesOnFirstAndFullRefresh proves that the scanner
// asks the refresher for the remote image fetchers as upstream
// MetadataService.GetNonLocalImageProviders runs them: on a first refresh
// and on a FullRefresh image refresh, including Refresh Metadata, but not on
// a plain rescan. It also passes the stored images (deadfin#847).
func TestScanRunsRemoteImagesOnFirstAndFullRefresh(t *testing.T) {
	recorder := &remoteImageRecorder{runs: map[string]bool{}, stored: map[string]int{}}
	h := newCollectionsHarnessWith(t, false, recorder)
	if !recorder.runs["Alpha"] || recorder.stored["Alpha"] != 0 {
		t.Fatalf("first scan: run = %t, stored = %d; want true, 0", recorder.runs["Alpha"], recorder.stored["Alpha"])
	}

	h.scan(t)
	if recorder.runs["Alpha"] {
		t.Error("plain rescan runs the remote image fetchers, want not")
	}

	if _, err := h.scanner.ScanWithProgress(t.Context(), library.RefreshOptions{Metadata: library.RefreshModeFullRefresh, Image: library.RefreshModeFullRefresh}, nil); err != nil {
		t.Fatal(err)
	}
	if !recorder.runs["Alpha"] || recorder.stored["Alpha"] != 1 {
		t.Errorf("full refresh: run = %t, stored = %d; want true, 1", recorder.runs["Alpha"], recorder.stored["Alpha"])
	}

	recorder.runs = map[string]bool{}
	// The recorder persists nothing new, so the task reports no persistence;
	// only the refresh intent matters here.
	_, _ = h.scanner.RefreshExistingMetadata(t.Context(), nil)
	if !recorder.runs["Alpha"] {
		t.Error("Refresh Metadata does not run the remote image fetchers, want it to")
	}
}
