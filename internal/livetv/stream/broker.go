// SPDX-License-Identifier: GPL-2.0-only

// Package stream shares live channel streams between viewers. It ports the
// open, share and close half of upstream LiveStream and SharedHttpStream
// (Jellyfin.LiveTv/TunerHosts/LiveStream.cs, SharedHttpStream.cs) and the
// consumer count of MediaSourceManager (docs/spikes/livetv-upstream.md
// §2.6-2.8).
//
// One supervised goroutine per open stream copies the upstream bytes into a
// fixed-size buffer file; readers tail that file. Viewers that open the same
// key share one upstream connection. The broker limits the open streams of
// each tuner host, opens without a global lock, and closes a stream when its
// last consumer releases it or when no reader has read it for the idle
// timeout.
package stream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

var (
	// ErrTunerBusy reports that every tuner of the host is in use
	// (upstream LiveTvConflictException).
	ErrTunerBusy = errors.New("livetv stream: all tuners are in use")
	// ErrNotFound reports an unknown or closed stream id.
	ErrNotFound = errors.New("livetv stream: stream not found")
	// ErrNoData reports an upstream that ended before its first byte
	// (upstream EndOfStreamException in SharedHttpStream.Open).
	ErrNoData = errors.New("livetv stream: source sent no data")
	// ErrOpenTimeout reports an upstream that sent no byte within the open
	// timeout.
	ErrOpenTimeout = errors.New("livetv stream: source did not start in time")
	// ErrClosed reports a read from a stream or broker that was closed.
	ErrClosed = errors.New("livetv stream: closed")
)

const (
	// DefaultBufferSize bounds the buffer file of one stream: about 30 s
	// at 8 Mbit/s.
	DefaultBufferSize = 32 << 20
	// DefaultIdleTimeout closes a stream that no reader reads.
	DefaultIdleTimeout = 60 * time.Second
	// DefaultOpenTimeout bounds the wait for the first upstream byte.
	DefaultOpenTimeout = 30 * time.Second
	// DefaultStallTimeout is how long a started upstream may send nothing
	// before the broker drops the connection and reconnects.
	DefaultStallTimeout = 15 * time.Second
	// DefaultMaxReconnects is how many reconnects in a row may deliver no
	// data before the stream ends.
	DefaultMaxReconnects = 3
	// maxReconnectDelay caps the wait between reconnects (1 s, 2 s, 4 s).
	maxReconnectDelay = 8 * time.Second
	// joinOffset and joinAge port LiveStream.GetStream (LiveStream.cs:97-114):
	// a reader that joins a stream older than 10 s starts 20 000 bytes
	// before the live edge.
	joinOffset = 20000
	joinAge    = 10 * time.Second
	// chunkSize is the largest upstream read. Readers treat the chunk in
	// front of the write position as unsafe.
	chunkSize = 64 << 10
	// packetSize is the MPEG-TS packet size; a reader that skips ahead lands
	// on a packet boundary.
	packetSize = 188
)

// Source is one channel stream to open.
type Source struct {
	// Key identifies the stream for sharing: opens with the same key share
	// one upstream connection (upstream OriginalStreamId).
	Key string
	// TunerHostID is the configured tuner the stream counts against.
	TunerHostID string
	// TunerCount is the most streams the tuner host may hold open; 0 means
	// no limit.
	TunerCount int
	// Connect opens the upstream byte stream. The broker closes the
	// returned body and cancels ctx when the stream closes.
	Connect func(ctx context.Context) (io.ReadCloser, error)
	// ConnectInterval is the shortest time between two upstream connects
	// to the tuner host, for opens and reconnects of all its streams
	// (D-93). A connect inside the interval waits for its turn; 0 never
	// waits.
	ConnectInterval time.Duration
}

// Info describes one open stream.
type Info struct {
	// ID is the stream's unique id (upstream ILiveStream.UniqueId).
	ID          string
	Key         string
	TunerHostID string
	Consumers   int
	Opened      time.Time
	// Connected is when the first upstream connection answered, and
	// FirstByte is when its first data arrived. Both are zero until then.
	Connected time.Time
	FirstByte time.Time
	// Delivered counts the upstream bytes written to the buffer.
	Delivered int64
}

// Options configures a Broker.
type Options struct {
	// Dir holds the buffer files, for example the transcode path.
	Dir   string
	Clock clock.Clock
	// BufferSize is the buffer file size per stream; 0 is
	// DefaultBufferSize. The minimum is 1 MiB.
	BufferSize int64
	// IdleTimeout is DefaultIdleTimeout when 0.
	IdleTimeout time.Duration
	// OpenTimeout is DefaultOpenTimeout when 0.
	OpenTimeout time.Duration
	// StallTimeout is DefaultStallTimeout when 0.
	StallTimeout time.Duration
	// MaxReconnects is DefaultMaxReconnects when 0; a negative value
	// turns reconnects off.
	MaxReconnects int
	// NewID returns a stream id; nil uses 16 random bytes in hex.
	NewID  func() string
	Logger *slog.Logger
}

