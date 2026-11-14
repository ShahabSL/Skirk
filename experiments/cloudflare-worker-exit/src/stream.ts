import type { CandidateObject, ExitProcessResult } from "./types";

const DEFAULT_STREAM_UP_PREFIX = "cfstream/v1/up/";
const DEFAULT_STREAM_DOWN_PREFIX = "cfstream/v1/down/";
const DEFAULT_IDLE_TIMEOUT_MS = 120_000;
const DEFAULT_READ_WAIT_MS = 1000;
const DEFAULT_UP_POLL_MS = 1000;
const DEFAULT_WRITE_TIMEOUT_MS = 20_000;
const MAX_UP_CHANGES_PER_TICK = 1000;
const MAX_DOWN_CHUNK_BYTES = 96 * 1024;
const MAX_STREAM_SECONDS = 14 * 60;

export interface StreamDriveClient {
  downloadFile(fileId: string): Promise<ArrayBuffer>;
  uploadFile(input: {
    name: string;
    body: BlobPart;
    mimeType?: string;
    parents?: string[];
  }): Promise<{ id?: string; name?: string; size?: string }>;
  deleteFile(fileId: string): Promise<void>;
  listFilesByNameContains(input: {
    contains: string;
    parentId?: string;
    pageSize?: number;
  }): Promise<Array<{ id?: string; name?: string; size?: string; createdTime?: string; modifiedTime?: string }>>;
}

export interface StreamProcessorOptions {
  upPrefix?: string;
  downPrefix?: string;
  folderId?: string;
  idleTimeoutMs?: number;
  readWaitMs?: number;
  upPollMs?: number;
  scanDuringStream?: boolean;
  writeTimeoutMs?: number;
}

interface ActiveDriveStream {
  parsed: StreamObjectName;
  socket: Socket;
  reader: ReadableStreamDefaultReader<Uint8Array>;
  writer: WritableStreamDefaultWriter<Uint8Array>;
  nextUpSeq: number;
  nextDownSeq: number;
  startedAt: number;
  upstreamBytes: number;
  downstreamBytes: number;
  closed: boolean;
}

interface StreamOpenMessage {
  type: "open";
  host: string;
  port: number;
  initialData?: string;
}

interface StreamDataMessage {
  type: "data";
  data: string;
}

interface StreamFinMessage {
  type: "fin";
}

type StreamUpMessage = StreamOpenMessage | StreamDataMessage | StreamFinMessage;

interface StreamObjectName {
  session: string;
  stream: string;
  seq: number;
  kind: string;
}

export class DriveStreamRuntime {
  private readonly streams = new Map<string, ActiveDriveStream>();

  constructor(
    private readonly drive: StreamDriveClient,
    private readonly options: StreamProcessorOptions,
    private readonly waitUntil: (promise: Promise<unknown>) => void = () => undefined,
  ) {}

  async processCandidate(candidate: CandidateObject): Promise<ExitProcessResult> {
    const upPrefix = this.options.upPrefix ?? DEFAULT_STREAM_UP_PREFIX;
    const parsed = parseUpName(candidate.name, upPrefix);
    if (!parsed) {
      return { processed: false, requestFileId: candidate.fileId, requestName: candidate.name };
    }
    if (parsed.kind === "open") {
      return this.openStream(candidate, parsed);
    }
    const active = this.streams.get(streamKey(parsed));
    if (!active) {
      return { processed: false, requestFileId: candidate.fileId, requestName: candidate.name };
    }
    return this.processUpCandidate(active, candidate, parsed);
  }

