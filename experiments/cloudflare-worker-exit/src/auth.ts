import type { Env } from "./types";

const encoder = new TextEncoder();

export async function constantTimeEqual(left: string, right: string): Promise<boolean> {
  const [leftDigest, rightDigest] = await Promise.all([
    crypto.subtle.digest("SHA-256", encoder.encode(left)),
    crypto.subtle.digest("SHA-256", encoder.encode(right)),
  ]);
  const a = new Uint8Array(leftDigest);
  const b = new Uint8Array(rightDigest);
  let diff = a.length ^ b.length;
  for (let i = 0; i < Math.max(a.length, b.length); i += 1) {
    diff |= (a[i] ?? 0) ^ (b[i] ?? 0);
  }
  return diff === 0;
}

export async function isAdminAuthorized(request: Request, env: Env): Promise<boolean> {
  const expected = env.SKIRK_ADMIN_TOKEN?.trim();
  if (!expected) {
    return false;
  }
  const authorization = request.headers.get("authorization") ?? "";
  const match = authorization.match(/^Bearer\s+(.+)$/i);
  if (!match) {
    return false;
  }
  return constantTimeEqual(match[1].trim(), expected);
}

export async function isDriveWebhookAuthorized(request: Request, env: Env): Promise<boolean> {
  const expected = env.SKIRK_WEBHOOK_TOKEN?.trim();
  if (!expected) {
    return false;
  }
  const actual = request.headers.get("x-goog-channel-token")?.trim() ?? "";
  return constantTimeEqual(actual, expected);
}

export function driveWebhookHeaders(request: Request): Record<string, string | undefined> {
  return {
    channelId: request.headers.get("x-goog-channel-id") ?? undefined,
    channelToken: request.headers.get("x-goog-channel-token") ?? undefined,
    resourceId: request.headers.get("x-goog-resource-id") ?? undefined,
    resourceState: request.headers.get("x-goog-resource-state") ?? undefined,
    messageNumber: request.headers.get("x-goog-message-number") ?? undefined,
  };
}
