// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

// A stand-in that honors "q" ends within the grace period; Stop returns
// after the wait goroutine has closed Done.
func TestExecStopSendsQuitAndWaitsForTheGroup(t *testing.T) {
	stderr := &lockedBuffer{}
	process, err := StartExec(context.Background(), ExecOptions{
		Path: "/bin/sh", Args: []string{"-c", `read key; echo "got $key" >&2`},
		Stderr: stderr, Clock: clock.NewFake(time.Unix(0, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	process.Stop()
	select {
	case <-process.Done():
	default:
		t.Fatal("Done is open after Stop returned")
	}
	if process.Err() != nil || stderr.String() != "got q\n" {
		t.Fatalf("Err = %v, stderr = %q", process.Err(), stderr.String())
	}
}

// A stand-in that ignores "q" is killed by process group when the injected
// clock passes the grace period, children included.
func TestExecStopKillsTheGroupAfterTheGrace(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	process, err := StartExec(context.Background(), ExecOptions{
		Path: "/bin/sh", Args: []string{"-c", `trap "" INT TERM; sleep 600 & read key; wait`},
		Clock: fake, KillGrace: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		process.Stop()
		close(stopped)
	}()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-stopped:
			if process.Err() == nil {
				t.Fatal("a killed process reported success")
			}
			return
		case <-deadline.C:
			t.Fatal("Stop did not kill the group after the grace period")
		default:
		}
		fake.Advance(time.Second)
		runtime.Gosched()
	}
}

func TestExecStartRequiresAClock(t *testing.T) {
	if _, err := StartExec(context.Background(), ExecOptions{Path: "/bin/true"}); err == nil {
		t.Fatal("StartExec accepted a nil clock")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := StartExec(ctx, ExecOptions{Path: "/bin/true", Clock: clock.NewFake(time.Unix(0, 0))}); err == nil {
		t.Fatal("StartExec ran with a canceled context")
	}
}
