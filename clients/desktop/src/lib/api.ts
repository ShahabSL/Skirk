import { invoke } from "@tauri-apps/api/core";

export type ConnectionPhase = "disconnected" | "connecting" | "connected" | "disconnecting" | "error";
export type ConnectionMode = "proxy" | "system" | "vpn";

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
  driveSpace: string;
  driveFolderId: string;
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
    systemProxyEnabled: boolean;
    tunnelActive: boolean;
    tunnelInterfaceName: string | null;
    message: string;
  };
  logsDir: string;
  configDir: string;
  logTail: string;
  tunnelLogTail: string;
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
    driveSpace: "appDataFolder",
    driveFolderId: "",
  },
];
let mockSelectedProfileId: string | null = "mock-profile";
let mockConnected = false;

function mockSnapshot(): DesktopSnapshot {
  const profile = mockProfiles.find((item) => item.id === mockSelectedProfileId) ?? mockProfiles[0];
  const socksAddress = profile ? `${profile.shareLan ? "0.0.0.0" : "127.0.0.1"}:${profile.socksPort}` : null;
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
      httpAddress: mockConnected && profile ? `127.0.0.1:${profile.httpPort}` : null,
      lanAddresses: mockConnected && profile?.shareLan ? [`192.168.1.20:${profile.socksPort}`] : [],
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
    platform: "windows",
    capabilities: {
      systemProxySupported: true,
      vpnModeSupported: true,
      vpnRequiresAdmin: true,
      vpnAdmin: false,
      vpnSidecarPresent: true,
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
        driveSpace: "appDataFolder",
        driveFolderId: "",
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

let mockMode: ConnectionMode = "proxy";

function modeLabel(mode: ConnectionMode) {
  if (mode === "system") {
    return "system proxy";
  }
  if (mode === "vpn") {
    return "VPN";
  }
  return "proxy";
}
