#!/usr/bin/env node

import http from "node:http";
import tls from "node:tls";
import { chmod, lstat, mkdir, readFile, writeFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { performance } from "node:perf_hooks";
import { homedir } from "node:os";
import path from "node:path";
import forge from "node-forge";

const DRIVE_URL = "https://www.googleapis.com/drive/v3";
const DRIVE_UPLOAD_URL = "https://www.googleapis.com/upload/drive/v3";
const DEFAULT_REQUEST_PREFIX = "cfexit/v1/req/";
const DEFAULT_RESPONSE_PREFIX = "cfexit/v1/res/";
const DEFAULT_CA_DIR = path.join(homedir(), ".skirk", "cf-free-mitm-ca");
const DEFAULT_MAX_BYTES = 32 * 1024 * 1024;
const DEFAULT_STREAMING_CHUNK_BYTES = 8 * 1024 * 1024;
const DEFAULT_MAX_REQUEST_BYTES = 1024 * 1024;
const BINARY_PROXY_MAGIC = Buffer.from("SKIRKCF1", "ascii");
const UNSAFE_RESPONSE_HEADERS = new Set([
  "connection",
  "keep-alive",
  "proxy-authenticate",
  "proxy-authorization",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
]);
const UNSAFE_REQUEST_HEADERS = new Set([
  "connection",
  "content-length",
  "keep-alive",
  "proxy-authenticate",
  "proxy-authorization",
  "proxy-connection",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
]);

const args = parseArgs(process.argv.slice(2));
const config = await loadConfig(args);
const listen = parseListen(args.listen ?? process.env.SKIRK_CF_HTTP_LISTEN ?? "127.0.0.1:18088");
const driveOnly = booleanOption(args.driveOnly ?? process.env.SKIRK_CF_HTTP_DRIVE_ONLY, true);
const workerUrl = driveOnly ? optionalUrl(args.workerUrl ?? process.env.WORKER_URL) : required(args.workerUrl ?? process.env.WORKER_URL, "WORKER_URL or --worker-url");
const adminToken = driveOnly ? args.adminToken ?? process.env.SKIRK_ADMIN_TOKEN : required(args.adminToken ?? process.env.SKIRK_ADMIN_TOKEN, "SKIRK_ADMIN_TOKEN or --admin-token");
const folderId = args.folderId ?? process.env.SKIRK_DRIVE_FOLDER_ID ?? config.drive?.folder_id;
const requestPrefix = args.requestPrefix ?? process.env.SKIRK_EXIT_REQUEST_PREFIX ?? DEFAULT_REQUEST_PREFIX;
const responsePrefix = args.responsePrefix ?? process.env.SKIRK_EXIT_RESPONSE_PREFIX ?? DEFAULT_RESPONSE_PREFIX;
const maxBytes = integerOption(args.maxBytes ?? process.env.SKIRK_CF_HTTP_MAX_BYTES, DEFAULT_MAX_BYTES);
const streamingChunkBytes = integerOption(
  args.streamingChunkBytes ?? process.env.SKIRK_CF_HTTP_STREAMING_CHUNK_BYTES,
  DEFAULT_STREAMING_CHUNK_BYTES,
);
const maxRequestBytes = integerOption(args.maxRequestBytes ?? process.env.SKIRK_CF_HTTP_MAX_REQUEST_BYTES, DEFAULT_MAX_REQUEST_BYTES);
const pollMs = integerOption(args.pollMs ?? process.env.SKIRK_CF_HTTP_POLL_MS, 250);
const timeoutMs = integerOption(args.timeoutMs ?? process.env.SKIRK_CF_HTTP_TIMEOUT_MS, 300_000);
const minPumpGapMs = integerOption(args.minPumpGapMs ?? process.env.SKIRK_CF_HTTP_MIN_PUMP_GAP_MS, 50);
const rangeFollow = booleanOption(args.rangeFollow ?? process.env.SKIRK_CF_HTTP_RANGE_FOLLOW, true);
const maxRangeChunks = integerOption(args.maxRangeChunks ?? process.env.SKIRK_CF_HTTP_MAX_RANGE_CHUNKS, 256);
const maxStreamingChunks = integerOption(args.maxStreamingChunks ?? process.env.SKIRK_CF_HTTP_MAX_STREAMING_CHUNKS, 64);
const legacyListFallback = booleanOption(args.legacyListFallback ?? process.env.SKIRK_CF_HTTP_LEGACY_LIST_FALLBACK, false);
const mitmEnabled = booleanOption(args.mitm ?? process.env.SKIRK_CF_MITM, false);
const caDir = args.caDir ?? process.env.SKIRK_CF_MITM_CA_DIR ?? DEFAULT_CA_DIR;
let ca;
let drive;
let pumpInFlight;
let lastPumpAt = 0;
let lastPumpResult;

async function handleProxyRequest(request, response, options = {}) {
  let clientClosed = false;
  response.once("close", () => {
    clientClosed = !response.writableEnded;
  });
  if (!isRelayMethod(request.method)) {
    response.writeHead(405, {
      "content-type": "text/plain; charset=utf-8",
      allow: "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS",
    });
    response.end("Skirk Cloudflare Free web relay does not support this HTTP method.\n");
    drainRequest(request);
    return;
  }
  if (request.headers.upgrade) {
    response.writeHead(501, {
      "content-type": "text/plain; charset=utf-8",
      "connection": "close",
    });
    response.end("Skirk Cloudflare Free web relay does not support WebSocket/Upgrade traffic.\n");
    drainRequest(request);
    return;
  }

  const targetUrl = targetUrlForRequest(request, options);
  const requestBody = await collectRequestBody(request, maxRequestBytes);
  const requestId = `http-${Date.now().toString(36)}-${randomUUID().slice(0, 8)}`;
  const baseHeaders = proxyHeaders(request.headers);
  let first = await relayWorkerFetch({
    requestId,
    method: request.method,
    targetUrl,
    headers: baseHeaders,
    body: requestBody,
    started: performance.now(),
    isCanceled: () => clientClosed,
  });

  if (
    first.truncated &&
    request.method === "GET" &&
    !baseHeaders.range &&
    (!rangeFollow || !canFollowRange(first.headers, first.status))
  ) {
    first = await relayWorkerFetch({
      requestId: `${requestId}-chunks`,
      method: request.method,
      targetUrl,
      headers: baseHeaders,
      body: requestBody,
      started: performance.now(),
      isCanceled: () => clientClosed,
      chunked: true,
    });
  }

  const shouldStreamDriveChunks = Boolean(first.more && Array.isArray(first.responseFileIds));
  const shouldStreamRanges =
    rangeFollow &&
    request.method === "GET" &&
    first.truncated &&
    !shouldStreamDriveChunks &&
    !baseHeaders.range &&
    canFollowRange(first.headers, first.status);

  const responseHeaders = proxyResponseHeaders(first.headers ?? {}, first.body, request.method, first.truncated, {
    streaming: shouldStreamRanges || shouldStreamDriveChunks,
  });
  setHeader(responseHeaders, "x-skirk-cf-free", "1");
  setHeader(responseHeaders, "x-skirk-cf-mode", options.scheme === "https" ? "mitm-https" : "http");
  setHeader(responseHeaders, "x-skirk-cf-upstream-ms", String(Math.round(first.durationMs ?? 0)));
  setHeader(responseHeaders, "x-skirk-cf-truncated", first.truncated ? "1" : "0");

  if (first.truncated && !shouldStreamRanges && !shouldStreamDriveChunks) {
    throw new Error(`Worker response exceeded ${maxBytes} bytes and cannot be completed with Range`);
  }

  response.writeHead(first.status ?? 502, responseHeaders);
  if (request.method === "HEAD") {
    response.end();
    return;
  }
  await writeResponseChunk(response, first.body);

  if (shouldStreamDriveChunks) {
    const chunkResult = await streamDriveResponseChunks(response, {
      responseFileIds: first.responseFileIds,
      nextIndex: Number.isFinite(first.chunkIndex) ? Number(first.chunkIndex) + 1 : 1,
      started: performance.now(),
      isCanceled: () => clientClosed,
    });
    if (!chunkResult.complete) {
      response.destroy(new Error(`Drive chunk continuation stopped after ${chunkResult.chunks} chunks`));
      return;
    }
  } else if (shouldStreamRanges) {
    const rangeResult = await streamRangeContinuation(response, {
      requestId,
      method: request.method,
      targetUrl,
      baseHeaders,
      offset: first.body.length,
      totalLength: responseTotalLength(first.headers),
      isCanceled: () => clientClosed,
    });
    if (!rangeResult.complete) {
      response.destroy(new Error(`range continuation stopped at ${rangeResult.offset} bytes`));
      return;
    }
  }
  response.end();
}

async function relayWorkerFetch({
  requestId,
  method,
  targetUrl,
  headers,
  body,
  started = performance.now(),
  isCanceled = () => false,
  chunked = false,
}) {
  if (isCanceled()) {
    throw new Error("client closed before request upload");
  }
  const responseFileIds = await drive.generateIds(chunked ? maxStreamingChunks : 1);
  const responseFileId = responseFileIds[0];
  const responseMaxBytes = chunked ? streamingChunkBytes : maxBytes;
  const requestName = `${requestPrefix}${requestId}`;
  const responseNeedle = `${responsePrefix}${requestId}`;
  let requestFile;
  try {
    requestFile = await drive.uploadWithParentFallback({
      name: requestName,
      folderId,
      mimeType: "application/json",
      body: JSON.stringify({
        kind: "http_proxy_v1",
        requestId,
        method,
        url: targetUrl.href,
        headers,
        bodyBase64: body.length > 0 ? body.toString("base64") : undefined,
        maxBytes: responseMaxBytes,
        responseFileId,
        ...(chunked ? { responseFileIds, responseChunkBytes: streamingChunkBytes } : {}),
        responseFormat: "binary_v1",
        createdAt: Date.now(),
        expiresAt: Date.now() + timeoutMs,
      }),
    });

    const responseFile = await waitForWorkerResponse({
      requestId,
      requestName,
      responseNeedle,
      responseFileId,
      started,
      isCanceled,
    });
    const payload = decodeWorkerResponse(await drive.download(responseFile.id));
    await drive.delete(responseFile.id);
    if (!payload.ok) {
      throw new Error(payload.error || "Worker returned an error response");
    }
    if (chunked) {
      payload.responseFileIds = responseFileIds;
    }
    return payload;
  } finally {
    if (requestFile?.id) {
      await drive.delete(requestFile.id).catch(() => undefined);
    }
  }
}

async function waitForWorkerResponse({ requestId, requestName, responseNeedle, responseFileId, started, isCanceled = () => false }) {
  let pumpCalls = 0;
  while (performance.now() - started < timeoutMs) {
    if (isCanceled()) {
      throw new Error("client closed while waiting for Worker response");
    }
    if (!driveOnly) {
      const pumpResult = triggerAdminPump();
      pumpCalls += pumpResult.started ? 1 : 0;
      const pumped = responseFromPumpResult(lastPumpResult, requestId, requestName, responseFileId);
      if (pumped?.responseFileId) {
        return { id: pumped.responseFileId, name: pumped.responseName };
      }
    }
    const byId = await drive.metadata(responseFileId).catch((error) => {
      if (isDriveNotFound(error)) {
        return undefined;
      }
      throw error;
    });
    if (byId?.id) {
      return byId;
    }
    if (legacyListFallback) {
      const listed = await drive.findByNameContains(responseNeedle, folderId).catch(() => undefined);
      if (listed?.id) {
        return listed;
      }
    }
    await sleep(pollMs);
  }
  throw new Error(`timed out waiting for Worker response after ${pumpCalls} pump calls`);
}

async function waitForResponseFileId(responseFileId, started, isCanceled = () => false) {
  while (performance.now() - started < timeoutMs) {
    if (isCanceled()) {
      throw new Error("client closed while waiting for Worker response chunk");
    }
    if (!driveOnly) {
      triggerAdminPump();
    }
    const byId = await drive.metadata(responseFileId).catch((error) => {
      if (isDriveNotFound(error)) {
        return undefined;
      }
      throw error;
    });
    if (byId?.id) {
      return byId;
    }
    await sleep(pollMs);
  }
  throw new Error("timed out waiting for Worker response chunk");
}

function responseFromPumpResult(pumpResult, requestId, requestName, responseFileId) {
  for (const result of pumpResult?.exitResults ?? []) {
    if (
      result?.responseFileId === responseFileId ||
      result?.requestName === requestName ||
      String(result?.requestName ?? "").endsWith(`/${requestId}`) ||
      String(result?.responseName ?? "").includes(requestId)
    ) {
      return result;
    }
  }
}

function targetUrlForRequest(request, options = {}) {
  const rawUrl = request.url ?? "/";
  if (/^https?:\/\//i.test(rawUrl)) {
    return validatedTargetUrl(rawUrl, { allowHttps: options.scheme === "https" });
  }
  const host = options.authority ?? request.headers.host;
  if (!host) {
    throw new Error("proxy request missing Host header");
  }
  const scheme = options.scheme === "https" ? "https" : "http";
  return validatedTargetUrl(`${scheme}://${host}${rawUrl.startsWith("/") ? rawUrl : `/${rawUrl}`}`, {
    allowHttps: options.scheme === "https",
  });
}

function validatedTargetUrl(rawUrl, options = {}) {
  const url = new URL(rawUrl);
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new Error("only http and https URLs are supported");
  }
  if (url.protocol === "https:" && !options.allowHttps) {
    throw new Error("HTTPS through an HTTP proxy uses CONNECT, which Cloudflare Free HTTP relay does not support");
  }
  if (url.username || url.password) {
    throw new Error("URLs with embedded credentials are not supported");
  }
  if (!isAllowedDestinationHost(url.hostname)) {
    throw new Error(`blocked private or unsafe destination host: ${url.hostname}`);
  }
  return url;
}

