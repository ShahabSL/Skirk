import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile, writeFile, mkdtemp, rm } from 'node:fs/promises';
import { createServer } from 'node:http';
import { spawn } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { startObserver, listenPath } from './observer.mjs';

const path = process.env.SKIRK_WEBCHANNEL_BLOB_PATH;
const database = 'projects/skirk-p/databases/skirk-front-probe-abcdef012345';
const document = database + '/documents/owned/record';
const config = { bridge_url: 'http://127.0.0.1:12345', bridge_auth: 'a'.repeat(64), database, document, access_token: 'owner-opaque-test-token', duration_ms: 5000 };

test('a modified vendor implementation cannot obtain a transport', { skip: !path }, async () => {
  const blob = await readFile(path);
  blob[0] ^= 1;
  let network = 0;
  await assert.rejects(startObserver(config, blob, async () => { network++; }), /unapproved/);
  assert.equal(network, 0);
});

test('official SDK creates owned Listen request with token in payload, never URL', { skip: !path }, async () => {
  const blob = await readFile(path);
  let receiveFirst;
  const first = new Promise(resolve => { receiveFirst = resolve; });
  const localRequests = [];
  const local = async (url, options) => {
    assert.equal(new URL(url).origin, config.bridge_url);
    assert.equal(options.headers.Authorization, `Bearer ${config.bridge_auth}`);
    const payload = JSON.parse(options.body);
    localRequests.push({ path: new URL(url).pathname, payload });
    if (new URL(url).pathname === '/event') return new Response(null, { status: 204 });
    receiveFirst(payload);
    return new Promise((_, reject) => {
      options.signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true });
      if (options.signal.aborted) reject(new DOMException('aborted', 'AbortError'));
    });
  };
  const observer = await startObserver(config, blob, local);
  try {
    const payload = await Promise.race([first, new Promise((_, reject) => setTimeout(() => reject(new Error('no SDK handshake')), 1000))]);
    const logical = new URL('https://firestore.googleapis.com' + payload.path);
    assert.equal(logical.pathname, listenPath);
    assert.equal(logical.searchParams.get('database'), database);
    assert.ok(!logical.href.includes(config.access_token));
    assert.ok(!logical.searchParams.has('$httpHeaders'));
    const form = new URLSearchParams(Buffer.from(payload.body_base64, 'base64').toString());
    const values = [...form.values()].join('\n');
    assert.ok(values.includes('Bearer ' + config.access_token));
    assert.ok(values.includes('Authorization'));
    assert.ok(values.includes(document));
    assert.ok(values.includes('addTarget'));
  } finally { await observer.close(); }
  assert.equal(observer.state().stopped, true);
  assert.equal(observer.state().foreign_requests_rejected, 0);
  assert.ok(localRequests.length < 5);
});

function framed(value) {
  const text = JSON.stringify(value);
  return `${Buffer.byteLength(text)}\n${text}`;
}

