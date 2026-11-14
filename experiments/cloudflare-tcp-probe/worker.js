// Disposable, authenticated capability probe. Never a general-purpose relay.
import { connect } from "cloudflare:sockets";

const targets = { google: "www.google.com", github: "github.com", cloudflare: "www.cloudflare.com" };

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    const [, action, target] = url.pathname.split("/");
    const supplied = request.headers.get("Authorization")?.replace(/^Bearer /, "") ||
      request.headers.get("Sec-WebSocket-Protocol")?.split(",").map(s => s.trim())[1] || "";
    const encoder = new TextEncoder();
    const expected = encoder.encode(env.PROBE_TOKEN || "");
    const actual = encoder.encode(supplied);
    if (!expected.length || actual.length !== expected.length ||
        !crypto.subtle.timingSafeEqual(actual, expected) || Date.now() > Number(env.EXPIRES_AT || 0)) {
      return new Response("Forbidden", { status: 403 });
    }
    const hostname = Object.hasOwn(targets, target) ? targets[target] : null;
    if (request.method !== "GET" || !hostname || !["probe", "tunnel"].includes(action)) {
      return new Response("Not found", { status: 404 });
    }
    if (action === "tunnel" && request.headers.get("Upgrade")?.toLowerCase() !== "websocket") {
      return new Response("WebSocket required", { status: 400 });
    }
    const started = performance.now();
    const socket = connect({ hostname, port: 443 }, { secureTransport: "off" });
    void socket.closed.catch(() => {});
    let timer = setTimeout(() => { void socket.close().catch(() => {}); }, 8000);
    try {
      await socket.opened;
    } catch (error) {
      clearTimeout(timer);
      await socket.close().catch(() => {});
      return Response.json({ target, connected: false, error: String(error).slice(0, 300) }, { status: 502 });
    }
    clearTimeout(timer);
    if (action === "probe") {
      await socket.close().catch(() => {});
      return Response.json({ target, connected: true, connect_ms: performance.now() - started });
    }
    const [client, server] = Object.values(new WebSocketPair());
    server.binaryType = "arraybuffer";
    server.accept();
    const writer = socket.writable.getWriter();
    const reader = socket.readable.getReader();
    let stopped = false, uploaded = 0, downloaded = 0;
    let writes = Promise.resolve();
    const close = () => {
      if (stopped) return;
      stopped = true;
      clearTimeout(timer);
      try { server.close(1000, `probe complete up=${uploaded} down=${downloaded}`); } catch {}
      void socket.close().catch(() => {});
    };
    timer = setTimeout(close, 15000);
    server.addEventListener("message", event => {
      if (stopped || typeof event.data === "string") return close();
      const bytes = new Uint8Array(event.data);
      uploaded += bytes.byteLength;
      if (uploaded > 64 * 1024) return close();
      writes = writes.then(() => writer.write(bytes)).catch(close);
    });
    server.addEventListener("close", close);
    server.addEventListener("error", close);
    ctx.waitUntil((async () => {
      try {
        while (!stopped) {
          const { value, done } = await reader.read();
          if (done) break;
          downloaded += value.byteLength;
          if (downloaded > 128 * 1024) break;
          server.send(value);
        }
      } catch {} finally { close(); }
    })());
    return new Response(null, { status: 101, webSocket: client, headers: { "Sec-WebSocket-Protocol": "skirk-probe" } });
  }
};