function proxyResponseHeaders(rawHeaders, body, method, truncated, options = {}) {
  const headers = {};
  for (const [key, value] of Object.entries(rawHeaders)) {
    const lower = key.toLowerCase();
    if (UNSAFE_RESPONSE_HEADERS.has(lower)) {
      continue;
    }
    if (lower === "content-length") {
      continue;
    }
    headers[lower] = Array.isArray(value) ? value.map(String) : String(value);
  }
  if (method !== "HEAD" && !options.streaming) {
    headers["content-length"] = String(body.length);
  }
  if (truncated) {
    headers.warning = "199 skirk-cf-free response truncated by local max-bytes";
  }
  return headers;
}

function proxyHeaders(headers) {
  const out = {};
  for (const [key, value] of Object.entries(headers)) {
    if (UNSAFE_REQUEST_HEADERS.has(key.toLowerCase())) {
      continue;
    }
    if (Array.isArray(value)) {
      out[key] = value.join(", ");
    } else if (typeof value === "string") {
      out[key] = value;
    }
  }
  return out;
}

function setHeader(headers, key, value) {
  headers[key.toLowerCase()] = value;
}

function decodeWorkerResponse(bytes) {
  const buffer = Buffer.from(bytes);
  if (buffer.subarray(0, BINARY_PROXY_MAGIC.length).equals(BINARY_PROXY_MAGIC)) {
    const headerLength = buffer.readUInt32BE(BINARY_PROXY_MAGIC.length);
    const headerStart = BINARY_PROXY_MAGIC.length + 4;
    const headerEnd = headerStart + headerLength;
    if (headerEnd > buffer.length) {
      throw new Error("Worker binary response header exceeds response body");
    }
    const payload = JSON.parse(buffer.subarray(headerStart, headerEnd).toString("utf8"));
    payload.body = buffer.subarray(headerEnd);
    return payload;
  }
  const payload = JSON.parse(buffer.toString("utf8"));
  payload.body = Buffer.from(payload.bodyBase64 ?? "", "base64");
  return payload;
}

