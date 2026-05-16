mod system_proxy;

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::{
    collections::BTreeSet,
    fs::{self, OpenOptions},
    net::{TcpListener, TcpStream},
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    sync::Mutex,
    time::{Duration, Instant},
};
use tauri::{Manager, State};

use system_proxy::SystemProxyManager;

#[cfg(windows)]
use std::os::windows::process::CommandExt;

#[cfg(windows)]
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

#[cfg(windows)]
use windows_sys::Win32::{
    Foundation::CloseHandle,
    Security::{GetTokenInformation, TokenElevation, TOKEN_ELEVATION, TOKEN_QUERY},
    System::Threading::{GetCurrentProcess, OpenProcessToken},
};

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ClientProfile {
    id: String,
    name: String,
    config_path: String,
    socks_host: String,
    socks_port: u16,
    #[serde(default = "default_http_host")]
    http_host: String,
    #[serde(default = "default_http_port")]
    http_port: u16,
    #[serde(default)]
    share_lan: bool,
    route_mode: String,
    drive_space: String,
    drive_folder_id: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
enum ConnectionPhase {
    Disconnected,
    Connecting,
    Connected,
    Disconnecting,
    Error,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
enum ConnectionMode {
    Proxy,
    System,
    Vpn,
}

impl Default for ConnectionMode {
    fn default() -> Self {
        Self::Proxy
    }
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct ConnectionStatus {
    phase: ConnectionPhase,
    mode: ConnectionMode,
    active_profile_id: Option<String>,
    pid: Option<u32>,
    tunnel_pid: Option<u32>,
    socks_address: Option<String>,
    http_address: Option<String>,
    lan_addresses: Vec<String>,
    system_proxy_enabled: bool,
    tunnel_active: bool,
    tunnel_interface_name: Option<String>,
    message: String,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct PlatformCapabilities {
    system_proxy_supported: bool,
    vpn_mode_supported: bool,
    vpn_requires_admin: bool,
    vpn_admin: bool,
    vpn_sidecar_present: bool,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct DesktopSnapshot {
    profiles: Vec<ClientProfile>,
    selected_profile_id: Option<String>,
    connection: ConnectionStatus,
    logs_dir: String,
    config_dir: String,
    log_tail: String,
    tunnel_log_tail: String,
    platform: String,
    capabilities: PlatformCapabilities,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
struct Settings {
    selected_profile_id: Option<String>,
    #[serde(default)]
    connection_mode: ConnectionMode,
}

struct ManagedClient {
    child: Child,
    profile_id: String,
    mode: ConnectionMode,
    socks_address: String,
    http_address: String,
    system_proxy_enabled: bool,
}

struct ManagedTunnel {
    child: Child,
    interface_name: String,
}

#[derive(Default)]
struct RuntimeState {
    client: Option<ManagedClient>,
    tunnel: Option<ManagedTunnel>,
    phase: ConnectionPhase,
    message: String,
}

impl Default for ConnectionPhase {
    fn default() -> Self {
        Self::Disconnected
    }
}

fn default_http_host() -> String {
    "127.0.0.1".into()
}

fn default_http_port() -> u16 {
    18081
}

struct DesktopRuntime {
    paths: AppPaths,
    resource_dir: Option<PathBuf>,
    inner: Mutex<RuntimeState>,
}

#[derive(Clone)]
struct AppPaths {
    config_dir: PathBuf,
    runtime_dir: PathBuf,
    logs_dir: PathBuf,
    profiles_file: PathBuf,
    settings_file: PathBuf,
}

impl DesktopRuntime {
    fn new(paths: AppPaths, resource_dir: Option<PathBuf>) -> Self {
        Self {
            paths,
            resource_dir,
            inner: Mutex::new(RuntimeState::default()),
        }
    }

    fn snapshot(&self) -> Result<DesktopSnapshot, String> {
        let profiles = load_profiles(&self.paths)?;
        let mut settings = load_settings(&self.paths)?;
        if settings
            .selected_profile_id
            .as_ref()
            .map(|id| profiles.iter().any(|profile| &profile.id == id))
            .unwrap_or(false)
            == false
        {
            settings.selected_profile_id = profiles.first().map(|profile| profile.id.clone());
            save_settings(&self.paths, &settings)?;
        }

        let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
        refresh_state(&mut state, &self.paths);
        let connection = if let Some(client) = state.client.as_ref() {
            ConnectionStatus {
                phase: state.phase.clone(),
                mode: client.mode.clone(),
                active_profile_id: Some(client.profile_id.clone()),
                pid: Some(client.child.id()),
                tunnel_pid: state.tunnel.as_ref().map(|tunnel| tunnel.child.id()),
                socks_address: Some(client.socks_address.clone()),
                http_address: Some(client.http_address.clone()),
                lan_addresses: share_addresses(&client.socks_address),
                system_proxy_enabled: client.system_proxy_enabled,
                tunnel_active: state.tunnel.is_some(),
                tunnel_interface_name: state
                    .tunnel
                    .as_ref()
                    .map(|tunnel| tunnel.interface_name.clone()),
                message: state.message.clone(),
            }
        } else {
            ConnectionStatus {
                phase: state.phase.clone(),
                mode: settings.connection_mode.clone(),
                active_profile_id: None,
                pid: None,
                tunnel_pid: None,
                socks_address: None,
                http_address: None,
                lan_addresses: Vec::new(),
                system_proxy_enabled: false,
                tunnel_active: false,
                tunnel_interface_name: None,
                message: state.message.clone(),
            }
        };

        Ok(DesktopSnapshot {
            profiles,
            selected_profile_id: settings.selected_profile_id,
            connection,
            logs_dir: self.paths.logs_dir.display().to_string(),
            config_dir: self.paths.config_dir.display().to_string(),
            log_tail: read_log_tail(&client_log_path(&self.paths), 80),
            tunnel_log_tail: read_log_tail(&tunnel_log_path(&self.paths), 80),
            platform: std::env::consts::OS.to_string(),
            capabilities: self.platform_capabilities(),
        })
    }

    fn import_config(
        &self,
        name: String,
        raw_config: String,
        socks_port: u16,
        http_port: u16,
        share_lan: bool,
    ) -> Result<(), String> {
        let raw_config = raw_config.trim();
        if raw_config.is_empty() {
            return Err("client config is empty".into());
        }
        let stored_config =
            extract_inline_config(raw_config).unwrap_or_else(|| raw_config.to_string());
        let parsed = self.decode_config(&stored_config)?;
        let route_mode = parsed
            .pointer("/route/mode")
            .and_then(Value::as_str)
            .unwrap_or("direct")
            .to_string();
        let drive_space = parsed
            .pointer("/drive/space")
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string();
        let drive_folder_id = parsed
            .pointer("/drive/folder_id")
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string();
        if drive_space != "appDataFolder" && drive_folder_id.is_empty() {
            return Err("client config is missing a Drive mailbox".into());
        }
        if socks_port == http_port {
            return Err("SOCKS and HTTP proxy ports must be different".into());
        }
        let id = format!("profile-{}", epoch_millis());
        let config_path = self
            .paths
            .config_dir
            .join(if looks_like_inline_config(&stored_config) {
                format!("{id}.skirk")
            } else {
                format!("{id}.json")
            });
        fs::write(&config_path, &stored_config)
            .map_err(|error| format!("failed to write config: {error}"))?;

        let profile = ClientProfile {
            id: id.clone(),
            name: if name.trim().is_empty() {
                "Skirk profile".into()
            } else {
                name.trim().into()
            },
            config_path: config_path.display().to_string(),
            socks_host: if share_lan {
                "0.0.0.0".into()
            } else {
                "127.0.0.1".into()
            },
            socks_port,
            http_host: if share_lan {
                "0.0.0.0".into()
            } else {
                "127.0.0.1".into()
            },
            http_port,
            share_lan,
            route_mode,
            drive_space,
            drive_folder_id,
        };
        let mut profiles = load_profiles(&self.paths)?;
        profiles.retain(|existing| existing.id != profile.id);
        profiles.push(profile);
        save_profiles(&self.paths, &profiles)?;
        save_settings(
            &self.paths,
            &Settings {
                selected_profile_id: Some(id),
                ..load_settings(&self.paths)?
            },
        )
    }

    fn decode_config(&self, raw_config: &str) -> Result<Value, String> {
        if let Some(inline_config) = extract_inline_config(raw_config) {
            let skirk = self.resolve_sidecar()?;
            let decoded_path = self
                .paths
                .config_dir
                .join(format!("decode-{}.json", epoch_millis()));
            let output = Command::new(skirk)
                .arg("config")
                .arg("decode")
                .arg("--config")
                .arg(inline_config)
                .arg("--out")
                .arg(&decoded_path)
                .output()
                .map_err(|error| format!("failed to decode one-line config: {error}"))?;
            if !output.status.success() {
                let _ = fs::remove_file(&decoded_path);
                let stderr = String::from_utf8_lossy(&output.stderr);
                let stdout = String::from_utf8_lossy(&output.stdout);
                let stderr = stderr.trim();
                let stdout = stdout.trim();
                let detail = if stderr.is_empty() { stdout } else { stderr };
                return Err(format!(
                    "one-line config decode failed: {}{}",
                    output.status,
                    if detail.is_empty() {
                        String::new()
                    } else {
                        format!(": {detail}")
                    }
                ));
            }
            let content = fs::read_to_string(&decoded_path)
                .map_err(|error| format!("failed to read decoded config: {error}"))?;
            let _ = fs::remove_file(&decoded_path);
            return serde_json::from_str(&content)
                .map_err(|error| format!("decoded config is invalid JSON: {error}"));
        }
        serde_json::from_str(raw_config).map_err(|error| format!("invalid JSON: {error}"))
    }

    fn delete_profile(&self, profile_id: &str) -> Result<(), String> {
        let mut profiles = load_profiles(&self.paths)?;
        if let Some(profile) = profiles.iter().find(|profile| profile.id == profile_id) {
            let _ = fs::remove_file(&profile.config_path);
        }
        profiles.retain(|profile| profile.id != profile_id);
        save_profiles(&self.paths, &profiles)?;
        let mut settings = load_settings(&self.paths)?;
        if settings.selected_profile_id.as_deref() == Some(profile_id) {
            settings.selected_profile_id = profiles.first().map(|profile| profile.id.clone());
            save_settings(&self.paths, &settings)?;
        }
        Ok(())
    }

    fn select_profile(&self, profile_id: Option<String>) -> Result<(), String> {
        let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
        refresh_state(&mut state, &self.paths);
        if state.client.is_some() {
            return Err("disconnect before switching profiles".into());
        }
        drop(state);
        save_settings(
            &self.paths,
            &Settings {
                selected_profile_id: profile_id,
                ..load_settings(&self.paths)?
            },
        )
    }

    fn set_connection_mode(&self, mode: ConnectionMode) -> Result<(), String> {
        let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
        refresh_state(&mut state, &self.paths);
        if state.client.is_some() {
            return Err("disconnect before changing connection mode".into());
        }
        drop(state);
        if matches!(mode, ConnectionMode::System) && !cfg!(windows) {
            return Err("system proxy mode is only available on Windows".into());
        }
        if matches!(mode, ConnectionMode::Vpn) && !self.platform_capabilities().vpn_mode_supported {
            return Err(vpn_unavailable_message());
        }
        let mut settings = load_settings(&self.paths)?;
        settings.connection_mode = mode;
        save_settings(&self.paths, &settings)
    }

    fn connect(&self) -> Result<(), String> {
        let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
        refresh_state(&mut state, &self.paths);
        if state.client.is_some() {
            return Ok(());
        }
        state.phase = ConnectionPhase::Connecting;
        state.message = "Starting Skirk sidecar".into();
        drop(state);

        let profiles = load_profiles(&self.paths)?;
        let settings = load_settings(&self.paths)?;
        let profile = settings
            .selected_profile_id
            .as_ref()
            .and_then(|id| profiles.iter().find(|profile| &profile.id == id))
            .or_else(|| profiles.first())
            .ok_or_else(|| "no profile selected".to_string())?
            .clone();
        let mode = settings.connection_mode;
        if matches!(mode, ConnectionMode::System) && !cfg!(windows) {
            return Err("system proxy mode is only available on Windows".into());
        }
        if matches!(mode, ConnectionMode::Vpn) && !self.platform_capabilities().vpn_mode_supported {
            return Err(vpn_unavailable_message());
        }
        let socks_address = format!("{}:{}", profile.socks_host, profile.socks_port);
        let http_address = format!("{}:{}", profile.http_host, profile.http_port);
        let route_mode = "google_front_pinned";
        ensure_port_free(&socks_address)?;
        ensure_port_free(&http_address)?;
        let skirk = self.resolve_sidecar()?;
        let sidecar_process_path = process_path_for_rules(&skirk);
        let log_path = client_log_path(&self.paths);
        let log = OpenOptions::new()
            .create(true)
            .append(true)
            .open(&log_path)
            .map_err(|error| format!("failed to open log: {error}"))?;
        let log_err = log
            .try_clone()
            .map_err(|error| format!("failed to clone log: {error}"))?;
        let mut command = Command::new(skirk);
        command
            .arg("serve-client")
            .arg("--config")
            .arg(&profile.config_path)
            .arg("--listen")
            .arg(&socks_address)
            .arg("--http-proxy-listen")
            .arg(&http_address)
            .arg("--client-id")
            .arg(&profile.id)
            .arg("--route-mode")
            .arg(route_mode)
            .arg("--poll-ms")
            .arg("100")
            .stdout(Stdio::from(log))
            .stderr(Stdio::from(log_err));
        #[cfg(windows)]
        command.creation_flags(CREATE_NO_WINDOW);
        let child = command
            .spawn()
            .map_err(|error| format!("failed to start skirk: {error}"))?;
        let mut child = child;

        let socks_probe_address = loopback_probe_address(&socks_address);
        let http_probe_address = loopback_probe_address(&http_address);
        if !wait_for_tcp_endpoint(&socks_probe_address, Duration::from_secs(10)) {
            let _ = child.kill();
            let _ = child.wait();
            return Err(format!(
                "Skirk did not open SOCKS endpoint {socks_address}\n{}",
                read_log_tail(&log_path, 80)
            ));
        }
        if !wait_for_tcp_endpoint(&http_probe_address, Duration::from_secs(10)) {
            let _ = child.kill();
            let _ = child.wait();
            return Err(format!(
                "Skirk did not open HTTP proxy endpoint {http_address}\n{}",
                read_log_tail(&log_path, 80)
            ));
        }

        let mut tunnel = None;
        if matches!(mode, ConnectionMode::Vpn) {
            match self.spawn_tunnel(profile.socks_port, &sidecar_process_path) {
                Ok(next_tunnel) => {
                    tunnel = Some(next_tunnel);
                }
                Err(error) => {
                    let _ = child.kill();
                    let _ = child.wait();
                    return Err(error);
                }
            }
        }
        let system_proxy_enabled = if matches!(mode, ConnectionMode::System) {
            if let Err(error) = SystemProxyManager::enable(&self.paths, profile.http_port) {
                if let Some(mut tunnel) = tunnel {
                    terminate_child(&mut tunnel.child);
                }
                let _ = child.kill();
                let _ = child.wait();
                return Err(error);
            }
            true
        } else {
            false
        };

        let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
        state.client = Some(ManagedClient {
            child,
            profile_id: profile.id,
            mode: mode.clone(),
            socks_address,
            http_address,
            system_proxy_enabled,
        });
        state.tunnel = tunnel;
        state.phase = ConnectionPhase::Connected;
        state.message = connected_message(&mode);
        Ok(())
    }

    fn disconnect(&self) -> Result<(), String> {
        let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
        state.phase = ConnectionPhase::Disconnecting;
        stop_runtime(&mut state, &self.paths);
        state.phase = ConnectionPhase::Disconnected;
        state.message = "Disconnected".into();
        Ok(())
    }

    fn cleanup_for_exit(&self) {
        if let Ok(mut state) = self.inner.lock() {
            stop_runtime(&mut state, &self.paths);
            state.phase = ConnectionPhase::Disconnected;
            state.message = "Disconnected".into();
        }
    }

    fn platform_capabilities(&self) -> PlatformCapabilities {
        let vpn_sidecar_present = self.resolve_tunnel_sidecar().is_ok();
        let vpn_requires_admin = cfg!(windows);
        let vpn_admin = windows_is_admin();
        PlatformCapabilities {
            system_proxy_supported: cfg!(windows),
            vpn_mode_supported: cfg!(windows) && vpn_sidecar_present,
            vpn_requires_admin,
            vpn_admin,
            vpn_sidecar_present,
        }
    }

    fn spawn_tunnel(
        &self,
        socks_port: u16,
        sidecar_process_path: &str,
    ) -> Result<ManagedTunnel, String> {
        if !cfg!(windows) {
            return Err("VPN mode is only available on Windows".into());
        }
        if !windows_is_admin() {
            return Err(
                "VPN mode needs Administrator privileges to create the Windows TUN adapter".into(),
            );
        }
        let tunnel = self.resolve_tunnel_sidecar()?;
        let log_path = tunnel_log_path(&self.paths);
        let config_path = tunnel_config_path(&self.paths);
        let config = tunnel_config(socks_port, sidecar_process_path);
        fs::write(
            &config_path,
            serde_json::to_vec_pretty(&config)
                .map_err(|error| format!("failed to serialize VPN config: {error}"))?,
        )
        .map_err(|error| format!("failed to write VPN config: {error}"))?;

        let log = OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .open(&log_path)
            .map_err(|error| format!("failed to open VPN log: {error}"))?;
        let log_err = log
            .try_clone()
            .map_err(|error| format!("failed to clone VPN log: {error}"))?;
        let mut command = Command::new(tunnel);
        command
            .arg("run")
            .arg("-c")
            .arg(&config_path)
            .stdout(Stdio::from(log))
            .stderr(Stdio::from(log_err));
        #[cfg(windows)]
        command.creation_flags(CREATE_NO_WINDOW);
        let mut child = command
            .spawn()
            .map_err(|error| format!("failed to start VPN sidecar: {error}"))?;
        if !wait_for_log_marker(
            &mut child,
            &log_path,
            &["sing-box started", "started at"],
            Duration::from_secs(15),
        ) {
            terminate_child(&mut child);
            return Err(format!(
                "VPN sidecar did not become ready\n{}",
                read_log_tail(&log_path, 80)
            ));
        }
        Ok(ManagedTunnel {
            child,
            interface_name: tunnel_interface_name(),
        })
    }

    fn resolve_sidecar(&self) -> Result<PathBuf, String> {
        let exe_dir = std::env::current_exe()
            .ok()
            .and_then(|path| path.parent().map(Path::to_path_buf));
        let current_dir = std::env::current_dir().ok();
        let candidates = sidecar_candidate_paths(
            exe_dir.as_deref(),
            self.resource_dir.as_deref(),
            current_dir.as_deref(),
        );
        candidates
            .iter()
            .find(|path| path.is_file())
            .cloned()
            .ok_or_else(|| sidecar_not_found_message(&candidates))
    }

    fn resolve_tunnel_sidecar(&self) -> Result<PathBuf, String> {
        let exe_dir = std::env::current_exe()
            .ok()
            .and_then(|path| path.parent().map(Path::to_path_buf));
        let current_dir = std::env::current_dir().ok();
        let candidates = tunnel_sidecar_candidate_paths(
            exe_dir.as_deref(),
            self.resource_dir.as_deref(),
            current_dir.as_deref(),
        );
        candidates
            .iter()
            .find(|path| path.is_file())
            .cloned()
            .ok_or_else(|| tunnel_sidecar_not_found_message(&candidates))
    }
}

fn sidecar_candidate_paths(
    exe_dir: Option<&Path>,
    resource_dir: Option<&Path>,
    current_dir: Option<&Path>,
) -> Vec<PathBuf> {
    let names: &[&str] = if cfg!(windows) {
        &["skirk-sidecar.exe", "skirk.exe", "skirk-windows-amd64.exe"]
    } else {
        &["skirk", "skirk-linux-amd64"]
    };
    let os_dir = if cfg!(windows) { "windows" } else { "linux" };
    let mut candidates = Vec::new();
    let mut seen = BTreeSet::new();

    for var in ["SKIRK_DESKTOP_SIDECAR", "SKIRK_SIDECAR"] {
        if let Ok(path) = std::env::var(var) {
            if !path.trim().is_empty() {
                push_sidecar_candidate(&mut candidates, &mut seen, PathBuf::from(path));
            }
        }
    }

    if let Some(dir) = exe_dir {
        for name in names {
            push_sidecar_candidate(
                &mut candidates,
                &mut seen,
                dir.join("sidecars").join(os_dir).join(name),
            );
            push_sidecar_candidate(
                &mut candidates,
                &mut seen,
                dir.join("resources")
                    .join("sidecars")
                    .join(os_dir)
                    .join(name),
            );
            if !cfg!(windows) || *name == "skirk-sidecar.exe" {
                push_sidecar_candidate(&mut candidates, &mut seen, dir.join(name));
            }
        }
    }

    if let Some(dir) = resource_dir {
        for name in names {
            push_sidecar_candidate(
                &mut candidates,
                &mut seen,
                dir.join("sidecars").join(os_dir).join(name),
            );
            push_sidecar_candidate(
                &mut candidates,
                &mut seen,
                dir.join("resources")
                    .join("sidecars")
                    .join(os_dir)
                    .join(name),
            );
        }
    }

    if let Some(current) = current_dir {
        for name in names {
            push_sidecar_candidate(
                &mut candidates,
                &mut seen,
                current.join("../../bin").join(name),
            );
            push_sidecar_candidate(
                &mut candidates,
                &mut seen,
                current.join("../bin").join(name),
            );
            push_sidecar_candidate(&mut candidates, &mut seen, current.join("bin").join(name));
        }
    }

    candidates
}

fn tunnel_sidecar_candidate_paths(
    exe_dir: Option<&Path>,
    resource_dir: Option<&Path>,
    current_dir: Option<&Path>,
) -> Vec<PathBuf> {
    let names: &[&str] = if cfg!(windows) {
        &["skirk-tunnel.exe", "sing-box.exe"]
    } else {
        &["skirk-tunnel", "sing-box"]
    };
    let os_dir = if cfg!(windows) { "windows" } else { "linux" };
    let mut candidates = Vec::new();
    let mut seen = BTreeSet::new();

    for var in ["SKIRK_TUNNEL_SIDECAR", "SKIRK_DESKTOP_TUNNEL"] {
        if let Ok(path) = std::env::var(var) {
            if !path.trim().is_empty() {
                push_sidecar_candidate(&mut candidates, &mut seen, PathBuf::from(path));
            }
        }
    }

    if let Some(dir) = exe_dir {
        for name in names {
            push_sidecar_candidate(
                &mut candidates,
                &mut seen,
                dir.join("sidecars").join(os_dir).join(name),
            );
            push_sidecar_candidate(
                &mut candidates,
                &mut seen,
                dir.join("resources")
                    .join("sidecars")
                    .join(os_dir)
                    .join(name),
            );
            push_sidecar_candidate(&mut candidates, &mut seen, dir.join(name));
        }
    }

    if let Some(dir) = resource_dir {
        for name in names {
            push_sidecar_candidate(
                &mut candidates,
                &mut seen,
                dir.join("sidecars").join(os_dir).join(name),
            );
            push_sidecar_candidate(
                &mut candidates,
                &mut seen,
                dir.join("resources")
                    .join("sidecars")
                    .join(os_dir)
                    .join(name),
            );
        }
    }

    if let Some(current) = current_dir {
        for name in names {
            push_sidecar_candidate(
                &mut candidates,
                &mut seen,
                current.join("../../bin").join(name),
            );
            push_sidecar_candidate(
                &mut candidates,
                &mut seen,
                current.join("../bin").join(name),
            );
            push_sidecar_candidate(&mut candidates, &mut seen, current.join("bin").join(name));
        }
    }

    candidates
}

fn push_sidecar_candidate(
    candidates: &mut Vec<PathBuf>,
    seen: &mut BTreeSet<PathBuf>,
    path: PathBuf,
) {
    if seen.insert(path.clone()) {
        candidates.push(path);
    }
}

fn sidecar_not_found_message(candidates: &[PathBuf]) -> String {
    let searched = candidates
        .iter()
        .take(12)
        .map(|path| path.display().to_string())
        .collect::<Vec<_>>()
        .join("; ");
    "skirk sidecar not found; place skirk-sidecar.exe under sidecars/windows/ or resources/sidecars/windows/, or set SKIRK_DESKTOP_SIDECAR. searched: "
        .to_string()
        + &searched
}

fn tunnel_sidecar_not_found_message(candidates: &[PathBuf]) -> String {
    let searched = candidates
        .iter()
        .take(12)
        .map(|path| path.display().to_string())
        .collect::<Vec<_>>()
        .join("; ");
    "VPN mode needs the bundled TUN sidecar skirk-tunnel.exe. searched: ".to_string() + &searched
}

fn looks_like_inline_config(raw_config: &str) -> bool {
    extract_inline_config(raw_config).is_some()
}

fn extract_inline_config(raw_config: &str) -> Option<String> {
    let mut text = raw_config.trim();
    if let Some(rest) = text.strip_prefix("SKIRK_CONFIG=") {
        text = rest.trim();
    }
    text = text.trim_matches(|ch| ch == '"' || ch == '\'' || ch == '`');

    let start = text.find("skirk:")?;
    let payload = &text[start + "skirk:".len()..];
    let mut encoded = String::new();
    let mut seen_payload = false;
    let mut chars = payload.char_indices().peekable();
    while let Some((_, ch)) = chars.next() {
        if is_raw_url_base64_char(ch) {
            encoded.push(ch);
            seen_payload = true;
            continue;
        }
        if ch.is_whitespace() {
            if !seen_payload {
                continue;
            }
            while matches!(chars.peek(), Some((_, next)) if next.is_whitespace()) {
                let _ = chars.next();
            }
            let remaining = chars
                .peek()
                .and_then(|(idx, _)| payload.get(*idx..))
                .unwrap_or_default();
            if remaining.is_empty() || remaining.starts_with("--") {
                break;
            }
            let Some((_, next)) = chars.peek().copied() else {
                break;
            };
            if matches!(next, '\'' | '"' | '`') {
                break;
            }
            if is_raw_url_base64_char(next) {
                continue;
            }
            break;
        }
        if matches!(ch, '\'' | '"' | '`') {
            if seen_payload {
                break;
            }
            continue;
        }
        if seen_payload {
            break;
        }
        return None;
    }

    if encoded.is_empty() {
        None
    } else {
        Some(format!("skirk:{encoded}"))
    }
}

fn is_raw_url_base64_char(ch: char) -> bool {
    ch.is_ascii_alphanumeric() || ch == '-' || ch == '_'
}

fn refresh_state(state: &mut RuntimeState, paths: &AppPaths) {
    let Some(client) = state.client.as_mut() else {
        if !matches!(state.phase, ConnectionPhase::Error) {
            state.phase = ConnectionPhase::Disconnected;
        }
        return;
    };
    let tunnel_died = if let Some(tunnel) = state.tunnel.as_mut() {
        match tunnel.child.try_wait() {
            Ok(Some(status)) => Some(format!("VPN sidecar exited: {status}")),
            Ok(None) => None,
            Err(error) => Some(format!("VPN status failed: {error}")),
        }
    } else {
        None
    };
    if let Some(message) = tunnel_died {
        stop_runtime(state, paths);
        state.phase = ConnectionPhase::Error;
        state.message = message;
        return;
    }
    match client.child.try_wait() {
        Ok(Some(status)) => {
            stop_runtime(state, paths);
            state.phase = ConnectionPhase::Error;
            state.message = format!("Skirk exited: {status}");
        }
        Ok(None) => {
            state.phase = ConnectionPhase::Connected;
        }
        Err(error) => {
            stop_runtime(state, paths);
            state.phase = ConnectionPhase::Error;
            state.message = format!("Skirk status failed: {error}");
        }
    }
}

fn stop_runtime(state: &mut RuntimeState, paths: &AppPaths) {
    if let Some(mut tunnel) = state.tunnel.take() {
        terminate_child(&mut tunnel.child);
    }
    if let Some(mut client) = state.client.take() {
        if client.system_proxy_enabled {
            let _ = SystemProxyManager::disable(paths);
        }
        terminate_child(&mut client.child);
    } else {
        let _ = SystemProxyManager::cleanup_stale_proxy(paths);
    }
}

fn terminate_child(child: &mut Child) {
    let _ = child.kill();
    let _ = child.wait();
}

fn wait_for_tcp_endpoint(address: &str, timeout: Duration) -> bool {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if TcpStream::connect(address).is_ok() {
            return true;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    false
}

fn wait_for_log_marker(
    child: &mut Child,
    path: &Path,
    markers: &[&str],
    timeout: Duration,
) -> bool {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if matches!(child.try_wait(), Ok(Some(_))) {
            return false;
        }
        let tail = read_log_tail(path, 120).to_ascii_lowercase();
        if markers
            .iter()
            .any(|marker| tail.contains(&marker.to_ascii_lowercase()))
        {
            return true;
        }
        std::thread::sleep(Duration::from_millis(200));
    }
    false
}

fn connected_message(mode: &ConnectionMode) -> String {
    match mode {
        ConnectionMode::Proxy => "Connected in local proxy mode".into(),
        ConnectionMode::System => "Connected and Windows system proxy is enabled".into(),
        ConnectionMode::Vpn => "Connected in VPN mode".into(),
    }
}

fn vpn_unavailable_message() -> String {
    if !cfg!(windows) {
        return "VPN mode is only available on Windows".into();
    }
    if !windows_is_admin() {
        return "VPN mode needs Administrator privileges".into();
    }
    "VPN mode needs the bundled TUN sidecar skirk-tunnel.exe".into()
}

fn tunnel_log_path(paths: &AppPaths) -> PathBuf {
    paths.logs_dir.join("skirk-tunnel.log")
}

fn tunnel_config_path(paths: &AppPaths) -> PathBuf {
    paths.runtime_dir.join("skirk-tunnel.json")
}

fn tunnel_interface_name() -> String {
    "Skirk Tunnel".into()
}

fn loopback_probe_address(address: &str) -> String {
    address
        .strip_prefix("0.0.0.0:")
        .map(|port| format!("127.0.0.1:{port}"))
        .unwrap_or_else(|| address.to_string())
}

fn process_path_for_rules(path: &Path) -> String {
    path.canonicalize()
        .unwrap_or_else(|_| path.to_path_buf())
        .display()
        .to_string()
}

fn tunnel_config(socks_port: u16, sidecar_process_path: &str) -> Value {
    json!({
        "log": {
            "level": "info",
            "timestamp": true
        },
        "dns": {
            "servers": [
                {
                    "type": "local",
                    "tag": "local"
                }
            ],
            "final": "local",
            "strategy": "prefer_ipv4"
        },
        "inbounds": [
            {
                "type": "tun",
                "tag": "tun-in",
                "interface_name": tunnel_interface_name(),
                "address": [
                    "172.19.0.1/30",
                    "fdfe:dcba:9876::1/126"
                ],
                "auto_route": true,
                "strict_route": true,
                "stack": "mixed",
                "route_exclude_address": [
                    "127.0.0.0/8",
                    "10.0.0.0/8",
                    "100.64.0.0/10",
                    "169.254.0.0/16",
                    "172.16.0.0/12",
                    "192.168.0.0/16",
                    "224.0.0.0/4",
                    "::1/128",
                    "fc00::/7",
                    "fe80::/10"
                ]
            }
        ],
        "outbounds": [
            {
                "type": "socks",
                "tag": "proxy",
                "server": "127.0.0.1",
                "server_port": socks_port,
                "version": "5"
            },
            {
                "type": "direct",
                "tag": "direct"
            }
        ],
        "route": {
            "rules": [
                {
                    "inbound": "tun-in",
                    "action": "sniff",
                    "timeout": "1s"
                },
                {
                    "type": "logical",
                    "mode": "or",
                    "rules": [
                        {
                            "process_name": [
                                "skirk-sidecar.exe",
                                "skirk.exe",
                                "skirk-windows-amd64.exe"
                            ]
                        },
                        {
                            "process_path": [
                                sidecar_process_path
                            ]
                        }
                    ],
                    "action": "route",
                    "outbound": "direct"
                },
                {
                    "type": "logical",
                    "mode": "or",
                    "rules": [
                        {
                            "protocol": "dns"
                        },
                        {
                            "port": 53
                        }
                    ],
                    "action": "hijack-dns"
                },
                {
                    "network": "udp",
                    "action": "reject"
                },
                {
                    "ip_is_private": true,
                    "action": "route",
                    "outbound": "direct"
                }
            ],
            "final": "proxy",
            "auto_detect_interface": true,
            "find_process": true,
            "default_domain_resolver": "local"
        }
    })
}

#[cfg(windows)]
fn windows_is_admin() -> bool {
    unsafe {
        let mut token = std::ptr::null_mut();
        if OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut token) == 0 {
            return false;
        }
        let mut elevation = TOKEN_ELEVATION { TokenIsElevated: 0 };
        let mut returned = 0u32;
        let ok = GetTokenInformation(
            token,
            TokenElevation,
            &mut elevation as *mut TOKEN_ELEVATION as *mut _,
            std::mem::size_of::<TOKEN_ELEVATION>() as u32,
            &mut returned,
        );
        let _ = CloseHandle(token);
        ok != 0 && elevation.TokenIsElevated != 0
    }
}

#[cfg(not(windows))]
fn windows_is_admin() -> bool {
    false
}

#[tauri::command]
async fn load_snapshot(runtime: State<'_, DesktopRuntime>) -> Result<DesktopSnapshot, String> {
    runtime.snapshot()
}

#[tauri::command]
async fn import_config(
    runtime: State<'_, DesktopRuntime>,
    name: String,
    raw_config: String,
    socks_port: u16,
    http_port: u16,
    share_lan: bool,
) -> Result<DesktopSnapshot, String> {
    runtime.import_config(name, raw_config, socks_port, http_port, share_lan)?;
    runtime.snapshot()
}

#[tauri::command]
async fn delete_profile(
    runtime: State<'_, DesktopRuntime>,
    profile_id: String,
) -> Result<DesktopSnapshot, String> {
    runtime.delete_profile(&profile_id)?;
    runtime.snapshot()
}

#[tauri::command]
async fn select_profile(
    runtime: State<'_, DesktopRuntime>,
    profile_id: Option<String>,
) -> Result<DesktopSnapshot, String> {
    runtime.select_profile(profile_id)?;
    runtime.snapshot()
}

#[tauri::command]
async fn set_connection_mode(
    runtime: State<'_, DesktopRuntime>,
    mode: ConnectionMode,
) -> Result<DesktopSnapshot, String> {
    runtime.set_connection_mode(mode)?;
    runtime.snapshot()
}

#[tauri::command]
async fn connect(runtime: State<'_, DesktopRuntime>) -> Result<DesktopSnapshot, String> {
    runtime.connect()?;
    runtime.snapshot()
}

#[tauri::command]
async fn disconnect(runtime: State<'_, DesktopRuntime>) -> Result<DesktopSnapshot, String> {
    runtime.disconnect()?;
    runtime.snapshot()
}

pub fn run() {
    tauri::Builder::default()
        .setup(|app| {
            let paths = AppPaths::resolve(&app.handle())?;
            let _ = SystemProxyManager::cleanup_stale_proxy(&paths);
            let resource_dir = app.path().resource_dir().ok();
            app.manage(DesktopRuntime::new(paths, resource_dir));
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            load_snapshot,
            import_config,
            delete_profile,
            select_profile,
            set_connection_mode,
            connect,
            disconnect
        ])
        .build(tauri::generate_context!())
        .expect("error while building Skirk desktop")
        .run(|app, event| match event {
            tauri::RunEvent::ExitRequested { .. } | tauri::RunEvent::Exit => {
                app.state::<DesktopRuntime>().cleanup_for_exit();
            }
            _ => {}
        });
}

impl AppPaths {
    fn resolve<R: tauri::Runtime>(app: &tauri::AppHandle<R>) -> Result<Self, String> {
        let (config_dir, runtime_dir, logs_dir) = if let Some(root) = portable_root() {
            (root.join("config"), root.join("runtime"), root.join("logs"))
        } else {
            let config_dir = app
                .path()
                .app_config_dir()
                .map_err(|error| format!("failed to resolve config dir: {error}"))?;
            let data_dir = app
                .path()
                .app_local_data_dir()
                .map_err(|error| format!("failed to resolve data dir: {error}"))?;
            (config_dir, data_dir.join("runtime"), data_dir.join("logs"))
        };
        for dir in [&config_dir, &runtime_dir, &logs_dir] {
            fs::create_dir_all(dir)
                .map_err(|error| format!("failed to create {}: {error}", dir.display()))?;
        }
        Ok(Self {
            profiles_file: config_dir.join("profiles.json"),
            settings_file: config_dir.join("settings.json"),
            config_dir,
            runtime_dir,
            logs_dir,
        })
    }
}

fn portable_root() -> Option<PathBuf> {
    if std::env::var("SKIRK_PORTABLE")
        .ok()
        .map(|value| matches!(value.to_ascii_lowercase().as_str(), "1" | "true" | "yes"))
        .unwrap_or(false)
    {
        let exe = std::env::current_exe().ok()?;
        return Some(exe.parent()?.join("portable-data"));
    }
    let exe = std::env::current_exe().ok()?;
    let dir = exe.parent()?;
    if dir.join("portable-data").exists() || dir.join("skirk-portable").exists() {
        return Some(dir.join("portable-data"));
    }
    None
}

fn load_profiles(paths: &AppPaths) -> Result<Vec<ClientProfile>, String> {
    read_json(&paths.profiles_file).or_else(|error| {
        if paths.profiles_file.exists() {
            Err(error)
        } else {
            Ok(Vec::new())
        }
    })
}

fn save_profiles(paths: &AppPaths, profiles: &[ClientProfile]) -> Result<(), String> {
    write_json(&paths.profiles_file, profiles)
}

fn load_settings(paths: &AppPaths) -> Result<Settings, String> {
    read_json(&paths.settings_file).or_else(|error| {
        if paths.settings_file.exists() {
            Err(error)
        } else {
            Ok(Settings::default())
        }
    })
}

fn save_settings(paths: &AppPaths, settings: &Settings) -> Result<(), String> {
    write_json(&paths.settings_file, settings)
}

fn read_json<T: serde::de::DeserializeOwned>(path: &Path) -> Result<T, String> {
    let content = fs::read_to_string(path)
        .map_err(|error| format!("failed to read {}: {error}", path.display()))?;
    serde_json::from_str(&content)
        .map_err(|error| format!("failed to parse {}: {error}", path.display()))
}

fn write_json<T: serde::Serialize + ?Sized>(path: &Path, value: &T) -> Result<(), String> {
    let content = serde_json::to_string_pretty(value)
        .map_err(|error| format!("failed to serialize JSON: {error}"))?;
    fs::write(path, content).map_err(|error| format!("failed to write {}: {error}", path.display()))
}

fn client_log_path(paths: &AppPaths) -> PathBuf {
    paths.logs_dir.join("skirk-client.log")
}

fn read_log_tail(path: &Path, limit: usize) -> String {
    let Ok(content) = fs::read_to_string(path) else {
        return String::new();
    };
    let mut lines = content.lines().collect::<Vec<_>>();
    if lines.len() > limit {
        lines = lines.split_off(lines.len() - limit);
    }
    lines.join("\n")
}

fn ensure_port_free(address: &str) -> Result<(), String> {
    TcpListener::bind(address)
        .map(|listener| drop(listener))
        .map_err(|error| format!("{address} is not available: {error}"))
}

fn share_addresses(address: &str) -> Vec<String> {
    let Some((host, port)) = address.rsplit_once(':') else {
        return Vec::new();
    };
    if host != "0.0.0.0" && host != "::" {
        return Vec::new();
    }
    discover_lan_ips()
        .into_iter()
        .map(|ip| format!("{ip}:{port}"))
        .collect()
}

fn discover_lan_ips() -> Vec<String> {
    let mut ips = Vec::new();
    for target in ["8.8.8.8:80", "1.1.1.1:80"] {
        if let Ok(socket) = std::net::UdpSocket::bind("0.0.0.0:0") {
            if socket.connect(target).is_ok() {
                if let Ok(addr) = socket.local_addr() {
                    let ip = addr.ip().to_string();
                    if ip != "0.0.0.0" && !ips.contains(&ip) {
                        ips.push(ip);
                    }
                }
            }
        }
    }
    ips
}

fn epoch_millis() -> u128 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|duration| duration.as_millis())
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn detects_inline_config_inside_pasted_text() {
        assert!(looks_like_inline_config("skirk:abc"));
        assert!(looks_like_inline_config("SKIRK_CONFIG=skirk:abc"));
        assert!(looks_like_inline_config(
            "skirk serve-client --config 'skirk:abc' --listen 127.0.0.1:18080"
        ));
        assert!(!looks_like_inline_config(r#"{"secret":"abc"}"#));
    }

    #[test]
    fn extracts_inline_config_from_full_client_command() {
        assert_eq!(
            extract_inline_config(
                "skirk serve-client --config 'skirk:abc_DEF-123' --listen 127.0.0.1:18080"
            )
            .as_deref(),
            Some("skirk:abc_DEF-123")
        );
    }

    #[test]
    fn extracts_inline_config_from_env_assignment() {
        assert_eq!(
            extract_inline_config("SKIRK_CONFIG=\"skirk:abc_DEF-123\"").as_deref(),
            Some("skirk:abc_DEF-123")
        );
    }

    #[test]
    fn sidecar_candidates_cover_portable_and_tauri_resource_layouts() {
        let os_dir = if cfg!(windows) { "windows" } else { "linux" };
        let sidecar_name = if cfg!(windows) {
            "skirk-sidecar.exe"
        } else {
            "skirk"
        };
        let exe_dir = PathBuf::from("/opt/skirk");
        let resource_dir = exe_dir.join("resources");
        let candidates =
            sidecar_candidate_paths(Some(&exe_dir), Some(&resource_dir), Some(Path::new("/tmp")));

        assert!(candidates.contains(&exe_dir.join("sidecars").join(os_dir).join(sidecar_name)));
        assert!(candidates.contains(
            &exe_dir
                .join("resources")
                .join("sidecars")
                .join(os_dir)
                .join(sidecar_name)
        ));
        assert!(candidates.contains(
            &resource_dir
                .join("sidecars")
                .join(os_dir)
                .join(sidecar_name)
        ));
    }

    #[test]
    fn tunnel_config_uses_sing_box_1_13_tun_fields() {
        let config = tunnel_config(18080, r"C:\Skirk\skirk-sidecar.exe");
        let inbound = config
            .pointer("/inbounds/0")
            .and_then(Value::as_object)
            .expect("tun inbound object");

        assert_eq!(
            inbound
                .get("address")
                .and_then(Value::as_array)
                .map(Vec::len),
            Some(2)
        );
        assert!(inbound.get("sniff").is_none());
        assert!(inbound.get("sniff_override_destination").is_none());
        assert!(inbound.get("inet4_address").is_none());
        assert!(inbound.get("inet6_address").is_none());

        assert_eq!(
            config
                .pointer("/route/rules/0/action")
                .and_then(Value::as_str),
            Some("sniff")
        );
        assert_eq!(
            config
                .pointer("/route/rules/0/inbound")
                .and_then(Value::as_str),
            Some("tun-in")
        );
    }
}
