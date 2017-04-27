// +build !solaris

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd/osutils"
	runc "github.com/crosbymichael/go-runc"
	"github.com/tonistiigi/fifo"
	"golang.org/x/net/context"
)

// openIO opens the pre-created fifo's for use with the container
// in RDWR so that they remain open if the other side stops listening
func (p *process) openIO() error {
	p.stdio = &stdio{}
	var (
		uid = p.state.RootUID
		gid = p.state.RootGID
	)

	ctx, _ := context.WithTimeout(context.Background(), 15*time.Second)

	stdinCloser, err := fifo.OpenFifo(ctx, p.state.Stdin, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	p.stdinCloser = stdinCloser

	if p.state.Terminal {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		socket, err := runc.NewConsoleSocket(filepath.Join(cwd, "pty.sock"))
		if err != nil {
			return err
		}
		p.socket = socket

		go func() error {
			master, err := socket.ReceiveMaster()
			if err != nil {
				return err
			}
			p.console = master
			stdin, err := fifo.OpenFifo(ctx, p.state.Stdin, syscall.O_RDONLY, 0)
			if err != nil {
				return err
			}
			go io.Copy(master, stdin)
			stdoutw, err := fifo.OpenFifo(ctx, p.state.Stdout, syscall.O_WRONLY, 0)
			if err != nil {
				return err
			}
			stdoutr, err := fifo.OpenFifo(ctx, p.state.Stdout, syscall.O_RDONLY, 0)
			if err != nil {
				return err
			}
			p.Add(1)
			p.ioCleanupFn = func() {
				master.Close()
				stdoutr.Close()
				stdoutw.Close()
			}
			go func() {
				io.Copy(stdoutw, master)
				p.Done()
			}()
			return nil
		}()
		return nil
	}
	i, err := p.initializeIO(uid, gid)
	if err != nil {
		return err
	}
	p.shimIO = i
	// non-tty
	var ioClosers []io.Closer
	for _, pair := range []struct {
		name string
		dest func(wc io.WriteCloser, rc io.Closer)
	}{
		{
			p.state.Stdout,
			func(wc io.WriteCloser, rc io.Closer) {
				p.Add(1)
				go func() {
					io.Copy(wc, i.Stdout)
					p.Done()
				}()
			},
		},
		{
			p.state.Stderr,
			func(wc io.WriteCloser, rc io.Closer) {
				p.Add(1)
				go func() {
					io.Copy(wc, i.Stderr)
					p.Done()
				}()
			},
		},
	} {
		fw, err := fifo.OpenFifo(ctx, pair.name, syscall.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("containerd-shim: opening %s failed: %s", pair.name, err)
		}
		fr, err := fifo.OpenFifo(ctx, pair.name, syscall.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("containerd-shim: opening %s failed: %s", pair.name, err)
		}
		pair.dest(fw, fr)
		ioClosers = append(ioClosers, fw, fr)
	}

	f, err := fifo.OpenFifo(ctx, p.state.Stdin, syscall.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("containerd-shim: opening %s failed: %s", p.state.Stdin, err)
	}
	ioClosers = append(ioClosers, i.Stdin, f)
	p.ioCleanupFn = func() {
		for _, c := range ioClosers {
			c.Close()
		}
	}
	go func() {
		io.Copy(i.Stdin, f)
	}()

	return nil
}

func (p *process) Wait() {
	p.WaitGroup.Wait()
	if p.ioCleanupFn != nil {
		p.ioCleanupFn()
	}
}

func (p *process) killAll() error {
	if !p.state.Exec {
		cmd := exec.Command(p.runtime, append(p.state.RuntimeArgs, "kill", "--all", p.id, "SIGKILL")...)
		cmd.SysProcAttr = osutils.SetPDeathSig()
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %v", out, err)
		}
	}
	return nil
}