function canFollowRange(headers, status) {
  if (status !== 200) {
    return false;
  }
  const acceptRanges = String(headers?.["accept-ranges"] ?? "").toLowerCase();
  return acceptRanges.includes("bytes");
}

async function streamRangeContinuation(response, input) {
  let offset = input.offset;
  let totalLength = input.totalLength;
  let chunks = 0;
  let complete = Number.isFinite(totalLength) ? offset >= totalLength : false;

  while (!response.destroyed && chunks < maxRangeChunks && !complete) {
    const end = Number.isFinite(totalLength) ? Math.min(totalLength - 1, offset + maxBytes - 1) : offset + maxBytes - 1;
    const chunkId = `${input.requestId}-r${chunks + 1}`;
    const chunkHeaders = {
      ...input.baseHeaders,
      range: `bytes=${offset}-${end}`,
    };
    const chunk = await relayWorkerFetch({
      requestId: chunkId,
      method: input.method,
      targetUrl: input.targetUrl,
      headers: chunkHeaders,
      body: Buffer.alloc(0),
      started: performance.now(),
      isCanceled: input.isCanceled,
    });
    if (!chunk.ok || chunk.status !== 206 || chunk.body.length === 0) {
      break;
    }
    const chunkTotal = responseTotalLength(chunk.headers);
    if (Number.isFinite(chunkTotal)) {
      totalLength = chunkTotal;
    }
    await writeResponseChunk(response, chunk.body);
    offset += chunk.body.length;
    chunks += 1;
    if (Number.isFinite(totalLength)) {
      complete = offset >= totalLength;
    } else if (!chunk.truncated && chunk.body.length < maxBytes) {
      complete = true;
    }
  }
  return { offset, totalLength, complete };
}