// Broker owns the open streams. Close stops every stream goroutine.
type Broker struct {
	dir         string
	clock       clock.Clock
	bufferSize  int64
	idleTimeout time.Duration
	openTimeout time.Duration
	stall       time.Duration
	reconnects  int
	newID       func() string
	logger      *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
	byKey  map[string]*stream
	byID   map[string]*stream
	hosts  map[string]int
	// nextConnect is the earliest time of the next upstream connect per
	// tuner host.
	nextConnect map[string]time.Time
}

// New returns a broker. It starts no goroutine until a stream opens.
func New(options Options) (*Broker, error) {
	if options.Clock == nil {
		return nil, errors.New("livetv stream: clock is required")
	}
	if options.Dir == "" {
		return nil, errors.New("livetv stream: buffer directory is required")
	}
	if err := os.MkdirAll(options.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("livetv stream: buffer directory: %w", err)
	}
	b := &Broker{
		dir:         options.Dir,
		clock:       options.Clock,
		bufferSize:  options.BufferSize,
		idleTimeout: options.IdleTimeout,
		openTimeout: options.OpenTimeout,
		stall:       options.StallTimeout,
		reconnects:  options.MaxReconnects,
		newID:       options.NewID,
		logger:      options.Logger,
		byKey:       make(map[string]*stream),
		byID:        make(map[string]*stream),
		hosts:       make(map[string]int),
		nextConnect: make(map[string]time.Time),
	}
	if b.bufferSize == 0 {
		b.bufferSize = DefaultBufferSize
	}
	b.bufferSize = max(b.bufferSize, 1<<20)
	if b.idleTimeout <= 0 {
		b.idleTimeout = DefaultIdleTimeout
	}
	if b.openTimeout <= 0 {
		b.openTimeout = DefaultOpenTimeout
	}
	if b.stall <= 0 {
		b.stall = DefaultStallTimeout
	}
	switch {
	case b.reconnects == 0:
		b.reconnects = DefaultMaxReconnects
	case b.reconnects < 0:
		b.reconnects = 0
	}
	if b.newID == nil {
		b.newID = randomID
	}
	if b.logger == nil {
		b.logger = slog.New(slog.DiscardHandler)
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	return b, nil
}

func randomID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// stream is one open upstream connection and its buffer.
type stream struct {
	broker *Broker
	id     string
	key    string
	host   string
	opened time.Time
	// interval is the source's ConnectInterval.
	interval time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	// ready is closed after the first upstream byte or the open failure.
	ready   chan struct{}
	openErr error
	idle    clock.Timer
	stall   clock.Timer

	// Guarded by broker.mu.
	consumers int
	shared    bool
	finished  bool

	// Guarded by mu.
	mu      sync.Mutex
	ring    *ring
	written int64
	ended   bool
	notify  chan struct{}
	readers int
	// lastByte is when the upstream last sent data; dropConn cancels the
	// current upstream connection, nil between connections.
	lastByte time.Time
	dropConn context.CancelFunc
	// connected and firstByte time the first connection, for the startup
	// trace.
	connected time.Time
	firstByte time.Time
}

// Open returns the stream of src, sharing an open stream with the same key.
// It waits for the first upstream byte. Each successful Open adds one
// consumer, which Release removes.
func (b *Broker) Open(ctx context.Context, src Source) (Info, error) {
	if src.Connect == nil {
		return Info{}, errors.New("livetv stream: source has no connect function")
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return Info{}, ErrClosed
	}
	s := b.byKey[src.Key]
	if s != nil {
		s.consumers++
		b.mu.Unlock()
	} else {
		if src.TunerCount > 0 && b.hosts[src.TunerHostID] >= src.TunerCount {
			b.mu.Unlock()
			return Info{}, ErrTunerBusy
		}
		s = b.start(src)
		b.mu.Unlock()
	}
	if err := s.wait(ctx, b.clock, b.openTimeout); err != nil {
		b.Release(s.id)
		return Info{}, err
	}
	return b.info(s), nil
}

// start registers a new stream and starts its goroutines. b.mu is held.
func (b *Broker) start(src Source) *stream {
	ctx, cancel := context.WithCancel(b.ctx)
	s := &stream{
		broker:    b,
		id:        b.newID(),
		key:       src.Key,
		host:      src.TunerHostID,
		opened:    b.clock.Now(),
		interval:  src.ConnectInterval,
		ctx:       ctx,
		cancel:    cancel,
		ready:     make(chan struct{}),
		notify:    make(chan struct{}),
		consumers: 1,
		shared:    true,
	}
	// The idle timer starts now: a stream that is opened and never read
	// closes after the idle timeout.
	s.idle = b.clock.NewTimer(b.idleTimeout)
	s.stall = b.clock.NewTimer(b.stall)
	b.byKey[s.key] = s
	b.byID[s.id] = s
	b.hosts[s.host]++
	b.wg.Add(2)
	go s.pump(src.Connect)
	go s.watchIdle()
	return s
}

func (s *stream) wait(ctx context.Context, clk clock.Clock, timeout time.Duration) error {
	select {
	case <-s.ready:
		return s.openErr
	default:
	}
	timer := clk.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.ready:
		return s.openErr
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C():
		return ErrOpenTimeout
	case <-s.ctx.Done():
		return ErrClosed
	}
}

