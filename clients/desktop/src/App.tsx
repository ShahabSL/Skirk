import { useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import {
  Check,
  ClipboardPaste,
  Copy,
  HardDrive,
  Laptop,
  Loader2,
  Moon,
  Play,
  Power,
  RefreshCw,
  Server,
  Shield,
  Sun,
  Trash2,
  Upload,
  Wifi,
} from "lucide-react";

import { desktopApi, type ClientProfile, type DesktopSnapshot } from "./lib/api";
import logoMark from "./assets/logo-mark.png";

type Theme = "light" | "dark";

function App() {
  const [snapshot, setSnapshot] = useState<DesktopSnapshot | null>(null);
  const [rawConfig, setRawConfig] = useState("");
  const [profileName, setProfileName] = useState("Skirk profile");
  const [socksPort, setSocksPort] = useState("18080");
  const [shareLan, setShareLan] = useState(false);
  const [theme, setTheme] = useState<Theme>(() =>
    window.localStorage.getItem("skirk-theme") === "dark" ? "dark" : "light",
  );
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function refresh() {
    try {
      setSnapshot(await desktopApi.loadSnapshot());
      setError("");
    } catch (nextError) {
      setError(normalizeError(nextError));
    }
  }

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    window.localStorage.setItem("skirk-theme", theme);
  }, [theme]);

  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => void refresh(), 1500);
    return () => window.clearInterval(timer);
  }, []);

  const selectedProfile = useMemo(() => {
    if (!snapshot) {
      return null;
    }
    return (
      snapshot.profiles.find((profile) => profile.id === snapshot.selectedProfileId) ??
      snapshot.profiles[0] ??
      null
    );
  }, [snapshot]);

  const activeProfile = snapshot?.profiles.find(
    (profile) => profile.id === snapshot.connection.activeProfileId,
  );
  const connected = snapshot?.connection.phase === "connected";
  const connecting = snapshot?.connection.phase === "connecting";
  const portNumber = Number(socksPort);
  const portValid = Number.isInteger(portNumber) && portNumber >= 1024 && portNumber <= 65535;
  const importDisabled = busy || rawConfig.trim() === "" || !portValid;

  async function run(action: () => Promise<DesktopSnapshot>) {
    setBusy(true);
    try {
      setSnapshot(await action());
      setError("");
    } catch (nextError) {
      setError(normalizeError(nextError));
      await refresh();
    } finally {
      setBusy(false);
    }
  }

  async function pasteConfig() {
    try {
      const text = await navigator.clipboard.readText();
      setRawConfig(text);
      setError("");
    } catch (nextError) {
      setError(normalizeError(nextError));
    }
  }

  return (
    <div className="app-shell">
      <aside className="sidebar">
        <div className="brand-block">
          <div className="brand-mark">
            <img src={logoMark} alt="" />
          </div>
          <div>
            <strong>Skirk</strong>
            <span>Desktop client</span>
          </div>
        </div>

        <StatusCard
          phase={snapshot?.connection.phase ?? "disconnected"}
          address={snapshot?.connection.socksAddress ?? selectedProfileAddress(selectedProfile)}
        />

        <nav className="side-nav" aria-label="Skirk sections">
          <a href="#runtime">Runtime</a>
          <a href="#profiles">Profiles</a>
          <a href="#logs">Logs</a>
        </nav>

        <button
          className="icon-line"
          onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
        >
          {theme === "dark" ? <Sun /> : <Moon />}
          {theme === "dark" ? "Light theme" : "Dark theme"}
        </button>
      </aside>

      <main className="workspace">
        <header className="workspace-header">
          <div>
            <span className="eyebrow">Windows SOCKS5 runtime</span>
            <h1>Profiles, proxy, logs.</h1>
          </div>
          <div className="header-actions">
            <button className="icon-button" onClick={() => void refresh()} title="Refresh">
              <RefreshCw />
            </button>
            <PhaseBadge phase={snapshot?.connection.phase ?? "disconnected"} />
          </div>
        </header>

        {error ? <div className="alert">{error}</div> : null}

        <section className="runtime-strip" id="runtime">
          <Metric label="Active" value={activeProfile?.name ?? selectedProfile?.name ?? "None"} />
          <Metric
            label="SOCKS"
            value={snapshot?.connection.socksAddress ?? selectedProfileAddress(selectedProfile)}
          />
          <Metric label="LAN" value={snapshot?.connection.lanAddresses.join(", ") || "-"} />
          <Metric label="PID" value={snapshot?.connection.pid?.toString() ?? "-"} />
        </section>

        <section className="action-bar">
          <button
            className="primary"
            disabled={busy || connected || !selectedProfile}
            onClick={() => run(() => desktopApi.connect())}
          >
            {busy || connecting ? <Loader2 className="spin" /> : <Play />}
            Connect
          </button>
          <button disabled={busy || !connected} onClick={() => run(() => desktopApi.disconnect())}>
            <Power />
            Disconnect
          </button>
          <button
            disabled={!selectedProfile}
            onClick={() =>
              copyText(snapshot?.connection.socksAddress ?? selectedProfileAddress(selectedProfile))
            }
          >
            <Copy />
            Copy SOCKS
          </button>
          <span className="runtime-message">
            {snapshot?.connection.message || runtimeMessage(connected, activeProfile)}
          </span>
        </section>

        <div className="content-grid">
          <section className="panel import-panel">
            <SectionTitle
              icon={<Upload />}
              title="Import"
              detail={portValid ? "Ready" : "Port must be 1024-65535"}
            />

            <label>
              <span>Name</span>
              <input value={profileName} onChange={(event) => setProfileName(event.target.value)} />
            </label>

            <label>
              <span>SOCKS port</span>
              <input
                inputMode="numeric"
                value={socksPort}
                onChange={(event) => setSocksPort(event.target.value.replace(/\D/g, "").slice(0, 5))}
              />
            </label>

            <label>
              <span>Client profile text</span>
              <textarea
                value={rawConfig}
                onChange={(event) => setRawConfig(event.target.value)}
                spellCheck={false}
              />
            </label>

            <label className="switch-row">
              <input
                type="checkbox"
                checked={shareLan}
                onChange={(event) => setShareLan(event.target.checked)}
              />
              <span>
                <strong>Share on LAN</strong>
                <small>Listen on 0.0.0.0 instead of loopback.</small>
              </span>
            </label>

            <div className="button-row">
              <button className="primary" disabled={importDisabled} onClick={() =>
                run(() => desktopApi.importConfig(profileName, rawConfig, portNumber, shareLan))
              }>
                <Upload />
                Import profile
              </button>
              <button type="button" onClick={() => void pasteConfig()}>
                <ClipboardPaste />
                Paste
              </button>
            </div>
          </section>

          <section className="panel" id="profiles">
            <SectionTitle
              icon={<Shield />}
              title="Profiles"
              detail={`${snapshot?.profiles.length ?? 0} saved`}
            />
            <div className="profile-list">
              {snapshot?.profiles.length ? (
                snapshot.profiles.map((profile) => (
                  <ProfileRow
                    key={profile.id}
                    profile={profile}
                    selected={profile.id === selectedProfile?.id}
                    disabled={busy || connected}
                    onSelect={() => run(() => desktopApi.selectProfile(profile.id))}
                    onDelete={() => run(() => desktopApi.deleteProfile(profile.id))}
                  />
                ))
              ) : (
                <div className="empty-state">
                  <HardDrive />
                  <span>No profiles imported.</span>
                </div>
              )}
            </div>
          </section>

          <section className="panel runtime-panel">
            <SectionTitle icon={<Server />} title="Runtime" detail={snapshot?.platform ?? "-"} />
            <div className="runtime-copy">
              <div>
                <Laptop />
                <span>Windows runs the packaged Skirk sidecar as a SOCKS5 proxy.</span>
              </div>
              <div>
                <Wifi />
                <span>Android provides proxy and VPN modes from the same profile text.</span>
              </div>
            </div>
          </section>

          <section className="panel logs-panel" id="logs">
            <SectionTitle icon={<HardDrive />} title="Logs" detail={snapshot?.logsDir ?? "-"} />
            <pre>{snapshot?.logTail || "No log output yet."}</pre>
          </section>
        </div>
      </main>
    </div>
  );
}

