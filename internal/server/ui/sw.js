// exe's service worker. The desktop is a live view of the daemon, so nothing
// is ever served from a cache while the daemon answers: API calls, the
// terminals' sockets and app frames go to the network first, always. It has
// two jobs for when the daemon does not answer — a load that fails on the
// network, or comes back from a proxy in front of the daemon as a
// 502/503/504:
//
//   - a top-level load gets /ui/offline.html, precached at install;
//   - an app that says "offline" in its app.json gets the copy kept of its
//     page and of what it named there (below, "what an app keeps").
//
// The daemon stamps the version below from the UI build, so a deploy that
// changes any UI byte makes this file differ, which installs a fresh worker
// and retires the old offline page. What the apps keep outlives a build.
const VERSION = "__EXE_BUILD__";
const CACHE = "exe-" + VERSION;
const OFFLINE = "/ui/offline.html";

self.addEventListener("install", e => {
  e.waitUntil(caches.open(CACHE)
    .then(c => c.add(new Request(OFFLINE, { cache: "reload" })))
    .then(() => self.skipWaiting()));
});

self.addEventListener("activate", e => {
  e.waitUntil((async () => {
    const keys = await caches.keys();
    await Promise.all(keys.filter(k => k !== CACHE && k !== KEEP).map(k => caches.delete(k)));
    // let the browser start the navigation's request while this worker boots
    if (self.registration.navigationPreload) await self.registration.navigationPreload.enable();
    await self.clients.claim();
  })());
});

// ---- what an app keeps ----
// An app's app.json may say "offline": true, or name addresses (the daemon
// passes on those that are the app's to keep: apps.go, appOffline). On a
// device that has had the desk open, such an app can be read while exe does
// not answer:
//
//   its page    everything under /apps/<name>/: asked of exe, the answer
//               kept as it passes; the copy answers when exe does not.
//   keep        whole addresses, the same way, and a copy that answers
//               says when it was kept in X-Exe-Kept (milliseconds), which
//               is how the app knows it is reading an old answer. A copy
//               with a tag is asked about with it, and "unchanged" costs
//               no download.
//   fixed       starts of addresses under which nothing ever changes (a
//               file named by its hash): kept the first time one passes,
//               answered from the copy after, exe or no exe.
//   wanted      one more keep address, whose answer is a JSON list of
//               fixed addresses, written relative to itself, to be had
//               though nobody has asked for them: whenever it passes,
//               those not kept yet are fetched (have). A reader saving
//               data is not sent them.
//
// The copies are in the cache KEEP under their addresses without the token.
// An app may read its own there and delete them: a frame that was open
// before this worker first took the page over is not this worker's to
// answer (the browser leaves it be until it is loaded again), and reads
// its copy itself. Which apps keep what is learned from the
// desk's own GET /v1/apps as it passes, and kept there too, slimmed to what
// this needs: offline.html reads it to offer the apps it can open. Whenever
// the list passes, and whenever a push arrives, every page and every keep
// address is asked for again (warm), so a copy is no older than the last
// time this device heard from exe, whether or not the app's window was
// opened since.
const KEEP = "exe-keep";
const APPS = "/v1/apps";
const AUTH = "/sw/auth";       // the Authorization the desk last sent, for the asking this does on its own
const WAIT = 3000;             // how long exe has to answer before a copy answers for it
const DESK_WAIT = 8000;        // how long exe has to begin answering a top-level load before the alert does (longer than the alert's own PROBE_BY)
const WARM_EVERY = 60e3;
const FIXED_MOST = 32 * 2 ** 20; // a fixed file larger than this is not kept

let plan = null;               // { pages, keep, fixed, wanted }, null until the kept list has been read
function planOf(list) {
  const p = { pages: [], keep: new Set(), fixed: [], wanted: new Set() };
  for (const a of Array.isArray(list) ? list : []) {
    if (!a || !a.offline || typeof a.name !== "string") continue;
    p.pages.push("/apps/" + encodeURIComponent(a.name) + "/");
    for (const k of a.offline.keep || []) p.keep.add(k);
    for (const k of a.offline.fixed || []) p.fixed.push(k);
    if (a.offline.wanted) p.wanted.add(a.offline.wanted);
  }
  return p;
}
const planRead = (async () => {
  try {
    const r = await (await caches.open(KEEP)).match(APPS);
    plan = planOf(r ? await r.json() : []);
  } catch (err) { plan = planOf([]); }
})();

