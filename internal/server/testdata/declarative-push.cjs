// node internal/server/testdata/declarative-push.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs'), vm = require('node:vm'), path = require('node:path');
const source = fs.readFileSync(path.join(__dirname, '../ui/sw.js'), 'utf8');
const desktop = fs.readFileSync(path.join(__dirname, '../ui/index.html'), 'utf8');
const origin = 'https://desk.example.com';
function worker({ clients = [], failShow = false, failWarm = false } = {}) {
  const events = {}, shown = [], opened = [], state = { warmed: 0, closed: 0 };
  const context = vm.createContext({
    URL, Request, Response, Headers, setTimeout, clearTimeout,
    self: {
      location: { origin }, addEventListener: (name, fn) => { events[name] = fn; },
      registration: { showNotification: async (title, options) => {
        if (failShow) throw new Error('notification service unavailable');
        shown.push(JSON.parse(JSON.stringify({ title, ...options })));
      } },
      clients: { matchAll: async () => clients, openWindow: async url => { opened.push(url); } },
    },
    caches: { open: async () => ({ match: async () => undefined }) },
    warmTest: async () => { state.warmed++; if (failWarm) throw new Error('offline'); },
  });
  vm.runInContext(source, context);
  vm.runInContext('warm = warmTest', context);
  return { shown, opened, state,
    async push(payload) {
      let done;
      events.push({ data: { json: () => payload, text: () => String(payload) }, waitUntil: p => { done = p; } });
      await done;
    },
    async click(data) {
      let done;
      events.notificationclick({ notification: { data, close: () => state.closed++ }, waitUntil: p => { done = p; } });
      await done;
    },
  };
}
const flat = { title: 'Codex finished', body: 'Review the patch', tag: 'agent-exe-codex-3', url: '/#show=codex:exe-codex-3', show: 'codex:exe-codex-3' };
const nested = { web_push: 8030, mutable: true, notification: {
  title: flat.title, body: flat.body, tag: flat.tag, navigate: origin + flat.url,
  icon: origin + '/ui/icon-192.png', mutable: true, data: { url: flat.url, show: flat.show },
} };

test('old, dual-format and declarative-only pushes each show one notification and warm once', async () => {
  for (const payload of [flat, { ...flat, ...nested }, nested]) {
    const w = worker(); await w.push(payload);
    assert.equal(w.shown.length, 1); assert.equal(w.state.warmed, 1);
    assert.equal(w.shown[0].title, flat.title); assert.equal(w.shown[0].body, flat.body);
    assert.equal(w.shown[0].tag, flat.tag); assert.equal(w.shown[0].data.show, flat.show);
    assert.equal(new URL(w.shown[0].data.url, origin).href, nested.notification.navigate);
    assert.equal(w.shown[0].navigate, undefined, 'replacement must retain notificationclick handling');
  }
});

test('the declarative notification wins over stale compatibility fields', async () => {
  const w = worker(); await w.push({ ...flat, title: 'Old title', body: 'Old body', ...nested });
  assert.equal(w.shown[0].title, flat.title); assert.equal(w.shown[0].body, flat.body);
});

test('offline cache refresh does not prevent display', async () => {
  const w = worker({ failWarm: true }); await w.push(nested);
  assert.equal(w.shown.length, 1); assert.equal(w.state.warmed, 1);
});

test('failed display rejects the event for browser fallback, without retrying or adding a second notification', async () => {
  const w = worker({ failShow: true });
  await assert.rejects(w.push(nested), /unavailable/);
  assert.equal(w.shown.length, 0); assert.equal(w.state.warmed, 1);
});

test('a click focuses the existing desktop and selects its session without navigation', async () => {
  let focused = 0; const messages = [];
  const desk = { frameType: 'top-level', url: origin + '/', focus: async () => focused++, postMessage: m => messages.push(JSON.parse(JSON.stringify(m))) };
  const w = worker({ clients: [
    { frameType: 'nested', url: origin + '/' },
    { frameType: 'top-level', url: origin + '/docs.md' }, desk,
  ] });
  await w.push(nested); await w.click(w.shown[0].data);
  assert.equal(focused, 1); assert.deepEqual(messages, [{ exeShow: flat.show }]);
  assert.deepEqual(w.opened, []); assert.equal(w.state.closed, 1);
});

test('a click with no desktop opens the declared URL, including its session fragment', async () => {
  const w = worker(); await w.push(nested); await w.click(w.shown[0].data);
  assert.deepEqual(w.opened, [nested.notification.navigate]);
});

test('a declarative-only sender can name the session through navigate without custom data', async () => {
  const messages = [];
  const w = worker({ clients: [{ frameType: 'top-level', url: origin + '/', focus: async () => {}, postMessage: m => messages.push(JSON.parse(JSON.stringify(m))) }] });
  await w.push({ ...nested, notification: { ...nested.notification, data: undefined } });
  await w.click(w.shown[0].data);
  assert.deepEqual(messages, [{ exeShow: flat.show }]);
});

test('a fallback navigation to an open desktop handles and clears the session fragment', () => {
  const events = {}, targets = [], cleared = [];
  const context = vm.createContext({
    location: { hash: '#show=claude%3Aexe-claude-2', pathname: '/', search: '' },
    history: { replaceState: (_, __, url) => cleared.push(url) },
    showTarget: target => targets.push(target), addEventListener: (t, fn) => { events[t] = fn; },
  });
  const start = desktop.indexOf('function showFromHash()');
  vm.runInContext(desktop.slice(start, desktop.indexOf('// A phone\'s keyboard:', start)), context);
  events.hashchange();
  assert.deepEqual(targets, ['claude:exe-claude-2']); assert.deepEqual(cleared, ['/']);
  context.location.hash = '#show=%broken';
  assert.doesNotThrow(() => events.hashchange());
  assert.equal(targets.length, 1);
});
