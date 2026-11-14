import type { DriveChangePage, DriveChannel, DriveFile, Env, OAuthAccessToken } from "./types";

const GOOGLE_TOKEN_URL = "https://oauth2.googleapis.com/token";
const GOOGLE_DRIVE_URL = "https://www.googleapis.com/drive/v3";
const GOOGLE_DRIVE_UPLOAD_URL = "https://www.googleapis.com/upload/drive/v3";
const TOKEN_REFRESH_SKEW_MS = 60_000;
const defaultFetch: typeof fetch = (input, init) => fetch(input, init);

export class DriveApiError extends Error {
  readonly status: number;
  readonly body: string;

  constructor(message: string, status: number, body: string) {
    super(message);
    this.name = "DriveApiError";
    this.status = status;
    this.body = body;
  }
}

export interface AccessTokenCache {
  get(): Promise<OAuthAccessToken | undefined>;
  put(token: OAuthAccessToken): Promise<void>;
}

export async function getCachedAccessToken(
  env: Env,
  cache: AccessTokenCache,
  fetcher: typeof fetch = defaultFetch,
  now = Date.now(),
): Promise<string> {
  const cached = await cache.get();
  if (cached && cached.expiresAt > now + TOKEN_REFRESH_SKEW_MS) {
    return cached.accessToken;
  }
  const refreshed = await refreshAccessToken(env, fetcher, now);
  await cache.put(refreshed);
  return refreshed.accessToken;
}

export async function refreshAccessToken(
  env: Env,
  fetcher: typeof fetch = defaultFetch,
  now = Date.now(),
): Promise<OAuthAccessToken> {
  requireSecret(env.GOOGLE_OAUTH_CLIENT_ID, "GOOGLE_OAUTH_CLIENT_ID");
  requireSecret(env.GOOGLE_OAUTH_CLIENT_SECRET, "GOOGLE_OAUTH_CLIENT_SECRET");
  requireSecret(env.GOOGLE_REFRESH_TOKEN, "GOOGLE_REFRESH_TOKEN");

  const body = new URLSearchParams({
    client_id: env.GOOGLE_OAUTH_CLIENT_ID ?? "",
    client_secret: env.GOOGLE_OAUTH_CLIENT_SECRET ?? "",
    refresh_token: env.GOOGLE_REFRESH_TOKEN ?? "",
    grant_type: "refresh_token",
  });
  const response = await fetcher(GOOGLE_TOKEN_URL, {
    method: "POST",
    headers: {
      "content-type": "application/x-www-form-urlencoded",
    },
    body,
  });
  const text = await response.text();
  if (!response.ok) {
    throw new DriveApiError("google oauth refresh failed", response.status, text);
  }
  const payload = JSON.parse(text) as { access_token?: string; expires_in?: number };
  if (!payload.access_token) {
    throw new Error("google oauth refresh response missing access_token");
  }
  const expiresInMs = Math.max(1, payload.expires_in ?? 3600) * 1000;
  return {
    accessToken: payload.access_token,
    expiresAt: now + expiresInMs,
  };
}

export class DriveClient {
  constructor(
    private readonly accessToken: () => Promise<string>,
    private readonly fetcher: typeof fetch = defaultFetch,
  ) {}

  async getStartPageToken(): Promise<string> {
    const payload = await this.driveFetch<{ startPageToken?: string }>("/changes/startPageToken?fields=startPageToken");
    const token = payload.startPageToken?.trim();
    if (!token) {
      throw new Error("drive start page token response missing token");
    }
    return token;
  }

  async listChanges(pageToken: string, includeRemoved = true, pageSize = 100): Promise<DriveChangePage> {
    const token = pageToken.trim();
    if (!token) {
      throw new Error("drive changes page token is required");
    }
    const values = new URLSearchParams({
      pageToken: token,
      pageSize: String(pageSize),
      includeRemoved: String(includeRemoved),
      fields: "nextPageToken,newStartPageToken,changes(fileId,removed,time,file(id,name,size,modifiedTime,mimeType,parents))",
    });
    return this.driveFetch<DriveChangePage>(`/changes?${values.toString()}`);
  }

  async downloadFile(fileId: string): Promise<ArrayBuffer> {
    const id = fileId.trim();
    if (!id) {
      throw new Error("drive download requires file id");
    }
    const response = await this.driveRawFetch(`/files/${encodeURIComponent(id)}?alt=media`);
    if (!response.ok) {
      throw await DriveApiError.fromResponse("drive download failed", response);
    }
    return response.arrayBuffer();
  }

