// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/jfimport"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/naming"
	"github.com/deadfin-org/deadfin/internal/playlists"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/jellyfinsource"
	"github.com/deadfin-org/deadfin/internal/trickplay"
)

// importJellyfin runs the one-way Jellyfin import (D3). It reads a copy of
// Jellyfin's jellyfin.db read-only, imports users, device tokens, API keys,
// watch state, playlists, and collections, and matches the watch state to the items Deadfin's scan
// found. With --jellyfin-root it first copies Jellyfin library definitions
// that Deadfin lacks; scan those libraries before importing watch state.
// Stop the server first. The command is safe to rerun.
//
// Usage: deadfin import-jellyfin --jellyfin-db /path/jellyfin.db [--jellyfin-root /path/root/default] [--dry-run]
func importJellyfin(args []string) {
	flags := flag.NewFlagSet("import-jellyfin", flag.ExitOnError)
	jellyfinDB := flags.String("jellyfin-db", "", "path to a copy of Jellyfin's data/jellyfin.db (opened read-only)")
	jellyfinRoot := flags.String("jellyfin-root", "", "optional: Jellyfin's root/default directory; copy library definitions Deadfin lacks")
	dryRun := flags.Bool("dry-run", false, "report what would be imported and write nothing")
	programData := flags.String("program-data", "", programDataUsage)
	databaseURL := flags.String("database-url", "", databaseURLUsage)
	if err := flags.Parse(args); err != nil {
		log.Fatal(err)
	}
	if *jellyfinDB == "" && *jellyfinRoot == "" {
		log.Fatal("usage: deadfin import-jellyfin --jellyfin-db <jellyfin.db> [--jellyfin-root <root/default>] [--dry-run] [--program-data <root>] [--database-url <url>]")
	}
	if err := runImportJellyfin(context.Background(), os.Stdout, resolveProgramData(*programData, os.Getenv), resolveDatabaseURL(*databaseURL, os.Getenv(databaseURLEnv)), *jellyfinDB, *jellyfinRoot, *dryRun); err != nil {
		log.Fatal(err)
	}
}

func runImportJellyfin(ctx context.Context, out io.Writer, programData, databaseURL, jellyfinDB, jellyfinRoot string, dryRun bool) (err error) {
	paths, err := config.NewPaths(programData)
	if err != nil {
		return fmt.Errorf("derive program-data paths: %w", err)
	}
	if jellyfinRoot != "" {
		if err := copyJellyfinLibraries(out, jellyfinRoot, filepath.Join(paths.Root, "root", "default"), dryRun); err != nil {
			return err
		}
	}
	if jellyfinDB == "" {
		return nil
	}
	src, err := jellyfinsource.Open(ctx, jellyfinDB)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := src.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	stores, _, err := openProgramDatabase(ctx, databaseURL, paths.Data)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := stores.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close stores: %w", closeErr)
		}
	}()
	identity, items := store.NewIdentity(stores), store.NewItemStore(stores)
	options := jfimport.Options{DryRun: dryRun}
	if !dryRun {
		// Playlists live under the PlaylistsFolder, which the server creates
		// at startup. Create it here too so an import into a fresh database
		// keeps its playlists.
		if err := items.BootstrapSystemFolders(ctx, paths.Root); err != nil {
			return fmt.Errorf("bootstrap system folders: %w", err)
		}
		var playlistService *playlists.Service
		playlistService, err = playlists.New(playlists.Options{Items: items, Users: identityUsers{identity}, Clock: clock.NewReal(), ProgramData: paths.Root, DataPath: paths.Data})
		if err != nil {
			return fmt.Errorf("open playlists: %w", err)
		}
		options.Playlists = playlistService
		var topology *library.Topology
		topology, err = library.NewTopology(paths.Root, store.NewLibraryStore(stores))
		if err != nil {
			return fmt.Errorf("open topology: %w", err)
		}
		defer func() {
			if closeErr := topology.Close(); err == nil && closeErr != nil {
				err = fmt.Errorf("close topology: %w", closeErr)
			}
		}()
		var scanner *library.Scanner
		scanner, err = library.NewScanner(topology, items, library.NewDefaultChain(naming.DefaultOptions()), library.ScannerOptions{CollectionsPath: filepath.Join(paths.Data, "collections")})
		if err != nil {
			return fmt.Errorf("open scanner: %w", err)
		}
		defer func() {
			if closeErr := scanner.Close(); err == nil && closeErr != nil {
				err = fmt.Errorf("close scanner: %w", closeErr)
			}
		}()
		options.Collections = collectionImporter{scanner: scanner, items: items}
		options.Trickplay = trickplayImporter{items: items}
	}
	report, err := jfimport.Run(ctx, src, identity, items, options)
	printImportReport(out, report, dryRun)
	return err
}