  private async openStream(candidate: CandidateObject, parsed: StreamObjectName): Promise<ExitProcessResult> {
    const started = performance.now();
    let opened: StreamOpenMessage;
    try {
      opened = JSON.parse(new TextDecoder().decode(await this.drive.downloadFile(candidate.fileId))) as StreamOpenMessage;
      if (opened.type !== "open") {
        throw new Error("stream open object body must have type=open");
      }
      if (!opened.host || !Number.isFinite(opened.port) || opened.port < 1 || opened.port > 65535) {
        throw new Error("stream open object has invalid host or port");
      }
      if (!isAllowedDestinationHost(opened.host)) {
        throw new Error(`blocked private or unsafe destination host: ${opened.host}`);
      }
    } catch (error) {
      await this.drive.deleteFile(candidate.fileId).catch(() => undefined);
      await uploadDownObject(
        this.drive,
        parsed,
        1,
        "err",
        new TextEncoder().encode(error instanceof Error ? error.message : String(error)),
        this.options,
      ).catch(() => undefined);
      return streamResult(candidate, parsed, started, 0, 0, true, error);
    }

    let socket: Socket | undefined;
    let writer: WritableStreamDefaultWriter<Uint8Array> | undefined;
    try {
      const { connect } = await import("cloudflare:sockets");
      socket = connect({ hostname: opened.host, port: opened.port }, { secureTransport: "off", allowHalfOpen: true });
      writer = socket.writable.getWriter();
      await socket.opened;
      if (opened.initialData) {
        await writeWithTimeout(writer, base64ToBytes(opened.initialData), configuredWriteTimeoutMs(this.options));
      }
    } catch (error) {
      writer?.releaseLock();
      await socket?.close().catch(() => undefined);
      await this.drive.deleteFile(candidate.fileId).catch(() => undefined);
      await uploadDownObject(
        this.drive,
        parsed,
        1,
        "err",
        new TextEncoder().encode(error instanceof Error ? error.message : String(error)),
        this.options,
      ).catch(() => undefined);
      return streamResult(candidate, parsed, started, 0, 0, true, error);
    }
    if (!socket || !writer) {
      throw new Error("stream socket was not initialized");
    }

    const active: ActiveDriveStream = {
      parsed,
      socket,
      reader: socket.readable.getReader(),
      writer,
      nextUpSeq: parsed.seq + 1,
      nextDownSeq: 2,
      startedAt: started,
      upstreamBytes: opened.initialData ? base64ToBytes(opened.initialData).byteLength : 0,
      downstreamBytes: 0,
      closed: false,
    };
    this.streams.set(streamKey(parsed), active);

    try {
      await this.drive.deleteFile(candidate.fileId);
      await uploadDownObject(this.drive, parsed, 1, "opened", new Uint8Array(), this.options);
      this.waitUntil(this.readLoop(active));
      return streamResult(candidate, parsed, started, active.downstreamBytes, active.upstreamBytes, true);
    } catch (error) {
      this.closeActive(active).catch(() => undefined);
      return streamResult(candidate, parsed, started, active.downstreamBytes, active.upstreamBytes, true, error);
    }
  }

  private async processUpCandidate(
    active: ActiveDriveStream,
    candidate: CandidateObject,
    parsed: StreamObjectName,
  ): Promise<ExitProcessResult> {
    if (active.closed) {
      return { processed: false, requestFileId: candidate.fileId, requestName: candidate.name };
    }
    if (parsed.seq < active.nextUpSeq) {
      await this.drive.deleteFile(candidate.fileId).catch(() => undefined);
      return streamResult(candidate, active.parsed, active.startedAt, active.downstreamBytes, active.upstreamBytes, true);
    }
    if (parsed.seq > active.nextUpSeq) {
      return { processed: false, requestFileId: candidate.fileId, requestName: candidate.name };
    }
    try {
      await this.processOneUpObject(active, candidate.fileId, parsed.kind);
      await this.processContiguousUpObjects(active);
      return streamResult(candidate, active.parsed, active.startedAt, active.downstreamBytes, active.upstreamBytes, true);
    } catch (error) {
      await this.uploadStreamError(active, error);
      await this.closeActive(active).catch(() => undefined);
      return streamResult(candidate, active.parsed, active.startedAt, active.downstreamBytes, active.upstreamBytes, true, error);
    }
  }

  private async processContiguousUpObjects(active: ActiveDriveStream): Promise<void> {
    while (!active.closed) {
      const files = await this.drive.listFilesByNameContains({
        contains: upObjectPrefix(active.parsed, this.options),
        parentId: this.options.folderId,
        pageSize: MAX_UP_CHANGES_PER_TICK,
      });
      const next = files
        .map((file) => ({ file, parsed: parseUpName(file.name ?? "", this.options.upPrefix ?? DEFAULT_STREAM_UP_PREFIX) }))
        .find((item) => item.file.id && item.parsed?.seq === active.nextUpSeq);
      if (!next?.file.id || !next.parsed) {
        return;
      }
      await this.processOneUpObject(active, next.file.id, next.parsed.kind);
    }
  }