  async uploadFile(input: {
    id?: string;
    name: string;
    body: BlobPart;
    mimeType?: string;
    parents?: string[];
  }): Promise<{ id?: string; name?: string; size?: string }> {
    const name = input.name.trim();
    if (!name) {
      throw new Error("drive upload requires file name");
    }
    const boundary = `skirk-worker-${crypto.randomUUID()}`;
    const metadata: Record<string, unknown> = {
      name,
      mimeType: input.mimeType ?? "application/octet-stream",
    };
    if (input.id?.trim()) {
      metadata.id = input.id.trim();
    }
    const parents = input.parents?.filter((parent) => parent.trim());
    if (parents?.length) {
      metadata.parents = parents;
    }
    const body = new Blob([
      `--${boundary}\r\ncontent-type: application/json; charset=utf-8\r\n\r\n`,
      JSON.stringify(metadata),
      `\r\n--${boundary}\r\ncontent-type: ${metadata.mimeType}\r\n\r\n`,
      input.body,
      `\r\n--${boundary}--\r\n`,
    ]);
    const values = new URLSearchParams({
      uploadType: "multipart",
      fields: "id,name,size",
    });
    return this.driveFetchAt<{ id?: string; name?: string; size?: string }>(
      GOOGLE_DRIVE_UPLOAD_URL,
      `/files?${values.toString()}`,
      {
        method: "POST",
        headers: {
          "content-type": `multipart/related; boundary=${boundary}`,
        },
        body,
      },
    );
  }

  async listFilesByNameContains(input: {
    contains: string;
    parentId?: string;
    pageSize?: number;
  }): Promise<DriveFile[]> {
    const contains = input.contains.trim();
    if (!contains) {
      throw new Error("drive file list requires name contains filter");
    }
    const q = [`trashed = false`, `name contains '${escapeDriveQuery(contains)}'`];
    if (input.parentId?.trim()) {
      q.push(`'${escapeDriveQuery(input.parentId)}' in parents`);
    }
    const files: DriveFile[] = [];
    let pageToken: string | undefined;
    do {
      const values = new URLSearchParams({
        q: q.join(" and "),
        pageSize: String(clampPageSize(input.pageSize ?? 100)),
        fields: "nextPageToken,incompleteSearch,files(id,name,size,createdTime,modifiedTime)",
      });
      if (pageToken) {
        values.set("pageToken", pageToken);
      }
      const payload = await this.driveFetch<{ files?: DriveFile[]; nextPageToken?: string }>(`/files?${values.toString()}`);
      files.push(...(payload.files ?? []));
      pageToken = payload.nextPageToken;
    } while (pageToken);
    return files;
  }

  async deleteFile(fileId: string): Promise<void> {
    const id = fileId.trim();
    if (!id) {
      throw new Error("drive delete requires file id");
    }
    await this.driveRawFetch(`/files/${encodeURIComponent(id)}`, { method: "DELETE" }).then(async (response) => {
      if (!response.ok && response.status !== 404) {
        throw await DriveApiError.fromResponse("drive delete failed", response);
      }
    });
  }

  async watchChanges(input: {
    id: string;
    address: string;
    token: string;
    expiration?: number;
    pageToken: string;
  }): Promise<DriveChannel> {
    const values = new URLSearchParams({
      pageToken: input.pageToken,
    });
    const body: Record<string, string | number> = {
      id: input.id,
      type: "web_hook",
      address: input.address,
      token: input.token,
    };
    if (input.expiration) {
      body.expiration = input.expiration;
    }
    return this.driveFetch<DriveChannel>(`/changes/watch?${values.toString()}`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  async stopChannel(channel: DriveChannel): Promise<void> {
    if (!channel.id || !channel.resourceId) {
      throw new Error("drive channel stop requires channel id and resource id");
    }
    await this.driveFetch<void>("/channels/stop", {
      method: "POST",
      body: JSON.stringify({ id: channel.id, resourceId: channel.resourceId }),
    });
  }

  private async driveFetch<T>(path: string, init: RequestInit = {}): Promise<T> {
    return this.driveFetchAt<T>(GOOGLE_DRIVE_URL, path, init);
  }

  private async driveFetchAt<T>(baseUrl: string, path: string, init: RequestInit = {}): Promise<T> {
    const response = await this.driveRawFetchAt(baseUrl, path, init);
    const text = await response.text();
    if (!response.ok) {
      throw new DriveApiError(`drive api request failed status=${response.status} body=${truncateForError(text)}`, response.status, text);
    }
    if (response.status === 204 || text.length === 0) {
      return undefined as T;
    }
    return JSON.parse(text) as T;
  }

  private async driveRawFetch(path: string, init: RequestInit = {}): Promise<Response> {
    return this.driveRawFetchAt(GOOGLE_DRIVE_URL, path, init);
  }

  private async driveRawFetchAt(baseUrl: string, path: string, init: RequestInit = {}): Promise<Response> {
    const token = await this.accessToken();
    const headers = new Headers(init.headers);
    headers.set("authorization", `Bearer ${token}`);
    if (init.body && !headers.has("content-type")) {
      headers.set("content-type", "application/json");
    }
    return this.fetcher(`${baseUrl}${path}`, {
      ...init,
      headers,
    });
  }
}

export namespace DriveApiError {
  export async function fromResponse(message: string, response: Response): Promise<DriveApiError> {
    return new DriveApiError(message, response.status, await response.text());
  }
}

function requireSecret(value: string | undefined, name: string): void {
  if (!value?.trim()) {
    throw new Error(`${name} is required`);
  }
}

function truncateForError(value: string): string {
  return value.length <= 600 ? value : `${value.slice(0, 600)}...`;
}

function escapeDriveQuery(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/'/g, "\\'");
}

function clampPageSize(value: number): number {
  if (!Number.isFinite(value) || value < 1) {
    return 100;
  }
  return Math.max(1, Math.min(1000, Math.floor(value)));
}
