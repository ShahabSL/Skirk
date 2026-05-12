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

export const desktopApi = {
  loadSnapshot: () => invoke<DesktopSnapshot>("load_snapshot"),
  importConfig: (name: string, rawConfig: string, socksPort: number, shareLan: boolean) =>
    invoke<DesktopSnapshot>("import_config", { name, rawConfig, socksPort, shareLan }),
  deleteProfile: (profileId: string) => invoke<DesktopSnapshot>("delete_profile", { profileId }),
  selectProfile: (profileId: string | null) =>
    invoke<DesktopSnapshot>("select_profile", { profileId }),
  connect: () => invoke<DesktopSnapshot>("connect"),
  disconnect: () => invoke<DesktopSnapshot>("disconnect"),
};
