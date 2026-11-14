import type { CandidateObject, ExitProcessResult } from "./types";

const DEFAULT_REQUEST_PREFIX = "cfexit/v1/req/";
const DEFAULT_RESPONSE_PREFIX = "cfexit/v1/res/";
const DEFAULT_MAX_RESPONSE_BYTES = 32 * 1024 * 1024;
const MAX_RESPONSE_BYTES_LIMIT = 32 * 1024 * 1024;
const MAX_REQUEST_BODY_BYTES = 1024 * 1024;
const BINARY_PROXY_MAGIC = "SKIRKCF1";
type HeaderBag = Record<string, string | string[]>;

export interface ExitDriveClient {
  downloadFile(fileId: string): Promise<ArrayBuffer>;
  uploadFile(input: {
    id?: string;
    name: string;
    body: BlobPart;
    mimeType?: string;
    parents?: string[];
  }): Promise<{ id?: string; name?: string; size?: string }>;
  deleteFile(fileId: string): Promise<void>;
}

export interface ExitProcessorOptions {
  requestPrefix?: string;
  responsePrefix?: string;
  folderId?: string;
  maxResponseBytes?: number;
  fetcher?: typeof fetch;
}

export interface WorkerExitRequest {
  kind?: "http_fetch_v1" | "http_proxy_v1" | "tcp_http_v1";
  requestId?: string;
  url?: string;
  host?: string;
  port?: number;
  path?: string;
  secureTransport?: "on" | "off";
  method?: string;
  headers?: Record<string, string | string[]>;
  bodyBase64?: string;
  maxBytes?: number;
  responseFileId?: string;
  responseFileIds?: string[];
  responseChunkBytes?: number;
  responseFormat?: "json_v1" | "binary_v1";
  createdAt?: number;
  expiresAt?: number;
}

interface UpstreamResult {
  status?: number;
  body: Uint8Array;
  upstreamBytes: number;
  durationMs: number;
  truncated: boolean;
}

export async function processExitCandidate(
  drive: ExitDriveClient,
  candidate: CandidateObject,
  options: ExitProcessorOptions = {},
): Promise<ExitProcessResult> {
  const requestPrefix = options.requestPrefix ?? DEFAULT_REQUEST_PREFIX;
  if (!candidate.name.startsWith(requestPrefix)) {
    return { processed: false, requestFileId: candidate.fileId, requestName: candidate.name };
  }

  const requestId = requestIdFromCandidate(candidate, requestPrefix);
  let responseName = responseObjectName(options.responsePrefix, requestId, "error", 0, 0, false);
  let requestPayload: WorkerExitRequest | undefined;
  try {
    const request = parseRequest(await drive.downloadFile(candidate.fileId));
    requestPayload = request;
    if (request.expiresAt && Date.now() > request.expiresAt) {
      throw new Error("request expired before Worker processing");
    }
    const maxBytes = configuredMaxBytes(request.maxBytes, options.maxResponseBytes);
    if ((request.kind ?? "http_fetch_v1") === "http_proxy_v1" && request.responseFormat === "binary_v1" && request.responseFileIds?.length) {
      const streamed = await fetchHttpProxyChunks(drive, request, maxBytes, options);
      await drive.deleteFile(candidate.fileId);
      return {
        processed: true,
        requestFileId: candidate.fileId,
        requestName: candidate.name,
        ...streamed,
        deletedRequest: true,
      };
    }
    const upstream = await executeRequest(request, maxBytes, options.fetcher ?? fetch);
    responseName = responseObjectName(
      options.responsePrefix,
      request.requestId || requestId,
      String(upstream.status ?? "tcp"),
      upstream.body.byteLength,
      upstream.durationMs,
      upstream.truncated,
    );
    const uploaded = await uploadResponseFile(drive, {
      id: request.responseFileId,
      name: responseName,
      body: arrayBufferForBody(upstream.body),
      mimeType: "application/octet-stream",
      parents: options.folderId ? [options.folderId] : undefined,
    });
    await drive.deleteFile(candidate.fileId);
    return {
      processed: true,
      requestFileId: candidate.fileId,
      requestName: candidate.name,
      responseName,
      responseFileId: uploaded.id,
      responseBytes: upstream.body.byteLength,
      upstreamBytes: upstream.upstreamBytes,
      upstreamStatus: upstream.status,
      upstreamMs: upstream.durationMs,
      truncated: upstream.truncated,
      deletedRequest: true,
    };
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    const payload = new TextEncoder().encode(JSON.stringify({ ok: false, error: message }) + "\n");
    try {
      const uploaded = await uploadResponseFile(drive, {
        id: requestPayload?.responseFileId,
        name: responseName,
        body: arrayBufferForBody(payload),
        mimeType: "application/json",
        parents: options.folderId ? [options.folderId] : undefined,
      });
      await drive.deleteFile(candidate.fileId);
      return {
        processed: true,
        requestFileId: candidate.fileId,
        requestName: candidate.name,
        responseName,
        responseFileId: uploaded.id,
        responseBytes: payload.byteLength,
        deletedRequest: true,
        error: message,
      };
    } catch (cleanupError) {
      return {
        processed: true,
        requestFileId: candidate.fileId,
        requestName: candidate.name,
        responseName,
        responseBytes: payload.byteLength,
        deletedRequest: false,
        error: `${message}; failed to publish error response: ${
          cleanupError instanceof Error ? cleanupError.message : String(cleanupError)
        }`,
      };
    }
  }
}

