// SPDX-License-Identifier: GPL-2.0-only

package library_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/providers/remote"
	"github.com/deadfin-org/deadfin/internal/providers/remote/remotetest"
	"github.com/deadfin-org/deadfin/internal/providers/tmdb"
	"github.com/deadfin-org/deadfin/internal/store"
)

// TestRefreshExistingMetadataPreservesProbeAndSidecarFields proves the
// metadata-backfill operation preserves probe and sidecar fields when the
// remote stage omits them. The test seeds an existing item with Overview,
// RunTimeTicks, Genres, Studios, People, and Tags through a normal scan,
// then runs a backfill whose refresher omits all of those fields and adds
// only a provider ID and a remote image. The stored fields must survive.
func TestRefreshExistingMetadataPreservesProbeAndSidecarFields(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	// Seed the library with a scan that populates probe and sidecar fields.
	// The seedingRefresher sets Overview, RunTimeTicks, Genres, Studios,
	// People, and Tags — the fields a media probe and a local sidecar would
	// populate — plus a provider ID and a remote image so the seed is a
	// complete item row.
	seed := &probeFieldRefresher{
		overview:     "A probe-sourced overview from embedded metadata",
		runtimeTicks: int64Ptr(12345678900),
		genres:       []string{"Action", "Sci-Fi"},
		studios:      []string{"Warner Bros"},
		tags:         []string{"4k", "hdr"},
		people: []item.PersonCredit{
			{Name: "Lana Wachowski", Type: item.PersonKindDirector},
			{Name: "Keanu Reeves", Type: item.PersonKindActor, Role: stringPtrHelper("Neo")},
		},
		providerID:   "603",
		providerName: "Tmdb",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	seeded := scannerItemAtPath(t, harness.items, matrix)
	assertProbeFields(t, seeded, "seed")

	// Second pass: a backfill whose refresher omits all probe and sidecar
	// fields and adds only a new remote image. The backfill mode skips media
	// probing, so the desired item omits Overview, RunTimeTicks, Genres,
	// Studios, People, and Tags. The stored values must survive.
	omitFields := &probeFieldRefresher{
		omitProbeFields: true,
		providerID:      "603",
		providerName:    "Tmdb",
		remoteImage:     "https://image.tmdb.org/t/p/original/backdrop.jpg",
		remoteImageType: item.ImageTypeBackdrop,
	}
	newBackfillScannerWithRefresher(t, harness, omitFields)
	if _, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil); err != nil {
		t.Fatalf("backfill with omitted fields: %v", err)
	}
	backfilled := scannerItemAtPath(t, harness.items, matrix)

	// Assert the probe and sidecar fields are preserved exactly.
	if backfilled.Overview == nil || *backfilled.Overview != "A probe-sourced overview from embedded metadata" {
		t.Fatalf("backfilled Overview = %v, want the seed value — the backfill must preserve an omitted probe field", backfilled.Overview)
	}
	if backfilled.RunTimeTicks == nil || *backfilled.RunTimeTicks != 12345678900 {
		t.Fatalf("backfilled RunTimeTicks = %v, want 12345678900 — the backfill must preserve an omitted probe field", backfilled.RunTimeTicks)
	}
	if !equalStringSlice(backfilled.Values.Genres, []string{"Action", "Sci-Fi"}) {
		t.Fatalf("backfilled Genres = %v, want [Action, Sci-Fi] — the backfill must preserve an omitted sidecar field", backfilled.Values.Genres)
	}
	if !equalStringSlice(backfilled.Values.Studios, []string{"Warner Bros"}) {
		t.Fatalf("backfilled Studios = %v, want [Warner Bros] — the backfill must preserve an omitted sidecar field", backfilled.Values.Studios)
	}
	if !equalStringSlice(backfilled.Values.Tags, []string{"4k", "hdr"}) {
		t.Fatalf("backfilled Tags = %v, want [4k, hdr] — the backfill must preserve an omitted sidecar field", backfilled.Values.Tags)
	}
	if !reflect.DeepEqual(backfilled.People, seeded.People) {
		t.Fatalf("backfilled People = %#v, want %#v — the backfill must preserve the complete people slice", backfilled.People, seeded.People)
	}

	// Assert structural identity is preserved exactly.
	if backfilled.ID != seeded.ID {
		t.Fatalf("backfilled ID = %s, want %s — the backfill must preserve structural identity", backfilled.ID, seeded.ID)
	}
	if backfilled.Kind != seeded.Kind {
		t.Fatalf("backfilled Kind = %s, want %s — the backfill must preserve structural identity", backfilled.Kind, seeded.Kind)
	}
	if backfilled.Path == nil || seeded.Path == nil || *backfilled.Path != *seeded.Path {
		t.Fatalf("backfilled Path = %v, want %v — the backfill must preserve structural identity", backfilled.Path, seeded.Path)
	}
	if !equalUUIDPtr(backfilled.ParentID, seeded.ParentID) {
		t.Fatalf("backfilled ParentID = %v, want %v — the backfill must preserve structural identity", backfilled.ParentID, seeded.ParentID)
	}

	// Assert the provider ID is preserved (the refresher set the same value).
	if id, ok := backfilled.ProviderIDs.Get("Tmdb"); !ok || id != "603" {
		t.Fatalf("backfilled TMDb id = %q,%t, want 603,true", id, ok)
	}

	// Assert the remote image was added.
	var hasRemoteBackdrop bool
	for _, image := range backfilled.ImageInfos {
		if image.Type == item.ImageTypeBackdrop && image.Path == "https://image.tmdb.org/t/p/original/backdrop.jpg" {
			hasRemoteBackdrop = true
		}
	}
	if !hasRemoteBackdrop {
		t.Fatalf("backfilled movie has no remote Backdrop image; ImageInfos = %#v", backfilled.ImageInfos)
	}
}

// TestRefreshExistingMetadataReplacesUnlockedProviderValues proves a non-empty
// remote overview and provider ID replace the matching unlocked stored values.
// The test seeds an item with a stored overview and a stale provider ID, then
// runs a backfill whose refresher supplies a new overview and a new provider
// ID. The new values must replace the stored values.
func TestRefreshExistingMetadataReplacesUnlockedProviderValues(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	// Seed with a stored overview and a stale provider ID.
	seed := &probeFieldRefresher{
		overview:     "A stale overview",
		providerID:   "999",
		providerName: "Tmdb",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	// Backfill with a new overview and a new provider ID.
	replace := &probeFieldRefresher{
		overview:     "A fresh remote overview",
		providerID:   "603",
		providerName: "Tmdb",
		remoteImage:  "https://image.tmdb.org/t/p/original/poster.jpg",
	}
	newBackfillScannerWithRefresher(t, harness, replace)
	if _, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil); err != nil {
		t.Fatalf("backfill with replacement values: %v", err)
	}
	replaced := scannerItemAtPath(t, harness.items, matrix)

	if replaced.Overview == nil || *replaced.Overview != "A fresh remote overview" {
		t.Fatalf("replaced Overview = %v, want the new remote value", replaced.Overview)
	}
	if id, ok := replaced.ProviderIDs.Get("Tmdb"); !ok || id != "603" {
		t.Fatalf("replaced TMDb id = %q,%t, want 603,true — a non-empty remote provider ID must replace the stored value", id, ok)
	}
}

// TestRefreshExistingMetadataPreservesLockedFields proves locked fields survive
// the backfill. The test seeds an item with a locked Overview, then runs a
// backfill whose refresher supplies a different overview. The locked overview
// must remain unchanged.
func TestRefreshExistingMetadataPreservesLockedFields(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	lockedOverview := "A locked overview that must not change"
	// Seed with a locked overview.
	seed := &probeFieldRefresher{
		overview:     lockedOverview,
		lockOverview: true,
		providerID:   "603",
		providerName: "Tmdb",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	// Backfill with a different overview. The locked overview must survive.
	replace := &probeFieldRefresher{
		overview:     "A remote overview that must not replace the locked value",
		providerID:   "603",
		providerName: "Tmdb",
		remoteImage:  "https://image.tmdb.org/t/p/original/poster.jpg",
	}
	newBackfillScannerWithRefresher(t, harness, replace)
	if _, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil); err != nil {
		t.Fatalf("backfill with locked fields: %v", err)
	}
	locked := scannerItemAtPath(t, harness.items, matrix)
	if locked.Overview == nil || *locked.Overview != lockedOverview {
		t.Fatalf("locked Overview = %v, want the locked value %q — a locked field must survive the backfill", locked.Overview, lockedOverview)
	}
	if locked.IsLocked {
		t.Fatalf("locked item IsLocked = true, want false — the seed locks only the Overview field; IsLocked=%v LockedFields=%v", locked.IsLocked, locked.LockedFields)
	}
	if !containsMetadataField(locked.LockedFields, item.MetadataFieldOverview) {
		t.Fatalf("locked item has no Overview lock; LockedFields=%v — the seed locks the Overview field and the lock must survive the backfill", locked.LockedFields)
	}
}

// TestRefreshExistingMetadataRollsBackOnProviderError proves a provider error
// during the backfill rolls back the current item transaction and the task
// reports a Failed result. The refresher returns an error that carries the
// item path and the provider cause.
func TestRefreshExistingMetadataRollsBackOnProviderError(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	// Seed a complete item.
	seed := &probeFieldRefresher{
		overview:     "A probe-sourced overview",
		providerID:   "603",
		providerName: "Tmdb",
		remoteImage:  "https://image.tmdb.org/t/p/original/poster.jpg",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	// Backfill with a refresher that returns an error after changing the
	// overview. The error must roll back the transaction, so the stored
	// overview must remain the seed value.
	providerCause := errors.New("provider rate limit exceeded")
	failing := &errorRefresher{
		overview:   "A remote overview that must not persist",
		refreshErr: providerCause,
	}
	newBackfillScannerWithRefresher(t, harness, failing)
	result, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil)
	if err == nil && len(result.Errors) == 0 {
		t.Fatalf("RefreshExistingMetadata with a provider error returned nil with no errors; want a non-nil error or folder errors")
	}
	// The folder errors carry the provider cause and the folder path. The
	// top-level error may be the zero-work report; the folder error is the
	// provider cause.
	joined := errors.Join(append([]error{err}, result.Errors...)...)
	if joined == nil {
		t.Fatalf("RefreshExistingMetadata produced no error at all")
	}
	if !errors.Is(joined, providerCause) {
		t.Fatalf("RefreshExistingMetadata error = %v, want it to wrap the provider cause %v", joined, providerCause)
	}
	// The error must include the folder path.
	if !containsString(joined.Error(), harness.source) {
		t.Fatalf("RefreshExistingMetadata error = %q, want it to contain the folder path %q", joined.Error(), harness.source)
	}

	// The stored overview must remain the seed value.
	rolled := scannerItemAtPath(t, harness.items, matrix)
	if rolled.Overview == nil || *rolled.Overview != "A probe-sourced overview" {
		t.Fatalf("rolled back Overview = %v, want the seed value — the transaction must roll back on provider error", rolled.Overview)
	}
}

