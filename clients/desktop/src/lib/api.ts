import { invoke } from "@tauri-apps/api/core";

export type ConnectionPhase = "disconnected" | "connecting" | "connected" | "disconnecting" | "error";
export type ConnectionMode = "proxy" | "system" | "vpn";
export type PerformancePreset = "lower_usage" | "recommended" | "responsive" | "bulk_transfer" | "custom";

export type ClientPerformanceSettings = {
  preset: PerformancePreset;
  pollMs: number;
  uploadConcurrency: number;
  downloadConcurrency: number;
  burstPoll: boolean;
  burstPollMs: number;
  burstPollWindowMs: number;
};

export type ClientProfile = {
  id: string;
  name: string;
  configPath: string;
  socksHost: string;
  socksPort: number;
  httpHost: string;
  httpPort: number;
  shareLan: boolean;
  routeMode: string;
  googleIp: string;
  driveSpace: string;
  driveFolderId: string;
  performance: ClientPerformanceSettings;
};

export type RuntimeMetrics = {
  version: number;
  role: string;
  startedAt: string;
  updatedAt: string;
  durationSeconds: number;
  routeMode: string;
  transport: string;
  pollMs: number;
  uploadConcurrency: number;
  downloadConcurrency: number;
  burstPoll: boolean;
  estimatedProjectUnitsPerMin: number;
  estimatedUserUnitsPerMin: number;
  recentQuota: {
    calls: number;
    units: number;
    errors: number;
    lastErrorReason?: string;
    responseBytes: number;
  };
  recentQuotaPerMinute: {
    calls: number;
    units: number;
    errors: number;
    lastErrorReason?: string;
    responseBytes: number;
  };
  recentQuotaOps: string;
  driveBackoff?: {
    active: boolean;
    until?: string;
    waitSeconds?: number;
    reason?: string;
    op?: string;
    failures?: number;
  } | null;
  note: string;
};

export type DesktopSnapshot = {
  profiles: ClientProfile[];
  selectedProfileId: string | null;
  connection: {
    phase: ConnectionPhase;
    mode: ConnectionMode;
    activeProfileId: string | null;
    pid: number | null;
    tunnelPid: number | null;
    socksAddress: string | null;
    httpAddress: string | null;
    lanAddresses: string[];
    lanHttpAddresses: string[];
    systemProxyEnabled: boolean;
    tunnelActive: boolean;
    tunnelInterfaceName: string | null;
    message: string;
  };
  logsDir: string;
  configDir: string;
  logTail: string;
  tunnelLogTail: string;
  metrics: RuntimeMetrics | null;
  platform: string;
  capabilities: {
    systemProxySupported: boolean;
    vpnModeSupported: boolean;
    vpnRequiresAdmin: boolean;
    vpnAdmin: boolean;
    vpnSidecarPresent: boolean;
  };
};

const isTauriRuntime =
  typeof window !== "undefined" &&
  "__TAURI_INTERNALS__" in (window as unknown as { __TAURI_INTERNALS__?: unknown });
const useBrowserPreview = import.meta.env.DEV && !isTauriRuntime;

let mockProfiles: ClientProfile[] = [
  {
    id: "mock-profile",
    name: "Skirk profile",
    configPath: "portable-data/config/mock.skirk",
    socksHost: "127.0.0.1",
    socksPort: 18080,
    httpHost: "127.0.0.1",
    httpPort: 18081,
    shareLan: false,
    routeMode: "google_front_pinned",
    googleIp: "216.239.38.120",
    driveSpace: "appDataFolder",
    driveFolderId: "",
    performance: recommendedPerformance(),
  },
];
let mockSelectedProfileId: string | null = "mock-profile";
let mockConnected = false;
const mockPlatform = browserPreviewPlatform();

function mockSnapshot(): DesktopSnapshot {
  const profile = mockProfiles.find((item) => item.id === mockSelectedProfileId) ?? mockProfiles[0];
  const socksAddress = profile ? `${profile.shareLan ? "0.0.0.0" : "127.0.0.1"}:${profile.socksPort}` : null;
  const httpAddress = profile ? `${profile.shareLan ? "0.0.0.0" : "127.0.0.1"}:${profile.httpPort}` : null;
  return {
    profiles: [...mockProfiles],
    selectedProfileId: profile?.id ?? null,
    connection: {
      phase: mockConnected ? "connected" : "disconnected",
      mode: mockMode,
      activeProfileId: mockConnected ? profile?.id ?? null : null,
      pid: mockConnected ? 4242 : null,
      tunnelPid: mockConnected && mockMode === "vpn" ? 4243 : null,
      socksAddress: mockConnected ? socksAddress : null,
      httpAddress: mockConnected ? httpAddress : null,
      lanAddresses: mockConnected && profile?.shareLan ? [`192.168.1.20:${profile.socksPort}`] : [],
      lanHttpAddresses: mockConnected && profile?.shareLan ? [`192.168.1.20:${profile.httpPort}`] : [],
      systemProxyEnabled: mockConnected && mockMode === "system",
      tunnelActive: mockConnected && mockMode === "vpn",
      tunnelInterfaceName: mockConnected && mockMode === "vpn" ? "Skirk Tunnel" : null,
      message: mockConnected ? `Connected in ${modeLabel(mockMode)} mode` : "Disconnected",
    },
    logsDir: "portable-data/logs",
    configDir: "portable-data/config",
    logTail: mockConnected
      ? "skirk client SOCKS5 listening on 127.0.0.1:18080\\nmailbox download direction=down status=ok duration=452ms"
      : "",
    tunnelLogTail: mockConnected && mockMode === "vpn" ? "sing-box started\\nTUN interface Skirk Tunnel ready" : "",
    metrics: mockConnected
      ? {
          version: 1,
          role: "client",
          startedAt: new Date(Date.now() - 60_000).toISOString(),
          updatedAt: new Date().toISOString(),
          durationSeconds: 60,
          routeMode: "google_front_pinned",
          transport: "muxv4",
          pollMs: profile?.performance.pollMs ?? 1000,
          uploadConcurrency: profile?.performance.uploadConcurrency ?? 8,
          downloadConcurrency: profile?.performance.downloadConcurrency ?? 16,
          burstPoll: profile?.performance.burstPoll ?? false,
          estimatedProjectUnitsPerMin: 1_000_000,
          estimatedUserUnitsPerMin: 325_000,
          recentQuota: { calls: 66, units: 6600, errors: 0, responseBytes: 8192 },
          recentQuotaPerMinute: { calls: 60, units: 6000, errors: 0, responseBytes: 7400 },
          recentQuotaOps: "list:60/6000u",
          driveBackoff: null,
          note: "Estimated local Drive API units from this Skirk process only; not project-wide remaining quota.",
        }
      : null,
    platform: mockPlatform,
    capabilities: {
      systemProxySupported: mockPlatform === "windows",
      vpnModeSupported: mockPlatform === "windows",
      vpnRequiresAdmin: mockPlatform === "windows",
      vpnAdmin: false,
      vpnSidecarPresent: mockPlatform === "windows",
    },
  };
}

