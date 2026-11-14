#!/usr/bin/env node

import net from "node:net";
import { readFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";

const DRIVE_URL = "https://www.googleapis.com/drive/v3";
const DRIVE_UPLOAD_URL = "https://www.googleapis.com/upload/drive/v3";
const DEFAULT_UP_PREFIX = "cfstream/v1/up/";
const DEFAULT_DOWN_PREFIX = "cfstream/v1/down/";
const DEFAULT_LIST_PAGE_SIZE = 1000;

const args = parseArgs(process.argv.slice(2));
const config = await loadConfig(args);
const configuredFolderId = args.folderId ?? process.env.SKIRK_DRIVE_FOLDER_ID ?? config.drive?.folder_id ?? config.folder_id;
const listen = parseListen(args.listen ?? process.env.SKIRK_SOCKS_LISTEN ?? "127.0.0.1:18180");
const upPrefix = args.upPrefix ?? process.env.SKIRK_STREAM_UP_PREFIX ?? DEFAULT_UP_PREFIX;
const downPrefix = args.downPrefix ?? process.env.SKIRK_STREAM_DOWN_PREFIX ?? DEFAULT_DOWN_PREFIX;
const pollMs = integerOption(args.pollMs ?? process.env.SKIRK_STREAM_CLIENT_POLL_MS, 350);
const connectTimeoutMs = integerOption(args.connectTimeoutMs ?? process.env.SKIRK_STREAM_CONNECT_TIMEOUT_MS, 45_000);
const chunkBytes = integerOption(args.chunkBytes ?? process.env.SKIRK_STREAM_CLIENT_CHUNK_BYTES, 32 * 1024);
const initialDataWaitMs = integerOption(args.initialDataWaitMs ?? process.env.SKIRK_STREAM_INITIAL_DATA_WAIT_MS, 2_500);
const optimisticWebOpen = booleanOption(args.optimisticWebOpen ?? process.env.SKIRK_STREAM_OPTIMISTIC_WEB_OPEN, true);
const optimisticAll = booleanOption(args.optimisticAll ?? process.env.SKIRK_STREAM_OPTIMISTIC_ALL, false);
const optimisticPorts = parsePortSet(args.optimisticPorts ?? process.env.SKIRK_STREAM_OPTIMISTIC_PORTS ?? "80,443");
const allowWebPorts = booleanOption(args.allowWebPorts ?? process.env.SKIRK_STREAM_ALLOW_WEB_PORTS, false);
const maxPendingUploadBytes = integerOption(
  args.maxPendingUploadBytes ?? process.env.SKIRK_STREAM_CLIENT_MAX_PENDING_UPLOAD_BYTES,
  2 * 1024 * 1024,
);
const workerUrl = args.workerUrl ?? process.env.WORKER_URL;
const adminToken = args.adminToken ?? process.env.SKIRK_ADMIN_TOKEN;
const sessionId = sanitizeName(args.session ?? process.env.SKIRK_STREAM_SESSION ?? `s${Date.now().toString(36)}`);

const stats = {
  startedAt: new Date().toISOString(),
  connections: 0,
  successfulConnects: 0,
  failedConnects: 0,
  uploads: 0,
  uploadBytes: 0,
  downloads: 0,
  downloadBytes: 0,
  lists: 0,
  deletes: 0,
  workerPumpCalls: 0,
  workerPumpErrors: 0,
};

let drive;
let shuttingDown = false;
let pumpInFlight;
let pumpAgain = false;

async function handleSocks(socket) {
  stats.connections += 1;
  socket.setNoDelay(true);
  const reader = new SocketReader(socket);
  await negotiateSocks(reader, socket);
  const request = await readConnectRequest(reader);
  const streamId = sanitizeName(randomUUID().replaceAll("-", "").slice(0, 16));
  let upSeq = 0;
  let downSeq = 2;
  let closed = false;
  let uploadChain = Promise.resolve();
  let queuedUploadBytes = 0;
  let uploadPaused = false;
  const resumePendingUploadBytes = Math.max(1, Math.floor(maxPendingUploadBytes / 2));
  const streamPrefix = `${sessionId}/${streamId}/`;
  const shouldUseOptimisticOpen = optimisticWebOpen && (optimisticAll || optimisticPorts.has(request.port));
  if (!allowWebPorts && isNormalWebPort(request.port)) {
    writeSocksReply(socket, 0x02);
    throw new Error(
      `normal web ports are disabled in this lab SOCKS helper: ${request.host}:${request.port}. Use the HTTP fetch relay for HTTP, inspected HTTPS mode for HTTPS, or pass --allow-web-ports true to reproduce the Cloudflare TCP boundary.`,
    );
  }

  let initialData = Buffer.alloc(0);
  if (shouldUseOptimisticOpen) {
    await writeAndDrain(socket, socksReply(0x00));
    initialData = await reader.readAvailableWithin(initialDataWaitMs, chunkBytes);
  }

  await uploadJson(`${upPrefix}${streamPrefix}${padSeq(upSeq++)}.open`, {
    type: "open",
    host: request.host,
    port: request.port,
    ...(initialData.byteLength > 0 ? { initialData: initialData.toString("base64") } : {}),
  });
  wakeWorker();

  const connectResult = await waitForDownObject(streamPrefix, 1, connectTimeoutMs);
  if (!connectResult || connectResult.kind === "err") {
    stats.failedConnects += 1;
    const message = connectResult?.body?.toString("utf8") || "connect timeout";
    if (!shouldUseOptimisticOpen) {
      writeSocksReply(socket, isCloudflareWebTcpRejection(message, request.port) ? 0x02 : 0x01);
    }
    throw new Error(`worker upstream connect failed: ${formatConnectFailure(message, request)}`);
  }
  if (connectResult.kind !== "opened") {
    stats.failedConnects += 1;
    if (!shouldUseOptimisticOpen) {
      writeSocksReply(socket, 0x01);
    }
    throw new Error(`worker returned unexpected connect object kind=${connectResult.kind}`);
  }

  stats.successfulConnects += 1;
  if (!shouldUseOptimisticOpen) {
    writeSocksReply(socket, 0x00);
  }

  reader.stop();
  const leftover = reader.takeBuffered();
  if (leftover.byteLength > 0) {
    enqueueData(leftover);
  }

  socket.on("data", (chunk) => {
    for (let offset = 0; offset < chunk.byteLength; offset += chunkBytes) {
      enqueueData(chunk.subarray(offset, Math.min(chunk.byteLength, offset + chunkBytes)));
    }
  });
  socket.on("end", () => {
    enqueueFin();
  });
  socket.on("close", () => {
    closed = true;
  });
  socket.on("error", () => {
    closed = true;
  });

  while (!closed && !shuttingDown) {
    const files = await drive.listByNameContains(`${downPrefix}${streamPrefix}`);
    const ordered = files
      .map((file) => ({ file, parsed: parseStreamLeaf(file.name ?? "") }))
      .filter((item) => item.parsed)
      .sort((a, b) => a.parsed.seq - b.parsed.seq);
    let progressed = false;
    for (const item of ordered) {
      if (item.parsed.seq !== downSeq || !item.file.id) {
        continue;
      }
      let streamError;
      if (item.parsed.kind === "data") {
        const body = Buffer.from(await drive.download(item.file.id));
        if (body.byteLength > 0 && !socket.destroyed) {
          await writeAndDrain(socket, body);
        }
      } else if (item.parsed.kind === "fin") {
        closed = true;
      } else if (item.parsed.kind === "err") {
        const body = Buffer.from(await drive.download(item.file.id));
        closed = true;
        streamError = `worker stream error: ${body.toString("utf8")}`;
      }
      await drive.delete(item.file.id);
      if (streamError) {
        throw new Error(streamError);
      }
      downSeq += 1;
      progressed = true;
    }
    if (closed) {
      break;
    }
    await sleep(progressed ? 10 : pollMs);
  }

  await uploadChain.catch(() => {});
  socket.end();

  function enqueueData(chunk) {
    if (closed || chunk.byteLength === 0) {
      return;
    }
    const name = `${upPrefix}${streamPrefix}${padSeq(upSeq++)}.data`;
    queuedUploadBytes += chunk.byteLength;
    maybePauseUploads();
    uploadChain = uploadChain
      .then(() =>
        uploadJson(name, {
          type: "data",
          data: Buffer.from(chunk).toString("base64"),
        }),
      )
      .then(() => {
        queuedUploadBytes -= chunk.byteLength;
        maybeResumeUploads();
        wakeWorker();
      })
      .catch((error) => {
        closed = true;
        socket.destroy(error);
      });
  }

  function enqueueFin() {
    if (closed) {
      return;
    }
    const name = `${upPrefix}${streamPrefix}${padSeq(upSeq++)}.fin`;
    uploadChain = uploadChain
      .then(() => uploadJson(name, { type: "fin" }))
      .then(() => wakeWorker())
      .catch((error) => {
        closed = true;
        socket.destroy(error);
      });
  }

  function maybePauseUploads() {
    if (!uploadPaused && queuedUploadBytes >= maxPendingUploadBytes) {
      uploadPaused = true;
      socket.pause();
    }
  }

  function maybeResumeUploads() {
    if (uploadPaused && queuedUploadBytes <= resumePendingUploadBytes && !socket.destroyed) {
      uploadPaused = false;
      socket.resume();
    }
  }
}

async function waitForDownObject(streamPrefix, seq, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    wakeWorker();
    const files = await drive.listByNameContains(`${downPrefix}${streamPrefix}`);
    const ordered = files
      .map((file) => ({ file, parsed: parseStreamLeaf(file.name ?? "") }))
      .filter((item) => item.parsed)
      .sort((a, b) => a.parsed.seq - b.parsed.seq);
    for (const item of ordered) {
      if (item.parsed.seq !== seq || !item.file.id) {
        continue;
      }
      const body = item.parsed.kind === "opened" || item.parsed.kind === "fin" ? Buffer.alloc(0) : Buffer.from(await drive.download(item.file.id));
      await drive.delete(item.file.id);
      return { kind: item.parsed.kind, body };
    }
    await sleep(pollMs);
  }
  return undefined;
}

