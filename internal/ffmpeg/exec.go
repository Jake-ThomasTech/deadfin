// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

// ExecOptions describes one standalone ffmpeg process. The transcode worker
// uses it to run a job that a server placed on it.
type ExecOptions struct {
	Path   string
	Args   []string
	Dir    string
	Stderr io.Writer
	Clock  clock.Clock
	// KillGrace is the time between "q" on stdin and SIGKILL to the group.
	KillGrace time.Duration
}

// Exec is one ffmpeg process in its own process group (CONVENTIONS 5.2).
// One goroutine waits for the process; Stop and Done end it.
type Exec struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	clock     clock.Clock
	killGrace time.Duration
	done      chan struct{}
	err       error

	mu       sync.Mutex
	stopOnce sync.Once
}

// StartExec starts ffmpeg. The context bounds only the start.
func StartExec(ctx context.Context, options ExecOptions) (*Exec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Clock == nil {
		return nil, errors.New("ffmpeg exec clock is nil")
	}
	if options.KillGrace <= 0 {
		options.KillGrace = 5 * time.Second
	}
	command := exec.Command(options.Path, options.Args...)
	command.Dir = options.Dir
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = io.Discard
	command.Stderr = options.Stderr
	if command.Stderr == nil {
		command.Stderr = io.Discard
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	process := &Exec{cmd: command, stdin: stdin, clock: options.Clock, killGrace: options.KillGrace, done: make(chan struct{})}
	go process.wait()
	return process, nil
}

func (e *Exec) wait() {
	err := e.cmd.Wait()
	_ = reapProcessGroup(e.cmd.Process.Pid)
	e.mu.Lock()
	e.err = err
	e.mu.Unlock()
	close(e.done)
}

// Stdin forwards keystrokes to ffmpeg.
func (e *Exec) Stdin() io.Writer { return e.stdin }

// Done closes when the process group is gone.
func (e *Exec) Done() <-chan struct{} { return e.done }

// Err is the exit result. It is valid after Done closes.
func (e *Exec) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// Stop sends "q", waits KillGrace, then kills the group. It returns after
// the process group is gone.
func (e *Exec) Stop() {
	e.stopOnce.Do(func() {
		_, _ = io.WriteString(e.stdin, "q\n")
		grace := e.clock.NewTimer(e.killGrace)
		select {
		case <-e.done:
			stopTimer(grace)
		case <-grace.C():
			_ = syscall.Kill(-e.cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = e.stdin.Close()
	})
	<-e.done
}
