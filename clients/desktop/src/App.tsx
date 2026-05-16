import { useCallback, useEffect, useId, useMemo, useState } from "react";
import type { ReactNode } from "react";
import {
  Check,
  ClipboardPaste,
  Copy,
  HardDrive,
  Laptop,
  Loader2,
  MonitorCog,
  Moon,
  Network,
  Play,
  Power,
  RefreshCw,
  Server,
  Shield,
  ShieldCheck,
  Sun,
  Trash2,
  Upload,
} from "lucide-react";

import { desktopApi, type ClientProfile, type ConnectionMode, type DesktopSnapshot } from "./lib/api";
import logoMark from "./assets/logo-mark.png";

type Theme = "light" | "dark";
type BusyAction = "connect" | "disconnect" | "import" | "select" | "delete" | "mode";

function App() {
  const [snapshot, setSnapshot] = useState<DesktopSnapshot | null>(null);
  const [rawConfig, setRawConfig] = useState("");
  const [profileName, setProfileName] = useState("Skirk profile");
  const [socksPort, setSocksPort] = useState("18080");
  const [httpPort, setHttpPort] = useState("18081");
  const [shareLan, setShareLan] = useState(false);
  const [theme, setTheme] = useState<Theme>(() =>
    window.localStorage.getItem("skirk-theme") === "dark" ? "dark" : "light",
  );
  const [error, setError] = useState("");
  const [busyAction, setBusyAction] = useState<BusyAction | null>(null);
  const [copyStatus, setCopyStatus] = useState("");
  const profileNameId = useId();
  const socksPortId = useId();
  const httpPortId = useId();
  const socksPortHelpId = useId();
  const httpPortHelpId = useId();
  const rawConfigId = useId();

  const refresh = useCallback(async () => {
    try {
      setSnapshot(await desktopApi.loadSnapshot());
      setError("");
    } catch (nextError) {
      setError(normalizeError(nextError));
    }
  }, []);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    window.localStorage.setItem("skirk-theme", theme);
  }, [theme]);

  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => void refresh(), 1500);
    return () => window.clearInterval(timer);
  }, [refresh]);

  useEffect(() => {
    if (!copyStatus) {
      return;
    }
    const timer = window.setTimeout(() => setCopyStatus(""), 1800);
    return () => window.clearTimeout(timer);
  }, [copyStatus]);

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
  const disconnecting = snapshot?.connection.phase === "disconnecting";
  const disconnectAvailable = connected || disconnecting;
  const initialLoading = snapshot === null && error === "";
  const busy = busyAction !== null;
  const runtimeBusy = busy || connecting || disconnecting;
  const portNumber = Number(socksPort);
  const httpPortNumber = Number(httpPort);
  const portValid = Number.isInteger(portNumber) && portNumber >= 1024 && portNumber <= 65535;
  const httpPortValid = Number.isInteger(httpPortNumber) && httpPortNumber >= 1024 && httpPortNumber <= 65535 && httpPortNumber !== portNumber;
  const importDisabled = busy || rawConfig.trim() === "" || !portValid || !httpPortValid;
  const phase = snapshot?.connection.phase ?? (initialLoading ? "loading" : "disconnected");
  const socksAddress = snapshot?.connection.socksAddress ?? selectedProfileAddress(selectedProfile);
  const httpAddress = snapshot?.connection.httpAddress ?? selectedProfileHTTPAddress(selectedProfile);
  const selectedMode = snapshot?.connection.mode ?? "proxy";
  const vpnNeedsAdmin =
    selectedMode === "vpn" &&
    Boolean(snapshot?.capabilities.vpnRequiresAdmin) &&
    !snapshot?.capabilities.vpnAdmin;
  const runtimeProfile = activeProfile ?? selectedProfile;
  const profileStatusLabel = activeProfile
    ? "Active profile"
    : selectedProfile
      ? "Selected profile"
      : "Profile";
  const profileDetail = initialLoading
    ? "Checking saved profiles..."
    : runtimeProfile
      ? `${runtimeProfile.routeMode} · ${selectedProfileAddress(runtimeProfile)}`
      : "Import a profile to enable Connect.";
  const lanAddressValue = initialLoading
    ? "Loading..."
    : snapshot?.connection.lanAddresses.join(", ") || "-";
  const endpointValue = initialLoading ? "Loading..." : socksAddress;
  const httpEndpointValue = initialLoading ? "Loading..." : httpAddress;
  const copyDisabled = !selectedProfile || socksAddress === "-";
  const runtimeStatusMessage =
    copyStatus ||
    (initialLoading
      ? "Loading runtime status..."
      : vpnNeedsAdmin
        ? "VPN mode needs Administrator privileges. Close Skirk and open Skirk.exe with Run as administrator."
      : snapshot?.connection.message || runtimeMessage(connected, activeProfile));

  async function run(actionName: BusyAction, action: () => Promise<DesktopSnapshot>) {
    setBusyAction(actionName);
    try {
      setSnapshot(await action());
      setError("");
    } catch (nextError) {
      setError(normalizeError(nextError));
      await refresh();
    } finally {
      setBusyAction(null);
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

  async function copySocksAddress() {
    if (socksAddress === "-") {
      return;
    }
    try {
      await copyText(socksAddress);
      setCopyStatus("SOCKS address copied.");
      setError("");
    } catch (nextError) {
      setCopyStatus("");
      setError(normalizeError(nextError));
    }
  }

  async function changeMode(mode: ConnectionMode) {
    if (mode === selectedMode || runtimeBusy) {
      return;
    }
    await run("mode", () => desktopApi.setConnectionMode(mode));
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
          phase={phase}
          address={initialLoading ? "Loading..." : socksAddress}
        />

        <nav className="side-nav" aria-label="Skirk sections">
          <a href="#runtime">Runtime</a>
          <a href="#profiles">Profiles</a>
          <a href="#import">Import</a>
          <a href="#logs">Logs</a>
        </nav>

        <button
          type="button"
          className="icon-line"
          aria-pressed={theme === "dark"}
          aria-label={theme === "dark" ? "Switch to light theme" : "Switch to dark theme"}
          title={theme === "dark" ? "Switch to light theme" : "Switch to dark theme"}
          onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
        >
          {theme === "dark" ? <Sun /> : <Moon />}
          {theme === "dark" ? "Light theme" : "Dark theme"}
        </button>
      </aside>

      <main className="workspace">
        <header className="workspace-header">
          <div>
            <span className="eyebrow">Skirk Desktop</span>
            <h1>Connection console</h1>
          </div>
          <div className="header-actions">
            <button
              type="button"
              className="icon-button"
              onClick={() => void refresh()}
              aria-label="Refresh status"
              title="Refresh status"
            >
              <RefreshCw className={initialLoading ? "spin" : undefined} aria-hidden="true" />
            </button>
            <PhaseBadge phase={phase} />
          </div>
        </header>

        {error ? (
          <div className="alert" role="alert">
            {error}
          </div>
        ) : null}

        <section className={`control-surface ${phase}`} id="runtime" aria-labelledby="runtime-title">
          <div className="control-main">
            <div className={`status-indicator ${phase}`} aria-hidden="true">
              <span />
            </div>
            <div className="control-copy">
              <span className="eyebrow">Connection status</span>
              <h2 id="runtime-title">{statusTitle(phase)}</h2>
              <p aria-live="polite">{runtimeStatusMessage}</p>
            </div>
          </div>

          <div className="profile-summary" aria-label="Profile in use">
            <span>{profileStatusLabel}</span>
            <strong>{initialLoading ? "Loading..." : runtimeProfile?.name ?? "No profile selected"}</strong>
            <small>{profileDetail}</small>
          </div>

          <div className="mode-selector" aria-label="Connection mode">
            <ModeButton
              active={selectedMode === "proxy"}
              disabled={runtimeBusy || connected}
              icon={<Network />}
              label="Proxy"
              onClick={() => void changeMode("proxy")}
            />
            <ModeButton
              active={selectedMode === "system"}
              disabled={runtimeBusy || connected || !snapshot?.capabilities.systemProxySupported}
              icon={<MonitorCog />}
              label="System proxy"
              onClick={() => void changeMode("system")}
            />
            <ModeButton
              active={selectedMode === "vpn"}
              disabled={runtimeBusy || connected || !snapshot?.capabilities.vpnModeSupported}
              icon={<ShieldCheck />}
              label="VPN"
              onClick={() => void changeMode("vpn")}
            />
          </div>

          <div className="command-row" aria-label="Connection actions">
            {disconnectAvailable ? (
              <button
                type="button"
                className="primary"
                disabled={busy}
                onClick={() => void run("disconnect", () => desktopApi.disconnect())}
              >
                {busyAction === "disconnect" || disconnecting ? <Loader2 className="spin" /> : <Power />}
                Disconnect
              </button>
            ) : (
              <button
                type="button"
                className="primary"
                disabled={runtimeBusy || !selectedProfile || vpnNeedsAdmin}
                onClick={() => void run("connect", () => desktopApi.connect())}
              >
                {busyAction === "connect" || connecting ? (
                  <Loader2 className="spin" />
                ) : vpnNeedsAdmin ? (
                  <ShieldCheck />
                ) : (
                  <Play />
                )}
                {vpnNeedsAdmin ? "Run as administrator" : "Connect"}
              </button>
            )}
            <button
              type="button"
              disabled={copyDisabled}
              onClick={() => void copySocksAddress()}
            >
              {copyStatus ? <Check /> : <Copy />}
              {copyStatus ? "Copied" : "Copy SOCKS"}
            </button>
          </div>

          <div className="metric-grid" aria-label="Runtime details">
            <Metric label="SOCKS endpoint" value={endpointValue} />
            <Metric label="HTTP endpoint" value={httpEndpointValue} />
            <Metric label="LAN endpoints" value={lanAddressValue} />
            <Metric label="Runtime" value={runtimeMetric(snapshot)} />
          </div>
        </section>

        <div className="content-grid">
          <section className="panel profiles-panel" id="profiles">
            <SectionTitle
              icon={<Shield />}
              title="Profiles"
              detail={initialLoading ? "Loading" : `${snapshot?.profiles.length ?? 0} saved`}
            />
            <div className="profile-list">
              {snapshot?.profiles.length ? (
                snapshot.profiles.map((profile) => (
                  <ProfileRow
                    key={profile.id}
                    profile={profile}
                    selected={profile.id === selectedProfile?.id}
                    disabled={runtimeBusy || connected}
                    onSelect={() => void run("select", () => desktopApi.selectProfile(profile.id))}
                    onDelete={() => void run("delete", () => desktopApi.deleteProfile(profile.id))}
                  />
                ))
              ) : initialLoading ? (
                <div className="empty-state" aria-live="polite">
                  <Loader2 className="spin" />
                  <span>Loading profiles...</span>
                </div>
              ) : (
                <div className="empty-state">
                  <HardDrive />
                  <span>No profiles imported.</span>
                </div>
              )}
            </div>
          </section>

          <section className="panel runtime-panel" aria-label="Runtime paths">
            <SectionTitle icon={<Server />} title="Runtime" detail={snapshot?.platform ?? "-"} />
            <div className="runtime-copy">
              <div>
                <Laptop />
                <span>{runtimeCopy(phase, activeProfile ?? selectedProfile)}</span>
              </div>
              <div>
                <HardDrive />
                <span>Config directory: {snapshot?.configDir ?? "-"}</span>
              </div>
            </div>
          </section>

          <details className="panel disclosure-panel import-panel" id="import">
            <DisclosureSummary
              icon={<Upload />}
              title="Import profile"
              detail={portValid ? "Ready" : "Port must be 1024-65535"}
            />

            <div className="import-form">
              <div className="form-grid">
                <label htmlFor={profileNameId}>
                  <span>Name</span>
                  <input
                    id={profileNameId}
                    value={profileName}
                    autoComplete="off"
                    onChange={(event) => setProfileName(event.target.value)}
                  />
                </label>

                <label htmlFor={socksPortId}>
                  <span>SOCKS port</span>
                  <input
                    id={socksPortId}
                    inputMode="numeric"
                    aria-describedby={socksPortHelpId}
                    aria-invalid={!portValid}
                    value={socksPort}
                    onChange={(event) => setSocksPort(event.target.value.replace(/\D/g, "").slice(0, 5))}
                  />
                  <small id={socksPortHelpId} className={portValid ? "field-help" : "field-error"}>
                    Use a local port from 1024 to 65535.
                  </small>
                </label>

                <label htmlFor={httpPortId}>
                  <span>HTTP proxy port</span>
                  <input
                    id={httpPortId}
                    inputMode="numeric"
                    aria-describedby={httpPortHelpId}
                    aria-invalid={!httpPortValid}
                    value={httpPort}
                    onChange={(event) => setHttpPort(event.target.value.replace(/\D/g, "").slice(0, 5))}
                  />
                  <small id={httpPortHelpId} className={httpPortValid ? "field-help" : "field-error"}>
                    Use a different local port from 1024 to 65535.
                  </small>
                </label>
              </div>

              <label htmlFor={rawConfigId}>
                <span>Client profile text</span>
                <textarea
                  id={rawConfigId}
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
                <button
                  type="button"
                  className="primary"
                  disabled={importDisabled}
                  onClick={() =>
                    void run("import", () =>
                      desktopApi.importConfig(profileName, rawConfig, portNumber, httpPortNumber, shareLan),
                    )
                  }
                >
                  {busyAction === "import" ? <Loader2 className="spin" /> : <Upload />}
                  Import profile
                </button>
                <button type="button" disabled={busy} onClick={() => void pasteConfig()}>
                  <ClipboardPaste />
                  Paste
                </button>
              </div>
            </div>
          </details>

          <details className="panel disclosure-panel logs-panel" id="logs">
            <DisclosureSummary icon={<HardDrive />} title="Logs" detail={snapshot?.logsDir ?? "-"} />
            <pre aria-label="Runtime log output" tabIndex={0}>
              {initialLoading ? "Loading logs..." : combinedLogs(snapshot)}
            </pre>
          </details>
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
        <span className="section-icon" aria-hidden="true">
          {icon}
        </span>
        <h2>{title}</h2>
      </div>
      <span>{detail}</span>
    </div>
  );
}

function DisclosureSummary({
  icon,
  title,
  detail,
}: {
  icon: ReactNode;
  title: string;
  detail: string;
}) {
  return (
    <summary className="section-title disclosure-summary" aria-label={`${title}: ${detail}`}>
      <div>
        <span className="section-icon" aria-hidden="true">
          {icon}
        </span>
        <h2>{title}</h2>
      </div>
      <span>{detail}</span>
    </summary>
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
        type="button"
        disabled={disabled}
        aria-pressed={selected}
        aria-label={selected ? `${profile.name} is selected` : `Select ${profile.name}`}
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
      <button
        type="button"
        className="icon-button"
        disabled={disabled}
        onClick={onDelete}
        aria-label={`Delete ${profile.name}`}
        title="Delete profile"
      >
        <Trash2 aria-hidden="true" />
      </button>
    </div>
  );
}

function ModeButton({
  active,
  disabled,
  icon,
  label,
  onClick,
}: {
  active: boolean;
  disabled: boolean;
  icon: ReactNode;
  label: string;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      className={active ? "mode-button active" : "mode-button"}
      disabled={disabled}
      aria-pressed={active}
      onClick={onClick}
    >
      {icon}
      <span>{label}</span>
    </button>
  );
}

function StatusCard({ phase, address }: { phase: string; address: string }) {
  return (
    <div className={`status-card ${phase}`} aria-live="polite">
      <span>Status</span>
      <strong>{formatPhase(phase)}</strong>
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
  return (
    <div className={`phase-badge ${phase}`} role="status" aria-live="polite">
      {formatPhase(phase)}
    </div>
  );
}

function statusTitle(phase: string) {
  if (phase === "connected") {
    return "Connected";
  }
  if (phase === "connecting") {
    return "Connecting";
  }
  if (phase === "disconnecting") {
    return "Disconnecting";
  }
  if (phase === "loading") {
    return "Checking status";
  }
  if (phase === "error") {
    return "Needs attention";
  }
  return "Ready to connect";
}

function formatPhase(phase: string) {
  return phase.replace(/^\w/, (letter) => letter.toUpperCase());
}

function selectedProfileAddress(profile: ClientProfile | null) {
  if (!profile) {
    return "-";
  }
  return `${profile.shareLan ? "0.0.0.0" : "127.0.0.1"}:${profile.socksPort}`;
}

function selectedProfileHTTPAddress(profile: ClientProfile | null) {
  if (!profile) {
    return "-";
  }
  return `${profile.shareLan ? "0.0.0.0" : "127.0.0.1"}:${profile.httpPort}`;
}

function runtimeMetric(snapshot: DesktopSnapshot | null) {
  if (!snapshot) {
    return "Loading...";
  }
  const connection = snapshot.connection;
  if (connection.phase !== "connected") {
    return "-";
  }
  const parts = [`PID ${connection.pid ?? "-"}`];
  if (connection.systemProxyEnabled) {
    parts.push("Windows proxy");
  }
  if (connection.tunnelActive) {
    parts.push(connection.tunnelInterfaceName ?? "VPN");
  }
  return parts.join(" · ");
}

function combinedLogs(snapshot: DesktopSnapshot | null) {
  if (!snapshot) {
    return "Loading logs...";
  }
  const parts = [];
  if (snapshot.logTail) {
    parts.push(`[client]\n${snapshot.logTail}`);
  }
  if (snapshot.tunnelLogTail) {
    parts.push(`[vpn]\n${snapshot.tunnelLogTail}`);
  }
  return parts.join("\n\n") || "No log output yet.";
}

function runtimeMessage(connected: boolean, profile?: ClientProfile) {
  if (connected && profile) {
    return `Connected with ${profile.name}.`;
  }
  return "Disconnected.";
}

function runtimeCopy(phase: string, profile: ClientProfile | null) {
  if (phase === "connected" && profile) {
    return `Sidecar running for ${profile.name}.`;
  }
  if (phase === "connecting") {
    return "Starting packaged Skirk sidecar.";
  }
  if (phase === "disconnecting") {
    return "Stopping packaged Skirk sidecar.";
  }
  return "Sidecar is stopped.";
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
