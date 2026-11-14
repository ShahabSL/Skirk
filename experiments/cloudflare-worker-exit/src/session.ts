import { DriveClient, getCachedAccessToken } from "./drive";
import { defaultRequestPrefix, defaultResponsePrefix, processExitCandidate } from "./exit";
import { createInitialMetrics, pumpDriveChanges } from "./pump";
import { defaultStreamDownPrefix, defaultStreamUpPrefix, DriveStreamRuntime } from "./stream";
import type { DriveChannel, Env, ExitProcessResult, OAuthAccessToken, PumpResult, SessionMetrics } from "./types";

const PAGE_TOKEN_KEY = "drivePageToken";
const ACCESS_TOKEN_KEY = "googleAccessToken";
const METRICS_KEY = "metrics";
const CHANNEL_KEY = "driveChannel";
const SEEN_CANDIDATES_KEY = "seenCandidates";
const ACTIVE_UNTIL_KEY = "activeUntil";
const CHANNEL_RENEW_BEFORE_MS = 12 * 60 * 60 * 1000;
const DEFAULT_CHANNEL_TTL_MS = 6 * 24 * 60 * 60 * 1000;
const SEEN_CANDIDATE_LIMIT = 5000;
const DEFAULT_IDLE_POLL_MS = 5000;
const DEFAULT_ACTIVE_POLL_MS = 1000;
const DEFAULT_ACTIVE_WINDOW_MS = 30_000;
const DEFAULT_EXIT_FETCH_CONCURRENCY = 4;

export class SkirkSession {
  private pumpInFlight?: Promise<PumpResult>;
  private pumpAgain = false;
  private streamRuntime?: DriveStreamRuntime;

  constructor(
    private readonly state: DurableObjectState,
    private readonly env: Env,
  ) {}

  async alarm(): Promise<void> {
    await this.updateMetrics((metrics) => ({
      ...metrics,
      lastAlarmAt: new Date().toISOString(),
    }));
    try {
      await this.pump();
    } catch (error) {
      await this.updateMetrics((metrics) => ({
        ...metrics,
        lastError: error instanceof Error ? error.message : String(error),
      }));
      await this.scheduleNextAlarm();
    }
  }

