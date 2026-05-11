import { useEffect, useMemo, useState } from "react";
import {
  CheckCircle2,
  Copy,
  FileJson,
  Globe2,
  Network,
  LoaderCircle,
  Play,
  Power,
  RefreshCw,
  Shield,
  Trash2,
  Upload,
} from "lucide-react";

import { desktopApi, type ClientProfile, type DesktopSnapshot } from "./lib/api";
import logoMark from "./assets/logo-mark.png";

function App() {
  const [snapshot, setSnapshot] = useState<DesktopSnapshot | null>(null);
  const [rawConfig, setRawConfig] = useState("");
  const [profileName, setProfileName] = useState("Skirk profile");
  const [socksPort, setSocksPort] = useState(18080);
  const [shareLan, setShareLan] = useState(false);
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

  const connected = snapshot?.connection.phase === "connected";
  const activeProfile = snapshot?.profiles.find(
    (profile) => profile.id === snapshot.connection.activeProfileId,
  );

  return (
    <div className="shell">
      <aside>
        <div className="brand">
          <div className="mark">
            <img src={logoMark} alt="" />
          </div>
          <span>Skirk</span>
        </div>
        <nav>
          <a className="active" href="#profiles">
            Profiles
          </a>
          <a href="#runtime">Runtime</a>
          <a href="#logs">Logs</a>
        </nav>
      </aside>

      <main>
        <section className="page">
          <header>
            <div>
              <h1>Skirk Desktop</h1>
              <p>Import one profile, start the local proxy, then point apps at SOCKS5.</p>
            </div>
            <StatusBadge phase={snapshot?.connection.phase ?? "disconnected"} />
          </header>

          {error ? <div className="alert">{error}</div> : null}

          <div className="grid two" id="profiles">
            <section className="panel">
              <div className="panel-header">
                <div>
                  <h2>Import Config</h2>
                  <p>Paste a one-line config. Profiles and logs stay in portable data beside the app.</p>
                </div>
                <FileJson />
              </div>
              <label>
                Name
                <input value={profileName} onChange={(event) => setProfileName(event.target.value)} />
              </label>
              <label>
                SOCKS Port
                <input
                  type="number"
                  min={1024}
                  max={65535}
                  value={socksPort}
                  onChange={(event) => setSocksPort(Number(event.target.value))}
                />
              </label>
              <label>
                Client config
                <textarea value={rawConfig} onChange={(event) => setRawConfig(event.target.value)} />
              </label>
              <label className="check-row">
                <input
                  type="checkbox"
                  checked={shareLan}
                  onChange={(event) => setShareLan(event.target.checked)}
                />
                <span>
                  <strong>Share SOCKS on LAN</strong>
                  <small>Expose this Windows machine as a proxy for nearby devices.</small>
                </span>
              </label>
              <button
                className="primary"
                disabled={busy || rawConfig.trim() === ""}
                onClick={() =>
                  run(() => desktopApi.importConfig(profileName, rawConfig, socksPort, shareLan))
                }
              >
                <Upload />
                Import
              </button>
            </section>

            <section className="panel">
              <div className="panel-header">
                <div>
                  <h2>Saved Profiles</h2>
                  <p>Select a profile, then connect in proxy mode.</p>
                </div>
                <Shield />
              </div>
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
                  <div className="empty">No profiles yet</div>
                )}
              </div>
            </section>
          </div>

          <section className="panel" id="runtime">
            <div className="panel-header">
              <div>
                <h2>Runtime</h2>
                <p>Windows currently runs Skirk as a local SOCKS5 sidecar.</p>
              </div>
              <button className="ghost" onClick={() => void refresh()}>
                <RefreshCw />
              </button>
            </div>
            <div className="metrics">
              <Metric label="Mode" value="Proxy" />
              <Metric label="SOCKS" value={snapshot?.connection.socksAddress ?? selectedProfileAddress(selectedProfile)} />
              <Metric label="LAN" value={snapshot?.connection.lanAddresses.join(", ") || "-"} />
              <Metric label="PID" value={snapshot?.connection.pid?.toString() ?? "-"} />
            </div>
            <div className="mode-note">
              <Globe2 />
              <span>VPN mode is available in the Android client. The Windows desktop build is intentionally proxy-first until a proper Wintun driver flow is added.</span>
            </div>
            <div className="actions">
              <button
                className="primary"
                disabled={busy || connected || !selectedProfile}
                onClick={() => run(() => desktopApi.connect())}
              >
                {busy ? <LoaderCircle className="spin" /> : <Play />}
                Connect
              </button>
              <button
                disabled={busy || !connected}
                onClick={() => run(() => desktopApi.disconnect())}
              >
                <Power />
                Disconnect
              </button>
              <button
                disabled={!selectedProfile}
                onClick={() =>
                  copyText(
                    snapshot?.connection.socksAddress ??
                      selectedProfileAddress(selectedProfile),
                  )
                }
              >
                <Copy />
                Copy SOCKS
              </button>
            </div>
            <p>{snapshot?.connection.message || runtimeMessage(connected, activeProfile)}</p>
          </section>

          <section className="panel" id="logs">
            <div className="panel-header">
              <div>
                <h2>Logs</h2>
                <p>{snapshot?.logsDir ?? "Runtime logs will appear after connect."}</p>
              </div>
            </div>
            <pre>{snapshot?.logTail || "No log output yet."}</pre>
          </section>
        </section>
      </main>
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
    <div className={selected ? "profile selected" : "profile"}>
      <button disabled={disabled} onClick={onSelect}>
        <div>
          <strong>{profile.name}</strong>
          <span>
            {profile.routeMode} · {selectedProfileAddress(profile)}
            {profile.shareLan ? " · LAN" : ""}
          </span>
        </div>
        {selected ? <CheckCircle2 /> : profile.shareLan ? <Network /> : null}
      </button>
      <button className="icon" disabled={disabled} onClick={onDelete}>
        <Trash2 />
      </button>
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

function StatusBadge({ phase }: { phase: string }) {
  return <div className={`badge ${phase}`}>{phase}</div>;
}

function selectedProfileAddress(profile: ClientProfile | null) {
  if (!profile) {
    return "-";
  }
  return `${profile.shareLan ? "0.0.0.0" : "127.0.0.1"}:${profile.socksPort}`;
}

function runtimeMessage(connected: boolean, profile?: ClientProfile) {
  if (connected && profile) {
    return `Connected using ${profile.name}. Configure apps for SOCKS5 ${selectedProfileAddress(profile)}.`;
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
