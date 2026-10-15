// Run with node internal/server/testdata/desktop-resume.cjs.
// Exercise the shipped desktop's lifecycle and build checks with a paused
// clock and controlled network, including the stale-worker reload regression.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const html = fs.readFileSync(require('node:path').join(__dirname, '../ui/index.html'), 'utf8');
const section = (start, end) => html.slice(html.indexOf(start), html.indexOf(end));
const source = section('// ---- a new build reloads', '// ---- window manager ----')
  + section('// A Home Screen app can resume', '// ---- Stickies:');

function desk() {
  let now = 10000, nextTimer = 0;
  const timers = new Map(), events = {}, stored = new Map();
  const state = { heads: 0, loads: 0, apps: 0, streams: 0, layouts: 0, draft: false, modal: false, build: 'current', status: 200 };
  const listen = (name, cb) => (events[name] ||= []).push(cb);
  const document = {
    hidden: false, addEventListener: listen,
    querySelector: selector => selector.includes('data-dirty') ? state.draft : state.modal,
    querySelectorAll: () => [],
  };
  const timer = (fn, ms, repeat = false) => {
    const id = ++nextTimer;
    timers.set(id, { fn, at: now + ms, repeat: repeat ? ms : 0 });
    return id;
  };
  const context = vm.createContext({
    document, addEventListener: listen, AbortController,
    performance: { now: () => now }, openMenuEl: null, toast: () => {},
    sessionStorage: { getItem: k => stored.get(k), setItem: (k, v) => stored.set(k, v) },
    location: { reload: () => state.loads++ },
    setTimeout: (fn, ms) => timer(fn, ms), setInterval: (fn, ms) => timer(fn, ms, true),
    clearTimeout: id => timers.delete(id), clearInterval: id => timers.delete(id),
    loadApps: async () => { state.apps++; },
    appDataSubscribe: () => state.streams++, winES: null, winSubscribe: () => state.layouts++,
    fetch: async (url, opts) => {
      state.heads++;
      assert.equal(url, '/'); assert.equal(opts.method, 'HEAD'); assert.equal(opts.cache, 'no-store');
      if (state.hang) return new Promise((resolve, reject) => opts.signal.addEventListener('abort', () => reject(new Error('aborted'))));
      if (state.offline) throw new Error('offline');
      return { ok: state.status === 200, headers: { get: () => state.build } };
    },
  });
  vm.runInContext(source.replaceAll('__EXE_BUILD__', 'current'), context);
  const run = code => vm.runInContext(code, context);
  const settle = async () => { for (let i = 0; i < 15; i++) await Promise.resolve(); };
  const advance = async ms => {
    const until = now + ms;
    for (;;) {
      const due = [...timers].filter(([, t]) => t.at <= until).sort((a, b) => a[1].at - b[1].at)[0];
      if (!due) break;
      const [id, t] = due; now = t.at;
      if (t.repeat) t.at += t.repeat; else timers.delete(id);
      t.fn(); await settle();
    }
    now = until; await settle();
  };
  return { state, document, stored, run, settle, advance,
    event: (name, props = {}) => (events[name] || []).forEach(cb => cb(props)) };
}

test('a phone wake coalesces lifecycle signals, checks the build and refreshes apps', async () => {
  const d = desk(); d.state.build = 'new';
  d.event('pageshow', { persisted: true }); d.event('visibilitychange'); d.event('online');
  await d.advance(100);
  assert.equal(d.state.heads, 1); assert.equal(d.state.loads, 1);
  assert.equal(d.state.apps, 1); assert.equal(d.state.streams, 1);
  assert.equal(d.state.layouts, 0); // mobile has no layout subscription
});

test('initial pageshow and a hidden page do not reconnect or poll', async () => {
  const d = desk(); d.event('pageshow', { persisted: false }); await d.advance(100);
  assert.equal(d.state.heads, 0);
  d.document.hidden = true; d.event('visibilitychange'); d.event('online');
  await d.advance(60000);
  assert.equal(d.state.heads, 0); assert.equal(d.state.apps, 0);
});

test('same build keeps the page and reconnects existing desktop streams', async () => {
  const d = desk(); d.run('winES = {}'); d.event('visibilitychange'); await d.advance(100);
  assert.equal(d.state.loads, 0); assert.equal(d.state.layouts, 1); assert.equal(d.state.apps, 1);
});

test('unsaved work and active input still hold a confirmed update', async () => {
  const d = desk(); d.state.build = 'new'; d.state.draft = true;
  d.event('visibilitychange'); await d.advance(100);
  assert.equal(d.run('buildNew'), 'new'); assert.equal(d.state.loads, 0);
  d.state.draft = false; d.run('buildNote({type: "keydown"})'); await d.advance(2000);
  assert.equal(d.state.loads, 0);
  await d.advance(1000); assert.equal(d.state.loads, 1);
});

test('offline wake recovers without another lifecycle event', async () => {
  const d = desk(); d.state.offline = true; d.event('visibilitychange'); await d.advance(100);
  assert.equal(d.state.loads, 0);
  d.state.offline = false; d.state.build = 'new'; await d.advance(29900);
  assert.equal(d.state.loads, 1);
});

test('a hung check times out, shares its request, then permits a retry', async () => {
  const d = desk(); d.state.hang = true;
  d.run('buildSeen("one"); buildSeen("two"); buildCheck()'); await d.settle();
  assert.equal(d.state.heads, 1); await d.advance(8000);
  assert.equal(d.run('buildChecking'), null); assert.equal(d.state.loads, 0);
  d.state.hang = false; d.state.build = 'new'; d.event('online'); await d.advance(100);
  assert.equal(d.state.heads, 2); assert.equal(d.state.loads, 1);
});

test('gateway errors and stale cached build headers cannot trigger a reload loop', async () => {
  const d = desk(); d.state.status = 503; d.state.build = 'old';
  d.run('buildSeen("old")'); await d.settle(); assert.equal(d.state.loads, 0);
  d.state.status = 200; d.state.build = 'current';
  d.run('buildSeen("old")'); await d.settle();
  d.run('buildSeen("old")'); await d.settle();
  assert.equal(d.state.heads, 2); assert.equal(d.state.loads, 0);
  // A later real deploy to a previously denied build is still discovered.
  d.state.build = 'old'; d.event('visibilitychange'); await d.advance(100);
  assert.equal(d.state.loads, 1);
});

test('confirmation follows the live build, even if the candidate was another version', async () => {
  const d = desk(); d.state.build = 'newest';
  d.run('buildSeen("old")'); await d.settle();
  assert.equal(d.stored.get('exe_build_tried'), 'newest'); assert.equal(d.state.loads, 1);
});

test('one try per build survives another resume of the same stale page', async () => {
  const d = desk(); d.state.build = 'new'; d.stored.set('exe_build_tried', 'new');
  d.event('visibilitychange'); await d.advance(100);
  assert.equal(d.state.loads, 0); assert.equal(d.run('buildNew'), '');
});
