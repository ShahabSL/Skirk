#!/usr/bin/env node

import { readFile } from "node:fs/promises";
import { performance } from "node:perf_hooks";

const DRIVE_URL = "https://www.googleapis.com/drive/v3";
const DRIVE_UPLOAD_URL = "https://www.googleapis.com/upload/drive/v3";
const DEFAULT_REQUEST_PREFIX = "cfexit/v1/req/";
const DEFAULT_RESPONSE_PREFIX = "cfexit/v1/res/";

const args = parseArgs(process.argv.slice(2));
const config = await loadConfig(args);
const requestBytes = integerOption(args.bytes ?? process.env.SKIRK_BENCH_BYTES, 8 * 1024 * 1024);
const targetUrl = args.url ?? process.env.SKIRK_BENCH_URL ?? "https://speed.cloudflare.com/__down?bytes=8388608";
const workerUrl = required(args.workerUrl ?? process.env.WORKER_URL, "WORKER_URL or --worker-url");
const adminToken = required(args.adminToken ?? process.env.SKIRK_ADMIN_TOKEN, "SKIRK_ADMIN_TOKEN or --admin-token");
const folderId = args.folderId ?? process.env.SKIRK_DRIVE_FOLDER_ID ?? config.drive?.folder_id;
const requestPrefix = args.requestPrefix ?? process.env.SKIRK_EXIT_REQUEST_PREFIX ?? DEFAULT_REQUEST_PREFIX;
const responsePrefix = args.responsePrefix ?? process.env.SKIRK_EXIT_RESPONSE_PREFIX ?? DEFAULT_RESPONSE_PREFIX;
const pollMs = integerOption(args.pollMs ?? process.env.SKIRK_BENCH_POLL_MS, 500);
const timeoutMs = integerOption(args.timeoutMs ?? process.env.SKIRK_BENCH_TIMEOUT_MS, 120_000);
const knownResponseId = booleanOption(args.knownResponseId ?? process.env.SKIRK_BENCH_KNOWN_RESPONSE_ID, true);

const accessToken = await refreshAccessToken(config);
const started = performance.now();
const requestId = `bench-${new Date().toISOString().replace(/[^0-9TZ]/g, "")}-${crypto.randomUUID().slice(0, 8)}`;
const requestName = `${requestPrefix}${requestId}`;
const responseNeedle = `${responsePrefix}${requestId}`;
const responseFileId = knownResponseId ? (await generateIds(accessToken, 1))[0] : undefined;
let driveListPolls = 0;
let driveMetadataPolls = 0;
let adminPumpCalls = 0;
let responseFile;

await adminPump(workerUrl, adminToken);
adminPumpCalls += 1;
const uploadStarted = performance.now();
const requestFile = await uploadFileWithParentFallback(accessToken, {
  name: requestName,
  folderId,
  mimeType: "application/json",
  body: JSON.stringify({
    kind: args.kind ?? "http_fetch_v1",
    requestId,
    url: targetUrl,
    maxBytes: requestBytes,
    responseFileId,
  }),
});
const uploadMs = performance.now() - uploadStarted;

try {
  while (performance.now() - started < timeoutMs) {
    await sleep(pollMs);
    await adminPump(workerUrl, adminToken);
    adminPumpCalls += 1;
    if (responseFileId) {
      driveMetadataPolls += 1;
      responseFile = await metadata(accessToken, responseFileId).catch((error) => {
        if (isDriveNotFound(error)) {
          return undefined;
        }
        throw error;
      });
    } else {
      driveListPolls += 1;
      responseFile = await findResponse(accessToken, responseNeedle, folderId);
    }
    if (responseFile) {
      break;
    }
  }
  if (!responseFile) {
    throw new Error(`timed out waiting for response ${responseNeedle}`);
  }

  const downloadStarted = performance.now();
  const body = await downloadFile(accessToken, responseFile.id);
  const downloadMs = performance.now() - downloadStarted;
  await deleteFile(accessToken, responseFile.id);
  const elapsedMs = performance.now() - started;
  const mbps = (body.byteLength * 8) / (elapsedMs / 1000) / 1_000_000;
  const payloadMbps = (body.byteLength * 8) / (downloadMs / 1000) / 1_000_000;
  const quotaUnits = {
    responseGenerateId: responseFileId ? 5 : 0,
    requestUpload: 50,
    adminPumpChangesListEstimate: adminPumpCalls * 100,
    responseListPolls: driveListPolls * 100,
    responseMetadataPolls: driveMetadataPolls * 5,
    workerRequestDownload: 200,
    workerResponseUpload: 50,
    workerRequestDelete: 50,
    responseDownload: 200,
    responseDelete: 50,
  };
  const totalQuotaUnits = Object.values(quotaUnits).reduce((sum, value) => sum + value, 0);
  console.log(
    JSON.stringify(
      {
        ok: true,
        requestId,
        requestFileId: requestFile.id,
        responseFileId: responseFile.id,
        bytes: body.byteLength,
        elapsedMs,
        uploadMs,
        downloadMs,
        mbps: round(mbps, 3),
        payloadDownloadMbps: round(payloadMbps, 3),
        adminPumpCalls,
        driveListPolls,
        driveMetadataPolls,
        quotaUnits,
        totalQuotaUnits,
        unitsPerMiB: round(totalQuotaUnits / Math.max(1, body.byteLength / 1024 / 1024), 1),
      },
      null,
      2,
    ),
  );
} catch (error) {
  await deleteFile(accessToken, requestFile.id).catch(() => {});
  throw error;
}