async function negotiateSocks(reader, socket) {
  const greeting = await reader.readAtLeast(2);
  if (greeting[0] !== 0x05) {
    throw new Error("only SOCKS5 is supported");
  }
  const methods = await reader.readAtLeast(greeting[1]);
  if (!methods.includes(0x00)) {
    socket.write(Buffer.from([0x05, 0xff]));
    throw new Error("SOCKS client did not offer no-auth method");
  }
  socket.write(Buffer.from([0x05, 0x00]));
}

async function readConnectRequest(reader) {
  const header = await reader.readAtLeast(4);
  if (header[0] !== 0x05 || header[1] !== 0x01) {
    throw new Error("only SOCKS5 CONNECT is supported");
  }
  let host;
  if (header[3] === 0x01) {
    host = Array.from(await reader.readAtLeast(4)).join(".");
  } else if (header[3] === 0x03) {
    const length = (await reader.readAtLeast(1))[0];
    host = (await reader.readAtLeast(length)).toString("utf8");
  } else if (header[3] === 0x04) {
    const bytes = await reader.readAtLeast(16);
    host = Array.from({ length: 8 }, (_, index) => bytes.readUInt16BE(index * 2).toString(16)).join(":");
  } else {
    throw new Error(`unsupported SOCKS address type ${header[3]}`);
  }
  const portBytes = await reader.readAtLeast(2);
  return { host, port: portBytes.readUInt16BE(0) };
}