// the address a copy is kept under
function keyOf(u, bare) {
  const k = new URL(u);
  if (bare) k.search = ""; else k.searchParams.delete("token");
  return k.pathname + k.search;
}
// how an address is kept, "" when it is not
function kind(u) {
  if (plan.pages.some(p => u.pathname.startsWith(p))) return "page";
  if (plan.keep.has(keyOf(u))) return "keep";
  if (plan.wanted.has(keyOf(u))) return "wanted";
  if (plan.fixed.some(p => u.pathname.startsWith(p))) return "fixed";
  return "";
}
const mine = p => p.startsWith("/apps/") || p.startsWith("/platinum/") || p.startsWith("/v1/svc/") || p.startsWith("/v1/apps/");
const top = r => r.mode === "navigate" && r.destination === "document";
const sleep = ms => new Promise(r => setTimeout(r, ms));

// exe's answer, or an error when it is a gateway saying exe did not answer
async function ask(req, preload) {
  const res = (preload && await preload) || await fetch(req);
  if (res.status >= 502 && res.status <= 504) throw new Error("gateway " + res.status);
  return res;
}
// a request asking whether the copy is still what exe would say
function again(req, had) {
  const tag = had.headers.get("ETag"), since = had.headers.get("Last-Modified");
  if (req.mode === "navigate" || (!tag && !since)) return req;
  const h = new Headers(req.headers);
  if (tag) h.set("If-None-Match", tag); else h.set("If-Modified-Since", since);
  return new Request(req, { headers: h });
}
// a keep address's copy says when it was kept, or last found unchanged
const dated = how => how === "keep" || how === "wanted";
function stamped(c, key, body, headers) {
  const h = new Headers(headers);
  h.set("X-Exe-Kept", String(Date.now()));
  return c.put(key, new Response(body, { status: 200, headers: h }));
}
// exe's own whole answer is kept, nothing else: not an error, not a part
async function put(c, key, res, how) {
  if (res.type !== "basic" || res.status !== 200) return;
  const size = res.headers.get("Content-Length");
  if (how === "fixed" && (size === null || +size > FIXED_MOST)) return;
  if (!dated(how)) return c.put(key, res);
  return stamped(c, key, await res.blob(), res.headers);
}
// exe said the copy is current: it is stamped again, and passes as exe's answer
async function current(c, key, had) {
  const body = await had.clone().blob();
  await stamped(c, key, body, had.headers).catch(() => {});
  const h = new Headers(had.headers);
  h.delete("X-Exe-Kept");
  return new Response(body, { status: 200, headers: h });
}

// What a wanted list names and is not kept yet is fetched, one at a time,
// under the fixed addresses alone. The first that fails ends it: exe is
// away, and the rest come the next time the list passes.
const having = new Set();
async function have(c, key, auth) {
  if (having.has(key) || (self.navigator.connection && self.navigator.connection.saveData)) return;
  having.add(key);
  try {
    const res = await c.match(key, { ignoreVary: true });
    const list = res ? await res.json() : [];
    for (const a of Array.isArray(list) ? list.slice(0, 1000) : []) {
      if (typeof a !== "string") continue;
      const u = new URL(a, new URL(key, self.location.origin));
      if (u.origin !== self.location.origin || !plan.fixed.some(p => u.pathname.startsWith(p))) continue;
      const k = keyOf(u);
      if (await c.match(k, { ignoreVary: true })) continue;
      await put(c, k, await ask(new Request(k, { headers: auth ? { Authorization: auth } : {} })), "fixed");
    }
  } catch (err) {}
  having.delete(key);
}

async function kept(e, u, how) {
  const r = e.request, key = keyOf(u, how === "page");
  const c = await caches.open(KEEP);
  const had = await c.match(key, { ignoreVary: true });
  if (had && how === "fixed") return had;
  const net = (async () => {
    const req = had && dated(how) ? again(r, had) : r;
    const res = await ask(req, e.preloadResponse);
    const then = () => how === "wanted" ? have(c, key, r.headers.get("Authorization")) : null;
    if (res.status === 304 && req !== r) { const now = await current(c, key, had); e.waitUntil(then()); return now; }
    e.waitUntil(put(c, key, res.clone(), how).catch(() => {}).then(then));
    return res;
  })();
  e.waitUntil(net.catch(() => {}));
  // nothing kept: exe's answer, or its failing, as if this were not here
  if (!had) return top(r) ? bounded(net).catch(() => offline()) : net;
  return (await Promise.race([net.catch(() => null), sleep(WAIT).then(() => null)])) || had;
}