// TestRefreshExistingMetadataMixedPlanPreservesAndInserts proves the
// remote-backfill mode splits one folder plan by stored identity without
// losing either half. The test seeds one movie through a normal scan, then
// adds a second never-scanned movie, then runs a backfill whose refresher
// omits all probe and sidecar fields. The existing movie must keep its
// stored fields and identity, the absent movie must insert through the
// ordinary reconcile path with its full candidate, and the reconcile must
// delete no existing item.
func TestRefreshExistingMetadataMixedPlanPreservesAndInserts(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	// Seed the library with a scan that populates probe and sidecar fields
	// on the first movie.
	seed := &probeFieldRefresher{
		overview:     "A probe-sourced overview from embedded metadata",
		runtimeTicks: int64Ptr(12345678900),
		genres:       []string{"Action", "Sci-Fi"},
		studios:      []string{"Warner Bros"},
		tags:         []string{"4k", "hdr"},
		people: []item.PersonCredit{
			{Name: "Lana Wachowski", Type: item.PersonKindDirector},
			{Name: "Keanu Reeves", Type: item.PersonKindActor, Role: stringPtrHelper("Neo")},
		},
		providerID:   "603",
		providerName: "Tmdb",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	seeded := scannerItemAtPath(t, harness.items, matrix)
	assertProbeFields(t, seeded, "seed")

	// Add a second, never-scanned movie next to the seeded one. The backfill
	// plan for the folder now mixes one stored item and one absent item.
	sequel := scannerPath(t, harness.source, "the-matrix-reloaded.mkv")
	writeScannerFile(t, sequel, "the matrix reloaded")

	// Second pass: a backfill whose refresher omits all probe and sidecar
	// fields and adds only provider enrichment. The stored movie must keep
	// its probe and sidecar fields; the absent movie must insert through the
	// ordinary reconcile path.
	omitFields := &probeFieldRefresher{
		omitProbeFields: true,
		providerID:      "603",
		providerName:    "Tmdb",
		remoteImage:     "https://image.tmdb.org/t/p/original/poster.jpg",
		remoteImageType: item.ImageTypePrimary,
	}
	newBackfillScannerWithRefresher(t, harness, omitFields)
	if _, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil); err != nil {
		t.Fatalf("backfill over a mixed plan: %v", err)
	}

	// The seeded movie keeps its stored fields, identity, and satellites.
	preserved := scannerItemAtPath(t, harness.items, matrix)
	if preserved.ID != seeded.ID {
		t.Fatalf("preserved ID = %s, want %s — the backfill must keep the stored identity of an existing item", preserved.ID, seeded.ID)
	}
	if preserved.Overview == nil || *preserved.Overview != "A probe-sourced overview from embedded metadata" {
		t.Fatalf("preserved Overview = %v, want the seed value — the backfill must preserve an omitted probe field", preserved.Overview)
	}
	if preserved.RunTimeTicks == nil || *preserved.RunTimeTicks != 12345678900 {
		t.Fatalf("preserved RunTimeTicks = %v, want 12345678900 — the backfill must preserve an omitted probe field", preserved.RunTimeTicks)
	}
	if !equalStringSlice(preserved.Values.Genres, []string{"Action", "Sci-Fi"}) {
		t.Fatalf("preserved Genres = %v, want [Action, Sci-Fi] — the backfill must preserve an omitted sidecar field", preserved.Values.Genres)
	}
	if !equalStringSlice(preserved.Values.Studios, []string{"Warner Bros"}) {
		t.Fatalf("preserved Studios = %v, want [Warner Bros] — the backfill must preserve an omitted sidecar field", preserved.Values.Studios)
	}
	if !reflect.DeepEqual(preserved.People, seeded.People) {
		t.Fatalf("preserved People = %#v, want %#v — the backfill must preserve the complete people slice", preserved.People, seeded.People)
	}

	// The absent movie inserted through the ordinary reconcile path with the
	// full candidate from the refresher.
	inserted := scannerItemAtPath(t, harness.items, sequel)
	if inserted.ID.IsNil() || inserted.ID == seeded.ID {
		t.Fatalf("inserted ID = %s, want a fresh ID distinct from the seeded %s — the absent item must insert through reconcile", inserted.ID, seeded.ID)
	}
	if inserted.Kind != item.KindMovie {
		t.Fatalf("inserted Kind = %s, want Movie — the absent item must insert with its full candidate", inserted.Kind)
	}
	if id, ok := inserted.ProviderIDs.Get("Tmdb"); !ok || id != "603" {
		t.Fatalf("inserted TMDb id = %q,%t, want 603,true — the inserted item must carry the remote enrichment", id, ok)
	}
	var hasRemotePrimary bool
	for _, image := range inserted.ImageInfos {
		if image.Type == item.ImageTypePrimary && image.Path == "https://image.tmdb.org/t/p/original/poster.jpg" {
			hasRemotePrimary = true
			break
		}
	}
	if !hasRemotePrimary {
		t.Fatalf("inserted movie has no remote Primary image; ImageInfos = %#v", inserted.ImageInfos)
	}

	// No existing item was deleted by the mixed-plan reconcile.
	existing, err := harness.items.ReconciliationChildren(t.Context(), *seeded.ParentID)
	if err != nil {
		t.Fatalf("read children after mixed-plan backfill: %v", err)
	}
	if len(existing) != 2 {
		t.Fatalf("child count after mixed-plan backfill = %d, want 2 — the reconcile must not delete an existing sibling", len(existing))
	}
	foundSeeded := false
	for _, child := range existing {
		if child.ID == seeded.ID {
			foundSeeded = true
		}
	}
	if !foundSeeded {
		t.Fatalf("seeded item %s is missing from the stored children — the reconcile must not delete an existing sibling", seeded.ID)
	}
}

// probeFieldRefresher is a library.Refresher that populates probe and sidecar
// fields on the first pass and optionally omits them on a backfill pass. It
// simulates the media probe and local sidecar enrichment that the backfill
// mode skips, and the remote provider enrichment that the backfill mode
// preserves.
type probeFieldRefresher struct {
	overview             string
	runtimeTicks         *int64
	communityRating      *float32
	genres               []string
	studios              []string
	tags                 []string
	people               []item.PersonCredit
	providerID           string
	providerName         string
	remoteImage          string
	remoteImageType      item.ImageType
	lockOverview         bool
	omitProbeFields      bool
	keepPeopleInBackfill bool
	// sidecarName and sidecarSortName set a stored Name and SortName that
	// differ from the filename. A provider that persists a provider ID
	// without changing Name must preserve these stored sidecar values.
	sidecarName     string
	sidecarSortName string
}

func (r *probeFieldRefresher) Refresh(_ context.Context, _ library.RefreshContext, target *item.Item) error {
	if r.lockOverview {
		target.IsLocked = false
		target.LockedFields = append(target.LockedFields, item.MetadataFieldOverview)
	}
	if !r.omitProbeFields {
		if r.overview != "" {
			target.Overview = &r.overview
		}
		if r.runtimeTicks != nil {
			target.RunTimeTicks = r.runtimeTicks
		}
		if r.communityRating != nil {
			target.CommunityRating = r.communityRating
		}
		if r.sidecarName != "" {
			target.Name = &r.sidecarName
		}
		if r.sidecarSortName != "" {
			target.SortName = &r.sidecarSortName
		}
		if r.genres != nil {
			target.Values.Genres = r.genres
		}
		if r.studios != nil {
			target.Values.Studios = r.studios
		}
		if r.tags != nil {
			target.Values.Tags = r.tags
		}
	}
	if r.people != nil && (r.keepPeopleInBackfill || !r.omitProbeFields) {
		target.People = r.people
	}
	if r.providerID != "" && r.providerName != "" {
		target.ProviderIDs.Set(r.providerName, r.providerID)
	}
	if r.remoteImage != "" {
		imageType := r.remoteImageType
		if imageType == 0 {
			imageType = item.ImageTypePrimary
		}
		target.ImageInfos = append(target.ImageInfos, item.ImageInfo{Type: imageType, Path: r.remoteImage})
	}
	return nil
}

// errorRefresher is a library.Refresher that sets an overview and then returns
// an error, so the test can prove the transaction rolls back.
type errorRefresher struct {
	overview   string
	refreshErr error
}

func (r *errorRefresher) Refresh(_ context.Context, _ library.RefreshContext, target *item.Item) error {
	if r.overview != "" {
		target.Overview = &r.overview
	}
	return r.refreshErr
}

func assertProbeFields(t *testing.T, value item.Item, label string) {
	t.Helper()
	if value.Overview == nil || *value.Overview == "" {
		t.Fatalf("%s item has no Overview; item = %#v", label, value)
	}
	if value.RunTimeTicks == nil || *value.RunTimeTicks == 0 {
		t.Fatalf("%s item has no RunTimeTicks; item = %#v", label, value)
	}
	if len(value.Values.Genres) == 0 {
		t.Fatalf("%s item has no Genres; item = %#v", label, value)
	}
	if len(value.Values.Studios) == 0 {
		t.Fatalf("%s item has no Studios; item = %#v", label, value)
	}
	if len(value.Values.Tags) == 0 {
		t.Fatalf("%s item has no Tags; item = %#v", label, value)
	}
	if len(value.People) == 0 {
		t.Fatalf("%s item has no People; item = %#v", label, value)
	}
}

func int64Ptr(value int64) *int64 { return &value }

func stringPtrHelper(value string) *string { return &value }

func float32PtrHelper(value float32) *float32 { return &value }

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// imageSetEqual reports whether two image slices contain the same images by
// (Type, Index, Path) without regard for order.
func imageSetEqual(got, want []item.ImageInfo) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g.Type == w.Type && g.Index == w.Index && g.Path == w.Path {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func equalUUIDPtr(a, b *dto.UUID) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func containsMetadataField(fields []item.MetadataField, field item.MetadataField) bool {
	for _, f := range fields {
		if f == field {
			return true
		}
	}
	return false
}

func containsString(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || (len(needle) > 0 && stringContains(haystack, needle)))
}

func stringContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestRemoteRefreshRootPartitionPreservesExistingRoots proves the
// collection-level plan routes stored root Folders through the backfill
// merge, not the insert path. The harness has one root; the seed scan
// stores it. The backfill pass re-plans the same root Folder; the stored
// root must keep its stored User_Data and satellites, and the scan must not
// report an ordinary insert for it.
func TestRemoteRefreshRootPartitionPreservesExistingRoots(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	// Seed the library: the ordinary scan stores the root Folder and the
	// movie beneath it, with a locked Overview and a local Primary image.
	seed := &probeFieldRefresher{
		overview:        "A probe-sourced overview",
		lockOverview:    true,
		providerID:      "603",
		providerName:    "Tmdb",
		remoteImage:     "https://image.tmdb.org/t/p/original/seed.jpg",
		remoteImageType: item.ImageTypeBackdrop,
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	seeded := scannerItemAtPath(t, harness.items, harness.source)
	if seeded.ID.IsNil() {
		t.Fatalf("seed scan did not persist the root Folder item")
	}
	// Seed playback state on the stored root Folder so the backfill must
	// preserve user data that lives outside the item row.
	userID := dto.UUID{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	if _, err := harness.items.MutateUserDataForItem(t.Context(), userID, seeded.ID, func(data *store.UserData) error {
		data.Played = true
		data.PlayCount = 3
		return nil
	}); err != nil {
		t.Fatalf("seed root Folder user data: %v", err)
	}
	// Read the seeded playback state back through the mutation seam, which
	// resolves the item's canonical user-data key.
	var before store.UserData
	if _, err := harness.items.MutateUserDataForItem(t.Context(), userID, seeded.ID, func(data *store.UserData) error {
		before = *data
		return nil
	}); err != nil {
		t.Fatalf("read seeded root Folder user data: %v", err)
	}

	// Backfill pass: the same root re-plans. The stored root Folder must
	// route through the backfill merge, not the insert path: the locked
	// Overview, the stored images, and the playback user data all survive.
	backfill := &probeFieldRefresher{
		providerID:   "603",
		providerName: "Tmdb",
		remoteImage:  "https://image.tmdb.org/t/p/original/backfill.jpg",
	}
	newBackfillScannerWithRefresher(t, harness, backfill)
	result, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil)
	if err != nil {
		t.Fatalf("RefreshExistingMetadata: %v", err)
	}
	redone := scannerItemAtPath(t, harness.items, harness.source)
	if redone.ID != seeded.ID {
		t.Fatalf("backfill did not preserve the stored root Folder ID; seeded = %#v, redone = %#v", seeded, redone)
	}
	var after store.UserData
	if _, err := harness.items.MutateUserDataForItem(t.Context(), userID, seeded.ID, func(data *store.UserData) error {
		after = *data
		return nil
	}); err != nil {
		t.Fatalf("read root Folder user data after backfill: %v", err)
	}
	if after != before {
		t.Fatalf("backfill changed the stored root Folder user data; before = %#v, after = %#v", before, after)
	}
	// The stored root Folder keeps its name, and the movie beneath it keeps
	// its stored Overview: the root re-plan must not disturb the subtree.
	if redone.Name == nil || *redone.Name != *seeded.Name {
		t.Fatalf("backfill changed the stored root Folder name; seeded = %#v, redone = %#v", seeded.Name, redone.Name)
	}
	movie := scannerItemAtPath(t, harness.items, matrix)
	if movie.ID.IsNil() {
		t.Fatalf("backfill deleted the movie beneath the stored root Folder")
	}
	if movie.Overview == nil || *movie.Overview != "A probe-sourced overview" {
		t.Fatalf("backfill lost the movie Overview beneath the root; Overview = %v", movie.Overview)
	}
	if result.PersistedProviderIDs == 0 && result.PersistedImages == 0 {
		t.Fatalf("backfill reported no persistence at all")
	}
}

// TestRemoteRefreshRootPartitionInsertsAbsentRoots proves an absent root
// Folder inserts through the ordinary reconcile path while the stored
// sibling root merges on the backfill path.
func TestRemoteRefreshRootPartitionInsertsAbsentRoots(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	secondSource := t.TempDir()
	if err := os.MkdirAll(filepath.Join(secondSource, "second"), 0o755); err != nil {
		t.Fatalf("mkdir second root: %v", err)
	}
	writeScannerFile(t, filepath.Join(secondSource, "second", "b.mkv"), "b")

	// Seed scan over the first root only, then attach the second root as a
	// second media path before the backfill pass.
	newBackfillScannerWithRefresher(t, harness, &countingRefresher{enrich: true})
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan over the first root: %v", err)
	}
	firstRoot := scannerItemAtPath(t, harness.items, harness.source)
	if firstRoot.ID.IsNil() {
		t.Fatalf("seed scan did not persist the first root Folder")
	}
	folders, err := harness.topology.VirtualFolders(t.Context())
	if err != nil || len(folders) == 0 {
		t.Fatalf("list virtual folders: %#v, %v", folders, err)
	}
	if err := harness.topology.AddMediaPath(t.Context(), folders[0].ID, filepath.Join(secondSource, "second")); err != nil {
		t.Fatalf("add second media path: %v", err)
	}
	newBackfillScannerWithRefresher(t, harness, &countingRefresher{enrich: true})
	if _, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil); err != nil {
		t.Fatalf("backfill over absent root: %v", err)
	}
	reinserted := scannerItemAtPath(t, harness.items, filepath.Join(secondSource, "second"))
	if reinserted.ID.IsNil() {
		t.Fatalf("backfill did not insert the absent second root")
	}
	survivor := scannerItemAtPath(t, harness.items, harness.source)
	if survivor.ID != firstRoot.ID {
		t.Fatalf("backfill deleted or replaced the stored first root; want %s", firstRoot.ID)
	}
}

