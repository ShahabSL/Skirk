import { DriveApiError } from "./drive";
import type { CandidateObject, DriveChange, DriveChangePage, PumpResult, SessionMetrics } from "./types";

const DEFAULT_MAX_PAGES = 8;
const DEFAULT_MAX_CHANGES = 1000;

export interface ChangeFeedClient {
  getStartPageToken(): Promise<string>;
  listChanges(pageToken: string, includeRemoved: boolean, pageSize?: number): Promise<DriveChangePage>;
}

export interface PumpState {
  getPageToken(): Promise<string | undefined>;
  putPageToken(token: string): Promise<void>;
  updateMetrics(update: (metrics: SessionMetrics) => SessionMetrics): Promise<SessionMetrics>;
  dedupeCandidates?(candidates: CandidateObject[]): Promise<CandidateObject[]>;
}

export interface PumpOptions {
  prefix: string;
  maxPages?: number;
  maxChanges?: number;
  now?: Date;
}

export async function pumpDriveChanges(
  drive: ChangeFeedClient,
  state: PumpState,
  options: PumpOptions,
): Promise<PumpResult> {
  const now = options.now ?? new Date();
  const maxPages = clampPositive(options.maxPages, DEFAULT_MAX_PAGES);
  const maxChanges = clampPositive(options.maxChanges, DEFAULT_MAX_CHANGES);
  const prefix = options.prefix;
  let pageToken = await state.getPageToken();
  if (!pageToken) {
    pageToken = await drive.getStartPageToken();
    await state.putPageToken(pageToken);
    const result: PumpResult = {
      ok: true,
      initialized: true,
      pages: 0,
      changes: 0,
      matched: 0,
      removed: 0,
      truncated: false,
      pageToken,
      candidates: [],
    };
    await state.updateMetrics((metrics) => ({
      ...metrics,
      initCount: metrics.initCount + 1,
      lastPumpAt: now.toISOString(),
      lastPump: result,
      lastError: undefined,
    }));
    return result;
  }

  try {
    const result = await drainChanges(drive, pageToken, prefix, maxPages, maxChanges);
    const metricResult = await dedupeResultCandidates(state, result);
    await state.putPageToken(result.pageToken);
    await state.updateMetrics((metrics) => ({
      ...metrics,
      pumpCount: metrics.pumpCount + 1,
      totalPages: metrics.totalPages + metricResult.pages,
      totalChanges: metrics.totalChanges + metricResult.changes,
      totalMatched: metrics.totalMatched + metricResult.matched,
      totalRemoved: metrics.totalRemoved + metricResult.removed,
      lastPumpAt: now.toISOString(),
      lastPump: metricResult,
      lastError: undefined,
    }));
    return metricResult;
  } catch (error) {
    if (isPageTokenRejected(error)) {
      const resetToken = await drive.getStartPageToken();
      await state.putPageToken(resetToken);
      const result: PumpResult = {
        ok: true,
        initialized: true,
        pages: 0,
        changes: 0,
        matched: 0,
        removed: 0,
        truncated: false,
        pageToken: resetToken,
        candidates: [],
      };
      await state.updateMetrics((metrics) => ({
        ...metrics,
        resetCount: metrics.resetCount + 1,
        lastPumpAt: now.toISOString(),
        lastPump: result,
        lastError: "drive change token reset",
      }));
      return result;
    }
    await state.updateMetrics((metrics) => ({
      ...metrics,
      errorCount: metrics.errorCount + 1,
      lastError: error instanceof Error ? error.message : String(error),
    }));
    throw error;
  }
}

async function dedupeResultCandidates(state: PumpState, result: PumpResult): Promise<PumpResult> {
  if (!state.dedupeCandidates || result.candidates.length === 0) {
    return result;
  }
  const candidates = await state.dedupeCandidates(result.candidates);
  return {
    ...result,
    matched: candidates.length,
    candidates,
  };
}

async function drainChanges(
  drive: ChangeFeedClient,
  initialPageToken: string,
  prefix: string,
  maxPages: number,
  maxChanges: number,
): Promise<PumpResult> {
  let pageToken = initialPageToken;
  let pages = 0;
  let changes = 0;
  let matched = 0;
  let removed = 0;
  const candidates: CandidateObject[] = [];
  let truncated = false;

  while (pages < maxPages && changes < maxChanges) {
    const page = await drive.listChanges(pageToken, true, 100);
    pages += 1;
    for (const change of page.changes ?? []) {
      changes += 1;
      if (change.removed) {
        removed += 1;
        continue;
      }
      const candidate = candidateFromChange(change, prefix);
      if (candidate) {
        matched += 1;
        candidates.push(candidate);
      }
      if (changes >= maxChanges) {
        truncated = true;
        break;
      }
    }
    if (truncated) {
      break;
    }
    if (page.nextPageToken) {
      pageToken = page.nextPageToken;
      continue;
    }
    if (page.newStartPageToken) {
      pageToken = page.newStartPageToken;
    }
    return {
      ok: true,
      initialized: false,
      pages,
      changes,
      matched,
      removed,
      truncated: false,
      pageToken,
      candidates,
    };
  }

  return {
    ok: true,
    initialized: false,
    pages,
    changes,
    matched,
    removed,
    truncated: true,
    pageToken,
    candidates,
  };
}

function candidateFromChange(change: DriveChange, prefix: string): CandidateObject | undefined {
  const name = change.file?.name ?? "";
  if (!name || (prefix && !name.startsWith(prefix))) {
    return undefined;
  }
  const fileId = change.file?.id ?? change.fileId ?? "";
  if (!fileId) {
    return undefined;
  }
  return {
    fileId,
    name,
    size: Number.parseInt(change.file?.size ?? "0", 10) || 0,
    modifiedTime: change.file?.modifiedTime ?? change.time,
  };
}

function clampPositive(value: number | undefined, fallback: number): number {
  if (!Number.isFinite(value) || !value || value < 1) {
    return fallback;
  }
  return Math.floor(value);
}

function isPageTokenRejected(error: unknown): boolean {
  if (!(error instanceof DriveApiError)) {
    return false;
  }
  return error.status === 410 || error.status === 404;
}

export function createInitialMetrics(now = new Date()): SessionMetrics {
  return {
    startedAt: now.toISOString(),
    pumpCount: 0,
    initCount: 0,
    errorCount: 0,
    resetCount: 0,
    totalPages: 0,
    totalChanges: 0,
    totalMatched: 0,
    totalRemoved: 0,
  };
}