const tauriApi = {
  loadSnapshot: () => invoke<DesktopSnapshot>("load_snapshot"),
  importConfig: (name: string, rawConfig: string, socksPort: number, httpPort: number, shareLan: boolean) =>
    invoke<DesktopSnapshot>("import_config", { name, rawConfig, socksPort, httpPort, shareLan }),
  deleteProfile: (profileId: string) => invoke<DesktopSnapshot>("delete_profile", { profileId }),
  selectProfile: (profileId: string | null) =>
    invoke<DesktopSnapshot>("select_profile", { profileId }),
  setConnectionMode: (mode: ConnectionMode) => invoke<DesktopSnapshot>("set_connection_mode", { mode }),
  updateProfilePerformance: (profileId: string, performance: ClientPerformanceSettings) =>
    invoke<DesktopSnapshot>("update_profile_performance", { profileId, performance }),
  connect: () => invoke<DesktopSnapshot>("connect"),
  disconnect: () => invoke<DesktopSnapshot>("disconnect"),
};

const browserPreviewApi = {
  loadSnapshot: async () => mockSnapshot(),
  importConfig: async (name: string, _rawConfig: string, socksPort: number, httpPort: number, shareLan: boolean) => {
    const id = `mock-${Date.now()}`;
    mockProfiles = [
      ...mockProfiles,
      {
        id,
        name: name.trim() || "Skirk profile",
        configPath: `portable-data/config/${id}.skirk`,
        socksHost: shareLan ? "0.0.0.0" : "127.0.0.1",
        socksPort,
        httpHost: shareLan ? "0.0.0.0" : "127.0.0.1",
        httpPort,
        shareLan,
        routeMode: "google_front_pinned",
        googleIp: "216.239.38.120",
        driveSpace: "appDataFolder",
        driveFolderId: "",
        performance: recommendedPerformance(),
      },
    ];
    mockSelectedProfileId = id;
    return mockSnapshot();
  },
  deleteProfile: async (profileId: string) => {
    mockProfiles = mockProfiles.filter((profile) => profile.id !== profileId);
    if (mockSelectedProfileId === profileId) {
      mockSelectedProfileId = mockProfiles[0]?.id ?? null;
    }
    return mockSnapshot();
  },
  selectProfile: async (profileId: string | null) => {
    mockSelectedProfileId = profileId;
    return mockSnapshot();
  },
  setConnectionMode: async (mode: ConnectionMode) => {
    mockMode = mode;
    return mockSnapshot();
  },
  updateProfilePerformance: async (profileId: string, performance: ClientPerformanceSettings) => {
    mockProfiles = mockProfiles.map((profile) =>
      profile.id === profileId ? { ...profile, performance } : profile,
    );
    return mockSnapshot();
  },
  connect: async () => {
    mockConnected = true;
    return mockSnapshot();
  },
  disconnect: async () => {
    mockConnected = false;
    return mockSnapshot();
  },
};

export const desktopApi = useBrowserPreview ? browserPreviewApi : tauriApi;

export function recommendedPerformance(): ClientPerformanceSettings {
  return {
    preset: "recommended",
    pollMs: 1000,
    uploadConcurrency: 8,
    downloadConcurrency: 16,
    burstPoll: false,
    burstPollMs: 75,
    burstPollWindowMs: 5000,
  };
}

let mockMode: ConnectionMode = "proxy";

function browserPreviewPlatform() {
  if (typeof window === "undefined") {
    return "windows";
  }
  const value = new URLSearchParams(window.location.search).get("platform")?.toLowerCase();
  if (value === "linux") {
    return "linux";
  }
  return "windows";
}

function modeLabel(mode: ConnectionMode) {
  if (mode === "system") {
    return "system proxy";
  }
  if (mode === "vpn") {
    return "VPN";
  }
  return "proxy";
}