  private async processOneUpObject(active: ActiveDriveStream, fileId: string, kind: string): Promise<void> {
    try {
      if (kind === "data") {
        const payload = JSON.parse(new TextDecoder().decode(await this.drive.downloadFile(fileId))) as StreamDataMessage;
        if (payload.type !== "data") {
          throw new Error(`stream data object body has type=${(payload as { type?: string }).type ?? "unknown"}`);
        }
        const bytes = base64ToBytes(payload.data);
        await writeWithTimeout(active.writer, bytes, configuredWriteTimeoutMs(this.options));
        active.upstreamBytes += bytes.byteLength;
      } else if (kind === "fin") {
        await active.writer.close().catch(() => undefined);
        active.closed = true;
      } else {
        throw new Error(`unsupported stream object kind=${kind}`);
      }
    } finally {
      await this.drive.deleteFile(fileId).catch(() => undefined);
      active.nextUpSeq += 1;
    }
  }

  private async readLoop(active: ActiveDriveStream): Promise<void> {
    try {
      while (!active.closed && performance.now() - active.startedAt < MAX_STREAM_SECONDS * 1000) {
        const read = await active.reader.read();
        if (read.done) {
          active.closed = true;
          await uploadDownObject(this.drive, active.parsed, active.nextDownSeq++, "fin", new Uint8Array(), this.options);
          return;
        }
        if (read.value?.byteLength) {
          for (const chunk of splitChunks(read.value, MAX_DOWN_CHUNK_BYTES)) {
            await uploadDownObject(this.drive, active.parsed, active.nextDownSeq++, "data", chunk, this.options);
            active.downstreamBytes += chunk.byteLength;
          }
        }
      }
    } catch (error) {
      await this.uploadStreamError(active, error);
    } finally {
      await this.closeActive(active).catch(() => undefined);
    }
  }

  private async uploadStreamError(active: ActiveDriveStream, error: unknown): Promise<void> {
    const message = error instanceof Error ? error.message : String(error);
    await uploadDownObject(this.drive, active.parsed, active.nextDownSeq++, "err", new TextEncoder().encode(message), this.options).catch(
      () => undefined,
    );
  }

  private async closeActive(active: ActiveDriveStream): Promise<void> {
    if (!active.closed) {
      active.closed = true;
    }
    this.streams.delete(streamKey(active.parsed));
    await active.reader.cancel().catch(() => undefined);
    active.reader.releaseLock();
    active.writer.releaseLock();
    await active.socket.close().catch(() => undefined);
  }
}

