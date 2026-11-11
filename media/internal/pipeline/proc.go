package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// RunProgress executes ffmpeg and reports its progress as it goes. The args
// must include `-progress pipe:1`; HLSJob adds it when Progressive is set.
func (r Runner) RunProgress(ctx context.Context, args []string, onProgress func(Progress)) error {
	proc, err := r.Start(ctx, args, false, onProgress)
	if err != nil {
		return err
	}
	return proc.Wait()
}

// Proc is an ffmpeg that is still running.
type Proc struct {
	// Stdin is ffmpeg's input when it was started with one. Closing it is how
	// a live encode is ended politely: ffmpeg reads end-of-file, flushes its
	// encoders, closes the last segment and writes ENDLIST. Killing it instead
	// loses the segment it was in the middle of.
	Stdin io.WriteCloser

	cmd    *exec.Cmd
	ctx    context.Context
	cancel context.CancelFunc
	stderr *tailBuffer
	read   chan struct{}
	exited chan struct{}
	bin    string
	limit  time.Duration

	once sync.Once
	err  error
}

// Start launches ffmpeg and returns while it runs. stdin asks for a pipe to
// write its input into; onProgress, when not nil, is called from another
// goroutine with each `-progress` block.
func (r Runner) Start(ctx context.Context, args []string, stdin bool, onProgress func(Progress)) (*Proc, error) {
	cancel := context.CancelFunc(func() {})
	if r.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
	}
	cmd := exec.CommandContext(ctx, r.ffmpeg(), args...)
	// After a kill, do not wait forever on pipes a grandchild might still hold.
	cmd.WaitDelay = 10 * time.Second
	p := &Proc{
		cmd:    cmd,
		ctx:    ctx,
		cancel: cancel,
		stderr: &tailBuffer{limit: 4096},
		read:   make(chan struct{}),
		exited: make(chan struct{}),
		bin:    filepath.Base(r.ffmpeg()),
		limit:  r.Timeout,
	}
	cmd.Stderr = p.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if stdin {
		if p.Stdin, err = cmd.StdinPipe(); err != nil {
			cancel()
			return nil, err
		}
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("%s could not be started: %w", p.bin, err)
	}
	go func() {
		defer close(p.read)
		ReadProgress(stdout, onProgress)
	}()
	go func() { _ = p.Wait() }()
	return p, nil
}

// Wait blocks until ffmpeg exits and says how it went. Safe to call more than
// once and from more than one goroutine; every call gets the same answer.
func (p *Proc) Wait() error {
	p.once.Do(func() {
		defer close(p.exited)
		defer p.cancel()
		// The reader first: cmd.Wait closes the stdout pipe, and a report still
		// in it would be lost.
		<-p.read
		err := p.cmd.Wait()
		switch {
		case err == nil:
		case p.limit > 0 && errors.Is(p.ctx.Err(), context.DeadlineExceeded):
			p.err = fmt.Errorf("%s was still running after %s and was stopped", p.bin, p.limit)
		case p.ctx.Err() != nil:
			p.err = fmt.Errorf("%s was stopped: %w", p.bin, p.ctx.Err())
		default:
			p.err = fmt.Errorf("%s failed: %w: %s", p.bin, err, tail(p.stderr.String(), 800))
		}
	})
	<-p.exited
	return p.err
}

// Exited is closed once ffmpeg has gone, however it went.
func (p *Proc) Exited() <-chan struct{} { return p.exited }

// Kill stops ffmpeg now. For when asking nicely has already been tried.
func (p *Proc) Kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// tailBuffer keeps the last `limit` bytes written to it. A live encode runs
// for hours and ffmpeg can complain about every damaged packet in it; only the
// end of that is ever read, and the rest must not be held in memory.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.limit; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