export function defaultRequestPrefix(): string {
  return DEFAULT_REQUEST_PREFIX;
}

export function defaultResponsePrefix(): string {
  return DEFAULT_RESPONSE_PREFIX;
}

export function configuredMaxBytes(requestMaxBytes?: number, envMaxBytes?: number): number {
  const envCap = clampMaxBytes(envMaxBytes, DEFAULT_MAX_RESPONSE_BYTES);
  if (!Number.isFinite(requestMaxBytes) || !requestMaxBytes || requestMaxBytes < 1) {
    return envCap;
  }
  return Math.min(clampMaxBytes(requestMaxBytes, envCap), envCap);
}

async function executeRequest(request: WorkerExitRequest, maxBytes: number, fetcher: typeof fetch): Promise<UpstreamResult> {
  switch (request.kind ?? "http_fetch_v1") {
    case "http_fetch_v1":
      return fetchHttp(request, maxBytes, fetcher);
    case "http_proxy_v1":
      return fetchHttpProxy(request, maxBytes, fetcher);
    case "tcp_http_v1":
      return tcpHttp(request, maxBytes);
    default:
      throw new Error(`unsupported worker exit request kind: ${String(request.kind)}`);
  }
}

async function fetchHttp(request: WorkerExitRequest, maxBytes: number, fetcher: typeof fetch): Promise<UpstreamResult> {
  if (request.method && request.method !== "GET" && request.method !== "HEAD") {
    throw new Error("http_fetch_v1 only supports GET and HEAD");
  }
  if (!request.url) {
    throw new Error("http_fetch_v1 requires url");
  }
  const url = validatedPublicHttpUrl(request.url);
  const started = performance.now();
  const response = await fetcher(url, {
    method: request.method ?? "GET",
    headers: filteredRequestHeaders(request.headers),
    redirect: "manual",
  });
  const body = await collectLimited(response.body, maxBytes);
  return {
    status: response.status,
    body: body.bytes,
    upstreamBytes: body.totalBytes,
    durationMs: performance.now() - started,
    truncated: body.truncated,
  };
}