async function streamDriveResponseChunks(response, input) {
  let chunks = 0;
  for (let index = input.nextIndex; index < input.responseFileIds.length && !response.destroyed; index += 1) {
    const fileId = input.responseFileIds[index];
    if (!fileId) {
      break;
    }
    const file = await waitForResponseFileId(fileId, input.started, input.isCanceled);
    const payload = decodeWorkerResponse(await drive.download(file.id));
    await drive.delete(file.id);
    if (!payload.ok) {
      throw new Error(payload.error || "Worker returned an error response chunk");
    }
    await writeResponseChunk(response, payload.body);
    chunks += 1;
    if (payload.done || !payload.more) {
      return { complete: true, chunks };
    }
  }
  return { complete: false, chunks };
}

function responseTotalLength(headers) {
  const contentRangeLength = totalLengthFromContentRange(headers?.["content-range"]);
  if (Number.isFinite(contentRangeLength)) {
    return contentRangeLength;
  }
  const raw = headers?.["content-length"];
  const parsed = Number.parseInt(String(raw ?? ""), 10);
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : Number.NaN;
}

function totalLengthFromContentRange(raw) {
  const value = Array.isArray(raw) ? raw[0] : raw;
  const match = /^bytes\s+\d+-\d+\/(\d+|\*)$/i.exec(String(value ?? "").trim());
  if (!match || match[1] === "*") {
    return Number.NaN;
  }
  const parsed = Number.parseInt(match[1], 10);
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : Number.NaN;
}

async function writeResponseChunk(response, body) {
  if (!body.length) {
    return;
  }
  if (response.write(body)) {
    return;
  }
  await new Promise((resolve) => response.once("drain", resolve));
}

async function collectRequestBody(request, limit) {
  if (request.method === "GET" || request.method === "HEAD") {
    drainRequest(request);
    return Buffer.alloc(0);
  }
  const chunks = [];
  let total = 0;
  for await (const chunk of request) {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    total += buffer.length;
    if (total > limit) {
      throw new Error(`request body exceeds ${limit} byte local relay cap`);
    }
    chunks.push(buffer);
  }
  return Buffer.concat(chunks);
}

function isRelayMethod(method) {
  return ["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"].includes(method ?? "");
}

