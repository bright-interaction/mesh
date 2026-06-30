// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package code

import "testing"

const goSrc = `package demo

import (
	"fmt"
	"strings"
)

// Greeter greets people.
type Greeter struct{ name string }

// Shape is a shape.
type Shape interface{ Area() float64 }

type Celsius float64

// MaxRetries caps retries.
const MaxRetries = 3

var Logger = fmt.Println

// Hello returns a greeting.
func Hello(name string) string {
	return strings.ToUpper(greet(name))
}

func greet(n string) string { return "hi " + n }

// Greet greets via the receiver.
func (g *Greeter) Greet() string { return Hello(g.name) }
`

func symByName(cf *CodeFile) map[string]Symbol {
	m := map[string]Symbol{}
	for _, s := range cf.Symbols {
		m[s.Name] = s
	}
	return m
}

func TestParseGoSymbols(t *testing.T) {
	cf := &CodeFile{Path: "demo/demo.go", Lang: "go"}
	parseGo(cf, []byte(goSrc))

	if cf.Package != "demo" {
		t.Errorf("package = %q, want demo", cf.Package)
	}
	if len(cf.Imports) != 2 || cf.Imports[0] != "fmt" || cf.Imports[1] != "strings" {
		t.Errorf("imports = %v, want [fmt strings]", cf.Imports)
	}

	want := map[string]string{
		"Greeter":       "struct",
		"Shape":         "interface",
		"Celsius":       "type",
		"MaxRetries":    "const",
		"Logger":        "var",
		"Hello":         "func",
		"greet":         "func",
		"Greeter.Greet": "method",
	}
	got := symByName(cf)
	for name, kind := range want {
		s, ok := got[name]
		if !ok {
			t.Errorf("missing symbol %q", name)
			continue
		}
		if s.Kind != kind {
			t.Errorf("%q kind = %q, want %q", name, s.Kind, kind)
		}
		if s.Start <= 0 {
			t.Errorf("%q start line = %d, want > 0", name, s.Start)
		}
	}
	// Unexported package-level const/var is noise; only exported ones are indexed.
	if _, ok := got["name"]; ok {
		t.Error("indexed an unexported field/var")
	}
}

func TestParseGoCallGraph(t *testing.T) {
	cf := &CodeFile{Path: "demo/demo.go", Lang: "go"}
	parseGo(cf, []byte(goSrc))
	got := symByName(cf)

	hello := got["Hello"]
	if !contains(hello.Calls, "greet") || !contains(hello.Calls, "ToUpper") {
		t.Errorf("Hello.Calls = %v, want greet + ToUpper", hello.Calls)
	}
	if m := got["Greeter.Greet"]; !contains(m.Calls, "Hello") {
		t.Errorf("Greeter.Greet.Calls = %v, want Hello", m.Calls)
	}
}

func TestParseGoBroken(t *testing.T) {
	// A syntactically broken file must degrade to whatever parsed, never panic.
	cf := &CodeFile{Path: "x/x.go", Lang: "go"}
	parseGo(cf, []byte("package x\nfunc Good() {}\nfunc Bad( {\n"))
	if len(symByName(cf)) == 0 {
		t.Error("expected partial parse to recover at least one symbol")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
