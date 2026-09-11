// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package syncwire implements HTTP content coding for the Mesh sync protocol.
// The JSON schema remains in syncproto; this package only changes how those bytes
// travel over the wire.
package syncwire

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	EncodingZstd = "zstd"
	// ResponseLimit preserves the client's pre-zstd 256 MiB response ceiling,
	// but applies it after decompression so a small compression bomb cannot evade it.
	ResponseLimit int64 = 256 << 20
)

var ErrTooLarge = errors.New("decoded sync body exceeds limit")

// AcceptsZstd applies the relevant Accept-Encoding semantics, including q=0.
func AcceptsZstd(header string) bool {
	explicit := false
	explicitQuality := 0.0
	wildcardQuality := 0.0
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		name := strings.ToLower(strings.TrimSpace(fields[0]))
		if name != EncodingZstd && name != "*" {
			continue
		}
		quality := 1.0
		for _, param := range fields[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
				continue
			}
			q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || q < 0 || q > 1 {
				quality = 0
			} else {
				quality = q
			}
		}
		if name == EncodingZstd {
			explicit = true
			explicitQuality = quality
		} else {
			wildcardQuality = quality
		}
	}
	if explicit {
		return explicitQuality > 0
	}
	return wildcardQuality > 0
}

func NewZstdReader(r io.Reader) (*zstd.Decoder, error) {
	return NewZstdReaderLimit(r, ResponseLimit)
}

func NewZstdReaderLimit(r io.Reader, limit int64) (*zstd.Decoder, error) {
	return zstd.NewReader(r,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(uint64(limit)),
	)
}

func NewZstdWriter(w io.Writer) (*zstd.Encoder, error) {
	return zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
	)
}

// ReadAllLimited reads at most limit decoded bytes and reports overflow instead
// of silently returning a truncated JSON document.
func ReadAllLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrTooLarge, limit)
	}
	return b, nil
}
