// SPDX-License-Identifier: GPL-2.0-only

package livetv

import (
	"bufio"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/deadfin-org/deadfin/internal/livetv/guide"
)

// spillChannelDays is the guide size of one write batch, in channel-days.
// A 1 000 channel × 7 day guide is about 185 000 programmes, so a batch
// holds about 90 000.
const spillChannelDays = 3500

// programSpill keeps the programmes of one refresh out of memory. The guide
// is read once; each programme goes to the temporary file of its channel's
// batch, and the refresh reads back one batch at a time. Memory then grows
// with the batch size, not with the guide size.
type programSpill struct {
	dir     string
	size    int
	files   []*os.File
	writers []*bufio.Writer
	encs    []*gob.Encoder
}

// spilledProgram is one programme of the channel at index State.
type spilledProgram struct {
	State   int
	Program guide.Program
}

// newProgramSpill makes the temporary directory under root (the system
// temporary directory when root is empty) for a guide of channels × days.
func newProgramSpill(root string, channels, days int) (*programSpill, error) {
	if root != "" {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, fmt.Errorf("livetv: guide spill: %w", err)
		}
	}
	dir, err := os.MkdirTemp(root, "livetv-guide-*")
	if err != nil {
		return nil, fmt.Errorf("livetv: guide spill: %w", err)
	}
	size := max(1, spillChannelDays/max(1, days))
	batches := max(1, (channels+size-1)/size)
	return &programSpill{
		dir:     dir,
		size:    size,
		files:   make([]*os.File, batches),
		writers: make([]*bufio.Writer, batches),
		encs:    make([]*gob.Encoder, batches),
	}, nil
}

// batches is the number of batches.
func (s *programSpill) batches() int { return len(s.files) }

// bounds is the channel index range [first, last) of batch b.
func (s *programSpill) bounds(b, channels int) (first, last int) {
	return b * s.size, min((b+1)*s.size, channels)
}

// add stores a programme of the channel at index.
func (s *programSpill) add(index int, program guide.Program) error {
	b := index / s.size
	if s.files[b] == nil {
		file, err := os.CreateTemp(s.dir, "batch-*")
		if err != nil {
			return fmt.Errorf("livetv: guide spill: %w", err)
		}
		s.files[b] = file
		s.writers[b] = bufio.NewWriter(file)
		s.encs[b] = gob.NewEncoder(s.writers[b])
	}
	if err := s.encs[b].Encode(spilledProgram{State: index, Program: program}); err != nil {
		return fmt.Errorf("livetv: guide spill: %w", err)
	}
	return nil
}

// read yields the programmes of batch b in the order they were added.
func (s *programSpill) read(b int, yield func(index int, program guide.Program)) error {
	file := s.files[b]
	if file == nil {
		return nil
	}
	if err := s.writers[b].Flush(); err != nil {
		return fmt.Errorf("livetv: guide spill: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("livetv: guide spill: %w", err)
	}
	decoder := gob.NewDecoder(bufio.NewReader(file))
	for {
		// gob leaves zero fields unset, so each record needs a new value.
		var record spilledProgram
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("livetv: guide spill: %w", err)
		}
		yield(record.State, record.Program)
	}
}

// release closes and removes batch b once it is written.
func (s *programSpill) release(b int) {
	if s.files[b] != nil {
		_ = s.files[b].Close()
		_ = os.Remove(s.files[b].Name())
		s.files[b], s.writers[b], s.encs[b] = nil, nil, nil
	}
}

// close removes every temporary file.
func (s *programSpill) close() {
	for b := range s.files {
		s.release(b)
	}
	_ = os.RemoveAll(s.dir)
}
