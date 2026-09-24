// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
// Run with node --test or bun test. Executes the actual search view against a
// minimal DOM adapter; this is rendering logic coverage, not a browser journey.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

test("search view displays and escapes guidance warnings, including compact cards", async () => {
  const handlers = {};
  const results = { innerHTML: "", querySelectorAll: () => [] };
  const input = {
    value: "guidance", focus() {},
    addEventListener(name, handler) { handlers[name] = handler; },
    parentElement: { appendChild() {} },
  };
  let run;
  const document = {
    createElement(tag) {
      if (tag === "button") return { addEventListener(name, handler) { if (name === "click") run = handler; } };
      return { querySelector: (selector) => selector === "#srch-q" ? input : results };
    },
  };
  const window = {};
  vm.runInNewContext(readFileSync(new URL("../internal/web/assets/search.js", import.meta.url), "utf8"), { window, document, clearTimeout });
  let cards = [
    { NoteID: "incomplete", Title: "Historical claim", Tier0: true, Snippet: "", MissingGuidance: ["do", "why"], SupersededBy: "replacement" },
    { NoteID: "complete", Title: "Authored guidance", Tier0: true },
  ];
  window.Mesh.views.search({ replaceChildren() {} }, { searchOnSubmit: true, api: async () => ({ cards, tokens: 123 }) });
  await run();
  assert.match(results.innerHTML, /Incomplete guidance: missing do, why; verify before relying on this note\./);
  assert.equal((results.innerHTML.match(/rc-warning/g) || []).length, 2);
  assert.equal((results.innerHTML.match(/rc-superseded/g) || []).length, 1);
  assert.match(results.innerHTML, /Superseded: historical note/);
  assert.match(results.innerHTML, /data-id="replacement">Read replacement<\/button>/);
  assert.match(results.innerHTML, /data-id="complete"/);
  cards = [{ NoteID: "x", MissingGuidance: ['<img src=x onerror="boom">'], SupersededBy: '\"><img src=x onerror="boom">' }];
  await run();
  assert.doesNotMatch(results.innerHTML, /<img/);
  assert.match(results.innerHTML, /&lt;img/);
  assert.match(results.innerHTML, /data-id="&quot;&gt;&lt;img/);
  for (const pointer of [undefined, null, "", 123, {}, []]) {
    cards = [{ NoteID: "x", SupersededBy: pointer }];
    await run();
    assert.doesNotMatch(results.innerHTML, /rc-superseded|rc-replacement/);
  }
});