// TestRefreshExistingMetadataPreservesVirtualMetadataAndInsertsAbsentChild
// proves the remote-backfill mode partitions virtual candidates by stored
// identity. The test seeds a Person virtual with metadata and a satellite
// family, an existing child that references the Person, and an absent child
// in the same backfill plan. A sparse remote candidate omits the stored
// virtual metadata and satellite family. The Person virtual must keep its
// stored metadata, structural identity, and satellites. The existing child
// must stay unchanged. The absent child must insert through ordinary
// reconciliation with its complete candidate. No existing child, virtual
// row, metadata field, or satellite row is deleted.
func TestRefreshExistingMetadataPreservesVirtualMetadataAndInsertsAbsentChild(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	personName := "Lana Wachowski"
	personCredit := item.PersonCredit{Name: personName, Type: item.PersonKindDirector}

	// Seed the library with a scan that populates probe and sidecar fields
	// and People credits so nfoPeople creates the Person virtual.
	seed := &probeFieldRefresher{
		overview:     "A probe-sourced overview from embedded metadata",
		runtimeTicks: int64Ptr(12345678900),
		genres:       []string{"Action", "Sci-Fi"},
		studios:      []string{"Warner Bros"},
		tags:         []string{"4k", "hdr"},
		people:       []item.PersonCredit{personCredit},
		providerID:   "603",
		providerName: "Tmdb",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	seeded := scannerItemAtPath(t, harness.items, matrix)
	assertProbeFields(t, seeded, "seed")

	// Enrich the stored Person virtual with metadata, a provider ID, an
	// image, and a satellite family. The backfill must preserve all of these
	// because the sparse remote candidate omits them.
	person := library.PersonEntity(personName)
	person.Overview = stringPtrHelper("A director biography from a sidecar")
	person.RunTimeTicks = int64Ptr(999999)
	person.Values.Genres = []string{"Film Noir"}
	person.Values.Studios = []string{"Studio Ghibli"}
	person.Values.Tags = []string{"tag-person"}
	person.ProviderIDs.Set("Imdb", "nm0001")
	person.ImageInfos = []item.ImageInfo{
		{Type: item.ImageTypePrimary, Index: 0, Path: "https://image.tmdb.org/t/p/original/person.jpg"},
	}
	person.TrailerTypes = []item.TrailerType{item.TrailerTypeLocalTrailer}
	if err := harness.items.Upsert(t.Context(), person); err != nil {
		t.Fatalf("enrich Person virtual: %v", err)
	}
	enriched, err := harness.items.Get(t.Context(), person.ID)
	if err != nil {
		t.Fatalf("load enriched Person virtual: %v", err)
	}

	// Add a second, never-scanned movie next to the seeded one. The backfill
	// plan for the folder mixes one stored child and one absent child.
	sequel := scannerPath(t, harness.source, "the-matrix-reloaded.mkv")
	writeScannerFile(t, sequel, "the matrix reloaded")

	// Backfill pass: the refresher keeps People on the movie so nfoPeople
	// produces the same Person virtual, but omits all other probe and
	// sidecar fields. The sparse Person virtual omits the stored metadata
	// and satellite family.
	omitFields := &probeFieldRefresher{
		omitProbeFields:      true,
		keepPeopleInBackfill: true,
		people:               []item.PersonCredit{personCredit},
		providerID:           "603",
		providerName:         "Tmdb",
		remoteImage:          "https://image.tmdb.org/t/p/original/poster.jpg",
		remoteImageType:      item.ImageTypePrimary,
	}
	newBackfillScannerWithRefresher(t, harness, omitFields)
	if _, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil); err != nil {
		t.Fatalf("backfill over a mixed plan with virtuals: %v", err)
	}

	// The Person virtual keeps its stored metadata, structural identity, and
	// satellites. The sparse remote candidate omitted all of these.
	after, err := harness.items.Get(t.Context(), person.ID)
	if err != nil {
		t.Fatalf("load Person virtual after backfill: %v", err)
	}
	if after.ID != enriched.ID {
		t.Fatalf("Person virtual ID = %s, want %s — the backfill must preserve the stored identity", after.ID, enriched.ID)
	}
	if after.Kind != item.KindPerson {
		t.Fatalf("Person virtual Kind = %s, want Person — the backfill must preserve structural identity", after.Kind)
	}
	if !reflect.DeepEqual(after.Path, enriched.Path) {
		t.Fatalf("Person virtual Path = %v, want %v — the backfill must preserve structural identity", after.Path, enriched.Path)
	}
	if !reflect.DeepEqual(after.Name, enriched.Name) {
		t.Fatalf("Person virtual Name = %v, want %v — the backfill must preserve structural identity", after.Name, enriched.Name)
	}
	if after.Overview == nil || *after.Overview != "A director biography from a sidecar" {
		t.Fatalf("Person virtual Overview = %v, want the stored value — the backfill must preserve omitted metadata", after.Overview)
	}
	if after.RunTimeTicks == nil || *after.RunTimeTicks != 999999 {
		t.Fatalf("Person virtual RunTimeTicks = %v, want 999999 — the backfill must preserve omitted metadata", after.RunTimeTicks)
	}
	if !equalStringSlice(after.Values.Genres, []string{"Film Noir"}) {
		t.Fatalf("Person virtual Genres = %v, want [Film Noir] — the backfill must preserve omitted metadata", after.Values.Genres)
	}
	if !equalStringSlice(after.Values.Studios, []string{"Studio Ghibli"}) {
		t.Fatalf("Person virtual Studios = %v, want [Studio Ghibli] — the backfill must preserve omitted metadata", after.Values.Studios)
	}
	if !equalStringSlice(after.Values.Tags, []string{"tag-person"}) {
		t.Fatalf("Person virtual Tags = %v, want [tag-person] — the backfill must preserve omitted metadata", after.Values.Tags)
	}
	if id, ok := after.ProviderIDs.Get("Imdb"); !ok || id != "nm0001" {
		t.Fatalf("Person virtual Imdb id = %q,%t, want nm0001,true — the backfill must preserve an unrelated provider ID", id, ok)
	}
	var hasPersonImage bool
	for _, image := range after.ImageInfos {
		if image.Type == item.ImageTypePrimary && image.Path == "https://image.tmdb.org/t/p/original/person.jpg" {
			hasPersonImage = true
		}
	}
	if !hasPersonImage {
		t.Fatalf("Person virtual lost its stored Primary image; ImageInfos = %#v", after.ImageInfos)
	}
	if !reflect.DeepEqual(after.TrailerTypes, enriched.TrailerTypes) {
		t.Fatalf("Person virtual TrailerTypes = %v, want %v — the backfill must preserve an untouched satellite family", after.TrailerTypes, enriched.TrailerTypes)
	}

	// The existing child keeps its stored fields and identity.
	preserved := scannerItemAtPath(t, harness.items, matrix)
	if preserved.ID != seeded.ID {
		t.Fatalf("existing child ID = %s, want %s — the backfill must keep the stored identity", preserved.ID, seeded.ID)
	}
	if preserved.Overview == nil || *preserved.Overview != "A probe-sourced overview from embedded metadata" {
		t.Fatalf("existing child Overview = %v, want the seed value — the backfill must preserve an omitted probe field", preserved.Overview)
	}
	if preserved.RunTimeTicks == nil || *preserved.RunTimeTicks != 12345678900 {
		t.Fatalf("existing child RunTimeTicks = %v, want 12345678900 — the backfill must preserve an omitted probe field", preserved.RunTimeTicks)
	}
	if !equalStringSlice(preserved.Values.Genres, []string{"Action", "Sci-Fi"}) {
		t.Fatalf("existing child Genres = %v, want [Action, Sci-Fi] — the backfill must preserve an omitted sidecar field", preserved.Values.Genres)
	}

	// The absent child inserted through the ordinary reconcile path with
	// the full candidate from the refresher.
	inserted := scannerItemAtPath(t, harness.items, sequel)
	if inserted.ID.IsNil() || inserted.ID == seeded.ID {
		t.Fatalf("absent child ID = %s, want a fresh ID — the absent item must insert through reconcile", inserted.ID)
	}
	if inserted.Kind != item.KindMovie {
		t.Fatalf("absent child Kind = %s, want Movie — the absent item must insert with its full candidate", inserted.Kind)
	}
	if id, ok := inserted.ProviderIDs.Get("Tmdb"); !ok || id != "603" {
		t.Fatalf("absent child TMDb id = %q,%t, want 603,true — the inserted item must carry the remote enrichment", id, ok)
	}

	// No existing child or virtual row was deleted.
	existing, err := harness.items.ReconciliationChildren(t.Context(), *seeded.ParentID)
	if err != nil {
		t.Fatalf("read children after mixed-plan backfill: %v", err)
	}
	if len(existing) != 2 {
		t.Fatalf("child count after mixed-plan backfill = %d, want 2 — the reconcile must not delete an existing sibling", len(existing))
	}
	personCount := 0
	for _, candidate := range scannerItems(t, harness.items) {
		if candidate.Kind == item.KindPerson && candidate.ID == person.ID {
			personCount++
		}
	}
	if personCount != 1 {
		t.Fatalf("Person virtual count = %d, want 1 — the backfill must not delete or duplicate the Person virtual", personCount)
	}
}

// enableInternetProviders rewrites the library options.xml so the next
// VirtualFolders call sees EnableInternetProviders=true. The registry gates
// the remote stage on this flag, so the TMDb provider runs only after the
// rewrite.
func enableInternetProviders(t *testing.T, harness *scannerHarness) {
	t.Helper()
	folders, err := harness.topology.VirtualFolders(t.Context())
	if err != nil || len(folders) == 0 {
		t.Fatalf("list virtual folders before reconfigure: %#v, %v", folders, err)
	}
	enabled := folders[0].Options
	enabled.EnableInternetProviders = true
	enabled.PreferredMetadataLanguage = "en"
	enabled.MetadataCountryCode = "US"
	rewritten, err := xml.MarshalIndent(enabled, "", "  ")
	if err != nil {
		t.Fatalf("marshal enabled library options: %v", err)
	}
	optionsPath := filepath.Join(folders[0].Path, "options.xml")
	if err := os.WriteFile(optionsPath, append(rewritten, '\n'), 0o600); err != nil {
		t.Fatalf("rewrite options.xml to enable internet providers: %v", err)
	}
}

