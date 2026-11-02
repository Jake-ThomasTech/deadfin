// SPDX-License-Identifier: GPL-2.0-only

package trickplay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/images"
	"github.com/deadfin-org/deadfin/internal/rootfs"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
	"golang.org/x/sys/unix"
)

var (
	errMetadataReconciliationUnknown = errors.New("trickplay metadata reconciliation is indeterminate")
	ErrStaleSourceRevision           = errors.New("trickplay source revision is stale")
	ErrGeneratorClosed               = errors.New("trickplay generator is closed")
)

// FrameRequest describes one software frame-extraction job. Source is the
// pinned media descriptor; the extractor inherits it into ffmpeg and never
// closes it. Output is an existing, empty, caller-owned directory where the
// extractor may publish JPEG frames.
type FrameRequest struct {
	ItemID               dto.UUID
	Source               *os.File
	Output               *rootfs.Dir
	Width                int
	IntervalMilliseconds int64
	RunTimeTicks         *int64
	ReferenceFrameRate   *float32
}

// FrameExtraction is the ordered output of one frame-extraction job. Names are
// single-component filenames resolved only from the request Output descriptor.
type FrameExtraction struct {
	Names  []string
	Height int
}

type (
	FrameExtractor interface {
		Extract(context.Context, FrameRequest) (FrameExtraction, error)
	}
	TileComposer interface {
		Compose(context.Context, images.TileRequest) (images.TileResult, error)
	}
	MetadataStore interface {
		ReplaceTrickplay(context.Context, Metadata) error
	}
	MetadataReader interface {
		ReadTrickplay(context.Context, dto.UUID, int) (*Metadata, error)
	}
)

type GeneratorConfig struct {
	Root        string
	Extractor   FrameExtractor
	Composer    TileComposer
	Store       MetadataStore
	Reader      MetadataReader
	Publication *PublicationCoordinator
}

type GenerateRequest struct {
	ItemID               dto.UUID
	Source               *sourcefile.Source
	VideoWidth           *int
	RunTimeTicks         *int64
	RequestedWidth       int
	TileWidth            int
	TileHeight           int
	IntervalMilliseconds int64
	ReferenceFrameRate   *float32
	JPEGQuality          int
	SourceRevision       string
}

// Generator owns root for its full lifetime. rootPath is diagnostic and a
// freshness gate only: every artifact operation below uses rootfs descriptors.
type Generator struct {
	root                                      *rootfs.Dir
	rootPath                                  string
	extractor                                 FrameExtractor
	composer                                  TileComposer
	store                                     MetadataStore
	reader                                    MetadataReader
	exchange                                  func(*rootfs.Dir, string, *rootfs.Dir, string) error
	publication                               *PublicationCoordinator
	afterPublicationBeforeMetadata            func()
	afterMetadataBeforeFinalize               func()
	afterExchangeBeforeJournalRewrite         func()
	afterRollbackExchangeBeforeJournalRemoval func()
	afterRollbackJournalBeforeExchange        func()
	syncDirectory                             func(*rootfs.Dir) error
	makeStaging                               func(*rootfs.Dir) (string, *rootfs.Dir, error)
	removeAll                                 func(*rootfs.Dir, string) error
	lifecycle                                 sync.RWMutex
	closed                                    bool
	closeErr                                  error
	oneAtATime                                chan struct{}
}

func NewGenerator(config GeneratorConfig) (*Generator, error) {
	if strings.TrimSpace(config.Root) == "" {
		return nil, errors.New("trickplay root is empty")
	}
	if isNilAdapter(config.Extractor) {
		return nil, errors.New("trickplay frame extractor is nil")
	}
	if isNilAdapter(config.Composer) {
		return nil, errors.New("trickplay tile composer is nil")
	}
	if isNilAdapter(config.Reader) {
		return nil, errors.New("trickplay metadata reader is nil")
	}
	if isNilAdapter(config.Store) {
		return nil, errors.New("trickplay metadata store is nil")
	}
	if config.Publication == nil {
		return nil, errors.New("trickplay publication coordinator is nil")
	}

	rootPath, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("make trickplay root absolute: %w", err)
	}
	if err := os.MkdirAll(rootPath, 0o755); err != nil {
		return nil, fmt.Errorf("create trickplay root: %w", err)
	}
	info, err := os.Lstat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("stat trickplay root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("trickplay root %q is not a directory", rootPath)
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return nil, fmt.Errorf("canonicalize trickplay root: %w", err)
	}
	root, err := rootfs.Pin(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open trickplay root descriptor: %w", err)
	}
	return &Generator{
		root: root, rootPath: rootPath, extractor: config.Extractor, composer: config.Composer,
		store: config.Store, reader: config.Reader, publication: config.Publication,
		exchange: func(from *rootfs.Dir, fromName string, to *rootfs.Dir, toName string) error {
			return from.Exchange(fromName, to, toName)
		},
		syncDirectory: func(dir *rootfs.Dir) error { return dir.Sync() },
		makeStaging:   makeGeneratorStaging,
		removeAll:     func(parent *rootfs.Dir, name string) error { return parent.RemoveAll(name) },
		oneAtATime:    make(chan struct{}, 1),
	}, nil
}

