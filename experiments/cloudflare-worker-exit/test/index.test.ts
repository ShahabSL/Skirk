import { describe, expect, it, vi } from "vitest";
import worker from "../src/index";
import type { Env } from "../src/types";

describe("Worker routes", () => {
  it("reports health without secrets", async () => {
    const response = await worker.fetch(new Request("https://worker.example/health"), envWithStub(), ctx());

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      ok: true,
      mode: "cloudflare-free-drive-http-relay",
      session: "default",
      capabilities: {
        httpFetchRelay: true,
        socksOrVpn: false,
      },
    });
  });

  it("serves a small public landing page", async () => {
    const response = await worker.fetch(new Request("https://worker.example/"), envWithStub(), ctx());

    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toContain("text/html");
    await expect(response.text()).resolves.toContain("Skirk Cloudflare Free Relay");
  });

  it("rejects unauthorized Drive webhooks", async () => {
    const response = await worker.fetch(new Request("https://worker.example/drive/push", { method: "POST" }), envWithStub(), ctx());

    expect(response.status).toBe(401);
  });

  it("accepts authorized Drive webhooks and schedules a pump", async () => {
    const execution = ctx();
    const response = await worker.fetch(
      new Request("https://worker.example/drive/push", {
        method: "POST",
        headers: {
          "x-goog-channel-token": "webhook-token",
          "x-goog-channel-id": "channel-1",
          "x-goog-resource-id": "resource-1",
          "x-goog-resource-state": "change",
        },
      }),
      envWithStub(),
      execution,
    );

    expect(response.status).toBe(202);
    expect(execution.waitUntil).toHaveBeenCalledTimes(2);
  });

  it("scheduled events renew the watch and run the fallback pump", async () => {
    const execution = ctx();
    await worker.scheduled({} as ScheduledController, envWithStub(), execution);

    expect(execution.waitUntil).toHaveBeenCalledTimes(2);
  });

  it("rejects unauthorized admin routes", async () => {
    const response = await worker.fetch(new Request("https://worker.example/admin/metrics"), envWithStub(), ctx());

    expect(response.status).toBe(401);
  });
});

function envWithStub(): Env {
  const stub = {
    fetch: vi.fn(async () => new Response(JSON.stringify({ ok: true }))),
  } as unknown as DurableObjectStub;
  return {
    SKIRK_WEBHOOK_TOKEN: "webhook-token",
    SKIRK_ADMIN_TOKEN: "admin-token",
    SESSIONS: {
      idFromName: vi.fn(() => ({ toString: () => "id" }) as unknown as DurableObjectId),
      get: vi.fn(() => stub),
    } as unknown as DurableObjectNamespace,
  };
}

function ctx(): ExecutionContext {
  return {
    waitUntil: vi.fn(),
    passThroughOnException: vi.fn(),
    props: {},
  } as unknown as ExecutionContext;
}