export async function processStreamCandidate(
  drive: StreamDriveClient,
  candidate: CandidateObject,
  options: StreamProcessorOptions = {},
): Promise<ExitProcessResult> {
  const upPrefix = options.upPrefix ?? DEFAULT_STREAM_UP_PREFIX;
  const parsed = parseUpName(candidate.name, upPrefix);
  if (!parsed || parsed.kind !== "open") {
    return { processed: false, requestFileId: candidate.fileId, requestName: candidate.name };
  }
  const started = performance.now();
  let opened: StreamOpenMessage;
  try {
    opened = JSON.parse(new TextDecoder().decode(await drive.downloadFile(candidate.fileId))) as StreamOpenMessage;
    if (opened.type !== "open") {
      throw new Error("stream open object body must have type=open");
    }
    if (!opened.host || !Number.isFinite(opened.port) || opened.port < 1 || opened.port > 65535) {
      throw new Error("stream open object has invalid host or port");
    }
    if (!isAllowedDestinationHost(opened.host)) {
      throw new Error(`blocked private or unsafe destination host: ${opened.host}`);
    }
  } catch (error) {
    await drive.deleteFile(candidate.fileId).catch(() => undefined);
    await uploadDownObject(
      drive,
      parsed,
      1,
      "err",
      new TextEncoder().encode(error instanceof Error ? error.message : String(error)),
      options,
    ).catch(() => undefined);
    return {
      processed: true,
      requestFileId: candidate.fileId,
      requestName: candidate.name,
      responseName: downObjectPrefix(parsed, options),
      responseBytes: 0,
      upstreamBytes: 0,
      upstreamMs: Math.round(performance.now() - started),
      deletedRequest: true,
      error: error instanceof Error ? error.message : String(error),
    };
  }
  traceStream("open", parsed, {
    host: opened.host,
    port: opened.port,
    initialBytes: opened.initialData ? base64ToBytes(opened.initialData).byteLength : 0,
  });

  let socket: Socket | undefined;
  let writer: WritableStreamDefaultWriter<Uint8Array> | undefined;
  try {
    const { connect } = await import("cloudflare:sockets");
    socket = connect({ hostname: opened.host, port: opened.port }, { secureTransport: "off", allowHalfOpen: true });
    writer = socket.writable.getWriter();
    await socket.opened;
    if (opened.initialData) {
      await writer.write(base64ToBytes(opened.initialData));
    }
    traceStream("connected", parsed, { host: opened.host, port: opened.port });
  } catch (error) {
    writer?.releaseLock();
    await socket?.close().catch(() => undefined);
    await drive.deleteFile(candidate.fileId).catch(() => undefined);
    await uploadDownObject(
      drive,
      parsed,
      1,
      "err",
      new TextEncoder().encode(error instanceof Error ? error.message : String(error)),
      options,
    );
    return {
      processed: true,
      requestFileId: candidate.fileId,
      requestName: candidate.name,
      responseName: downObjectPrefix(parsed, options),
      responseBytes: 0,
      upstreamBytes: 0,
      upstreamMs: Math.round(performance.now() - started),
      deletedRequest: true,
      error: error instanceof Error ? error.message : String(error),
    };
  }
  if (!socket) {
    throw new Error("stream socket was not initialized");
  }
  const reader = socket.readable.getReader();
  if (!writer) {
    writer = socket.writable.getWriter();
  }
  const state = {
    nextUpSeq: parsed.seq + 1,
    nextDownSeq: 2,
    upstreamClosed: false,
    downstreamClosed: false,
    lastActivity: performance.now(),
    downstreamBytes: 0,
    upstreamBytes: opened.initialData ? base64ToBytes(opened.initialData).byteLength : 0,
    uploadedObjects: 0,
    processedUpObjects: 1,
    nextUpPollAt: performance.now(),
  };
  let pendingRead: Promise<ReadableStreamReadResult<Uint8Array>> | undefined;

  try {
    await drive.deleteFile(candidate.fileId);
    await uploadDownObject(drive, parsed, 1, "opened", new Uint8Array(), options);
    try {
      while (
        !state.downstreamClosed &&
        performance.now() - state.lastActivity < configuredIdleTimeoutMs(options) &&
        performance.now() - started < MAX_STREAM_SECONDS * 1000
      ) {
        const now = performance.now();
        if (now >= state.nextUpPollAt) {
          await processUpObjects(drive, parsed, state, writer, options);
          state.nextUpPollAt = performance.now() + configuredUpPollMs(options);
        }
        if (options.scanDuringStream === true) {
          await processNewerOpenObjects(drive, parsed, options);
        }
        if (!pendingRead) {
          pendingRead = reader.read();
        }
        const read = await withTimeout(pendingRead, configuredReadWaitMs(options));
        if (read !== undefined) {
          pendingRead = undefined;
          if (read.done) {
            state.downstreamClosed = true;
            await uploadDownObject(drive, parsed, state.nextDownSeq++, "fin", new Uint8Array(), options);
            traceStream("remote_fin", parsed, { upstreamBytes: state.upstreamBytes, downstreamBytes: state.downstreamBytes });
            break;
          }
          if (read.value?.byteLength) {
            state.lastActivity = performance.now();
            for (const chunk of splitChunks(read.value, MAX_DOWN_CHUNK_BYTES)) {
              const seq = state.nextDownSeq++;
              await uploadDownObject(drive, parsed, seq, "data", chunk, options);
              state.uploadedObjects += 1;
              state.downstreamBytes += chunk.byteLength;
              traceStream("down_data", parsed, { seq, bytes: chunk.byteLength });
            }
            state.nextUpPollAt = performance.now();
          }
        }
        if (state.upstreamClosed && !pendingRead) {
          await sleep(configuredReadWaitMs(options));
        }
      }
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      traceStream("error", parsed, {
        error: message,
        upstreamBytes: state.upstreamBytes,
        downstreamBytes: state.downstreamBytes,
        processedUpObjects: state.processedUpObjects,
        uploadedObjects: state.uploadedObjects,
        durationMs: Math.round(performance.now() - started),
      });
      await uploadDownObject(drive, parsed, state.nextDownSeq++, "err", new TextEncoder().encode(message), options).catch(
        () => undefined,
      );
      return {
        processed: true,
        requestFileId: candidate.fileId,
        requestName: candidate.name,
        responseName: downObjectPrefix(parsed, options),
        responseBytes: state.downstreamBytes,
        upstreamBytes: state.upstreamBytes,
        upstreamMs: Math.round(performance.now() - started),
        deletedRequest: true,
        error: message,
      };
    }
    if (!state.downstreamClosed) {
      await uploadDownObject(drive, parsed, state.nextDownSeq++, "fin", new Uint8Array(), options);
    }
    traceStream("closed", parsed, {
      upstreamBytes: state.upstreamBytes,
      downstreamBytes: state.downstreamBytes,
      processedUpObjects: state.processedUpObjects,
      uploadedObjects: state.uploadedObjects,
      durationMs: Math.round(performance.now() - started),
    });
    return {
      processed: true,
      requestFileId: candidate.fileId,
      requestName: candidate.name,
      responseName: downObjectPrefix(parsed, options),
      responseBytes: state.downstreamBytes,
      upstreamBytes: state.upstreamBytes,
      upstreamMs: Math.round(performance.now() - started),
      deletedRequest: true,
    };
  } finally {
    await reader.cancel().catch(() => {});
    writer.releaseLock();
    reader.releaseLock();
    await socket.close().catch(() => undefined);
  }
}