func printImportReport(out io.Writer, report jfimport.Report, dryRun bool) {
	if dryRun {
		fmt.Fprintln(out, "dry run: nothing was written")
	}
	fmt.Fprintf(out, "users: %d created with new ids, %d matched by name (keep their id, password, and policy)\n", report.UsersCreated, report.UsersMerged)
	fmt.Fprintf(out, "device tokens: %d imported, %d already present\n", report.DevicesImported, report.DevicesExisting)
	fmt.Fprintf(out, "api keys: %d imported, %d already present\n", report.APIKeysImported, report.APIKeysExisting)
	matched := report.UserDataByKey + report.UserDataByPath
	live := matched + report.UserDataUnmatchedLive
	percent := 0.0
	if live > 0 {
		percent = 100 * float64(matched) / float64(live)
	}
	fmt.Fprintf(out, "watch state: %d matched (%d by key, %d by path) = %.1f%% of rows whose item still exists\n", matched, report.UserDataByKey, report.UserDataByPath, percent)
	fmt.Fprintf(out, "watch state not imported: %d for items that still exist in Jellyfin, %d for items Jellyfin no longer has, %d for unknown users\n", report.UserDataUnmatchedLive, report.UserDataUnmatchedOrphaned, report.UserDataSkippedUser)
	roots := make([]string, 0, len(report.UserDataUnmatchedByRoot))
	for root := range report.UserDataUnmatchedByRoot {
		roots = append(roots, root)
	}
	sort.Slice(roots, func(i, j int) bool {
		return report.UserDataUnmatchedByRoot[roots[i]] > report.UserDataUnmatchedByRoot[roots[j]]
	})
	for _, root := range roots {
		fmt.Fprintf(out, "  unmatched under %s: %d\n", root, report.UserDataUnmatchedByRoot[root])
	}
	fmt.Fprintf(out, "collections: %d imported; members: %d matched, %d not found in Deadfin\n", report.CollectionsImported, report.CollectionEntriesMatched, report.CollectionEntriesUnmatched)
	fmt.Fprintf(out, "playlists: %d imported, %d already present, %d skipped; entries: %d matched, %d not found in Deadfin\n", report.PlaylistsImported, report.PlaylistsExisting, report.PlaylistsSkipped, report.PlaylistEntriesMatched, report.PlaylistEntriesUnmatched)
	fmt.Fprintf(out, "trickplay: %d imported, %d already present, %d without tiles saved with the media, %d for items not found in Deadfin\n", report.TrickplayImported, report.TrickplayExisting, report.TrickplayNotWithMedia, report.TrickplayUnmatched)
	for _, warning := range report.Warnings {
		fmt.Fprintf(out, "warning: %s\n", warning)
	}
}