// newCassetteRegistry builds a providers.Registry that wraps a real TMDb
// provider backed by the given cassette interactions. The registry is the
// scanner Refresher so the library test exercises the genuine remote-stage
// gate rather than a raw fake.
func newCassetteRegistry(cassette *remotetest.Cassette) *providers.Registry {
	config := tmdb.DefaultConfig()
	config.APIKey = "test-key"
	provider := tmdb.NewProvider(tmdb.Options{
		HTTP:   remote.NewClient(remote.Config{Doer: cassette, CacheEntries: 32}),
		Config: config,
	})
	return providers.NewRegistry(provider)
}

// multiRefresher runs a list of library.Refreshers in order. The first
// refresher sets local fields and the stored TMDb provider ID on the desired
// item, and the second refresher (the providers.Registry) uses that provider ID
// to fetch the remote response from the cassette. This mirrors the real scan
// flow: local enrichment runs before the remote stage, and the remote stage
// reads the provider ID the local stage set.
type multiRefresher struct {
	refreshers []library.Refresher
}

func (m multiRefresher) Refresh(ctx context.Context, rc library.RefreshContext, target *item.Item) error {
	for _, r := range m.refreshers {
		if err := r.Refresh(ctx, rc, target); err != nil {
			return err
		}
	}
	return nil
}

// cassetteBackfillRefresher builds a composite refresher that first applies
// local probe and sidecar fields from localFields, then runs the TMDb provider
// through the providers.Registry backed by the cassette. The local refresher
// sets the TMDb provider ID on the desired item so the provider skips the
// search step and fetches /movie/{id} directly from the cassette.
func cassetteBackfillRefresher(cassette *remotetest.Cassette, localFields library.Refresher) multiRefresher {
	return multiRefresher{refreshers: []library.Refresher{localFields, newCassetteRegistry(cassette)}}
}

// minimalMovieCassette returns a cassette that answers /configuration and
// /movie/603 with a minimal movie response. The response carries a TMDb id,
// a title, and a poster path, but omits Overview, Genres, Studios, People,
// Tags, and all image sub-resources. This lets the test prove the backfill
// preserves local probe and sidecar fields the remote stage omits.
func minimalMovieCassette() *remotetest.Cassette {
	return remotetest.NewCassette(
		remotetest.Interaction{
			Method: http.MethodGet,
			URL:    "https://api.themoviedb.org/3/configuration",
			Status: http.StatusOK,
			Header: map[string][]string{"Content-Type": {"application/json"}},
			Body:   []byte(`{"images":{"secure_base_url":"https://image.tmdb.org/t/p/","poster_sizes":["original"],"backdrop_sizes":["original"]}}`),
		},
		remotetest.Interaction{
			Method: http.MethodGet,
			URL:    "https://api.themoviedb.org/3/movie/603?language=en&include_image_language=en,null&append_to_response=credits,release_dates,images,videos,keywords",
			Status: http.StatusOK,
			Header: map[string][]string{"Content-Type": {"application/json"}},
			Body:   []byte(`{"id":603,"title":"The Matrix","poster_path":"/matrix-poster.jpg","imdb_id":"tt0133093"}`),
		},
	)
}

// minimalMovieCassetteNoTitle returns a cassette that answers /configuration
// and /movie/603 with a movie response that omits the title field. The
// response carries a TMDb id and an imdb id, but no title, so the TMDb
// provider does not supply a Name. This lets the test prove the backfill
// preserves a stored sidecar Name when the remote omits the field.
func minimalMovieCassetteNoTitle() *remotetest.Cassette {
	return remotetest.NewCassette(
		remotetest.Interaction{
			Method: http.MethodGet,
			URL:    "https://api.themoviedb.org/3/configuration",
			Status: http.StatusOK,
			Header: map[string][]string{"Content-Type": {"application/json"}},
			Body:   []byte(`{"images":{"secure_base_url":"https://image.tmdb.org/t/p/","poster_sizes":["original"],"backdrop_sizes":["original"]}}`),
		},
		remotetest.Interaction{
			Method: http.MethodGet,
			URL:    "https://api.themoviedb.org/3/movie/603?language=en&include_image_language=en,null&append_to_response=credits,release_dates,images,videos,keywords",
			Status: http.StatusOK,
			Header: map[string][]string{"Content-Type": {"application/json"}},
			Body:   []byte(`{"id":603,"imdb_id":"tt0133093"}`),
		},
	)
}

// TestRefreshExistingMetadataCassettePreservesOmittedFields proves the
// metadata-backfill operation preserves probe and sidecar fields when the
// real TMDb provider, backed by a cassette, omits them. The test seeds an
// existing item with Overview, RunTimeTicks, Genres, Studios, People, and
// Tags through a local refresher, then runs a backfill whose cassette
// response omits all of those fields and adds only a provider ID and a remote
// image. The stored fields must survive the backfill with exact values. The
// provider ID and remote image must replace the stored values.
func TestRefreshExistingMetadataCassettePreservesOmittedFields(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	// Seed the library with a local refresher that populates probe and
	// sidecar fields and a stale TMDb provider ID. Internet providers are
	// disabled during the seed, so the remote stage does not run.
	seed := &probeFieldRefresher{
		overview:     "A probe-sourced overview from embedded metadata",
		runtimeTicks: int64Ptr(12345678900),
		genres:       []string{"Action", "Sci-Fi"},
		studios:      []string{"Warner Bros"},
		tags:         []string{"4k", "hdr"},
		people: []item.PersonCredit{
			{Name: "Lana Wachowski", Type: item.PersonKindDirector},
			{Name: "Keanu Reeves", Type: item.PersonKindActor, Role: stringPtrHelper("Neo")},
		},
		providerID:   "999",
		providerName: "Tmdb",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	seeded := scannerItemAtPath(t, harness.items, matrix)
	assertProbeFields(t, seeded, "seed")
	if id, ok := seeded.ProviderIDs.Get("Tmdb"); !ok || id != "999" {
		t.Fatalf("seed TMDb id = %q,%t, want 999 — the seed must set a stale provider ID", id, ok)
	}

	// Enable internet providers and switch to the real TMDb provider backed
	// by a cassette. The composite refresher first sets the TMDb provider ID
	// on the desired item (mirroring local metadata enrichment), then runs the
	// registry, which fetches /movie/603 from the cassette. The cassette
	// response omits Overview, Genres, Studios, People, and Tags. The backfill
	// must preserve the stored values.
	enableInternetProviders(t, harness)
	backfillLocal := &probeFieldRefresher{
		omitProbeFields: true,
		providerID:      "603",
		providerName:    "Tmdb",
	}
	refresher := cassetteBackfillRefresher(minimalMovieCassette(), backfillLocal)
	newBackfillScannerWithRefresher(t, harness, refresher)
	if _, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil); err != nil {
		t.Fatalf("cassette backfill: %v", err)
	}
	backfilled := scannerItemAtPath(t, harness.items, matrix)

	// The probe and sidecar fields are preserved exactly.
	if backfilled.Overview == nil || *backfilled.Overview != "A probe-sourced overview from embedded metadata" {
		t.Fatalf("backfilled Overview = %v, want the seed value — the cassette omitted overview so the backfill must preserve it", backfilled.Overview)
	}
	if backfilled.RunTimeTicks == nil || *backfilled.RunTimeTicks != 12345678900 {
		t.Fatalf("backfilled RunTimeTicks = %v, want 12345678900 — the cassette omitted runtime so the backfill must preserve it", backfilled.RunTimeTicks)
	}
	if !equalStringSlice(backfilled.Values.Genres, []string{"Action", "Sci-Fi"}) {
		t.Fatalf("backfilled Genres = %v, want [Action, Sci-Fi] — the cassette omitted genres so the backfill must preserve them", backfilled.Values.Genres)
	}
	if !equalStringSlice(backfilled.Values.Studios, []string{"Warner Bros"}) {
		t.Fatalf("backfilled Studios = %v, want [Warner Bros] — the cassette omitted studios so the backfill must preserve them", backfilled.Values.Studios)
	}
	if !equalStringSlice(backfilled.Values.Tags, []string{"4k", "hdr"}) {
		t.Fatalf("backfilled Tags = %v, want [4k, hdr] — the cassette omitted tags so the backfill must preserve them", backfilled.Values.Tags)
	}
	// The complete People slice must survive the backfill, not only the
	// count. canonicalizePeople rewrites credits that share a zero PersonID
	// to the first name, so the assertion compares the full slice against the
	// seeded values with exact equality.
	if !reflect.DeepEqual(backfilled.People, seeded.People) {
		t.Fatalf("backfilled People = %#v, want %#v — the cassette omitted people so the backfill must preserve the complete slice", backfilled.People, seeded.People)
	}

	// The provider ID is replaced with the cassette value.
	if id, ok := backfilled.ProviderIDs.Get("Tmdb"); !ok || id != "603" {
		t.Fatalf("backfilled TMDb id = %q,%t, want 603 — a non-empty remote provider ID must replace the stored value", id, ok)
	}

	// Structural identity is preserved exactly.
	if backfilled.ID != seeded.ID {
		t.Fatalf("backfilled ID = %s, want %s — the backfill must preserve structural identity", backfilled.ID, seeded.ID)
	}
	if backfilled.Kind != seeded.Kind {
		t.Fatalf("backfilled Kind = %s, want %s — the backfill must preserve structural identity", backfilled.Kind, seeded.Kind)
	}
	if backfilled.Path == nil || seeded.Path == nil || *backfilled.Path != *seeded.Path {
		t.Fatalf("backfilled Path = %v, want %v — the backfill must preserve structural identity", backfilled.Path, seeded.Path)
	}
	if !equalUUIDPtr(backfilled.ParentID, seeded.ParentID) {
		t.Fatalf("backfilled ParentID = %v, want %v — the backfill must preserve structural identity", backfilled.ParentID, seeded.ParentID)
	}

	// The remote image was added. The cassette /configuration response sets
	// the image base to https://image.tmdb.org/t/p/ and the poster size to
	// original, and /movie/603 returns poster_path /matrix-poster.jpg, so the
	// full Primary image path is fixed. Assert the exact image set rather
	// than a URL prefix.
	wantImages := []item.ImageInfo{
		{Type: item.ImageTypePrimary, Index: 0, Path: "https://image.tmdb.org/t/p/original/matrix-poster.jpg"},
	}
	if !imageSetEqual(backfilled.ImageInfos, wantImages) {
		t.Fatalf("backfilled ImageInfos = %#v, want exact set %#v — the remote image must persist with the exact cassette path", backfilled.ImageInfos, wantImages)
	}
}