class SocketReader {
  chunks = [];
  waiters = [];

  constructor(socket) {
    this.socket = socket;
    this.onData = (chunk) => {
      this.chunks.push(chunk);
      this.wake();
    };
    socket.on("data", this.onData);
  }

  async readAtLeast(size) {
    while (this.bufferedBytes() < size) {
      await new Promise((resolve) => this.waiters.push(resolve));
    }
    return this.read(size);
  }

  takeBuffered() {
    const size = this.bufferedBytes();
    return size ? this.read(size) : Buffer.alloc(0);
  }

  async readAvailableWithin(timeoutMs, maxBytes) {
    const deadline = Date.now() + timeoutMs;
    while (this.bufferedBytes() === 0) {
      const remaining = deadline - Date.now();
      if (remaining <= 0) {
        return Buffer.alloc(0);
      }
      await Promise.race([new Promise((resolve) => this.waiters.push(resolve)), sleep(remaining)]);
    }
    return this.read(Math.min(this.bufferedBytes(), maxBytes));
  }

  read(size) {
    const out = Buffer.alloc(size);
    let offset = 0;
    while (offset < size) {
      const chunk = this.chunks[0];
      const take = Math.min(chunk.byteLength, size - offset);
      chunk.copy(out, offset, 0, take);
      offset += take;
      if (take === chunk.byteLength) {
        this.chunks.shift();
      } else {
        this.chunks[0] = chunk.subarray(take);
      }
    }
    return out;
  }

  bufferedBytes() {
    return this.chunks.reduce((total, chunk) => total + chunk.byteLength, 0);
  }

  wake() {
    const waiters = this.waiters.splice(0);
    for (const waiter of waiters) {
      waiter();
    }
  }

  stop() {
    this.socket.off("data", this.onData);
  }
}

async function uploadJson(name, payload) {
  await drive.upload({
    name,
    mimeType: "application/json",
    body: JSON.stringify(payload),
  });
}