// pump copies the upstream into the buffer until the upstream ends or the
// stream closes. It owns the buffer file and removes it on close.
func (s *stream) pump(connect func(context.Context) (io.ReadCloser, error)) {
	defer s.broker.wg.Done()
	defer func() {
		<-s.ctx.Done()
		s.mu.Lock()
		if s.ring != nil {
			s.ring.remove()
			s.ring = nil
		}
		s.mu.Unlock()
	}()

	buffer, err := newRing(s.broker.dir, s.broker.bufferSize)
	if err != nil {
		s.fail(err)
		return
	}
	s.mu.Lock()
	s.ring = buffer
	s.mu.Unlock()

	started := false
	failures := 0
	for {
		delivered, err := s.copyConnection(connect, buffer, &started)
		if s.ctx.Err() != nil {
			return
		}
		if !started {
			if errors.Is(err, io.EOF) {
				err = ErrNoData
			}
			s.fail(err)
			return
		}
		if errors.Is(err, errBuffer) {
			s.end(err, true)
			return
		}
		// A started live upstream that ends, fails or stalls is
		// reconnected (D-88). Reconnects that bring no data count against
		// MaxReconnects; one that brings data starts the count again.
		if delivered {
			failures = 0
		}
		failures++
		if failures > s.broker.reconnects {
			s.end(err, true)
			return
		}
		s.broker.logger.Info("live stream upstream lost; reconnecting", "stream", s.id, "attempt", failures, "error", err)
		if !s.sleep(reconnectDelay(failures)) {
			return
		}
	}
}

// errBuffer marks a failed write to the buffer file.
var errBuffer = errors.New("livetv stream: buffer write failed")

// copyConnection opens one upstream connection and copies it into the
// buffer until it ends, fails or stalls. delivered reports any data.
func (s *stream) copyConnection(connect func(context.Context) (io.ReadCloser, error), buffer *ring, started *bool) (delivered bool, err error) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	if !s.sleep(s.broker.connectDelay(s.host, s.interval)) {
		return false, ErrClosed
	}
	body, err := connect(ctx)
	if err != nil {
		return false, err
	}
	stop := context.AfterFunc(ctx, func() { _ = body.Close() })
	defer func() {
		if stop() {
			_ = body.Close()
		}
	}()
	s.mu.Lock()
	s.dropConn = cancel
	s.lastByte = s.broker.clock.Now()
	if s.connected.IsZero() {
		s.connected = s.lastByte
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.dropConn = nil
		s.mu.Unlock()
	}()

	chunk := make([]byte, chunkSize)
	for {
		n, readErr := body.Read(chunk)
		if n > 0 {
			if err := buffer.writeAt(chunk[:n], s.position()); err != nil {
				return delivered, fmt.Errorf("%w: %w", errBuffer, err)
			}
			s.advance(int64(n))
			delivered = true
			if !*started {
				*started = true
				close(s.ready)
			}
		}
		if readErr != nil {
			if ctx.Err() != nil && s.ctx.Err() == nil {
				return delivered, errStalled
			}
			return delivered, readErr
		}
	}
}

// errStalled reports an upstream dropped for sending nothing within the
// stall timeout.
var errStalled = errors.New("livetv stream: source stalled")

// connectDelay reserves the next upstream connect slot of a tuner host and
// returns the wait until it (D-93). Connects get slots in call order, one
// interval apart.
func (b *Broker) connectDelay(host string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	slot := now
	if next := b.nextConnect[host]; next.After(now) {
		slot = next
	}
	b.nextConnect[host] = slot.Add(interval)
	return slot.Sub(now)
}

func reconnectDelay(attempt int) time.Duration {
	return min(time.Second<<(attempt-1), maxReconnectDelay)
}

// sleep waits on the broker clock; it reports false when the stream closes.
func (s *stream) sleep(delay time.Duration) bool {
	if delay <= 0 {
		return s.ctx.Err() == nil
	}
	timer := s.broker.clock.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C():
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *stream) position() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written
}