function SectionTitle({
  icon,
  title,
  detail,
}: {
  icon: ReactNode;
  title: string;
  detail: string;
}) {
  return (
    <div className="section-title">
      <div>
        {icon}
        <h2>{title}</h2>
      </div>
      <span>{detail}</span>
    </div>
  );
}

function ProfileRow({
  profile,
  selected,
  disabled,
  onSelect,
  onDelete,
}: {
  profile: ClientProfile;
  selected: boolean;
  disabled: boolean;
  onSelect: () => void;
  onDelete: () => void;
}) {
  return (
    <div className={selected ? "profile-row selected" : "profile-row"}>
      <button
        disabled={disabled}
        onClick={() => {
          if (!selected) {
            onSelect();
          }
        }}
      >
        <span className="profile-name">
          {selected ? <Check /> : <Shield />}
          {profile.name}
        </span>
        <span>
          {profile.routeMode} · {selectedProfileAddress(profile)}
          {profile.shareLan ? " · LAN" : ""}
        </span>
      </button>
      <button className="icon-button" disabled={disabled} onClick={onDelete} title="Delete profile">
        <Trash2 />
      </button>
    </div>
  );
}

function StatusCard({ phase, address }: { phase: string; address: string }) {
  return (
    <div className="status-card">
      <span>Status</span>
      <strong>{phase}</strong>
      <small>{address}</small>
    </div>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="metric">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function PhaseBadge({ phase }: { phase: string }) {
  return <div className={`phase-badge ${phase}`}>{phase}</div>;
}

function selectedProfileAddress(profile: ClientProfile | null) {
  if (!profile) {
    return "-";
  }
  return `${profile.shareLan ? "0.0.0.0" : "127.0.0.1"}:${profile.socksPort}`;
}

function runtimeMessage(connected: boolean, profile?: ClientProfile) {
  if (connected && profile) {
    return `Connected with ${profile.name}.`;
  }
  return "Disconnected.";
}

function normalizeError(value: unknown) {
  if (value instanceof Error) {
    return value.message;
  }
  return String(value);
}

async function copyText(value: string) {
  await navigator.clipboard.writeText(value);
}

export default App;
