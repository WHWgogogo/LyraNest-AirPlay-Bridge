//go:build windows

package engine

import (
	"errors"
	"time"
)

// ErrCmdPipeUnsupported is returned on platforms without FIFO support. The
// cliairplay kernel has no Windows build, so the native kernel is the only
// option there.
var ErrCmdPipeUnsupported = errors.New("cliairplay --cmdpipe requires a POSIX named pipe; the cliairplay kernel is not available on Windows")

type cmdPipe struct {
	path string
}

func openCmdPipe(path string) (*cmdPipe, error) {
	return nil, ErrCmdPipeUnsupported
}

func (p *cmdPipe) Attach(timeout time.Duration) error { return ErrCmdPipeUnsupported }

func (p *cmdPipe) Write(command string) error { return ErrCmdPipeUnsupported }

func (p *cmdPipe) Close() error { return nil }