// TestRefreshExistingMetadataCassetteReplacesUnlockedProviderValues proves a
// non-empty remote overview and provider ID from the real TMDb cassette
// replace the matching unlocked stored values. The test seeds an item with a
// stale overview and a stale provider ID, then runs a backfill against the
// full the-matrix.json cassette, which supplies a fresh overview and a new
// provider ID.
func TestRefreshExistingMetadataCassetteReplacesUnlockedProviderValues(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	// Seed with a stale overview and a stale provider ID.
	seed := &probeFieldRefresher{
		overview:     "A stale overview",
		providerID:   "999",
		providerName: "Tmdb",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	// Seed an unrelated stored Extra field. The remote stage does not produce
	// the video aspect ratio, so the backfill merge must preserve it.
	seeded := scannerItemAtPath(t, harness.items, matrix)
	seeded.Extra.Video = &item.VideoExtra{AspectRatio: stringPtrHelper("16:9")}
	if err := harness.items.Upsert(t.Context(), seeded); err != nil {
		t.Fatalf("seed video extra: %v", err)
	}

	// Enable internet providers and switch to the real TMDb provider backed
	// by the full the-matrix.json cassette. The composite refresher first
	// sets the TMDb provider ID on the desired item, then runs the registry.
	// The cassette supplies a fresh overview, a new TMDb id, genres, studios,
	// people, and images.
	enableInternetProviders(t, harness)
	cassette, err := remotetest.Load("../providers/tmdb/testdata/the-matrix.json")
	if err != nil {
		t.Fatalf("load cassette: %v", err)
	}
	backfillLocal := &probeFieldRefresher{
		omitProbeFields: true,
		providerID:      "603",
		providerName:    "Tmdb",
	}
	refresher := cassetteBackfillRefresher(cassette, backfillLocal)
	newBackfillScannerWithRefresher(t, harness, refresher)
	if _, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil); err != nil {
		t.Fatalf("cassette backfill: %v", err)
	}
	replaced := scannerItemAtPath(t, harness.items, matrix)

	// The remote overview replaced the stored stale value.
	if replaced.Overview == nil || *replaced.Overview != "A computer hacker learns from mysterious rebels.\nHe joins the fight." {
		t.Fatalf("replaced Overview = %v, want the cassette value — a non-empty remote overview must replace the stored value", replaced.Overview)
	}
	// The remote provider ID replaced the stored stale value.
	if id, ok := replaced.ProviderIDs.Get("Tmdb"); !ok || id != "603" {
		t.Fatalf("replaced TMDb id = %q,%t, want 603 — a non-empty remote provider ID must replace the stored value", id, ok)
	}
	// The remote genres replaced the stored value.
	if !equalStringSlice(replaced.Values.Genres, []string{"Action", "Science Fiction"}) {
		t.Fatalf("replaced Genres = %v, want [Action, Science Fiction] — a non-empty remote value must replace the stored value", replaced.Values.Genres)
	}

	// The remote TMDb collection name persisted. The cassette /movie/603
	// response lists belongs_to_collection.name as "The Matrix Collection".
	if replaced.Extra.Movie == nil || replaced.Extra.Movie.TmdbCollectionName == nil || *replaced.Extra.Movie.TmdbCollectionName != "The Matrix Collection" {
		t.Fatalf("replaced TmdbCollectionName = %v, want \"The Matrix Collection\" — a non-empty remote extra value must persist", replaced.Extra.Movie)
	}

	// The remote trailers persisted. The cassette /movie/603 response lists
	// one Trailer-type YouTube video (key vKQi3bBA1y8). The Featurette is not
	// a trailer, so the list has exactly one entry with the exact URL and name.
	if len(replaced.Extra.RemoteTrailers) != 1 {
		t.Fatalf("replaced RemoteTrailers len = %d, want 1 — the remote trailer list must persist", len(replaced.Extra.RemoteTrailers))
	}
	wantTrailerURL := "https://www.youtube.com/watch?v=vKQi3bBA1y8"
	if replaced.Extra.RemoteTrailers[0].URL == nil || *replaced.Extra.RemoteTrailers[0].URL != wantTrailerURL {
		t.Fatalf("replaced RemoteTrailers[0].URL = %v, want %q — the exact remote trailer URL must persist", replaced.Extra.RemoteTrailers[0].URL, wantTrailerURL)
	}
	wantTrailerName := "The Matrix Trailer"
	if replaced.Extra.RemoteTrailers[0].Name == nil || *replaced.Extra.RemoteTrailers[0].Name != wantTrailerName {
		t.Fatalf("replaced RemoteTrailers[0].Name = %v, want %q — the exact remote trailer name must persist", replaced.Extra.RemoteTrailers[0].Name, wantTrailerName)
	}

	// The unrelated stored Extra field survived. The remote stage does not
	// produce the video aspect ratio, so the backfill merge preserves it.
	if replaced.Extra.Video == nil || replaced.Extra.Video.AspectRatio == nil || *replaced.Extra.Video.AspectRatio != "16:9" {
		t.Fatalf("replaced Video.AspectRatio = %v, want \"16:9\" — an unrelated stored Extra field must survive the backfill", replaced.Extra.Video)
	}
}

// TestRefreshExistingMetadataCassettePreservesLockedFields proves locked
// fields survive the backfill against the real TMDb provider. The test seeds
// an item with a locked Overview, then runs a backfill whose cassette
// supplies a different overview. The locked overview must remain unchanged.
func TestRefreshExistingMetadataCassettePreservesLockedFields(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	lockedOverview := "A locked overview that must not change"
	seed := &probeFieldRefresher{
		overview:     lockedOverview,
		lockOverview: true,
		providerID:   "603",
		providerName: "Tmdb",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	enableInternetProviders(t, harness)
	cassette, err := remotetest.Load("../providers/tmdb/testdata/the-matrix.json")
	if err != nil {
		t.Fatalf("load cassette: %v", err)
	}
	backfillLocal := &probeFieldRefresher{
		omitProbeFields: true,
		providerID:      "603",
		providerName:    "Tmdb",
	}
	refresher := cassetteBackfillRefresher(cassette, backfillLocal)
	newBackfillScannerWithRefresher(t, harness, refresher)
	if _, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil); err != nil {
		t.Fatalf("cassette backfill with locked fields: %v", err)
	}
	locked := scannerItemAtPath(t, harness.items, matrix)
	if locked.Overview == nil || *locked.Overview != lockedOverview {
		t.Fatalf("locked Overview = %v, want the locked value %q — a locked field must survive the cassette backfill", locked.Overview, lockedOverview)
	}
	// Compare the complete lock state, not only the presence of Overview.
	// The seed locks only the Overview field and leaves IsLocked false, so
	// an exact comparison fails when an extra lock field slips through.
	if locked.IsLocked {
		t.Fatalf("locked item IsLocked = true, want false — the seed leaves IsLocked false; IsLocked=%v LockedFields=%v", locked.IsLocked, locked.LockedFields)
	}
	wantLockedFields := []item.MetadataField{item.MetadataFieldOverview}
	if !reflect.DeepEqual(locked.LockedFields, wantLockedFields) {
		t.Fatalf("locked item LockedFields = %v, want %v — the seed locks only the Overview field and the exact lock set must survive the cassette backfill", locked.LockedFields, wantLockedFields)
	}
}

// TestRefreshExistingMetadataCassetteMixedPlanPreservesAndInserts proves the
// remote-backfill mode splits one folder plan by stored identity without
// losing either half when the remote stage is the real TMDb provider. The
// test seeds one movie with local probe and sidecar fields, adds a second
// never-scanned movie, then runs a backfill against a cassette that omits all
// probe and sidecar fields. The existing movie must keep its stored fields
// and identity. The absent movie must insert through the ordinary reconcile
// path with the full candidate from the cassette. No stored item is deleted.
func TestRefreshExistingMetadataCassetteMixedPlanPreservesAndInserts(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	seed := &probeFieldRefresher{
		overview:     "A probe-sourced overview from embedded metadata",
		runtimeTicks: int64Ptr(12345678900),
		genres:       []string{"Action", "Sci-Fi"},
		studios:      []string{"Warner Bros"},
		tags:         []string{"4k", "hdr"},
		people: []item.PersonCredit{
			{Name: "Lana Wachowski", Type: item.PersonKindDirector},
			{Name: "Keanu Reeves", Type: item.PersonKindActor, Role: stringPtrHelper("Neo")},
		},
		providerID:   "603",
		providerName: "Tmdb",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	seeded := scannerItemAtPath(t, harness.items, matrix)
	assertProbeFields(t, seeded, "seed")

	sequel := scannerPath(t, harness.source, "the-matrix-reloaded.mkv")
	writeScannerFile(t, sequel, "the matrix reloaded")

	// Enable internet providers and switch to the real TMDb provider backed
	// by a minimal cassette. The composite refresher first sets the TMDb
	// provider ID on both desired items, then runs the registry. The cassette
	// response for /movie/603 omits all probe and sidecar fields.
	enableInternetProviders(t, harness)
	backfillLocal := &probeFieldRefresher{
		omitProbeFields: true,
		providerID:      "603",
		providerName:    "Tmdb",
	}
	refresher := cassetteBackfillRefresher(minimalMovieCassette(), backfillLocal)
	newBackfillScannerWithRefresher(t, harness, refresher)
	result, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil)
	if err != nil {
		t.Fatalf("cassette backfill over mixed plan: %v", err)
	}

	// The seeded movie keeps its stored fields and identity.
	preserved := scannerItemAtPath(t, harness.items, matrix)
	if preserved.ID != seeded.ID {
		t.Fatalf("preserved ID = %s, want %s — the backfill must keep the stored identity", preserved.ID, seeded.ID)
	}
	if preserved.Overview == nil || *preserved.Overview != "A probe-sourced overview from embedded metadata" {
		t.Fatalf("preserved Overview = %v, want the seed value — the cassette omitted overview so the backfill must preserve it", preserved.Overview)
	}
	if preserved.RunTimeTicks == nil || *preserved.RunTimeTicks != 12345678900 {
		t.Fatalf("preserved RunTimeTicks = %v, want 12345678900 — the cassette omitted runtime so the backfill must preserve it", preserved.RunTimeTicks)
	}
	if !equalStringSlice(preserved.Values.Genres, []string{"Action", "Sci-Fi"}) {
		t.Fatalf("preserved Genres = %v, want [Action, Sci-Fi] — the cassette omitted genres so the backfill must preserve them", preserved.Values.Genres)
	}
	if !equalStringSlice(preserved.Values.Studios, []string{"Warner Bros"}) {
		t.Fatalf("preserved Studios = %v, want [Warner Bros] — the cassette omitted studios so the backfill must preserve them", preserved.Values.Studios)
	}
	if !reflect.DeepEqual(preserved.People, seeded.People) {
		t.Fatalf("preserved People = %#v, want %#v — the cassette omitted people so the backfill must preserve the complete people slice", preserved.People, seeded.People)
	}

	// The absent movie inserted through the ordinary reconcile path.
	inserted := scannerItemAtPath(t, harness.items, sequel)
	if inserted.ID.IsNil() || inserted.ID == seeded.ID {
		t.Fatalf("inserted ID = %s, want a fresh ID distinct from %s — the absent item must insert through reconcile", inserted.ID, seeded.ID)
	}
	if inserted.Kind != item.KindMovie {
		t.Fatalf("inserted Kind = %s, want Movie — the absent item must insert with its full candidate", inserted.Kind)
	}

	// No existing item was deleted by the mixed-plan reconcile.
	existing, err := harness.items.ReconciliationChildren(t.Context(), *seeded.ParentID)
	if err != nil {
		t.Fatalf("read children after mixed-plan backfill: %v", err)
	}
	if len(existing) != 2 {
		t.Fatalf("child count after mixed-plan backfill = %d, want 2 — the reconcile must not delete an existing sibling", len(existing))
	}

	// The backfill counters report two stored items updated and zero skipped.
	// The stored root Folder and the stored movie both route to MetadataBackfill
	// with ForceRefresh, so both count as updated. The absent candidate inserted
	// through ordinary reconciliation and does not count as a backfill update
	// or skip.
	if result.BackfillUpdated != 2 {
		t.Fatalf("BackfillUpdated = %d, want 2 — the stored root Folder and the stored movie were both updated by the backfill", result.BackfillUpdated)
	}
	if result.BackfillSkipped != 0 {
		t.Fatalf("BackfillSkipped = %d, want 0 — no stored item was skipped in the mixed plan", result.BackfillSkipped)
	}
}

