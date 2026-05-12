import { invoke } from "@tauri-apps/api/core";

export type ConnectionPhase = "disconnected" | "connecting" | "connected" | "disconnecting" | "error";

export type ClientProfile = {
  id: string;
  name: string;
  configPath: string;
  socksHost: string;
  socksPort: number;
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
    activeProfileId: string | null;
    pid: number | null;
    socksAddress: string | null;
    lanAddresses: string[];
    message: string;
  };
  logsDir: string;
  configDir: string;
  logTail: string;
  platform: string;
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
      activeProfileId: mockConnected ? profile?.id ?? null : null,
      pid: mockConnected ? 4242 : null,
      socksAddress: mockConnected ? socksAddress : null,
      lanAddresses: mockConnected && profile?.shareLan ? [`192.168.1.20:${profile.socksPort}`] : [],
      message: mockConnected ? "Connected" : "Disconnected",
    },
    logsDir: "portable-data/logs",
    configDir: "portable-data/config",
    logTail: mockConnected
      ? "skirk client SOCKS5 listening on 127.0.0.1:18080\\nmailbox download direction=down status=ok duration=452ms"
      : "",
    platform: "windows",
  };
}

const tauriApi = {
  loadSnapshot: () => invoke<DesktopSnapshot>("load_snapshot"),
  importConfig: (name: string, rawConfig: string, socksPort: number, shareLan: boolean) =>
    invoke<DesktopSnapshot>("import_config", { name, rawConfig, socksPort, shareLan }),
  deleteProfile: (profileId: string) => invoke<DesktopSnapshot>("delete_profile", { profileId }),
  selectProfile: (profileId: string | null) =>
    invoke<DesktopSnapshot>("select_profile", { profileId }),
  connect: () => invoke<DesktopSnapshot>("connect"),
  disconnect: () => invoke<DesktopSnapshot>("disconnect"),
};

const browserPreviewApi = {
  loadSnapshot: async () => mockSnapshot(),
  importConfig: async (name: string, _rawConfig: string, socksPort: number, shareLan: boolean) => {
    const id = `mock-${Date.now()}`;
    mockProfiles = [
      ...mockProfiles,
      {
        id,
        name: name.trim() || "Skirk profile",
        configPath: `portable-data/config/${id}.skirk`,
        socksHost: shareLan ? "0.0.0.0" : "127.0.0.1",
        socksPort,
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
