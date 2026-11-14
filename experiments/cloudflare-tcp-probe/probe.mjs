// Usage: node probe.mjs https://temporary-worker TOKEN_JSON OUTPUT_JSON
// The secret file contains {"PROBE_TOKEN":"..."}. No secret is printed.
import { readFile, writeFile } from "node:fs/promises";
import { Duplex } from "node:stream";
import tls from "node:tls";

const [endpoint, secretFile, output] = process.argv.slice(2);
if (!endpoint || !secretFile || !output) throw new Error("requires endpoint, secret JSON path, output JSON path");
const { PROBE_TOKEN: token } = JSON.parse(await readFile(secretFile, "utf8"));
const targets = { google: "www.google.com", github: "github.com", cloudflare: "www.cloudflare.com" };
const rows = [];
for (const target of Object.keys(targets)) {
  const response = await fetch(`${endpoint}/probe/${target}`, {
    headers: { Authorization: `Bearer ${token}` }, signal: AbortSignal.timeout(12000),
  });
  rows.push({ kind: "tcp_open", http_status: response.status, ...await response.json() });
}

for (const target of ["google", "github"]) {
  const started = performance.now();
  const row = await new Promise(resolve => {
    const ws = new WebSocket(`${endpoint.replace(/^http/, "ws")}/tunnel/${target}`, ["skirk-probe", token]);
    ws.binaryType = "arraybuffer";
    let secured, transport, finished = false, uploaded = 0, downloaded = 0, closeReason = "";
    const timer = setTimeout(() => finish({ ok: false, failure: "timeout" }), 18000);
    function finish(result) {
      if (finished) return;
      finished = true;
      clearTimeout(timer);
      secured?.destroy();
      transport?.destroy();
      if (ws.readyState === WebSocket.OPEN) ws.close();
      resolve({ kind: "opaque_tls", target, elapsed_ms: performance.now() - started, uploaded, downloaded, close_reason: closeReason, ...result });
    }
    ws.addEventListener("error", () => finish({ ok: false, failure: "websocket" }));
    ws.addEventListener("close", event => { closeReason = event.reason; if (!finished) transport?.push(null); });
    ws.addEventListener("open", () => {
      transport = new Duplex({
        read() {},
        write(chunk, _encoding, done) {
          try { uploaded += chunk.length; ws.send(chunk); done(); } catch (error) { done(error); }
        },
      });
      ws.addEventListener("message", event => { const data = Buffer.from(event.data); downloaded += data.length; transport.push(data); });
      secured = tls.connect({ socket: transport, servername: targets[target], rejectUnauthorized: true, ALPNProtocols: ["http/1.1"] });
      secured.once("secureConnect", () => secured.write(`HEAD /robots.txt HTTP/1.1\r\nHost: ${targets[target]}\r\nConnection: close\r\n\r\n`));
      let headers = "";
      secured.on("data", chunk => {
        headers += chunk.toString("latin1");
        if (headers.length > 16384) return finish({ ok: false, failure: "header_limit" });
        if (headers.includes("\r\n\r\n")) finish({ ok: /^HTTP\/1\.[01] \d{3}/.test(headers), tls_authorized: secured.authorized,
          http_status: Number(headers.split(" ")[1]), tls_version: secured.getProtocol() });
      });
      secured.once("error", error => finish({ ok: false, failure: error.code || "tls" }));
      secured.once("end", () => { if (!finished) finish({ ok: false, failure: "early_eof" }); });
    });
  });
  rows.push(row);
}
await writeFile(output, JSON.stringify(rows, null, 2) + "\n", { mode: 0o600, flag: "wx" });
console.log(JSON.stringify(rows));
