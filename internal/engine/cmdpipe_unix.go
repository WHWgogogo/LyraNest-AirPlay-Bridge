//go:build !windows

package engine

import (
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

// cmdPipe is the write end of a cliairplay --cmdpipe named pipe (FIFO).
type cmdPipe struct {
	path string
	file *os.File
}

// openCmdPipe creates the FIFO at path. The write end is opened lazily because
// opening a FIFO for writing blocks until the reader (cliairplay) attaches.
func openCmdPipe(path string) (*cmdPipe, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale cmdpipe %s: %w", path, err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		return nil, fmt.Errorf("mkfifo %s: %w", path, err)
	}
	return &cmdPipe{path: path}, nil
}

// Attach opens the write end, waiting up to timeout for the reader.
func (p *cmdPipe) Attach(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		file, err := os.OpenFile(p.path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			p.file = file
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cmdpipe %s never became writable: %w", p.path, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (p *cmdPipe) Write(command string) error {
	if p.file == nil {
		return fmt.Errorf("cmdpipe %s is not attached", p.path)
	}
	_, err := io.WriteString(p.file, command)
	return err
}

func (p *cmdPipe) Close() error {
	var err error
	if p.file != nil {
		err = p.file.Close()
	}
	if removeErr := os.Remove(p.path); removeErr != nil && !os.IsNotExist(removeErr) && err == nil {
		err = removeErr
	}
	return err
}