class LocalCertificateAuthority {
  static async open(directory) {
    await mkdir(directory, { recursive: true, mode: 0o700 });
    const authority = new LocalCertificateAuthority(directory);
    await authority.loadOrCreate();
    return authority;
  }

  constructor(directory) {
    this.directory = directory;
    this.certPath = path.join(directory, "skirk-cf-free-ca.crt");
    this.keyPath = path.join(directory, "skirk-cf-free-ca.key");
    this.contexts = new Map();
  }

  async loadOrCreate() {
    await assertSafePath(this.directory, { directory: true, mode: 0o700 });
    try {
      await assertSafePath(this.certPath, { optional: true, mode: 0o644 });
      await assertSafePath(this.keyPath, { optional: true, mode: 0o600 });
      this.caCertPem = await readFile(this.certPath, "utf8");
      this.caKeyPem = await readFile(this.keyPath, "utf8");
      this.caCert = forge.pki.certificateFromPem(this.caCertPem);
      this.caKey = forge.pki.privateKeyFromPem(this.caKeyPem);
      return;
    } catch {
      // Fall through to first-run CA generation.
    }
    const keys = forge.pki.rsa.generateKeyPair({ bits: 2048, workers: 2 });
    const cert = forge.pki.createCertificate();
    cert.publicKey = keys.publicKey;
    cert.serialNumber = randomSerial();
    const now = new Date();
    cert.validity.notBefore = new Date(now.getTime() - 60_000);
    cert.validity.notAfter = new Date(now.getTime() + 397 * 24 * 60 * 60 * 1000);
    const attrs = [
      { name: "commonName", value: "Skirk Cloudflare Free Local Relay CA" },
      { name: "organizationName", value: "Skirk local relay" },
    ];
    cert.setSubject(attrs);
    cert.setIssuer(attrs);
    cert.setExtensions([
      { name: "basicConstraints", cA: true, critical: true },
      { name: "keyUsage", keyCertSign: true, cRLSign: true, digitalSignature: true, critical: true },
      { name: "subjectKeyIdentifier" },
    ]);
    cert.sign(keys.privateKey, forge.md.sha256.create());
    this.caCertPem = forge.pki.certificateToPem(cert);
    this.caKeyPem = forge.pki.privateKeyToPem(keys.privateKey);
    await writeFile(this.certPath, this.caCertPem, { mode: 0o644 });
    await writeFile(this.keyPath, this.caKeyPem, { mode: 0o600 });
    this.caCert = cert;
    this.caKey = keys.privateKey;
  }

  contextForHost(hostname) {
    const host = hostname.toLowerCase().replace(/^\[|\]$/g, "");
    const cached = this.contexts.get(host);
    if (cached) {
      return cached;
    }
    const keys = forge.pki.rsa.generateKeyPair({ bits: 2048, workers: 2 });
    const cert = forge.pki.createCertificate();
    cert.publicKey = keys.publicKey;
    cert.serialNumber = randomSerial();
    const now = new Date();
    cert.validity.notBefore = new Date(now.getTime() - 60_000);
    cert.validity.notAfter = new Date(now.getTime() + 90 * 24 * 60 * 60 * 1000);
    cert.setSubject([{ name: "commonName", value: host.slice(0, 64) || "unknown" }]);
    cert.setIssuer(this.caCert.subject.attributes);
    cert.setExtensions([
      { name: "basicConstraints", cA: false, critical: true },
      { name: "keyUsage", digitalSignature: true, keyEncipherment: true, critical: true },
      { name: "extKeyUsage", serverAuth: true },
      { name: "subjectAltName", altNames: subjectAltNames(host) },
    ]);
    cert.sign(this.caKey, forge.md.sha256.create());
    const context = tls.createSecureContext({
      key: forge.pki.privateKeyToPem(keys.privateKey),
      cert: forge.pki.certificateToPem(cert),
      ALPNProtocols: ["http/1.1"],
    });
    this.contexts.set(host, context);
    return context;
  }

  fingerprint() {
    const der = forge.asn1.toDer(forge.pki.certificateToAsn1(this.caCert)).getBytes();
    const digest = forge.md.sha256.create();
    digest.update(der);
    return digest.digest().toHex().match(/.{2}/g).join(":");
  }
}

function subjectAltNames(host) {
  if (/^\d+\.\d+\.\d+\.\d+$/.test(host)) {
    return [{ type: 7, ip: host }];
  }
  if (host.includes(":")) {
    return [{ type: 7, ip: host }];
  }
  return [{ type: 2, value: host }];
}

function randomSerial() {
  return forge.util.bytesToHex(forge.random.getBytesSync(16)).replace(/^00/, "01");
}

async function assertSafePath(target, options = {}) {
  let stat;
  try {
    stat = await lstat(target);
  } catch (error) {
    if (options.optional && error?.code === "ENOENT") {
      return;
    }
    throw error;
  }
  if (stat.isSymbolicLink()) {
    throw new Error(`refusing MITM CA symlink path: ${target}`);
  }
  if (options.directory && !stat.isDirectory()) {
    throw new Error(`MITM CA path is not a directory: ${target}`);
  }
  if (!options.directory && stat.isDirectory()) {
    throw new Error(`MITM CA file path is a directory: ${target}`);
  }
  if (process.platform !== "win32" && typeof options.mode === "number") {
    await chmod(target, options.mode);
  }
}