// copyJellyfinLibraries copies each Jellyfin library directory that Deadfin
// lacks. Deadfin reads the same layout: options.xml, a <type>.collection
// marker, and .mblink files naming the media paths. A library whose media
// path does not exist here is skipped.
func copyJellyfinLibraries(out io.Writer, jellyfinRoot, deadfinRoot string, dryRun bool) error {
	entries, err := os.ReadDir(jellyfinRoot)
	if err != nil {
		return fmt.Errorf("read jellyfin libraries: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		target := filepath.Join(deadfinRoot, name)
		if _, err := os.Stat(target); err == nil {
			fmt.Fprintf(out, "library %q: already present\n", name)
			continue
		}
		source := filepath.Join(jellyfinRoot, name)
		files, err := os.ReadDir(source)
		if err != nil {
			return fmt.Errorf("read library %q: %w", name, err)
		}
		var missing []string
		var copyFiles []string
		for _, file := range files {
			// Skip hidden files such as macOS AppleDouble "._movies.mblink",
			// which a copy made on a Mac adds beside every file.
			if file.IsDir() || strings.HasPrefix(file.Name(), ".") {
				continue
			}
			switch ext := filepath.Ext(file.Name()); {
			case ext == ".mblink":
				link, err := os.ReadFile(filepath.Join(source, file.Name()))
				if err != nil {
					return fmt.Errorf("read %s: %w", file.Name(), err)
				}
				mediaPath := strings.TrimSpace(string(link))
				if _, err := os.Stat(mediaPath); err != nil {
					missing = append(missing, mediaPath)
				}
				copyFiles = append(copyFiles, file.Name())
			case ext == ".collection" || file.Name() == "options.xml":
				copyFiles = append(copyFiles, file.Name())
			}
		}
		if len(missing) > 0 {
			fmt.Fprintf(out, "library %q: skipped, media path not available here: %s\n", name, strings.Join(missing, ", "))
			continue
		}
		fmt.Fprintf(out, "library %q: copied (%s)\n", name, strings.Join(copyFiles, ", "))
		if dryRun {
			continue
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("create library %q: %w", name, err)
		}
		for _, file := range copyFiles {
			contents, err := os.ReadFile(filepath.Join(source, file))
			if err != nil {
				return fmt.Errorf("read %s/%s: %w", name, file, err)
			}
			if err := os.WriteFile(filepath.Join(target, file), contents, 0o644); err != nil {
				return fmt.Errorf("write %s/%s: %w", name, file, err)
			}
		}
	}
	return nil
}

// identityUsers adapts the identity store to playlists.Users.
type identityUsers struct{ identity *store.Identity }

func (u identityUsers) Get(ctx context.Context, id dto.UUID) (store.User, error) {
	return u.identity.User(ctx, id)
}

// collectionImporter creates or finds a collection by name, links its
// members, and keeps its provider IDs and images.
type collectionImporter struct {
	scanner *library.Scanner
	items   *store.ItemStore
}

func (c collectionImporter) ImportCollection(ctx context.Context, value jfimport.CollectionImport) ([]string, error) {
	collection, err := c.scanner.CreateCollection(ctx, value.Name, false)
	if err != nil {
		return nil, err
	}
	warnings, changed := keepCollectionMetadata(&collection, value)
	if changed {
		if err := c.items.Upsert(ctx, collection); err != nil {
			return warnings, fmt.Errorf("store collection metadata: %w", err)
		}
	}
	if len(value.Children) == 0 {
		return warnings, nil
	}
	return warnings, c.items.AddToCollection(ctx, collection.ID, value.Children)
}

// collectionImageName returns the local image file name a scan reads for
// imageType (Jellyfin's local image names), or false for a type that has
// none.
func collectionImageName(imageType item.ImageType) (string, bool) {
	switch imageType {
	case item.ImageTypePrimary:
		return "poster", true
	case item.ImageTypeArt:
		return "clearart", true
	case item.ImageTypeBackdrop:
		return "backdrop", true
	case item.ImageTypeBanner:
		return "banner", true
	case item.ImageTypeLogo:
		return "logo", true
	case item.ImageTypeThumb:
		return "landscape", true
	case item.ImageTypeDisc:
		return "disc", true
	default:
		return "", false
	}
}

// keepCollectionMetadata adds the Jellyfin provider IDs the collection does
// not have, and keeps the Jellyfin images. An image file is copied into the
// collection directory under its local image name, so later scans keep it;
// an image URL is stored as a remote image. A type that the collection has
// already is not changed, so a rerun changes nothing. It reports whether
// collection changed, and a warning for each image it could not keep.
func keepCollectionMetadata(collection *item.Item, value jfimport.CollectionImport) ([]string, bool) {
	changed := false
	names := make([]string, 0, len(value.ProviderIDs))
	for name := range value.ProviderIDs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, found := collection.ProviderIDs.Get(name); !found {
			collection.ProviderIDs.Set(name, value.ProviderIDs[name])
			changed = true
		}
	}
	var warnings []string
	backdrops := 0
	for _, image := range value.Images {
		imageType := item.ImageType(image.Type)
		base, supported := collectionImageName(imageType)
		if !supported || image.Type < 0 || image.Type > int(item.ImageTypeProfile) {
			warnings = append(warnings, fmt.Sprintf("collection %q: image %s: unsupported image type %d; skipped", value.Name, image.Path, image.Type))
			continue
		}
		if imageType == item.ImageTypeBackdrop {
			if backdrops > 0 {
				base = fmt.Sprintf("backdrop%d", backdrops)
			}
			backdrops++
		} else if collectionHasImage(*collection, imageType) {
			continue
		}
		if strings.HasPrefix(image.Path, "http://") || strings.HasPrefix(image.Path, "https://") {
			if !collectionHasImage(*collection, imageType) {
				collection.ImageInfos = append(collection.ImageInfos, item.ImageInfo{Type: imageType, Path: image.Path})
				changed = true
			}
			continue
		}
		if collection.Path == nil {
			continue
		}
		target := filepath.Join(*collection.Path, base+strings.ToLower(filepath.Ext(image.Path)))
		if _, err := os.Stat(target); err == nil {
			continue
		}
		if err := copyFile(image.Path, target); err != nil {
			warnings = append(warnings, fmt.Sprintf("collection %q: image %s: %v; skipped", value.Name, image.Path, err))
			continue
		}
		collection.ImageInfos = append(collection.ImageInfos, item.ImageInfo{Type: imageType, Path: target})
		changed = true
	}
	return warnings, changed
}