export function defaultStreamUpPrefix(): string {
  return DEFAULT_STREAM_UP_PREFIX;
}

export function defaultStreamDownPrefix(): string {
  return DEFAULT_STREAM_DOWN_PREFIX;
}

function parseUpName(name: string, upPrefix: string): StreamObjectName | undefined {
  if (!name.startsWith(upPrefix)) {
    return undefined;
  }
  const rest = name.slice(upPrefix.length);
  const parts = rest.split("/");
  if (parts.length < 3) {
    return undefined;
  }
  const [session, stream, leaf] = parts;
  const match = /^(\d+)\.([A-Za-z0-9_-]+)$/.exec(leaf);
  if (!session || !stream || !match) {
    return undefined;
  }
  return {
    session,
    stream,
    seq: Number.parseInt(match[1], 10),
    kind: match[2],
  };
}

async function processUpObjects(
  drive: StreamDriveClient,
  stream: StreamObjectName,
  state: {
    nextUpSeq: number;
    upstreamClosed: boolean;
    lastActivity: number;
    processedUpObjects: number;
    upstreamBytes: number;
  },
  writer: WritableStreamDefaultWriter<Uint8Array>,
  options: StreamProcessorOptions,
): Promise<void> {
  if (state.upstreamClosed) {
    return;
  }
  const files = await listStreamFiles(drive, {
    contains: upObjectPrefix(stream, options),
    parentId: options.folderId,
    pageSize: MAX_UP_CHANGES_PER_TICK,
  });
  const ordered = files
    .map((file) => ({ file, parsed: parseUpName(file.name ?? "", options.upPrefix ?? DEFAULT_STREAM_UP_PREFIX) }))
    .filter((item): item is { file: { id: string; name?: string }; parsed: StreamObjectName } =>
      Boolean(item.file.id && item.parsed && item.parsed.seq >= state.nextUpSeq),
    )
    .sort((a, b) => a.parsed.seq - b.parsed.seq);

  for (const item of ordered) {
    if (item.parsed.seq !== state.nextUpSeq) {
      break;
    }
    try {
      const payload = JSON.parse(new TextDecoder().decode(await drive.downloadFile(item.file.id))) as StreamUpMessage;
      if (payload.type === "data") {
        const bytes = base64ToBytes(payload.data);
        await writeWithTimeout(writer, bytes, configuredWriteTimeoutMs(options));
        state.upstreamBytes += bytes.byteLength;
        traceStream("up_data", stream, { seq: item.parsed.seq, bytes: bytes.byteLength });
      } else if (payload.type === "fin") {
        state.upstreamClosed = true;
        await writer.close().catch(() => {});
        traceStream("up_fin", stream, { seq: item.parsed.seq });
      } else {
        throw new Error(`unsupported stream object type ${(payload as { type?: string }).type ?? "unknown"}`);
      }
    } catch (error) {
      state.upstreamClosed = true;
      await writer.abort(error).catch(() => {});
      traceStream("up_error", stream, { seq: item.parsed.seq, error: error instanceof Error ? error.message : String(error) });
    }
    await drive.deleteFile(item.file.id);
    state.nextUpSeq += 1;
    state.processedUpObjects += 1;
    state.lastActivity = performance.now();
    if (state.upstreamClosed) {
      break;
    }
  }
}