// The desk's list of apps goes to it whatever it says, and so does its
// failing; a good one tells this which apps keep what.
async function apps(e) {
  const res = await fetch(e.request);
  if (res.status === 200) e.waitUntil(learn(res.clone(), e.request.headers.get("Authorization")).catch(() => {}));
  return res;
}
async function learn(res, auth) {
  const list = (await res.json()).filter(a => a && a.offline).map(a => ({ name: a.name, title: a.title, offline: a.offline }));
  const c = await caches.open(KEEP);
  await c.put(APPS, new Response(JSON.stringify(list), { headers: { "Content-Type": "application/json" } }));
  if (auth) await c.put(AUTH, new Response(auth)); else await c.delete(AUTH);
  plan = planOf(list);
  // a copy nothing asks for any more goes: an app taken away, an address it no longer names
  for (const k of await c.keys()) {
    const u = new URL(k.url);
    if (u.pathname !== APPS && u.pathname !== AUTH && !kind(u)) await c.delete(k);
  }
  await warm();
}
let warmed = 0;
async function warm() {
  await planRead;
  if (Date.now() - warmed < WARM_EVERY) return;
  warmed = Date.now();
  const c = await caches.open(KEEP);
  const a = await c.match(AUTH), auth = a ? await a.text() : "";
  const one = async (key, how) => {
    const had = await c.match(key, { ignoreVary: true });
    const stop = new AbortController(), t = setTimeout(() => stop.abort(), 20000);
    try {
      let req = new Request(key, { cache: "no-store", signal: stop.signal, headers: auth && key.startsWith("/v1/") ? { Authorization: auth } : {} });
      if (had) req = again(req, had);
      const res = await fetch(req);
      if (res.status === 304 && had) { if (dated(how)) await current(c, key, had); }
      else await put(c, key, res, how);
    } catch (err) {}
    clearTimeout(t);
    if (how === "wanted") await have(c, key, auth);
  };
  await Promise.all([...plan.pages.map(p => one(p, "page")), ...[...plan.keep].map(k => one(k, "keep")), ...[...plan.wanted].map(k => one(k, "wanted"))]);
}

async function offline() {
  const c = await caches.open(CACHE);
  return (await c.match(OFFLINE)) || Response.error();
}
// A top-level load: exe's page, or the alert when exe does not answer. A
// line that carries nothing (a phone whose tailnet is up with nobody at
// the other end) never fails by itself, so the load is given DESK_WAIT to
// begin answering; a slow line whose answer has begun is waited for.
const bounded = p => Promise.race([p, sleep(DESK_WAIT).then(() => { throw new Error("no answer in " + DESK_WAIT + " ms"); })]);
async function desk(e) {
  try { return await bounded(ask(e.request, e.preloadResponse)); }
  catch (err) { return offline(); }
}

self.addEventListener("fetch", e => {
  const r = e.request;
  if (r.method === "GET" && !r.headers.has("range")) {
    const u = new URL(r.url);
    if (u.origin === self.location.origin) {
      if (u.pathname === APPS && !u.search) { e.respondWith(apps(e)); return; }
      if (mine(u.pathname)) {
        if (!plan) {
          // the worker has just woken and is still reading what it keeps
          e.respondWith(planRead.then(async () => {
            const how = kind(u);
            return how ? kept(e, u, how) : top(r) ? desk(e) : (await e.preloadResponse) || fetch(r);
          }));
          return;
        }
        const how = kind(u);
        if (how) { e.respondWith(kept(e, u, how)); return; }
      }
    }
  }
  // otherwise only a top-level document load; an app's iframe, a fetch, an
  // image or a socket goes straight to the network as if there were no worker
  if (top(r)) e.respondWith(desk(e));
});

// A push from the daemon — the ticker's price alerts (docs/price-alerts.md),
// the weather's, an agent's turn end (agentpush.go): show it — the tag
// replaces an older notification for the same token or session rather than
// stacking. It is also word that exe is there, so what the apps keep is
// asked for again.
self.addEventListener("push", e => {
  let d = {};
  try { d = e.data ? e.data.json() : {}; } catch (err) { d = { title: "exe", body: e.data ? e.data.text() : "" }; }
  e.waitUntil(Promise.all([self.registration.showNotification(d.title || "exe", {
    body: d.body || "", tag: d.tag || "exe", renotify: true,
    icon: "/ui/icon-192.png", badge: "/ui/icon-192.png", data: { url: d.url || "/", show: d.show || "" } }),
    warm().catch(() => {})]));
});

// A tap brings the desktop forward or opens it. A push that names a window
// (show: an agent session, a chat) has the desktop open it there: an open
// desktop is told in a message, one that has to be loaded finds the same in
// the URL's fragment. The desktop is the top-level page at "/" — an app's
// frame or the manual in a tab of its own is not it.
self.addEventListener("notificationclick", e => {
  e.notification.close();
  const d = e.notification.data || {};
  const url = new URL(d.url || "/", self.location.origin).href;
  e.waitUntil(self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(cs => {
    const c = cs.find(x => x.frameType === "top-level" && new URL(x.url).origin === self.location.origin && new URL(x.url).pathname === "/");
    if (!c) return self.clients.openWindow(url);
    if (d.show) c.postMessage({ exeShow: d.show });
    return c.focus();
  }));
});
