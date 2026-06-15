---
id: modernc-cannot-load-c-extensions
type: gotcha
title: Modernc cannot load C extensions
when: "2026-06-16"
created: "2026-06-16"
tags:
    - sqlite
    - vectors
do: Use a flat []float32 + brute-force cosine for vectors on every platform; pure-Go HNSW is the scale path
dont: Depend on sqlite-vec/vec0 or any C extension
why: modernc.org/sqlite is a pure-Go translation with no C-extension loading; there is no 'where available'
---

# Modernc cannot load C extensions

## Symptom
<!-- TODO: how the problem shows up -->

## Cause
<!-- TODO: the root cause -->

## Fix
<!-- TODO: the resolution or workaround -->