async function listStreamFiles(
  drive: StreamDriveClient,
  input: {
    contains: string;
    parentId?: string;
    pageSize?: number;
  },
): Promise<Array<{ id?: string; name?: string; size?: string; createdTime?: string; modifiedTime?: string }>> {
  try {
    return await drive.listFilesByNameContains(input);
  } catch (error) {
    if (!input.parentId || !isMissingDriveParent(error)) {
      throw error;
    }
    return drive.listFilesByNameContains({
      ...input,
      parentId: undefined,
    });
  }
}

function traceStream(event: string, stream: StreamObjectName, details: Record<string, unknown> = {}): void {
  console.log(
    JSON.stringify({
      component: "skirk_stream",
      event,
      session: stream.session,
      stream: stream.stream,
      ...details,
    }),
  );
}

async function uploadDownObject(
  drive: StreamDriveClient,
  stream: StreamObjectName,
  seq: number,
  kind: "opened" | "data" | "fin" | "err",
  bytes: Uint8Array,
  options: StreamProcessorOptions,
): Promise<void> {
  await uploadStreamFile(drive, {
    name: `${downObjectPrefix(stream, options)}${padSeq(seq)}.${kind}`,
    body: arrayBufferForBody(bytes),
    mimeType: "application/octet-stream",
    parents: options.folderId ? [options.folderId] : undefined,
  });
}

async function uploadStreamFile(
  drive: StreamDriveClient,
  input: {
    name: string;
    body: BlobPart;
    mimeType?: string;
    parents?: string[];
  },
): Promise<void> {
  try {
    await drive.uploadFile(input);
  } catch (error) {
    if (!input.parents?.length || !isMissingDriveParent(error)) {
      throw error;
    }
    await drive.uploadFile({
      ...input,
      parents: undefined,
    });
  }
}

function upObjectPrefix(stream: StreamObjectName, options: StreamProcessorOptions): string {
  const prefix = options.upPrefix ?? DEFAULT_STREAM_UP_PREFIX;
  return `${prefix}${stream.session}/${stream.stream}/`;
}

function downObjectPrefix(stream: StreamObjectName, options: StreamProcessorOptions): string {
  const prefix = options.downPrefix ?? DEFAULT_STREAM_DOWN_PREFIX;
  return `${prefix}${stream.session}/${stream.stream}/`;
}

function streamKey(stream: StreamObjectName): string {
  return `${stream.session}/${stream.stream}`;
}

function streamResult(
  candidate: CandidateObject,
  stream: StreamObjectName,
  started: number,
  responseBytes: number,
  upstreamBytes: number,
  deletedRequest: boolean,
  error?: unknown,
): ExitProcessResult {
  return {
    processed: true,
    requestFileId: candidate.fileId,
    requestName: candidate.name,
    responseName: downObjectPrefix(stream, {}),
    responseBytes,
    upstreamBytes,
    upstreamMs: Math.round(performance.now() - started),
    deletedRequest,
    error: error === undefined ? undefined : error instanceof Error ? error.message : String(error),
  };
}