function wakeWorker() {
  if (!workerUrl || !adminToken) {
    return Promise.resolve(false);
  }
  if (pumpInFlight) {
    pumpAgain = true;
    return pumpInFlight;
  }
  pumpInFlight = (async () => {
    let ok = true;
    do {
      pumpAgain = false;
      stats.workerPumpCalls += 1;
      try {
        const response = await fetch(new URL("/admin/pump", workerUrl), {
          method: "POST",
          headers: { authorization: `Bearer ${adminToken}` },
        });
        if (!response.ok) {
          ok = false;
          stats.workerPumpErrors += 1;
        }
      } catch {
        ok = false;
        stats.workerPumpErrors += 1;
      }
    } while (pumpAgain && !shuttingDown);
    return ok;
  })().finally(() => {
    pumpInFlight = undefined;
  });
  return pumpInFlight;
}

class DriveApi {
  accessToken = "";
  expiresAt = 0;

  constructor(config, folderId) {
    this.config = config;
    this.folderId = folderId;
    this.warnedFolderFallback = false;
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
    const activeFolderId = this.folderId;
    try {
      return await this.uploadOnce(input, activeFolderId);
    } catch (error) {
      if (!activeFolderId || !isMissingDriveParent(error)) {
        throw error;
      }
      this.disableFolderFallback(activeFolderId);
      return this.uploadOnce(input, undefined);
    }
  }

  async uploadOnce(input, activeFolderId) {
    const boundary = `skirk-socks-${randomUUID()}`;
    const metadata = {
      name: input.name,
      mimeType: input.mimeType,
      ...(activeFolderId ? { parents: [activeFolderId] } : {}),
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
      throw new Error(`drive upload failed: status=${response.status} body=${await response.text()}`);
    }
    const payload = await response.json();
    stats.uploads += 1;
    stats.uploadBytes += Buffer.byteLength(String(input.body));
    return payload;
  }

  async listByNameContains(contains) {
    try {
      return await this.listByNameContainsOnce(contains, this.folderId);
    } catch (error) {
      if (!this.folderId || !isMissingDriveParent(error)) {
        throw error;
      }
      this.disableFolderFallback(this.folderId);
      return this.listByNameContainsOnce(contains, undefined);
    }
  }

  async listByNameContainsOnce(contains, activeFolderId) {
    const files = [];
    let pageToken;
    do {
      const url = new URL("/drive/v3/files", DRIVE_URL);
      const q = [`trashed = false`, `name contains '${escapeDriveQuery(contains)}'`];
      if (activeFolderId) {
        q.push(`'${escapeDriveQuery(activeFolderId)}' in parents`);
      }
      url.searchParams.set("q", q.join(" and "));
      url.searchParams.set("pageSize", String(DEFAULT_LIST_PAGE_SIZE));
      url.searchParams.set("fields", "nextPageToken,incompleteSearch,files(id,name,size,createdTime)");
      if (pageToken) {
        url.searchParams.set("pageToken", pageToken);
      }
      const response = await fetch(url, { headers: { authorization: `Bearer ${await this.refresh()}` } });
      if (!response.ok) {
        throw new Error(`drive list failed: status=${response.status} body=${await response.text()}`);
      }
      const payload = await response.json();
      stats.lists += 1;
      files.push(...(payload.files ?? []));
      pageToken = payload.nextPageToken;
    } while (pageToken);
    return files;
  }

  async download(fileId) {
    const response = await fetch(`${DRIVE_URL}/files/${encodeURIComponent(fileId)}?alt=media`, {
      headers: { authorization: `Bearer ${await this.refresh()}` },
    });
    if (!response.ok) {
      throw new Error(`drive download failed: ${response.status} ${await response.text()}`);
    }
    const body = new Uint8Array(await response.arrayBuffer());
    stats.downloads += 1;
    stats.downloadBytes += body.byteLength;
    return body;
  }

  async delete(fileId) {
    const response = await fetch(`${DRIVE_URL}/files/${encodeURIComponent(fileId)}`, {
      method: "DELETE",
      headers: { authorization: `Bearer ${await this.refresh()}` },
    });
    if (!response.ok && response.status !== 404) {
      throw new Error(`drive delete failed: ${response.status} ${await response.text()}`);
    }
    stats.deletes += 1;
  }

  disableFolderFallback(folderId) {
    this.folderId = undefined;
    if (!this.warnedFolderFallback) {
      this.warnedFolderFallback = true;
      console.warn(`Drive folder ${folderId} is unavailable; falling back to Drive root for this run`);
    }
  }
}

