import { afterEach, describe, expect, it, vi } from "vitest";
import { SkirkSession } from "../src/session";
import type { Env } from "../src/types";

class MemoryStorage {
  readonly values = new Map<string, unknown>();
  alarmTime: number | null = null;

  async get<T = unknown>(key: string): Promise<T | undefined> {
    return this.values.get(key) as T | undefined;
  }

  async put<T>(key: string, value: T): Promise<void> {
    this.values.set(key, value);
  }

  async delete(key: string): Promise<boolean> {
    return this.values.delete(key);
  }

  async getAlarm(): Promise<number | null> {
    return this.alarmTime;
  }

  async setAlarm(scheduledTime: number | Date): Promise<void> {
    this.alarmTime = scheduledTime instanceof Date ? scheduledTime.getTime() : scheduledTime;
  }

  async deleteAlarm(): Promise<void> {
    this.alarmTime = null;
  }
}

describe("SkirkSession alarms", () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("schedules idle fallback polling after registering a Drive watch", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-05-24T12:00:00.000Z"));
    stubGoogleDrive();
    const { session, storage } = sessionWithStorage();

    const response = await session.fetch(
      new Request("https://session/watch", {
        method: "POST",
        body: JSON.stringify({ address: "https://worker.example/drive/push" }),
      }),
    );

    expect(response.status).toBe(200);
    expect(storage.alarmTime).toBe(Date.parse("2026-05-24T12:00:05.000Z"));
    await expect(metrics(session)).resolves.toMatchObject({
      nextAlarmAt: "2026-05-24T12:00:05.000Z",
    });
  });

  it("switches to the faster active fallback after a Drive webhook", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-05-24T12:00:00.000Z"));
    stubGoogleDrive();
    const { session, storage } = sessionWithStorage();

    await session.fetch(
      new Request("https://session/watch", {
        method: "POST",
        body: JSON.stringify({ address: "https://worker.example/drive/push" }),
      }),
    );
    const response = await session.fetch(new Request("https://session/webhook-seen", { method: "POST" }));

    expect(response.status).toBe(200);
    expect(storage.alarmTime).toBe(Date.parse("2026-05-24T12:00:01.000Z"));
    await expect(metrics(session)).resolves.toMatchObject({
      lastWebhookAt: "2026-05-24T12:00:00.000Z",
      nextAlarmAt: "2026-05-24T12:00:01.000Z",
    });
  });

  it("pumps on alarm and keeps active polling while changes are arriving", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-05-24T12:00:00.000Z"));
    stubGoogleDrive({
      changes: [{ fileId: "file-1", file: { id: "file-1", name: "b20ff/r1/p0/0001", size: "123" } }],
      newStartPageToken: "start-2",
    });
    const { session, storage } = sessionWithStorage();

    await session.fetch(
      new Request("https://session/watch", {
        method: "POST",
        body: JSON.stringify({ address: "https://worker.example/drive/push" }),
      }),
    );
    await session.alarm();

    expect(storage.alarmTime).toBe(Date.parse("2026-05-24T12:00:01.000Z"));
    await expect(metrics(session)).resolves.toMatchObject({
      lastAlarmAt: "2026-05-24T12:00:00.000Z",
      totalMatched: 1,
      nextAlarmAt: "2026-05-24T12:00:01.000Z",
    });
  });

  it("deletes the fallback alarm when the watch is stopped", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-05-24T12:00:00.000Z"));
    stubGoogleDrive();
    const { session, storage } = sessionWithStorage();

    await session.fetch(
      new Request("https://session/watch", {
        method: "POST",
        body: JSON.stringify({ address: "https://worker.example/drive/push" }),
      }),
    );
    const response = await session.fetch(new Request("https://session/stop", { method: "POST" }));

    expect(response.status).toBe(200);
    expect(storage.alarmTime).toBeNull();
    const currentMetrics = await metrics(session);
    expect(currentMetrics.channel).toBeUndefined();
    expect(currentMetrics.nextAlarmAt).toBeUndefined();
  });
});

function sessionWithStorage(): { session: SkirkSession; storage: MemoryStorage } {
  const storage = new MemoryStorage();
  const state = { storage } as unknown as DurableObjectState;
  const session = new SkirkSession(state, {
    SESSIONS: {} as DurableObjectNamespace,
    SKIRK_PREFIX: "b20ff/",
    SKIRK_WEBHOOK_TOKEN: "webhook-token",
    GOOGLE_OAUTH_CLIENT_ID: "client-id",
    GOOGLE_OAUTH_CLIENT_SECRET: "client-secret",
    GOOGLE_REFRESH_TOKEN: "refresh-token",
  } satisfies Env);
  return { session, storage };
}

async function metrics(session: SkirkSession): Promise<Record<string, unknown>> {
  const response = await session.fetch(new Request("https://session/metrics"));
  return (await response.json()) as Record<string, unknown>;
}

function stubGoogleDrive(changePage: Record<string, unknown> = { changes: [], newStartPageToken: "start-2" }): void {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL): Promise<Response> => {
      const url = input.toString();
      if (url === "https://oauth2.googleapis.com/token") {
        return new Response(JSON.stringify({ access_token: "access-token", expires_in: 3600 }));
      }
      if (url.includes("/drive/v3/changes/startPageToken")) {
        return new Response(JSON.stringify({ startPageToken: "start-1" }));
      }
      if (url.includes("/drive/v3/changes/watch")) {
        return new Response(JSON.stringify({ id: "channel-1", resourceId: "resource-1", expiration: "1800000000000" }));
      }
      if (url.includes("/drive/v3/changes?")) {
        return new Response(JSON.stringify(changePage));
      }
      if (url.includes("/drive/v3/channels/stop")) {
        return new Response(null, { status: 204 });
      }
      return new Response(`unexpected ${url}`, { status: 500 });
    }),
  );
}
