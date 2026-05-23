mod system_proxy;

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::{
    collections::BTreeSet,
    fs::{self, OpenOptions},
    io::{Read, Write},
    net::{IpAddr, TcpListener, TcpStream},
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

const CLIENT_READY_TIMEOUT: Duration = Duration::from_secs(30);
const CUSTOM_MIN_POLL_MS: u16 = 250;
const CUSTOM_MAX_UPLOAD_WORKERS: u16 = 64;
const CUSTOM_MAX_DOWNLOAD_WORKERS: u16 = 64;

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
    #[serde(default = "default_google_ip")]
    google_ip: String,
    drive_space: String,
    drive_folder_id: String,
    #[serde(default = "default_performance_settings")]
    performance: ClientPerformanceSettings,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
enum PerformancePreset {
    LowerUsage,
    Recommended,
    Responsive,
    BulkTransfer,
    Custom,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ClientPerformanceSettings {
    preset: PerformancePreset,
    poll_ms: u16,
    upload_concurrency: u16,
    download_concurrency: u16,
    burst_poll: bool,
    burst_poll_ms: u16,
    burst_poll_window_ms: u16,
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
    lan_http_addresses: Vec<String>,
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
    metrics: Option<RuntimeMetrics>,
    platform: String,
    capabilities: PlatformCapabilities,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all(serialize = "camelCase", deserialize = "snake_case"))]
struct RuntimeMetrics {
    version: i32,
    role: String,
    started_at: String,
    updated_at: String,
    duration_seconds: f64,
    route_mode: String,
    transport: String,
    poll_ms: i32,
    upload_concurrency: i32,
    download_concurrency: i32,
    burst_poll: bool,
    estimated_project_units_per_min: i64,
    estimated_user_units_per_min: i64,
    recent_quota: QuotaSnapshot,
    recent_quota_per_minute: QuotaRate,
    recent_quota_ops: String,
    drive_backoff: Option<DriveBackoff>,
    note: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all(serialize = "camelCase", deserialize = "snake_case"))]
struct QuotaSnapshot {
    calls: i64,
    units: i64,
    errors: i64,
    last_error_reason: Option<String>,
    response_bytes: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all(serialize = "camelCase", deserialize = "snake_case"))]
struct QuotaRate {
    calls: f64,
    units: f64,
    errors: f64,
    last_error_reason: Option<String>,
    response_bytes: f64,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all(serialize = "camelCase", deserialize = "snake_case"))]