// TestRefreshExistingMetadataCassettePreservesSidecarNames proves the
// metadata-backfill operation preserves a stored sidecar Name and SortName
// that differ from the resolver-derived filename when the remote cassette
// omits both fields. The test seeds an item with a sidecar Name and SortName
// set by the local refresher, then runs a backfill whose refresher omits all
// probe fields and the cassette omits Name and SortName. The resolver
// re-derives Name from the filename, so the scanner must restore the stored
// sidecar values before the store merge. The mirror case in the next test
// proves the other direction: a genuine provider name does replace an
// unlocked stored name. Both directions are asserted in the same run.
func TestRefreshExistingMetadataCassettePreservesSidecarNames(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	storedName := "The Matrix Collection Edition"
	storedSortName := "Matrix Collection, The"

	// Seed with a sidecar Name and SortName that differ from the filename.
	// The resolver derives "the-matrix" from the filename; the local
	// refresher overwrites it with the sidecar values.
	seed := &probeFieldRefresher{
		overview:        "A probe-sourced overview",
		sidecarName:     storedName,
		sidecarSortName: storedSortName,
		providerID:      "999",
		providerName:    "Tmdb",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	seeded := scannerItemAtPath(t, harness.items, matrix)
	if seeded.Name == nil || *seeded.Name != storedName {
		t.Fatalf("seed Name = %v, want %q — the seed must set the sidecar name", seeded.Name, storedName)
	}
	if seeded.SortName == nil || *seeded.SortName != storedSortName {
		t.Fatalf("seed SortName = %v, want %q — the seed must set the sidecar sort name", seeded.SortName, storedSortName)
	}

	// Backfill: the refresher omits all probe fields and the cassette omits
	// Name and SortName. The resolver re-derives Name from the filename, so
	// the scanner must restore the stored sidecar values.
	enableInternetProviders(t, harness)
	backfillLocal := &probeFieldRefresher{
		omitProbeFields: true,
		providerID:      "603",
		providerName:    "Tmdb",
	}
	refresher := cassetteBackfillRefresher(minimalMovieCassetteNoTitle(), backfillLocal)
	newBackfillScannerWithRefresher(t, harness, refresher)
	if _, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil); err != nil {
		t.Fatalf("cassette backfill with sidecar names: %v", err)
	}
	backfilled := scannerItemAtPath(t, harness.items, matrix)
	if backfilled.Name == nil || *backfilled.Name != storedName {
		t.Fatalf("backfilled Name = %v, want %q — the backfill must preserve the stored sidecar name when the remote omits it", backfilled.Name, storedName)
	}
	if backfilled.SortName == nil || *backfilled.SortName != storedSortName {
		t.Fatalf("backfilled SortName = %v, want %q — the backfill must preserve the stored sidecar sort name when the remote omits it", backfilled.SortName, storedSortName)
	}
}

// TestRefreshExistingMetadataCassetteEnrichesUnlockedName proves the mirror
// case: a genuine non-empty provider-supplied name replaces an unlocked
// stored name during the metadata backfill. The test seeds an item with a
// filename-derived name, then runs a backfill against the full the-matrix.json
// cassette, which supplies a title. The remote title must replace the stored
// filename. This test and the previous one assert both directions in the same
// run, so the next round does not reopen whichever one is missing.
func TestRefreshExistingMetadataCassetteEnrichesUnlockedName(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	// Seed with a filename-derived name. The resolver sets Name to
	// "the-matrix".
	seed := &probeFieldRefresher{
		overview:     "A probe-sourced overview",
		providerID:   "603",
		providerName: "Tmdb",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	seeded := scannerItemAtPath(t, harness.items, matrix)
	if seeded.Name == nil || *seeded.Name != "the-matrix" {
		t.Fatalf("seed Name = %v, want the filename-derived name %q", seeded.Name, "the-matrix")
	}

	// Backfill against the full the-matrix.json cassette, which supplies a
	// title. The remote title must replace the stored filename.
	enableInternetProviders(t, harness)
	cassette, err := remotetest.Load("../providers/tmdb/testdata/the-matrix.json")
	if err != nil {
		t.Fatalf("load cassette: %v", err)
	}
	backfillLocal := &probeFieldRefresher{
		omitProbeFields: true,
		providerID:      "603",
		providerName:    "Tmdb",
	}
	refresher := cassetteBackfillRefresher(cassette, backfillLocal)
	newBackfillScannerWithRefresher(t, harness, refresher)
	if _, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil); err != nil {
		t.Fatalf("cassette backfill with provider name: %v", err)
	}
	enriched := scannerItemAtPath(t, harness.items, matrix)
	if enriched.Name == nil || *enriched.Name != "The Matrix" {
		t.Fatalf("enriched Name = %v, want the cassette title %q — a non-empty provider name must replace an unlocked stored name", enriched.Name, "The Matrix")
	}
}

// TestRefreshExistingMetadataCassetteProviderErrorKeepsItem proves a remote
// provider error from the real TMDb cassette (HTTP 503 on the movie fetch) is
// logged, not propagated, as upstream MetadataService.ExecuteRemoteProviders
// does. The folder does not fail and the stored item keeps its seeded
// values, because the failed provider changed nothing.
func TestRefreshExistingMetadataCassetteProviderErrorKeepsItem(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	// Seed a complete item with a TMDb provider ID so the provider skips the
	// search step and fetches /movie/603 directly.
	seed := &probeFieldRefresher{
		overview:     "A probe-sourced overview",
		providerID:   "603",
		providerName: "Tmdb",
		remoteImage:  "https://image.tmdb.org/t/p/original/seed.jpg",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	// Capture the complete stored item before the failed refresh so the
	// rollback assertion compares every field, not only Overview and the
	// provider ID.
	seeded := scannerItemAtPath(t, harness.items, matrix)

	// Enable internet providers and switch to the real TMDb provider backed
	// by a cassette that returns HTTP 503 on the movie fetch. The composite
	// refresher first sets the TMDb provider ID on the desired item, then
	// runs the registry, which fetches /movie/603 and receives the 503.
	enableInternetProviders(t, harness)
	errorCassette := remotetest.NewCassette(
		remotetest.Interaction{
			Method: http.MethodGet,
			URL:    "https://api.themoviedb.org/3/configuration",
			Status: http.StatusOK,
			Header: map[string][]string{"Content-Type": {"application/json"}},
			Body:   []byte(`{"images":{"secure_base_url":"https://image.tmdb.org/t/p/"}}`),
		},
		remotetest.Interaction{
			Method: http.MethodGet,
			URL:    "https://api.themoviedb.org/3/movie/603?language=en&include_image_language=en,null&append_to_response=credits,release_dates,images,videos,keywords",
			Status: http.StatusServiceUnavailable,
			Header: map[string][]string{"Content-Type": {"application/json"}},
			Body:   []byte(`{"status_message":"service unavailable"}`),
		},
	)
	backfillLocal := &probeFieldRefresher{
		omitProbeFields: true,
		providerID:      "603",
		providerName:    "Tmdb",
	}
	refresher := cassetteBackfillRefresher(errorCassette, backfillLocal)
	newBackfillScannerWithRefresher(t, harness, refresher)
	result, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil)

	// The 503 is logged by the registry. No folder failure carries it.
	joined := errors.Join(append([]error{err}, result.Errors...)...)
	var statusErr *remote.StatusError
	if errors.As(joined, &statusErr) {
		t.Fatalf("RefreshExistingMetadata error = %v, want the remote 503 logged, not propagated", joined)
	}

	// The complete stored item must equal the snapshot captured before the
	// failed refresh: the failed provider changed nothing.
	kept := scannerItemAtPath(t, harness.items, matrix)
	if !reflect.DeepEqual(kept, seeded) {
		t.Fatalf("stored item = %#v, want the exact seeded item %#v — a failed remote provider must change nothing", kept, seeded)
	}
}

// blockingCassetteDoer is a remotetest.Cassette-like Doer that blocks on the
// first request to /movie/ until the test releases it or the context is
// cancelled. This lets the test cancel the refresh while the provider is
// inside the HTTP call, so the test can prove the cancellation rolls back the
// transaction and the refresh reports a failure.
type blockingCassetteDoer struct {
	configuration remotetest.Interaction
	entered       chan struct{}
	// enterOnce closes entered once. Do never reassigns entered, so the
	// test goroutine can read the field without a race.
	enterOnce sync.Once
}