function parseStreamLeaf(name) {
  const leaf = name.split("/").pop() ?? "";
  const match = /^(\d+)\.([A-Za-z0-9_-]+)$/.exec(leaf);
  if (!match) {
    return undefined;
  }
  return { seq: Number.parseInt(match[1], 10), kind: match[2] };
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
    } else if (argv[index + 1] && !argv[index + 1].startsWith("--")) {
      parsed[key] = argv[++index];
    } else {
      parsed[key] = "true";
    }
  }
  return parsed;
}

function formatConnectFailure(message, request) {
  if (!isCloudflareWebTcpRejection(message, request.port)) {
    return message;
  }
  return `${message} (Cloudflare Workers rejects raw TCP sockets to normal web ports ${request.host}:${request.port}; use the HTTP fetch relay or explicit local inspection mode for HTTP(S) testing.)`;
}

function isNormalWebPort(port) {
  return port === 80 || port === 443;
}

function isCloudflareWebTcpRejection(message, port) {
  return (
    isNormalWebPort(port) &&
    message.toLowerCase().includes("trying to connect to a http-based service") &&
    message.toLowerCase().includes("consider using fetch")
  );
}

function parseListen(value) {
  const index = value.lastIndexOf(":");
  if (index < 0) {
    return { host: "127.0.0.1", port: Number.parseInt(value, 10) };
  }
  return { host: value.slice(0, index), port: Number.parseInt(value.slice(index + 1), 10) };
}

function integerOption(value, fallback) {
  const parsed = Number.parseInt(String(value ?? ""), 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

function booleanOption(value, fallback) {
  if (value === undefined || value === null || value === "") {
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

function parsePortSet(value) {
  const ports = new Set();
  for (const part of String(value ?? "").split(",")) {
    const port = Number.parseInt(part.trim(), 10);
    if (Number.isFinite(port) && port > 0 && port <= 65535) {
      ports.add(port);
    }
  }
  return ports;
}

function padSeq(seq) {
  return String(seq).padStart(12, "0");
}

function sanitizeName(value) {
  return value.replace(/[^A-Za-z0-9._=-]/g, "_");
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
  if (!(error instanceof Error)) {
    return false;
  }
  const message = error.message.toLowerCase();
  return message.includes("file not found") || message.includes("notfound") || message.includes("not found");
}

function writeSocksReply(socket, code) {
  socket.write(socksReply(code));
}

function socksReply(code) {
  return Buffer.from([0x05, code, 0x00, 0x01, 0, 0, 0, 0, 0, 0]);
}

function writeAndDrain(socket, body) {
  return new Promise((resolve, reject) => {
    if (socket.destroyed) {
      resolve();
      return;
    }
    const onError = (error) => {
      cleanup();
      reject(error);
    };
    const onDrain = () => {
      cleanup();
      resolve();
    };
    const cleanup = () => {
      socket.off("error", onError);
      socket.off("drain", onDrain);
    };
    socket.once("error", onError);
    if (socket.write(body)) {
      cleanup();
      resolve();
      return;
    }
    socket.once("drain", onDrain);
  });
}

function quotaEstimate() {
  return {
    uploads: stats.uploads * 50,
    downloads: stats.downloads * 200,
    lists: stats.lists * 100,
    deletes: stats.deletes * 50,
    workerPumps: stats.workerPumpCalls * 100,
    total:
      stats.uploads * 50 +
      stats.downloads * 200 +
      stats.lists * 100 +
      stats.deletes * 50 +
      stats.workerPumpCalls * 100,
  };
}

function printStats() {
  console.log(
    JSON.stringify(
      {
        ...stats,
        finishedAt: new Date().toISOString(),
        estimatedDriveUnits: quotaEstimate(),
      },
      null,
      2,
    ),
  );
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

drive = new DriveApi(config, configuredFolderId);
await drive.refresh();
await wakeWorker();

const server = net.createServer((socket) => {
  void handleSocks(socket).catch((error) => {
    console.error(`socks connection failed: ${error instanceof Error ? error.message : String(error)}`);
    socket.destroy();
  });
});

server.listen(listen.port, listen.host, () => {
  console.log(`Skirk Worker Drive SOCKS listening on ${listen.host}:${listen.port} session=${sessionId}`);
});

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => {
    shuttingDown = true;
    server.close(() => {
      printStats();
      process.exit(0);
    });
    setTimeout(() => {
      printStats();
      process.exit(0);
    }, 1000).unref();
  });
}
