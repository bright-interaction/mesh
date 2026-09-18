// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const { readPath } = require('./client');
class Broker {
  constructor(client, send, actions = {}) { this.client = client; this.send = send; this.actions = actions; this.pending = new Map(); this.disposed = false; this.count = 0; this.window = Date.now(); }
  async handle(message) {
    if (this.disposed || !message || typeof message !== 'object') return;
    if (message.type === 'ready') { this.actions.ready?.(); return; }
    if (message.type !== 'request' || !Number.isSafeInteger(message.id) || message.id < 1 || this.pending.has(message.id)) return;
    const { id } = message;
    const reply = value => { if (!this.disposed) this.send({ type: 'response', id, ...value }); };
    if (Date.now() - this.window > 60000) { this.window = Date.now(); this.count = 0; }
    if (++this.count > 120 || this.pending.size >= 6) { reply({ error: 'Too many requests. Please try again.' }); return; }
    const controller = new AbortController();
    try {
      if (message.method !== 'GET' || message.body !== undefined || message.headers !== undefined) throw new Error('Read-only viewer');
      const path = readPath(message.path);
      this.pending.set(id, controller);
      const result = await this.client.request(path, controller.signal);
      if (!this.disposed && result.status === 200) this.actions.read?.(path);
      reply(result);
    } catch (_) { reply({ error: 'Mesh viewer unavailable. Run mesh ui ~/Corpus (without --own-index), or use Mesh: Set Local Viewer URL, then Refresh View.' }); }
    finally { this.pending.delete(id); }
  }
  dispose() { this.disposed = true; for (const c of this.pending.values()) c.abort(); this.pending.clear(); }
}
module.exports = { Broker };