func (d *blockingCassetteDoer) Do(request *http.Request) (*http.Response, error) {
	if strings.Contains(request.URL.Path, "/configuration") {
		header := make(http.Header)
		for name, values := range d.configuration.Header {
			header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
		return &http.Response{
			StatusCode: d.configuration.Status,
			Header:     header,
			Body:       io.NopCloser(bytes.NewReader(d.configuration.Body)),
			Request:    request,
		}, nil
	}
	// Block on the movie fetch until the context cancels.
	d.enterOnce.Do(func() { close(d.entered) })
	<-request.Context().Done()
	return nil, context.Canceled
}

// TestRefreshExistingMetadataCassetteCancellationRollsBack proves context
// cancellation during the provider HTTP call rolls back the current item
// transaction and the refresh reports a failure. The test starts the
// refresh in a goroutine, cancels the context while the provider is inside
// the HTTP call, and asserts the stored item keeps its seeded values and the
// error includes the folder path and the cancellation cause.
func TestRefreshExistingMetadataCassetteCancellationRollsBack(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	matrix := scannerPath(t, harness.source, "the-matrix.mkv")
	writeScannerFile(t, matrix, "the matrix")

	// Seed a complete item with a TMDb provider ID so the provider skips
	// the search step and fetches /movie/603 directly.
	seed := &probeFieldRefresher{
		overview:     "A probe-sourced overview",
		providerID:   "603",
		providerName: "Tmdb",
		remoteImage:  "https://image.tmdb.org/t/p/original/seed.jpg",
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	// Capture the complete stored item before the failed refresh so the
	// rollback assertion compares every field, not only Overview and the
	// provider ID.
	seeded := scannerItemAtPath(t, harness.items, matrix)

	// Enable internet providers and switch to the real TMDb provider backed
	// by a blocking doer. The doer answers /configuration and then blocks on
	// /movie/603 until the context cancels. The composite refresher first
	// sets the TMDb provider ID on the desired item, then runs the registry.
	enableInternetProviders(t, harness)
	blocker := &blockingCassetteDoer{
		configuration: remotetest.Interaction{
			Method: http.MethodGet,
			URL:    "https://api.themoviedb.org/3/configuration",
			Status: http.StatusOK,
			Header: map[string][]string{"Content-Type": {"application/json"}},
			Body:   []byte(`{"images":{"secure_base_url":"https://image.tmdb.org/t/p/"}}`),
		},
		entered: make(chan struct{}),
	}
	config := tmdb.DefaultConfig()
	config.APIKey = "test-key"
	provider := tmdb.NewProvider(tmdb.Options{
		HTTP:   remote.NewClient(remote.Config{Doer: blocker, CacheEntries: 32}),
		Config: config,
	})
	registry := providers.NewRegistry(provider)
	backfillLocal := &probeFieldRefresher{
		omitProbeFields: true,
		providerID:      "603",
		providerName:    "Tmdb",
	}
	refresher := multiRefresher{refreshers: []library.Refresher{backfillLocal, registry}}
	newBackfillScannerWithRefresher(t, harness, refresher)

	// Start the refresh in a goroutine with a cancellable context.
	ctx, cancel := context.WithCancel(t.Context())
	type refreshOutcome struct {
		result library.RefreshResult
		err    error
	}
	done := make(chan refreshOutcome, 1)
	go func() {
		result, err := harness.scanner.RefreshExistingMetadata(ctx, nil)
		done <- refreshOutcome{result: result, err: err}
	}()

	// Wait until the provider entered the blocking HTTP call, then cancel.
	<-blocker.entered
	cancel()

	// Wait for the refresh to complete with a timeout.
	select {
	case outcome := <-done:
		joined := errors.Join(append([]error{outcome.err}, outcome.result.Errors...)...)
		if joined == nil {
			t.Fatalf("RefreshExistingMetadata with cancellation returned no error")
		}
		// The error must include the folder path.
		if !containsString(joined.Error(), harness.source) {
			t.Fatalf("RefreshExistingMetadata error = %q, want it to contain the folder path %q", joined.Error(), harness.source)
		}
		// The error must include the cancellation cause.
		if !errors.Is(joined, context.Canceled) {
			t.Fatalf("RefreshExistingMetadata error = %v, want it to wrap context.Canceled", joined)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("RefreshExistingMetadata with cancellation did not complete within 30s")
	}

	// The complete stored item must equal the snapshot captured before the
	// failed refresh — the transaction rolled back.
	rolled := scannerItemAtPath(t, harness.items, matrix)
	if !reflect.DeepEqual(rolled, seeded) {
		t.Fatalf("rolled back item = %#v, want the exact seeded item %#v — the transaction must roll back every field on cancellation", rolled, seeded)
	}
}

// scannerRootFolder returns the root Folder item that is the direct child of
// the collection folder. The harness has one virtual folder with one media
// root, so the collection folder has exactly one root Folder child.
func scannerRootFolder(t *testing.T, harness *scannerHarness) item.Item {
	t.Helper()
	collection := scannerCollectionFolder(t, harness)
	children, err := harness.items.ReconciliationChildren(t.Context(), collection.ID)
	if err != nil {
		t.Fatalf("read collection children: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("collection has %d children, want 1 root Folder", len(children))
	}
	return children[0]
}

// assertChildIDsEqual asserts two ID slices contain the same IDs in any order.
func assertChildIDsEqual(t *testing.T, got, want []dto.UUID) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("child IDs = %v (count %d), want %v (count %d)", got, len(got), want, len(want))
	}
	gotSet := make(map[dto.UUID]struct{}, len(got))
	for _, id := range got {
		gotSet[id] = struct{}{}
	}
	for _, id := range want {
		if _, found := gotSet[id]; !found {
			t.Fatalf("child IDs = %v, missing %v", got, id)
		}
	}
}

// scannerRootChildIDs returns the sorted set of direct-child item IDs under the
// root Folder.
func scannerRootChildIDs(t *testing.T, harness *scannerHarness) []dto.UUID {
	t.Helper()
	root := scannerRootFolder(t, harness)
	children, err := harness.items.ReconciliationChildren(t.Context(), root.ID)
	if err != nil {
		t.Fatalf("read root Folder children: %v", err)
	}
	ids := make([]dto.UUID, len(children))
	for index, child := range children {
		ids[index] = child.ID
	}
	return ids
}

// hasRemotePrimaryImage reports whether an item has a primary image with the
// given remote path. The test uses this to prove the backfill enriched the
// stored candidate with a remote image the seed scan did not supply.
func hasRemotePrimaryImage(value item.Item, path string) bool {
	for _, image := range value.ImageInfos {
		if image.Type == item.ImageTypePrimary && image.Path == path {
			return true
		}
	}
	return false
}

// TestRefreshExistingMetadataMixedPlanPreservesUnplannedChild proves a mixed
// backfill plan inserts the absent candidate, enriches the stored candidate,
// and keeps every pre-existing unplanned direct child. The test seeds two
// movies through a normal scan, removes one file before the backfill, then
// adds a third never-scanned file. The backfill plan mixes one stored item
// and one absent item. The removed movie is an unplanned stored child the
// backfill must not delete.
func TestRefreshExistingMetadataMixedPlanPreservesUnplannedChild(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	movieA := scannerPath(t, harness.source, "movie-a.mkv")
	movieB := scannerPath(t, harness.source, "movie-b.mkv")
	writeScannerFile(t, movieA, "movie a")
	writeScannerFile(t, movieB, "movie b")

	seed := &probeFieldRefresher{providerID: "1", providerName: "Tmdb"}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	storedA := scannerItemAtPath(t, harness.items, movieA)
	storedB := scannerItemAtPath(t, harness.items, movieB)

	if err := os.Remove(movieB); err != nil {
		t.Fatalf("remove movie B before backfill: %v", err)
	}
	movieC := scannerPath(t, harness.source, "movie-c.mkv")
	writeScannerFile(t, movieC, "movie c")

	backfill := &probeFieldRefresher{omitProbeFields: true, providerID: "1", providerName: "Tmdb", remoteImage: "https://image.tmdb.org/t/p/original/a.jpg"}
	newBackfillScannerWithRefresher(t, harness, backfill)
	result, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil)
	if err != nil {
		t.Fatalf("backfill over mixed plan: %v", err)
	}
	if result.ProviderInvocations == 0 {
		t.Fatalf("ProviderInvocations = 0, want at least one provider call on the mixed plan")
	}

	keptA := scannerItemAtPath(t, harness.items, movieA)
	if keptA.ID != storedA.ID {
		t.Fatalf("stored movie A ID changed: %s, want %s", keptA.ID, storedA.ID)
	}
	// The stored candidate A received the remote image from the backfill,
	// which proves the backfill enriched the stored candidate.
	if !hasRemotePrimaryImage(keptA, "https://image.tmdb.org/t/p/original/a.jpg") {
		t.Fatalf("stored movie A was not enriched by the backfill — want remote primary image %q, got %#v", "https://image.tmdb.org/t/p/original/a.jpg", keptA.ImageInfos)
	}
	if _, err := harness.items.Get(t.Context(), storedB.ID); err != nil {
		t.Fatalf("unplanned movie B %s was deleted by the backfill — the enrichment-only contract must preserve it", storedB.ID)
	}
	insertedC := scannerItemAtPath(t, harness.items, movieC)
	if insertedC.ID.IsNil() {
		t.Fatalf("absent movie C did not insert through the ordinary reconcile path")
	}
	assertChildIDsEqual(t, scannerRootChildIDs(t, harness), []dto.UUID{storedA.ID, storedB.ID, insertedC.ID})
}

// TestRefreshExistingMetadataAllStoredPlanPreservesStaleChild proves an
// all-stored backfill plan does not delete a stale direct child. The test
// seeds two movies, removes one file before the backfill, then runs a
// backfill whose plan contains only the remaining stored item. The removed
// movie is an unplanned stored child the all-stored plan must not delete.
func TestRefreshExistingMetadataAllStoredPlanPreservesStaleChild(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	movieA := scannerPath(t, harness.source, "movie-a.mkv")
	movieB := scannerPath(t, harness.source, "movie-b.mkv")
	writeScannerFile(t, movieA, "movie a")
	writeScannerFile(t, movieB, "movie b")

	seed := &probeFieldRefresher{providerID: "1", providerName: "Tmdb"}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	storedA := scannerItemAtPath(t, harness.items, movieA)
	storedB := scannerItemAtPath(t, harness.items, movieB)

	if err := os.Remove(movieB); err != nil {
		t.Fatalf("remove movie B before backfill: %v", err)
	}

	backfill := &probeFieldRefresher{omitProbeFields: true, providerID: "1", providerName: "Tmdb", remoteImage: "https://image.tmdb.org/t/p/original/a.jpg"}
	newBackfillScannerWithRefresher(t, harness, backfill)
	result, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil)
	if err != nil {
		t.Fatalf("backfill over all-stored plan: %v", err)
	}
	if result.ProviderInvocations == 0 {
		t.Fatalf("ProviderInvocations = 0, want at least one provider call on the all-stored plan")
	}

	keptA := scannerItemAtPath(t, harness.items, movieA)
	if keptA.ID != storedA.ID {
		t.Fatalf("stored movie A ID changed: %s, want %s", keptA.ID, storedA.ID)
	}
	// The stored candidate A received the remote image from the backfill,
	// which proves the backfill enriched the stored candidate.
	if !hasRemotePrimaryImage(keptA, "https://image.tmdb.org/t/p/original/a.jpg") {
		t.Fatalf("stored movie A was not enriched by the all-stored backfill — want remote primary image %q, got %#v", "https://image.tmdb.org/t/p/original/a.jpg", keptA.ImageInfos)
	}
	if _, err := harness.items.Get(t.Context(), storedB.ID); err != nil {
		t.Fatalf("unplanned movie B %s was deleted by an all-stored backfill — the enrichment-only contract must preserve it", storedB.ID)
	}
	assertChildIDsEqual(t, scannerRootChildIDs(t, harness), []dto.UUID{storedA.ID, storedB.ID})
}

// TestRefreshExistingMetadataEmptyPlanPreservesStaleChild proves an empty
// backfill plan does not delete a stale direct child. The test seeds one
// movie, removes the file before the backfill, then runs a backfill whose
// plan is empty because the scanner enumerates no files. The refresh returns
// a non-nil error because the empty plan made no provider invocation, but
// the stored movie must survive the empty backfill.
func TestRefreshExistingMetadataEmptyPlanPreservesStaleChild(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	movieA := scannerPath(t, harness.source, "movie-a.mkv")
	writeScannerFile(t, movieA, "movie a")

	seed := &probeFieldRefresher{providerID: "1", providerName: "Tmdb"}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	storedA := scannerItemAtPath(t, harness.items, movieA)

	if err := os.Remove(movieA); err != nil {
		t.Fatalf("remove movie A before backfill: %v", err)
	}

	backfill := &probeFieldRefresher{omitProbeFields: true, providerID: "1", providerName: "Tmdb", remoteImage: "https://image.tmdb.org/t/p/original/a.jpg"}
	newBackfillScannerWithRefresher(t, harness, backfill)
	result, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil)

	// The empty plan made no provider invocation, so the refresh reports a
	// non-nil error. This is the defined behavior for an empty backfill: the
	// task adapter rejects a false-success result that did no provider work.
	// The error does not mean the backfill deleted data. The enrichment-only
	// contract preserves the stored child regardless of the task result.
	if err == nil {
		t.Fatalf("RefreshExistingMetadata on an empty plan returned nil; want a non-nil error — no provider invocation must not report success")
	}
	if result.ProviderInvocations != 0 {
		t.Fatalf("ProviderInvocations = %d, want 0 on an empty plan", result.ProviderInvocations)
	}

	if _, err := harness.items.Get(t.Context(), storedA.ID); err != nil {
		t.Fatalf("unplanned movie A %s was deleted by an empty-plan backfill — the enrichment-only contract must preserve it", storedA.ID)
	}
	assertChildIDsEqual(t, scannerRootChildIDs(t, harness), []dto.UUID{storedA.ID})
}

// TestRefreshExistingMetadataPreservesUserDataAndSatellites proves a backfill
// preserves stored user-data and satellite rows for an unplanned preserved
// child. The test seeds two movies. Movie A has user data and a provider ID
// satellite. The test removes movie A before the backfill, so the backfill
// plan omits it. Movie B stays present, so the provider runs and the backfill
// reports nil error. The stored user data and the provider ID for movie A
// must survive.
func TestRefreshExistingMetadataPreservesUserDataAndSatellites(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	movieA := scannerPath(t, harness.source, "movie-a.mkv")
	movieB := scannerPath(t, harness.source, "movie-b.mkv")
	writeScannerFile(t, movieA, "movie a")
	writeScannerFile(t, movieB, "movie b")

	seed := &probeFieldRefresher{providerID: "603", providerName: "Tmdb", remoteImage: "https://image.tmdb.org/t/p/original/seed.jpg"}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	storedA := scannerItemAtPath(t, harness.items, movieA)
	storedB := scannerItemAtPath(t, harness.items, movieB)

	userID := dto.UUID{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	if _, err := harness.items.MutateUserDataForItem(t.Context(), userID, storedA.ID, func(data *store.UserData) error {
		data.Played = true
		data.PlayCount = 5
		return nil
	}); err != nil {
		t.Fatalf("seed user data: %v", err)
	}

	if err := os.Remove(movieA); err != nil {
		t.Fatalf("remove movie A before backfill: %v", err)
	}

	backfill := &probeFieldRefresher{omitProbeFields: true, providerID: "1", providerName: "Tmdb", remoteImage: "https://image.tmdb.org/t/p/original/a.jpg"}
	newBackfillScannerWithRefresher(t, harness, backfill)
	result, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil)
	if err != nil {
		t.Fatalf("backfill with a present sibling: %v", err)
	}
	if result.ProviderInvocations == 0 {
		t.Fatalf("ProviderInvocations = 0, want at least one provider call on the present sibling")
	}

	kept, err := harness.items.Get(t.Context(), storedA.ID)
	if err != nil {
		t.Fatalf("stored movie A was deleted by the backfill — the enrichment-only contract must preserve it")
	}
	if id, ok := kept.ProviderIDs.Get("Tmdb"); !ok || id != "603" {
		t.Fatalf("provider ID = %q,%t, want 603,true — the satellite must survive the backfill", id, ok)
	}
	var userData store.UserData
	if _, err := harness.items.MutateUserDataForItem(t.Context(), userID, storedA.ID, func(data *store.UserData) error {
		userData = *data
		return nil
	}); err != nil {
		t.Fatalf("read user data after backfill: %v", err)
	}
	if !userData.Played || userData.PlayCount != 5 {
		t.Fatalf("user data = %+v, want Played=true PlayCount=5 — the user data must survive the backfill", userData)
	}
	assertChildIDsEqual(t, scannerRootChildIDs(t, harness), []dto.UUID{storedA.ID, storedB.ID})
}