function configuredIdleTimeoutMs(options: StreamProcessorOptions): number {
  return positiveInteger(options.idleTimeoutMs, DEFAULT_IDLE_TIMEOUT_MS);
}

function configuredReadWaitMs(options: StreamProcessorOptions): number {
  return positiveInteger(options.readWaitMs, DEFAULT_READ_WAIT_MS);
}

function configuredUpPollMs(options: StreamProcessorOptions): number {
  return positiveInteger(options.upPollMs, DEFAULT_UP_POLL_MS);
}

function configuredWriteTimeoutMs(options: StreamProcessorOptions): number {
  return positiveInteger(options.writeTimeoutMs, DEFAULT_WRITE_TIMEOUT_MS);
}

async function writeWithTimeout(
  writer: WritableStreamDefaultWriter<Uint8Array>,
  bytes: Uint8Array,
  timeoutMs: number,
): Promise<void> {
  const result = await withTimeout(writer.write(bytes), timeoutMs);
  if (result === undefined) {
    throw new Error(`tcp write timeout after ${timeoutMs}ms bytes=${bytes.byteLength}`);
  }
}

function positiveInteger(value: number | undefined, fallback: number): number {
  return Number.isFinite(value) && value && value > 0 ? Math.floor(value) : fallback;
}

function base64ToBytes(value: string): Uint8Array {
  return Uint8Array.from(atob(value), (char) => char.charCodeAt(0));
}

function arrayBufferForBody(bytes: Uint8Array): ArrayBuffer {
  return bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) as ArrayBuffer;
}

function splitChunks(bytes: Uint8Array, size: number): Uint8Array[] {
  const chunks = [];
  for (let offset = 0; offset < bytes.byteLength; offset += size) {
    chunks.push(bytes.slice(offset, offset + size));
  }
  return chunks;
}

function isAllowedDestinationHost(hostname: string): boolean {
  const host = hostname.toLowerCase().replace(/^\[|\]$/g, "");
  if (!host || host === "localhost" || host.endsWith(".localhost") || host === "metadata.google.internal") {
    return false;
  }
  if (/^\d+\.\d+\.\d+\.\d+$/.test(host)) {
    const [a, b] = host.split(".").map((part) => Number.parseInt(part, 10));
    return !(
      a === 0 ||
      a === 10 ||
      a === 127 ||
      (a === 169 && b === 254) ||
      (a === 172 && b >= 16 && b <= 31) ||
      (a === 192 && b === 168) ||
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

async function processNewerOpenObjects(
  drive: StreamDriveClient,
  activeStream: StreamObjectName,
  options: StreamProcessorOptions,
): Promise<void> {
  const files = await drive.listFilesByNameContains({
    contains: options.upPrefix ?? DEFAULT_STREAM_UP_PREFIX,
    parentId: options.folderId,
    pageSize: 100,
  });
  const openObjects = files
    .map((file) => ({ file, parsed: parseUpName(file.name ?? "", options.upPrefix ?? DEFAULT_STREAM_UP_PREFIX) }))
    .filter((item): item is { file: { id: string; name?: string }; parsed: StreamObjectName } =>
      Boolean(
        item.file.id &&
          item.parsed &&
          item.parsed.kind === "open" &&
          !(item.parsed.session === activeStream.session && item.parsed.stream === activeStream.stream),
      ),
  );
  for (const item of openObjects) {
    await processStreamCandidate(
      drive,
      { fileId: item.file.id, name: item.file.name ?? "", size: 0 },
      options,
    );
  }
}

function isMissingDriveParent(error: unknown): boolean {
  if (!(error instanceof Error)) {
    return false;
  }
  const message = error.message.toLowerCase();
  return message.includes("status=404") && message.includes("file not found");
}

function padSeq(seq: number): string {
  return String(seq).padStart(12, "0");
}

async function withTimeout<T>(promise: Promise<T>, timeoutMs: number): Promise<T | undefined> {
  return Promise.race([promise, sleep(timeoutMs).then(() => undefined)]);
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