struct DriveBackoff {
    active: bool,
    until: Option<String>,
    wait_seconds: Option<f64>,
    reason: Option<String>,
    op: Option<String>,
    failures: Option<i32>,
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
    metrics_path: PathBuf,
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

fn default_google_ip() -> String {
    "216.239.38.120".into()
}

fn default_performance_settings() -> ClientPerformanceSettings {
    performance_for_preset(PerformancePreset::Recommended)
}

fn performance_for_preset(preset: PerformancePreset) -> ClientPerformanceSettings {
    match preset {
        PerformancePreset::LowerUsage => ClientPerformanceSettings {
            preset,
            poll_ms: 2000,
            upload_concurrency: 4,
            download_concurrency: 8,
            burst_poll: false,
            burst_poll_ms: 75,
            burst_poll_window_ms: 5000,
        },
        PerformancePreset::Recommended => ClientPerformanceSettings {
            preset,
            poll_ms: 1000,
            upload_concurrency: 8,
            download_concurrency: 16,
            burst_poll: false,
            burst_poll_ms: 75,
            burst_poll_window_ms: 5000,
        },
        PerformancePreset::Responsive => ClientPerformanceSettings {
            preset,
            poll_ms: 1000,
            upload_concurrency: 8,
            download_concurrency: 16,
            burst_poll: true,
            burst_poll_ms: 75,
            burst_poll_window_ms: 5000,
        },
        PerformancePreset::BulkTransfer => ClientPerformanceSettings {
            preset,
            poll_ms: 1000,
            upload_concurrency: 16,
            download_concurrency: 32,
            burst_poll: false,
            burst_poll_ms: 75,
            burst_poll_window_ms: 5000,
        },
        PerformancePreset::Custom => ClientPerformanceSettings {
            preset,
            poll_ms: 1000,
            upload_concurrency: 8,
            download_concurrency: 16,
            burst_poll: false,
            burst_poll_ms: 75,
            burst_poll_window_ms: 5000,
        },
    }
}

fn normalize_performance_settings(
    settings: ClientPerformanceSettings,
) -> Result<ClientPerformanceSettings, String> {
    if settings.preset != PerformancePreset::Custom {
        return Ok(performance_for_preset(settings.preset));
    }
    if settings.poll_ms < CUSTOM_MIN_POLL_MS || settings.poll_ms > 60_000 {
        return Err("check interval must be between 250 ms and 60000 ms".into());
    }
    if settings.upload_concurrency < 1 || settings.upload_concurrency > CUSTOM_MAX_UPLOAD_WORKERS {
        return Err("upload workers must be between 1 and 64".into());
    }
    if settings.download_concurrency < 1
        || settings.download_concurrency > CUSTOM_MAX_DOWNLOAD_WORKERS
    {
        return Err("download workers must be between 1 and 64".into());
    }
    if settings.burst_poll_ms < 25 || settings.burst_poll_ms > 1000 {
        return Err("burst interval must be between 25 ms and 1000 ms".into());
    }
    if settings.burst_poll_window_ms < 1000 || settings.burst_poll_window_ms > 30000 {
        return Err("burst window must be between 1000 ms and 30000 ms".into());
    }
    Ok(settings)
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
                lan_http_addresses: share_addresses(&client.http_address),
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
                lan_http_addresses: Vec::new(),
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
            metrics: state
                .client
                .as_ref()
                .and_then(|client| read_runtime_metrics(&client.metrics_path)),
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
        let google_ip = parsed
            .pointer("/route/google_ip")
            .and_then(Value::as_str)
            .unwrap_or("");
        let google_ip = canonical_google_ip(google_ip)?;
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
            google_ip,
            drive_space,
            drive_folder_id,
            performance: default_performance_settings(),
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

    fn update_profile_performance(
        &self,
        profile_id: &str,
        performance: ClientPerformanceSettings,
    ) -> Result<(), String> {
        let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
        refresh_state(&mut state, &self.paths);
        if state.client.is_some() {
            return Err("disconnect before changing performance settings".into());
        }
        drop(state);
        let performance = normalize_performance_settings(performance)?;
        let mut profiles = load_profiles(&self.paths)?;
        let profile = profiles
            .iter_mut()
            .find(|profile| profile.id == profile_id)
            .ok_or_else(|| "profile was not found".to_string())?;
        profile.performance = performance;
        save_profiles(&self.paths, &profiles)
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
        let performance = normalize_performance_settings(profile.performance.clone())?;
        if matches!(mode, ConnectionMode::System) && !cfg!(windows) {
            return Err("system proxy mode is only available on Windows".into());
        }
        if matches!(mode, ConnectionMode::Vpn) && !self.platform_capabilities().vpn_mode_supported {
            return Err(vpn_unavailable_message());
        }
        let (socks_address, http_address) = listener_addresses_for_mode(&profile, &mode);
        let route_mode = "google_front_pinned";
        let google_ip = canonical_google_ip(&profile.google_ip)?;
        ensure_port_free(&socks_address)?;
        ensure_port_free(&http_address)?;
        let skirk = self.resolve_sidecar()?;
        let sidecar_process_path = process_path_for_rules(&skirk);
        let log_path = client_log_path(&self.paths);
        let metrics_path = client_metrics_path(&self.paths);
        let _ = fs::remove_file(&metrics_path);
        let log = OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
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
            .arg("--google-ip")
            .arg(&google_ip);
        if performance.burst_poll {
            command
                .arg("--burst-poll")
                .arg("--burst-poll-ms")
                .arg(performance.burst_poll_ms.to_string())
                .arg("--burst-poll-window-ms")
                .arg(performance.burst_poll_window_ms.to_string());
        } else {
            command.arg("--no-burst-poll");
        }
        command
            .arg("--poll-ms")
            .arg(performance.poll_ms.to_string())
            .arg("--upload-concurrency")
            .arg(performance.upload_concurrency.to_string())
            .arg("--download-concurrency")
            .arg(performance.download_concurrency.to_string())
            .arg("--metrics-path")
            .arg(&metrics_path)
            .arg("--watch-parent-pid")
            .arg(std::process::id().to_string())
            .stdout(Stdio::from(log))
            .stderr(Stdio::from(log_err));
        #[cfg(windows)]
        command.creation_flags(CREATE_NO_WINDOW);
        {
            let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
            state.phase = ConnectionPhase::Connecting;
            state.message = "Starting Skirk sidecar".into();
        }
        let child = command
            .spawn()
            .map_err(|error| self.mark_connect_error(format!("failed to start skirk: {error}")))?;
        let mut child = child;

        let socks_probe_address = loopback_probe_address(&socks_address);
        let http_probe_address = loopback_probe_address(&http_address);
        if let Err(error) = wait_for_socks5_endpoint_or_exit(
            &mut child,
            &socks_probe_address,
            CLIENT_READY_TIMEOUT,
            &log_path,
        ) {
            let _ = child.kill();
            let _ = child.wait();
            return Err(self.mark_connect_error(error));
        }
        if let Err(error) = wait_for_tcp_endpoint_or_exit(
            &mut child,
            &http_probe_address,
            "HTTP proxy",
            CLIENT_READY_TIMEOUT,
            &log_path,
        ) {
            let _ = child.kill();
            let _ = child.wait();
            return Err(self.mark_connect_error(error));
        }

        let mut tunnel = None;
        if matches!(mode, ConnectionMode::Vpn) {
            match self.spawn_tunnel(profile.socks_port, &sidecar_process_path, &google_ip) {
                Ok(next_tunnel) => {
                    tunnel = Some(next_tunnel);
                }
                Err(error) => {
                    let _ = child.kill();
                    let _ = child.wait();
                    return Err(self.mark_connect_error(error));
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
                return Err(self.mark_connect_error(error));
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
            metrics_path,
            system_proxy_enabled,
        });
        state.tunnel = tunnel;
        state.phase = ConnectionPhase::Connected;
        state.message = connected_message(&mode);
        Ok(())
    }

    fn mark_connect_error(&self, message: String) -> String {
        if let Ok(mut state) = self.inner.lock() {
            if state.client.is_none() {
                state.phase = ConnectionPhase::Error;
                state.message = message.clone();
            }
        }
        message
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
        let vpn_requires_admin = cfg!(windows) || cfg!(target_os = "linux");
        let vpn_admin = platform_has_tun_privileges();
        let vpn_platform_supported = cfg!(windows) || cfg!(target_os = "linux");
        PlatformCapabilities {
            system_proxy_supported: cfg!(windows),
            vpn_mode_supported: vpn_platform_supported && vpn_sidecar_present,
            vpn_requires_admin,
            vpn_admin,
            vpn_sidecar_present,
        }
    }

    fn spawn_tunnel(
        &self,
        socks_port: u16,
        sidecar_process_path: &str,
        google_ip: &str,
    ) -> Result<ManagedTunnel, String> {
        if !cfg!(windows) && !cfg!(target_os = "linux") {
            return Err("VPN mode is only available on Windows and Linux".into());
        }
        if !platform_has_tun_privileges() {
            return Err(vpn_admin_required_message());
        }
        let tunnel = self.resolve_tunnel_sidecar()?;
        let log_path = tunnel_log_path(&self.paths);
        let config_path = tunnel_config_path(&self.paths);
        let config = tunnel_config(socks_port, sidecar_process_path, google_ip);
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
        if let Err(error) = wait_for_process_start(&mut child, &log_path, Duration::from_secs(2)) {
            terminate_child(&mut child);
            return Err(format!(
                "VPN sidecar failed to start: {error}\n{}",
                read_log_tail(&log_path, 120)
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
    let names = sidecar_names();
    let os_dir = sidecar_os_dir();
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
    let os_dir = sidecar_os_dir();
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

fn sidecar_os_dir() -> &'static str {
    if cfg!(windows) {
        "windows"
    } else if cfg!(target_os = "macos") {
        "macos"
    } else {
        "linux"
    }
}

fn sidecar_names() -> &'static [&'static str] {
    if cfg!(windows) {
        &["skirk-sidecar.exe", "skirk.exe", "skirk-windows-amd64.exe"]
    } else if cfg!(target_os = "macos") {
        &["skirk", "skirk-darwin-arm64", "skirk-darwin-amd64"]
    } else {
        &["skirk", "skirk-linux-amd64"]
    }
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
    let guidance = if cfg!(windows) {
        "skirk sidecar not found; place skirk-sidecar.exe under sidecars/windows/ or resources/sidecars/windows/, or set SKIRK_DESKTOP_SIDECAR. searched: "
    } else if cfg!(target_os = "macos") {
        "skirk sidecar not found; place skirk under sidecars/macos/ or resources/sidecars/macos/, or set SKIRK_DESKTOP_SIDECAR. searched: "
    } else {
        "skirk sidecar not found; place skirk under sidecars/linux/ or resources/sidecars/linux/, or set SKIRK_DESKTOP_SIDECAR. searched: "
    };
    guidance.to_string() + &searched
}

fn tunnel_sidecar_not_found_message(candidates: &[PathBuf]) -> String {
    let searched = candidates
        .iter()
        .take(12)
        .map(|path| path.display().to_string())
        .collect::<Vec<_>>()
        .join("; ");
    let guidance = if cfg!(windows) {
        "VPN mode needs the bundled TUN sidecar skirk-tunnel.exe. searched: "
    } else if cfg!(target_os = "linux") {
        "VPN mode needs the bundled TUN sidecar skirk-tunnel. searched: "
    } else {
        "VPN mode is not available on this platform. A TUN sidecar was searched only for diagnostics: "
    };
    guidance.to_string() + &searched
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
        cleanup_linux_tun_routes();
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

fn cleanup_linux_tun_routes() {
    if !cfg!(target_os = "linux") {
        return;
    }
    for pref in ["9000", "9001", "9002", "9003", "9010", "32768"] {
        for _ in 0..8 {
            if run_quiet("ip", &["rule", "del", "pref", pref]).is_err() {
                break;
            }
        }
    }
    let _ = run_quiet("ip", &["route", "flush", "table", "2022"]);
    let _ = run_quiet("ip", &["link", "del", &tunnel_interface_name()]);
}

fn run_quiet(program: &str, args: &[&str]) -> Result<(), ()> {
    let status = Command::new(program)
        .args(args)
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .map_err(|_| ())?;
    if status.success() {
        Ok(())
    } else {
        Err(())
    }
}

fn wait_for_tcp_endpoint_or_exit(
    child: &mut Child,
    address: &str,
    label: &str,
    timeout: Duration,
    log_path: &Path,
) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if TcpStream::connect(address).is_ok() {
            return Ok(());
        }
        if let Some(status) = child.try_wait().map_err(|error| {
            format!("Skirk sidecar status check failed while waiting for {label}: {error}")
        })? {
            return Err(startup_probe_error(
                label,
                address,
                Some(&format!("sidecar exited with {status}")),
                log_path,
            ));
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    Err(startup_probe_error(label, address, None, log_path))
}

fn wait_for_socks5_endpoint_or_exit(
    child: &mut Child,
    address: &str,
    timeout: Duration,
    log_path: &Path,
) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if socks5_no_auth_probe(address) {
            return Ok(());
        }
        if let Some(status) = child.try_wait().map_err(|error| {
            format!("Skirk sidecar status check failed while waiting for SOCKS: {error}")
        })? {
            return Err(startup_probe_error(
                "SOCKS",
                address,
                Some(&format!("sidecar exited with {status}")),
                log_path,
            ));
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    Err(startup_probe_error("SOCKS", address, None, log_path))
}

fn startup_probe_error(
    label: &str,
    address: &str,
    detail: Option<&str>,
    log_path: &Path,
) -> String {
    let mut message = format!(
        "Skirk could not verify the local {label} listener at {address}. Skirk was stopped."
    );
    if let Some(detail) = detail {
        message.push(' ');
        message.push_str(detail);
        message.push('.');
    }
    if let Some(line) = last_meaningful_log_line(log_path) {
        message.push_str(" Last log: ");
        message.push_str(&line);
        message.push('.');
    }
    message.push_str(" Open Logs for full sidecar details.");
    message
}

fn last_meaningful_log_line(path: &Path) -> Option<String> {
    let tail = read_log_tail(path, 24);
    tail.lines()
        .rev()
        .map(str::trim)
        .find(|line| !line.is_empty())
        .map(|line| {
            const MAX_LINE: usize = 220;
            let mut chars = line.chars();
            let shortened: String = chars.by_ref().take(MAX_LINE).collect();
            if chars.next().is_some() {
                format!("{shortened}...")
            } else {
                line.to_string()
            }
        })
}

fn socks5_no_auth_probe(address: &str) -> bool {
    let Ok(mut stream) = TcpStream::connect(address) else {
        return false;
    };
    let timeout = Some(Duration::from_millis(500));
    let _ = stream.set_read_timeout(timeout);
    let _ = stream.set_write_timeout(timeout);
    if stream.write_all(&[0x05, 0x01, 0x00]).is_err() {
        return false;
    }
    let mut response = [0u8; 2];
    stream.read_exact(&mut response).is_ok() && response == [0x05, 0x00]
}

fn wait_for_process_start(
    child: &mut Child,
    path: &Path,
    stable_for: Duration,
) -> Result<(), String> {
    let deadline = Instant::now() + stable_for;
    while Instant::now() < deadline {
        match child.try_wait() {
            Ok(Some(status)) => {
                let tail = read_log_tail(path, 120);
                return Err(format!("process exited early with {status}; log: {tail}"));
            }
            Ok(None) => {}
            Err(error) => {
                let tail = read_log_tail(path, 120);
                return Err(format!("process status failed: {error}; log: {tail}"));
            }
        }
        std::thread::sleep(Duration::from_millis(200));
    }
    Ok(())
}

fn connected_message(mode: &ConnectionMode) -> String {
    match mode {
        ConnectionMode::Proxy => "Connected in local proxy mode".into(),
        ConnectionMode::System => "Connected and Windows system proxy is enabled".into(),
        ConnectionMode::Vpn => "Connected in VPN mode".into(),
    }
}

fn vpn_unavailable_message() -> String {
    if !cfg!(windows) && !cfg!(target_os = "linux") {
        return "VPN mode is only available on Windows and Linux".into();
    }
    if !platform_has_tun_privileges() {
        return vpn_admin_required_message();
    }
    if cfg!(windows) {
        "VPN mode needs the bundled TUN sidecar skirk-tunnel.exe".into()
    } else {
        "VPN mode needs the bundled TUN sidecar skirk-tunnel".into()
    }
}

fn vpn_admin_required_message() -> String {
    if cfg!(windows) {
        "VPN mode needs Administrator privileges to create the Windows TUN adapter".into()
    } else {
        "VPN mode needs root or CAP_NET_ADMIN privileges to create the Linux TUN interface".into()
    }
}

fn tunnel_log_path(paths: &AppPaths) -> PathBuf {
    paths.logs_dir.join("skirk-tunnel.log")
}

fn tunnel_config_path(paths: &AppPaths) -> PathBuf {
    paths.runtime_dir.join("skirk-tunnel.json")
}

fn tunnel_interface_name() -> String {
    if cfg!(target_os = "linux") {
        "skirk-tun0".into()
    } else {
        "Skirk Tunnel".into()
    }
}

fn loopback_probe_address(address: &str) -> String {
    address
        .strip_prefix("0.0.0.0:")
        .map(|port| format!("127.0.0.1:{port}"))
        .unwrap_or_else(|| address.to_string())
}

fn listener_addresses_for_mode(profile: &ClientProfile, mode: &ConnectionMode) -> (String, String) {
    if matches!(mode, ConnectionMode::Vpn) {
        return (
            format!("127.0.0.1:{}", profile.socks_port),
            format!("127.0.0.1:{}", profile.http_port),
        );
    }
    (
        format!("{}:{}", profile.socks_host, profile.socks_port),
        format!("{}:{}", profile.http_host, profile.http_port),
    )
}

fn process_path_for_rules(path: &Path) -> String {
    normalize_windows_process_path(
        &path
            .canonicalize()
            .unwrap_or_else(|_| path.to_path_buf())
            .display()
            .to_string(),
    )
}

fn normalize_windows_process_path(path: &str) -> String {
    if let Some(rest) = path.strip_prefix(r"\\?\UNC\") {
        return format!(r"\\{rest}");
    }
    path.strip_prefix(r"\\?\").unwrap_or(path).to_string()
}

fn parse_google_ip(value: &str) -> Result<IpAddr, String> {
    let value = value.trim();
    let value = if value.is_empty() {
        default_google_ip()
    } else {
        value.to_string()
    };
    value
        .parse::<IpAddr>()
        .map_err(|_| "profile route.google_ip must be an IP address".to_string())
}

fn canonical_google_ip(value: &str) -> Result<String, String> {
    Ok(parse_google_ip(value)?.to_string())
}

fn google_control_cidr(google_ip: &str) -> String {
    let ip =
        parse_google_ip(google_ip).expect("Google control IP must be canonical before routing");
    if ip.is_ipv4() {
        format!("{ip}/32")
    } else {
        format!("{ip}/128")
    }
}

fn tunnel_config(socks_port: u16, sidecar_process_path: &str, google_ip: &str) -> Value {
    let google_control_cidr = google_control_cidr(google_ip);
    let google_control_route_cidr = google_control_cidr.clone();
    let sidecar_process_names = if cfg!(windows) {
        json!(["skirk-sidecar.exe", "skirk.exe", "skirk-windows-amd64.exe"])
    } else {
        json!(["skirk", "skirk-linux-amd64"])
    };
    // The sidecar's Drive control plane must stay outside the TUN path. Keep
    // the pinned Google CIDR as a deterministic fallback for process matching.
    json!({
        "log": {
            "level": "warn",
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
            "strategy": "ipv4_only"
        },
        "inbounds": [
            {
                "type": "tun",
                "tag": "tun-in",
                "interface_name": tunnel_interface_name(),
                "address": [
                    "172.19.0.1/30"
                ],
                "auto_route": true,
                "strict_route": true,
                "stack": "mixed",
                // Keep auto_redirect off for now; Linux E2E showed sing-box
                // can leave nftables/ip-rule state behind after GUI shutdown.
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
                    "fe80::/10",
                    google_control_cidr
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
                    "ip_cidr": [
                        google_control_route_cidr
                    ],
                    "network": "tcp",
                    "port": 443,
                    "action": "route",
                    "outbound": "direct"
                },
                {
                    "type": "logical",
                    "mode": "or",
                    "rules": [
                        {
                            "process_name": sidecar_process_names
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
                    "inbound": "tun-in",
                    "action": "sniff",
                    "timeout": "1s"
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
                    "ip_is_private": true,
                    "action": "route",
                    "outbound": "direct"
                },
                {
                    "network": "udp",
                    "action": "reject",
                    "method": "default",
                    "no_drop": true
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

fn platform_has_tun_privileges() -> bool {
    if cfg!(windows) {
        return windows_is_admin();
    }
    if cfg!(target_os = "linux") {
        return linux_has_net_admin();
    }
    false
}

#[cfg(target_os = "linux")]
fn linux_has_net_admin() -> bool {
    let Ok(status) = fs::read_to_string("/proc/self/status") else {
        return false;
    };
    status.lines().any(|line| {
        let Some(hex) = line.strip_prefix("CapEff:") else {
            return false;
        };
        let Ok(caps) = u64::from_str_radix(hex.trim(), 16) else {
            return false;
        };
        caps & (1 << 12) != 0
    })
}

#[cfg(not(target_os = "linux"))]
fn linux_has_net_admin() -> bool {
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
async fn update_profile_performance(
    runtime: State<'_, DesktopRuntime>,
    profile_id: String,
    performance: ClientPerformanceSettings,
) -> Result<DesktopSnapshot, String> {
    runtime.update_profile_performance(&profile_id, performance)?;
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
            update_profile_performance,
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
    let mut profiles: Vec<ClientProfile> = read_json(&paths.profiles_file).or_else(|error| {
        if paths.profiles_file.exists() {
            Err(error)
        } else {
            Ok(Vec::new())
        }
    })?;
    for profile in &mut profiles {
        if profile.socks_host == "0.0.0.0" || profile.http_host == "0.0.0.0" {
            profile.share_lan = true;
        }
    }
    Ok(profiles)
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

fn client_metrics_path(paths: &AppPaths) -> PathBuf {
    paths.runtime_dir.join("skirk-client-metrics.json")
}

fn read_runtime_metrics(path: &Path) -> Option<RuntimeMetrics> {
    let content = fs::read_to_string(path).ok()?;
    serde_json::from_str(&content).ok()
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
        let os_dir = sidecar_os_dir();
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
    fn process_path_for_rules_uses_win32_style_paths() {
        assert_eq!(
            normalize_windows_process_path(r"\\?\C:\Skirk\skirk-sidecar.exe"),
            r"C:\Skirk\skirk-sidecar.exe"
        );
        assert_eq!(
            normalize_windows_process_path(r"\\?\UNC\server\share\skirk-sidecar.exe"),
            r"\\server\share\skirk-sidecar.exe"
        );
    }

    #[test]
    fn listener_addresses_force_loopback_for_vpn_when_profile_shares_lan() {
        let profile = test_profile(true);
        let (socks, http) = listener_addresses_for_mode(&profile, &ConnectionMode::Vpn);
        assert_eq!(socks, "127.0.0.1:18080");
        assert_eq!(http, "127.0.0.1:18081");
    }

    #[test]
    fn listener_addresses_preserve_lan_for_proxy_and_system() {
        let profile = test_profile(true);
        assert_eq!(
            listener_addresses_for_mode(&profile, &ConnectionMode::Proxy),
            ("0.0.0.0:18080".into(), "0.0.0.0:18081".into())
        );
        assert_eq!(
            listener_addresses_for_mode(&profile, &ConnectionMode::System),
            ("0.0.0.0:18080".into(), "0.0.0.0:18081".into())
        );
    }

    #[test]
    fn normalize_performance_settings_accepts_explicit_high_custom_combo() {
        let result = normalize_performance_settings(ClientPerformanceSettings {
            preset: PerformancePreset::Custom,
            poll_ms: 250,
            upload_concurrency: 64,
            download_concurrency: 64,
            burst_poll: true,
            burst_poll_ms: 75,
            burst_poll_window_ms: 5000,
        });
        assert!(result.is_ok());
    }

    #[test]
    fn startup_probe_error_does_not_dump_log_tail() {
        let dir = std::env::temp_dir().join(format!("skirk-test-{}", epoch_millis()));
        fs::create_dir_all(&dir).expect("tempdir");
        let log_path = dir.join("skirk-client.log");
        fs::write(
            &log_path,
            "old line\nskirk client SOCKS5 listening on 127.0.0.1:18080\nanother detailed line\n",
        )
        .expect("write log");

        let message = startup_probe_error("SOCKS", "127.0.0.1:18080", None, &log_path);
        assert!(message.contains("Open Logs for full sidecar details"));
        assert!(!message.contains("old line"));
        assert!(!message.contains("skirk client SOCKS5 listening"));
        let _ = fs::remove_dir_all(dir);
    }

    #[test]
    fn tunnel_config_uses_sing_box_1_13_tun_fields() {
        let config = tunnel_config(18080, r"C:\Skirk\skirk-sidecar.exe", "216.239.38.120");
        let inbound = config
            .pointer("/inbounds/0")
            .and_then(Value::as_object)
            .expect("tun inbound object");

        assert_eq!(
            inbound
                .get("address")
                .and_then(Value::as_array)
                .map(Vec::len),
            Some(1)
        );
        assert_eq!(
            inbound
                .get("address")
                .and_then(Value::as_array)
                .and_then(|addresses| addresses.first())
                .and_then(Value::as_str),
            Some("172.19.0.1/30")
        );
        assert!(inbound.get("sniff").is_none());
        assert!(inbound.get("sniff_override_destination").is_none());
        assert!(inbound.get("inet4_address").is_none());
        assert!(inbound.get("inet6_address").is_none());

        assert_eq!(
            config.pointer("/log/level").and_then(Value::as_str),
            Some("warn")
        );
        assert_eq!(
            config.pointer("/dns/strategy").and_then(Value::as_str),
            Some("ipv4_only")
        );
        assert!(config
            .pointer("/inbounds/0/route_exclude_address")
            .and_then(Value::as_array)
            .expect("route_exclude_address")
            .iter()
            .any(|value| value.as_str() == Some("216.239.38.120/32")));
        assert_eq!(
            config
                .pointer("/route/rules/0/ip_cidr/0")
                .and_then(Value::as_str),
            Some("216.239.38.120/32")
        );
        assert_eq!(
            config
                .pointer("/route/rules/0/outbound")
                .and_then(Value::as_str),
            Some("direct")
        );
        assert_eq!(
            config
                .pointer("/route/rules/0/network")
                .and_then(Value::as_str),
            Some("tcp")
        );
        assert_eq!(
            config
                .pointer("/route/rules/0/port")
                .and_then(Value::as_i64),
            Some(443)
        );
        assert_eq!(
            config
                .pointer("/route/rules/1/action")
                .and_then(Value::as_str),
            Some("route")
        );
        assert_eq!(
            config
                .pointer("/route/rules/1/outbound")
                .and_then(Value::as_str),
            Some("direct")
        );
        assert_eq!(
            config
                .pointer("/route/rules/1/rules/0/process_name/0")
                .and_then(Value::as_str),
            if cfg!(windows) {
                Some("skirk-sidecar.exe")
            } else {
                Some("skirk")
            }
        );
        assert_eq!(
            config
                .pointer("/route/rules/1/rules/1/process_path/0")
                .and_then(Value::as_str),
            Some(r"C:\Skirk\skirk-sidecar.exe")
        );
        assert_eq!(
            config
                .pointer("/route/rules/2/action")
                .and_then(Value::as_str),
            Some("sniff")
        );
        assert_eq!(
            config
                .pointer("/route/rules/2/inbound")
                .and_then(Value::as_str),
            Some("tun-in")
        );
        assert_eq!(
            config
                .pointer("/route/rules/2/timeout")
                .and_then(Value::as_str),
            Some("1s")
        );
        assert_eq!(
            config
                .pointer("/route/rules/5/action")
                .and_then(Value::as_str),
            Some("reject")
        );
        assert_eq!(
            config
                .pointer("/route/rules/5/network")
                .and_then(Value::as_str),
            Some("udp")
        );
        assert_eq!(
            config
                .pointer("/route/rules/5/no_drop")
                .and_then(Value::as_bool),
            Some(true)
        );
        assert_eq!(
            config
                .pointer("/route/find_process")
                .and_then(Value::as_bool),
            Some(true)
        );
        assert!(config.pointer("/inbounds/0/auto_redirect").is_none());
    }

    #[test]
    fn tunnel_config_uses_custom_google_control_ip() {
        let config = tunnel_config(18080, r"C:\Skirk\skirk-sidecar.exe", "8.8.8.8");
        assert!(config
            .pointer("/inbounds/0/route_exclude_address")
            .and_then(Value::as_array)
            .expect("route_exclude_address")
            .iter()
            .any(|value| value.as_str() == Some("8.8.8.8/32")));
        assert_eq!(
            config
                .pointer("/route/rules/0/ip_cidr/0")
                .and_then(Value::as_str),
            Some("8.8.8.8/32")
        );
    }

    #[test]
    fn google_control_ip_is_canonicalized() {
        assert_eq!(
            canonical_google_ip("").expect("default Google IP"),
            "216.239.38.120"
        );
        assert_eq!(
            canonical_google_ip(" 8.8.8.8 ").expect("custom Google IP"),
            "8.8.8.8"
        );
        assert!(canonical_google_ip("not-an-ip").is_err());
    }

    fn test_profile(share_lan: bool) -> ClientProfile {
        ClientProfile {
            id: "profile-test".into(),
            name: "Test profile".into(),
            config_path: "/tmp/client.skirk".into(),
            socks_host: if share_lan { "0.0.0.0" } else { "127.0.0.1" }.into(),
            socks_port: 18080,
            http_host: if share_lan { "0.0.0.0" } else { "127.0.0.1" }.into(),
            http_port: 18081,
            share_lan,
            route_mode: "google_front_pinned".into(),
            google_ip: "216.239.38.120".into(),
            drive_space: "appDataFolder".into(),
            drive_folder_id: String::new(),
            performance: default_performance_settings(),
        }
    }
}