function connectedBridge({ eventDelay = 400, eventStatus = 204, documentFirst = false, terminateStatus = 200, terminateDelay = 0, terminateFailure = '' } = {}) {
  let initial = false;
  let remove = false;
  let pull;
  let documentResolve;
  const documentSeen = new Promise(resolve => { documentResolve = resolve; });
  const reply = text => new Response(JSON.stringify({ status: 200, headers: { 'Content-Type': 'text/plain', 'X-HTTP-Session-Id': 'fixture-session' }, body_base64: Buffer.from(text).toString('base64') }), { status: 200 });
  const removed = () => framed([[3, [{ targetChange: { targetChangeType: 'REMOVE', targetIds: [2] } }]]]);
  const fetch = async (url, options) => {
    const value = JSON.parse(options.body);
    if (new URL(url).pathname === '/event') {
      if (value.kind === 'document') documentResolve();
      if (value.kind === 'target' && value.change === 'CURRENT') {
        return new Promise((resolve, reject) => {
          const timer = setTimeout(() => resolve(new Response(null, { status: eventStatus })), eventDelay);
          options.signal.addEventListener('abort', () => { clearTimeout(timer); reject(new DOMException('aborted', 'AbortError')); }, { once: true });
        });
      }
      return new Response(null, { status: 204 });
    }
    const logical = new URL('https://firestore.googleapis.com' + value.path);
    if (logical.searchParams.get('TYPE') === 'terminate') {
      if (terminateDelay) await new Promise(resolve => setTimeout(resolve, terminateDelay));
      if (terminateFailure === 'network') throw new TypeError('synthetic-owner-token must never be logged');
      if (terminateFailure === 'response_body') return { ok: true, status: 200, text: async () => { throw new TypeError('synthetic-owner-token must never be logged'); } };
      return terminateStatus === 200 ? reply('') : new Response(null, { status: terminateStatus });
    }
    if (!logical.searchParams.has('SID')) return reply(framed([[0, ['c', 'fixture-session', '', 8, 'fixture', 5000]]]));
    if (value.method === 'POST') {
      if (decodeURIComponent(Buffer.from(value.body_base64, 'base64').toString()).includes('removeTarget')) {
        remove = true;
        if (pull) { pull(reply(removed())); pull = null; }
      }
      return reply(framed([1, 3, 0]));
    }
    if (!initial) {
      initial = true;
      const current = { targetChange: { targetChangeType: 'CURRENT', targetIds: [2] } };
      const changed = { documentChange: { document: { name: document, fields: { sealed: { bytesValue: 'YQ==' } } } } };
      const messages = documentFirst ? [changed, current] : [current, changed];
      return reply(framed(messages.map((value, index) => [index + 1, [value]])));
    }
    if (remove) return reply(removed());
    return new Promise((resolve, reject) => {
      pull = resolve;
      options.signal.addEventListener('abort', () => { pull = null; reject(new DOMException('aborted', 'AbortError')); }, { once: true });
    });
  };
  return { fetch, documentSeen };
}

test('connected SDK close drains a delayed event ACK and disposes owned timers', { skip: !path }, async () => {
  const nativeSet = globalThis.setTimeout;
  const nativeClear = globalThis.clearTimeout;
  const timers = new Map();
  globalThis.setTimeout = (fn, delay, ...args) => {
    const handle = nativeSet(() => { timers.delete(handle); fn(...args); }, delay);
    timers.set(handle, delay);
    return handle;
  };
  globalThis.clearTimeout = handle => { timers.delete(handle); nativeClear(handle); };
  let observer;
  try {
    // The final document reaches Go before the following target event is
    // locally acknowledged: this is the ordering of the failed live close.
    const bridge = connectedBridge({ documentFirst: true });
    observer = await startObserver(config, await readFile(path), bridge.fetch);
    let timeout;
    await Promise.race([bridge.documentSeen, new Promise((_, reject) => { timeout = setTimeout(() => reject(new Error('no connected SDK document')), 2000); })]);
    clearTimeout(timeout);
    await observer.close();
    assert.equal(observer.state().document_events, 1);
    assert.equal(observer.state().target_remove_acknowledged, true);
    assert.equal(observer.state().stopped, true);
    assert.equal(observer.state().pending_events, 0);
    assert.equal(observer.state().pending_requests, 0);
    assert.deepEqual({ error: observer.state().error, live_timers: [...timers.values()] }, { error: '', live_timers: [] });
  } finally {
    if (observer) await observer.close();
    for (const handle of timers.keys()) nativeClear(handle);
    globalThis.setTimeout = nativeSet;
    globalThis.clearTimeout = nativeClear;
  }
});

