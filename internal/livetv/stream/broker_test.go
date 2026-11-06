// SPDX-License-Identifier: GPL-2.0-only

package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/deadfin-org/deadfin/internal/clock"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// upstream is a fake tuner: each connect returns a pipe the test writes to.
type upstream struct {
	mu        sync.Mutex
	conns     []*conn
	connected chan *conn
	err       error
}

type conn struct {
	ctx    context.Context
	writer *io.PipeWriter
}

func newUpstream() *upstream {
	return &upstream{connected: make(chan *conn, 16)}
}

func (u *upstream) source(key, host string, tuners int) Source {
	return Source{Key: key, TunerHostID: host, TunerCount: tuners, Connect: u.connect}
}

func (u *upstream) connect(ctx context.Context) (io.ReadCloser, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.err != nil {
		return nil, u.err
	}
	reader, writer := io.Pipe()
	c := &conn{ctx: ctx, writer: writer}
	u.conns = append(u.conns, c)
	u.connected <- c
	return reader, nil
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.conns)
}

func newBroker(t *testing.T, clk clock.Clock, options Options) *Broker {
	t.Helper()
	options.Dir = t.TempDir()
	options.Clock = clk
	next := 0
	options.NewID = func() string {
		next++
		return fmt.Sprintf("s%d", next)
	}
	b, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return b
}

type openResult struct {
	info Info
	err  error
}

// openNew opens a source that connects a new upstream and writes data to it.
func openNew(t *testing.T, b *Broker, u *upstream, src Source, data []byte) (Info, *conn) {
	t.Helper()
	done := make(chan openResult, 1)
	go func() {
		info, err := b.Open(context.Background(), src)
		done <- openResult{info, err}
	}()
	c := <-u.connected
	if _, err := c.writer.Write(data); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	return result.info, c
}