class DriveApi {
  accessToken = "";
  expiresAt = 0;

  constructor(config) {
    this.config = config;
  }

  async refresh() {
    if (this.accessToken && this.expiresAt > Date.now() + 60_000) {
      return this.accessToken;
    }
    const auth = this.config.auth ?? {};
    const response = await fetch("https://oauth2.googleapis.com/token", {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({
        client_id: required(auth.client_id, "auth.client_id"),
        client_secret: auth.client_secret ?? "",
        refresh_token: required(auth.refresh_token, "auth.refresh_token"),
        grant_type: "refresh_token",
      }),
    });
    if (!response.ok) {
      throw new Error(`oauth refresh failed: ${response.status} ${await response.text()}`);
    }
    const payload = await response.json();
    this.accessToken = payload.access_token;
    this.expiresAt = Date.now() + Math.max(1, payload.expires_in ?? 3600) * 1000;
    return this.accessToken;
  }

  async upload(input) {
    const boundary = `skirk-cf-http-${randomUUID()}`;
    const metadata = {
      ...(input.id ? { id: input.id } : {}),
      name: input.name,
      mimeType: input.mimeType,
      ...(input.folderId ? { parents: [input.folderId] } : {}),
    };
    const body = new Blob([
      `--${boundary}\r\ncontent-type: application/json; charset=utf-8\r\n\r\n`,
      JSON.stringify(metadata),
      `\r\n--${boundary}\r\ncontent-type: ${input.mimeType}\r\n\r\n`,
      input.body,
      `\r\n--${boundary}--\r\n`,
    ]);
    const url = new URL("/upload/drive/v3/files", DRIVE_UPLOAD_URL);
    url.searchParams.set("uploadType", "multipart");
    url.searchParams.set("fields", "id,name,size");
    const response = await fetch(url, {
      method: "POST",
      headers: {
        authorization: `Bearer ${await this.refresh()}`,
        "content-type": `multipart/related; boundary=${boundary}`,
      },
      body,
    });
    if (!response.ok) {
      throw new Error(`drive upload failed: ${response.status} ${await response.text()}`);
    }
    return response.json();
  }

  async generateIds(count) {
    const url = new URL("/drive/v3/files/generateIds", DRIVE_URL);
    url.searchParams.set("count", String(count));
    url.searchParams.set("fields", "ids");
    const response = await fetch(url, {
      headers: { authorization: `Bearer ${await this.refresh()}` },
    });
    const text = await response.text();
    if (!response.ok) {
      throw new Error(`drive generate ids failed: ${response.status} ${text}`);
    }
    const ids = JSON.parse(text).ids?.filter(Boolean) ?? [];
    if (ids.length < count) {
      throw new Error(`drive generate ids returned ${ids.length}, wanted ${count}`);
    }
    return ids;
  }

  async uploadWithParentFallback(input) {
    try {
      return await this.upload(input);
    } catch (error) {
      if (!input.folderId || !isMissingDriveParent(error)) {
        throw error;
      }
      return this.upload({
        ...input,
        folderId: undefined,
      });
    }
  }

  async findByNameContains(contains, parentId) {
    const url = new URL("/drive/v3/files", DRIVE_URL);
    const q = [`trashed = false`, `name contains '${escapeDriveQuery(contains)}'`];
    if (parentId) {
      q.push(`'${escapeDriveQuery(parentId)}' in parents`);
    }
    url.searchParams.set("q", q.join(" and "));
    url.searchParams.set("pageSize", "10");
    url.searchParams.set("fields", "files(id,name,size,createdTime)");
    const response = await fetch(url, {
      headers: { authorization: `Bearer ${await this.refresh()}` },
    });
    if (!response.ok) {
      throw new Error(`drive list failed: ${response.status} ${await response.text()}`);
    }
    const file = (await response.json()).files?.[0];
    if (file || !parentId) {
      return file;
    }
    return this.findByNameContains(contains, undefined);
  }

  async metadata(fileId) {
    const url = new URL(`/drive/v3/files/${encodeURIComponent(fileId)}`, DRIVE_URL);
    url.searchParams.set("fields", "id,name,size,createdTime");
    const response = await fetch(url, {
      headers: { authorization: `Bearer ${await this.refresh()}` },
    });
    const text = await response.text();
    if (!response.ok) {
      throw new Error(`drive metadata failed: ${response.status} ${text}`);
    }
    return JSON.parse(text);
  }

  async download(fileId) {
    const response = await fetch(`${DRIVE_URL}/files/${encodeURIComponent(fileId)}?alt=media`, {
      headers: { authorization: `Bearer ${await this.refresh()}` },
    });
    if (!response.ok) {
      throw new Error(`drive download failed: ${response.status} ${await response.text()}`);
    }
    return new Uint8Array(await response.arrayBuffer());
  }

