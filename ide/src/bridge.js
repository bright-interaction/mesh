(() => {
  'use strict';
  const editor = acquireVsCodeApi();
  const pending = new Map(); let serial = 0;
  window.Mesh = { views: {}, searchOnSubmit: true, readOnlyViewer: true };
  const routes = new Set(['graph', 'search', 'dashboard', 'docs']);
  const saved = editor.getState();
  if (routes.has(saved?.route)) location.hash = saved.route === 'graph' ? '' : '#/' + saved.route;
  function save() { const route = location.hash.replace(/^#\//, '') || 'graph'; if (routes.has(route)) editor.setState({ route }); }
  window.addEventListener('hashchange', save);
  window.fetch = (path, options = {}) => new Promise((resolve, reject) => {
    if (typeof path !== 'string' || (options.method || 'GET').toUpperCase() !== 'GET' || options.body !== undefined || options.headers !== undefined || pending.size >= 6) { reject(new Error('Read-only Mesh viewer')); return; }
    const id = ++serial;
    const timer = setTimeout(() => { pending.delete(id); reject(new Error('Mesh viewer timed out. Use Mesh: Refresh View to retry.')); }, 35000);
    pending.set(id, { resolve, reject, timer });
    editor.postMessage({ type: 'request', id, method: 'GET', path });
  });
  window.addEventListener('message', ({ data }) => {
    if (data?.type !== 'response') return;
    const job = pending.get(data.id); if (!job) return;
    pending.delete(data.id); clearTimeout(job.timer);
    if (data.error) job.reject(new Error(data.error));
    else job.resolve(new Response(data.body, { status: data.status, headers: { 'Content-Type': 'application/json' } }));
  });
  // Note bodies cannot use file:, command:, vscode:, remote or relative links to
  // escape the read-only viewer. In-page section links are the only navigation.
  const safeLink = href => /^#\/(graph|search|dashboard|docs)$/.test(href || '');
  function neutralize(anchor) {
    const href = anchor.getAttribute('href');
    if (href !== null && !safeLink(href)) {
      anchor.removeAttribute('href'); anchor.removeAttribute('target');
      anchor.setAttribute('aria-disabled', 'true'); anchor.title = 'Links are disabled in this read-only IDE viewer.';
    }
  }
  document.querySelectorAll('a').forEach(neutralize);
  new MutationObserver(records => {
    for (const record of records) {
      if (record.type === 'attributes') neutralize(record.target);
      else for (const node of record.addedNodes) if (node.nodeType === 1) {
        if (node.matches('a')) neutralize(node);
        node.querySelectorAll('a').forEach(neutralize);
      }
    }
  }).observe(document.documentElement, { subtree: true, childList: true, attributes: true, attributeFilter: ['href'] });
  for (const type of ['click', 'auxclick', 'contextmenu', 'dragstart']) document.addEventListener(type, event => {
    const anchor = event.target.closest?.('a'); if (!anchor) return;
    const href = anchor.getAttribute('href') || '';
    event.preventDefault(); event.stopPropagation();
    if (type === 'click' && safeLink(href)) location.hash = href;
  }, true);
  window.addEventListener('pagehide', () => { for (const job of pending.values()) { clearTimeout(job.timer); job.reject(new Error('Viewer closed')); } pending.clear(); });
  editor.postMessage({ type: 'ready' });
})();
