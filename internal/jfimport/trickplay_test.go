// SPDX-License-Identifier: GPL-2.0-only

package jfimport_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/jfimport"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/jellyfinsource"
)

type trickplaySource struct {
	fakeSource
	infos []jellyfinsource.TrickplayInfo
}

func (s trickplaySource) TrickplayInfos(context.Context) ([]jellyfinsource.TrickplayInfo, error) {
	return s.infos, nil
}

type trickplayCall struct {
	itemID dto.UUID
	info   jellyfinsource.TrickplayInfo
}

type fakeTrickplayImporter struct {
	results map[int]jfimport.TrickplayImportResult
	calls   []trickplayCall
}

func (f *fakeTrickplayImporter) ImportTrickplay(_ context.Context, itemID dto.UUID, info jellyfinsource.TrickplayInfo) (jfimport.TrickplayImportResult, error) {
	f.calls = append(f.calls, trickplayCall{itemID: itemID, info: info})
	return f.results[info.Width], nil
}

func TestRunImportsTrickplayForItemsMatchedByPath(t *testing.T) {
	heat := jellyfinsource.TrickplayInfo{ItemID: id(1), ItemPath: "/m/Heat.mkv", Width: 320, Height: 180, TileWidth: 10, TileHeight: 10, ThumbnailCount: 570, Interval: 10000, Bandwidth: 26560}
	heatWide := heat
	heatWide.Width = 640
	heatLocal := heat
	heatLocal.Width = 480
	gone := jellyfinsource.TrickplayInfo{ItemID: id(2), ItemPath: "/m/Gone.mkv", Width: 320, TileWidth: 10, TileHeight: 10, Interval: 10000}
	src := trickplaySource{infos: []jellyfinsource.TrickplayInfo{heat, heatWide, heatLocal, gone}}
	items := &fakeItems{index: store.ImportMatchIndex{ByPath: map[string]dto.UUID{"/m/Heat.mkv": id(51)}}}
	importer := &fakeTrickplayImporter{results: map[int]jfimport.TrickplayImportResult{
		320: jfimport.TrickplayImported, 640: jfimport.TrickplayExisting, 480: jfimport.TrickplayNotWithMedia,
	}}

	report, err := jfimport.Run(t.Context(), src, &fakeIdentity{}, items, jfimport.Options{Trickplay: importer, NewUserID: mintNana})
	if err != nil {
		t.Fatal(err)
	}
	if want := []trickplayCall{{id(51), heat}, {id(51), heatWide}, {id(51), heatLocal}}; !reflect.DeepEqual(importer.calls, want) {
		t.Fatalf("importer calls = %+v, want the matched item's rows %+v", importer.calls, want)
	}
	if report.TrickplayImported != 1 || report.TrickplayExisting != 1 || report.TrickplayNotWithMedia != 1 || report.TrickplayUnmatched != 1 {
		t.Fatalf("trickplay report = imported %d existing %d not-with-media %d unmatched %d, want 1 each",
			report.TrickplayImported, report.TrickplayExisting, report.TrickplayNotWithMedia, report.TrickplayUnmatched)
	}
}

func TestRunDryRunCountsTrickplayWithoutImport(t *testing.T) {
	src := trickplaySource{infos: []jellyfinsource.TrickplayInfo{{ItemID: id(1), ItemPath: "/m/Heat.mkv", Width: 320}}}
	items := &fakeItems{index: store.ImportMatchIndex{ByPath: map[string]dto.UUID{"/m/Heat.mkv": id(51)}}}
	importer := &fakeTrickplayImporter{}

	report, err := jfimport.Run(t.Context(), src, &fakeIdentity{}, items, jfimport.Options{DryRun: true, Trickplay: importer, NewUserID: mintNana})
	if err != nil {
		t.Fatal(err)
	}
	if len(importer.calls) != 0 || report.TrickplayImported != 1 {
		t.Fatalf("dry run: importer calls = %d, imported = %d; want 0 calls and 1 counted", len(importer.calls), report.TrickplayImported)
	}
}