async function fetchHttpProxy(request: WorkerExitRequest, maxBytes: number, fetcher: typeof fetch): Promise<UpstreamResult> {
  if (!request.url) {
    throw new Error("http_proxy_v1 requires url");
  }
  const method = normalizedRelayMethod(request.method);
  const url = validatedPublicHttpUrl(request.url);
  const requestBytes = requestBody(request.bodyBase64);
  if (hasUpgradeHeader(request.headers)) {
    throw new Error("http_proxy_v1 does not support WebSocket/Upgrade traffic");
  }
  const started = performance.now();
  const response = await fetcher(url, {
    method,
    headers: filteredRequestHeaders(request.headers),
    body: method === "GET" || method === "HEAD" || !requestBytes ? undefined : arrayBufferForBody(requestBytes),
    redirect: "manual",
  });
  const body = await collectLimited(response.body, maxBytes);
  const durationMs = performance.now() - started;
  const responseBody =
    request.responseFormat === "binary_v1"
      ? encodeBinaryProxyResponse(proxyResponseEnvelope(response, body, durationMs), body.bytes)
      : new TextEncoder().encode(JSON.stringify(proxyResponseEnvelope(response, body, durationMs, body.bytes)));
  return {
    status: response.status,
    body: responseBody,
    upstreamBytes: body.totalBytes,
    durationMs,
    truncated: body.truncated,
  };
}

async function fetchHttpProxyChunks(
  drive: ExitDriveClient,
  request: WorkerExitRequest,
  maxBytes: number,
  options: ExitProcessorOptions,
): Promise<Partial<ExitProcessResult>> {
  if (!request.url) {
    throw new Error("http_proxy_v1 requires url");
  }
  const responseFileIds = request.responseFileIds?.filter(Boolean) ?? [];
  if (responseFileIds.length === 0) {
    throw new Error("http_proxy_v1 chunked response requires responseFileIds");
  }
  const method = normalizedRelayMethod(request.method);
  const url = validatedPublicHttpUrl(request.url);
  const requestBytes = requestBody(request.bodyBase64);
  if (hasUpgradeHeader(request.headers)) {
    throw new Error("http_proxy_v1 does not support WebSocket/Upgrade traffic");
  }

  const started = performance.now();
  const response = await (options.fetcher ?? fetch)(url, {
    method,
    headers: filteredRequestHeaders(request.headers),
    body: method === "GET" || method === "HEAD" || !requestBytes ? undefined : arrayBufferForBody(requestBytes),
    redirect: "manual",
  });
  const chunkBytes = configuredMaxBytes(request.responseChunkBytes, maxBytes);
  let chunkIndex = 0;
  let uploadedBytes = 0;
  let upstreamBytes = 0;
  let responseName = responseObjectChunkName(options.responsePrefix, request.requestId || responseFileIds[0], response.status, 0, 0, true);
  const uploadInFlight: Promise<void>[] = [];
  const maxUploadInFlight = 2;

  const uploadChunk = async (body: Uint8Array<ArrayBufferLike>, done: boolean): Promise<void> => {
    const index = chunkIndex;
    chunkIndex += 1;
    const id = responseFileIds[index];
    if (!id) {
      throw new Error(`http_proxy_v1 response exceeded ${responseFileIds.length} preallocated Drive chunks`);
    }
    const durationMs = performance.now() - started;
    const metadata = {
      ...proxyResponseEnvelope(response, { bytes: body, totalBytes: upstreamBytes, truncated: false }, durationMs),
      chunkIndex: index,
      done,
      more: !done,
      responseChunks: responseFileIds.length,
    };
    responseName = responseObjectChunkName(options.responsePrefix, request.requestId || id, response.status, body.byteLength, durationMs, done);
    const encoded = encodeBinaryProxyResponse(metadata, body);
    const uploaded = await uploadResponseFile(drive, {
      id,
      name: responseName,
      body: arrayBufferForBody(encoded),
      mimeType: "application/octet-stream",
      parents: options.folderId ? [options.folderId] : undefined,
    });
    uploadedBytes += encoded.byteLength;
    responseFileIds[index] = uploaded.id || id;
  };

  const scheduleUploadChunk = async (body: Uint8Array<ArrayBufferLike>, done: boolean): Promise<void> => {
    const upload = uploadChunk(body, done);
    upload.catch(() => undefined);
    uploadInFlight.push(upload);
    if (uploadInFlight.length >= maxUploadInFlight) {
      await uploadInFlight.shift();
    }
  };

  if (!response.body || method === "HEAD") {
    await scheduleUploadChunk(new Uint8Array(), true);
    await Promise.all(uploadInFlight);
    return {
      responseName,
      responseFileId: responseFileIds[0],
      responseBytes: uploadedBytes,
      upstreamBytes,
      upstreamStatus: response.status,
      upstreamMs: performance.now() - started,
      truncated: false,
    };
  }

  const reader = response.body.getReader();
  let pending: Uint8Array<ArrayBufferLike> = new Uint8Array();
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      if (!value?.byteLength) {
        continue;
      }
      upstreamBytes += value.byteLength;
      pending = appendBytes(pending, copyBytes(value));
      while (pending.byteLength >= chunkBytes) {
        await scheduleUploadChunk(pending.slice(0, chunkBytes), false);
        pending = pending.slice(chunkBytes);
      }
    }
  } finally {
    reader.releaseLock();
  }
  await scheduleUploadChunk(pending, true);
  await Promise.all(uploadInFlight);
  return {
    responseName,
    responseFileId: responseFileIds[0],
    responseBytes: uploadedBytes,
    upstreamBytes,
    upstreamStatus: response.status,
    upstreamMs: performance.now() - started,
    truncated: false,
  };
}

