// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package code

import "testing"

func TestParseDeclTypeScript(t *testing.T) {
	src := `import { x } from "y";

// fetchUser loads a user.
export function fetchUser(id: string): User {
  return db.get(id);
}

export const makeClient = (opts: Opts) => new Client(opts);

export default class Repo {
  find() {}
}

export interface User { id: string }

export type ID = string;

export enum Color { Red, Green }

function helper() { return 1; }
`
	cf := &CodeFile{Path: "app/x.ts", Lang: "ts"}
	parseDecls(cf, []byte(src), "ts")
	got := symByName(cf)

	want := map[string]string{
		"fetchUser":  "func",
		"makeClient": "func",
		"Repo":       "class",
		"User":       "interface",
		"ID":         "type",
		"Color":      "enum",
		"helper":     "func",
	}
	for name, kind := range want {
		s, ok := got[name]
		if !ok {
			t.Errorf("missing %q", name)
			continue
		}
		if s.Kind != kind {
			t.Errorf("%q kind = %q, want %q", name, s.Kind, kind)
		}
	}
	if s := got["fetchUser"]; s.Doc == "" {
		t.Error("fetchUser lost its leading doc comment")
	}
}

func TestParseDeclPython(t *testing.T) {
	src := `import os

class Service:
    def handle(self, req):
        return req

def main():
    pass
`
	cf := &CodeFile{Path: "svc/x.py", Lang: "py"}
	parseDecls(cf, []byte(src), "py")
	got := symByName(cf)

	if s, ok := got["Service"]; !ok || s.Kind != "class" {
		t.Errorf("Service = %+v, want class", s)
	}
	if s, ok := got["handle"]; !ok || s.Kind != "method" {
		t.Errorf("handle = %+v, want method (indented def)", s)
	}
	if s, ok := got["main"]; !ok || s.Kind != "func" {
		t.Errorf("main = %+v, want func (top-level def)", s)
	}
}

func TestParseDeclNoFalsePositives(t *testing.T) {
	// A plain value const and markup lines must not become symbols.
	src := `const count = 3;
const items = [1, 2, 3];
<div class="card">hello</div>
let total = count + 1;
`
	cf := &CodeFile{Path: "a/x.tsx", Lang: "tsx"}
	parseDecls(cf, []byte(src), "tsx")
	if len(cf.Symbols) != 0 {
		t.Errorf("expected no symbols from value consts + markup, got %v", cf.Symbols)
	}
}