test('connected SDK keeps real local event 400 and 429 failures during close', { skip: !path }, async () => {
  for (const status of [400, 429]) {
    const bridge = connectedBridge({ documentFirst: true, eventDelay: 150, eventStatus: status });
    const observer = await startObserver(config, await readFile(path), bridge.fetch);
    try {
      let timer;
      await Promise.race([bridge.documentSeen, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('no connected SDK document')), 2000); })]).finally(() => clearTimeout(timer));
      await observer.close();
      const state = observer.state();
      assert.equal(state.error, 'EVENT_DELIVERY');
      assert.equal(state.stopped, true);
      assert.ok(state.local_errors.some(value => value.phase === 'event' && value.reason === 'http_status' && value.status === status && value.abort_source === 'none' && !value.owned_shutdown_abort));
    } finally { await observer.close(); }
  }
});

test('a local request 429 remains a failure rather than a shutdown abort', { skip: !path }, async () => {
  let dispatched;
  const seen = new Promise(resolve => { dispatched = resolve; });
  const observer = await startObserver(config, await readFile(path), async (url, options) => {
    if (new URL(url).pathname === '/event') return new Response(null, { status: 204 });
    dispatched();
    return new Response(null, { status: 429 });
  });
  try {
    let timer;
    await Promise.race([seen, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('no request')), 1000); })]).finally(() => clearTimeout(timer));
    await observer.close();
    const state = observer.state();
    assert.equal(state.error, 'LOCAL_REQUEST_DELIVERY');
    assert.equal(state.stopped, true);
    assert.ok(state.local_errors.some(value => value.phase === 'request' && value.reason === 'http_status' && value.status === 429 && value.abort_source === 'none' && !value.owned_shutdown_abort));
  } finally { await observer.close(); }
});

test('a late local 429 after transport cancellation is never classified as owned abort', { skip: !path }, async () => {
  const bridge = connectedBridge({ documentFirst: true, eventDelay: 0, terminateStatus: 429, terminateDelay: 250 });
  const observer = await startObserver(config, await readFile(path), bridge.fetch);
  try {
    let timer;
    await Promise.race([bridge.documentSeen, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('no connected SDK document')), 2000); })]).finally(() => clearTimeout(timer));
    await observer.close();
    const state = observer.state();
    assert.equal(state.error, 'LOCAL_REQUEST_DELIVERY');
    assert.equal(state.stopped, true);
    assert.ok(state.local_errors.some(value => value.phase === 'request' && value.reason === 'http_status' && value.status === 429 && value.signal_aborted && value.abort_source === 'none' && !value.owned_shutdown_abort));
  } finally { await observer.close(); }
});

test('close owns the SDK request body-read window before local dispatch', { skip: !path }, async () => {
  const nativeArrayBuffer = Request.prototype.arrayBuffer;
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  let entered;
  const preparing = new Promise(resolve => { entered = resolve; });
  Request.prototype.arrayBuffer = async function () {
    entered();
    await gate;
    return nativeArrayBuffer.call(this);
  };
  let observer;
  let closing;
  try {
    observer = await startObserver(config, await readFile(path), async (url, options) => {
      if (new URL(url).pathname === '/event') return new Response(null, { status: 204 });
      assert.equal(options.signal.aborted, true);
      throw new DOMException('owned canceled request', 'AbortError');
    });
    let timer;
    await Promise.race([preparing, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('SDK did not prepare a request')), 1000); })]).finally(() => clearTimeout(timer));
    closing = observer.close();
    await new Promise(resolve => setTimeout(resolve, 1750));
    assert.equal(observer.state().stopped, false);
    assert.ok(observer.state().pending_requests > 0);
    release();
    await closing;
    assert.equal(observer.state().stopped, true);
    assert.equal(observer.state().pending_requests, 0);
    assert.equal(observer.state().error, '');
    assert.ok(observer.state().local_errors.every(value => value.reason === 'abort' && value.abort_source === 'observer_transport_abort' && value.owned_shutdown_abort));
  } finally {
    release();
    if (closing) await closing;
    if (observer) await observer.close();
    Request.prototype.arrayBuffer = nativeArrayBuffer;
  }
});

