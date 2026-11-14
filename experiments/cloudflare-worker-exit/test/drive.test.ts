import { describe, expect, it } from "vitest";
import { DriveClient, refreshAccessToken } from "../src/drive";
import type { Env } from "../src/types";

describe("Drive client", () => {
  it("refreshes OAuth access tokens with refresh_token grant", async () => {
    let body = "";
    const fetcher = async (_input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      body = init?.body?.toString() ?? "";
      return new Response(JSON.stringify({ access_token: "access-1", expires_in: 3600 }));
    };

    const token = await refreshAccessToken(
      {
        GOOGLE_OAUTH_CLIENT_ID: "client-id",
        GOOGLE_OAUTH_CLIENT_SECRET: "client-secret",
        GOOGLE_REFRESH_TOKEN: "refresh-token",
      } as Env,
      fetcher as typeof fetch,
      1_000,
    );

    expect(token).toEqual({ accessToken: "access-1", expiresAt: 3_601_000 });
    expect(body).toContain("grant_type=refresh_token");
    expect(body).toContain("refresh_token=refresh-token");
  });

  it("builds changes.watch with pageToken and webhook body", async () => {
    let requestedUrl = "";
    let requestedBody = "";
    const fetcher = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      requestedUrl = input.toString();
      requestedBody = init?.body?.toString() ?? "";
      return new Response(JSON.stringify({ id: "channel-1", resourceId: "resource-1" }));
    };
    const drive = new DriveClient(async () => "access-token", fetcher as typeof fetch);

    const channel = await drive.watchChanges({
      id: "channel-1",
      address: "https://worker.example/drive/push",
      token: "webhook-token",
      pageToken: "page-1",
      expiration: 1_800_000_000_000,
    });

    expect(channel).toEqual({ id: "channel-1", resourceId: "resource-1" });
    expect(requestedUrl).toContain("/drive/v3/changes/watch?pageToken=page-1");
    expect(JSON.parse(requestedBody)).toEqual({
      id: "channel-1",
      type: "web_hook",
      address: "https://worker.example/drive/push",
      token: "webhook-token",
      expiration: 1_800_000_000_000,
    });
  });

  it("downloads, uploads, and deletes Drive files through the media endpoints", async () => {
    const calls: Array<{ url: string; method: string; body?: BodyInit | null }> = [];
    const fetcher = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = input.toString();
      const method = init?.method ?? "GET";
      calls.push({ url, method, body: init?.body });
      if (url.includes("/drive/v3/files/file-1?alt=media")) {
        return new Response("payload");
      }
      if (url.includes("/upload/drive/v3/files?uploadType=multipart")) {
        return new Response(JSON.stringify({ id: "uploaded-1", name: "out.bin", size: "7" }));
      }
      if (url.includes("/drive/v3/files/file-1") && method === "DELETE") {
        return new Response(null, { status: 204 });
      }
      return new Response(`unexpected ${url}`, { status: 500 });
    };
    const drive = new DriveClient(async () => "access-token", fetcher as typeof fetch);

    await expect(new Response(await drive.downloadFile("file-1")).text()).resolves.toBe("payload");
    await expect(
      drive.uploadFile({
        id: "generated-id",
        name: "out.bin",
        body: new TextEncoder().encode("payload").buffer as ArrayBuffer,
        parents: ["folder-1"],
      }),
    ).resolves.toEqual({ id: "uploaded-1", name: "out.bin", size: "7" });
    await expect(drive.deleteFile("file-1")).resolves.toBeUndefined();

    expect(calls.map((call) => `${call.method} ${new URL(call.url).pathname}`)).toEqual([
      "GET /drive/v3/files/file-1",
      "POST /upload/drive/v3/files",
      "DELETE /drive/v3/files/file-1",
    ]);
    await expect(new Response(calls[1].body).text()).resolves.toContain('"id":"generated-id"');
  });

  it("follows Drive files.list pagination for name searches", async () => {
    const requestedPageTokens: Array<string | null> = [];
    const fetcher = async (input: RequestInfo | URL): Promise<Response> => {
      const url = new URL(input.toString());
      requestedPageTokens.push(url.searchParams.get("pageToken"));
      if (!url.searchParams.has("pageToken")) {
        return new Response(
          JSON.stringify({
            nextPageToken: "page-2",
            files: [{ id: "file-1", name: "cfstream/v1/down/s/stream/000000000001.data" }],
          }),
        );
      }
      return new Response(
        JSON.stringify({
          files: [{ id: "file-2", name: "cfstream/v1/down/s/stream/000000000002.fin" }],
        }),
      );
    };
    const drive = new DriveClient(async () => "access-token", fetcher as typeof fetch);

    const files = await drive.listFilesByNameContains({ contains: "cfstream/v1/down/s/stream/" });

    expect(files.map((file) => file.id)).toEqual(["file-1", "file-2"]);
    expect(requestedPageTokens).toEqual([null, "page-2"]);
  });
});