  async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);
    try {
      if (request.method === "POST" && url.pathname === "/pump") {
        const result = await this.pump();
        return json(result);
      }
      if (request.method === "POST" && url.pathname === "/webhook-seen") {
        await this.markWebhookSeen();
        return json({ ok: true });
      }
      if (request.method === "POST" && url.pathname === "/watch") {
        const input = (await request.json()) as { address?: string; channelId?: string; expiration?: number };
        const channel = await this.watch(input);
        return json({ ok: true, channel });
      }
      if (request.method === "POST" && url.pathname === "/stop") {
        const stopped = await this.stopWatch();
        return json({ ok: true, stopped });
      }
      if (request.method === "POST" && url.pathname === "/renew-watch") {
        const renewed = await this.renewWatch();
        return json({ ok: true, renewed });
      }
      if (request.method === "GET" && url.pathname === "/metrics") {
        return json(await this.metrics());
      }
      return json({ ok: false, error: "not found" }, 404);
    } catch (error) {
      await this.updateMetrics((metrics) => ({
        ...metrics,
        errorCount: metrics.errorCount + 1,
        lastError: error instanceof Error ? error.message : String(error),
      }));
      return json({ ok: false, error: error instanceof Error ? error.message : String(error) }, 500);
    }
  }

  private async pump(): Promise<PumpResult> {
    if (this.pumpInFlight) {
      this.pumpAgain = true;
      return this.pumpInFlight;
    }
    this.pumpInFlight = this.drainCoalescedPumps().then(async (result) => {
      await this.scheduleNextAlarm(result);
      return result;
    });
    try {
      return await this.pumpInFlight;
    } finally {
      this.pumpInFlight = undefined;
    }
  }

  private async drainCoalescedPumps(): Promise<PumpResult> {
    let result: PumpResult;
    do {
      this.pumpAgain = false;
      result = await pumpDriveChanges(this.drive(), this, {
        prefix: this.env.SKIRK_PREFIX ?? "",
        maxPages: parseInteger(this.env.SKIRK_MAX_PAGES_PER_PUMP),
        maxChanges: parseInteger(this.env.SKIRK_MAX_CHANGES_PER_PUMP),
      });
      result = await this.processWorkerRequests(result);
    } while (this.pumpAgain);
    return result;
  }

  private async watch(input: { address?: string; channelId?: string; expiration?: number }): Promise<DriveChannel> {
    if (!input.address?.trim()) {
      throw new Error("watch address is required");
    }
    if (!this.env.SKIRK_WEBHOOK_TOKEN?.trim()) {
      throw new Error("SKIRK_WEBHOOK_TOKEN is required");
    }
    let pageToken = await this.getPageToken();
    if (!pageToken) {
      pageToken = await this.drive().getStartPageToken();
      await this.putPageToken(pageToken);
    }
    const channel = await this.drive().watchChanges({
      id: input.channelId?.trim() || crypto.randomUUID(),
      address: input.address.trim(),
      token: this.env.SKIRK_WEBHOOK_TOKEN,
      expiration: input.expiration ?? Date.now() + DEFAULT_CHANNEL_TTL_MS,
      pageToken,
    });
    const storedChannel = {
      ...channel,
      address: input.address.trim(),
      createdAt: new Date().toISOString(),
    };
    await this.state.storage.put(CHANNEL_KEY, storedChannel);
    await this.updateMetrics((metrics) => ({ ...metrics, channel: storedChannel }));
    await this.scheduleNextAlarm();
    return storedChannel;
  }

  private async stopWatch(): Promise<boolean> {
    const channel = await this.state.storage.get<DriveChannel>(CHANNEL_KEY);
    if (!channel) {
      return false;
    }
    await this.drive().stopChannel(channel);
    await this.state.storage.delete(CHANNEL_KEY);
    await this.state.storage.delete(ACTIVE_UNTIL_KEY);
    await this.state.storage.deleteAlarm();
    await this.updateMetrics((metrics) => {
      const next = { ...metrics };
      delete next.channel;
      delete next.nextAlarmAt;
      return next;
    });
    return true;
  }

  private async renewWatch(): Promise<boolean> {
    const channel = await this.state.storage.get<DriveChannel>(CHANNEL_KEY);
    if (!channel?.address) {
      return false;
    }
    const expiresAt = Number.parseInt(channel.expiration ?? "0", 10);
    if (Number.isFinite(expiresAt) && expiresAt > Date.now() + CHANNEL_RENEW_BEFORE_MS) {
      return false;
    }
    const renewed = await this.watch({ address: channel.address });
    if (channel.resourceId) {
      try {
        await this.drive().stopChannel(channel);
      } catch {
        // The new channel is already active; failing to stop an expired old channel is not fatal.
      }
    }
    await this.updateMetrics((metrics) => ({ ...metrics, channel: renewed }));
    return true;
  }

  private async markWebhookSeen(): Promise<void> {
    await this.markActiveFor(configuredActiveWindowMs(this.env));
    await this.updateMetrics((metrics) => ({
      ...metrics,
      lastWebhookAt: new Date().toISOString(),
    }));
    await this.scheduleNextAlarm();
  }

  private drive(): DriveClient {
    return new DriveClient(() =>
      getCachedAccessToken(this.env, {
        get: () => this.state.storage.get<OAuthAccessToken>(ACCESS_TOKEN_KEY),
        put: (token) => this.state.storage.put(ACCESS_TOKEN_KEY, token),
      }),
    );
  }

  async getPageToken(): Promise<string | undefined> {
    return this.state.storage.get<string>(PAGE_TOKEN_KEY);
  }

  async putPageToken(token: string): Promise<void> {
    await this.state.storage.put(PAGE_TOKEN_KEY, token);
  }

  async dedupeCandidates(candidates: PumpResult["candidates"]): Promise<PumpResult["candidates"]> {
    if (candidates.length === 0) {
      return candidates;
    }
    const seen = (await this.state.storage.get<Record<string, string>>(SEEN_CANDIDATES_KEY)) ?? {};
    const now = new Date().toISOString();
    const fresh = [];
    for (const candidate of candidates) {
      if (candidate.name.startsWith(configuredRequestPrefix(this.env)) || candidate.name.startsWith(configuredStreamUpPrefix(this.env))) {
        fresh.push(candidate);
        continue;
      }
      if (seen[candidate.fileId]) {
        continue;
      }
      seen[candidate.fileId] = now;
      fresh.push(candidate);
    }
    await this.state.storage.put(SEEN_CANDIDATES_KEY, trimSeenCandidates(seen));
    return fresh;
  }

  private async processWorkerRequests(result: PumpResult): Promise<PumpResult> {
    const requestPrefix = configuredRequestPrefix(this.env);
    const streamUpPrefix = configuredStreamUpPrefix(this.env);
    const requests = result.candidates.filter(
      (candidate) => candidate.name.startsWith(requestPrefix) || candidate.name.startsWith(streamUpPrefix),
    );
    if (requests.length === 0) {
      return result;
    }

    const drive = this.drive();
    const exitResults = await mapBounded(requests, configuredExitFetchConcurrency(this.env), async (candidate) =>
      candidate.name.startsWith(streamUpPrefix)
        ? this.getStreamRuntime(drive).processCandidate(candidate)
        : processExitCandidate(drive, candidate, {
            requestPrefix,
            responsePrefix: configuredResponsePrefix(this.env),
            folderId: this.env.SKIRK_DRIVE_FOLDER_ID,
            maxResponseBytes: parseInteger(this.env.SKIRK_EXIT_MAX_RESPONSE_BYTES),
          }),
    );
    for (const exitResult of exitResults) {
      await this.updateExitMetrics(exitResult);
    }
    return {
      ...result,
      exitResults,
    };
  }

  private async updateExitMetrics(result: ExitProcessResult): Promise<void> {
    await this.updateMetrics((metrics) => ({
      ...metrics,
      exitProcessedCount: (metrics.exitProcessedCount ?? 0) + (result.processed ? 1 : 0),
      exitErrorCount: (metrics.exitErrorCount ?? 0) + (result.error ? 1 : 0),
      exitUploadedBytes: (metrics.exitUploadedBytes ?? 0) + (result.responseBytes ?? 0),
      lastExit: result.processed ? result : metrics.lastExit,
    }));
  }

  private getStreamRuntime(drive: DriveClient): DriveStreamRuntime {
    if (!this.streamRuntime) {
      this.streamRuntime = new DriveStreamRuntime(
        drive,
        {
          upPrefix: configuredStreamUpPrefix(this.env),
          downPrefix: configuredStreamDownPrefix(this.env),
          folderId: this.env.SKIRK_DRIVE_FOLDER_ID,
          idleTimeoutMs: parseInteger(this.env.SKIRK_STREAM_IDLE_TIMEOUT_MS),
          readWaitMs: parseInteger(this.env.SKIRK_STREAM_READ_WAIT_MS),
          upPollMs: parseInteger(this.env.SKIRK_STREAM_UP_POLL_MS),
          scanDuringStream: parseBoolean(this.env.SKIRK_STREAM_SCAN_DURING_STREAM),
          writeTimeoutMs: parseInteger(this.env.SKIRK_STREAM_WRITE_TIMEOUT_MS),
        },
        (promise) => this.state.waitUntil(promise),
      );
    }
    return this.streamRuntime;
  }

  private async scheduleNextAlarm(result?: PumpResult): Promise<void> {
    const channel = await this.state.storage.get<DriveChannel>(CHANNEL_KEY);
    if (!channel) {
      await this.state.storage.deleteAlarm();
      await this.updateMetrics((metrics) => {
        const next = { ...metrics };
        delete next.nextAlarmAt;
        return next;
      });
      return;
    }

    if (result && isActivePumpResult(result)) {
      await this.markActiveFor(configuredActiveWindowMs(this.env));
    }

    const delayMs = await this.nextFallbackDelayMs();
    if (delayMs <= 0) {
      await this.state.storage.deleteAlarm();
      await this.updateMetrics((metrics) => {
        const next = { ...metrics };
        delete next.nextAlarmAt;
        return next;
      });
      return;
    }

    const target = Date.now() + delayMs;
    const currentAlarm = await this.state.storage.getAlarm();
    if (currentAlarm === null || currentAlarm > target + Math.max(250, Math.floor(delayMs / 2))) {
      await this.state.storage.setAlarm(target);
    }
    await this.updateMetrics((metrics) => ({
      ...metrics,
      nextAlarmAt: new Date(target).toISOString(),
    }));
  }

  private async nextFallbackDelayMs(): Promise<number> {
    const activeUntil = (await this.state.storage.get<number>(ACTIVE_UNTIL_KEY)) ?? 0;
    if (activeUntil > Date.now()) {
      return configuredActivePollMs(this.env);
    }
    return configuredIdlePollMs(this.env);
  }

  private async markActiveFor(durationMs: number): Promise<void> {
    if (durationMs <= 0) {
      return;
    }
    await this.state.storage.put(ACTIVE_UNTIL_KEY, Date.now() + durationMs);
  }

  async updateMetrics(update: (metrics: SessionMetrics) => SessionMetrics): Promise<SessionMetrics> {
    const current = await this.metrics();
    const next = update(current);
    await this.state.storage.put(METRICS_KEY, next);
    return next;
  }

  private async metrics(): Promise<SessionMetrics> {
    return (await this.state.storage.get<SessionMetrics>(METRICS_KEY)) ?? createInitialMetrics();
  }
}