function normalizedRelayMethod(method: string | undefined): string {
  const normalized = (method || "GET").toUpperCase();
  if (!["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"].includes(normalized)) {
    throw new Error(`http_proxy_v1 unsupported method: ${normalized}`);
  }
  return normalized;
}

function requestBody(bodyBase64: string | undefined): Uint8Array | undefined {
  if (!bodyBase64) {
    return undefined;
  }
  const binary = atob(bodyBase64);
  if (binary.length > MAX_REQUEST_BODY_BYTES) {
    throw new Error(`http_proxy_v1 request body exceeds ${MAX_REQUEST_BODY_BYTES} byte cap`);
  }
  const body = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) {
    body[index] = binary.charCodeAt(index);
  }
  return body;
}

function hasUpgradeHeader(headers: HeaderBag | undefined): boolean {
  for (const [key, value] of Object.entries(headers ?? {})) {
    const combined = Array.isArray(value) ? value.join(",") : value;
    if (key.toLowerCase() === "upgrade" || combined.toLowerCase() === "websocket") {
      return true;
    }
  }
  return false;
}

async function tcpHttp(request: WorkerExitRequest, maxBytes: number): Promise<UpstreamResult> {
  if (!request.host) {
    throw new Error("tcp_http_v1 requires host");
  }
  if (!isAllowedDestinationHost(request.host)) {
    throw new Error(`blocked private or unsafe tcp_http_v1 host: ${request.host}`);
  }
  const port = request.port ?? (request.secureTransport === "on" ? 443 : 80);
  const path = request.path?.startsWith("/") ? request.path : `/${request.path ?? ""}`;
  const started = performance.now();
  const { connect } = await import("cloudflare:sockets");
  const socket = connect(
    { hostname: request.host, port },
    {
      secureTransport: request.secureTransport ?? (port === 443 ? "on" : "off"),
      allowHalfOpen: true,
    },
  );
  try {
    const writer = socket.writable.getWriter();
    await writer.write(
      new TextEncoder().encode(
        `GET ${path} HTTP/1.1\r\nhost: ${request.host}\r\nconnection: close\r\naccept: */*\r\n\r\n`,
      ),
    );
    await writer.close();
    const body = await collectLimited(socket.readable, maxBytes);
    return {
      body: body.bytes,
      upstreamBytes: body.totalBytes,
      durationMs: performance.now() - started,
      truncated: body.truncated,
    };
  } finally {
    socket.close();
  }
}

function validatedPublicHttpUrl(rawUrl: string): URL {
  const url = new URL(rawUrl);
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    throw new Error("only http and https URLs are supported");
  }
  if (url.username || url.password) {
    throw new Error("URLs with embedded credentials are not supported");
  }
  if (!isAllowedDestinationHost(url.hostname)) {
    throw new Error(`blocked private or unsafe destination host: ${url.hostname}`);
  }
  return url;
}