  async delete(fileId) {
    if (!fileId) {
      return;
    }
    const response = await fetch(`${DRIVE_URL}/files/${encodeURIComponent(fileId)}`, {
      method: "DELETE",
      headers: { authorization: `Bearer ${await this.refresh()}` },
    });
    if (!response.ok && response.status !== 404) {
      throw new Error(`drive delete failed: ${response.status} ${await response.text()}`);
    }
  }
}

async function adminPump(workerUrl, adminToken) {
  const response = await fetch(new URL("/admin/pump", workerUrl), {
    method: "POST",
    headers: { authorization: `Bearer ${adminToken}` },
  });
  if (!response.ok) {
    throw new Error(`admin pump failed: ${response.status} ${await response.text()}`);
  }
  return response.json();
}

function triggerAdminPump() {
  if (!workerUrl || !adminToken) {
    return { promise: Promise.resolve(lastPumpResult), started: false };
  }
  const now = performance.now();
  if (lastPumpResult && now - lastPumpAt < minPumpGapMs) {
    return { promise: Promise.resolve(lastPumpResult), started: false };
  }
  let started = false;
  if (!pumpInFlight) {
    started = true;
    pumpInFlight = adminPump(workerUrl, adminToken)
      .then((result) => {
        lastPumpAt = performance.now();
        lastPumpResult = result;
        return result;
      })
      .finally(() => {
        pumpInFlight = undefined;
      });
    pumpInFlight.catch(() => undefined);
  }
  return { promise: pumpInFlight, started };
}

async function sharedAdminPump() {
  return triggerAdminPump().promise;
}

function optionalUrl(value) {
  const trimmed = String(value ?? "").trim();
  return trimmed || undefined;
}

async function loadConfig(options) {
  if (options.config) {
    return JSON.parse(await readFile(options.config, "utf8"));
  }
  return {
    auth: {
      client_id: required(process.env.GOOGLE_OAUTH_CLIENT_ID, "GOOGLE_OAUTH_CLIENT_ID"),
      client_secret: process.env.GOOGLE_OAUTH_CLIENT_SECRET ?? "",
      refresh_token: required(process.env.GOOGLE_REFRESH_TOKEN, "GOOGLE_REFRESH_TOKEN"),
    },
    drive: {
      folder_id: process.env.SKIRK_DRIVE_FOLDER_ID,
    },
  };
}

function parseArgs(argv) {
  const parsed = {};
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (!arg.startsWith("--")) {
      continue;
    }
    const [rawKey, rawValue] = arg.slice(2).split("=", 2);
    const key = rawKey.replace(/-([a-z])/g, (_, char) => char.toUpperCase());
    if (rawValue !== undefined) {
      parsed[key] = rawValue;
    } else if (index + 1 < argv.length && !argv[index + 1].startsWith("--")) {
      parsed[key] = argv[++index];
    } else {
      parsed[key] = true;
    }
  }
  return parsed;
}

function booleanOption(value, fallback) {
  if (value === undefined) {
    return fallback;
  }
  if (typeof value === "boolean") {
    return value;
  }
  const normalized = String(value).trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(normalized)) {
    return true;
  }
  if (["0", "false", "no", "off"].includes(normalized)) {
    return false;
  }
  return fallback;
}

function parseListen(value) {
  const index = value.lastIndexOf(":");
  if (index < 0) {
    return { host: "127.0.0.1", port: Number.parseInt(value, 10) };
  }
  return { host: value.slice(0, index), port: Number.parseInt(value.slice(index + 1), 10) };
}

function isAllowedDestinationHost(hostname) {
  const host = hostname.toLowerCase().replace(/^\[|\]$/g, "");
  if (
    !host ||
    host === "localhost" ||
    host.endsWith(".localhost") ||
    host.endsWith(".local") ||
    host.endsWith(".internal") ||
    host === "metadata.google.internal"
  ) {
    return false;
  }
  const mapped = host.match(/^::ffff:(\d+\.\d+\.\d+\.\d+)$/);
  if (mapped) {
    return isAllowedDestinationHost(mapped[1]);
  }
  if (/^\d+\.\d+\.\d+\.\d+$/.test(host)) {
    const parts = host.split(".").map((part) => Number.parseInt(part, 10));
    if (parts.some((part) => !Number.isFinite(part) || part < 0 || part > 255)) {
      return false;
    }
    const [a, b, c] = parts;
    return !(
      a === 0 ||
      a === 10 ||
      a === 127 ||
      (a === 100 && b >= 64 && b <= 127) ||
      (a === 169 && b === 254) ||
      (a === 172 && b >= 16 && b <= 31) ||
      (a === 192 && (b === 0 || b === 168)) ||
      (a === 198 && (b === 18 || b === 19 || (b === 51 && c === 100))) ||
      (a === 203 && b === 0 && c === 113) ||
      a >= 224
    );
  }
  if (host.includes(":")) {
    const normalized = host.replace(/^0+:/, ":");
    return !(
      normalized === "::1" ||
      normalized === "::" ||
      normalized.startsWith("fc") ||
      normalized.startsWith("fd") ||
      normalized.startsWith("fe8") ||
      normalized.startsWith("fe9") ||
      normalized.startsWith("fea") ||
      normalized.startsWith("feb")
    );
  }
  return true;
}

