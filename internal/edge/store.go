// SPDX-License-Identifier: GPL-2.0-only

package edge

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/hls"
)

// errTooLarge reports an upload past the file limit.
var errTooLarge = errors.New("edge file exceeds the size limit")

// segmentStore holds the mirrored family files, one directory per family
// stem. A file appears only complete: a write goes to a temporary file that
// is renamed into place.
type segmentStore struct {
	root string

	mu      sync.Mutex
	waiters map[string][]chan struct{}
}

func newSegmentStore(root string) (*segmentStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &segmentStore{root: root, waiters: make(map[string][]chan struct{})}, nil
}

func (s *segmentStore) path(stem, name string) string {
	return filepath.Join(s.root, stem, name)
}

// put stores one complete file and wakes the readers that wait for it.
func (s *segmentStore) put(stem, name string, body io.Reader, limit int64) error {
	directory := filepath.Join(s.root, stem)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".upload-*")
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(temporary, io.LimitReader(body, limit+1))
	closeErr := temporary.Close()
	switch {
	case copyErr != nil:
		err = copyErr
	case closeErr != nil:
		err = closeErr
	case written > limit:
		err = errTooLarge
	default:
		// The edge serves the file to clients: it must be a well-formed
		// family file (#624).
		if err = hls.CheckFamilyFile(name, temporary.Name()); err == nil {
			err = os.Rename(temporary.Name(), s.path(stem, name))
		}
	}
	if err != nil {
		_ = os.Remove(temporary.Name())
		return err
	}
	key := stem + "/" + name
	s.mu.Lock()
	waiters := s.waiters[key]
	delete(s.waiters, key)
	s.mu.Unlock()
	for _, waiter := range waiters {
		close(waiter)
	}
	return nil
}

// open returns a stored file.
func (s *segmentStore) open(stem, name string) (*os.File, error) {
	return os.Open(s.path(stem, name))
}

// wait returns the file once it is stored, or the context error.
func (s *segmentStore) wait(ctx context.Context, stem, name string) (*os.File, error) {
	key := stem + "/" + name
	for {
		ready := make(chan struct{})
		s.mu.Lock()
		s.waiters[key] = append(s.waiters[key], ready)
		s.mu.Unlock()
		file, err := s.open(stem, name)
		if err == nil {
			s.forget(key, ready)
			return file, nil
		}
		select {
		case <-ready:
		case <-ctx.Done():
			s.forget(key, ready)
			return nil, ctx.Err()
		}
	}
}

func (s *segmentStore) forget(key string, ready chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	waiters := s.waiters[key]
	for index, waiter := range waiters {
		if waiter == ready {
			s.waiters[key] = append(waiters[:index], waiters[index+1:]...)
			break
		}
	}
	if len(s.waiters[key]) == 0 {
		delete(s.waiters, key)
	}
}

// sweep removes the files last written before cutoff, then the oldest files
// until the store holds at most maxBytes, then the empty family directories.
func (s *segmentStore) sweep(cutoff time.Time, maxBytes int64) {
	type entry struct {
		path    string
		size    int64
		modTime time.Time
	}
	var kept []entry
	var total int64
	families, err := os.ReadDir(s.root)
	if err != nil {
		return
	}
	for _, family := range families {
		if !family.IsDir() {
			continue
		}
		directory := filepath.Join(s.root, family.Name())
		files, err := os.ReadDir(directory)
		if err != nil {
			continue
		}
		for _, file := range files {
			info, err := file.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			path := filepath.Join(directory, file.Name())
			if info.ModTime().Before(cutoff) {
				_ = os.Remove(path)
				continue
			}
			kept = append(kept, entry{path: path, size: info.Size(), modTime: info.ModTime()})
			total += info.Size()
		}
	}
	if maxBytes > 0 && total > maxBytes {
		sort.Slice(kept, func(i, j int) bool { return kept[i].modTime.Before(kept[j].modTime) })
		for _, file := range kept {
			if total <= maxBytes {
				break
			}
			if os.Remove(file.path) == nil {
				total -= file.size
			}
		}
	}
	for _, family := range families {
		if family.IsDir() {
			_ = os.Remove(filepath.Join(s.root, family.Name()))
		}
	}
}