func readN(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	out := make([]byte, n)
	if _, err := io.ReadFull(r, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func bufferFiles(t *testing.T, b *Broker) int {
	t.Helper()
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// Two viewers of one key share one upstream connection; the stream closes
// after the last release.
func TestBrokerSharesOneUpstream(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{})
	u := newUpstream()
	src := u.source("ch1", "tuner", 2)

	first, c := openNew(t, b, u, src, []byte("abc"))
	second, err := b.Open(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || second.Consumers != 2 || u.count() != 1 {
		t.Fatalf("not shared: %+v %+v connects %d", first, second, u.count())
	}
	if b.InUse("tuner") != 1 {
		t.Fatalf("in use %d", b.InUse("tuner"))
	}

	b.Release(first.ID)
	if c.ctx.Err() != nil {
		t.Fatal("closed with a consumer left")
	}
	b.Release(second.ID)
	<-c.ctx.Done()
	if _, ok := b.Info(first.ID); ok || b.InUse("tuner") != 0 {
		t.Fatal("stream still open")
	}
	// Close is idempotent.
	b.Release(first.ID)
}

// A host with two tuners refuses a third channel until one is released.
func TestBrokerTunerLimit(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{})
	u := newUpstream()

	one, _ := openNew(t, b, u, u.source("ch1", "tuner", 2), []byte("1"))
	openNew(t, b, u, u.source("ch2", "tuner", 2), []byte("2"))
	if _, err := b.Open(context.Background(), u.source("ch3", "tuner", 2)); !errors.Is(err, ErrTunerBusy) {
		t.Fatalf("third channel: %v", err)
	}
	// Another host is not limited by this one.
	openNew(t, b, u, u.source("other-ch3", "other", 1), []byte("3"))

	b.Release(one.ID)
	openNew(t, b, u, u.source("ch3", "tuner", 2), []byte("3"))
	if u.count() != 4 {
		t.Fatalf("connects %d", u.count())
	}
}

// A reader gets the bytes in order and waits at the live edge.
func TestBrokerReader(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{})
	u := newUpstream()
	info, c := openNew(t, b, u, u.source("ch1", "tuner", 0), []byte("hello "))

	r, err := b.Reader(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := readN(t, r, 6); string(got) != "hello " {
		t.Fatalf("got %q", got)
	}
	go func() { _, _ = c.writer.Write([]byte("world")) }()
	if got := readN(t, r, 5); string(got) != "world" {
		t.Fatalf("got %q", got)
	}

	if _, err := b.Reader(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

// The buffer file never grows past its size; a reader that falls behind
// skips to the oldest kept bytes on a packet boundary.
func TestBrokerBufferIsBounded(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	const size = 1 << 20
	b := newBroker(t, clk, Options{BufferSize: size})
	u := newUpstream()
	info, c := openNew(t, b, u, u.source("ch1", "tuner", 0), []byte{0})

	r, err := b.Reader(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Write 4 buffers' worth of a counting pattern.
	pattern := make([]byte, 4*size)
	for i := range pattern {
		pattern[i] = byte(i % 251)
	}
	if _, err := c.writer.Write(pattern); err != nil {
		t.Fatal(err)
	}
	waitWritten(t, b, info.ID, int64(1+len(pattern)))
	b.mu.Lock()
	s := b.byID[info.ID]
	b.mu.Unlock()
	s.mu.Lock()
	name := s.ring.file.Name()
	s.mu.Unlock()
	stat, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Size() > size {
		t.Fatalf("buffer file %d bytes", stat.Size())
	}

	got := readN(t, r, 1000)
	written := int64(1 + len(pattern))
	start := r.pos - 1000
	if start < written-size || start%packetSize != 0 {
		t.Fatalf("reader started at %d of %d", start, written)
	}
	// Stream byte n is pattern[n-1].
	if !bytes.Equal(got, pattern[start-1:start-1+1000]) {
		t.Fatal("skipped reader got the wrong bytes")
	}
}

// A late joiner of a stream older than 10 s starts near the live edge.
func TestBrokerLateJoiner(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{})
	u := newUpstream()
	info, _ := openNew(t, b, u, u.source("ch1", "tuner", 0), make([]byte, 100000))
	waitWritten(t, b, info.ID, 100000)

	early, err := b.Reader(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer early.Close()
	if early.pos != 0 {
		t.Fatalf("early reader at %d", early.pos)
	}
	clk.Advance(11 * time.Second)
	late, err := b.Reader(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	if late.pos != alignUp(100000-joinOffset) {
		t.Fatalf("late reader at %d", late.pos)
	}
}

// A stream with no reader closes after the idle timeout, on the injected
// clock; a reader holds it open.
func TestBrokerIdleClose(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{IdleTimeout: time.Minute, StallTimeout: time.Hour})
	u := newUpstream()
	info, c := openNew(t, b, u, u.source("ch1", "tuner", 1), []byte("x"))

	r, err := b.Reader(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)
	if c.ctx.Err() != nil {
		t.Fatal("closed while read")
	}
	_ = r.Close()
	clk.Advance(59 * time.Second)
	if c.ctx.Err() != nil {
		t.Fatal("closed before the timeout")
	}
	clk.Advance(time.Second)
	<-c.ctx.Done()
	if b.InUse("tuner") != 0 {
		t.Fatal("tuner held")
	}
}

// An upstream that fails or sends nothing fails the open and frees the
// tuner.
func TestBrokerOpenFailures(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{})

	u := newUpstream()
	u.err = errors.New("refused")
	if _, err := b.Open(context.Background(), u.source("ch1", "tuner", 1)); err == nil || err.Error() != "refused" {
		t.Fatalf("connect error: %v", err)
	}

	empty := Source{Key: "ch2", TunerHostID: "tuner", TunerCount: 1, Connect: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}}
	if _, err := b.Open(context.Background(), empty); !errors.Is(err, ErrNoData) {
		t.Fatalf("empty source: %v", err)
	}
	if b.InUse("tuner") != 0 || len(b.Streams()) != 0 {
		t.Fatalf("held: %d %v", b.InUse("tuner"), b.Streams())
	}
	waitNoBuffers(t, b)
}

// An upstream that never sends a byte fails the open after the open
// timeout and is disconnected.
func TestBrokerOpenTimeout(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{OpenTimeout: 5 * time.Second, IdleTimeout: time.Hour})
	u := newUpstream()
	done := make(chan error, 1)
	go func() {
		_, err := b.Open(context.Background(), u.source("ch1", "tuner", 1))
		done <- err
	}()
	c := <-u.connected
	for {
		select {
		case err := <-done:
			if !errors.Is(err, ErrOpenTimeout) {
				t.Fatalf("hang: %v", err)
			}
			<-c.ctx.Done()
			if b.InUse("tuner") != 0 {
				t.Fatal("tuner held")
			}
			return
		default:
			clk.Advance(time.Second)
			runtime.Gosched()
		}
	}
}

// When the upstream ends and no reconnect is left, readers drain the buffer
// and get EOF, and the next open of the channel connects again.
func TestBrokerUpstreamEnd(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{MaxReconnects: -1})
	u := newUpstream()
	src := u.source("ch1", "tuner", 1)
	info, c := openNew(t, b, u, src, []byte("bye"))

	r, err := b.Reader(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_ = c.writer.Close()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "bye" {
		t.Fatalf("drain %q %v", got, err)
	}
	if b.InUse("tuner") != 0 {
		t.Fatal("ended stream holds the tuner")
	}
	again, _ := openNew(t, b, u, src, []byte("hi"))
	if again.ID == info.ID || u.count() != 2 {
		t.Fatalf("reopen shared the ended stream: %+v", again)
	}
}

// Close stops every stream, wakes readers and removes the buffer files.
func TestBrokerClose(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	dir := t.TempDir()
	b, err := New(Options{Dir: dir, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	u := newUpstream()
	info, c := openNew(t, b, u, u.source("ch1", "tuner", 0), []byte("x"))
	r, err := b.Reader(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	readN(t, r, 1)
	readErr := make(chan error, 1)
	go func() {
		_, err := r.Read(make([]byte, 8))
		readErr <- err
	}()
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-c.ctx.Done()
	if err := <-readErr; !errors.Is(err, ErrClosed) {
		t.Fatalf("blocked reader: %v", err)
	}
	_ = r.Close()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}
	if _, err := b.Open(context.Background(), u.source("ch2", "tuner", 0)); !errors.Is(err, ErrClosed) {
		t.Fatalf("open after close: %v", err)
	}
}

// A reader closes when its context ends.
func TestBrokerReaderContext(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{})
	u := newUpstream()
	info, _ := openNew(t, b, u, u.source("ch1", "tuner", 0), []byte("x"))
	ctx, cancel := context.WithCancel(context.Background())
	r, err := b.Reader(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	readN(t, r, 1)
	cancel()
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("read after cancel: %v", err)
	}
}

// waitNoBuffers waits for the stream goroutines to remove their files. The
// file goes after the stream's context is cancelled, in the pump goroutine.
func waitNoBuffers(t *testing.T, b *Broker) {
	t.Helper()
	for i := 0; i < 100000; i++ {
		if bufferFiles(t, b) == 0 {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("%d buffer files left", bufferFiles(t, b))
}

// waitWritten waits for the pump to store n bytes. A pipe write returns
// once the pump has read the bytes, before it stores them.
func waitWritten(t *testing.T, b *Broker, id string, n int64) {
	t.Helper()
	b.mu.Lock()
	s := b.byID[id]
	b.mu.Unlock()
	for i := 0; i < 100000; i++ {
		if s.position() >= n {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("written %d, want %d", s.position(), n)
}

// Info times the first upstream connection and its first data, for the
// startup trace. A shared viewer sees the same times.
func TestBrokerInfoStartupTimes(t *testing.T) {
	start := time.Unix(100, 0)
	clk := clock.NewFake(start)
	b := newBroker(t, clk, Options{IdleTimeout: time.Hour})
	u := newUpstream()
	src := u.source("ch1", "tuner", 1)
	src.Connect = func(ctx context.Context) (io.ReadCloser, error) {
		// The upstream takes one second to answer.
		clk.Advance(time.Second)
		return u.connect(ctx)
	}
	done := make(chan openResult, 1)
	go func() {
		info, err := b.Open(context.Background(), src)
		done <- openResult{info, err}
	}()
	c := <-u.connected
	for {
		info, ok := b.Info("s1")
		if ok && !info.Connected.IsZero() {
			if !info.FirstByte.IsZero() {
				t.Fatalf("first byte before data: %+v", info)
			}
			break
		}
		runtime.Gosched()
	}
	clk.Advance(2 * time.Second)
	if _, err := c.writer.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.info.Delivered != 3 || !result.info.Opened.Equal(start) || !result.info.Connected.Equal(start.Add(time.Second)) || !result.info.FirstByte.Equal(start.Add(3*time.Second)) {
		t.Fatalf("startup times: %+v", result.info)
	}
	shared, err := b.Open(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if !shared.Connected.Equal(result.info.Connected) || !shared.FirstByte.Equal(result.info.FirstByte) {
		t.Fatalf("shared startup times: %+v", shared)
	}
	if streams := b.Streams(); len(streams) != 1 || !streams[0].FirstByte.Equal(result.info.FirstByte) {
		t.Fatalf("streams: %+v", streams)
	}
}

// timedUpstream records the broker clock time of each connect.
type timedUpstream struct {
	*upstream
	clk *clock.Fake
	mu  sync.Mutex
	at  []time.Duration
}

func (u *timedUpstream) connect(ctx context.Context) (io.ReadCloser, error) {
	u.mu.Lock()
	u.at = append(u.at, u.clk.Now().Sub(time.Unix(0, 0)))
	u.mu.Unlock()
	return u.upstream.connect(ctx)
}

// D-93: opens of one tuner host connect at least ConnectInterval apart, in
// call order; another host and a zero interval do not wait. The streams
// send nothing while the test advances the clock, so a long stall timeout
// keeps them from reconnecting on a slow runner.
func TestBrokerConnectInterval(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{OpenTimeout: time.Hour, IdleTimeout: time.Hour, StallTimeout: time.Hour})
	u := &timedUpstream{upstream: newUpstream(), clk: clk}
	source := func(key, host string, interval time.Duration) Source {
		return Source{Key: key, TunerHostID: host, Connect: u.connect, ConnectInterval: interval}
	}
	openNew(t, b, u.upstream, source("ch1", "tuner", time.Second), []byte("1"))
	openNew(t, b, u.upstream, source("other", "other", time.Second), []byte("o"))
	openNew(t, b, u.upstream, source("free", "free", 0), []byte("f"))
	openNew(t, b, u.upstream, source("free2", "free", 0), []byte("f"))

	done := make(chan error, 2)
	for _, key := range []string{"ch2", "ch3"} {
		go func() {
			_, err := b.Open(context.Background(), source(key, "tuner", time.Second))
			done <- err
		}()
	}
	for connected := 0; connected < 2; {
		select {
		case c := <-u.connected:
			if _, err := c.writer.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			connected++
		default:
			clk.Advance(10 * time.Millisecond)
			runtime.Gosched()
		}
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.at) != 6 || u.at[0] != 0 || u.at[1] != 0 || u.at[2] != 0 || u.at[3] != 0 {
		t.Fatalf("connect times %v", u.at)
	}
	if u.at[4] < time.Second || u.at[5] < 2*time.Second {
		t.Fatalf("tuner connects at %v and %v, want 1 s apart", u.at[4], u.at[5])
	}
}
