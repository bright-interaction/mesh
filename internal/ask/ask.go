// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

// Package ask answers a natural-language question from the team's own knowledge: it
// retrieves the relevant notes (the graph) plus the relevant code symbols (the code
// index), hands them to the BYOAI LLM as the ONLY allowed context, and returns the
// answer with citations. The conversational second brain, grounded in the vault, no
// hallucinated facts. Reuses the retriever + code index + llm.Client (claude -p).
package ask

import (
	"context"
	"fmt"
	"strings"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/llm"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

// Citation is a source the answer drew on, by its [N] number in the context.
type Citation struct {
	N     int    `json:"n"`
	Kind  string `json:"kind"` // note | code
	ID    string `json:"id"`
	Title string `json:"title"`
	Loc   string `json:"loc"` // note path or file:line
}

// Result is the grounded answer plus the sources it was given.
type Result struct {
	Answer    string     `json:"answer"`
	Citations []Citation `json:"citations"`
}

const system = `You answer a developer's question using ONLY the provided context, which is the team's own notes and source code. Do not use any outside or general knowledge. If the provided context does not contain the answer, reply exactly with "The vault has nothing on that." and nothing else, rather than guessing or answering from what you already know. Cite every claim with its source number like [2]. Be concise and concrete, and ground every statement in a cited source.`

// codeReadable reports whether the caller may see the source-code index. Code symbols
// carry no per-note scope, so the whole index is treated as dev-scoped (the same rule
// the MCP code tools enforce via codeScopeDenied): only an unrestricted caller or one
// who can read the dev scope gets code in their answer. Without this a scope-confined
// member's answer would leak dev source symbols (name, signature, file:line).
func codeReadable(allowedScopes map[string]bool) bool {
	return allowedScopes == nil || allowedScopes["dev"]
}

// Answer retrieves notes + code for the question, asks the LLM grounded on them, and
// returns the answer with the cited sources. allowedScopes (nil = unrestricted) keeps
// a scoped caller's answer within their readable notes.
func Answer(ctx context.Context, rtr *retrieve.Retriever, store *index.Store, client llm.Client, question string, budget int, allowedScopes map[string]bool) (Result, error) {
	if strings.TrimSpace(question) == "" {
		return Result{}, fmt.Errorf("empty question")
	}
	if budget <= 0 {
		budget = 3000
	}
	var cites []Citation
	var b strings.Builder
	n := 0

	if rtr != nil {
		cards, _ := rtr.Retrieve(ctx, question, retrieve.Options{Budget: budget, AllowedScopes: allowedScopes})
		for _, c := range cards {
			n++
			fmt.Fprintf(&b, "[%d] NOTE %q (%s)\n%s\n\n", n, c.Title, c.Path, strings.TrimSpace(c.Snippet))
			cites = append(cites, Citation{N: n, Kind: "note", ID: c.NoteID, Title: c.Title, Loc: c.Path})
		}
	}
	if store != nil && codeReadable(allowedScopes) {
		if hits, err := store.SearchCode(question, 5, nil); err == nil {
			for _, h := range hits {
				n++
				loc := fmt.Sprintf("%s:%d", h.Path, h.Line)
				fmt.Fprintf(&b, "[%d] CODE %s %s (%s)\n%s\n\n", n, h.Kind, h.Name, loc, strings.TrimSpace(h.Signature))
				cites = append(cites, Citation{N: n, Kind: "code", ID: h.ID, Title: h.Name, Loc: loc})
			}
		}
	}
	if n == 0 {
		return Result{Answer: "The vault has nothing on that yet.", Citations: nil}, nil
	}

	user := "Question: " + question + "\n\nContext:\n" + b.String()
	out, err := client.Complete(ctx, system, user)
	if err != nil {
		return Result{}, err
	}
	return Result{Answer: strings.TrimSpace(out), Citations: cites}, nil
}
