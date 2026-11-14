import { describe, expect, it } from "vitest";
import { driveWebhookHeaders, isAdminAuthorized, isDriveWebhookAuthorized } from "../src/auth";
import type { Env } from "../src/types";

describe("auth", () => {
  const env = {
    SKIRK_ADMIN_TOKEN: "admin-token",
    SKIRK_WEBHOOK_TOKEN: "webhook-token",
  } as Env;

  it("authorizes admin bearer tokens", async () => {
    const request = new Request("https://worker/admin/metrics", {
      headers: { authorization: "Bearer admin-token" },
    });

    await expect(isAdminAuthorized(request, env)).resolves.toBe(true);
  });

  it("rejects missing and incorrect admin bearer tokens", async () => {
    await expect(isAdminAuthorized(new Request("https://worker/admin/metrics"), env)).resolves.toBe(false);
    await expect(
      isAdminAuthorized(
        new Request("https://worker/admin/metrics", {
          headers: { authorization: "Bearer wrong" },
        }),
        env,
      ),
    ).resolves.toBe(false);
  });

  it("authorizes Drive webhook channel tokens", async () => {
    const request = new Request("https://worker/drive/push", {
      headers: {
        "x-goog-channel-id": "channel-1",
        "x-goog-channel-token": "webhook-token",
        "x-goog-resource-id": "resource-1",
        "x-goog-resource-state": "change",
        "x-goog-message-number": "4",
      },
    });

    await expect(isDriveWebhookAuthorized(request, env)).resolves.toBe(true);
    expect(driveWebhookHeaders(request)).toEqual({
      channelId: "channel-1",
      channelToken: "webhook-token",
      resourceId: "resource-1",
      resourceState: "change",
      messageNumber: "4",
    });
  });

  it("rejects bad Drive webhook channel tokens", async () => {
    const request = new Request("https://worker/drive/push", {
      headers: { "x-goog-channel-token": "wrong" },
    });

    await expect(isDriveWebhookAuthorized(request, env)).resolves.toBe(false);
  });
});
