// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var ErrConfinedFileTooLarge = errors.New("note file too large")

// ReadConfinedFile reads a regular file through held vault directory handles,
// refusing symlink components and replacements between inspection and open.
// maxBytes=0 means no byte cap. Cancellation is cooperative between filesystem
// calls; this synchronous primitive starts no worker that could outlive return.
func ReadConfinedFile(ctx context.Context, root, rel string, maxBytes int) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes < 0 || !filepath.IsLocal(rel) {
		return nil, errors.New("invalid confined read")
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { dir.Close() }()
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		before, err := dir.Lstat(part)
		if err != nil {
			return nil, err
		}
		if !before.IsDir() {
			return nil, errors.New("not a note directory")
		}
		next, err := dir.OpenRoot(part)
		if err != nil {
			return nil, err
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			next.Close()
			return nil, errors.New("note directory changed")
		}
		dir.Close()
		dir = next
	}
	name := parts[len(parts)-1]
	info, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular note")
	}
	if maxBytes > 0 && info.Size() > int64(maxBytes) {
		return nil, ErrConfinedFileTooLarge
	}
	file, err := dir.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("note file changed")
	}
	var body []byte
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, readErr := file.Read(buffer)
		if err := ctx.Err(); err != nil {
			return nil, err // no late allocation or next read after cancellation
		}
		if maxBytes > 0 && len(body)+n > maxBytes {
			return nil, ErrConfinedFileTooLarge
		}
		body = append(body, buffer[:n]...)
		if readErr == io.EOF {
			return body, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

// ReadConfinedFileContext adds the same caller-owned wait as ReadFileContext.
// One kernel read may outlive cancellation; its private result cannot change a
// completed response, and the synchronous reader stops when that read returns.
// Joined batch workers use ReadConfinedFile instead, never this detached wait.
func ReadConfinedFileContext(ctx context.Context, root, rel string, maxBytes int) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return readBytesContext(ctx, func() ([]byte, error) {
		return ReadConfinedFile(ctx, root, rel, maxBytes)
	})
}