func makeGeneratorStaging(root *rootfs.Dir) (string, *rootfs.Dir, error) {
	for range 32 {
		bytes := make([]byte, 16)
		if _, err := rand.Read(bytes); err != nil {
			return "", nil, err
		}
		name := ".trickplay-" + hex.EncodeToString(bytes)
		if err := root.Mkdir(name, 0o700); err == nil {
			dir, err := root.OpenDir(name)
			if err != nil {
				_ = root.RemoveAll(name)
				return "", nil, err
			}
			return name, dir, nil
		} else if !errors.Is(err, unix.EEXIST) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("could not allocate unique trickplay staging directory")
}

func (generator *Generator) Close() error {
	if generator == nil {
		return nil
	}
	generator.lifecycle.Lock()
	defer generator.lifecycle.Unlock()
	if generator.closed {
		return generator.closeErr
	}
	generator.closed = true
	generator.closeErr = generator.root.Close()
	return generator.closeErr
}

func (generator *Generator) acquireLifecycle() error {
	generator.lifecycle.RLock()
	if generator.closed {
		generator.lifecycle.RUnlock()
		return ErrGeneratorClosed
	}
	return nil
}

// validateRootIdentity is deliberately a freshness gate, not a correctness
// boundary. All subsequent mutation is rooted in the retained descriptor.
func (generator *Generator) validateRootIdentity() error {
	if err := generator.root.PathStillPinned(generator.rootPath); err != nil {
		return errors.New("trickplay root identity changed")
	}
	return nil
}

func (generator *Generator) Generate(ctx context.Context, request GenerateRequest) (Metadata, error) {
	if ctx == nil {
		return Metadata{}, errors.New("trickplay generation context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	select {
	case generator.oneAtATime <- struct{}{}:
		defer func() { <-generator.oneAtATime }()
	case <-ctx.Done():
		return Metadata{}, ctx.Err()
	}
	if err := generator.acquireLifecycle(); err != nil {
		return Metadata{}, err
	}
	defer generator.lifecycle.RUnlock()
	if err := generator.validateRootIdentity(); err != nil {
		return Metadata{}, err
	}
	return generator.generate(ctx, request)
}

// Recover only traverses descriptor-rooted owned-tree levels. It intentionally
// does not use the root pathname after construction.
func (generator *Generator) Recover(ctx context.Context) error {
	if ctx == nil {
		return errors.New("trickplay recovery context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	generator.publication.Lock()
	defer generator.publication.Unlock()
	if err := generator.acquireLifecycle(); err != nil {
		return err
	}
	defer generator.lifecycle.RUnlock()
	if err := generator.validateRootIdentity(); err != nil {
		return err
	}

	entries, err := generator.root.ReadNames()
	if err != nil {
		return fmt.Errorf("read trickplay recovery root: %w", err)
	}
	var targets []Metadata
	for _, shardEntry := range entries {
		name := shardEntry.Name()
		st, err := generator.root.Lstat(name)
		if err != nil {
			return fmt.Errorf("inspect trickplay recovery root %q: %w", name, err)
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return fmt.Errorf("trickplay recovery refuses symlink %q", name)
		}
		if strings.HasPrefix(name, ".trickplay-") {
			continue
		}
		if len(name) != 2 || st.Mode&unix.S_IFMT != unix.S_IFDIR {
			continue
		}
		shard, err := generator.root.OpenDir(name)
		if err != nil {
			return fmt.Errorf("open trickplay recovery shard %q: %w", name, err)
		}
		itemErr := func() error {
			items, err := shard.ReadNames()
			if err != nil {
				return err
			}
			for _, itemEntry := range items {
				itemName := itemEntry.Name()
				itemSt, err := shard.Lstat(itemName)
				if err != nil {
					return err
				}
				if itemSt.Mode&unix.S_IFMT == unix.S_IFLNK {
					return fmt.Errorf("trickplay recovery refuses symlink %q", itemName)
				}
				if itemSt.Mode&unix.S_IFMT != unix.S_IFDIR {
					continue
				}
				item, err := shard.OpenDir(itemName)
				if err != nil {
					return err
				}
				err = func() error {
					files, err := item.ReadNames()
					if err != nil {
						return err
					}
					for _, fileEntry := range files {
						fileName := fileEntry.Name()
						fileSt, err := item.Lstat(fileName)
						if err != nil {
							return err
						}
						if fileSt.Mode&unix.S_IFMT == unix.S_IFLNK {
							return fmt.Errorf("trickplay recovery refuses symlink %q", fileName)
						}
						if strings.HasPrefix(fileName, ".trickplay-journal-") && fileSt.Mode&unix.S_IFMT == unix.S_IFREG {
							if err := item.Remove(fileName); err != nil {
								return err
							}
							continue
						}
						if fileSt.Mode&unix.S_IFMT != unix.S_IFREG || !strings.HasPrefix(fileName, ".") || !strings.HasSuffix(fileName, ".journal") {
							continue
						}
						raw, err := item.ReadFile(fileName, 1<<20)
						if err != nil {
							return err
						}
						journal, err := decodePublicationJournal(fileName, raw)
						if err != nil {
							return err
						}
						if fileName != "."+journal.Resolution+".journal" {
							return fmt.Errorf("trickplay journal %q has inconsistent Resolution component", fileName)
						}
						targets = append(targets, journal.Metadata)
					}
					return nil
				}()
				closeErr := item.Close()
				if err != nil {
					return err
				}
				if closeErr != nil {
					return closeErr
				}
			}
			return nil
		}()
		closeErr := shard.Close()
		if itemErr != nil {
			return fmt.Errorf("scan trickplay journals: %w", itemErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close trickplay recovery shard: %w", closeErr)
		}
	}
	for _, metadata := range targets {
		if err := generator.recoverPublicationJournal(ctx, metadata.ItemID, metadata.Width, metadata.TileWidth, metadata.TileHeight); err != nil {
			return err
		}
	}
	entries, err = generator.root.ReadNames()
	if err != nil {
		return fmt.Errorf("read abandoned trickplay staging: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".trickplay-") {
			continue
		}
		st, err := generator.root.Lstat(name)
		if err != nil {
			return fmt.Errorf("inspect abandoned trickplay staging %q: %w", name, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return fmt.Errorf("abandoned trickplay staging %q is not an owned directory", name)
		}
		if err := generator.removeAll(generator.root, name); err != nil {
			return fmt.Errorf("remove abandoned trickplay staging %q: %w", name, err)
		}
		if err := generator.syncDirectory(generator.root); err != nil {
			return fmt.Errorf("sync removed abandoned trickplay staging: %w", err)
		}
	}
	return nil
}

func (generator *Generator) generate(ctx context.Context, request GenerateRequest) (metadata Metadata, returnErr error) {
	if err := validateGenerateRequest(request); err != nil {
		return Metadata{}, err
	}
	if err := generator.syncDirectory(generator.root); err != nil {
		return Metadata{}, fmt.Errorf("sync trickplay root: %w", err)
	}
	quality := request.JPEGQuality
	if quality == 0 {
		quality = 90
	}
	manager := Manager{}
	if !manager.CanGenerate(request.RunTimeTicks, request.IntervalMilliseconds) {
		return Metadata{}, errors.New("trickplay runtime is shorter than one sampling interval")
	}
	if request.IntervalMilliseconds < 1_000 {
		request.IntervalMilliseconds = 1_000
	}
	preplan, err := manager.Plan(GenerationRequest{ItemID: request.ItemID, RequestedWidth: request.RequestedWidth, VideoWidth: request.VideoWidth, GeneratedHeight: 1, TileWidth: request.TileWidth, TileHeight: request.TileHeight, IntervalMilliseconds: request.IntervalMilliseconds, ThumbnailPaths: []string{"pending"}, TileByteSizes: []int64{0}})
	if err != nil {
		return Metadata{}, fmt.Errorf("plan trickplay geometry: %w", err)
	}
	generator.publication.Lock()
	if err := generator.validateRootIdentity(); err != nil {
		generator.publication.Unlock()
		return Metadata{}, err
	}
	if err := generator.recoverPublicationJournal(ctx, request.ItemID, preplan.Metadata.Width, request.TileWidth, request.TileHeight); err != nil {
		generator.publication.Unlock()
		return Metadata{}, err
	}
	if err := generator.recoverPriorResolution(request.ItemID, preplan.Metadata.Width, request.TileWidth, request.TileHeight); err != nil {
		generator.publication.Unlock()
		return Metadata{}, err
	}
	generator.publication.Unlock()
	if err := generator.validateDestination(request.ItemID, preplan.Metadata.Width, request.TileWidth, request.TileHeight); err != nil {
		return Metadata{}, err
	}

	stagingName, staging, err := generator.makeStaging(generator.root)
	if err != nil {
		return Metadata{}, fmt.Errorf("create trickplay staging directory: %w", err)
	}
	retainStaging := false
	defer func() {
		closeErr := staging.Close()
		if !retainStaging {
			if err := generator.removeAll(generator.root, stagingName); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("trickplay staging cleanup: %w", err))
			} else if err := generator.syncDirectory(generator.root); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("trickplay staging cleanup root sync: %w", err))
			}
		}
		returnErr = errors.Join(returnErr, closeErr)
	}()
	if err := generator.syncDirectory(generator.root); err != nil {
		return Metadata{}, fmt.Errorf("sync created trickplay staging directory: %w", err)
	}
	if err := staging.Mkdir("frames", 0o755); err != nil {
		return Metadata{}, fmt.Errorf("create frame staging directory: %w", err)
	}
	frames, err := staging.OpenDir("frames")
	if err != nil {
		return Metadata{}, fmt.Errorf("open frame staging directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, frames.Close()) }()
	extraction, err := generator.extractor.Extract(ctx, FrameRequest{ItemID: request.ItemID, Source: request.Source.File(), Output: frames, Width: preplan.Metadata.Width, RunTimeTicks: request.RunTimeTicks, IntervalMilliseconds: request.IntervalMilliseconds, ReferenceFrameRate: request.ReferenceFrameRate})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Metadata{}, ctxErr
		}
		return Metadata{}, fmt.Errorf("extract trickplay frames: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	if err := validateExtraction(frames, extraction); err != nil {
		return Metadata{}, err
	}
	if err := request.Source.VerifyUnchanged(); err != nil {
		return Metadata{}, fmt.Errorf("trickplay source changed during frame extraction: %w", ErrStaleSourceRevision)
	}

	resolution := trickplayResolutionName(preplan.Metadata.Width, request.TileWidth, request.TileHeight)
	if err := staging.Mkdir(resolution, 0o755); err != nil {
		return Metadata{}, fmt.Errorf("create tile staging directory: %w", err)
	}
	staged, err := staging.OpenDir(resolution)
	if err != nil {
		return Metadata{}, fmt.Errorf("open tile staging directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, staged.Close()) }()
	token, err := generator.writeGenerationManifest(staged)
	if err != nil {
		return Metadata{}, err
	}
	capacity, err := generatorTileCapacity(request.TileWidth, request.TileHeight)
	if err != nil {
		return Metadata{}, err
	}
	tileSizes := make([]int64, 0, (len(extraction.Names)+capacity-1)/capacity)
	for index, first := 0, 0; first < len(extraction.Names); index, first = index+1, first+capacity {
		if err := ctx.Err(); err != nil {
			return Metadata{}, err
		}
		last := min(first+capacity, len(extraction.Names))
		name := fmt.Sprintf("%d.jpg", index)
		result, err := generator.composer.Compose(ctx, images.TileRequest{SourceDir: frames, SourceNames: extraction.Names[first:last], OutputDir: staged, OutputName: name, CellWidth: preplan.Metadata.Width, CellHeight: extraction.Height, Columns: request.TileWidth, Rows: request.TileHeight, JPEGQuality: quality})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Metadata{}, ctxErr
			}
			return Metadata{}, fmt.Errorf("compose trickplay tile %d: %w", index, err)
		}
		if _, err := generatedTileSize(staged, name); err != nil {
			return Metadata{}, fmt.Errorf("measure trickplay tile %d: %w", index, err)
		}
		if result.ByteSize < 0 {
			return Metadata{}, fmt.Errorf("compose trickplay tile %d returned a negative byte size", index)
		}
		tileSizes = append(tileSizes, result.ByteSize)
	}
	plan, err := manager.Plan(GenerationRequest{ItemID: request.ItemID, RequestedWidth: request.RequestedWidth, VideoWidth: request.VideoWidth, GeneratedHeight: extraction.Height, TileWidth: request.TileWidth, TileHeight: request.TileHeight, IntervalMilliseconds: request.IntervalMilliseconds, ThumbnailPaths: extraction.Names, TileByteSizes: tileSizes})
	if err != nil {
		return Metadata{}, fmt.Errorf("plan generated trickplay: %w", err)
	}
	plan.Metadata.GenerationToken, plan.Metadata.SourceRevision = token, request.SourceRevision
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	generator.publication.Lock()
	defer generator.publication.Unlock()
	// Do not re-check rootPath here: this job already owns rooted descriptors;
	// a mid-flight replacement must complete coherently in the pinned inode.
	publication, err := generator.publishResolution(request.ItemID, plan.Metadata.Width, request.TileWidth, request.TileHeight, plan.Metadata, token, stagingName, staging, resolution)
	if err != nil {
		if publication.stagingParent != nil && !publication.settled {
			// A durable pending journal can name this staging tree even though no
			// publication mutation has happened yet.
			retainStaging = true
		}
		return Metadata{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, publication.parent.Close()) }()
	// An exchange puts the only old committed generation under staging. Keep it
	// until metadata commits and finalization, or rollback, reaches durability.
	retainStaging = publication.exchanged
	rollback := func(cause error) error {
		retainStaging = true
		settled, rollbackErr := generator.rollbackPublication(publication, cause)
		if settled {
			retainStaging = false
		}
		return rollbackErr
	}
	if err := generator.syncDirectory(publication.parent); err != nil {
		return Metadata{}, rollback(fmt.Errorf("sync published trickplay directory: %w", err))
	}
	if generator.afterPublicationBeforeMetadata != nil {
		generator.afterPublicationBeforeMetadata()
	}
	if err := request.Source.VerifyUnchanged(); err != nil {
		return Metadata{}, rollback(fmt.Errorf("%w: %w", ErrStaleSourceRevision, err))
	}
	if err := request.Source.VerifyPathStillCurrent(); err != nil {
		return Metadata{}, rollback(fmt.Errorf("%w: %w", ErrStaleSourceRevision, err))
	}
	if err := generator.store.ReplaceTrickplay(ctx, plan.Metadata); err != nil {
		if errors.Is(err, ErrStaleSourceRevision) {
			return Metadata{}, rollback(err)
		}
		settled, reconciliationErr := generator.reconcileFailedMetadataWrite(ctx, request.ItemID, plan.Metadata.Width, publication, plan.Metadata, err)
		if settled {
			retainStaging = false
		} else {
			retainStaging = true
		}
		return Metadata{}, reconciliationErr
	}
	publication.committed = true
	if err := generator.writePublicationJournal(publication); err != nil {
		retainStaging = true
		return Metadata{}, fmt.Errorf("record committed trickplay metadata: %w", err)
	}
	if err := generator.syncPublicationParents(publication); err != nil {
		retainStaging = true
		return Metadata{}, fmt.Errorf("sync committed trickplay metadata: %w", err)
	}
	if generator.afterMetadataBeforeFinalize != nil {
		generator.afterMetadataBeforeFinalize()
	}
	if err := generator.finalizePublication(publication); err != nil {
		retainStaging = true
		return Metadata{}, err
	}
	retainStaging = false
	return plan.Metadata, nil
}

func (generator *Generator) reconcileFailedMetadataWrite(ctx context.Context, itemID dto.UUID, width int, publication resolutionPublication, metadata Metadata, cause error) (bool, error) {
	persisted, err := generator.reader.ReadTrickplay(ctx, itemID, width)
	if err != nil {
		return false, errors.Join(fmt.Errorf("persist trickplay metadata: %w", cause), fmt.Errorf("%w: read trickplay metadata after failed persistence: %v", errMetadataReconciliationUnknown, err))
	}
	if persisted != nil && persisted.GenerationToken == publication.token {
		publication.committed = true
		if err := generator.writePublicationJournal(publication); err != nil {
			return false, errors.Join(fmt.Errorf("persist trickplay metadata: %w", cause), fmt.Errorf("record reconciled metadata: %w", err))
		}
		if err := generator.syncPublicationParents(publication); err != nil {
			return false, errors.Join(fmt.Errorf("persist trickplay metadata: %w", cause), fmt.Errorf("sync reconciled metadata: %w", err))
		}
		if err := generator.finalizePublication(publication); err != nil {
			return false, errors.Join(fmt.Errorf("persist trickplay metadata: %w", cause), err)
		}
		return true, fmt.Errorf("persist trickplay metadata: %w", cause)
	}
	return generator.rollbackPublication(publication, fmt.Errorf("persist trickplay metadata: %w", cause))
}

func validateGenerateRequest(request GenerateRequest) error {
	if request.ItemID.IsNil() {
		return errors.New("trickplay item ID is nil")
	}
	if request.Source == nil {
		return errors.New("trickplay source is nil")
	}
	if request.RequestedWidth <= 0 {
		return errors.New("trickplay requested width must be positive")
	}
	if request.TileWidth <= 0 || request.TileHeight <= 0 {
		return errors.New("trickplay tile dimensions must be positive")
	}
	if request.IntervalMilliseconds <= 0 {
		return errors.New("trickplay interval must be positive")
	}
	if request.JPEGQuality < 0 || request.JPEGQuality > 100 {
		return errors.New("trickplay JPEG quality must be in 0..100")
	}
	if request.RunTimeTicks == nil {
		return errors.New("trickplay runtime is unknown")
	}
	if strings.TrimSpace(request.SourceRevision) == "" {
		return errors.New("trickplay source revision is empty")
	}
	return nil
}

func validateExtraction(frames *rootfs.Dir, extraction FrameExtraction) error {
	if extraction.Height <= 0 {
		return errors.New("extracted trickplay frame height must be positive")
	}
	if len(extraction.Names) == 0 {
		return errors.New("frame extractor returned no frames")
	}
	for index, name := range extraction.Names {
		if !validFrameName(name) {
			return fmt.Errorf("extracted frame %d has invalid name %q", index, name)
		}
		info, err := frames.Lstat(name)
		if err != nil {
			return fmt.Errorf("stat extracted frame %d: %w", index, err)
		}
		if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Size == 0 {
			return fmt.Errorf("extracted frame %d is not a nonempty regular file", index)
		}
	}
	return nil
}

func generatedTileSize(directory *rootfs.Dir, name string) (int64, error) {
	info, err := directory.Lstat(name)
	if err != nil {
		return 0, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Size <= 0 {
		return 0, errors.New("tile is not a nonempty regular file")
	}
	return info.Size, nil
}

func (generator *Generator) recoverPublicationJournal(ctx context.Context, itemID dto.UUID, width, tileWidth, tileHeight int) (returnErr error) {
	resolution := trickplayResolutionName(width, tileWidth, tileHeight)
	parent, err := generator.openItemDir(itemID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect trickplay journal directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	journalName := "." + resolution + ".journal"
	raw, err := parent.ReadFile(journalName, 1<<20)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read trickplay journal: %w", err)
	}
	journal, err := decodePublicationJournal(journalName, raw)
	if err != nil {
		return err
	}
	staging, err := generator.root.OpenDir(journal.StagingName)
	if errors.Is(err, fs.ErrNotExist) && journal.RollingBack {
		if journal.Exchanged {
			destinationNew, manifestErr := generator.manifestMatches(parent, resolution, journal.Token)
			if manifestErr != nil {
				return manifestErr
			}
			if destinationNew {
				return errors.New("rolling-back exchanged trickplay publication is missing staging")
			}
			if _, statErr := parent.Lstat(resolution); statErr != nil {
				return fmt.Errorf("inspect restored prior trickplay resolution: %w", statErr)
			}
		}
		if err := generator.syncDirectory(generator.root); err != nil {
			return fmt.Errorf("sync removed recovered trickplay staging: %w", err)
		}
		return generator.removePublicationJournal(resolutionPublication{parent: parent, journalName: journalName})
	}
	if err != nil {
		return fmt.Errorf("open trickplay journal staging: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, staging.Close()) }()
	publication := resolutionPublication{parent: parent, stagingParent: staging, shard: journal.Shard, item: journal.Item, stagingName: journal.StagingName, resolution: journal.Resolution, journalName: journalName, exchanged: journal.Exchanged, committed: journal.Committed, finalized: journal.Finalized, rollingBack: journal.RollingBack, identityAtDestination: journal.IdentityAtDestination, token: journal.Token, metadata: journal.Metadata}
	persisted, err := generator.reader.ReadTrickplay(ctx, itemID, width)
	if err != nil {
		return fmt.Errorf("read trickplay metadata for journal recovery: %w", err)
	}
	destinationNew, err := generator.manifestMatches(parent, resolution, publication.token)
	if err != nil {
		return err
	}
	stagedNew, err := generator.manifestMatches(staging, resolution, publication.token)
	if err != nil {
		return err
	}
	if destinationNew && stagedNew {
		return errors.New("trickplay generation identity appears in both publication paths")
	}
	keepNew := persisted != nil && persisted.GenerationToken == publication.token
	if !publication.rollingBack && keepNew && !destinationNew && !stagedNew && publication.identityAtDestination {
		destinationNew = true
	}
	if publication.rollingBack {
		if keepNew {
			return errors.New("rolling back committed trickplay generation")
		}
		if !publication.exchanged && publication.identityAtDestination {
			if err := generator.removeAll(parent, resolution); err != nil {
				return fmt.Errorf("remove uncommitted initial trickplay resolution: %w", err)
			}
		} else if stagedNew {
			if err := generator.removeAll(staging, resolution); err != nil {
				return fmt.Errorf("remove rolled-back trickplay generation: %w", err)
			}
		} else if destinationNew {
			if _, err := staging.Lstat(resolution); err == nil {
				if err := generator.exchange(parent, resolution, staging, resolution); err != nil {
					return fmt.Errorf("restore prior trickplay resolution: %w", err)
				}
				if err := generator.syncPublicationParents(publication); err != nil {
					return fmt.Errorf("sync restored prior trickplay resolution: %w", err)
				}
				if err := generator.removeAll(staging, resolution); err != nil {
					return fmt.Errorf("remove rolled-back trickplay generation: %w", err)
				}
			} else if errors.Is(err, fs.ErrNotExist) {
				if err := generator.removeAll(parent, resolution); err != nil {
					return fmt.Errorf("remove uncommitted initial trickplay resolution: %w", err)
				}
			} else {
				return fmt.Errorf("inspect prior trickplay resolution: %w", err)
			}
		} else if _, err := staging.Lstat(resolution); err == nil {
			if err := generator.removeAll(staging, resolution); err != nil {
				return fmt.Errorf("remove abandoned trickplay generation: %w", err)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect abandoned trickplay generation: %w", err)
		}
		if err := generator.removeStagingParent(publication); err != nil {
			return fmt.Errorf("remove recovered trickplay staging: %w", err)
		}
		return generator.removePublicationJournal(publication)
	}
	if keepNew && stagedNew {
		if _, err := parent.Lstat(resolution); err != nil {
			return fmt.Errorf("inspect prior trickplay resolution: %w", err)
		}
		if err := generator.exchange(parent, resolution, staging, resolution); err != nil {
			return fmt.Errorf("restore committed trickplay resolution: %w", err)
		}
		if err := generator.syncPublicationParents(publication); err != nil {
			return fmt.Errorf("sync restored committed trickplay resolution: %w", err)
		}
		destinationNew, stagedNew = true, false
	}
	if !keepNew && destinationNew && !stagedNew {
		if _, err := staging.Lstat(resolution); err == nil {
			if err := generator.exchange(parent, resolution, staging, resolution); err != nil {
				return fmt.Errorf("restore prior trickplay resolution: %w", err)
			}
			if err := generator.syncPublicationParents(publication); err != nil {
				return fmt.Errorf("sync restored prior trickplay resolution: %w", err)
			}
			destinationNew, stagedNew = false, true
		} else if errors.Is(err, fs.ErrNotExist) {
			if err := generator.removeAll(parent, resolution); err != nil {
				return fmt.Errorf("remove uncommitted initial trickplay resolution: %w", err)
			}
			destinationNew = false
		} else {
			return fmt.Errorf("inspect prior trickplay resolution: %w", err)
		}
	}
	if keepNew && !destinationNew {
		return errors.New("committed trickplay generation identity is missing from canonical resolution")
	}
	if stagedNew {
		if err := generator.removeAll(staging, resolution); err != nil {
			return fmt.Errorf("remove abandoned trickplay generation: %w", err)
		}
	} else if _, err := staging.Lstat(resolution); err == nil {
		if err := generator.removeAll(staging, resolution); err != nil {
			return fmt.Errorf("remove prior trickplay resolution: %w", err)
		}
	}
	if keepNew {
		if err := generator.withResolutionDir(parent, resolution, generator.removeGenerationManifest); err != nil {
			return err
		}
	}
	if err := generator.removeStagingParent(publication); err != nil {
		return fmt.Errorf("remove recovered trickplay staging: %w", err)
	}
	return generator.removePublicationJournal(publication)
}

func (generator *Generator) recoverPriorResolution(itemID dto.UUID, width, tileWidth, tileHeight int) (returnErr error) {
	resolution := trickplayResolutionName(width, tileWidth, tileHeight)
	parent, err := generator.openItemDir(itemID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect trickplay recovery directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	if st, err := parent.Lstat(resolution); err == nil {
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return fmt.Errorf("trickplay recovery destination %q is not an owned directory", resolution)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect trickplay recovery destination %q: %w", resolution, err)
	}
	entries, err := parent.ReadNames()
	if err != nil {
		return fmt.Errorf("read trickplay recovery directory: %w", err)
	}
	prefix := resolution + ".previous-"
	candidate := ""
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if len(name) == len(prefix) {
			return fmt.Errorf("malformed prior trickplay resolution %q", name)
		}
		st, err := parent.Lstat(name)
		if err != nil {
			return fmt.Errorf("inspect prior trickplay resolution %q: %w", name, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return fmt.Errorf("malformed prior trickplay resolution %q", name)
		}
		if candidate != "" {
			return fmt.Errorf("multiple prior trickplay resolutions for %q", resolution)
		}
		candidate = name
	}
	if candidate == "" {
		return nil
	}
	if err := parent.Rename(candidate, parent, resolution); err != nil {
		return fmt.Errorf("restore prior trickplay resolution: %w", err)
	}
	if err := generator.syncDirectory(parent); err != nil {
		return fmt.Errorf("sync restored prior trickplay resolution: %w", err)
	}
	return nil
}

func (generator *Generator) validateDestination(itemID dto.UUID, width, tileWidth, tileHeight int) error {
	current := generator.root
	closeCurrent := func() {}
	defer func() { closeCurrent() }()
	for _, component := range []string{itemID.Dashed()[:2], itemID.Dashed(), trickplayResolutionName(width, tileWidth, tileHeight)} {
		st, err := current.Lstat(component)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect trickplay output %q: %w", component, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return fmt.Errorf("trickplay output component %q is not an owned directory", component)
		}
		next, err := current.OpenDir(component)
		if err != nil {
			return fmt.Errorf("open trickplay output component %q: %w", component, err)
		}
		if current != generator.root {
			_ = current.Close()
		}
		current = next
		closeCurrent = func() { _ = current.Close() }
	}
	return nil
}

type resolutionPublication struct {
	parent                *rootfs.Dir
	stagingParent         *rootfs.Dir
	shard                 string
	item                  string
	stagingName           string
	resolution            string
	journalName           string
	finalized             bool
	exchanged             bool
	committed             bool
	token                 string
	rollingBack           bool
	identityAtDestination bool
	metadata              Metadata
	settled               bool
}

type publicationJournal struct {
	Version               int
	Shard                 string
	Item                  string
	Resolution            string
	StagingName           string
	Exchanged             bool
	Metadata              Metadata
	Committed             bool
	Token                 string
	IdentityAtDestination bool
	RollingBack           bool
	Finalized             bool
	// Legacy pathname fields are never serialized. Their presence is detected
	// from the raw object before validation so recovery never consumes paths.
	Destination string `json:"-"`
	Staged      string `json:"-"`
}

func validJournalComponent(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\\`) && !strings.ContainsRune(name, 0)
}

func decodePublicationJournal(name string, raw []byte) (publicationJournal, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return publicationJournal{}, fmt.Errorf("decode trickplay journal %q: %w", name, err)
	}
	var journal publicationJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		return publicationJournal{}, fmt.Errorf("decode trickplay journal %q: %w", name, err)
	}
	if journal.Version == 0 {
		if _, destination := fields["Destination"]; destination {
			return publicationJournal{}, fmt.Errorf("legacy trickplay journal %q is unsupported; remove the stale root journal and staging", name)
		}
		if _, staged := fields["Staged"]; staged {
			return publicationJournal{}, fmt.Errorf("legacy trickplay journal %q is unsupported; remove the stale root journal and staging", name)
		}
	}
	if journal.Version != 2 {
		return publicationJournal{}, fmt.Errorf("trickplay journal %q has unsupported version %d", name, journal.Version)
	}
	for field, value := range map[string]string{"Shard": journal.Shard, "Item": journal.Item, "Resolution": journal.Resolution, "StagingName": journal.StagingName} {
		if !validJournalComponent(value) {
			return publicationJournal{}, fmt.Errorf("trickplay journal %q has invalid %s component", name, field)
		}
	}
	if len(journal.Item) < 2 {
		return publicationJournal{}, fmt.Errorf("trickplay journal %q has invalid Item component", name)
	}
	if !strings.HasPrefix(journal.StagingName, ".trickplay-") {
		return publicationJournal{}, fmt.Errorf("trickplay journal %q has invalid StagingName component", name)
	}
	if journal.Token == "" {
		return publicationJournal{}, fmt.Errorf("trickplay journal %q has empty Token", name)
	}
	if journal.Shard != journal.Item[:2] {
		return publicationJournal{}, fmt.Errorf("trickplay journal %q has inconsistent Shard component", name)
	}
	if journal.Item != journal.Metadata.ItemID.Dashed() {
		return publicationJournal{}, fmt.Errorf("trickplay journal %q has inconsistent Item component", name)
	}
	if journal.Resolution != trickplayResolutionName(journal.Metadata.Width, journal.Metadata.TileWidth, journal.Metadata.TileHeight) {
		return publicationJournal{}, fmt.Errorf("trickplay journal %q has inconsistent Resolution component", name)
	}
	return journal, nil
}

func (generator *Generator) publishResolution(itemID dto.UUID, width, tileWidth, tileHeight int, metadata Metadata, token, stagingName string, staging *rootfs.Dir, resolution string) (resolutionPublication, error) {
	if err := generator.validateDestination(itemID, width, tileWidth, tileHeight); err != nil {
		return resolutionPublication{}, err
	}
	parent, err := generator.ensureDestinationParent(itemID)
	if err != nil {
		return resolutionPublication{}, err
	}
	publication := resolutionPublication{parent: parent, stagingParent: staging, shard: itemID.Dashed()[:2], item: itemID.Dashed(), stagingName: stagingName, resolution: resolution, journalName: "." + resolution + ".journal", token: token, metadata: metadata}
	st, err := staging.Lstat(resolution)
	if err != nil {
		_ = parent.Close()
		return resolutionPublication{}, fmt.Errorf("stat staged trickplay resolution: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = parent.Close()
		return resolutionPublication{}, errors.New("staged trickplay resolution is not a directory")
	}
	if err := generator.writePublicationJournal(publication); err != nil {
		return closePublicationFailure(publication, err)
	}
	if err := generator.syncPublicationParents(publication); err != nil {
		return closePublicationFailure(publication, fmt.Errorf("sync pending trickplay journal: %w", err))
	}
	st, err = parent.Lstat(resolution)
	if errors.Is(err, fs.ErrNotExist) {
		if err := staging.Rename(resolution, parent, resolution); err != nil {
			return closePublicationFailure(publication, fmt.Errorf("publish trickplay resolution: %w", err))
		}
		publication.identityAtDestination = true
		if err := generator.writePublicationJournal(publication); err != nil {
			return generator.rollbackAndClose(publication, fmt.Errorf("record initial trickplay identity location: %w", err))
		}
		if err := generator.syncPublicationParents(publication); err != nil {
			return generator.rollbackAndClose(publication, fmt.Errorf("sync initial trickplay publication: %w", err))
		}
		return publication, nil
	}
	if err != nil {
		return closePublicationFailure(publication, fmt.Errorf("stat existing trickplay resolution: %w", err))
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return closePublicationFailure(publication, errors.New("existing trickplay resolution is not an owned directory"))
	}
	if err := generator.exchange(staging, resolution, parent, resolution); err != nil {
		return closePublicationFailure(publication, fmt.Errorf("atomically exchange trickplay resolutions: %w", err))
	}
	if generator.afterExchangeBeforeJournalRewrite != nil {
		generator.afterExchangeBeforeJournalRewrite()
	}
	publication.exchanged, publication.identityAtDestination = true, true
	if err := generator.writePublicationJournal(publication); err != nil {
		return generator.rollbackAndClose(publication, fmt.Errorf("record exchanged trickplay resolution: %w", err))
	}
	if err := generator.syncPublicationParents(publication); err != nil {
		return generator.rollbackAndClose(publication, fmt.Errorf("sync exchanged trickplay resolution: %w", err))
	}
	return publication, nil
}

func closePublicationFailure(publication resolutionPublication, cause error) (resolutionPublication, error) {
	return publication, errors.Join(cause, publication.parent.Close())
}

func (generator *Generator) rollbackAndClose(publication resolutionPublication, cause error) (resolutionPublication, error) {
	settled, rollbackErr := generator.rollbackPublication(publication, cause)
	publication.settled = settled
	return publication, errors.Join(rollbackErr, publication.parent.Close())
}

func (generator *Generator) writePublicationJournal(publication resolutionPublication) error {
	raw, err := json.Marshal(publicationJournal{Version: 2, Shard: publication.shard, Item: publication.item, Resolution: publication.resolution, StagingName: publication.stagingName, Exchanged: publication.exchanged, Metadata: publication.metadata, Committed: publication.committed, Token: publication.token, IdentityAtDestination: publication.identityAtDestination, RollingBack: publication.rollingBack, Finalized: publication.finalized})
	if err != nil {
		return fmt.Errorf("encode trickplay journal: %w", err)
	}
	if err := publication.parent.ReplaceFileDurable(publication.journalName, ".trickplay-journal-", raw, 0o600, func(file *os.File) error { return file.Sync() }); err != nil {
		return fmt.Errorf("publish trickplay journal: %w", err)
	}
	return nil
}

func (generator *Generator) syncPublicationParents(publication resolutionPublication) error {
	parentErr := generator.syncDirectory(publication.parent)
	var stagingErr error
	if publication.stagingParent != publication.parent {
		stagingErr = generator.syncDirectory(publication.stagingParent)
	}
	return errors.Join(parentErr, stagingErr)
}

const generationManifestName = ".deadfin-trickplay-generation"

func (generator *Generator) writeGenerationManifest(directory *rootfs.Dir) (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("create trickplay generation identity: %w", err)
	}
	token := hex.EncodeToString(bytes)
	if err := generator.writeGenerationManifestValue(directory, token); err != nil {
		return "", err
	}
	return token, nil
}

func (generator *Generator) writeGenerationManifestValue(directory *rootfs.Dir, token string) error {
	file, err := directory.CreateExclusive(generationManifestName, 0o600)
	if err != nil {
		return fmt.Errorf("create trickplay generation manifest: %w", err)
	}
	if _, err := file.WriteString(token + "\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("write trickplay generation manifest: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync trickplay generation manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close trickplay generation manifest: %w", err)
	}
	if err := generator.syncDirectory(directory); err != nil {
		return fmt.Errorf("sync trickplay generation manifest directory: %w", err)
	}
	return nil
}

func generationManifestMatches(directory *rootfs.Dir, token string) (bool, error) {
	if token == "" {
		return false, errors.New("trickplay generation identity is empty")
	}
	raw, err := directory.ReadFile(generationManifestName, 4096)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read trickplay generation manifest: %w", err)
	}
	return strings.TrimSpace(string(raw)) == token, nil
}

func (generator *Generator) manifestMatches(parent *rootfs.Dir, resolution, token string) (matches bool, returnErr error) {
	dir, err := parent.OpenDir(resolution)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open trickplay generation directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, dir.Close()) }()
	return generationManifestMatches(dir, token)
}

func (generator *Generator) ensureGenerationManifest(directory *rootfs.Dir, token string) error {
	raw, err := directory.ReadFile(generationManifestName, 4096)
	if errors.Is(err, fs.ErrNotExist) {
		return generator.writeGenerationManifestValue(directory, token)
	}
	if err != nil {
		return fmt.Errorf("read trickplay generation manifest: %w", err)
	}
	if strings.TrimSpace(string(raw)) != token {
		return errors.New("trickplay generation manifest does not match publication")
	}
	return nil
}

func (generator *Generator) removeGenerationManifest(directory *rootfs.Dir) error {
	if err := directory.Remove(generationManifestName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove trickplay generation manifest: %w", err)
	}
	if err := generator.syncDirectory(directory); err != nil {
		return fmt.Errorf("sync removed trickplay generation manifest: %w", err)
	}
	return nil
}

func (generator *Generator) removePublicationJournal(publication resolutionPublication) error {
	if err := publication.parent.Remove(publication.journalName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove trickplay journal: %w", err)
	}
	return generator.syncDirectory(publication.parent)
}

func (generator *Generator) rollbackPublication(publication resolutionPublication, cause error) (bool, error) {
	if publication.exchanged {
		if err := generator.withResolutionDir(publication.parent, publication.resolution, func(destination *rootfs.Dir) error {
			return generator.ensureGenerationManifest(destination, publication.token)
		}); err != nil {
			return false, fmt.Errorf("%w (restore trickplay generation identity: %v)", cause, err)
		}
	}
	publication.rollingBack = true
	if err := generator.writePublicationJournal(publication); err != nil {
		return false, fmt.Errorf("%w (record rollback intent: %v)", cause, err)
	}
	if err := generator.syncPublicationParents(publication); err != nil {
		return false, fmt.Errorf("%w (sync rollback intent: %v)", cause, err)
	}
	if generator.afterRollbackJournalBeforeExchange != nil {
		generator.afterRollbackJournalBeforeExchange()
	}
	if publication.exchanged {
		if err := generator.exchange(publication.parent, publication.resolution, publication.stagingParent, publication.resolution); err != nil {
			return false, fmt.Errorf("%w (restore prior trickplay resolution: %v)", cause, err)
		}
		if generator.afterRollbackExchangeBeforeJournalRemoval != nil {
			generator.afterRollbackExchangeBeforeJournalRemoval()
		}
		if err := generator.removeAll(publication.stagingParent, publication.resolution); err != nil {
			return false, fmt.Errorf("%w (remove failed trickplay resolution: %v)", cause, err)
		}
	} else if err := generator.removeAll(publication.parent, publication.resolution); err != nil {
		return false, fmt.Errorf("%w (remove unpublished trickplay resolution: %v)", cause, err)
	}
	if err := generator.syncPublicationParents(publication); err != nil {
		return false, errors.Join(cause, fmt.Errorf("sync restored trickplay publication: %w", err))
	}
	if err := generator.removePublicationJournal(publication); err != nil {
		return false, fmt.Errorf("%w (remove restored trickplay journal: %v)", cause, err)
	}
	return true, cause
}

func (generator *Generator) finalizePublication(publication resolutionPublication) error {
	publication.finalized = true
	if err := generator.writePublicationJournal(publication); err != nil {
		return fmt.Errorf("record finalized trickplay generation identity: %w", err)
	}
	if err := generator.syncPublicationParents(publication); err != nil {
		return fmt.Errorf("sync finalized trickplay generation identity: %w", err)
	}
	if publication.exchanged {
		if err := generator.removeAll(publication.stagingParent, publication.resolution); err != nil {
			return fmt.Errorf("remove prior trickplay resolution: %w", err)
		}
		if err := generator.syncPublicationParents(publication); err != nil {
			return fmt.Errorf("sync removed prior trickplay resolution: %w", err)
		}
	}
	if err := generator.withResolutionDir(publication.parent, publication.resolution, generator.removeGenerationManifest); err != nil {
		return fmt.Errorf("finalize trickplay generation identity: %w", err)
	}
	if err := generator.removePublicationJournal(publication); err != nil {
		return fmt.Errorf("finalize trickplay journal: %w", err)
	}
	return nil
}

func (generator *Generator) removeStagingParent(publication resolutionPublication) error {
	if !strings.HasPrefix(publication.stagingName, ".trickplay-") {
		return errors.New("invalid trickplay staging directory")
	}
	if err := generator.removeAll(generator.root, publication.stagingName); err != nil {
		return err
	}
	return generator.syncDirectory(generator.root)
}

func (generator *Generator) ensureDestinationParent(itemID dto.UUID) (*rootfs.Dir, error) {
	current := generator.root
	for _, component := range []string{itemID.Dashed()[:2], itemID.Dashed()} {
		created, err := current.EnsureDir(component, 0o755)
		if err != nil {
			if current != generator.root {
				_ = current.Close()
			}
			return nil, fmt.Errorf("create trickplay output directory %q: %w", component, err)
		}
		if created {
			if err := generator.syncDirectory(current); err != nil {
				if current != generator.root {
					_ = current.Close()
				}
				return nil, fmt.Errorf("sync trickplay output ancestor %q: %w", component, err)
			}
		}
		next, err := current.OpenDir(component)
		if err != nil {
			if current != generator.root {
				_ = current.Close()
			}
			return nil, fmt.Errorf("open trickplay output directory %q: %w", component, err)
		}
		if current != generator.root {
			_ = current.Close()
		}
		current = next
	}
	return current, nil
}

func (generator *Generator) openItemDir(itemID dto.UUID) (*rootfs.Dir, error) {
	current := generator.root
	for _, component := range []string{itemID.Dashed()[:2], itemID.Dashed()} {
		st, err := current.Lstat(component)
		if err != nil {
			if current != generator.root {
				_ = current.Close()
			}
			return nil, err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			if current != generator.root {
				_ = current.Close()
			}
			return nil, fmt.Errorf("trickplay journal component %q is not an owned directory", component)
		}
		next, err := current.OpenDir(component)
		if current != generator.root {
			_ = current.Close()
		}
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

func (generator *Generator) withResolutionDir(parent *rootfs.Dir, resolution string, fn func(*rootfs.Dir) error) error {
	dir, err := parent.OpenDir(resolution)
	if err != nil {
		return err
	}
	err = fn(dir)
	closeErr := dir.Close()
	return errors.Join(err, closeErr)
}

func trickplayResolutionName(width, tileWidth, tileHeight int) string {
	return fmt.Sprintf("%d - %dx%d", width, tileWidth, tileHeight)
}

func generatorTileCapacity(width, height int) (int, error) {
	maxInt := int(^uint(0) >> 1)
	if width <= 0 || height <= 0 || width > maxInt/height {
		return 0, errors.New("trickplay tile capacity is invalid")
	}
	return width * height, nil
}

func isDescendant(root, path string) bool {
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func isNilAdapter(value any) bool {
	if value == nil {
		return true
	}
	kind := reflect.ValueOf(value).Kind()
	return (kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice) && reflect.ValueOf(value).IsNil()
}