func (s *stream) advance(n int64) {
	s.mu.Lock()
	s.written += n
	s.lastByte = s.broker.clock.Now()
	if s.firstByte.IsZero() {
		s.firstByte = s.lastByte
	}
	close(s.notify)
	s.notify = make(chan struct{})
	s.mu.Unlock()
}

// fail ends a stream that never started. Waiting openers get err.
func (s *stream) fail(err error) {
	if s.ctx.Err() != nil {
		err = ErrClosed
	}
	s.openErr = err
	close(s.ready)
	s.end(err, false)
}

// end marks the upstream finished: the stream stops counting against its
// tuner, so a new open of the key connects again, and then readers drain the
// buffer and get EOF. A reader that sees EOF can open the key again at once.
func (s *stream) end(err error, started bool) {
	if started && s.ctx.Err() == nil && !errors.Is(err, io.EOF) {
		s.broker.logger.Warn("live stream ended", "stream", s.id, "error", err)
	}
	b := s.broker
	b.mu.Lock()
	b.unshare(s)
	b.mu.Unlock()
	s.mu.Lock()
	s.ended = true
	close(s.notify)
	s.notify = make(chan struct{})
	s.mu.Unlock()
}

// unshare removes s from sharing and frees its tuner. b.mu is held.
func (b *Broker) unshare(s *stream) {
	if !s.shared {
		return
	}
	s.shared = false
	if b.byKey[s.key] == s {
		delete(b.byKey, s.key)
	}
	b.hosts[s.host]--
	if b.hosts[s.host] <= 0 {
		delete(b.hosts, s.host)
	}
}

// finish closes s. b.mu is held.
func (b *Broker) finish(s *stream) {
	if s.finished {
		return
	}
	s.finished = true
	b.unshare(s)
	delete(b.byID, s.id)
	s.cancel()
}

// checkStall drops an upstream connection that has sent nothing for the
// stall timeout, so the pump reconnects. Before the first byte the open
// timeout applies instead.
func (s *stream) checkStall() {
	now := s.broker.clock.Now()
	next := s.broker.stall
	s.mu.Lock()
	if s.dropConn != nil && s.written > 0 {
		if quiet := now.Sub(s.lastByte); quiet >= s.broker.stall {
			s.dropConn()
		} else {
			next = s.broker.stall - quiet
		}
	}
	s.mu.Unlock()
	s.stall.Reset(next)
}

// watchIdle closes the stream when the idle timer fires with no reader.
func (s *stream) watchIdle() {
	defer s.broker.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			s.idle.Stop()
			s.stall.Stop()
			return
		case <-s.stall.C():
			s.checkStall()
		case <-s.idle.C():
			s.mu.Lock()
			readers := s.readers
			s.mu.Unlock()
			if readers > 0 {
				continue
			}
			s.broker.logger.Debug("closing idle live stream", "stream", s.id)
			b := s.broker
			b.mu.Lock()
			b.finish(s)
			b.mu.Unlock()
			return
		}
	}
}

// Release removes one consumer of a stream and closes the stream after the
// last (upstream MediaSourceManager.CloseLiveStream). An unknown id is not
// an error: close is idempotent.
func (b *Broker) Release(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.byID[id]
	if s == nil {
		return
	}
	s.consumers--
	if s.consumers <= 0 {
		b.finish(s)
	}
}

// Info returns an open stream.
func (b *Broker) Info(id string) (Info, bool) {
	b.mu.Lock()
	s := b.byID[id]
	b.mu.Unlock()
	if s == nil {
		return Info{}, false
	}
	return b.info(s), true
}

func (b *Broker) info(s *stream) Info {
	b.mu.Lock()
	defer b.mu.Unlock()
	return s.infoLocked()
}

// infoLocked describes s. b.mu is held; it takes s.mu.
func (s *stream) infoLocked() Info {
	s.mu.Lock()
	connected, firstByte, delivered := s.connected, s.firstByte, s.written
	s.mu.Unlock()
	return Info{ID: s.id, Key: s.key, TunerHostID: s.host, Consumers: s.consumers, Opened: s.opened, Connected: connected, FirstByte: firstByte, Delivered: delivered}
}

// Streams lists the open streams.
func (b *Broker) Streams() []Info {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Info, 0, len(b.byID))
	for _, s := range b.byID {
		out = append(out, s.infoLocked())
	}
	return out
}

// InUse reports the open streams that count against a tuner host.
func (b *Broker) InUse(tunerHostID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hosts[tunerHostID]
}

// Close closes every stream and waits for the stream goroutines, or for ctx.
func (b *Broker) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	for _, s := range b.byID {
		b.finish(s)
	}
	b.mu.Unlock()
	b.cancel()
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
