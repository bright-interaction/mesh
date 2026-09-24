// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
// Rendered fixture acceptance. Every HTTP request is intercepted; no live server,
// credentials, release download, installation, or process restart is used.
// MESH_BROWSER_MODULE=/path/to/playwright/index.mjs bun mesh/scripts/coordinated-updates.browser.mjs
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const { chromium } = await import(process.env.MESH_BROWSER_MODULE || "playwright");
const asset = name => readFileSync(new URL("../internal/web/assets/" + name, import.meta.url), "utf8");
const html = asset("index.html").replace(/<script\b[^>]*>[\s\S]*?<\/script>/g, "").replace("__MESH_BASE__", "/").replace('id="overlay" class="overlay"', 'id="overlay" class="overlay hidden"');
const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage({ viewport: { width: 1100, height: 900 } });
  const errors = [], requests = [];
  page.on("pageerror", error => errors.push(error.message));
  let response = { release: "v0.41.6", update: { available: false }, counts: { notes: 3 }, signals: {} };
  let responseCode = 200;
  let documentRequests = 0;
  await page.route("**/*", async route => {
    const req = route.request(), url = new URL(req.url());
    assert.equal(url.origin, "https://mesh-update-fixture.invalid", "no remote release or privileged request");
    assert.equal(req.method(), "GET", "updates must not introduce mutations");
    if (url.pathname === "/") {
      documentRequests++;
      return route.fulfill({ contentType: "text/html", body: html });
    }
    if (url.pathname === "/assets/style.css") return route.fulfill({ contentType: "text/css", body: asset("style.css") });
    if (["/assets/fonts/geist.woff2", "/assets/fonts/jetbrains-mono.woff2"].includes(url.pathname)) {
      return route.fulfill({ contentType: "font/woff2", body: readFileSync(new URL("../internal/web" + url.pathname, import.meta.url)) });
    }
    assert.equal(url.pathname, "/api/status", "only the existing status endpoint is used");
    requests.push(url.pathname);
    return route.fulfill({ status: responseCode, json: response });
  });
  await page.goto("https://mesh-update-fixture.invalid/");
  await page.addScriptTag({ content: asset("shell.js") });
  await page.waitForFunction(() => window.Mesh.status?.release === "v0.41.6");
  assert.equal(requests.length, 1, "one existing status request on startup");
  const dialog = page.getByRole("dialog", { name: "Update Mesh", exact: true });
  await page.locator("#mesh-update-open").focus();
  await page.keyboard.press("Enter");
  await dialog.waitFor();
  assert.equal(requests.length, 1, "opening update details does not fetch or install");
  assert.equal(await page.locator("#mesh-update-current").textContent(), "v0.41.6", "release independent of update discovery");
  assert.equal(await page.locator("#mesh-update-latest").textContent(), "Unknown");
  assert.match(await page.locator("#mesh-update-summary").textContent(), /unknown/);
  assert.match(await dialog.innerText(), /server operator manages/);
  assert.match(await dialog.innerText(), /local Mesh command does not update a hosted server/);
  assert.equal(await page.locator("#mesh-update-release").isVisible(), false);
  assert.equal(await page.locator("#update-banner").isVisible(), false);

  const refresh = async next => {
    response = next;
    await page.locator("#mesh-update-refresh").click();
    await page.waitForFunction(() => !document.getElementById("mesh-update-refresh").disabled);
  };
  await refresh({ release: "v0.41.6", update: { available: true, current: "v0.41.6", latest: "v0.41.7", url: "javascript:alert(1)", command: "sh malicious-command", prebuilt_command: "curl hostile-command" } });
  assert.match(await page.locator("#mesh-update-summary").textContent(), /newer server release.*Operator approval/);
  assert.equal(await page.locator("#mesh-update-release").getAttribute("href"), "https://github.com/bright-interaction/mesh/tree/v0.41.7");
  assert.doesNotMatch(await dialog.innerText(), /malicious-command|hostile-command|javascript:/);
  await page.keyboard.press("Escape");
  assert.equal(await dialog.isVisible(), false);
  assert.equal(await page.locator("#mesh-update-open").evaluate(el => el === document.activeElement), true, "dialog restores keyboard focus");
  await page.locator("#update-details").click();
  await dialog.waitFor();
  assert.equal(requests.length, 2, "banner uses the same dialog without background work");
  await page.locator("#mesh-update-close").click();
  await page.locator("#update-dismiss").click();
  assert.equal(await page.locator("#update-banner").isVisible(), false);
  await page.locator("#mesh-update-open").click();
  assert.match(await page.locator("#mesh-update-summary").textContent(), /newer server release/, "dismissal does not hide persistent update entry");

  await refresh({ release: "v0.41.7", update: { available: false, current: "v0.41.7", latest: "v0.41.7" } });
  assert.match(await page.locator("#mesh-update-summary").textContent(), /No newer server release/);
  assert.equal(await page.locator("#update-banner").isVisible(), false);
  await refresh({ update: { available: true, current: "v0.41.5", latest: "v0.41.7" } });
  assert.equal(await page.locator("#mesh-update-current").textContent(), "v0.41.5", "legacy status remains usable");
  assert.match(await page.locator("#mesh-update-summary").textContent(), /newer server release/);
  await refresh({ release: "v0.41.6", update: { available: false, current: "v0.41.5", latest: "v0.41.7" } });
  assert.match(await page.locator("#mesh-update-summary").textContent(), /unknown/, "mismatched server and update identities are not current");
  for (const [current, latest, available, pattern] of [
    ["v0.41.6", "v0.41.7", false, /unknown/],
    ["v0.41.7", "v0.41.6", true, /unknown/],
    ["v0.41.7+build.1", "v0.41.7+build.2", true, /unknown/],
    ["v0.41.7-rc.9", "v0.41.7-rc.10", true, /newer server release/],
    ["v0.41.7-rc.1", "v0.41.7", true, /newer server release/],
    ["v0.41.7", "v0.41.7-rc.1", false, /No newer server release/],
  ]) {
    await refresh({ release: current, update: { available, current, latest } });
    assert.match(await page.locator("#mesh-update-summary").textContent(), pattern, "version ordering and availability must agree");
  }

  for (const hostile of ['v0.41.7/<img src=x onerror=alert(1)>', "javascript:alert(1)", "v01.2.3", "v0.1.2-01", { value: "v0.41.7" }]) {
    await refresh({ release: hostile, update: { available: true, current: hostile, latest: hostile, url: "https://attacker.invalid/", command: "execute-me" } });
    assert.equal(await page.locator("#mesh-update-current").textContent(), "Unknown");
    assert.equal(await page.locator("#mesh-update-latest").textContent(), "Unknown");
    assert.equal(await page.locator("#mesh-update-release").getAttribute("href"), null);
    assert.equal(await page.locator("#mesh-update-dialog img, #mesh-update-dialog script").count(), 0);
    assert.equal(await page.locator("#update-banner").isVisible(), false);
  }
  await refresh({ release: "v0.41.7-rc.1+build.1", update: { available: false, current: "v0.41.7-rc.1+build.1", latest: "v0.41.7-rc.1+build.1" } });
  assert.match(await page.locator("#mesh-update-summary").textContent(), /No newer server release/);
  assert.equal(await page.locator("#mesh-update-release").getAttribute("href"), "https://github.com/bright-interaction/mesh/tree/v0.41.7-rc.1%2Bbuild.1");

  responseCode = 503;
  await refresh({ error: "fixture unavailable" });
  assert.match(await page.locator("#mesh-update-summary").textContent(), /unknown/, "failed refresh clears obsolete claims");
  assert.equal(await page.locator("#mesh-update-current").textContent(), "Unknown");
  assert.equal(await page.locator("#mesh-update-release").getAttribute("href"), null);
  responseCode = 200;
  await refresh({ release: "v0.41.6", update: {} });
  await page.setViewportSize({ width: 390, height: 844 });
  const box = await dialog.boundingBox();
  assert.ok(box.x >= 0 && box.x + box.width <= 390, "dialog fits mobile width");
  assert.ok(await page.locator("#mesh-update-refresh").isVisible());
  assert.equal(await dialog.evaluate(el => el.scrollWidth > el.clientWidth), false, "no horizontal overflow");

  responseCode = 401;
  await refresh({ error: "revoked fixture" });
  assert.equal(await dialog.isVisible(), false, "auth prompt is not trapped behind a modal after revocation");
  assert.equal(await page.locator("#login").isVisible(), true);
  assert.equal(await page.locator("#mesh-update-current").textContent(), "Unknown");
  assert.equal(await page.locator("#update-banner").isVisible(), false);
  assert.deepEqual(errors, []);
  assert.equal(documentRequests, 1, "no automatic reload");
  // The IDE uses the same shell with its native updater and host-owned CSS.
  // Hiding its browser controls must also remove the banner layout offset.
  responseCode = 200;
  response = { release: "v0.41.6", update: { current: "v0.41.6", latest: "v0.41.7", available: true } };
  await page.addStyleTag({ content: readFileSync(new URL("../ide/src/view.css", import.meta.url), "utf8") });
  await page.evaluate(() => { window.Mesh.readOnlyViewer = true; document.getElementById("mesh-update-refresh").click(); });
  await page.waitForFunction(() => !document.getElementById("mesh-update-refresh").disabled);
  assert.equal(await page.locator("#mesh-update-open").isVisible(), false);
  assert.equal(await page.locator("#update-banner").isVisible(), false);
  assert.equal(await page.locator("body").evaluate(el => el.classList.contains("update-available")), false, "IDE reserves no hidden-banner space");
  console.log(JSON.stringify({ result: "passed", intercepted_status_requests: requests.length, live_requests: 0 }));
} finally {
  await browser.close();
}