test('real network and body errors arriving after abort remain failures without raw error text', { skip: !path }, async () => {
  for (const reason of ['network', 'response_body']) {
    const bridge = connectedBridge({ documentFirst: true, eventDelay: 0, terminateDelay: 250, terminateFailure: reason });
    const observer = await startObserver(config, await readFile(path), bridge.fetch);
    try {
      let timer;
      await Promise.race([bridge.documentSeen, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('no connected SDK document')), 2000); })]).finally(() => clearTimeout(timer));
      await observer.close();
      const state = observer.state();
      assert.equal(state.error, 'LOCAL_REQUEST_DELIVERY');
      assert.equal(state.stopped, true);
      assert.ok(state.local_errors.some(value => value.phase === 'request' && value.reason === reason && value.signal_aborted && value.abort_source === 'none' && !value.owned_shutdown_abort));
      assert.ok(!JSON.stringify(state).includes('synthetic-owner-token'));
    } finally { await observer.close(); }
  }
});

test('the real observer process exits cleanly with native loopback HTTP and a delayed final ACK', { skip: !path }, async () => {
  const dir = await mkdtemp(join(tmpdir(), 'skirk-listen-owned-'));
  const bridge = connectedBridge({ documentFirst: true, eventDelay: 400 });
  const server = createServer(async (request, response) => {
    const controller = new AbortController();
    response.on('close', () => { if (!response.writableFinished) controller.abort(); });
    try {
      assert.equal(request.headers.authorization, `Bearer ${config.bridge_auth}`);
      const chunks = [];
      for await (const chunk of request) chunks.push(chunk);
      const reply = await bridge.fetch(`http://127.0.0.1${request.url}`, { body: Buffer.concat(chunks).toString(), signal: controller.signal });
      response.writeHead(reply.status, Object.fromEntries(reply.headers));
      response.end(Buffer.from(await reply.arrayBuffer()));
    } catch {
      if (!response.destroyed) { response.writeHead(500); response.end(); }
    }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  let child;
  let exited;
  let deadline;
  try {
    const privateConfig = join(dir, 'observer-config.json');
    await writeFile(privateConfig, JSON.stringify({ ...config, bridge_url: `http://127.0.0.1:${server.address().port}`, webchannel_blob: path }), { mode: 0o600 });
    child = spawn(process.execPath, [fileURLToPath(new URL('./observer.mjs', import.meta.url)), privateConfig], { env: { SKIRK_FIRESTORE_LISTEN_OBSERVER: '1', NO_PROXY: '127.0.0.1,localhost' }, stdio: ['ignore', 'ignore', 'pipe'] });
    let stderr = '';
    child.stderr.on('data', bytes => { stderr = (stderr + bytes).slice(0, 1024); });
    exited = new Promise(resolve => child.once('close', (code, signal) => resolve({ code, signal })));
    const timedOut = new Promise((_, reject) => { deadline = setTimeout(() => reject(new Error('observer process exceeded five-second owner shutdown window')), 5000); });
    await Promise.race([bridge.documentSeen, exited.then(() => { throw new Error('observer exited before document'); }), timedOut]);
    await writeFile(join(dir, 'stop-listen.json'), '{"stop":true}\n', { mode: 0o600 });
    const result = await Promise.race([exited, timedOut]);
    clearTimeout(deadline);
    assert.deepEqual(result, { code: 0, signal: null }, stderr);
    const state = JSON.parse(await readFile(join(dir, 'observer-result.json'), 'utf8'));
    assert.equal(state.error, '');
    assert.equal(state.stopped, true);
    assert.equal(state.target_remove_acknowledged, true);
    assert.equal(state.pending_requests, 0);
    assert.equal(state.pending_events, 0);
    assert.equal(state.pending_sdk_timers, 0);
  } finally {
    clearTimeout(deadline);
    if (child && child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
    if (exited) await exited;
    server.closeAllConnections();
    await new Promise(resolve => server.close(resolve));
    await rm(dir, { recursive: true, force: true });
  }
});