function integerOption(value, fallback) {
  const parsed = Number.parseInt(String(value ?? ""), 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

function escapeDriveQuery(value) {
  return value.replace(/\\/g, "\\\\").replace(/'/g, "\\'");
}

function required(value, name) {
  if (!value) {
    throw new Error(`${name} is required`);
  }
  return value;
}

function isMissingDriveParent(error) {
  const message = error instanceof Error ? error.message.toLowerCase() : String(error).toLowerCase();
  return message.includes("drive upload failed: 404") && message.includes("file not found");
}

function isDriveNotFound(error) {
  const message = error instanceof Error ? error.message.toLowerCase() : String(error).toLowerCase();
  return message.includes("failed: 404") || message.includes("not found");
}

function drainRequest(request) {
  request.resume();
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

ca = mitmEnabled ? await LocalCertificateAuthority.open(caDir) : undefined;
drive = new DriveApi(config);
await drive.refresh();
await sharedAdminPump().catch(() => undefined);

const server = http.createServer((request, response) => {
  void handleProxyRequest(request, response).catch((error) => {
    const message = error instanceof Error ? error.message : String(error);
    response.writeHead(502, {
      "content-type": "text/plain; charset=utf-8",
      "cache-control": "no-store",
    });
    response.end(`Skirk Cloudflare Free HTTP relay failed: ${message}\n`);
  });
});

const mitmHttpServer = http.createServer((request, response) => {
  const context = request.socket.skirkMitmContext;
  void handleProxyRequest(request, response, context).catch((error) => {
    const message = error instanceof Error ? error.message : String(error);
    response.writeHead(502, {
      "content-type": "text/plain; charset=utf-8",
      "cache-control": "no-store",
    });
    response.end(`Skirk Cloudflare Free HTTPS relay failed: ${message}\n`);
  });
});

server.on("connect", (request, socket, head) => {
  void handleConnectRequest(request, socket, head).catch((error) => {
    socket.write(`HTTP/1.1 502 Bad Gateway\r\nconnection: close\r\n\r\n${error instanceof Error ? error.message : String(error)}\n`);
    socket.destroy();
  });
});

async function handleConnectRequest(request, socket, head) {
  if (!mitmEnabled || !ca) {
    socket.write(
      [
        "HTTP/1.1 501 Not Implemented",
        "content-type: text/plain; charset=utf-8",
        "connection: close",
        "",
        "Skirk Cloudflare Free web relay requires --mitm for HTTPS CONNECT. This is an inspected HTTPS mode, not an opaque tunnel.",
        "",
      ].join("\r\n"),
    );
    socket.destroy();
    return;
  }
  const { host, port } = parseAuthority(request.url ?? "");
  if (port !== 443) {
    socket.write("HTTP/1.1 403 Forbidden\r\nconnection: close\r\n\r\nSkirk CF MITM only supports HTTPS CONNECT port 443.\n");
    socket.destroy();
    return;
  }
  if (!isAllowedDestinationHost(host)) {
    socket.write("HTTP/1.1 403 Forbidden\r\nconnection: close\r\n\r\nBlocked private or unsafe CONNECT destination.\n");
    socket.destroy();
    return;
  }
  socket.write("HTTP/1.1 200 Connection Established\r\n\r\n");
  const secureSocket = new tls.TLSSocket(socket, {
    isServer: true,
    secureContext: ca.contextForHost(host),
    ALPNProtocols: ["http/1.1"],
  });
  if (head?.length) {
    secureSocket.unshift(head);
  }
  secureSocket.skirkMitmContext = { scheme: "https", authority: host };
  secureSocket.on("secure", () => {
    mitmHttpServer.emit("connection", secureSocket);
  });
  secureSocket.on("error", () => {
    secureSocket.destroy();
  });
}

function parseAuthority(value) {
  if (!value.trim()) {
    throw new Error("CONNECT authority is required");
  }
  const withScheme = value.includes("://") ? value : `https://${value}`;
  const url = new URL(withScheme);
  return {
    host: url.hostname,
    port: Number.parseInt(url.port || "443", 10),
  };
}

server.listen(listen.port, listen.host, () => {
  console.log(`Skirk Cloudflare Free web relay listening on http://${listen.host}:${listen.port}`);
  console.log(`HTTP methods: GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS. Response cap: ${maxBytes} bytes.`);
  console.log(`Drive-only wake mode: ${driveOnly ? "enabled" : "disabled; direct Worker admin pump is enabled for lab use"}.`);
  if (mitmEnabled && ca) {
    console.log("HTTPS MITM mode: enabled. Install and trust this local CA in the browser/system you test with:");
    console.log(`  ${ca.certPath}`);
    console.log(`  SHA-256 ${ca.fingerprint()}`);
  } else {
    console.log("HTTPS MITM mode: disabled. Add --mitm to inspect HTTPS CONNECT traffic.");
  }
});