// TestRefreshExistingMetadataOrdinaryScanDeletesStaleChild proves a later
// ordinary scan retains its existing stale-child behavior. The test seeds two
// movies, runs a backfill that preserves both, then runs an ordinary scan with
// one file removed. The ordinary scan must delete the stale child.
func TestRefreshExistingMetadataOrdinaryScanDeletesStaleChild(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	movieA := scannerPath(t, harness.source, "movie-a.mkv")
	movieB := scannerPath(t, harness.source, "movie-b.mkv")
	writeScannerFile(t, movieA, "movie a")
	writeScannerFile(t, movieB, "movie b")

	seed := &probeFieldRefresher{providerID: "1", providerName: "Tmdb"}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	storedA := scannerItemAtPath(t, harness.items, movieA)
	storedB := scannerItemAtPath(t, harness.items, movieB)

	backfill := &probeFieldRefresher{omitProbeFields: true, providerID: "1", providerName: "Tmdb", remoteImage: "https://image.tmdb.org/t/p/original/a.jpg"}
	newBackfillScannerWithRefresher(t, harness, backfill)
	if _, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if _, err := harness.items.Get(t.Context(), storedB.ID); err != nil {
		t.Fatalf("backfill deleted movie B before the ordinary scan")
	}

	if err := os.Remove(movieB); err != nil {
		t.Fatalf("remove movie B before ordinary scan: %v", err)
	}
	ordinary := &probeFieldRefresher{providerID: "1", providerName: "Tmdb"}
	newBackfillScannerWithRefresher(t, harness, ordinary)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("ordinary scan: %v", err)
	}

	if _, err := harness.items.Get(t.Context(), storedB.ID); err == nil {
		t.Fatalf("stale movie B survived an ordinary scan — the ordinary scan must delete unplanned children")
	}
	if _, err := harness.items.Get(t.Context(), storedA.ID); err != nil {
		t.Fatalf("movie A was deleted by the ordinary scan — only the stale child must be removed")
	}
	assertChildIDsEqual(t, scannerRootChildIDs(t, harness), []dto.UUID{storedA.ID})
}

// TestRefreshExistingMetadataPreservesChildUnderInaccessibleRoot proves a
// child below an inaccessible root remains present after remote backfill.
// The test seeds two media roots. It stores one movie under each root through
// a normal scan. Then it makes the second root inaccessible by returning a
// permission error from ReadDir for that path. The backfill runs against the
// accessible root, so the provider invokes and the refresh reports nil error.
// The movie under the inaccessible root must survive the backfill. The movie
// under the accessible root must receive the remote enrichment. The refresh
// result reports one inaccessible root.
func TestRefreshExistingMetadataPreservesChildUnderInaccessibleRoot(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	secondSource := scannerPath(t, t.TempDir(), "second-media")
	if err := os.Mkdir(secondSource, 0o755); err != nil {
		t.Fatalf("create second media root: %v", err)
	}
	if err := harness.topology.AddMediaPath(t.Context(), harness.virtualID, secondSource); err != nil {
		t.Fatalf("add second media root: %v", err)
	}
	accessibleMovie := scannerPath(t, harness.source, "accessible.mkv")
	inaccessibleMovie := scannerPath(t, secondSource, "inaccessible.mkv")
	writeScannerFile(t, accessibleMovie, "accessible")
	writeScannerFile(t, inaccessibleMovie, "inaccessible")

	seed := &probeFieldRefresher{providerID: "1", providerName: "Tmdb"}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	storedAccessible := scannerItemAtPath(t, harness.items, accessibleMovie)
	storedInaccessible := scannerItemAtPath(t, harness.items, inaccessibleMovie)

	backfill := &probeFieldRefresher{omitProbeFields: true, providerID: "1", providerName: "Tmdb", remoteImage: "https://image.tmdb.org/t/p/original/a.jpg"}
	newBackfillScannerWithRefresherAndReadDir(t, harness, backfill, func(path string) ([]library.Entry, error) {
		if path == secondSource {
			return nil, fs.ErrPermission
		}
		return scannerReadDir(path)
	})
	result, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil)
	if err != nil {
		t.Fatalf("backfill with an inaccessible root: %v", err)
	}
	if result.ProviderInvocations == 0 {
		t.Fatalf("ProviderInvocations = 0, want at least one provider call on the accessible root")
	}
	if result.Inaccessible != 1 {
		t.Fatalf("Inaccessible = %d, want 1 — the inaccessible root must report its status", result.Inaccessible)
	}

	// The child under the inaccessible root survives the backfill.
	keptInaccessible, err := harness.items.Get(t.Context(), storedInaccessible.ID)
	if err != nil {
		t.Fatalf("child under inaccessible root %s was deleted by the backfill — the enrichment-only contract must preserve it", storedInaccessible.ID)
	}
	if keptInaccessible.ID != storedInaccessible.ID {
		t.Fatalf("inaccessible child ID changed: %s, want %s", keptInaccessible.ID, storedInaccessible.ID)
	}

	// The child under the accessible root receives the remote enrichment.
	keptAccessible := scannerItemAtPath(t, harness.items, accessibleMovie)
	if keptAccessible.ID != storedAccessible.ID {
		t.Fatalf("accessible child ID changed: %s, want %s", keptAccessible.ID, storedAccessible.ID)
	}
	if !hasRemotePrimaryImage(keptAccessible, "https://image.tmdb.org/t/p/original/a.jpg") {
		t.Fatalf("accessible child was not enriched by the backfill — want remote primary image %q, got %#v", "https://image.tmdb.org/t/p/original/a.jpg", keptAccessible.ImageInfos)
	}

	// The exact item IDs and child sets under each root survive.
	accessibleRoot := scannerItemAtPath(t, harness.items, harness.source)
	accessibleChildren, err := harness.items.ReconciliationChildren(t.Context(), accessibleRoot.ID)
	if err != nil {
		t.Fatalf("read accessible root children: %v", err)
	}
	assertChildIDsEqual(t, childIDs(accessibleChildren), []dto.UUID{storedAccessible.ID})

	inaccessibleRoot := scannerItemAtPath(t, harness.items, secondSource)
	inaccessibleChildren, err := harness.items.ReconciliationChildren(t.Context(), inaccessibleRoot.ID)
	if err != nil {
		t.Fatalf("read inaccessible root children: %v", err)
	}
	assertChildIDsEqual(t, childIDs(inaccessibleChildren), []dto.UUID{storedInaccessible.ID})
}

// childIDs extracts the ID slice from a set of items.
func childIDs(items []item.Item) []dto.UUID {
	ids := make([]dto.UUID, len(items))
	for index, child := range items {
		ids[index] = child.ID
	}
	return ids
}

// TestRefreshExistingMetadataKeepsCaseVariantVirtualPaths proves a Person or
// Studio credit whose name differs only in case from the stored entity does
// not fail the folder (#748). The entity ID comes from the lowercased name,
// so the credit resolves to the stored row with a different literal path.
// The backfill keeps the stored path and still persists the folder's provider
// enrichment.
func TestRefreshExistingMetadataKeepsCaseVariantVirtualPaths(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	movie := scannerPath(t, harness.source, "crouching-tiger.mkv")
	writeScannerFile(t, movie, "crouching tiger")

	seed := &probeFieldRefresher{
		studios: []string{"Starz"},
		people:  []item.PersonCredit{{Name: "Cheng Pei-Pei", Type: item.PersonKindActor}},
	}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	person := library.PersonEntity("Cheng Pei-Pei")
	storedPerson, err := harness.items.Get(t.Context(), person.ID)
	if err != nil {
		t.Fatalf("load seeded Person: %v", err)
	}
	var storedStudio item.Item
	for _, candidate := range scannerItems(t, harness.items) {
		if candidate.Kind == item.KindStudio {
			storedStudio = candidate
		}
	}
	if storedStudio.ID.IsNil() {
		t.Fatalf("seed scan stored no Studio")
	}

	backfill := &probeFieldRefresher{
		studios:      []string{"STARZ"},
		people:       []item.PersonCredit{{Name: "cheng pei-pei", Type: item.PersonKindActor}},
		providerID:   "146",
		providerName: "Tmdb",
		remoteImage:  "https://image.tmdb.org/t/p/original/tiger.jpg",
	}
	newBackfillScannerWithRefresher(t, harness, backfill)
	result, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil)
	if err != nil {
		t.Fatalf("backfill with case-variant credits: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("backfill errors = %v, want none — a case-variant credit must not fail the folder", result.Errors)
	}
	if result.PersistedProviderIDs == 0 || result.PersistedImages == 0 {
		t.Fatalf("persisted provider IDs = %d, images = %d, want both > 0", result.PersistedProviderIDs, result.PersistedImages)
	}
	enriched := scannerItemAtPath(t, harness.items, movie)
	if id, ok := enriched.ProviderIDs.Get("Tmdb"); !ok || id != "146" {
		t.Fatalf("movie Tmdb id = %q,%t, want 146,true", id, ok)
	}
	if !hasRemotePrimaryImage(enriched, "https://image.tmdb.org/t/p/original/tiger.jpg") {
		t.Fatalf("movie ImageInfos = %#v, want the remote primary image", enriched.ImageInfos)
	}
	afterPerson, err := harness.items.Get(t.Context(), person.ID)
	if err != nil {
		t.Fatalf("load Person after backfill: %v", err)
	}
	if !reflect.DeepEqual(afterPerson.Path, storedPerson.Path) {
		t.Fatalf("Person Path = %v, want the stored %v", afterPerson.Path, storedPerson.Path)
	}
	afterStudio, err := harness.items.Get(t.Context(), storedStudio.ID)
	if err != nil {
		t.Fatalf("load Studio after backfill: %v", err)
	}
	if !reflect.DeepEqual(afterStudio.Path, storedStudio.Path) {
		t.Fatalf("Studio Path = %v, want the stored %v", afterStudio.Path, storedStudio.Path)
	}
}

// TestRefreshExistingMetadataAdoptsStaleCaseVariantChild proves a stored row
// that a removed case-variant folder left behind does not fail its surviving
// sibling (#748). The row shares the scanned child's case-insensitive ID but
// names the removed folder as its parent. The backfill adopts it under the
// scanned folder, as an ordinary scan does, and enriches the folder.
func TestRefreshExistingMetadataAdoptsStaleCaseVariantChild(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	album := scannerPath(t, harness.source, "Red (2021)")
	disc := scannerPath(t, album, "Digital Media 02")
	track := scannerPath(t, disc, "track.mkv")
	writeScannerFile(t, track, "track")

	seed := &probeFieldRefresher{providerID: "1", providerName: "Tmdb"}
	newBackfillScannerWithRefresher(t, harness, seed)
	if _, err := harness.scanner.ScanWithOptions(t.Context(), library.DefaultRefreshOptions()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	storedAlbum := scannerItemAtPath(t, harness.items, album)
	storedDisc := scannerItemAtPath(t, harness.items, disc)

	// Simulate the removed twin: a stale folder row and the disc row stored
	// under it with the twin's path.
	twinPath := scannerPath(t, harness.source, "Red (2021) twin")
	twin := storedAlbum
	twin.ID = scannerID(twinPath)
	twin.Path = scannerString(twinPath)
	twin.Name = scannerString("Red (2021) twin")
	if err := harness.items.Upsert(t.Context(), twin); err != nil {
		t.Fatalf("seed stale twin folder: %v", err)
	}
	stale := storedDisc
	stale.ParentID = &twin.ID
	stale.Path = scannerString(scannerPath(t, twinPath, "Digital Media 02"))
	if err := harness.items.Upsert(t.Context(), stale); err != nil {
		t.Fatalf("move disc under the stale twin: %v", err)
	}

	backfill := &probeFieldRefresher{omitProbeFields: true, providerID: "1", providerName: "Tmdb", remoteImage: "https://image.tmdb.org/t/p/original/red.jpg"}
	newBackfillScannerWithRefresher(t, harness, backfill)
	result, err := harness.scanner.RefreshExistingMetadata(t.Context(), nil)
	if err != nil {
		t.Fatalf("backfill with a stale case-variant child: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("backfill errors = %v, want none — a stale child must not fail its surviving sibling", result.Errors)
	}
	adopted, err := harness.items.Get(t.Context(), storedDisc.ID)
	if err != nil {
		t.Fatalf("load disc after backfill: %v", err)
	}
	if adopted.ParentID == nil || *adopted.ParentID != storedAlbum.ID {
		t.Fatalf("disc ParentID = %v, want the scanned album %s", adopted.ParentID, storedAlbum.ID)
	}
	if adopted.Path == nil || *adopted.Path != disc {
		t.Fatalf("disc Path = %v, want %q", adopted.Path, disc)
	}
	if !hasRemotePrimaryImage(scannerItemAtPath(t, harness.items, track), "https://image.tmdb.org/t/p/original/red.jpg") {
		t.Fatalf("track under the adopted disc was not enriched")
	}
}
