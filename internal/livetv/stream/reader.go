// SPDX-License-Identifier: GPL-2.0-only

package stream

import (
	"context"
	"io"
	"sync"
)

// Reader tails the buffer of one stream. It blocks at the live edge until
// more bytes arrive, returns io.EOF after the upstream ends and the buffer
// is drained, and ErrClosed after the stream or the reader closes. A reader
// that falls a whole buffer behind skips to the oldest kept bytes.
type Reader struct {
	s    *stream
	pos  int64
	stop func() bool

	once   sync.Once
	closed chan struct{}
}

// Reader opens a reader of stream id. It closes when ctx ends. A reader of
// a stream older than 10 s starts 20 000 bytes before the live edge, as
// upstream LiveStream.GetStream does.
func (b *Broker) Reader(ctx context.Context, id string) (*Reader, error) {
	b.mu.Lock()
	s := b.byID[id]
	b.mu.Unlock()
	if s == nil {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	var start int64
	if b.clock.Now().Sub(s.opened) > joinAge {
		start = alignUp(max(s.written-joinOffset, s.oldest()))
	} else {
		start = alignUp(s.oldest())
	}
	s.readers++
	s.idle.Stop()
	s.mu.Unlock()

	r := &Reader{s: s, pos: start, closed: make(chan struct{})}
	r.stop = context.AfterFunc(ctx, func() { _ = r.Close() })
	return r, nil
}

// oldest is the first stream position the buffer still holds safely.
// s.mu is held.
func (s *stream) oldest() int64 {
	return max(0, s.written+chunkSize-s.broker.bufferSize)
}

func alignUp(position int64) int64 {
	return (position + packetSize - 1) / packetSize * packetSize
}

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s := r.s
	for {
		select {
		case <-r.closed:
			return 0, ErrClosed
		default:
		}
		s.mu.Lock()
		if s.ctx.Err() != nil || s.ring == nil {
			s.mu.Unlock()
			return 0, ErrClosed
		}
		if r.pos < s.oldest() {
			r.pos = alignUp(s.oldest())
		}
		if r.pos < s.written {
			want := min(int64(len(p)), s.written-r.pos)
			buffer := s.ring
			s.mu.Unlock()
			n, err := buffer.readAt(p[:want], r.pos)
			if err != nil {
				return 0, ErrClosed
			}
			s.mu.Lock()
			// The writer may have overwritten the bytes during the read.
			if r.pos < s.oldest() {
				s.mu.Unlock()
				continue
			}
			r.pos += int64(n)
			s.mu.Unlock()
			return n, nil
		}
		if s.ended {
			s.mu.Unlock()
			return 0, io.EOF
		}
		notify := s.notify
		s.mu.Unlock()
		select {
		case <-notify:
		case <-r.closed:
			return 0, ErrClosed
		case <-s.ctx.Done():
			return 0, ErrClosed
		}
	}
}

// Close detaches the reader. With no reader left, the stream's idle timer
// starts.
func (r *Reader) Close() error {
	r.once.Do(func() {
		close(r.closed)
		r.stop()
		s := r.s
		s.mu.Lock()
		s.readers--
		if s.readers == 0 {
			s.idle.Reset(s.broker.idleTimeout)
		}
		s.mu.Unlock()
	})
	return nil
}
