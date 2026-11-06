// SPDX-License-Identifier: GPL-2.0-only

package stream

import (
	"fmt"
	"os"
)

// ring is a fixed-size buffer file. Byte n of the stream is at offset
// n % size, so the file never grows past size; upstream SharedHttpStream
// writes one file that grows for the life of the stream.
type ring struct {
	file *os.File
	size int64
}

func newRing(dir string, size int64) (*ring, error) {
	file, err := os.CreateTemp(dir, "live-*.ts")
	if err != nil {
		return nil, fmt.Errorf("livetv stream: buffer: %w", err)
	}
	return &ring{file: file, size: size}, nil
}

// writeAt stores p as the stream bytes from absolute position at. p must
// not be longer than the ring.
func (r *ring) writeAt(p []byte, at int64) error {
	for len(p) > 0 {
		offset := at % r.size
		n := min(int64(len(p)), r.size-offset)
		if _, err := r.file.WriteAt(p[:n], offset); err != nil {
			return fmt.Errorf("livetv stream: buffer: %w", err)
		}
		p, at = p[n:], at+n
	}
	return nil
}

// readAt reads stream bytes from absolute position at, up to the end of the
// file (one contiguous piece).
func (r *ring) readAt(p []byte, at int64) (int, error) {
	offset := at % r.size
	n := min(int64(len(p)), r.size-offset)
	read, err := r.file.ReadAt(p[:n], offset)
	if err != nil && read < int(n) {
		return read, fmt.Errorf("livetv stream: buffer: %w", err)
	}
	return read, nil
}

func (r *ring) remove() {
	_ = r.file.Close()
	_ = os.Remove(r.file.Name())
}
