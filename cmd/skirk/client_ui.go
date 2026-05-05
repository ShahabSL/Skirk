package main

import (
	"context"
	"encoding/json"
	"flag"
	"html/template"
	"log"
	"net/http"
	"time"

	"skirk/internal/skirk"
)

type clientUIState struct {
	ConfigPath string `json:"config_path"`
	SOCKS      string `json:"socks"`
	UI         string `json:"ui"`
	RouteMode  string `json:"route_mode"`
	GoogleIP   string `json:"google_ip"`
	SessionID  string `json:"session_id"`
	StartedAt  string `json:"started_at"`
}

func clientUI(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("client-ui", flag.ExitOnError)
	configPath := fs.String("config", "skirk-kit/client.json", "config path")
	socksAddr := fs.String("socks", "", "SOCKS5 listen address")
	uiAddr := fs.String("ui", "127.0.0.1:18280", "local UI listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, drive, sheets, _, err := load(*configPath)
	if err != nil {
		return err
	}
	tunnel, err := skirk.NewTunnel(drive, controlStore(drive, sheets, cfg), cfg)
	if err != nil {
		return err
	}
	socks := firstNonEmpty(*socksAddr, cfg.Tunnel.Listen)
	state := clientUIState{
		ConfigPath: *configPath,
		SOCKS:      socks,
		UI:         *uiAddr,
		RouteMode:  cfg.Route.Mode,
		GoogleIP:   cfg.Route.GoogleIP,
		SessionID:  skirk.SessionString(tunnel.SessionID),
		StartedAt:  time.Now().Format(time.RFC3339),
	}

	errCh := make(chan error, 2)
	go func() {
		log.Printf("skirk client SOCKS5 listening on %s session=%s", socks, state.SessionID)
		errCh <- tunnel.ServeClient(ctx, socks)
	}()
	server := &http.Server{Addr: *uiAddr, Handler: clientUIHandler(state)}
	go func() {
		log.Printf("skirk client UI listening on http://%s", *uiAddr)
		err := server.ListenAndServe()
		if err == http.ErrServerClosed {
			err = nil
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return err
	}
}

func clientUIHandler(state clientUIState) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = clientUITemplate.Execute(w, state)
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state)
	})
	return mux
}

var clientUITemplate = template.Must(template.New("client-ui").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Skirk Client</title>
  <style>
    :root {
      color-scheme: light dark;
      --background: #f7f7f4;
      --foreground: #171717;
      --card: #ffffff;
      --muted: #ededeb;
      --muted-foreground: #666660;
      --border: #deded8;
      --primary: #111111;
      --primary-foreground: #ffffff;
      --accent: #e8efe7;
      --radius: 8px;
      font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }
    @media (prefers-color-scheme: dark) {
      :root {
        --background: #101010;
        --foreground: #f4f4f0;
        --card: #181818;
        --muted: #222222;
        --muted-foreground: #a3a39b;
        --border: #30302d;
        --primary: #f3f3ef;
        --primary-foreground: #101010;
        --accent: #1d2a22;
      }
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      min-height: 100vh;
      background: var(--background);
      color: var(--foreground);
      letter-spacing: 0;
    }
    .shell {
      display: grid;
      grid-template-columns: 280px minmax(0, 1fr);
      min-height: 100vh;
    }
    aside {
      border-right: 1px solid var(--border);
      padding: 24px;
      background: color-mix(in srgb, var(--card) 74%, transparent);
    }
    main { padding: 32px; }
    .brand {
      display: flex;
      align-items: center;
      gap: 10px;
      font-weight: 650;
      font-size: 15px;
      margin-bottom: 28px;
    }
    .mark {
      width: 28px;
      height: 28px;
      border-radius: 7px;
      background: var(--foreground);
      color: var(--background);
      display: grid;
      place-items: center;
      font-size: 13px;
    }
    nav {
      display: flex;
      flex-direction: column;
      gap: 6px;
    }
    nav a {
      color: var(--muted-foreground);
      text-decoration: none;
      padding: 9px 10px;
      border-radius: var(--radius);
      font-size: 14px;
    }
    nav a.active {
      color: var(--foreground);
      background: var(--muted);
    }
    .page {
      max-width: 980px;
      display: flex;
      flex-direction: column;
      gap: 20px;
    }
    header {
      display: flex;
      justify-content: space-between;
      align-items: flex-start;
      gap: 16px;
    }
    h1 {
      font-size: 28px;
      line-height: 1.15;
      margin: 0 0 8px;
      font-weight: 650;
    }
    p {
      margin: 0;
      color: var(--muted-foreground);
      font-size: 14px;
      line-height: 1.55;
    }
    .badge {
      border: 1px solid var(--border);
      border-radius: 999px;
      padding: 6px 10px;
      font-size: 13px;
      background: var(--accent);
      white-space: nowrap;
    }
    .grid {
      display: grid;
      grid-template-columns: repeat(3, minmax(0, 1fr));
      gap: 12px;
    }
    .card {
      border: 1px solid var(--border);
      border-radius: var(--radius);
      background: var(--card);
      padding: 18px;
      min-height: 104px;
    }
    .label {
      color: var(--muted-foreground);
      font-size: 12px;
      margin-bottom: 10px;
    }
    .value {
      font-size: 15px;
      line-height: 1.45;
      overflow-wrap: anywhere;
    }
    code {
      font-family: ui-monospace, "SFMono-Regular", Consolas, monospace;
      font-size: 13px;
      background: var(--muted);
      border: 1px solid var(--border);
      border-radius: 6px;
      padding: 2px 5px;
    }
    .command {
      border: 1px solid var(--border);
      border-radius: var(--radius);
      background: var(--card);
      padding: 16px;
      display: flex;
      flex-direction: column;
      gap: 10px;
    }
    .command code {
      display: block;
      padding: 12px;
      overflow-x: auto;
    }
    @media (max-width: 760px) {
      .shell { grid-template-columns: 1fr; }
      aside { border-right: 0; border-bottom: 1px solid var(--border); }
      main { padding: 20px; }
      header { flex-direction: column; }
      .grid { grid-template-columns: 1fr; }
    }
  </style>
</head>
<body>
  <div class="shell">
    <aside>
      <div class="brand"><div class="mark">S</div><span>Skirk Client</span></div>
      <nav>
        <a class="active" href="/">Status</a>
        <a href="/api/status">API</a>
      </nav>
    </aside>
    <main>
      <section class="page">
        <header>
          <div>
            <h1>SOCKS proxy is running</h1>
            <p>Point apps at the local SOCKS endpoint. DNS should use SOCKS hostname mode where available.</p>
          </div>
          <div class="badge">Connected session</div>
        </header>
        <div class="grid">
          <div class="card"><div class="label">SOCKS</div><div class="value">{{.SOCKS}}</div></div>
          <div class="card"><div class="label">Route</div><div class="value">{{.RouteMode}}</div></div>
          <div class="card"><div class="label">Google IP</div><div class="value">{{.GoogleIP}}</div></div>
        </div>
        <div class="grid">
          <div class="card"><div class="label">Config</div><div class="value">{{.ConfigPath}}</div></div>
          <div class="card"><div class="label">Session</div><div class="value">{{.SessionID}}</div></div>
          <div class="card"><div class="label">Started</div><div class="value">{{.StartedAt}}</div></div>
        </div>
        <div class="command">
          <p>Test command</p>
          <code>curl --socks5-hostname {{.SOCKS}} http://example.com/</code>
        </div>
      </section>
    </main>
  </div>
</body>
</html>`))
