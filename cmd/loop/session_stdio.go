package main

import (
	"io"
	"os"
	"sync"
)

// hookStdout redirects stdout and returns a function to restore it.
func hookStdout(orig *os.File, forward io.Writer,
	onChunk func([]byte)) (func() error, error) {

	return hookOutput(
		func(f *os.File) { os.Stdout = f },
		orig,
		forward,
		onChunk,
	)
}

// hookStderr redirects stderr and returns a function to restore it.
func hookStderr(orig *os.File, forward io.Writer,
	onChunk func([]byte)) (func() error, error) {

	return hookOutput(
		func(f *os.File) { os.Stderr = f },
		orig,
		forward,
		onChunk,
	)
}

// hookOutput redirects an output stream and returns a restore function.
func hookOutput(setDest func(*os.File), orig *os.File, forward io.Writer,
	onChunk func([]byte)) (func() error, error) {

	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	setDest(w)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer r.Close()

		writer := composeWriter(forward, onChunk)
		_, _ = io.Copy(writer, r)
	}()

	return func() error {
		setDest(orig)
		_ = w.Close()
		wg.Wait()

		return nil
	}, nil
}

// hookStdin redirects stdin and returns a function to restore it.
func hookStdin(orig *os.File, source io.Reader,
	onChunk func([]byte)) (func() error, error) {

	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	useOrig := false
	if source == nil {
		source = orig
		useOrig = true
	} else if source == orig {
		useOrig = true
	}

	if onChunk != nil {
		source = io.TeeReader(source, chunkWriter(onChunk))
	}

	os.Stdin = r

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer w.Close()

		_, _ = io.Copy(w, source)
	}()

	return func() error {
		os.Stdin = orig
		_ = r.Close()
		_ = w.Close()
		if !useOrig {
			wg.Wait()
		}

		return nil
	}, nil
}

// composeWriter builds a writer that forwards to the provided sinks.
func composeWriter(forward io.Writer, onChunk func([]byte)) io.Writer {
	switch {
	case forward != nil && onChunk != nil:
		return io.MultiWriter(forward, chunkWriter(onChunk))

	case forward != nil:
		return forward

	case onChunk != nil:
		return chunkWriter(onChunk)

	default:
		return io.Discard
	}
}

// chunkWriter writes copy-safe chunks to a callback.
type chunkWriter func([]byte)

// Write copies p before forwarding to the callback.
func (w chunkWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	copyBuf := make([]byte, len(p))
	copy(copyBuf, p)
	w(copyBuf)

	return len(p), nil
}
