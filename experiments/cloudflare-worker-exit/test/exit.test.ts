import { describe, expect, it } from "vitest";
import { configuredMaxBytes, processExitCandidate, type ExitDriveClient } from "../src/exit";

class FakeDrive implements ExitDriveClient {
  deleted: string[] = [];
  uploaded: Array<{ id?: string; name: string; body: string | ArrayBuffer; mimeType?: string; parents?: string[] }> = [];
  failFirstUploadWithMissingParent = false;

  constructor(private readonly requestBody: string) {}

  async downloadFile(_fileId: string): Promise<ArrayBuffer> {
    return new TextEncoder().encode(this.requestBody).buffer as ArrayBuffer;
  }

  async uploadFile(input: {
    id?: string;
    name: string;
    body: BlobPart;
    mimeType?: string;
    parents?: string[];
  }): Promise<{ id?: string; name?: string; size?: string }> {
    if (this.failFirstUploadWithMissingParent) {
      this.failFirstUploadWithMissingParent = false;
      throw new Error("drive api request failed status=404 body=File not found: stale-folder");
    }
    this.uploaded.push(input as { id?: string; name: string; body: string | ArrayBuffer; mimeType?: string; parents?: string[] });
    return { id: input.id ?? `uploaded-${this.uploaded.length}`, name: input.name };
  }

  async deleteFile(fileId: string): Promise<void> {
    this.deleted.push(fileId);
  }
}