async function loadConfig(options) {
  if (options.config) {
    return JSON.parse(await readFile(options.config, "utf8"));
  }
  return {
    auth: {
      client_id: required(process.env.GOOGLE_OAUTH_CLIENT_ID, "GOOGLE_OAUTH_CLIENT_ID"),
      client_secret: required(process.env.GOOGLE_OAUTH_CLIENT_SECRET, "GOOGLE_OAUTH_CLIENT_SECRET"),
      refresh_token: required(process.env.GOOGLE_REFRESH_TOKEN, "GOOGLE_REFRESH_TOKEN"),
    },
    drive: {
      folder_id: process.env.SKIRK_DRIVE_FOLDER_ID,
    },
  };
}

async function refreshAccessToken(config) {
  const auth = config.auth ?? {};
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
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`oauth refresh failed: ${response.status} ${text}`);
  }
  const payload = JSON.parse(text);
  if (!payload.access_token) {
    throw new Error("oauth refresh missing access_token");
  }
  return payload.access_token;
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

async function uploadFile(accessToken, input) {
  const boundary = `skirk-bench-${crypto.randomUUID()}`;
  const metadata = {
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
      authorization: `Bearer ${accessToken}`,
      "content-type": `multipart/related; boundary=${boundary}`,
    },
    body,
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`drive upload failed: ${response.status} ${text}`);
  }
  return JSON.parse(text);
}

async function generateIds(accessToken, count) {
  const url = new URL("/drive/v3/files/generateIds", DRIVE_URL);
  url.searchParams.set("count", String(count));
  url.searchParams.set("fields", "ids");
  const response = await fetch(url, {
    headers: { authorization: `Bearer ${accessToken}` },
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

async function metadata(accessToken, fileId) {
  const url = new URL(`/drive/v3/files/${encodeURIComponent(fileId)}`, DRIVE_URL);
  url.searchParams.set("fields", "id,name,size,createdTime");
  const response = await fetch(url, {
    headers: { authorization: `Bearer ${accessToken}` },
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`drive metadata failed: ${response.status} ${text}`);
  }
  return JSON.parse(text);
}

async function uploadFileWithParentFallback(accessToken, input) {
  try {
    return await uploadFile(accessToken, input);
  } catch (error) {
    if (!input.folderId || !isMissingDriveParent(error)) {
      throw error;
    }
    return uploadFile(accessToken, {
      ...input,
      folderId: undefined,
    });
  }
}

async function findResponse(accessToken, responseNeedle, folderId) {
  const q = [
    "trashed = false",
    `name contains '${escapeDriveQuery(responseNeedle)}'`,
    ...(folderId ? [`'${escapeDriveQuery(folderId)}' in parents`] : []),
  ].join(" and ");
  const url = new URL("/drive/v3/files", DRIVE_URL);
  url.searchParams.set("q", q);
  url.searchParams.set("pageSize", "10");
  url.searchParams.set("fields", "files(id,name,size,createdTime)");
  const response = await fetch(url, {
    headers: { authorization: `Bearer ${accessToken}` },
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`drive response list failed: ${response.status} ${text}`);
  }
  const payload = JSON.parse(text);
  if (payload.files?.[0] || !folderId) {
    return payload.files?.[0];
  }
  return findResponse(accessToken, responseNeedle, undefined);
}

async function downloadFile(accessToken, fileId) {
  const response = await fetch(`${DRIVE_URL}/files/${encodeURIComponent(fileId)}?alt=media`, {
    headers: { authorization: `Bearer ${accessToken}` },
  });
  if (!response.ok) {
    throw new Error(`drive response download failed: ${response.status} ${await response.text()}`);
  }
  return new Uint8Array(await response.arrayBuffer());
}

async function deleteFile(accessToken, fileId) {
  if (!fileId) {
    return;
  }
  const response = await fetch(`${DRIVE_URL}/files/${encodeURIComponent(fileId)}`, {
    method: "DELETE",
    headers: { authorization: `Bearer ${accessToken}` },
  });
  if (!response.ok && response.status !== 404) {
    throw new Error(`drive delete failed: ${response.status} ${await response.text()}`);
  }
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
    parsed[key] = rawValue ?? argv[++index];
  }
  return parsed;
}

function integerOption(value, fallback) {
  const parsed = Number.parseInt(String(value ?? ""), 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
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

function escapeDriveQuery(value) {
  return value.replace(/\\/g, "\\\\").replace(/'/g, "\\'");
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function round(value, places) {
  const scale = 10 ** places;
  return Math.round(value * scale) / scale;
}
