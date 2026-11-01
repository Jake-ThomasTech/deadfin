// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

type segmentCleanerState struct {
	Exited                bool
	Path                  string
	DownloadPositionTicks int64
	HLS                   bool
}

type segmentDirectory interface {
	Entries(batchSize int) ([]string, error)
	Remove(name string) error
	Close() error
}

type segmentCleanerOptions struct {
	Clock         clock.Clock
	Retention     time.Duration
	SegmentLength time.Duration
	// SegmentStarts is the media start of each segment file, by index, when
	// the files do not follow SegmentLength (a keyframe-cut remux). Nil uses
	// SegmentLength.
	SegmentStarts []int64
	State         func() segmentCleanerState
	OpenDir       func(string) (segmentDirectory, error)
}

type transcodingSegmentCleaner struct {
	options segmentCleanerOptions
	done    chan struct{}
	once    sync.Once
}

const segmentDirectoryBatchSize = 128

func newTranscodingSegmentCleaner(options segmentCleanerOptions) *transcodingSegmentCleaner {
	if options.Clock == nil {
		options.Clock = clock.NewReal()
	}
	return &transcodingSegmentCleaner{options: options, done: make(chan struct{})}
}

func (cleaner *transcodingSegmentCleaner) Run(ctx context.Context) error {
	for {
		timer := cleaner.options.Clock.NewTimer(20 * time.Second)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return nil
		case <-cleaner.done:
			stopTimer(timer)
			return nil
		case <-timer.C():
		}
		if cleaner.options.State == nil || cleaner.options.OpenDir == nil {
			continue
		}
		state := cleaner.options.State()
		if state.Exited {
			return nil
		}
		if !state.HLS || cleaner.options.SegmentLength <= 0 {
			continue
		}
		if err := cleaner.clean(ctx, state); err != nil {
			select {
			case <-ctx.Done():
				return nil
			case <-cleaner.done:
				return nil
			default:
			}
		}
	}
}

func (cleaner *transcodingSegmentCleaner) Close(context.Context) error {
	cleaner.once.Do(func() { close(cleaner.done) })
	return nil
}

func (cleaner *transcodingSegmentCleaner) clean(ctx context.Context, state segmentCleanerState) (err error) {
	retention := cleaner.options.Retention
	if retention < 20*time.Second {
		retention = 20 * time.Second
	}
	position := time.Duration(state.DownloadPositionTicks) * 100
	if position <= retention {
		return nil
	}
	lastExpired := int((position - retention) / cleaner.options.SegmentLength)
	if starts := cleaner.options.SegmentStarts; len(starts) != 0 {
		// A file expires when it ends before the retention window. Its end
		// is the start of the next file. The nominal length would expire
		// files ahead of the player when the GOPs are longer than the
		// segment length, and the player's next request would restart
		// ffmpeg (deadfin#799).
		cutoff := int64((position - retention) / 100)
		lastExpired = sort.Search(len(starts), func(index int) bool { return starts[index] > cutoff }) - 2
		if lastExpired < 0 {
			return nil
		}
	}
	base := filepath.Base(state.Path)
	if extension := filepath.Ext(base); extension != "" {
		base = base[:len(base)-len(extension)]
	}
	matcher := regexp.MustCompile("^" + regexp.QuoteMeta(base) + `([0-9]+)\.(?:ts|mp4)$`)
	directory, err := cleaner.options.OpenDir(state.Path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, directory.Close())
	}()
	gateOpened := false
	var removeErr error
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-cleaner.done:
			return nil
		default:
		}
		entries, readErr := directory.Entries(segmentDirectoryBatchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, name := range entries {
			match := matcher.FindStringSubmatch(name)
			if match == nil {
				continue
			}
			index, parseErr := strconv.Atoi(match[1])
			if parseErr != nil || index > lastExpired {
				continue
			}
			if !gateOpened {
				gate := cleaner.options.Clock.NewTimer(1500 * time.Millisecond)
				select {
				case <-ctx.Done():
					stopTimer(gate)
					return nil
				case <-cleaner.done:
					stopTimer(gate)
					return nil
				case <-gate.C():
				}
				gateOpened = true
			}
			select {
			case <-ctx.Done():
				return nil
			case <-cleaner.done:
				return nil
			default:
			}
			if removeErr == nil {
				removeErr = directory.Remove(name)
			} else {
				_ = directory.Remove(name)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return removeErr
		}
	}
}