function isAllowedDestinationHost(hostname: string): boolean {
  const host = hostname.toLowerCase().replace(/^\[|\]$/g, "");
  if (
    !host ||
    host === "localhost" ||
    host.endsWith(".localhost") ||
    host.endsWith(".local") ||
    host.endsWith(".internal")
  ) {
    return false;
  }
  if (host === "metadata.google.internal") {
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
    const [a, b] = parts;
    const c = parts[2];
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
    if (normalized === "::1" || normalized === "::" || normalized.startsWith("fc") || normalized.startsWith("fd")) {
      return false;
    }
    if (normalized.startsWith("fe8") || normalized.startsWith("fe9") || normalized.startsWith("fea") || normalized.startsWith("feb")) {
      return false;
    }
  }
  return true;
}

function filteredRequestHeaders(headers: HeaderBag | undefined): Headers {
  const out = new Headers();
  for (const [key, value] of Object.entries(headers ?? {})) {
    const lower = key.toLowerCase();
    if (
      lower === "accept-encoding" ||
      lower === "connection" ||
      lower === "content-length" ||
      lower === "keep-alive" ||
      lower === "proxy-authenticate" ||
      lower === "proxy-authorization" ||
      lower === "proxy-connection" ||
      lower === "te" ||
      lower === "trailer" ||
      lower === "transfer-encoding" ||
      lower === "upgrade" ||
      lower === "host"
    ) {
      continue;
    }
    const values = Array.isArray(value) ? value : [value];
    for (const item of values) {
      out.append(key, item);
    }
  }
  return out;
}

function serializableResponseHeaders(headers: Headers): HeaderBag {
  const out: HeaderBag = {};
  const setCookies = (headers as Headers & { getSetCookie?: () => string[] }).getSetCookie?.() ?? [];
  for (const [key, value] of headers.entries()) {
    const lower = key.toLowerCase();
    if (
      lower === "connection" ||
      lower === "keep-alive" ||
      lower === "proxy-authenticate" ||
      lower === "proxy-authorization" ||
      lower === "te" ||
      lower === "trailer" ||
      lower === "transfer-encoding" ||
      lower === "upgrade"
    ) {
      continue;
    }
    if (lower === "set-cookie" && setCookies.length > 0) {
      continue;
    }
    out[lower] = value;
  }
  if (setCookies.length > 0) {
    out["set-cookie"] = setCookies;
  }
  return out;
}

function proxyResponseEnvelope(
  response: Response,
  body: { bytes: Uint8Array; totalBytes: number; truncated: boolean },
  durationMs: number,
  bodyBytes?: Uint8Array,
): Record<string, unknown> {
  return {
    ok: true,
    kind: bodyBytes ? "http_proxy_response_v1" : "http_proxy_binary_v1",
    status: response.status,
    statusText: response.statusText,
    headers: serializableResponseHeaders(response.headers),
    ...(bodyBytes ? { bodyBase64: base64Encode(bodyBytes) } : {}),
    upstreamBytes: body.totalBytes,
    bodyBytes: body.bytes.byteLength,
    durationMs,
    truncated: body.truncated,
  };
}

function encodeBinaryProxyResponse(metadata: Record<string, unknown>, body: Uint8Array): Uint8Array {
  const header = new TextEncoder().encode(JSON.stringify(metadata));
  const out = new Uint8Array(BINARY_PROXY_MAGIC.length + 4 + header.byteLength + body.byteLength);
  for (let index = 0; index < BINARY_PROXY_MAGIC.length; index += 1) {
    out[index] = BINARY_PROXY_MAGIC.charCodeAt(index);
  }
  const view = new DataView(out.buffer);
  view.setUint32(BINARY_PROXY_MAGIC.length, header.byteLength);
  out.set(header, BINARY_PROXY_MAGIC.length + 4);
  out.set(body, BINARY_PROXY_MAGIC.length + 4 + header.byteLength);
  return out;
}

function base64Encode(bytes: Uint8Array): string {
  let binary = "";
  const chunkSize = 0x8000;
  for (let offset = 0; offset < bytes.byteLength; offset += chunkSize) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + chunkSize));
  }
  return btoa(binary);
}

