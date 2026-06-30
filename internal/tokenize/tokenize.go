// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

// Package tokenize counts tokens with a real BPE tokenizer: the cl100k_base
// vocabulary (the GPT-4 / GPT-3.5 tokenizer), a reproducible public standard.
// Mesh budgets retrieval to this count and the eval harness measures
// tokens-to-completion with it, so Gate 1 (beat grep+read on token cost) rests on
// a genuine token count instead of a chars-per-token estimate.
//
// Sovereignty: the vocabulary is compiled into the binary (the codec ships the
// merge ranks as Go source), so there is no runtime download. The BPE map is built
// once, lazily, on the first Count, so a mesh invocation that never budgets tokens
// pays nothing.
//
// Why cl100k_base: the default coding agent is Claude, whose exact tokenizer is not
// public, so no count is exact. cl100k is a real, widely-used BPE within a few
// percent of both Claude and o200k for English and code, counts slightly
// conservatively (the honest direction for a "we save tokens" claim), and is half
// the compiled size of o200k. The point is a real BPE, not a chars/4 guess; both
// retrieval arms in the eval count with this same function, so the Gate-1 ratio is
// near tokenizer-invariant regardless.
package tokenize

import (
	"sync"

	"github.com/tiktoken-go/tokenizer/codec"
)

// counter is the subset of the codec we use. Calling codec.NewCl100kBase directly
// (instead of tokenizer.Get, whose switch references every encoding's constructor)
// lets the linker drop the three unused vocabularies, keeping the binary lean.
type counter interface {
	Count(string) (int, error)
}

var (
	once sync.Once
	enc  counter
)

func load() {
	enc = codec.NewCl100kBase()
}

// Count returns the number of BPE tokens in s. If the codec is somehow
// unavailable it falls back to a fast char-class heuristic so token budgeting
// never breaks.
func Count(s string) int {
	if s == "" {
		return 0
	}
	once.Do(load)
	if enc != nil {
		if n, err := enc.Count(s); err == nil {
			return n
		}
	}
	return heuristic(s)
}

// heuristic is the fallback estimate (and the historical one): roughly one token
// per four word-characters, with each punctuation or symbol its own token.
func heuristic(s string) int {
	tokens := 0
	wlen := 0
	flush := func() {
		if wlen > 0 {
			tokens += (wlen + 3) / 4
			wlen = 0
		}
	}
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			wlen++
		case r == ' ' || r == '\n' || r == '\t' || r == '\r':
			flush()
		default:
			flush()
			tokens++
		}
	}
	flush()
	return tokens
}