function parseInteger(value: string | undefined): number | undefined {
  if (!value) {
    return undefined;
  }
  const parsed = Number.parseInt(value, 10);
  return Number.isFinite(parsed) ? parsed : undefined;
}

function parseBoolean(value: string | undefined): boolean | undefined {
  if (value === undefined || value.trim() === "") {
    return undefined;
  }
  const normalized = value.trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(normalized)) {
    return true;
  }
  if (["0", "false", "no", "off"].includes(normalized)) {
    return false;
  }
  return undefined;
}

function configuredIdlePollMs(env: Env): number {
  return nonNegativeInteger(env.SKIRK_FALLBACK_IDLE_POLL_MS, DEFAULT_IDLE_POLL_MS);
}

function configuredActivePollMs(env: Env): number {
  return nonNegativeInteger(env.SKIRK_FALLBACK_ACTIVE_POLL_MS, DEFAULT_ACTIVE_POLL_MS);
}

function configuredActiveWindowMs(env: Env): number {
  return nonNegativeInteger(env.SKIRK_FALLBACK_ACTIVE_WINDOW_MS, DEFAULT_ACTIVE_WINDOW_MS);
}

function configuredExitFetchConcurrency(env: Env): number {
  return boundedPositiveInteger(env.SKIRK_EXIT_FETCH_CONCURRENCY, DEFAULT_EXIT_FETCH_CONCURRENCY, 1, 4);
}