async function collectLimited(stream: ReadableStream<Uint8Array> | null, maxBytes: number): Promise<{
  bytes: Uint8Array;
  totalBytes: number;
  truncated: boolean;
}> {
  if (!stream) {
    return { bytes: new Uint8Array(), totalBytes: 0, truncated: false };
  }
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let totalBytes = 0;
  let copiedBytes = 0;
  let truncated = false;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      if (!value) {
        continue;
      }
      totalBytes += value.byteLength;
      const remaining = maxBytes - copiedBytes;
      if (remaining <= 0) {
        truncated = true;
        await reader.cancel("skirk worker response byte cap reached");
        break;
      }
      if (value.byteLength > remaining) {
        chunks.push(value.slice(0, remaining));
        copiedBytes += remaining;
        truncated = true;
        await reader.cancel("skirk worker response byte cap reached");
        break;
      }
      chunks.push(value);
      copiedBytes += value.byteLength;
    }
  } finally {
    reader.releaseLock();
  }
  const bytes = new Uint8Array(copiedBytes);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return { bytes, totalBytes, truncated };
}

function parseRequest(bytes: ArrayBuffer): WorkerExitRequest {
  const decoded = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  const payload = JSON.parse(decoded) as WorkerExitRequest;
  return payload;
}

function requestIdFromCandidate(candidate: CandidateObject, prefix: string): string {
  const trimmed = candidate.name.slice(prefix.length).replace(/^\/+/, "");
  return sanitizeNameSegment(trimmed || candidate.fileId);
}

function responseObjectName(
  responsePrefix = DEFAULT_RESPONSE_PREFIX,
  requestId: string,
  status: string,
  bytes: number,
  durationMs: number,
  truncated: boolean,
): string {
  const prefix = responsePrefix.endsWith("/") ? responsePrefix : `${responsePrefix}/`;
  return `${prefix}${sanitizeNameSegment(requestId)}.status-${sanitizeNameSegment(status)}.bytes-${bytes}.ms-${Math.max(
    0,
    Math.round(durationMs),
  )}.trunc-${truncated ? 1 : 0}`;
}

function responseObjectChunkName(
  responsePrefix = DEFAULT_RESPONSE_PREFIX,
  requestId: string,
  status: number | string,
  bytes: number,
  durationMs: number,
  done: boolean,
): string {
  return `${responseObjectName(responsePrefix, requestId, String(status), bytes, durationMs, false)}.done-${done ? 1 : 0}`;
}

function sanitizeNameSegment(value: string): string {
  return value.replace(/[^A-Za-z0-9._=-]/g, "_").slice(0, 128) || "request";
}

function clampMaxBytes(value: number | undefined, fallback: number): number {
  if (!Number.isFinite(value) || !value || value < 1) {
    return fallback;
  }
  return Math.min(Math.floor(value), MAX_RESPONSE_BYTES_LIMIT);
}

function arrayBufferForBody(bytes: Uint8Array<ArrayBufferLike>): ArrayBuffer {
  const copy = new Uint8Array(bytes.byteLength);
  copy.set(bytes);
  return copy.buffer;
}

function appendBytes(left: Uint8Array<ArrayBufferLike>, right: Uint8Array<ArrayBufferLike>): Uint8Array<ArrayBufferLike> {
  if (left.byteLength === 0) {
    return right;
  }
  const out = new Uint8Array(left.byteLength + right.byteLength);
  out.set(left, 0);
  out.set(right, left.byteLength);
  return out;
}

function copyBytes(bytes: Uint8Array<ArrayBufferLike>): Uint8Array {
  const out = new Uint8Array(bytes.byteLength);
  out.set(bytes);
  return out;
}

async function uploadResponseFile(
  drive: ExitDriveClient,
  input: {
    id?: string;
    name: string;
    body: BlobPart;
    mimeType?: string;
    parents?: string[];
  },
): Promise<{ id?: string; name?: string; size?: string }> {
  try {
    return await drive.uploadFile(input);
  } catch (error) {
    if (!input.parents?.length || !isMissingDriveParent(error)) {
      throw error;
    }
    return drive.uploadFile({
      ...input,
      parents: undefined,
    });
  }
}

function isMissingDriveParent(error: unknown): boolean {
  if (!(error instanceof Error)) {
    return false;
  }
  const message = error.message.toLowerCase();
  return message.includes("status=404") && message.includes("file not found");
}