func collectionHasImage(collection item.Item, imageType item.ImageType) bool {
	for _, image := range collection.ImageInfos {
		if image.Type == imageType {
			return true
		}
	}
	return false
}

// copyFile copies source to a new file at target.
func copyFile(source, target string) (err error) {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(target)
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

// trickplayImporter persists a Jellyfin trickplay rendition whose tiles are
// saved with the media, where the trickplay reader finds them. The row keeps
// Jellyfin's exact interval and thumbnail count.
type trickplayImporter struct{ items *store.ItemStore }

func (i trickplayImporter) ImportTrickplay(ctx context.Context, itemID dto.UUID, info jellyfinsource.TrickplayInfo) (jfimport.TrickplayImportResult, error) {
	persisted, err := i.items.Trickplays(ctx, itemID)
	if err != nil {
		return 0, err
	}
	for _, value := range persisted {
		if value.Width == info.Width {
			return jfimport.TrickplayExisting, nil
		}
	}
	snapshot, err := i.items.DerivedSourceSnapshot(ctx, itemID)
	if err != nil {
		return 0, err
	}
	if snapshot.Path == nil || snapshot.SourceRevision == "" {
		return jfimport.TrickplayNotWithMedia, nil
	}
	present, err := trickplay.MediaResolutionHasTiles(*snapshot.Path, info.Width, info.TileWidth, info.TileHeight)
	if err != nil {
		return 0, err
	}
	if !present {
		return jfimport.TrickplayNotWithMedia, nil
	}
	token, err := discoveredTrickplayToken()
	if err != nil {
		return 0, err
	}
	if err := i.items.ReplaceTrickplay(ctx, store.TrickplayInfo{
		ItemID: itemID, SourceRevision: snapshot.SourceRevision, Width: info.Width, Height: info.Height,
		TileWidth: info.TileWidth, TileHeight: info.TileHeight, ThumbnailCount: info.ThumbnailCount,
		IntervalMS: int64(info.Interval), Bandwidth: int64(info.Bandwidth), GenerationToken: token,
	}); err != nil {
		return 0, err
	}
	return jfimport.TrickplayImported, nil
}