function configuredRequestPrefix(env: Env): string {
  return env.SKIRK_EXIT_REQUEST_PREFIX?.trim() || defaultRequestPrefix();
}

function configuredResponsePrefix(env: Env): string {
  return env.SKIRK_EXIT_RESPONSE_PREFIX?.trim() || defaultResponsePrefix();
}

function configuredStreamUpPrefix(env: Env): string {
  return env.SKIRK_STREAM_UP_PREFIX?.trim() || defaultStreamUpPrefix();
}

function configuredStreamDownPrefix(env: Env): string {
  return env.SKIRK_STREAM_DOWN_PREFIX?.trim() || defaultStreamDownPrefix();
}

function nonNegativeInteger(value: string | undefined, fallback: number): number {
  if (value === undefined || value.trim() === "") {
    return fallback;
  }
  const parsed = Number.parseInt(value, 10);
  if (!Number.isFinite(parsed) || parsed < 0) {
    return fallback;
  }
  return Math.floor(parsed);
}

function boundedPositiveInteger(value: string | undefined, fallback: number, min: number, max: number): number {
  if (value === undefined || value.trim() === "") {
    return fallback;
  }
  const parsed = Number.parseInt(value, 10);
  if (!Number.isFinite(parsed)) {
    return fallback;
  }
  return Math.max(min, Math.min(max, Math.floor(parsed)));
}

async function mapBounded<T, R>(items: T[], concurrency: number, worker: (item: T) => Promise<R>): Promise<R[]> {
  const results = new Array<R>(items.length);
  let next = 0;
  const runners = Array.from({ length: Math.min(concurrency, items.length) }, async () => {
    while (next < items.length) {
      const index = next;
      next += 1;
      results[index] = await worker(items[index]);
    }
  });
  await Promise.all(runners);
  return results;
}

function isActivePumpResult(result: PumpResult): boolean {
  return result.truncated || result.changes > 0 || result.matched > 0 || result.removed > 0;
}

function trimSeenCandidates(seen: Record<string, string>): Record<string, string> {
  const entries = Object.entries(seen);
  if (entries.length <= SEEN_CANDIDATE_LIMIT) {
    return seen;
  }
  entries.sort((a, b) => b[1].localeCompare(a[1]));
  return Object.fromEntries(entries.slice(0, SEEN_CANDIDATE_LIMIT));
}

function json(payload: unknown, status = 200): Response {
  return new Response(JSON.stringify(payload, null, 2), {
    status,
    headers: { "content-type": "application/json; charset=utf-8" },
  });
}
