import { driveWebhookHeaders, isAdminAuthorized, isDriveWebhookAuthorized } from "./auth";
import { SkirkSession } from "./session";
import type { Env } from "./types";

export { SkirkSession };

export default {
  async fetch(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/") {
      return html(homePage(env, url.origin));
    }
    if (request.method === "GET" && url.pathname === "/health") {
      return json({
        ok: true,
        mode: "cloudflare-free-drive-http-relay",
        session: sessionName(env),
        capabilities: {
          drivePush: true,
          durableObjectState: true,
          httpFetchRelay: true,
          inspectedHttpsRelay: true,
          socksOrVpn: false,
          httpsConnect: false,
        },
      });
    }
    if (request.method === "POST" && url.pathname === "/drive/push") {
      return handleDrivePush(request, env, ctx);
    }
    if (url.pathname.startsWith("/admin/")) {
      if (!(await isAdminAuthorized(request, env))) {
        return json({ ok: false, error: "unauthorized" }, 401);
      }
      return handleAdmin(request, env, url);
    }
    return json({ ok: false, error: "not found" }, 404);
  },
  async scheduled(_controller: ScheduledController, env: Env, ctx: ExecutionContext): Promise<void> {
    const stub = sessionStub(env);
    ctx.waitUntil(stub.fetch("https://session/renew-watch", { method: "POST" }));
    ctx.waitUntil(stub.fetch("https://session/pump", { method: "POST" }));
  },
} satisfies ExportedHandler<Env>;

async function handleDrivePush(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
  if (!(await isDriveWebhookAuthorized(request, env))) {
    return json({ ok: false, error: "unauthorized webhook" }, 401);
  }
  const headers = driveWebhookHeaders(request);
  const stub = sessionStub(env);
  ctx.waitUntil(stub.fetch("https://session/webhook-seen", { method: "POST" }));
  ctx.waitUntil(stub.fetch("https://session/pump", { method: "POST" }));
  return json({ ok: true, accepted: true, headers }, 202);
}

async function handleAdmin(request: Request, env: Env, url: URL): Promise<Response> {
  const stub = sessionStub(env);
  if (request.method === "GET" && url.pathname === "/admin/metrics") {
    return stub.fetch("https://session/metrics");
  }
  if (request.method === "POST" && url.pathname === "/admin/pump") {
    return stub.fetch("https://session/pump", { method: "POST" });
  }
  if (request.method === "POST" && url.pathname === "/admin/stop") {
    return stub.fetch("https://session/stop", { method: "POST" });
  }
  if (request.method === "POST" && url.pathname === "/admin/renew") {
    return stub.fetch("https://session/renew-watch", { method: "POST" });
  }
  if (request.method === "POST" && url.pathname === "/admin/watch") {
    const body = await readJson<Record<string, unknown>>(request);
    const address = typeof body.address === "string" && body.address.trim() ? body.address : `${url.origin}/drive/push`;
    const channelId = typeof body.channelId === "string" ? body.channelId : undefined;
    const expiration = typeof body.expiration === "number" ? body.expiration : undefined;
    return stub.fetch("https://session/watch", {
      method: "POST",
      body: JSON.stringify({ address, channelId, expiration }),
    });
  }
  return json({ ok: false, error: "not found" }, 404);
}

function sessionStub(env: Env): DurableObjectStub {
  const id = env.SESSIONS.idFromName(sessionName(env));
  return env.SESSIONS.get(id);
}

function sessionName(env: Env): string {
  return env.SKIRK_SESSION_ID?.trim() || "default";
}

async function readJson<T>(request: Request): Promise<T> {
  if (!request.headers.get("content-type")?.includes("application/json")) {
    return {} as T;
  }
  return (await request.json()) as T;
}

function json(payload: unknown, status = 200): Response {
  return new Response(JSON.stringify(payload, null, 2), {
    status,
    headers: { "content-type": "application/json; charset=utf-8" },
  });
}

function html(body: string, status = 200): Response {
  return new Response(body, {
    status,
    headers: {
      "content-type": "text/html; charset=utf-8",
      "cache-control": "no-store",
    },
  });
}

function homePage(env: Env, origin: string): string {
  const escapedSession = escapeHtml(sessionName(env));
  const escapedOrigin = escapeHtml(origin);
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Skirk Cloudflare Free Relay</title>
  <style>
    :root { color-scheme: light dark; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    body { margin: 0; padding: 40px 20px; background: Canvas; color: CanvasText; }
    main { max-width: 760px; margin: 0 auto; }
    h1 { margin: 0 0 8px; font-size: 30px; line-height: 1.15; }
    p { line-height: 1.55; }
    code { background: color-mix(in srgb, CanvasText 9%, Canvas); padding: 2px 5px; border-radius: 4px; }
    .panel { border: 1px solid color-mix(in srgb, CanvasText 18%, Canvas); border-radius: 8px; padding: 18px; margin: 18px 0; }
    .ok { color: #15803d; font-weight: 700; }
    .warn { color: #b45309; font-weight: 700; }
  </style>
</head>
<body>
  <main>
    <h1>Skirk Cloudflare Free Relay</h1>
    <p class="ok">Worker online.</p>
    <div class="panel">
      <p><strong>Session:</strong> <code>${escapedSession}</code></p>
      <p><strong>Origin:</strong> <code>${escapedOrigin}</code></p>
    </div>
    <div class="panel">
      <p>This free-tier mode is a Drive-backed web fetch relay. The client writes bounded HTTP request objects to Google Drive, this Worker fetches HTTP(S) URLs, and the client reads response objects from Drive.</p>
      <p>HTTPS browser traffic requires explicit local inspection mode in the client-side proxy. The Worker receives HTTP request semantics, not opaque TLS bytes.</p>
      <p class="warn">It is not a SOCKS/VPN replacement and does not support opaque HTTPS <code>CONNECT</code>.</p>
    </div>
    <p>Use <code>GET /health</code> for public health and <code>GET /admin/metrics</code> with the admin bearer token for metrics.</p>
  </main>
</body>
</html>`;
}

function escapeHtml(value: string): string {
  return value.replace(/[&<>"']/g, (char) => {
    switch (char) {
      case "&":
        return "&amp;";
      case "<":
        return "&lt;";
      case ">":
        return "&gt;";
      case '"':
        return "&quot;";
      default:
        return "&#39;";
    }
  });
}
