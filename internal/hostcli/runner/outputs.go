//go:build unix

package runner

import (
	"io"
	"os"
	"reflect"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/tty"
)

type pipe struct {
	read  *os.File
	write *os.File
	dest  io.Writer
	done  chan struct{}
}

// outputs connects the command's stdout and stderr either directly to files
// or through pipes that are copied to the destination and to the capture.
type outputs struct {
	stdout, stderr io.Writer
	pipes          []*pipe
}

func attach(o Options) (*outputs, error) {
	out := &outputs{}
	stdoutPipe := needsPipe(o.Stdout, o.Capture)
	stderrPipe := needsPipe(o.Stderr, o.Capture)
	switch {
	case stdoutPipe && stderrPipe && sameDestination(o.Stdout, o.Stderr):
		p, err := out.newPipe(o.Stdout)
		if err != nil {
			return nil, err
		}
		out.stdout, out.stderr = p.write, p.write
	default:
		var err error
		if out.stdout, err = out.connect(o.Stdout, stdoutPipe); err != nil {
			out.abandon()
			return nil, err
		}
		if out.stderr, err = out.connect(o.Stderr, stderrPipe); err != nil {
			out.abandon()
			return nil, err
		}
	}
	return out, nil
}

func (out *outputs) connect(dest io.Writer, piped bool) (io.Writer, error) {
	if piped {
		p, err := out.newPipe(dest)
		if err != nil {
			return nil, err
		}
		return p.write, nil
	}
	if f, ok := dest.(*os.File); ok && f != nil {
		return f, nil
	}
	return nil, nil
}

func (out *outputs) newPipe(dest io.Writer) (*pipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p := &pipe{read: r, write: w, dest: dest, done: make(chan struct{})}
	out.pipes = append(out.pipes, p)
	return p, nil
}

func needsPipe(dest io.Writer, capture io.Writer) bool {
	if f, ok := dest.(*os.File); ok && f != nil {
		return capture != nil && !tty.IsTerminal(f)
	}
	return dest != nil || capture != nil
}

func sameDestination(a, b io.Writer) bool {
	if fa, ok := a.(*os.File); ok {
		fb, ok := b.(*os.File)
		if !ok || fa == nil || fb == nil {
			return false
		}
		if fa == fb {
			return true
		}
		ia, errA := fa.Stat()
		ib, errB := fb.Stat()
		return errA == nil && errB == nil && os.SameFile(ia, ib)
	}
	if a == nil || b == nil || !reflect.TypeOf(a).Comparable() || !reflect.TypeOf(b).Comparable() {
		return false
	}
	return a == b
}

func (out *outputs) closeWriteEnds() {
	for _, p := range out.pipes {
		_ = p.write.Close()
	}
}

func (out *outputs) abandon() {
	for _, p := range out.pipes {
		_ = p.write.Close()
		_ = p.read.Close()
	}
}

func (out *outputs) startCopying(capture io.Writer) {
	for _, p := range out.pipes {
		go p.copy(capture)
	}
}

func (p *pipe) copy(capture io.Writer) {
	defer close(p.done)
	defer func() { _ = p.read.Close() }()
	buf := make([]byte, copyBufferSize)
	destFailed := p.dest == nil
	for {
		n, err := p.read.Read(buf)
		if n > 0 {
			if capture != nil {
				_, _ = capture.Write(buf[:n])
			}
			if !destFailed {
				if _, werr := p.dest.Write(buf[:n]); werr != nil {
					destFailed = true
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// drain waits for the copiers; a background process that keeps the output open
// must not keep the job from finishing.
func (out *outputs) drain(grace time.Duration) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for _, p := range out.pipes {
		select {
		case <-p.done:
		case <-timer.C:
			_ = p.read.Close()
			<-p.done
			for _, rest := range out.pipes {
				_ = rest.read.Close()
				<-rest.done
			}
			return
		}
	}
}