describe("processExitCandidate", () => {
  it("does not let request maxBytes exceed the worker cap", () => {
    expect(configuredMaxBytes(32 * 1024 * 1024, 8 * 1024 * 1024)).toBe(8 * 1024 * 1024);
    expect(configuredMaxBytes(undefined, 8 * 1024 * 1024)).toBe(8 * 1024 * 1024);
  });

  it("ignores non-request objects", async () => {
    const drive = new FakeDrive("{}");
    const result = await processExitCandidate(drive, { fileId: "f1", name: "other/object", size: 1 });

    expect(result.processed).toBe(false);
    expect(drive.uploaded).toHaveLength(0);
    expect(drive.deleted).toHaveLength(0);
  });

  it("fetches an HTTP URL, uploads a bounded response, and deletes the request", async () => {
    const drive = new FakeDrive(
      JSON.stringify({
        kind: "http_fetch_v1",
        requestId: "req-1",
        url: "https://example.com/file.bin",
        maxBytes: 5,
      }),
    );
    const fetcher = async () => new Response("hello world", { status: 200 });

    const result = await processExitCandidate(
      drive,
      { fileId: "req-file", name: "cfexit/v1/req/req-1", size: 10 },
      {
        folderId: "folder-1",
        fetcher: fetcher as typeof fetch,
      },
    );

    expect(result).toMatchObject({
      processed: true,
      requestFileId: "req-file",
      responseFileId: "uploaded-1",
      responseBytes: 5,
      upstreamStatus: 200,
      upstreamBytes: 11,
      truncated: true,
      deletedRequest: true,
    });
    expect(result.responseName).toContain("cfexit/v1/res/req-1.status-200.bytes-5");
    expect(drive.uploaded[0].parents).toEqual(["folder-1"]);
    expect(await new Response(drive.uploaded[0].body).text()).toBe("hello");
    expect(drive.deleted).toEqual(["req-file"]);
  });

  it("fetches an HTTP proxy URL and uploads a response envelope", async () => {
    const drive = new FakeDrive(
      JSON.stringify({
        kind: "http_proxy_v1",
        requestId: "req-1",
        method: "GET",
        url: "http://example.com/index.html",
        headers: {
          accept: "text/html",
          connection: "keep-alive",
        },
      }),
    );
    const fetcher = async (_url: URL, init?: RequestInit) => {
      expect(init?.headers).toBeInstanceOf(Headers);
      expect((init?.headers as Headers).get("accept")).toBe("text/html");
      expect((init?.headers as Headers).get("connection")).toBeNull();
      return new Response("hello", {
        status: 203,
        headers: {
          "content-type": "text/plain",
          connection: "close",
        },
      });
    };

    const result = await processExitCandidate(
      drive,
      { fileId: "req-file", name: "cfexit/v1/req/req-1", size: 10 },
      {
        fetcher: fetcher as typeof fetch,
      },
    );

    const envelope = JSON.parse(await new Response(drive.uploaded[0].body).text()) as {
      ok: boolean;
      status: number;
      headers: Record<string, string>;
      bodyBase64: string;
    };
    expect(result.processed).toBe(true);
    expect(envelope.ok).toBe(true);
    expect(envelope.status).toBe(203);
    expect(envelope.headers["content-type"]).toBe("text/plain");
    expect(envelope.headers.connection).toBeUndefined();
    expect(new TextDecoder().decode(Uint8Array.from(atob(envelope.bodyBase64), (char) => char.charCodeAt(0)))).toBe("hello");
  });

  it("can upload binary HTTP proxy responses to a caller-provided Drive file id", async () => {
    const drive = new FakeDrive(
      JSON.stringify({
        kind: "http_proxy_v1",
        requestId: "req-1",
        responseFileId: "generated-response-id",
        responseFormat: "binary_v1",
        method: "GET",
        url: "https://example.com/download",
      }),
    );
    const fetcher = async () =>
      new Response("hello", {
        status: 200,
        headers: {
          "content-type": "text/plain",
          "content-length": "5",
        },
      });

    const result = await processExitCandidate(
      drive,
      { fileId: "req-file", name: "cfexit/v1/req/req-1", size: 10 },
      {
        fetcher: fetcher as typeof fetch,
      },
    );

    const body = new Uint8Array(await new Response(drive.uploaded[0].body).arrayBuffer());
    const magic = new TextDecoder().decode(body.slice(0, 8));
    const headerLength = new DataView(body.buffer, body.byteOffset + 8, 4).getUint32(0);
    const header = JSON.parse(new TextDecoder().decode(body.slice(12, 12 + headerLength))) as {
      ok: boolean;
      kind: string;
      status: number;
      bodyBase64?: string;
    };
    const payload = new TextDecoder().decode(body.slice(12 + headerLength));

    expect(result.responseFileId).toBe("generated-response-id");
    expect(drive.uploaded[0].id).toBe("generated-response-id");
    expect(magic).toBe("SKIRKCF1");
    expect(header.ok).toBe(true);
    expect(header.kind).toBe("http_proxy_binary_v1");
    expect(header.status).toBe(200);
    expect(header.bodyBase64).toBeUndefined();
    expect(payload).toBe("hello");
  });

  it("streams large binary HTTP proxy responses into caller-provided Drive chunks", async () => {
    const drive = new FakeDrive(
      JSON.stringify({
        kind: "http_proxy_v1",
        requestId: "req-1",
        responseFileId: "chunk-0",
        responseFileIds: ["chunk-0", "chunk-1", "chunk-2"],
        responseChunkBytes: 3,
        responseFormat: "binary_v1",
        method: "GET",
        url: "https://example.com/download",
      }),
    );
    const fetcher = async () => new Response("abcdefg", { status: 200 });

    const result = await processExitCandidate(
      drive,
      { fileId: "req-file", name: "cfexit/v1/req/req-1", size: 10 },
      {
        fetcher: fetcher as typeof fetch,
      },
    );

    const chunks = await Promise.all(drive.uploaded.map((upload) => decodeBinaryProxyUpload(upload.body)));
    expect(result.responseFileId).toBe("chunk-0");
    expect(result.upstreamBytes).toBe(7);
    expect(drive.uploaded.map((upload) => upload.id)).toEqual(["chunk-0", "chunk-1", "chunk-2"]);
    expect(chunks.map((chunk) => chunk.text)).toEqual(["abc", "def", "g"]);
    expect(chunks.map((chunk) => chunk.header.chunkIndex)).toEqual([0, 1, 2]);
    expect(chunks.map((chunk) => chunk.header.done)).toEqual([false, false, true]);
    expect(chunks.map((chunk) => chunk.header.more)).toEqual([true, true, false]);
  });

  it("preserves multiple Set-Cookie headers in binary proxy responses", async () => {
    const drive = new FakeDrive(
      JSON.stringify({
        kind: "http_proxy_v1",
        requestId: "req-1",
        responseFormat: "binary_v1",
        method: "GET",
        url: "https://example.com/login",
      }),
    );
    const fetcher = async () => {
      const response = new Response("ok", { status: 200 });
      Object.defineProperty(response.headers, "getSetCookie", {
        value: () => ["a=1; Path=/; HttpOnly", "b=2; Path=/; Secure"],
      });
      return response;
    };

    await processExitCandidate(
      drive,
      { fileId: "req-file", name: "cfexit/v1/req/req-1", size: 10 },
      {
        fetcher: fetcher as typeof fetch,
      },
    );

    const body = new Uint8Array(await new Response(drive.uploaded[0].body).arrayBuffer());
    const headerLength = new DataView(body.buffer, body.byteOffset + 8, 4).getUint32(0);
    const header = JSON.parse(new TextDecoder().decode(body.slice(12, 12 + headerLength))) as {
      headers: Record<string, string | string[]>;
    };
    expect(header.headers["set-cookie"]).toEqual(["a=1; Path=/; HttpOnly", "b=2; Path=/; Secure"]);
  });

  it("relays a small HTTPS POST envelope through Worker fetch", async () => {
    const drive = new FakeDrive(
      JSON.stringify({
        kind: "http_proxy_v1",
        requestId: "req-1",
        method: "POST",
        url: "https://example.com/api",
        headers: {
          "content-type": "application/json",
          host: "example.com",
        },
        bodyBase64: btoa('{"ok":true}'),
      }),
    );
    const fetcher = async (url: URL, init?: RequestInit) => {
      expect(url.href).toBe("https://example.com/api");
      expect(init?.method).toBe("POST");
      expect((init?.headers as Headers).get("host")).toBeNull();
      expect(await new Response(init?.body).text()).toBe('{"ok":true}');
      return new Response("created", { status: 201 });
    };

    const result = await processExitCandidate(
      drive,
      { fileId: "req-file", name: "cfexit/v1/req/req-1", size: 10 },
      {
        fetcher: fetcher as typeof fetch,
      },
    );

    const envelope = JSON.parse(await new Response(drive.uploaded[0].body).text()) as {
      ok: boolean;
      status: number;
      bodyBase64: string;
    };
    expect(result.processed).toBe(true);
    expect(envelope.ok).toBe(true);
    expect(envelope.status).toBe(201);
    expect(new TextDecoder().decode(Uint8Array.from(atob(envelope.bodyBase64), (char) => char.charCodeAt(0)))).toBe("created");
  });

  it("blocks private destinations in HTTP proxy mode", async () => {
    const drive = new FakeDrive(
      JSON.stringify({
        kind: "http_proxy_v1",
        requestId: "req-1",
        url: "http://127.0.0.1/admin",
      }),
    );

    const result = await processExitCandidate(drive, { fileId: "req-file", name: "cfexit/v1/req/req-1", size: 10 });

    expect(result.error).toContain("blocked private");
    expect(drive.deleted).toEqual(["req-file"]);
  });

  it("falls back to root upload when the configured Drive parent is stale", async () => {
    const drive = new FakeDrive(
      JSON.stringify({
        kind: "http_fetch_v1",
        requestId: "req-1",
        url: "https://example.com/file.bin",
      }),
    );
    drive.failFirstUploadWithMissingParent = true;
    const fetcher = async () => new Response("ok", { status: 200 });

    const result = await processExitCandidate(
      drive,
      { fileId: "req-file", name: "cfexit/v1/req/req-1", size: 10 },
      {
        folderId: "stale-folder",
        fetcher: fetcher as typeof fetch,
      },
    );

    expect(result.processed).toBe(true);
    expect(result.responseFileId).toBe("uploaded-1");
    expect(result.deletedRequest).toBe(true);
    expect(drive.uploaded[0].parents).toBeUndefined();
  });

  it("publishes an error response and deletes malformed requests", async () => {
    const drive = new FakeDrive("{bad json");

    const result = await processExitCandidate(drive, { fileId: "req-file", name: "cfexit/v1/req/req-2", size: 10 });

    expect(result.processed).toBe(true);
    expect(result.error).toContain("JSON");
    expect(drive.uploaded[0].mimeType).toBe("application/json");
    expect(drive.deleted).toEqual(["req-file"]);
  });
});

async function decodeBinaryProxyUpload(body: string | ArrayBuffer): Promise<{ header: Record<string, unknown>; text: string }> {
  const bytes = new Uint8Array(await new Response(body).arrayBuffer());
  const magic = new TextDecoder().decode(bytes.slice(0, 8));
  expect(magic).toBe("SKIRKCF1");
  const headerLength = new DataView(bytes.buffer, bytes.byteOffset + 8, 4).getUint32(0);
  const header = JSON.parse(new TextDecoder().decode(bytes.slice(12, 12 + headerLength))) as Record<string, unknown>;
  return {
    header,
    text: new TextDecoder().decode(bytes.slice(12 + headerLength)),
  };
}
