import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import type { KeyboardEvent, ReactNode } from "react";
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
  type ExitProbeResult,
  type PerformancePreset,
  type UserRouteMode,
} from "./lib/api";
import logoMark from "./assets/logo-mark.png";
import packageInfo from "../package.json";

type Theme = "light" | "dark";
type BusyAction = "connect" | "disconnect" | "import" | "select" | "delete" | "mode" | "performance" | "route";
const APP_VERSION = packageInfo.version;
const DRIVE_USER_UNITS_PER_MINUTE = 325_000;
const CUSTOM_MIN_POLL_MS = 250;
const CUSTOM_MAX_UPLOAD_WORKERS = 64;
const CUSTOM_MAX_DOWNLOAD_WORKERS = 64;

function App() {
  const [snapshot, setSnapshot] = useState<DesktopSnapshot | null>(null);
  const [rawConfig, setRawConfig] = useState("");
  const [profileFileError, setProfileFileError] = useState("");
  const [profileName, setProfileName] = useState("Skirk profile");
  const [socksPort, setSocksPort] = useState("18080");
  const [httpPort, setHttpPort] = useState("18081");
  const [shareLan, setShareLan] = useState(false);
  const [theme, setTheme] = useState<Theme>(() => {
    const saved = window.localStorage.getItem("skirk-theme");
    if (saved === "light" || saved === "dark") return saved;
    return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  });
  const [error, setError] = useState("");
  const [busyAction, setBusyAction] = useState<BusyAction | null>(null);
  const [exitProbe, setExitProbe] = useState<ExitProbeResult | null>(null);
  const [exitProbeError, setExitProbeError] = useState("");
  const [exitProbeRunning, setExitProbeRunning] = useState(false);
  const exitProbeGeneration = useRef(0);
  const [copyStatus, setCopyStatus] = useState("");
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const [importOpen, setImportOpen] = useState(false);
  const [replaceProfileId, setReplaceProfileId] = useState<string | null>(null);
  const [pendingDeleteProfile, setPendingDeleteProfile] = useState<ClientProfile | null>(null);
  const importPanelRef = useRef<HTMLDetailsElement>(null);
  const profileFileRef = useRef<HTMLInputElement>(null);
  const profileNameId = useId();
  const socksPortId = useId();
  const httpPortId = useId();
  const socksPortHelpId = useId();
  const httpPortHelpId = useId();
  const rawConfigId = useId();

  const refresh = useCallback(async () => {
    try {
      setSnapshot(await desktopApi.loadSnapshot());
    } catch (nextError) {
      setError(normalizeError(nextError));
    }
  }, []);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
  }, [theme]);

  useEffect(() => {
    const media = window.matchMedia("(prefers-color-scheme: dark)");
    const followSystem = () => {
      if (!window.localStorage.getItem("skirk-theme")) {
        setTheme(media.matches ? "dark" : "light");
      }
    };
    media.addEventListener("change", followSystem);
    return () => media.removeEventListener("change", followSystem);
  }, []);

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
  const disconnectAvailable = connected || connecting || disconnecting;
  const initialLoading = snapshot === null && error === "";
  const busy = busyAction !== null;
  const runtimeBusy = busy || connecting || disconnecting;
  const portNumber = Number(socksPort);
  const httpPortNumber = Number(httpPort);
  const portValid = Number.isInteger(portNumber) && portNumber >= 1024 && portNumber <= 65535;
  const httpPortValid = Number.isInteger(httpPortNumber) && httpPortNumber >= 1024 && httpPortNumber <= 65535 && httpPortNumber !== portNumber;
  const importDisabled = runtimeBusy || connected || rawConfig.trim() === "" ||
    (replaceProfileId === null ? !portValid || !httpPortValid : !snapshot?.profiles.some((profile) => profile.id === replaceProfileId));
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
      ? `${googleRouteLabel(runtimeProfile.routeMode)} · ${profileEndpointSummary(runtimeProfile, socksCopyAddress, httpCopyAddress, selectedMode)}`
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
        : connected && snapshot?.platform === "macos" && selectedMode === "proxy"
          ? "Local proxy ready. Configure your browser or app to use it."
          : snapshot?.connection.message || runtimeMessage(connected, activeProfile));
  // Quota metrics and log tails can survive a profile switch. The running
  // sidecar reports a terminal grant failure in this connection's message.
  const googleGrantExpired = !connected && isGoogleGrantExpired(
    error,
    phase === "error" ? snapshot?.connection.message : undefined,
  );
  const displayPhase = googleGrantExpired && !connected ? "error" : phase;
  const routeSelection = selectedProfile?.routeMode === "direct" || selectedProfile?.routeMode === "real_pinned"
    ? "direct"
    : "google_front_pinned";
  const replacingProfile = snapshot?.profiles.find((profile) => profile.id === replaceProfileId) ?? null;
  const performance = runtimeProfile?.performance ?? recommendedPerformance();
  const performanceDisabled = runtimeBusy || connected || !selectedProfile;

  useEffect(() => {
    exitProbeGeneration.current += 1;
    setExitProbe(null);
    setExitProbeError("");
    setExitProbeRunning(false);
  }, [snapshot?.connection.activeProfileId, connected]);

  useEffect(() => {
    if (!initialLoading && (googleGrantExpired || !snapshot?.profiles.length)) {
      setImportOpen(true);
    }
    if (googleGrantExpired && selectedProfile) {
      setReplaceProfileId(selectedProfile.id);
    }
  }, [googleGrantExpired, initialLoading, selectedProfile?.id, snapshot?.profiles.length]);

  async function run(actionName: BusyAction, action: () => Promise<DesktopSnapshot>) {
    setBusyAction(actionName);
    try {
      setSnapshot(await action());
      setError("");
    } catch (nextError) {
      const message = normalizeError(nextError);
      if (message !== "Connection cancelled") setError(message);
      await refresh();
    } finally {
      setBusyAction((current) => current === actionName ? null : current);
    }
  }

  async function pasteConfig() {
    try {
      const text = await navigator.clipboard.readText();
      setRawConfig(text);
      setProfileFileError("");
      setError("");
    } catch (nextError) {
      setError(normalizeError(nextError));
    }
  }

  async function readProfileFile(file: File) {
    try {
      if (file.size > 256 * 1024) {
        throw new Error("Profile file is too large (maximum 256 KiB).");
      }
      const data = await file.arrayBuffer();
      if (data.byteLength > 256 * 1024) {
        throw new Error("Profile file is too large (maximum 256 KiB).");
      }
      const value = new TextDecoder("utf-8", { fatal: true }).decode(data);
      if (!value.trim()) {
        throw new Error("The selected profile file is empty.");
      }
      setRawConfig(value);
      setProfileFileError("");
      setError("");
    } catch (cause) {
      setRawConfig("");
      setProfileFileError(cause instanceof Error &&
        (cause.message === "Profile file is too large (maximum 256 KiB)." || cause.message === "The selected profile file is empty.")
          ? cause.message
          : "Could not read this file. Choose a UTF-8 client.skirk or client.json.");
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

  async function changeRoute(routeMode: UserRouteMode) {
    if (!selectedProfile || selectedProfile.routeMode === routeMode || runtimeBusy || connected) {
      return;
    }
    await run("route", () => desktopApi.updateProfileRoute(selectedProfile.id, routeMode));
  }

  function beginProfileReplacement() {
    if (!selectedProfile) return;
    setReplaceProfileId(selectedProfile.id);
    setImportOpen(true);
    window.requestAnimationFrame(() => {
      importPanelRef.current?.scrollIntoView({ behavior: "smooth", block: "start" });
      document.getElementById(rawConfigId)?.focus();
    });
  }

  async function copyRenewCommand() {
    try {
      await copyText("skirk setup renew-oauth --kit skirk-kit");
      setCopyStatus("Renew command copied.");
    } catch (nextError) {
      setError(normalizeError(nextError));
    }
  }

  async function testExit() {
    if (!connected || exitProbeRunning) return;
    const generation = exitProbeGeneration.current;
    setExitProbeRunning(true);
    setExitProbe(null);
    setExitProbeError("");
    try {
      const result = await desktopApi.probeExit();
      if (exitProbeGeneration.current === generation) setExitProbe(result);
    } catch {
      if (exitProbeGeneration.current === generation) {
        setExitProbeError("The exit test could not run. Check that the local client is still connected.");
      }
    } finally {
      if (exitProbeGeneration.current === generation) setExitProbeRunning(false);
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
          phase={displayPhase}
          address={initialLoading ? "Loading..." : profileEndpointSummary(runtimeProfile, socksCopyAddress, httpCopyAddress, selectedMode)}
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
          onClick={() => {
            const next = theme === "dark" ? "light" : "dark";
            window.localStorage.setItem("skirk-theme", next);
            setTheme(next);
          }}
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

        {googleGrantExpired ? (
          <section className="recovery-card" aria-labelledby="recovery-title">
            <div>
              <h2 id="recovery-title">Google access needs renewal</h2>
              <p role="alert">Google rejected this device's saved access. Ask the exit owner for a renewed client profile.</p>
            </div>
            <ol>
              <li>On the exit, run <code>skirk setup renew-oauth --kit skirk-kit</code> using your kit folder, then restart the exit.</li>
              <li>Get the updated <code>client.skirk</code> from the exit owner and replace this profile. Your ports, route, and performance settings stay saved.</li>
            </ol>
            <div className="button-row">
              <button type="button" onClick={() => void copyRenewCommand()}>
                {copyStatus === "Renew command copied." ? <Check /> : <Copy />}
                {copyStatus === "Renew command copied." ? "Command copied" : "Copy command for exit owner"}
              </button>
              <button type="button" className="primary" disabled={!selectedProfile || runtimeBusy || connected} onClick={beginProfileReplacement}>
                <Upload /> Import renewed profile
              </button>
            </div>
            {connected ? <small>Disconnect before replacing the saved profile.</small> : null}
            <details className="recovery-owner-details">
              <summary>For Google Cloud project owners</summary>
              <p>If your OAuth app is in <strong>Testing</strong>, publish it under Google Auth Platform → Audience to remove the automatic seven-day expiry for new grants. Publishing does not restore a rejected grant. Renew it first; other token failures can still happen later.</p>
            </details>
          </section>
        ) : error ? (
          <div className="alert" role="alert">
            {error}
          </div>
        ) : null}

        <section className={`control-surface ${displayPhase}`} id="runtime" aria-labelledby="runtime-title">
          <div className="control-main">
            <div className={`status-indicator ${displayPhase}`} aria-hidden="true">
              <span />
            </div>
            <div className="control-copy">
              <span className="eyebrow">Connection status</span>
              <h2 id="runtime-title">{statusTitle(displayPhase)}</h2>
              <p aria-live="polite">{runtimeStatusMessage}</p>
            </div>
          </div>

          <div className="profile-summary" role="group" aria-label="Profile in use">
            <span>{profileStatusLabel}</span>
            <strong>{initialLoading ? "Loading..." : runtimeProfile?.name ?? "No profile selected"}</strong>
            <small>{profileDetail}</small>
          </div>

          <div className="mode-selector" role="radiogroup" aria-label="Connection mode" onKeyDown={handleRadioGroupKeyDown}>
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
              detail={snapshot?.capabilities.systemProxySupported === false ? "Windows and Mac" : "Proxy-aware apps"}
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

          <div className="route-choice" role="group" aria-label="Google access route">
            <div className="route-choice-copy">
              <strong>Google access route</strong>
              <small>Google front is the default. Direct Google is available if your region permits it.</small>
            </div>
            <div className="route-selector" role="radiogroup" aria-label="Google access route" onKeyDown={handleRadioGroupKeyDown}>
              <ModeButton
                active={Boolean(selectedProfile) && routeSelection === "google_front_pinned"}
                disabled={runtimeBusy || connected || !selectedProfile}
                icon={<Cloud />}
                label="Google front"
                detail="www.google.com SNI"
                onClick={() => void changeRoute("google_front_pinned")}
              />
              <ModeButton
                active={Boolean(selectedProfile) && routeSelection === "direct"}
                disabled={runtimeBusy || connected || !selectedProfile}
                icon={<Network />}
                label="Direct Google"
                detail="Drive API hosts"
                onClick={() => void changeRoute("direct")}
              />
            </div>
            {connected ? <small className="field-help">Disconnect to change the Google access route.</small> : null}
            {selectedProfile && !["google_front_pinned", "direct"].includes(selectedProfile.routeMode) ? (
              <small className="field-help">Saved route: {googleRouteLabel(selectedProfile.routeMode)}. Choose either option to update it.</small>
            ) : null}
          </div>

          <div className="command-row" role="group" aria-label="Connection actions">
            {disconnectAvailable ? (
              <button
                type="button"
                className="primary"
                disabled={busyAction !== null && busyAction !== "connect"}
                onClick={() => void run("disconnect", () => desktopApi.disconnect())}
              >
                {busyAction === "disconnect" || disconnecting ? <Loader2 className="spin" /> : <Power />}
                {connecting ? "Cancel connection" : "Disconnect"}
              </button>
            ) : (
              <button
                type="button"
                className="primary"
                disabled={runtimeBusy || !selectedProfile || (!googleGrantExpired && vpnNeedsAdmin)}
                onClick={googleGrantExpired ? beginProfileReplacement : () => void run("connect", () => desktopApi.connect())}
              >
                {googleGrantExpired ? (
                  <Upload />
                ) : busyAction === "connect" || connecting ? (
                  <Loader2 className="spin" />
                ) : vpnNeedsAdmin ? (
                  <ShieldCheck />
                ) : (
                  <Play />
                )}
                {googleGrantExpired ? "Import renewed profile" : vpnNeedsAdmin ? "Admin required" : "Connect"}
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
            {connected ? (
              <button
                type="button"
                disabled={exitProbeRunning}
                onClick={() => void testExit()}
              >
                {exitProbeRunning ? <Loader2 className="spin" /> : <RefreshCw />}
                {exitProbeRunning ? "Testing exit…" : "Test exit"}
              </button>
            ) : null}
            <button
              type="button"
              disabled={copyLocalHttpDisabled}
              onClick={() => void copyEndpoint("Local HTTP", httpAddress)}
            >
              {copyStatus.startsWith("Local HTTP") ? <Check /> : <Copy />}
              {copyStatus.startsWith("Local HTTP") ? "Copied local HTTP" : "Copy local HTTP"}
            </button>
            {!copyLanSocksDisabled ? (
              <button type="button" onClick={() => void copyEndpoint("LAN SOCKS", lanSocksAddress)}>
                {copyStatus.startsWith("LAN SOCKS") ? <Check /> : <Copy />}
                {copyStatus.startsWith("LAN SOCKS") ? "Copied LAN SOCKS" : "Copy LAN SOCKS"}
              </button>
            ) : null}
            {!copyLanHttpDisabled ? (
              <button type="button" onClick={() => void copyEndpoint("LAN HTTP", lanHttpAddress)}>
                {copyStatus.startsWith("LAN HTTP") ? <Check /> : <Copy />}
                {copyStatus.startsWith("LAN HTTP") ? "Copied LAN HTTP" : "Copy LAN HTTP"}
              </button>
            ) : null}
          </div>

          {connected ? (
            <div className="warning-note" style={{ gridColumn: "1 / -1" }} role="status">
              {exitProbe?.status === "verified"
                ? `Exit responded through Google Drive in ${exitProbe.roundTripMs} ms. This is one sample; website access is not checked.`
                : exitProbe?.status === "unverified"
                  ? "Exit could not be verified. It may be offline, running an older Skirk, or need Google authorization renewed. Ask the exit owner to check its status."
                  : exitProbeError || "Local connection is ready. Use Test exit to verify a round trip to the exit on demand."}
            </div>
          ) : null}

          {snapshot?.platform === "macos" && selectedMode === "proxy" && runtimeProfile ? (
            <div className="warning-note" style={{ gridColumn: "1 / -1" }} role="note">
              <strong>Mac proxy setup.</strong> Connect starts local listeners; browser and macOS traffic do not use
              them automatically. Set your browser or macOS HTTP and HTTPS proxy to{" "}
              <code>127.0.0.1:{runtimeProfile.httpPort}</code>, or use SOCKS5 at{" "}
              <code>127.0.0.1:{runtimeProfile.socksPort}</code> with remote DNS. Check the tunnel with{" "}
              <code>curl --socks5-hostname 127.0.0.1:{runtimeProfile.socksPort} https://example.com/</code>.
            </div>
          ) : null}

          {snapshot?.platform === "macos" && selectedMode === "system" ? (
            <div className="warning-note" style={{ gridColumn: "1 / -1" }} role="note">
              <strong>Mac system proxy.</strong> Connect sets the active network service's HTTP and HTTPS proxy for
              apps that use macOS proxy settings. Disconnect restores the previous settings. If that service already
              has another proxy or automatic proxy setup, use local Proxy mode instead. This does not capture apps
              that ignore system proxy settings.
            </div>
          ) : null}

          <SettingsStrip
            snapshot={snapshot}
            performance={performance}
            onOpenAdvanced={() => setAdvancedOpen(true)}
          />

          <div className="metric-grid" role="group" aria-label="Runtime details">
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
                    mode={selectedMode}
                    selected={profile.id === selectedProfile?.id}
                    disabled={runtimeBusy || connected}
                    onSelect={() => void run("select", () => desktopApi.selectProfile(profile.id))}
                    onDelete={() => setPendingDeleteProfile(profile)}
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

          <details
            ref={importPanelRef}
            className="panel disclosure-panel import-panel"
            id="import"
            open={importOpen}
            onToggle={(event) => setImportOpen(event.currentTarget.open)}
          >
            <DisclosureSummary
              icon={<Upload />}
              title={replaceProfileId ? "Replace profile" : "Import profile"}
              detail={replaceProfileId ? replacingProfile?.name ?? "Select a profile" : portValid ? "Ready" : "Port must be 1024-65535"}
            />

            <div className="import-form">
              <fieldset className="import-target">
                <legend>Import action</legend>
                <label>
                  <input type="radio" name="import-action" checked={replaceProfileId === null} onChange={() => setReplaceProfileId(null)} />
                  <span>New profile</span>
                </label>
                <label>
                  <input type="radio" name="import-action" disabled={!selectedProfile || runtimeBusy || connected} checked={replaceProfileId !== null} onChange={() => setReplaceProfileId(selectedProfile?.id ?? null)} />
                  <span>Replace selected profile</span>
                </label>
              </fieldset>

              <div className="button-row">
                <input
                  ref={profileFileRef}
                  type="file"
                  hidden
                  accept=".skirk,.json,text/plain,application/json"
                  onChange={(event) => {
                    const file = event.currentTarget.files?.[0];
                    event.currentTarget.value = "";
                    if (file) void readProfileFile(file);
                  }}
                />
                <button type="button" disabled={runtimeBusy || connected} onClick={() => profileFileRef.current?.click()}>
                  <FileText /> Choose client.skirk file
                </button>
                <button type="button" disabled={busy} onClick={() => void pasteConfig()}>
                  <ClipboardPaste /> Paste text
                </button>
              </div>

              {replacingProfile ? (
                <p className="replacement-note">
                  Replacing <strong>{replacingProfile.name}</strong> keeps SOCKS {replacingProfile.socksPort}, HTTP {replacingProfile.httpPort}, {googleRouteLabel(replacingProfile.routeMode)}, and {performancePresetLabel(replacingProfile.performance.preset)} settings.
                </p>
              ) : (
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
              )}

              <label htmlFor={rawConfigId}>
                <span>Client profile text</span>
                <textarea
                  id={rawConfigId}
                  value={rawConfig}
                  onChange={(event) => {
                    setRawConfig(event.target.value);
                    setProfileFileError("");
                  }}
                  spellCheck={false}
                />
              </label>

              {profileFileError ? <small className="field-error" role="alert">{profileFileError}</small> : null}

              {!replaceProfileId ? <label className="switch-row">
                <input
                  type="checkbox"
                  checked={shareLan}
                  onChange={(event) => setShareLan(event.target.checked)}
                />
                <span>
                  <strong>Share on LAN</strong>
                  <small>Listen on 0.0.0.0 instead of loopback.</small>
                </span>
              </label> : null}

              <div className="button-row">
                <button
                  type="button"
                  className="primary"
                  disabled={importDisabled}
                  onClick={() => void run("import", async () => {
                    const next = replaceProfileId && replacingProfile
                      ? await desktopApi.replaceProfileConfig(replaceProfileId, rawConfig)
                      : await desktopApi.importConfig(profileName, rawConfig, portNumber, httpPortNumber, shareLan);
                    setRawConfig("");
                    setProfileFileError("");
                    setReplaceProfileId(null);
                    setImportOpen(false);
                    return next;
                  })}
                >
                  {busyAction === "import" ? <Loader2 className="spin" /> : <Upload />}
                  {replaceProfileId ? "Replace profile" : "Import profile"}
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
        {pendingDeleteProfile ? (
          <ConfirmDeleteDialog
            profileName={pendingDeleteProfile.name}
            disabled={runtimeBusy || connected}
            onClose={() => setPendingDeleteProfile(null)}
            onConfirm={() => {
              const profileId = pendingDeleteProfile.id;
              setPendingDeleteProfile(null);
              void run("delete", () => desktopApi.deleteProfile(profileId));
            }}
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
  mode,
  selected,
  disabled,
  onSelect,
  onDelete,
}: {
  profile: ClientProfile;
  mode: ConnectionMode;
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
          {googleRouteLabel(profile.routeMode)} · {profileRowDetail(profile, mode)}
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
      role="radio"
      aria-checked={active}
      tabIndex={active ? 0 : -1}
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

function handleRadioGroupKeyDown(event: KeyboardEvent<HTMLDivElement>) {
  if (!["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown", "Home", "End"].includes(event.key)) return;
  const choices = Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>(":scope > button[role='radio']:not(:disabled), :scope > .route-selector > button[role='radio']:not(:disabled)"));
  if (!choices.length) return;
  event.preventDefault();
  const current = choices.indexOf(document.activeElement as HTMLButtonElement);
  const next = event.key === "Home" ? 0 : event.key === "End" ? choices.length - 1 :
    (current + (event.key === "ArrowRight" || event.key === "ArrowDown" ? 1 : -1) + choices.length) % choices.length;
  choices[next].focus();
  choices[next].click();
}

function StatusCard({ phase, address }: { phase: string; address: string }) {
  return (
    <div className={`status-card ${phase}`}>
      <span>Status</span>
      <strong>{phase === "error" ? "Needs attention" : formatPhase(phase)}</strong>
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
  const units = snapshot?.metrics?.recentQuotaPerMinute.units ?? 0;
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
  const dialogRef = useRef<HTMLElement>(null);
  useEffect(() => {
    const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    dialogRef.current?.querySelector<HTMLButtonElement>("button")?.focus();
    return () => previousFocus?.focus();
  }, []);

  function handleDialogKeyDown(event: KeyboardEvent<HTMLElement>) {
    if (event.key === "Escape") {
      event.stopPropagation();
      onClose();
      return;
    }
    if (event.key !== "Tab") return;
    const focusable = Array.from(dialogRef.current?.querySelectorAll<HTMLElement>("button:not(:disabled), input:not(:disabled), textarea:not(:disabled), a[href]") ?? [])
      .filter((element) => element.getClientRects().length > 0);
    if (!focusable.length) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }

  return (
    <div className="modal-backdrop" role="presentation" onMouseDown={onClose}>
      <section
        ref={dialogRef}
        className="advanced-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="advanced-settings-title"
        onMouseDown={(event) => event.stopPropagation()}
        onKeyDown={handleDialogKeyDown}
      >
        <header>
          <div>
            <h2 id="advanced-settings-title">Performance and Drive usage</h2>
          </div>
          <button type="button" className="icon-button" aria-label="Close advanced settings" onClick={onClose}>
            <X />
          </button>
        </header>

        <QuotaPanel snapshot={snapshot} />
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

function ConfirmDeleteDialog({
  profileName,
  disabled,
  onClose,
  onConfirm,
}: {
  profileName: string;
  disabled: boolean;
  onClose: () => void;
  onConfirm: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const dialog = dialogRef.current;
    dialog?.showModal();
    cancelRef.current?.focus();
    return () => {
      dialog?.close();
      previousFocus?.focus();
    };
  }, []);

  return (
    <dialog ref={dialogRef} className="delete-dialog" onCancel={onClose}>
      <h2>Remove {profileName}?</h2>
      <p>This removes the saved profile from this computer. You will need client.skirk to import it again.</p>
      <div className="button-row">
        <button ref={cancelRef} type="button" onClick={onClose}>Cancel</button>
        <button type="button" className="danger" disabled={disabled} onClick={onConfirm}>Remove profile</button>
      </div>
    </dialog>
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
  const [draft, setDraft] = useState(String(value));
  const [editing, setEditing] = useState(false);
  useEffect(() => {
    if (!editing) setDraft(String(value));
  }, [editing, value]);

  function commit() {
    setEditing(false);
    if (draft === "") {
      setDraft(String(value));
      return;
    }
    const next = Math.min(max, Math.max(min, Number(draft)));
    setDraft(String(next));
    if (next !== value) onChange(next);
  }

  return (
    <label>
      <span>
        {label}
        {suffix ? ` (${suffix})` : ""}
      </span>
      <input
        inputMode="numeric"
        pattern="[0-9]*"
        disabled={disabled}
        value={draft}
        onFocus={() => setEditing(true)}
        onChange={(event) => setDraft(event.target.value.replace(/\D/g, "").slice(0, 5))}
        onBlur={commit}
        onKeyDown={(event) => { if (event.key === "Enter") event.currentTarget.blur(); }}
      />
    </label>
  );
}

function QuotaPanel({ snapshot }: { snapshot: DesktopSnapshot | null }) {
  const metrics = snapshot?.metrics;
  const unitsPerMinute = metrics?.recentQuotaPerMinute.units ?? 0;
  const errors = metrics?.recentQuotaPerMinute.errors ?? 0;
  const errorReason = metrics?.recentQuotaPerMinute.lastErrorReason;
  return (
    <div className="quota-copy">
      <div className="quota-meter-header">
        <div>
          <strong>{drivePressureLabel(unitsPerMinute, errors, errorReason)}</strong>
          <span>{metrics ? "Measured from this desktop process" : "Connect to measure active use"}</span>
        </div>
        <span>{metrics ? `${formatNumber(Math.round(unitsPerMinute))} units/min` : "—"}</span>
      </div>
      <UsageMeter units={unitsPerMinute} errors={errors} />
      <div className="quota-line">
        <span>Recent local estimate</span>
        <strong>{metrics ? `${formatNumber(Math.round(unitsPerMinute))} units/min` : "Connect to measure"}</strong>
      </div>
      <div className="quota-line">
        <span>Client receive lists with zero streams</span>
        <strong>0 units/min</strong>
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
        With no active streams, this client makes no Drive receive-list calls. Transfers and the exit still use Drive;
        these figures cover this client only, not the shared Google project.
      </small>
      {metrics?.recentQuotaOps ? <code>{metrics.recentQuotaOps}</code> : null}
    </div>
  );
}

function UsageMeter({ units, errors }: { units: number; errors: number }) {
  const pressure = Math.round(Math.max(0, Math.min(100, drivePressureFraction(units) * 100)));
  const width = `${Math.max(3, pressure)}%`;
  const level =
    errors > 0 ? "error" : drivePressureFraction(units) >= 0.7 ? "high" : drivePressureFraction(units) >= 0.3 ? "medium" : "low";
  return (
    <div className={`usage-meter ${level}`} role="progressbar" aria-label="Drive API pressure" aria-valuemin={0} aria-valuemax={100} aria-valuenow={pressure}>
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

function profileEndpointSummary(profile: ClientProfile | null | undefined, socks: string, http: string, mode: ConnectionMode) {
  if (!profile) {
    return "-";
  }
  if (profile.shareLan && mode !== "vpn") {
    return `LAN SOCKS ${socks} · LAN HTTP ${http}`;
  }
  return `SOCKS ${socks} · HTTP ${http}`;
}

function profileRowDetail(profile: ClientProfile, mode: ConnectionMode) {
  if (profile.shareLan && mode === "vpn") {
    return `VPN uses local listeners · SOCKS ${profile.socksPort} · HTTP ${profile.httpPort} · ${performancePresetLabel(profile.performance.preset)}`;
  }
  if (profile.shareLan) {
    return `LAN sharing · SOCKS ${profile.socksPort} · HTTP ${profile.httpPort}`;
  }
  return `Local only · SOCKS ${profile.socksPort} · HTTP ${profile.httpPort} · ${performancePresetLabel(profile.performance.preset)}`;
}

function googleRouteLabel(mode: string) {
  switch (mode) {
    case "google_front_pinned": return "Google front (www.google.com SNI)";
    case "google_front": return "Google front (automatic IP)";
    case "google_front_h1": return "Google front (HTTP/1.1)";
    case "google_front_h1_pinned": return "Google front (pinned HTTP/1.1)";
    case "direct": return "Direct Google";
    case "real_pinned": return "Direct Google (pinned IP)";
    default: return "Other Google route";
  }
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
  if (compact.includes("invalidgrant") || compact.includes("unauthorized") || compact === "status401") {
    return "Google access rejected";
  }
  if (compact.includes("notfound")) {
    return "Drive mailbox missing";
  }
  if (compact.includes("timeout") || compact.includes("deadline")) {
    return "Drive timeout";
  }
  return "Drive errors";
}

function isGoogleGrantExpired(...messages: Array<string | undefined>) {
  return messages.some((message) => {
    const compact = (message ?? "").toLowerCase().replace(/[^a-z0-9]/g, "");
    return compact.includes("invalidgrant") || compact.includes("unauthorized") || compact.includes("status401");
  });
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
    parts.push("System proxy");
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
