package client

import (
	"io"
	"os"

	"golang.org/x/term"
)

// RawTerminal puts the local terminal into raw mode and returns a restore function.
func RawTerminal() (restore func(), err error) {
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() {
		term.Restore(fd, oldState)
	}, nil
}

// Proxy copies data bidirectionally between two ReadWriteClosers.
// Returns when either side closes or errors.
func Proxy(local io.ReadWriter, remote io.ReadWriter) error {
	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(remote, local)
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(local, remote)
		errCh <- err
	}()
	return <-errCh
}
