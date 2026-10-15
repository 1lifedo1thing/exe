// node internal/server/testdata/agent-wake-lock.cjs
// Controlled API promises exercise the shipped code's power/lifecycle races.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs'), vm = require('node:vm'), path = require('node:path');
const html = fs.readFileSync(path.join(__dirname, '../ui/index.html'), 'utf8');
const source = html.slice(html.indexOf('// ---- keep an installed app awake'), html.indexOf('// ---- boot ----'));

function desk({ installed = 'ios', supported = true } = {}) {
  const events = {}, observations = [], requests = [];
  const state = { id: '', hidden: false, shaded: false, focus: true };
  const mode = { matches: installed === 'desktop', addEventListener: (t, fn) => { mode.change = fn; } };
  const on = (t, fn) => (events[t] ||= []).push(fn);
  const window = {}, desktop = {};
  const document = {
    hidden: false, body: {}, hasFocus: () => state.focus, addEventListener: on,
    querySelector: () => state.id && !state.hidden && !state.shaded ? { id: state.id } : null,
    querySelectorAll: () => [window],
  };
  const navigator = { standalone: installed === 'ios' };
  if (supported) navigator.wakeLock = { request: type => {
    assert.equal(type, 'screen');
    return new Promise((resolve, reject) => requests.push({ resolve, reject }));
  } };
  const context = vm.createContext({
    navigator, document, matchMedia: () => mode, addEventListener: on,
    showAllOn: false, showDeskOn: false, $: () => desktop,
    MutationObserver: class {
      constructor(fn) { this.fn = fn; this.targets = []; observations.push(this); }
      observe(target, options) { this.targets.push({ target, options }); }
      disconnect() { this.targets = []; }
    },
  });
  const run = code => vm.runInContext(code, context);
  run(source); run('agentWakeStart()');
  const settle = async () => { for (let i = 0; i < 12; i++) await Promise.resolve(); };
  function lock() {
    const handlers = [];
    return {
      released: false, releases: 0,
      addEventListener: (type, fn) => { assert.equal(type, 'release'); handlers.push(fn); },
      emit: () => handlers.forEach(fn => fn()),
      async release() { this.releases++; this.released = true; this.emit(); },
    };
  }
  return { state, document, mode, requests, run, settle, lock, observations, window, desktop,
    change: () => { for (const o of observations) if (o.targets.some(t => t.target === window)) o.fn(); },
    event: t => (events[t] || []).forEach(fn => fn()) };
}

test('ordinary tabs and unsupported browsers never request a lock', () => {
  for (const options of [{ installed: false }, { supported: false }]) {
    const d = desk(options); d.state.id = 'win-claude'; d.change(); d.event('focus');
    assert.equal(d.requests.length, 0);
  }
});

test('both installed-app detection paths acquire for either coding agent only', async () => {
  for (const installed of ['ios', 'desktop']) for (const id of ['win-claude', 'win-codex']) {
    const d = desk({ installed });
    for (const other of ['win-chat', 'win-term-1', 'win-app-macos9', '']) {
      d.state.id = other; d.change(); assert.equal(d.requests.length, 0);
    }
    d.state.id = id; d.change();
    assert.equal(d.requests.length, 1);
    const lock = d.lock(); d.requests[0].resolve(lock); await d.settle();
    assert.equal(d.run('agentWakeLock'), lock);
    d.state.id = id === 'win-claude' ? 'win-codex' : 'win-claude'; d.change();
    assert.equal(d.requests.length, 1); assert.equal(lock.releases, 0);
    d.state.id = 'win-docs'; d.change(); await d.settle();
    assert.equal(lock.releases, 1);
  }
});

test('window hiding, shading, app visibility, focus and desktop overview release the lock', async () => {
  for (const away of ['hidden', 'shaded', 'background', 'blur', 'showAllOn', 'showDeskOn']) {
    const d = desk(); d.state.id = 'win-codex'; d.change();
    const lock = d.lock(); d.requests[0].resolve(lock); await d.settle();
    if (away === 'background') d.document.hidden = true;
    else if (away === 'blur') d.state.focus = false;
    else if (away.startsWith('show')) d.run(away + ' = true');
    else d.state[away] = true;
    d.change(); await d.settle();
    assert.equal(lock.releases, 1, away);
    d.document.hidden = false; d.state.focus = true; d.state.hidden = d.state.shaded = false;
    d.run('showAllOn = showDeskOn = false'); d.change();
    assert.equal(d.requests.length, 2, away + ' returned');
  }
});

test('a late grant after closing the agent window is immediately released', async () => {
  const d = desk(); d.state.id = 'win-claude'; d.change();
  d.state.id = ''; d.change();
  const lock = d.lock(); d.requests[0].resolve(lock); await d.settle();
  assert.equal(lock.releases, 1); assert.equal(d.run('agentWakeLock'), null);
});

test('pagehide releases even before visibility changes, and pageshow requests again', async () => {
  const d = desk(); d.state.id = 'win-codex'; d.change();
  d.event('pagehide');
  const lock = d.lock(); d.requests[0].resolve(lock); await d.settle();
  assert.equal(lock.releases, 1);
  d.event('pageshow'); assert.equal(d.requests.length, 2);
});

test('overlapping resume events share one pending request and recover an obsolete rejection', async () => {
  const d = desk(); d.state.id = 'win-claude'; d.change();
  d.event('pagehide'); d.event('pageshow'); d.event('visibilitychange'); d.event('focus');
  assert.equal(d.requests.length, 1);
  d.requests[0].reject(new Error('old page was hidden')); await d.settle();
  assert.equal(d.requests.length, 2);
  const lock = d.lock(); d.requests[1].resolve(lock); await d.settle();
  assert.equal(d.run('agentWakeLock'), lock); assert.equal(d.requests.length, 2);
});

test('a refusal or system release does not create a retry loop', async () => {
  const d = desk(); d.state.id = 'win-claude'; d.change();
  d.requests[0].reject(new Error('not allowed')); await d.settle();
  for (let i = 0; i < 10; i++) d.change();
  assert.equal(d.requests.length, 1);
  d.event('focus');
  const lock = d.lock(); d.requests[1].resolve(lock); await d.settle();
  await lock.release();
  for (let i = 0; i < 10; i++) d.change();
  assert.equal(d.requests.length, 2);
  d.event('focus'); assert.equal(d.requests.length, 3);
});

test('an old release event cannot clear a newer lock', async () => {
  const d = desk(); d.state.id = 'win-claude'; d.change();
  const old = d.lock(); d.requests[0].resolve(old); await d.settle();
  d.event('pagehide'); await d.settle(); d.event('pageshow');
  const current = d.lock(); d.requests[1].resolve(current); await d.settle();
  old.emit(); assert.equal(d.run('agentWakeLock'), current);
});

test('window observers watch chrome without traversing terminal output', () => {
  const d = desk();
  for (const observer of d.observations) for (const { options } of observer.targets)
    assert.notEqual(options.subtree, true);
  assert.ok(d.observations.some(o => o.targets.some(t => t.target === d.desktop && t.options.childList)));
  assert.ok(d.observations.some(o => o.targets.some(t => t.target === d.window && t.options.attributes)));
});
