import { useCallback, useEffect, useId, useMemo, useState } from "react";
import type { ReactNode } from "react";
import {
  Check,
  ClipboardPaste,
  Cloud,
  Copy,
  FileText,
  Folder,
  Laptop,
  Loader2,
  MonitorCog,
  Moon,
  Network,
  Play,
  Power,
  RefreshCw,
  Server,
  SlidersHorizontal,
  ShieldCheck,
  Sun,
  Trash2,
  Upload,
  UserRound,
  X,
} from "lucide-react";

import {
  desktopApi,
  recommendedPerformance,
  type ClientPerformanceSettings,
  type ClientProfile,
  type ConnectionMode,
  type DesktopSnapshot,
  type PerformancePreset,
} from "./lib/api";
import logoMark from "./assets/logo-mark.png";
import packageInfo from "../package.json";

type Theme = "light" | "dark";
type BusyAction = "connect" | "disconnect" | "import" | "select" | "delete" | "mode" | "performance";
const APP_VERSION = packageInfo.version;
const DRIVE_USER_UNITS_PER_MINUTE = 325_000;
const DRIVE_LIST_UNITS = 100;
const CUSTOM_MIN_POLL_MS = 250;
const CUSTOM_MAX_UPLOAD_WORKERS = 64;
const CUSTOM_MAX_DOWNLOAD_WORKERS = 64;

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
  const [advancedOpen, setAdvancedOpen] = useState(false);
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
  const socksCopyAddress = snapshot?.connection.lanAddresses[0] ?? socksAddress;
  const httpCopyAddress = snapshot?.connection.lanHttpAddresses[0] ?? httpAddress;
  const lanSocksAddress = snapshot?.connection.lanAddresses[0] ?? "-";
  const lanHttpAddress = snapshot?.connection.lanHttpAddresses[0] ?? "-";
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
      ? `${runtimeProfile.routeMode} · ${profileEndpointSummary(runtimeProfile, socksCopyAddress, httpCopyAddress)}`
      : "Import a profile to enable Connect.";
  const lanAddressValue = initialLoading
    ? "Loading..."
    : snapshot?.connection.lanAddresses.join(", ") || "-";
  const lanHttpAddressValue = initialLoading
    ? "Loading..."
    : snapshot?.connection.lanHttpAddresses.join(", ") || "-";
  const endpointValue = initialLoading ? "Loading..." : socksAddress;
  const httpEndpointValue = initialLoading ? "Loading..." : httpAddress;
  const copyLocalSocksDisabled = !selectedProfile || socksAddress === "-";
  const copyLocalHttpDisabled = !selectedProfile || httpAddress === "-";
  const copyLanSocksDisabled = !selectedProfile || lanSocksAddress === "-";
  const copyLanHttpDisabled = !selectedProfile || lanHttpAddress === "-";
  const runtimeStatusMessage =
    copyStatus ||
    (initialLoading
      ? "Loading runtime status..."
      : vpnNeedsAdmin
        ? vpnAdminMessage(snapshot?.platform)
      : snapshot?.connection.message || runtimeMessage(connected, activeProfile));
  const performance = runtimeProfile?.performance ?? recommendedPerformance();
  const performanceDisabled = runtimeBusy || connected || !selectedProfile;

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

  async function copyEndpoint(label: string, address: string) {
    if (address === "-") {
      return;
    }
    try {
      await copyText(address);
      setCopyStatus(`${label} address copied.`);
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

  async function updatePerformance(next: ClientPerformanceSettings) {
    if (!selectedProfile || performanceDisabled) {
      return;
    }
    await run("performance", () => desktopApi.updateProfilePerformance(selectedProfile.id, next));
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
          address={initialLoading ? "Loading..." : profileEndpointSummary(runtimeProfile, socksCopyAddress, httpCopyAddress)}
        />

        <nav className="side-nav" aria-label="Skirk sections">
          <a href="#profiles">Profiles</a>
          <a href="#import">Import</a>
          <a href="#runtime">Runtime</a>
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
            <span className="version-badge" aria-label={`Skirk Desktop version ${APP_VERSION}`}>
              v{APP_VERSION}
            </span>
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
              detail="SOCKS and HTTP"
              onClick={() => void changeMode("proxy")}
            />
            <ModeButton
              active={selectedMode === "system"}
              disabled={runtimeBusy || connected || !snapshot?.capabilities.systemProxySupported}
              icon={<MonitorCog />}
              label="System proxy"
              detail={snapshot?.capabilities.systemProxySupported === false ? "Windows only" : "Proxy-aware apps"}
              onClick={() => void changeMode("system")}
            />
            <ModeButton
              active={selectedMode === "vpn"}
              disabled={runtimeBusy || connected || !snapshot?.capabilities.vpnModeSupported}
              icon={<ShieldCheck />}
              label="VPN"
              detail={vpnModeDetail(snapshot)}
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
                {vpnNeedsAdmin ? "Admin required" : "Connect"}
              </button>
            )}
            <button
              type="button"
              disabled={copyLocalSocksDisabled}
              onClick={() => void copyEndpoint("Local SOCKS", socksAddress)}
            >
              {copyStatus.startsWith("Local SOCKS") ? <Check /> : <Copy />}
              {copyStatus.startsWith("Local SOCKS") ? "Copied local SOCKS" : "Copy local SOCKS"}
            </button>
            <button
              type="button"
              disabled={copyLocalHttpDisabled}
              onClick={() => void copyEndpoint("Local HTTP", httpAddress)}
            >
              {copyStatus.startsWith("Local HTTP") ? <Check /> : <Copy />}
              {copyStatus.startsWith("Local HTTP") ? "Copied local HTTP" : "Copy local HTTP"}
            </button>
            <button
              type="button"
              disabled={copyLanSocksDisabled}
              onClick={() => void copyEndpoint("LAN SOCKS", lanSocksAddress)}
            >
              {copyStatus.startsWith("LAN SOCKS") ? <Check /> : <Copy />}
              {copyStatus.startsWith("LAN SOCKS") ? "Copied LAN SOCKS" : "Copy LAN SOCKS"}
            </button>
            <button
              type="button"
              disabled={copyLanHttpDisabled}
              onClick={() => void copyEndpoint("LAN HTTP", lanHttpAddress)}
            >
              {copyStatus.startsWith("LAN HTTP") ? <Check /> : <Copy />}
              {copyStatus.startsWith("LAN HTTP") ? "Copied LAN HTTP" : "Copy LAN HTTP"}
            </button>
          </div>

          <SettingsStrip
            snapshot={snapshot}
            performance={performance}
            onOpenAdvanced={() => setAdvancedOpen(true)}
          />

          <div className="metric-grid" aria-label="Runtime details">
            <Metric label="SOCKS bind" value={endpointValue} />
            <Metric label="HTTP bind" value={httpEndpointValue} />
            <Metric label="LAN SOCKS" value={lanAddressValue} />
            <Metric label="LAN HTTP" value={lanHttpAddressValue} />
            <Metric label="Runtime" value={runtimeMetric(snapshot)} />
          </div>
        </section>

        <div className="content-grid">
          <section className="panel profiles-panel" id="profiles">
            <SectionTitle
              icon={<UserRound />}
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
                  <UserRound />
                  <span>No profiles imported.</span>
                </div>
              )}
            </div>
          </section>

          <details className="panel disclosure-panel import-panel" id="import" open={!initialLoading && !snapshot?.profiles.length}>
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

          <section className="panel runtime-panel" aria-label="Runtime paths">
            <SectionTitle icon={<Server />} title="Runtime" detail={snapshot?.platform ?? "-"} />
            <div className="runtime-copy">
              <div>
                <Laptop />
                <span>{runtimeCopy(phase, activeProfile ?? selectedProfile)}</span>
              </div>
              <div>
                <Folder />
                <span>Config directory: {snapshot?.configDir ?? "-"}</span>
              </div>
            </div>
          </section>

          <details className="panel disclosure-panel logs-panel" id="logs">
            <DisclosureSummary icon={<FileText />} title="Logs" detail={snapshot?.logsDir ?? "-"} />
            <pre aria-label="Runtime log output" tabIndex={0}>
              {initialLoading ? "Loading logs..." : combinedLogs(snapshot)}
            </pre>
          </details>
        </div>
        {advancedOpen ? (
          <AdvancedSettingsDialog
            snapshot={snapshot}
            performance={performance}
            disabled={performanceDisabled}
            busy={busyAction === "performance"}
            onClose={() => setAdvancedOpen(false)}
            onChange={(next) => void updatePerformance(next)}
          />
        ) : null}
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
          {selected ? <Check /> : <UserRound />}
          {profile.name}
        </span>
        <span>
          {profile.routeMode} · {profileRowDetail(profile)}
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
  detail,
  onClick,
}: {
  active: boolean;
  disabled: boolean;
  icon: ReactNode;
  label: string;
  detail: string;
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
      <span>
        <strong>{label}</strong>
        <small>{detail}</small>
      </span>
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

function SettingsStrip({
  snapshot,
  performance,
  onOpenAdvanced,
}: {
  snapshot: DesktopSnapshot | null;
  performance: ClientPerformanceSettings;
  onOpenAdvanced: () => void;
}) {
  const units = snapshot?.metrics?.recentQuotaPerMinute.units ?? estimatedIdleUnits(performance);
  const errors = snapshot?.metrics?.recentQuotaPerMinute.errors ?? 0;
  const errorReason = snapshot?.metrics?.recentQuotaPerMinute.lastErrorReason;
  return (
    <div className="settings-strip" aria-label="Advanced connection summary">
      <div>
        <Cloud aria-hidden="true" />
        <span>Drive</span>
        <strong>{drivePressureLabel(units, errors, errorReason)}</strong>
      </div>
      <div>
        <SlidersHorizontal aria-hidden="true" />
        <span>Performance</span>
        <strong>{performancePresetLabel(performance.preset)}</strong>
      </div>
      <button type="button" onClick={onOpenAdvanced}>
        <SlidersHorizontal />
        Advanced
      </button>
    </div>
  );
}

function AdvancedSettingsDialog({
  snapshot,
  performance,
  disabled,
  busy,
  onClose,
  onChange,
}: {
  snapshot: DesktopSnapshot | null;
  performance: ClientPerformanceSettings;
  disabled: boolean;
  busy: boolean;
  onClose: () => void;
  onChange: (performance: ClientPerformanceSettings) => void;
}) {
  return (
    <div className="modal-backdrop" role="presentation" onMouseDown={onClose}>
      <section
        className="advanced-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="advanced-settings-title"
        onMouseDown={(event) => event.stopPropagation()}
      >
        <header>
          <div>
            <span className="eyebrow">Advanced</span>
            <h2 id="advanced-settings-title">Performance and Drive usage</h2>
          </div>
          <button type="button" className="icon-button" aria-label="Close advanced settings" onClick={onClose}>
            <X />
          </button>
        </header>

        <QuotaPanel snapshot={snapshot} performance={performance} />
        <section className="advanced-section" aria-label="Performance settings">
          <SectionTitle icon={<SlidersHorizontal />} title="Performance" detail={performancePresetLabel(performance.preset)} />
          <PerformanceSettingsPanel
            performance={performance}
            disabled={disabled}
            busy={busy}
            onChange={onChange}
          />
        </section>
      </section>
    </div>
  );
}

function PerformanceSettingsPanel({
  performance,
  disabled,
  busy,
  onChange,
}: {
  performance: ClientPerformanceSettings;
  disabled: boolean;
  busy: boolean;
  onChange: (performance: ClientPerformanceSettings) => void;
}) {
  const presets: PerformancePreset[] = ["lower_usage", "recommended", "responsive", "bulk_transfer", "custom"];
  const custom = performance.preset === "custom";
  return (
    <div className="performance-settings">
      <div className="preset-grid" role="group" aria-label="Performance profile">
        {presets.map((preset) => (
          <button
            key={preset}
            type="button"
            className={performance.preset === preset ? "preset-button active" : "preset-button"}
            disabled={disabled || busy}
            aria-pressed={performance.preset === preset}
            onClick={() => onChange(performanceForPreset(preset, performance))}
          >
            <strong>{performancePresetLabel(preset)}</strong>
            <span>{performancePresetDetail(preset)}</span>
          </button>
        ))}
      </div>
      <div className="performance-summary" aria-live="polite">
        <span>{formatMs(performance.pollMs)} check</span>
        <span>{performance.uploadConcurrency} up</span>
        <span>{performance.downloadConcurrency} down</span>
        <span>{performance.burstPoll ? "Burst on" : "Burst off"}</span>
      </div>
      {performance.burstPoll ? (
        <div className="warning-note">
          Fast wake checks Drive much faster after traffic. Use it only when responsiveness matters more than quota.
        </div>
      ) : null}
      {custom ? (
        <div className="custom-grid">
          <NumberField
            label="Check interval"
            suffix="ms"
            value={performance.pollMs}
            min={CUSTOM_MIN_POLL_MS}
            max={60000}
            disabled={disabled || busy}
            onChange={(value) => onChange({ ...performance, pollMs: value })}
          />
          <NumberField
            label="Upload workers"
            value={performance.uploadConcurrency}
            min={1}
            max={CUSTOM_MAX_UPLOAD_WORKERS}
            disabled={disabled || busy}
            onChange={(value) => onChange({ ...performance, uploadConcurrency: value })}
          />
          <NumberField
            label="Download workers"
            value={performance.downloadConcurrency}
            min={1}
            max={CUSTOM_MAX_DOWNLOAD_WORKERS}
            disabled={disabled || busy}
            onChange={(value) => onChange({ ...performance, downloadConcurrency: value })}
          />
          <label className="switch-row compact">
            <input
              type="checkbox"
              checked={performance.burstPoll}
              disabled={disabled || busy}
              onChange={(event) => onChange({ ...performance, burstPoll: event.target.checked })}
            />
            <span>
              <strong>Fast wake after upload</strong>
              <small>Costs extra Drive list calls after traffic.</small>
            </span>
          </label>
        </div>
      ) : null}
      {custom ? (
        <small className="field-help">
          Lower check intervals and higher worker counts can burn Drive quota quickly. Use aggressive values only when you understand the tradeoff.
        </small>
      ) : null}
      {disabled ? <small className="field-help">Disconnect to change performance settings.</small> : null}
    </div>
  );
}

function NumberField({
  label,
  value,
  min,
  max,
  suffix,
  disabled,
  onChange,
}: {
  label: string;
  value: number;
  min: number;
  max: number;
  suffix?: string;
  disabled: boolean;
  onChange: (value: number) => void;
}) {
  return (
    <label>
      <span>
        {label}
        {suffix ? ` (${suffix})` : ""}
      </span>
      <input
        inputMode="numeric"
        disabled={disabled}
        value={String(value)}
        onChange={(event) => {
          const next = Number(event.target.value.replace(/\D/g, "").slice(0, 5));
          if (Number.isFinite(next)) {
            onChange(Math.min(max, Math.max(min, next || min)));
          }
        }}
      />
    </label>
  );
}

function QuotaPanel({
  snapshot,
  performance,
}: {
  snapshot: DesktopSnapshot | null;
  performance: ClientPerformanceSettings;
}) {
  const metrics = snapshot?.metrics;
  const idleUnits = estimatedIdleUnits(performance);
  const unitsPerMinute = metrics?.recentQuotaPerMinute.units ?? 0;
  const displayedUnits = metrics ? unitsPerMinute : idleUnits;
  const errors = metrics?.recentQuotaPerMinute.errors ?? 0;
  const errorReason = metrics?.recentQuotaPerMinute.lastErrorReason;
  return (
    <div className="quota-copy">
      <div className="quota-meter-header">
        <div>
          <strong>{drivePressureLabel(displayedUnits, errors, errorReason)}</strong>
          <span>{metrics ? "Measured from this desktop process" : "Idle estimate before connection"}</span>
        </div>
        <span>{formatNumber(Math.round(displayedUnits))} units/min</span>
      </div>
      <UsageMeter units={displayedUnits} errors={errors} />
      <div className="quota-line">
        <span>Recent local estimate</span>
        <strong>{metrics ? `${formatNumber(Math.round(unitsPerMinute))} units/min` : "Connect to measure"}</strong>
      </div>
      <div className="quota-line">
        <span>Idle check estimate</span>
        <strong>~{formatNumber(Math.round(idleUnits))} units/min</strong>
      </div>
      <div className="quota-line">
        <span>Errors</span>
        <strong>{metrics ? `${formatNumber(Math.round(errors))}${errorReason ? ` · ${driveErrorReasonLabel(errorReason)}` : ""}` : "-"}</strong>
      </div>
      {metrics?.driveBackoff?.active ? (
        <div className="quota-line warning">
          <span>Drive cooldown</span>
          <strong>
            {metrics.driveBackoff.reason || "rate limited"} · {Math.ceil(metrics.driveBackoff.waitSeconds ?? 0)}s
          </strong>
        </div>
      ) : null}
      <small>
        Skirk uses Google Drive requests, not Drive storage. This estimate is local to this desktop process; the
        exit and other clients share the Google budget.
      </small>
      {metrics?.recentQuotaOps ? <code>{metrics.recentQuotaOps}</code> : null}
    </div>
  );
}

function UsageMeter({ units, errors }: { units: number; errors: number }) {
  const width = `${Math.max(3, Math.min(100, drivePressureFraction(units) * 100))}%`;
  const level =
    errors > 0 ? "error" : drivePressureFraction(units) >= 0.7 ? "high" : drivePressureFraction(units) >= 0.3 ? "medium" : "low";
  return (
    <div className={`usage-meter ${level}`} aria-label={`Drive API pressure ${Math.round(Number(width.replace("%", "")))} percent`}>
      <span style={{ width }} />
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

function vpnAdminMessage(platform: string | undefined) {
  if (platform === "linux") {
    return "VPN mode needs root or CAP_NET_ADMIN. Close Skirk and open it with the needed TUN privileges.";
  }
  return "VPN mode needs Administrator privileges. Close Skirk and open Skirk.exe with Run as administrator.";
}

function vpnModeDetail(snapshot: DesktopSnapshot | null) {
  if (!snapshot) {
    return "Whole device";
  }
  if (snapshot.capabilities.vpnModeSupported) {
    if (snapshot.platform === "linux") {
      return snapshot.capabilities.vpnAdmin ? "Linux TUN" : "Needs root/CAP_NET_ADMIN";
    }
    return "Whole device";
  }
  if (snapshot.platform === "linux") {
    return "Sidecar missing";
  }
  if (snapshot.platform === "windows") {
    return "Sidecar missing";
  }
  return "Unavailable";
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

function profileEndpointSummary(profile: ClientProfile | null | undefined, socks: string, http: string) {
  if (!profile) {
    return "-";
  }
  if (profile.shareLan) {
    return `LAN SOCKS ${socks} · LAN HTTP ${http}`;
  }
  return `SOCKS ${socks} · HTTP ${http}`;
}

function profileRowDetail(profile: ClientProfile) {
  if (profile.shareLan) {
    return `LAN sharing · SOCKS ${profile.socksPort} · HTTP ${profile.httpPort}`;
  }
  return `Local only · SOCKS ${profile.socksPort} · HTTP ${profile.httpPort} · ${performancePresetLabel(profile.performance.preset)}`;
}

function performanceForPreset(
  preset: PerformancePreset,
  current: ClientPerformanceSettings = recommendedPerformance(),
): ClientPerformanceSettings {
  if (preset === "lower_usage") {
    return { preset, pollMs: 2000, uploadConcurrency: 4, downloadConcurrency: 8, burstPoll: false, burstPollMs: 75, burstPollWindowMs: 5000 };
  }
  if (preset === "responsive") {
    return { preset, pollMs: 1000, uploadConcurrency: 8, downloadConcurrency: 16, burstPoll: true, burstPollMs: 75, burstPollWindowMs: 5000 };
  }
  if (preset === "bulk_transfer") {
    return { preset, pollMs: 1000, uploadConcurrency: 16, downloadConcurrency: 32, burstPoll: false, burstPollMs: 75, burstPollWindowMs: 5000 };
  }
  if (preset === "custom") {
    return { ...current, preset };
  }
  return recommendedPerformance();
}

function performancePresetLabel(preset: PerformancePreset) {
  if (preset === "lower_usage") {
    return "Lower Drive usage";
  }
  if (preset === "responsive") {
    return "Responsive";
  }
  if (preset === "bulk_transfer") {
    return "Bulk transfer";
  }
  if (preset === "custom") {
    return "Custom";
  }
  return "Recommended";
}

function performancePresetDetail(preset: PerformancePreset) {
  const settings = performanceForPreset(preset);
  if (preset === "custom") {
    return "Manual limits";
  }
  return `${formatMs(settings.pollMs)} · ${settings.uploadConcurrency}/${settings.downloadConcurrency} workers${settings.burstPoll ? " · burst" : ""}`;
}

function formatMs(value: number) {
  if (value >= 1000 && value % 1000 === 0) {
    return `${value / 1000}s`;
  }
  return `${value}ms`;
}

function formatNumber(value: number) {
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 }).format(value);
}

function estimatedIdleUnits(performance: ClientPerformanceSettings) {
  const base = (60_000 / Math.max(CUSTOM_MIN_POLL_MS, performance.pollMs)) * DRIVE_LIST_UNITS;
  return performance.burstPoll ? base * 2 : base;
}

function drivePressureFraction(units: number) {
  return Math.max(0, Math.min(1, units / DRIVE_USER_UNITS_PER_MINUTE));
}

function drivePressureLabel(units: number, errors: number, reason?: string) {
  if (errors > 0) {
    return driveErrorReasonLabel(reason);
  }
  if (units <= 0) {
    return "Not measured";
  }
  if (units < DRIVE_USER_UNITS_PER_MINUTE * 0.08) {
    return "Normal";
  }
  if (units < DRIVE_USER_UNITS_PER_MINUTE * 0.3) {
    return "Moderate";
  }
  if (units < DRIVE_USER_UNITS_PER_MINUTE * 0.7) {
    return "High";
  }
  return "Limit risk";
}

function driveErrorReasonLabel(reason?: string) {
  const value = (reason ?? "").toLowerCase();
  const compact = value.replace(/[^a-z0-9]/g, "");
  if (compact.includes("storagequotaexceeded")) {
    return "Drive storage full";
  }
  if (compact.includes("ratelimit") || compact.includes("toomany") || compact === "status429") {
    return "Drive rate limited";
  }
  if (compact.includes("unauthorized") || compact === "status401") {
    return "Google login expired";
  }
  if (compact.includes("notfound")) {
    return "Drive mailbox missing";
  }
  if (compact.includes("timeout") || compact.includes("deadline")) {
    return "Drive timeout";
  }
  return "Drive errors";
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
