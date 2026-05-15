import asyncio
import os
import time
from datetime import datetime, timezone

from aiohttp import WSMsgType, web


STARTED_AT = datetime.now(timezone.utc).isoformat()


async def healthz(request: web.Request) -> web.Response:
    return web.Response(status=204)


async def root(request: web.Request) -> web.Response:
    return web.json_response(
        {
            "service": "skirk-cloudrun-probe",
            "started_at": STARTED_AT,
            "endpoints": ["/healthz", "/headers", "/stream", "/ws"],
        }
    )


async def headers(request: web.Request) -> web.Response:
    safe_headers = {
        key: value
        for key, value in request.headers.items()
        if key.lower() not in {"authorization", "cookie", "x-serverless-authorization"}
    }
    return web.json_response(
        {
            "method": request.method,
            "path": request.path_qs,
            "host": request.host,
            "scheme": request.scheme,
            "remote": request.remote,
            "headers": safe_headers,
            "time": datetime.now(timezone.utc).isoformat(),
        }
    )


async def stream(request: web.Request) -> web.StreamResponse:
    chunks = max(1, min(int(request.query.get("chunks", "5")), 30))
    delay_ms = max(0, min(int(request.query.get("delay_ms", "250")), 3000))
    payload = request.query.get("payload", "skirk")

    response = web.StreamResponse(
        status=200,
        headers={
            "content-type": "text/plain; charset=utf-8",
            "cache-control": "no-store",
            "x-skirk-stream-chunks": str(chunks),
            "x-skirk-stream-delay-ms": str(delay_ms),
        },
    )
    await response.prepare(request)

    for index in range(chunks):
        now = datetime.now(timezone.utc).isoformat()
        await response.write(f"{index}\t{now}\t{payload}\n".encode())
        await response.drain()
        if delay_ms:
            await asyncio.sleep(delay_ms / 1000)

    await response.write_eof()
    return response


async def websocket(request: web.Request) -> web.WebSocketResponse:
    ws = web.WebSocketResponse(heartbeat=20)
    await ws.prepare(request)

    await ws.send_json(
        {
            "type": "hello",
            "service": "skirk-cloudrun-probe",
            "time": datetime.now(timezone.utc).isoformat(),
        }
    )

    async for msg in ws:
        if msg.type == WSMsgType.TEXT:
            if msg.data == "close":
                await ws.close()
            else:
                await ws.send_json(
                    {
                        "type": "echo",
                        "data": msg.data,
                        "time": datetime.now(timezone.utc).isoformat(),
                    }
                )
        elif msg.type == WSMsgType.BINARY:
            await ws.send_bytes(msg.data)
        elif msg.type == WSMsgType.ERROR:
            break

    return ws


def create_app() -> web.Application:
    app = web.Application()
    app.add_routes(
        [
            web.get("/", root),
            web.get("/healthz", healthz),
            web.get("/headers", headers),
            web.get("/stream", stream),
            web.get("/ws", websocket),
        ]
    )
    return app


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8080"))
    web.run_app(create_app(), host="0.0.0.0", port=port)
