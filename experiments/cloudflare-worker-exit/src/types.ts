export interface Env {
  SESSIONS: DurableObjectNamespace;
  SKIRK_SESSION_ID?: string;
  SKIRK_PREFIX?: string;
  SKIRK_DRIVE_FOLDER_ID?: string;
  SKIRK_EXIT_REQUEST_PREFIX?: string;
  SKIRK_EXIT_RESPONSE_PREFIX?: string;
  SKIRK_EXIT_MAX_RESPONSE_BYTES?: string;
  SKIRK_EXIT_FETCH_CONCURRENCY?: string;
  SKIRK_STREAM_UP_PREFIX?: string;
  SKIRK_STREAM_DOWN_PREFIX?: string;
  SKIRK_STREAM_IDLE_TIMEOUT_MS?: string;
  SKIRK_STREAM_READ_WAIT_MS?: string;
  SKIRK_STREAM_UP_POLL_MS?: string;
  SKIRK_STREAM_SCAN_DURING_STREAM?: string;
  SKIRK_STREAM_WRITE_TIMEOUT_MS?: string;
  SKIRK_ADMIN_TOKEN?: string;
  SKIRK_WEBHOOK_TOKEN?: string;
  GOOGLE_OAUTH_CLIENT_ID?: string;
  GOOGLE_OAUTH_CLIENT_SECRET?: string;
  GOOGLE_REFRESH_TOKEN?: string;
  SKIRK_MAX_PAGES_PER_PUMP?: string;
  SKIRK_MAX_CHANGES_PER_PUMP?: string;
  SKIRK_FALLBACK_IDLE_POLL_MS?: string;
  SKIRK_FALLBACK_ACTIVE_POLL_MS?: string;
  SKIRK_FALLBACK_ACTIVE_WINDOW_MS?: string;
}

export interface DriveFile {
  id?: string;
  name?: string;
  size?: string;
  modifiedTime?: string;
  mimeType?: string;
  parents?: string[];
  createdTime?: string;
}

export interface DriveChange {
  fileId?: string;
  removed?: boolean;
  time?: string;
  file?: DriveFile;
}

export interface DriveChangePage {
  changes?: DriveChange[];
  nextPageToken?: string;
  newStartPageToken?: string;
}

export interface DriveChannel {
  id: string;
  resourceId?: string;
  resourceUri?: string;
  expiration?: string;
  address?: string;
  createdAt?: string;
}

export interface OAuthAccessToken {
  accessToken: string;
  expiresAt: number;
}

export interface CandidateObject {
  fileId: string;
  name: string;
  size: number;
  modifiedTime?: string;
}

export interface PumpResult {
  ok: true;
  initialized: boolean;
  pages: number;
  changes: number;
  matched: number;
  removed: number;
  truncated: boolean;
  pageToken: string;
  candidates: CandidateObject[];
  exitResults?: ExitProcessResult[];
}

export interface SessionMetrics {
  startedAt: string;
  lastPumpAt?: string;
  lastWebhookAt?: string;
  lastAlarmAt?: string;
  nextAlarmAt?: string;
  pumpCount: number;
  initCount: number;
  errorCount: number;
  resetCount: number;
  totalPages: number;
  totalChanges: number;
  totalMatched: number;
  totalRemoved: number;
  exitProcessedCount?: number;
  exitErrorCount?: number;
  exitUploadedBytes?: number;
  lastError?: string;
  lastPump?: PumpResult;
  lastExit?: ExitProcessResult;
  channel?: DriveChannel;
}

export interface ExitProcessResult {
  processed: boolean;
  requestFileId?: string;
  requestName?: string;
  responseName?: string;
  responseFileId?: string;
  responseBytes?: number;
  upstreamBytes?: number;
  upstreamStatus?: number;
  upstreamMs?: number;
  truncated?: boolean;
  deletedRequest?: boolean;
  error?: string;
}
