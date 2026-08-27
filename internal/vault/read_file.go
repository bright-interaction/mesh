// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
)

type fileReadResult struct {
	data []byte
	err  error
}

// ReadFileContext is os.ReadFile with a caller-owned wait. Filesystem reads do not
// accept a context and can block indefinitely on a FIFO, FUSE mount, or unhealthy
// network filesystem. Isolating the read behind a one-result buffered channel lets the
// caller stop waiting immediately; a read that eventually completes can only publish
// into that private channel, never into caller-owned state after cancellation.
func ReadFileContext(ctx context.Context, path string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Preserve os.ReadFile's low-overhead path for legacy, non-cancellable callers.
	if ctx.Done() == nil {
		return os.ReadFile(path)
	}
	return readBytesContext(ctx, func() ([]byte, error) {
		return readFileCooperatively(ctx, path)
	})
}

// readFileCooperatively bounds the work a detached read can do after cancellation.
// One kernel read may remain blocked, but once it returns the cancellation check stops
// the goroutine before it appends that chunk or issues another read. This avoids the
// unbounded post-cancellation allocation os.ReadFile could otherwise perform.
func readFileCooperatively(ctx context.Context, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return readAllCooperatively(ctx, f)
}

func readAllCooperatively(ctx context.Context, r io.Reader) ([]byte, error) {
	const chunkSize = 32 << 10
	chunk := make([]byte, chunkSize)
	var data []byte
	for {
		n, readErr := r.Read(chunk)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if n > 0 {
			data = append(data, chunk[:n]...)
		}
		switch readErr {
		case nil:
			continue
		case io.EOF:
			return data, nil
		default:
			return nil, readErr
		}
	}
}

// ReadFileHeadContext reads at most maxBytes from the start of path with the same
// cancellation guarantee as ReadFileContext. EOF and a short read are successful and
// return the bytes obtained, matching the bounded frontmatter scan's historic behavior.
func ReadFileHeadContext(ctx context.Context, path string, maxBytes int) ([]byte, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("read file head: negative byte limit %d", maxBytes)
	}
	return readBytesContext(ctx, func() ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		head := make([]byte, maxBytes)
		n, err := io.ReadFull(f, head)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			err = nil
		}
		return head[:n], err
	})
}

type contextFileInfoResult struct {
	info fs.FileInfo
	err  error
}

// StatContext is os.Stat with a caller-owned wait. Like ReadFileContext, any late
// filesystem result is confined to a private buffered channel and the detached work is
// strictly read-only.
func StatContext(ctx context.Context, path string) (fs.FileInfo, error) {
	return fileInfoContext(ctx, func() (fs.FileInfo, error) { return os.Stat(path) })
}

// LstatContext is os.Lstat with a caller-owned wait. It is useful when a caller must
// reject symlinks without following them onto a potentially unhealthy target mount.
func LstatContext(ctx context.Context, path string) (fs.FileInfo, error) {
	return fileInfoContext(ctx, func() (fs.FileInfo, error) { return os.Lstat(path) })
}

func fileInfoContext(ctx context.Context, stat func() (fs.FileInfo, error)) (fs.FileInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ctx.Done() == nil {
		return stat()
	}

	result := make(chan contextFileInfoResult, 1)
	go func() {
		info, err := stat()
		result <- contextFileInfoResult{info: info, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-result:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return r.info, r.err
	}
}

// readBytesContext is kept injectable so cancellation can be proved without relying on
// timing or a particular filesystem. Its result channel is buffered deliberately: when
// the caller has returned on cancellation, the isolated reader can still finish and
// exit without blocking or touching any state the caller can observe.
func readBytesContext(ctx context.Context, read func() ([]byte, error)) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Background and TODO contexts can never be cancelled. Keep legacy callers on
	// the direct path instead of paying for one goroutine per ordinary vault file.
	if ctx.Done() == nil {
		return read()
	}

	result := make(chan fileReadResult, 1)
	go func() {
		data, err := read()
		result <- fileReadResult{data: data, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-result:
		// Cancellation and completion can become ready together. Never hand a partial
		// read to a caller whose pass has already been cancelled.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return r.data, r.err
	}
}
