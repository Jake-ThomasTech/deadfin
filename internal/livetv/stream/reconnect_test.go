// SPDX-License-Identifier: GPL-2.0-only

package stream

import (
	"context"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

// nextConn advances the fake clock until the upstream connects again. It
// has no step limit: the pump arms its reconnect timer only when the
// scheduler runs it, and the test timeout ends a reconnect that never comes.
func nextConn(clk *clock.Fake, u *upstream) *conn {
	for {
		select {
		case c := <-u.connected:
			return c
		default:
			clk.Advance(100 * time.Millisecond)
			runtime.Gosched()
		}
	}
}

// A started upstream that ends is reconnected; readers of the stream keep
// reading and the tuner stays held by the one stream (D-88).
func TestBrokerReconnectsAfterDrop(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{IdleTimeout: time.Hour})
	u := newUpstream()
	info, c := openNew(t, b, u, u.source("ch1", "tuner", 1), []byte("ab"))
	r, err := b.Reader(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	readN(t, r, 2)

	_ = c.writer.Close()
	again := nextConn(clk, u)
	if _, err := again.writer.Write([]byte("cd")); err != nil {
		t.Fatal(err)
	}
	if got := string(readN(t, r, 2)); got != "cd" {
		t.Fatalf("after reconnect %q", got)
	}
	if b.InUse("tuner") != 1 || len(b.Streams()) != 1 {
		t.Fatalf("held %d, streams %v", b.InUse("tuner"), b.Streams())
	}
}

// A started upstream that sends nothing for the stall timeout is dropped
// and reconnected.
func TestBrokerReconnectsAfterStall(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{IdleTimeout: time.Hour, StallTimeout: 5 * time.Second})
	u := newUpstream()
	info, c := openNew(t, b, u, u.source("ch1", "tuner", 1), []byte("ab"))
	r, err := b.Reader(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	clk.Advance(4 * time.Second)
	if _, err := c.writer.Write([]byte("c")); err != nil {
		t.Fatal(err)
	}
	if got := string(readN(t, r, 3)); got != "abc" {
		t.Fatalf("read %q", got)
	}
	clk.Advance(4 * time.Second)
	runtime.Gosched()
	if c.ctx.Err() != nil {
		t.Fatal("dropped an upstream that sent data inside the timeout")
	}
	again := nextConn(clk, u)
	<-c.ctx.Done()
	if _, err := again.writer.Write([]byte("d")); err != nil {
		t.Fatal(err)
	}
	if got := string(readN(t, r, 1)); got != "d" {
		t.Fatalf("read %q", got)
	}
}

// Reconnects that bring no data end the stream after MaxReconnects; readers
// drain and get EOF, and the tuner is free.
func TestBrokerReconnectLimit(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{IdleTimeout: time.Hour, MaxReconnects: 2})
	u := newUpstream()
	info, c := openNew(t, b, u, u.source("ch1", "tuner", 1), []byte("ab"))
	r, err := b.Reader(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	drained := make(chan string, 1)
	go func() {
		got, _ := io.ReadAll(r)
		drained <- string(got)
	}()

	_ = c.writer.Close()
	for range 2 {
		_ = nextConn(clk, u).writer.Close()
	}
	if got := <-drained; got != "ab" {
		t.Fatalf("drained %q", got)
	}
	if u.count() != 3 || b.InUse("tuner") != 0 {
		t.Fatalf("connects %d, held %d", u.count(), b.InUse("tuner"))
	}
}

// Close during the reconnect wait stops the pump without another connect.
func TestBrokerCloseDuringReconnectWait(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	b := newBroker(t, clk, Options{IdleTimeout: time.Hour})
	u := newUpstream()
	info, c := openNew(t, b, u, u.source("ch1", "tuner", 1), []byte("ab"))
	_ = c.writer.Close()
	b.Release(info.ID)
	waitNoBuffers(t, b)
	if u.count() != 1 || b.InUse("tuner") != 0 {
		t.Fatalf("connects %d, held %d", u.count(), b.InUse("tuner"))
	}
}
