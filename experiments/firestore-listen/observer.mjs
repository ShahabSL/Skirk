// Research only. Google's pinned Closure implementation owns WebChannel framing;
// this adapter gives it no direct network primitive or browser fallback.
import { createHash } from 'node:crypto';
import { readFile, writeFile, stat, access } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { performance } from 'node:perf_hooks';
import vm from 'node:vm';

export const blobSHA256 = 'c820e65a0254c9765ab8564cc01faf2778dc87dfd72425421029f82aa8087467';
export const listenPath = '/google.firestore.v1.Firestore/Listen/channel';

export async function startObserver(config, blob, localFetch = globalThis.fetch) {
  if (createHash('sha256').update(blob).digest('hex') !== blobSHA256) throw new Error('unapproved WebChannel implementation');
  const bridge = new URL(config.bridge_url);
  if (bridge.protocol !== 'http:' || bridge.hostname !== '127.0.0.1' || !bridge.port || bridge.username || bridge.password || !['', '/'].includes(bridge.pathname) || bridge.search || bridge.hash || !/^[a-f0-9]{64}$/.test(config.bridge_auth)) throw new Error('invalid loopback bridge');
  const hybrid = config.hybrid_research === true;
  const databasePattern = hybrid ? /^projects\/skirk-p\/databases\/skirk-(?:front-probe|hybrid-probe)-[a-f0-9]{12}$/ : /^projects\/skirk-p\/databases\/skirk-front-probe-[a-f0-9]{12}$/;
  if (!databasePattern.test(config.database) || !config.document.startsWith(config.database + '/documents/') || config.document.length > 512 || !/^[\x21-\x7e]{1,16384}$/.test(config.access_token)) throw new Error('invalid owned target');
  const lifetime = Math.min(Math.max(config.duration_ms || 60000, 1000), hybrid ? 300000 : 90000);
  const requestLimit = hybrid ? config.request_limit ?? 256 : 28;
  const cleanupReserve = hybrid ? config.request_cleanup_reserve ?? 8 : 0;
  const eventLimit = hybrid ? config.event_limit ?? 512 : 64;
  if (!Number.isSafeInteger(requestLimit) || requestLimit < 1 || requestLimit > (hybrid ? 256 : 28) || !Number.isSafeInteger(eventLimit) || eventLimit < 1 || eventLimit > (hybrid ? 512 : 64)) throw new Error('invalid owned observer bounds');
  if (!Number.isSafeInteger(cleanupReserve) || cleanupReserve < 0 || cleanupReserve > 8 || cleanupReserve >= requestLimit) throw new Error('invalid owned cleanup reserve');
  const requests = new Set();
  const deliveries = new Set();
  const transportAbort = new AbortController();
  const eventAbort = new AbortController();
  const diagnostics = [];
  const sdkTimers = new Map();
  let nextTimer = 0;
  let timersDisposed = false;
  let timersCleared = 0;
  let eventTail = Promise.resolve();
  const sdkSetTimeout = (fn, delay, ...args) => {
    if (timersDisposed) return 0;
    const id = ++nextTimer;
    sdkTimers.set(id, setTimeout(() => { sdkTimers.delete(id); fn(...args); }, delay));
    return id;
  };
  const sdkClearTimeout = id => { if (sdkTimers.has(id)) { clearTimeout(sdkTimers.get(id)); sdkTimers.delete(id); } };
  const disposeTimers = () => { timersDisposed = true; timersCleared += sdkTimers.size; for (const id of [...sdkTimers.keys()]) sdkClearTimeout(id); };
  let closed = false;
  let stopped = false;
  let channelClosed = false;
  let closingPromise;
  let removeAcknowledged = false;
  let removeResolve;
  const removed = new Promise(resolve => { removeResolve = resolve; });
  let channel;
  let requestCount = 0;
  let activeRequestCount = 0;
  let cleanupRequestCount = 0;
  let rejectedRequestCount = 0;
  let documentCount = 0;
  let targetCount = 0;
  let rejected = 0;
  let eventCount = 0;
  let failure = '';
  const safeCodes = new Set(['OK', 'CANCELLED', 'UNKNOWN', 'INVALID_ARGUMENT', 'DEADLINE_EXCEEDED', 'NOT_FOUND', 'ALREADY_EXISTS', 'PERMISSION_DENIED', 'RESOURCE_EXHAUSTED', 'FAILED_PRECONDITION', 'ABORTED', 'OUT_OF_RANGE', 'UNIMPLEMENTED', 'INTERNAL', 'UNAVAILABLE', 'DATA_LOSS', 'UNAUTHENTICATED']);
  const isSignalAbort = (error, signal) => signal.aborted && (error === signal.reason || error?.name === 'AbortError');
  const localError = (phase, reason, status, signal) => Object.assign(new Error('bounded loopback delivery failure'), { phase, reason, status, aborted: signal.aborted });
  const recordError = (error, phase, signal, sdkSignal) => {
    const wasAbort = error.reason === 'abort' && signal.aborted;
    const abortSource = !wasAbort ? 'none' : phase === 'event' ? 'event_drain_timeout' : transportAbort.signal.aborted ? 'observer_transport_abort' : sdkSignal?.aborted ? (closed ? 'sdk_during_close' : 'sdk_request') : 'unknown';
    if (diagnostics.length < 32) diagnostics.push({ phase, reason: error.reason || 'network', status: error.status || 0, signal_aborted: signal.aborted, abort_source: abortSource, closing: !!closingPromise, owned_shutdown_abort: wasAbort && phase === 'request' && transportAbort.signal.aborted && !!closingPromise });
  };
  const postLocal = async (path, value, signal, phase) => {
    let result;
    try { result = await localFetch(new URL(path, bridge), { method: 'POST', redirect:'error', headers: { Authorization: `Bearer ${config.bridge_auth}`, 'Content-Type': 'application/json' }, body: JSON.stringify(value), signal }); }
    catch (error) { throw localError(phase, isSignalAbort(error, signal) ? 'abort' : 'network', 0, signal); }
    if (!result.ok) throw localError(phase, 'http_status', result.status, signal);
    if (result.status === 204) return null;
    let text;
    try { text = await result.text(); }
    catch (error) { throw localError(phase, isSignalAbort(error, signal) ? 'abort' : 'response_body', 0, signal); }
    if (text.length > 2 * 1024 * 1024) throw localError(phase, 'response_bound', result.status, signal);
    try { return JSON.parse(text); }
    catch { throw localError(phase, 'response_json', result.status, signal); }
  };
  const tracked = (promise, set) => {
    set.add(promise);
    promise.finally(() => set.delete(promise)).catch(() => {});
    return promise;
  };
  const emit = event => {
    if (closed) return;
    if (++eventCount > eventLimit) { failure ||= 'EVENT_COUNT_BOUND'; void close(); return; }
    if (deliveries.size >= 64) { failure ||= 'EVENT_QUEUE_BOUND'; void close(); return; }
    // One ordered, bounded local publisher cannot consume all forward slots.
    const task = eventTail.then(() => postLocal('/event', event, eventAbort.signal, 'event'));
    eventTail = task.catch(() => {});
    tracked(task, deliveries).catch(error => { recordError(error, 'event', eventAbort.signal); failure ||= 'EVENT_DELIVERY'; void close(); });
  };
  const proxyFetchOnce = async input => {
    const req = input instanceof Request ? input : new Request(input);
    const parsed = new URL(req.url);
    // In particular never let Closure's connectivity-image or termination
    // helpers escape to a native browser/Node network route.
    if (parsed.protocol !== 'https:' || parsed.hostname !== 'firestore.googleapis.com' || parsed.port || parsed.pathname !== listenPath || parsed.searchParams.get('database') !== config.database || parsed.username || parsed.password || parsed.searchParams.has('$httpHeaders') || decodeURIComponent(parsed.search).includes(config.access_token) || !['GET', 'POST'].includes(req.method)) {
      rejected++; failure ||= 'NETWORK_WHITELIST'; throw new Error('logical request outside owned listener');
    }
    const ownedCleanup = !!closingPromise;
    if (requestCount >= requestLimit || (!ownedCleanup && activeRequestCount >= requestLimit-cleanupReserve) || (ownedCleanup && cleanupReserve && cleanupRequestCount >= cleanupReserve)) {
      rejectedRequestCount++;failure ||= 'REQUEST_BOUND';void close();throw new Error('listener request bound');
    }
    requestCount++;
    if (ownedCleanup) cleanupRequestCount++; else activeRequestCount++;
    const bytes = Buffer.from(await req.arrayBuffer());
    if (bytes.length > 128 * 1024) throw new Error('listener body bound');
    const signal = AbortSignal.any([req.signal, transportAbort.signal]);
    let response;
    try { response = await postLocal('/request', { method: req.method, path: parsed.pathname + parsed.search, headers: Object.fromEntries(req.headers), body_base64: bytes.toString('base64') }, signal, 'request'); }
    catch(error) {
      recordError(error, 'request', signal, req.signal);
      // A real local 400/429/502, malformed bridge result, or network failure
      // must survive any subsequent close. Only an actual aborted request can
      // be a normal consequence of disposing the browser transport.
      if (error.reason !== 'abort') { failure ||= 'LOCAL_REQUEST_DELIVERY'; void close(); }
      throw error;
    }
    const body = Buffer.from(response.body_base64, 'base64');
    if (body.length > 1024 * 1024) throw new Error('listener response bound');
    return new Response(body, { status: response.status, headers: response.headers });
  };
  // Own preparation/body reading and response reconstruction as well as the
  // local HTTP operation, so close cannot report stopped in either window.
  const proxyFetch = input => tracked(proxyFetchOnce(input), requests);
  const sandbox = {
    module: { exports: {} },
    JSON, Math, Date,
    fetch: proxyFetch,
    Headers, Request, Response, AbortController, AbortSignal, TextDecoder, TextEncoder, ReadableStream,
    // Closure is browser code and explicitly tests timer IDs with typeof
    // 'number'. Native Node Timeout objects silently break its disposal.
    setTimeout:sdkSetTimeout, clearTimeout:sdkClearTimeout,
    console: { log() {}, warn() {}, error() {} },
  };
  sandbox.global = sandbox;
  sandbox.self = sandbox;
  vm.runInNewContext(blob.toString(), sandbox, { timeout: 1000, filename: 'pinned-google-webchannel.cjs' });
  const api = sandbox.module.exports;
  channel = api.createWebChannelTransport().createWebChannel('https://firestore.googleapis.com' + listenPath, {
    httpSessionIdParam: 'gsessionid',
    initMessageHeaders: { Authorization: `Bearer ${config.access_token}`, 'Content-Type': 'text/plain', 'X-Goog-Api-Client': 'gl-js/ fire/research' },
    messageUrlParams: { database: config.database },
    sendRawJson: true, supportsCrossDomainXhr: true,
    encodeInitMessageHeaders: true, useFetchStreams: true,
    forceLongPolling: true, detectBufferingProxy: false,
    longPollingTimeout: 5000, concurrentRequestLimit: 1,
    internalChannelParams: { forwardChannelRequestTimeoutMs: 20000 },
  });
  channel.listen(api.WebChannel.EventType.MESSAGE, message => {
    if (closed) return;
    let value = message.data?.[0];
    if (Array.isArray(value)) value = value[0];
    if (value?.error) {
      const code = safeCodes.has(value.error.status) ? value.error.status : 'UNKNOWN';
      failure ||= code; emit({ kind: 'error', code }); return;
    }
    const changed = value?.documentChange?.document;
    if (changed) {
      const data = changed.fields?.sealed?.bytesValue;
      if (changed.name !== config.document || typeof data !== 'string' || data.length > 512 * 1024 || !/^[A-Za-z0-9+/]*={0,2}$/.test(data)) { failure ||= 'MALFORMED_EVENT'; emit({ kind: 'error', code: failure }); return; }
      documentCount++;
      if ((hybrid && typeof changed.updateTime !== 'string') || (changed.updateTime !== undefined && (typeof changed.updateTime !== 'string' || changed.updateTime.length > 64))) { failure ||= 'MALFORMED_EVENT'; emit({ kind: 'error', code: failure }); return; }
      emit({ kind: 'document', name: changed.name, wire_base64: data, ...(changed.updateTime ? { update_time: changed.updateTime } : {}), received_unix_ms: performance.timeOrigin + performance.now() });
    } else if (value?.targetChange) {
      targetCount++;
      const target = value.targetChange;
      if (target.targetChangeType === 'REMOVE' && target.targetIds?.includes(2)) { removeAcknowledged = true; removeResolve(); }
      if (target.cause?.code) { failure ||= 'RPC_' + Math.min(Math.max(Number(target.cause.code) || 2, 0), 16); emit({ kind: 'error', code: failure }); return; }
      emit({ kind: 'target', change: ['NO_CHANGE', 'ADD', 'REMOVE', 'CURRENT', 'RESET'].includes(target.targetChangeType) ? target.targetChangeType : 'NO_CHANGE', ids: Array.isArray(target.targetIds) ? target.targetIds.slice(0, 8) : [] });
    }
  });
  channel.listen(api.WebChannel.EventType.ERROR, () => { if (!closed) { failure ||= 'WEBCHANNEL_ERROR'; emit({ kind: 'error', code: failure }); } });
  channel.listen(api.WebChannel.EventType.CLOSE, () => { channelClosed = true; });
  const timer = setTimeout(() => { failure ||= 'OBSERVER_DEADLINE'; void close(); }, lifetime);
  const close = async () => {
    if (closingPromise) return closingPromise;
    // Assign ownership before channel.send can synchronously emit callbacks.
    closingPromise = Promise.resolve().then(async () => {
      clearTimeout(timer);
      if (hybrid) {
        // This private authenticated control call consumes no Google quota.
        // It synchronously marks Go-side teardown ownership before native
        // REMOVE/terminate can consume the reserved eight forwarding slots.
        try { await postLocal('/stop', { reason: failure === 'REQUEST_BOUND' ? 'request_bound' : 'observer_close' }, AbortSignal.timeout(1000), 'control'); }
        catch (error) { recordError(error,'control',new AbortController().signal);failure ||= 'CONTROL_STOP_DELIVERY'; }
      }
      try { channel.send({ database: config.database, removeTarget: 2 }); } catch {}
      let removeTimer;
      await Promise.race([removed, new Promise(resolve => { removeTimer=setTimeout(resolve,1500); })]);
      clearTimeout(removeTimer);
      closed = true;
      try { channel.close(); } catch {}
      await new Promise(resolve => setTimeout(resolve, 100));
      transportAbort.abort();
      disposeTimers();
      const settle = async (set, timeout) => {
        let handle;
        const joined = await Promise.race([Promise.allSettled([...set]).then(()=>true),new Promise(resolve=>{handle=setTimeout(()=>resolve(false),timeout);})]);
        clearTimeout(handle);return joined;
      };
      // These independent sets must drain concurrently: sequential two-second
      // waits could exceed the Go owner's five-second graceful-stop window.
      const [requestsJoined, eventsJoined] = await Promise.all([settle(requests,2000), settle(deliveries,2000)]);
      if (!requestsJoined) failure ||= 'REQUEST_JOIN_TIMEOUT';
      if (!eventsJoined) { failure ||= 'EVENT_DRAIN_TIMEOUT'; eventAbort.abort(); await settle(deliveries,500); }
      else eventAbort.abort();
      stopped = requests.size===0 && deliveries.size===0 && sdkTimers.size===0;
      if (!stopped) failure ||= 'UNJOINED_OPERATIONS';
    });
    return closingPromise;
  };
  channel.open();
  channel.send({ database: config.database, addTarget: { documents: { documents: [config.document] }, targetId: 2 } });
  return { close, state: () => ({ request_count: requestCount, active_request_count:activeRequestCount, cleanup_request_count:cleanupRequestCount, rejected_request_count:rejectedRequestCount, document_events: documentCount, target_events: targetCount, emitted_events:eventCount,target_remove_acknowledged:removeAcknowledged, channel_closed:channelClosed,foreign_requests_rejected: rejected, pending_requests:requests.size,pending_events:deliveries.size,pending_sdk_timers:sdkTimers.size,disposed_sdk_timers:timersCleared,local_errors:diagnostics,error: failure, stopped }) };
}

async function main() {
  if (process.env.SKIRK_FIRESTORE_LISTEN_OBSERVER !== '1') throw new Error('explicit observer opt-in required');
  const path = process.argv[2];
  const info = await stat(path);
  if (!info.isFile() || (info.mode & 0o077) !== 0 || info.size > 32768) throw new Error('private observer configuration required');
  const config = JSON.parse(await readFile(path, 'utf8'));
  const observer = await startObserver(config, await readFile(config.webchannel_blob));
  const stopPath = join(dirname(path), 'stop-listen.json');
  const started = Date.now();
  while (!observer.state().stopped && !observer.state().error && Date.now() - started < 90000) {
    try { await access(stopPath); break; } catch {}
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  await observer.close();
  await writeFile(join(dirname(path), 'observer-result.json'), JSON.stringify(observer.state()) + '\n', { mode: 0o600, flag: 'wx' });
  if (observer.state().error) process.exitCode = 1;
}
if (process.argv[1] && new URL(import.meta.url).pathname === process.argv[1]) main().catch(() => { process.stderr.write('bounded Firestore observer failed\n'); process.exitCode = 1; });
