#[cfg(any(target_os = "linux", test))]
mod linux_tun;
mod system_proxy;

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::{
    collections::BTreeSet,
    fs::{self, OpenOptions},
    io::{Read, Seek, SeekFrom, Write},
    net::{IpAddr, TcpListener, TcpStream},
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    sync::{
        atomic::{AtomicBool, AtomicU64, Ordering},
        Mutex,
    },
    time::{Duration, Instant},
};
use tauri::{AppHandle, Manager, State};

use system_proxy::SystemProxyManager;

#[cfg(unix)]
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};

#[cfg(windows)]
use std::os::windows::process::CommandExt;

#[cfg(windows)]
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

const CLIENT_READY_TIMEOUT: Duration = Duration::from_secs(30);
const CUSTOM_MIN_POLL_MS: u16 = 250;
const CUSTOM_MAX_UPLOAD_WORKERS: u16 = 64;
const CUSTOM_MAX_DOWNLOAD_WORKERS: u16 = 64;
const LOG_TAIL_MAX_BYTES: u64 = 256 * 1024;
static NEXT_PRIVATE_WRITE: AtomicU64 = AtomicU64::new(0);

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
    system_proxy_restore_pending: bool,
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
    profile_lifecycle: Mutex<()>,
    connect_epoch: AtomicU64,
    connect_in_progress: AtomicBool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ExitProbeResult {
    status: String,
    #[serde(default, alias = "round_trip_ms")]
    round_trip_ms: u64,
    #[serde(default)]
    reason: String,
}

struct ConnectAttempt<'a>(&'a AtomicBool);

impl Drop for ConnectAttempt<'_> {
    fn drop(&mut self) {
        self.0.store(false, Ordering::Release);
    }
}

#[derive(Clone)]
struct AppPaths {
    config_dir: PathBuf,
    runtime_dir: PathBuf,
    logs_dir: PathBuf,
    profiles_file: PathBuf,
    settings_file: PathBuf,
}

fn tunnel_transport_for_identity(config: &Value) -> Result<&str, String> {
    let tunnel = match config.get("tunnel") {
        None | Some(Value::Null) => return Ok("muxv5"),
        Some(Value::Object(value)) => value,
        _ => return Err("client config has an invalid Skirk transport section".into()),
    };
    match tunnel.get("transport") {
        None | Some(Value::Null) => Ok("muxv5"),
        Some(Value::String(value)) => {
            let version = value.trim();
            Ok(if version.is_empty() { "muxv5" } else { version })
        }
        _ => Err("client config has an invalid Skirk transport version".into()),
    }
}

impl DesktopRuntime {
    fn new(paths: AppPaths, resource_dir: Option<PathBuf>) -> Self {
        Self {
            paths,
            resource_dir,
            inner: Mutex::new(RuntimeState::default()),
            profile_lifecycle: Mutex::new(()),
            connect_epoch: AtomicU64::new(0),
            connect_in_progress: AtomicBool::new(false),
        }
    }

    fn exit_probe_command(&self) -> Result<(PathBuf, String, String), String> {
        let (profile_id, socks_address) = {
            let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
            refresh_state(&mut state, &self.paths);
            if !matches!(state.phase, ConnectionPhase::Connected) {
                return Err("Connect before testing the exit".into());
            }
            let client = state.client.as_ref().ok_or("Skirk client is not running")?;
            (
                client.profile_id.clone(),
                loopback_probe_address(&client.socks_address),
            )
        };
        let profile = load_profiles(&self.paths)?
            .into_iter()
            .find(|item| item.id == profile_id)
            .ok_or("Active profile is unavailable")?;
        Ok((self.resolve_sidecar()?, profile.config_path, socks_address))
    }

    fn snapshot(&self) -> Result<DesktopSnapshot, String> {
        let profiles = load_profiles(&self.paths)?;
        let settings = load_settings(&self.paths)?;
        let selected_profile_id = if settings
            .selected_profile_id
            .as_ref()
            .map(|id| profiles.iter().any(|profile| &profile.id == id))
            .unwrap_or(false)
        {
            settings.selected_profile_id.clone()
        } else {
            profiles.first().map(|profile| profile.id.clone())
        };

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
                system_proxy_restore_pending: false,
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
                system_proxy_restore_pending: SystemProxyManager::restore_pending(&self.paths),
                tunnel_active: false,
                tunnel_interface_name: None,
                message: state.message.clone(),
            }
        };

        Ok(DesktopSnapshot {
            profiles,
            selected_profile_id,
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
        if self.connect_in_progress.load(Ordering::Acquire) {
            return Err("cancel connection startup before importing a profile".into());
        }
        let _lifecycle = self
            .profile_lifecycle
            .lock()
            .map_err(|_| "profile lifecycle lock poisoned")?;
        self.ensure_idle_for_profile_change("importing a profile")?;
        let raw_config = raw_config.trim();
        if raw_config.is_empty() {
            return Err("client config is empty".into());
        }
        let stored_config =
            extract_inline_config(raw_config).unwrap_or_else(|| raw_config.to_string());
        let parsed = self.decode_config(&stored_config)?;
        // Import starts on the Google front even when an older kit encodes a
        // direct route. The user can explicitly choose Direct Google in the UI.
        let route_mode = "google_front_pinned".to_string();
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
        write_private_file(&config_path, stored_config.as_bytes())?;

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
        )?;
        self.clear_idle_connection_error()
    }

    fn replace_profile_config(&self, profile_id: &str, raw_config: String) -> Result<(), String> {
        if self.connect_in_progress.load(Ordering::Acquire) {
            return Err("cancel connection startup before replacing a profile".into());
        }
        let _lifecycle = self
            .profile_lifecycle
            .lock()
            .map_err(|_| "profile lifecycle lock poisoned")?;
        self.ensure_idle_for_profile_change("replacing a profile")?;

        let raw_config = raw_config.trim();
        if raw_config.is_empty() {
            return Err("client config is empty".into());
        }
        let stored_config =
            extract_inline_config(raw_config).unwrap_or_else(|| raw_config.to_string());
        let parsed = self.decode_config(&stored_config)?;
        let drive_space = parsed
            .pointer("/drive/space")
            .and_then(Value::as_str)
            .unwrap_or("");
        let drive_folder_id = parsed
            .pointer("/drive/folder_id")
            .and_then(Value::as_str)
            .unwrap_or("");
        if drive_space != "appDataFolder" && drive_folder_id.is_empty() {
            return Err("client config is missing a Drive mailbox".into());
        }

        let mut profiles = load_profiles(&self.paths)?;
        let profile = profiles
            .iter_mut()
            .find(|profile| profile.id == profile_id)
            .ok_or_else(|| "profile was not found".to_string())?;
        if profile.drive_space != drive_space || profile.drive_folder_id != drive_folder_id {
            return Err(
                "updated client config uses a different Drive mailbox; import it as a new profile"
                    .into(),
            );
        }
        let previous_raw = fs::read_to_string(&profile.config_path)
            .map_err(|error| format!("failed to read saved client config: {error}"))?;
        let previous = self.decode_config(&previous_raw)?;
        for field in ["/session_id", "/secret", "/auth/client_id"] {
            let previous_value = previous
                .pointer(field)
                .and_then(Value::as_str)
                .filter(|value| !value.is_empty());
            let renewed_value = parsed
                .pointer(field)
                .and_then(Value::as_str)
                .filter(|value| !value.is_empty());
            if previous_value.is_none() || previous_value != renewed_value {
                return Err("updated client config must keep the same session, tunnel secret, and OAuth client; import a different setup as a new profile".into());
            }
        }
        if tunnel_transport_for_identity(&previous)? != tunnel_transport_for_identity(&parsed)? {
            return Err("updated client config uses a different Skirk transport version; import it as a new profile".into());
        }
        let google_ip = parsed
            .pointer("/route/google_ip")
            .and_then(Value::as_str)
            .unwrap_or("");
        let google_ip = canonical_google_ip(google_ip)?;
        write_private_file(Path::new(&profile.config_path), stored_config.as_bytes())?;
        profile.google_ip = google_ip;
        save_profiles(&self.paths, &profiles)?;
        let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
        state.phase = ConnectionPhase::Disconnected;
        state.message = "Profile updated. Connect again.".into();
        Ok(())
    }

    fn decode_config(&self, raw_config: &str) -> Result<Value, String> {
        if let Some(inline_config) = extract_inline_config(raw_config) {
            return decode_profile_with_sidecar(&self.resolve_sidecar()?, &inline_config);
        }
        serde_json::from_str(raw_config).map_err(|error| format!("invalid JSON: {error}"))
    }

    fn export_saved_profile(&self, profile_id: &str) -> Result<String, String> {
        let _lifecycle = self
            .profile_lifecycle
            .lock()
            .map_err(|_| "profile lifecycle lock poisoned")?;
        let profile = load_profiles(&self.paths)?
            .into_iter()
            .find(|profile| profile.id == profile_id)
            .ok_or("This profile is no longer saved.")?;
        let unavailable = "Could not read the saved profile. Import the original profile again.";
        let config = fs::canonicalize(&profile.config_path).map_err(|_| unavailable)?;
        let root = fs::canonicalize(&self.paths.config_dir).map_err(|_| unavailable)?;
        if config.parent() != Some(root.as_path()) || !config.is_file() {
            return Err(unavailable.into());
        }
        let raw = fs::read_to_string(&config).map_err(|_| unavailable)?;
        if raw.trim_start().starts_with("skirk:") {
            return extract_inline_config(&raw).ok_or_else(|| unavailable.into());
        }
        export_profile_with_sidecar(&self.resolve_sidecar()?, &config)
    }

    fn delete_profile(&self, profile_id: &str) -> Result<(), String> {
        if self.connect_in_progress.load(Ordering::Acquire) {
            return Err("cancel connection startup before deleting a profile".into());
        }
        let _lifecycle = self
            .profile_lifecycle
            .lock()
            .map_err(|_| "profile lifecycle lock poisoned")?;
        self.ensure_idle_for_profile_change("deleting a profile")?;
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
        if self.connect_in_progress.load(Ordering::Acquire) {
            return Err("cancel connection startup before changing performance settings".into());
        }
        let _lifecycle = self
            .profile_lifecycle
            .lock()
            .map_err(|_| "profile lifecycle lock poisoned")?;
        self.ensure_idle_for_profile_change("changing performance settings")?;
        let performance = normalize_performance_settings(performance)?;
        let mut profiles = load_profiles(&self.paths)?;
        let profile = profiles
            .iter_mut()
            .find(|profile| profile.id == profile_id)
            .ok_or_else(|| "profile was not found".to_string())?;
        profile.performance = performance;
        save_profiles(&self.paths, &profiles)
    }

    fn update_profile_route(&self, profile_id: &str, route_mode: &str) -> Result<(), String> {
        if self.connect_in_progress.load(Ordering::Acquire) {
            return Err("cancel connection startup before changing the Google access route".into());
        }
        let _lifecycle = self
            .profile_lifecycle
            .lock()
            .map_err(|_| "profile lifecycle lock poisoned")?;
        self.ensure_idle_for_profile_change("changing the Google access route")?;
        if !matches!(route_mode, "google_front_pinned" | "direct") {
            return Err("choose Google front or direct Google".into());
        }
        let mut profiles = load_profiles(&self.paths)?;
        let profile = profiles
            .iter_mut()
            .find(|profile| profile.id == profile_id)
            .ok_or_else(|| "profile was not found".to_string())?;
        profile.route_mode = route_mode.to_string();
        save_profiles(&self.paths, &profiles)
    }

    fn select_profile(&self, profile_id: Option<String>) -> Result<(), String> {
        if self.connect_in_progress.load(Ordering::Acquire) {
            return Err("cancel connection startup before switching profiles".into());
        }
        let _lifecycle = self
            .profile_lifecycle
            .lock()
            .map_err(|_| "profile lifecycle lock poisoned")?;
        self.ensure_idle_for_profile_change("switching profiles")?;
        save_settings(
            &self.paths,
            &Settings {
                selected_profile_id: profile_id,
                ..load_settings(&self.paths)?
            },
        )?;
        self.clear_idle_connection_error()
    }

    fn clear_idle_connection_error(&self) -> Result<(), String> {
        let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
        if state.client.is_none()
            && matches!(state.phase, ConnectionPhase::Error)
            && !SystemProxyManager::restore_pending(&self.paths)
        {
            state.phase = ConnectionPhase::Disconnected;
            state.message = "Profile selected. Connect to test it.".into();
        }
        Ok(())
    }

    fn set_connection_mode(&self, mode: ConnectionMode) -> Result<(), String> {
        if self.connect_in_progress.load(Ordering::Acquire) {
            return Err("cancel connection startup before changing connection mode".into());
        }
        let _lifecycle = self
            .profile_lifecycle
            .lock()
            .map_err(|_| "profile lifecycle lock poisoned")?;
        self.ensure_idle_for_profile_change("changing connection mode")?;
        if matches!(mode, ConnectionMode::System) && !(cfg!(windows) || cfg!(target_os = "macos")) {
            return Err("system proxy mode is only available on Windows and macOS".into());
        }
        if matches!(mode, ConnectionMode::Vpn) && !self.platform_capabilities().vpn_mode_supported {
            return Err(vpn_unavailable_message());
        }
        let mut settings = load_settings(&self.paths)?;
        settings.connection_mode = mode;
        save_settings(&self.paths, &settings)
    }

    fn ensure_idle_for_profile_change(&self, action: &str) -> Result<(), String> {
        let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
        refresh_state(&mut state, &self.paths);
        if state.client.is_some() {
            return Err(format!("disconnect before {action}"));
        }
        if self.connect_in_progress.load(Ordering::Acquire) {
            return Err(format!("cancel connection startup before {action}"));
        }
        Ok(())
    }

    fn connect(&self) -> Result<(), String> {
        let attempt_epoch = self.connect_epoch.load(Ordering::Acquire);
        if self.connect_in_progress.swap(true, Ordering::AcqRel) {
            return Err("connection startup is already in progress".into());
        }
        let _attempt_guard = ConnectAttempt(&self.connect_in_progress);
        let _lifecycle = self
            .profile_lifecycle
            .lock()
            .map_err(|_| "profile lifecycle lock poisoned")?;
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
        if matches!(mode, ConnectionMode::System) && !(cfg!(windows) || cfg!(target_os = "macos")) {
            return Err("system proxy mode is only available on Windows and macOS".into());
        }
        if matches!(mode, ConnectionMode::Vpn) && !self.platform_capabilities().vpn_mode_supported {
            return Err(vpn_unavailable_message());
        }
        if SystemProxyManager::restore_pending(&self.paths) {
            if let Err(error) = SystemProxyManager::cleanup_stale_proxy(&self.paths) {
                return Err(self.mark_proxy_restore_error(&error));
            }
        }
        let (socks_address, http_address) = listener_addresses_for_mode(&profile, &mode);
        let route_mode = profile.route_mode.as_str();
        let google_ip = canonical_google_ip(&profile.google_ip)?;
        let tunnel_preview = if matches!(mode, ConnectionMode::Vpn) {
            let raw = fs::read_to_string(&profile.config_path)
                .map_err(|error| format!("failed to read VPN profile: {error}"))?;
            TunnelPreviewOptions::from_config(&self.decode_config(&raw)?)?
        } else {
            TunnelPreviewOptions::default()
        };
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
            if self.connect_epoch.load(Ordering::Acquire) != attempt_epoch {
                return Err("Connection cancelled".into());
            }
            state.phase = ConnectionPhase::Connecting;
            state.message = "Starting Skirk sidecar".into();
        }
        let child = command.spawn().map_err(|error| {
            self.mark_connect_error(format!("failed to start skirk: {error}"), attempt_epoch)
        })?;
        let mut child = child;

        let socks_probe_address = loopback_probe_address(&socks_address);
        let http_probe_address = loopback_probe_address(&http_address);
        if let Err(error) = wait_for_socks5_endpoint_or_exit(
            &mut child,
            &socks_probe_address,
            CLIENT_READY_TIMEOUT,
            &log_path,
            &|| self.connect_epoch.load(Ordering::Acquire) != attempt_epoch,
        ) {
            let _ = child.kill();
            let _ = child.wait();
            return Err(self.mark_connect_error(error, attempt_epoch));
        }
        if let Err(error) = wait_for_tcp_endpoint_or_exit(
            &mut child,
            &http_probe_address,
            "HTTP proxy",
            CLIENT_READY_TIMEOUT,
            &log_path,
            &|| self.connect_epoch.load(Ordering::Acquire) != attempt_epoch,
        ) {
            let _ = child.kill();
            let _ = child.wait();
            return Err(self.mark_connect_error(error, attempt_epoch));
        }

        if self.connect_epoch.load(Ordering::Acquire) != attempt_epoch {
            terminate_child(&mut child);
            return Err("Connection cancelled".into());
        }

        let mut tunnel = None;
        if matches!(mode, ConnectionMode::Vpn) {
            match self.spawn_tunnel(profile.socks_port, &sidecar_process_path, &google_ip, tunnel_preview) {
                Ok(next_tunnel) => {
                    tunnel = Some(next_tunnel);
                }
                Err(error) => {
                    let _ = child.kill();
                    let _ = child.wait();
                    return Err(self.mark_connect_error(error, attempt_epoch));
                }
            }
        }
        let system_proxy_enabled = if matches!(mode, ConnectionMode::System) {
            if let Err(error) = SystemProxyManager::enable(&self.paths, profile.http_port) {
                let _ = child.kill();
                let _ = child.wait();
                if let Err(restore_error) = SystemProxyManager::cleanup_stale_proxy(&self.paths) {
                    return Err(self.mark_proxy_restore_error(&restore_error));
                }
                return Err(self.mark_connect_error(error, attempt_epoch));
            }
            true
        } else {
            false
        };

        let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
        if self.connect_epoch.load(Ordering::Acquire) != attempt_epoch {
            let restore = if system_proxy_enabled {
                SystemProxyManager::disable(&self.paths).map_err(|error| proxy_restore_message(&error))
            } else {
                Ok(())
            };
            let tunnel_stop = tunnel.as_mut().map(stop_tunnel).transpose();
            terminate_child(&mut child);
            if let Err(message) = tunnel_stop.and(restore) {
                state.phase = ConnectionPhase::Error;
                state.message = message.clone();
                return Err(message);
            }
            return Err("Connection cancelled".into());
        }
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

    fn mark_connect_error(&self, message: String, attempt_epoch: u64) -> String {
        if message == "Connection cancelled"
            || self.connect_epoch.load(Ordering::Acquire) != attempt_epoch
        {
            return "Connection cancelled".into();
        }
        if let Ok(mut state) = self.inner.lock() {
            if self.connect_epoch.load(Ordering::Acquire) != attempt_epoch {
                return "Connection cancelled".into();
            }
            if state.client.is_none() {
                state.phase = ConnectionPhase::Error;
                state.message = message.clone();
            }
        }
        message
    }

    fn mark_proxy_restore_error(&self, error: &str) -> String {
        let message = proxy_restore_message(error);
        if let Ok(mut state) = self.inner.lock() {
            state.phase = ConnectionPhase::Error;
            state.message = message.clone();
        }
        message
    }

    fn disconnect(&self) -> Result<(), String> {
        self.connect_epoch.fetch_add(1, Ordering::AcqRel);
        let mut state = self.inner.lock().map_err(|_| "runtime lock poisoned")?;
        state.phase = ConnectionPhase::Disconnecting;
        match stop_runtime(&mut state, &self.paths) {
            Ok(()) => {
                state.phase = ConnectionPhase::Disconnected;
                state.message = "Disconnected".into();
                Ok(())
            }
            Err(message) => {
                state.phase = ConnectionPhase::Error;
                state.message = message.clone();
                Err(message)
            }
        }
    }

    fn cleanup_for_exit(&self) {
        if let Ok(mut state) = self.inner.lock() {
            if let Err(error) = stop_runtime(&mut state, &self.paths) {
                eprintln!("Skirk: {error}");
            }
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
            system_proxy_supported: cfg!(windows) || cfg!(target_os = "macos"),
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
        preview: TunnelPreviewOptions,
    ) -> Result<ManagedTunnel, String> {
        if !cfg!(windows) && !cfg!(target_os = "linux") {
            return Err("VPN mode is only available on Windows and Linux".into());
        }
        if !platform_has_tun_privileges() {
            return Err(vpn_admin_required_message());
        }
        let interface_name = tunnel_interface_name();
        #[cfg(target_os = "linux")]
        linux_tun::require_available(&interface_name)?;
        let tunnel = self.resolve_tunnel_sidecar()?;
        let log_path = tunnel_log_path(&self.paths);
        let config_path = tunnel_config_path(&self.paths);
        let config = tunnel_config_with_preview(socks_port, sidecar_process_path, google_ip, preview);
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
            let cleanup_error = stop_tunnel_child(&mut child, &interface_name).err();
            return Err(append_cleanup_error(format!("VPN sidecar failed to start: {error}"), cleanup_error));
        }
        Ok(ManagedTunnel {
            child,
            interface_name,
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

fn decode_profile_with_sidecar(sidecar: &Path, profile: &str) -> Result<Value, String> {
    let mut command = Command::new(sidecar);
    command
        .args(["config", "decode", "--stdin", "--out", "-"])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null());
    #[cfg(windows)]
    command.creation_flags(CREATE_NO_WINDOW);
    let mut child = command
        .spawn()
        .map_err(|error| format!("could not start the profile decoder: {error}"))?;
    let write = child
        .stdin
        .take()
        .expect("profile decoder has piped stdin")
        .write_all(profile.as_bytes());
    let output = child
        .wait_with_output()
        .map_err(|error| format!("could not read the profile decoder response: {error}"))?;
    if !output.status.success() {
        return Err("Could not read this Skirk profile. Paste the complete profile again.".into());
    }
    write.map_err(|error| format!("could not send the profile to the decoder: {error}"))?;
    serde_json::from_slice(&output.stdout)
        .map_err(|_| "The profile decoder returned an invalid response.".into())
}

fn export_profile_with_sidecar(sidecar: &Path, config: &Path) -> Result<String, String> {
    let mut command = Command::new(sidecar);
    command
        .args(["config", "export", "--config"])
        .arg(config)
        .stdin(Stdio::null())
        .stderr(Stdio::null());
    #[cfg(windows)]
    command.creation_flags(CREATE_NO_WINDOW);
    let unavailable = "Could not export the saved profile. Import the original profile again.";
    let output = command.output().map_err(|_| unavailable)?;
    if !output.status.success() {
        return Err(unavailable.into());
    }
    let profile = String::from_utf8(output.stdout).map_err(|_| unavailable)?;
    extract_inline_config(&profile).ok_or_else(|| unavailable.into())
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
        if !matches!(
            state.phase,
            ConnectionPhase::Error | ConnectionPhase::Connecting
        ) {
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
        let restore_error = stop_runtime(state, paths).err();
        state.phase = ConnectionPhase::Error;
        state.message = append_cleanup_error(message, restore_error);
        return;
    }
    match client.child.try_wait() {
        Ok(Some(status)) => {
            let restore_error = stop_runtime(state, paths).err();
            state.phase = ConnectionPhase::Error;
            state.message = append_cleanup_error(
                client_exit_message(status.code(), &status.to_string()),
                restore_error,
            );
        }
        Ok(None) => {
            state.phase = ConnectionPhase::Connected;
        }
        Err(error) => {
            let restore_error = stop_runtime(state, paths).err();
            state.phase = ConnectionPhase::Error;
            state.message =
                append_cleanup_error(format!("Skirk status failed: {error}"), restore_error);
        }
    }
}

fn proxy_restore_message(error: &str) -> String {
    format!(
        "The system proxy may still point to Skirk's stopped local listener. Select Retry proxy restore or turn off the Skirk localhost proxy in network settings. Detail: {error}"
    )
}

fn append_cleanup_error(message: String, cleanup_error: Option<String>) -> String {
    match cleanup_error {
        Some(error) => format!("{message} {error}"),
        None => message,
    }
}

fn client_exit_message(code: Option<i32>, status: &str) -> String {
    if code == Some(78) {
        return "Google access needs renewal (invalid_grant). Ask the exit owner for an updated client.skirk, then replace this profile.".into();
    }
    format!("Skirk exited: {status}")
}

fn stop_runtime(state: &mut RuntimeState, paths: &AppPaths) -> Result<(), String> {
    let tunnel_stop = state.tunnel.take().as_mut().map(stop_tunnel).transpose();
    let restore = if let Some(mut client) = state.client.take() {
        let restore = if client.system_proxy_enabled {
            SystemProxyManager::disable(paths)
        } else {
            Ok(())
        };
        terminate_child(&mut client.child);
        restore
    } else {
        SystemProxyManager::cleanup_stale_proxy(paths)
    };
    tunnel_stop.and(restore.map_err(|error| proxy_restore_message(&error)))
}

fn terminate_child(child: &mut Child) {
    let _ = child.kill();
    let _ = child.wait();
}

fn stop_tunnel(tunnel: &mut ManagedTunnel) -> Result<(), String> {
    stop_tunnel_child(&mut tunnel.child, &tunnel.interface_name)
}

#[cfg(target_os = "linux")]
fn stop_tunnel_child(child: &mut Child, interface: &str) -> Result<(), String> {
    linux_tun::stop_owned(child, Duration::from_secs(5))?;
    linux_tun::require_available(interface)
        .map_err(|error| format!("Linux VPN route cleanup could not be verified. {error}"))
}

#[cfg(not(target_os = "linux"))]
fn stop_tunnel_child(child: &mut Child, _interface: &str) -> Result<(), String> {
    terminate_child(child);
    Ok(())
}

fn wait_for_tcp_endpoint_or_exit(
    child: &mut Child,
    address: &str,
    label: &str,
    timeout: Duration,
    log_path: &Path,
    cancelled: &impl Fn() -> bool,
) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if cancelled() {
            return Err("Connection cancelled".into());
        }
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
    cancelled: &impl Fn() -> bool,
) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if cancelled() {
            return Err("Connection cancelled".into());
        }
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
                return Err(process_start_error(
                    path,
                    &format!("process exited early with {status}"),
                ));
            }
            Ok(None) => {}
            Err(error) => {
                return Err(process_start_error(
                    path,
                    &format!("process status failed: {error}"),
                ));
            }
        }
        std::thread::sleep(Duration::from_millis(200));
    }
    Ok(())
}

fn process_start_error(path: &Path, detail: &str) -> String {
    let mut message = detail.to_string();
    if !message.ends_with('.') {
        message.push('.');
    }
    if let Some(line) = last_meaningful_log_line(path) {
        message.push_str(" Last log: ");
        message.push_str(&line);
        message.push('.');
    }
    message.push_str(" Open Logs for full sidecar details.");
    message
}

fn connected_message(mode: &ConnectionMode) -> String {
    match mode {
        ConnectionMode::Proxy => "Connected in local proxy mode".into(),
        ConnectionMode::System => "Connected and system proxy is enabled".into(),
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

#[derive(Clone, Copy, Debug, Default)]
struct TunnelPreviewOptions {
    public_udp: bool,
    remote_dns: bool,
    ipv6: bool,
}

impl TunnelPreviewOptions {
    fn from_config(config: &Value) -> Result<Self, String> {
        let flag = |name: &str| -> Result<bool, String> {
            match config.pointer(&format!("/tunnel/{name}")) {
                None => Ok(false),
                Some(Value::Bool(value)) => Ok(*value),
                Some(_) => Err(format!("tunnel.{name} must be a boolean")),
            }
        };
        let options = Self {
            public_udp: flag("udp_preview_public_ipv4")?,
            remote_dns: flag("udp_preview_remote_dns")?,
            ipv6: flag("udp_preview_ipv6")?,
        };
        if options.remote_dns && !options.public_udp
            || options.ipv6 && (!options.remote_dns || !flag("udp_preview_stable_udp")?)
        {
            return Err("VPN preview requires matching public UDP, exit DNS and stable IPv6 settings".into());
        }
        Ok(options)
    }
}

#[cfg(test)]
fn tunnel_config(socks_port: u16, sidecar_process_path: &str, google_ip: &str) -> Value {
    tunnel_config_with_preview(socks_port, sidecar_process_path, google_ip, TunnelPreviewOptions::default())
}

fn tunnel_config_with_preview(socks_port: u16, sidecar_process_path: &str, google_ip: &str, preview: TunnelPreviewOptions) -> Value {
    let google_control_cidr = google_control_cidr(google_ip);
    let google_control_route_cidr = google_control_cidr.clone();
    let sidecar_process_names = if cfg!(windows) {
        json!(["skirk-sidecar.exe", "skirk.exe", "skirk-windows-amd64.exe"])
    } else {
        json!(["skirk", "skirk-linux-amd64"])
    };
    // The sidecar's Drive control plane must stay outside the TUN path. Keep
    // the pinned Google CIDR as a deterministic fallback for process matching.
    let mut config = json!({
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
    });
    #[cfg(target_os = "linux")]
    {
        config["inbounds"][0]["iproute2_table_index"] = json!(linux_tun::ROUTE_TABLE);
        config["inbounds"][0]["iproute2_rule_index"] = json!(linux_tun::FIRST_RULE);
    }
    if preview.remote_dns {
        // This literal is intercepted by Skirk's authenticated exit-DNS stream.
        // Explicit detour prevents sing-box's DNS dialer using a direct socket.
        config["dns"] = json!({
            "servers": [{"type": "tcp", "tag": "exit-dns", "server": "198.18.0.2", "server_port": 53, "detour": "proxy"}],
            "final": "exit-dns",
            "strategy": if preview.ipv6 { "prefer_ipv4" } else { "ipv4_only" }
        });
        config["route"]["default_domain_resolver"] = json!("exit-dns");
    }
    if preview.public_udp {
        config["inbounds"][0]["mtu"] = json!(1280);
        // Keep standard SOCKS5 UDP association framing; sing-box's optional
        // UDP-over-TCP extension is not the HEV framing implemented by Skirk.
        config["route"]["rules"].as_array_mut().unwrap().retain(|rule| {
            !(rule["network"] == "udp" && rule["action"] == "reject")
        });
    }
    if preview.public_udp {
        // Capture the family even when public IPv6 UDP is disabled. Omitting
        // its route would let literal IPv6 traffic evade the preview policy.
        config["inbounds"][0]["address"].as_array_mut().unwrap()
            .push(json!("fd9d:36cb:b8bb::1/126"));
        if !preview.ipv6 {
            config["route"]["rules"].as_array_mut().unwrap().push(json!({
                "ip_version": 6, "network": "udp", "action": "reject",
                "method": "default", "no_drop": true
            }));
        }
    }
    config
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
async fn replace_profile_config(
    runtime: State<'_, DesktopRuntime>,
    profile_id: String,
    raw_config: String,
) -> Result<DesktopSnapshot, String> {
    runtime.replace_profile_config(&profile_id, raw_config)?;
    runtime.snapshot()
}

#[tauri::command]
async fn export_saved_profile(app: AppHandle, profile_id: String) -> Result<String, String> {
    tauri::async_runtime::spawn_blocking(move || {
        app.state::<DesktopRuntime>().export_saved_profile(&profile_id)
    })
    .await
    .map_err(|_| "Could not open the saved profile.".to_string())?
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
async fn update_profile_route(
    runtime: State<'_, DesktopRuntime>,
    profile_id: String,
    route_mode: String,
) -> Result<DesktopSnapshot, String> {
    runtime.update_profile_route(&profile_id, &route_mode)?;
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

fn run_exit_probe_sidecar(
    skirk: PathBuf,
    config: String,
    socks: String,
) -> Result<ExitProbeResult, String> {
    let mut command = Command::new(skirk);
    command
        .arg("probe-exit")
        .arg("--config")
        .arg(config)
        .arg("--socks")
        .arg(socks);
    #[cfg(windows)]
    command.creation_flags(CREATE_NO_WINDOW);
    let output = command
        .output()
        .map_err(|_| "Could not start the exit test".to_string())?;
    let result: ExitProbeResult = serde_json::from_slice(&output.stdout)
        .map_err(|_| "The exit test did not return a valid result".to_string())?;
    if !matches!(result.status.as_str(), "verified" | "unverified")
        || (!output.status.success() && result.status != "unverified")
    {
        return Err("The exit test did not finish correctly".into());
    }
    Ok(result)
}

#[tauri::command]
async fn probe_exit(runtime: State<'_, DesktopRuntime>) -> Result<ExitProbeResult, String> {
    let (skirk, config, socks) = runtime.exit_probe_command()?;
    tauri::async_runtime::spawn_blocking(move || run_exit_probe_sidecar(skirk, config, socks))
        .await
        .map_err(|_| "The exit test stopped unexpectedly".to_string())?
}

pub fn run() {
    tauri::Builder::default()
        .setup(|app| {
            let paths = AppPaths::resolve(&app.handle())?;
            let resource_dir = app.path().resource_dir().ok();
            app.manage(runtime_with_stale_proxy_cleanup(paths, resource_dir));
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            load_snapshot,
            import_config,
            replace_profile_config,
            export_saved_profile,
            delete_profile,
            select_profile,
            set_connection_mode,
            update_profile_performance,
            update_profile_route,
            connect,
            disconnect,
            probe_exit
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

fn runtime_with_stale_proxy_cleanup(
    paths: AppPaths,
    resource_dir: Option<PathBuf>,
) -> DesktopRuntime {
    let restore_error = SystemProxyManager::cleanup_stale_proxy(&paths).err();
    let runtime = DesktopRuntime::new(paths, resource_dir);
    if let Some(error) = restore_error {
        runtime.mark_proxy_restore_error(&error);
    }
    runtime
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
            ensure_private_dir(dir)?;
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

fn ensure_private_dir(path: &Path) -> Result<(), String> {
    fs::create_dir_all(path)
        .map_err(|error| format!("failed to create {}: {error}", path.display()))?;
    #[cfg(unix)]
    fs::set_permissions(path, fs::Permissions::from_mode(0o700))
        .map_err(|error| format!("failed to secure {}: {error}", path.display()))?;
    Ok(())
}

fn write_private_file(path: &Path, content: &[u8]) -> Result<(), String> {
    let name = path
        .file_name()
        .ok_or_else(|| format!("invalid private file path: {}", path.display()))?;
    let temp_path = path.with_file_name(format!(
        ".{}.tmp-{}-{}",
        name.to_string_lossy(),
        std::process::id(),
        NEXT_PRIVATE_WRITE.fetch_add(1, Ordering::Relaxed)
    ));
    let mut options = OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    options.mode(0o600);
    let result = (|| {
        let mut file = options
            .open(&temp_path)
            .map_err(|error| format!("failed to create {}: {error}", temp_path.display()))?;
        file.write_all(content)
            .map_err(|error| format!("failed to write {}: {error}", temp_path.display()))?;
        file.sync_all()
            .map_err(|error| format!("failed to sync {}: {error}", temp_path.display()))?;
        drop(file);
        fs::rename(&temp_path, path)
            .map_err(|error| format!("failed to replace {}: {error}", path.display()))
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temp_path);
    }
    result
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
    write_private_file(path, content.as_bytes())
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
    if limit == 0 {
        return String::new();
    }
    let Ok(mut file) = fs::File::open(path) else {
        return String::new();
    };
    let start = file
        .metadata()
        .ok()
        .map(|metadata| metadata.len().saturating_sub(LOG_TAIL_MAX_BYTES))
        .unwrap_or(0);
    if file.seek(SeekFrom::Start(start)).is_err() {
        return String::new();
    };
    let mut bytes = Vec::new();
    if file.read_to_end(&mut bytes).is_err() {
        return String::new();
    }
    let content = String::from_utf8_lossy(&bytes);
    let content = if start > 0 {
        content
            .find('\n')
            .map(|idx| &content[idx + 1..])
            .unwrap_or("")
    } else {
        content.as_ref()
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

    #[cfg(target_os = "macos")]
    #[test]
    fn stale_mac_proxy_restore_failure_is_visible_and_retryable() {
        let dir = std::env::temp_dir().join(format!(
            "skirk-proxy-restore-error-{}-{}",
            std::process::id(),
            epoch_millis()
        ));
        fs::create_dir_all(&dir).expect("create runtime directory");
        let backup = dir.join("macos-system-proxy-backup.json");
        let saved = json!({
            "service": format!("Skirk Missing Service {}", epoch_millis()),
            "web": {"enabled": false, "server": "", "port": 0, "authenticated": false},
            "secure_web": {"enabled": false, "server": "", "port": 0, "authenticated": false},
            "expected_port": 18081
        });
        write_private_file(&backup, saved.to_string().as_bytes()).expect("save test backup");
        let paths = AppPaths {
            config_dir: dir.clone(),
            runtime_dir: dir.clone(),
            logs_dir: dir.clone(),
            profiles_file: dir.join("profiles.json"),
            settings_file: dir.join("settings.json"),
        };
        let runtime = runtime_with_stale_proxy_cleanup(paths, None);
        let snapshot = runtime.snapshot().expect("startup snapshot");
        assert!(matches!(snapshot.connection.phase, ConnectionPhase::Error));
        assert!(snapshot
            .connection
            .message
            .contains("system proxy may still point"));
        assert!(snapshot.connection.system_proxy_restore_pending);
        assert!(backup.exists(), "failed restoration must keep its backup");
        runtime.clear_idle_connection_error().unwrap();
        assert!(matches!(
            runtime.snapshot().unwrap().connection.phase,
            ConnectionPhase::Error
        ));

        let error = runtime
            .disconnect()
            .expect_err("retry should still report failure");
        assert!(error.contains("Retry proxy restore"));
        assert!(backup.exists(), "retry must retain the backup");
        assert!(
            runtime
                .snapshot()
                .unwrap()
                .connection
                .system_proxy_restore_pending
        );
        fs::remove_dir_all(dir).expect("remove test runtime directory");
    }

    #[cfg(unix)]
    #[test]
    fn pasted_profile_reaches_only_the_decoder_stdin() {
        let dir = std::env::temp_dir().join(format!(
            "skirk-test-decoder-{}-{}",
            std::process::id(),
            epoch_millis()
        ));
        ensure_private_dir(&dir).unwrap();
        let decoder = dir.join("decoder");
        fs::write(&decoder, r#"#!/bin/sh
test "$#" = 5 && test "$*" = 'config decode --stdin --out -' || exit 1
test "$(cat)" = 'synthetic-profile' || exit 2
printf '%s' '{"decoded":true}'
"#).unwrap();
        fs::set_permissions(&decoder, fs::Permissions::from_mode(0o700)).unwrap();
        assert_eq!(
            decode_profile_with_sidecar(&decoder, "synthetic-profile").unwrap(),
            json!({"decoded": true})
        );
        let private_input = "malformed-private-input";
        let error = decode_profile_with_sidecar(&decoder, private_input).unwrap_err();
        assert!(!error.contains(private_input));
        assert_eq!(fs::read_dir(&dir).unwrap().count(), 1);
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn sharing_reads_the_requested_profile_only_from_its_config_directory() {
        let dir = std::env::temp_dir().join(format!(
            "skirk-test-share-{}-{}",
            std::process::id(),
            epoch_millis()
        ));
        let config_dir = dir.join("config");
        ensure_private_dir(&config_dir).unwrap();
        let paths = AppPaths {
            config_dir: config_dir.clone(),
            runtime_dir: dir.join("runtime"),
            logs_dir: dir.join("logs"),
            profiles_file: config_dir.join("profiles.json"),
            settings_file: config_dir.join("settings.json"),
        };
        let mut profile = test_profile(false);
        let config = config_dir.join("test.skirk");
        let text = format!("skirk:{}", "synthetic-profile");
        write_private_file(&config, format!("{text}\n").as_bytes()).unwrap();
        profile.config_path = config.display().to_string();
        save_profiles(&paths, &[profile.clone()]).unwrap();
        let runtime = DesktopRuntime::new(paths.clone(), None);
        assert_eq!(runtime.export_saved_profile(&profile.id).unwrap(), text);
        assert!(runtime.export_saved_profile("unknown").is_err());

        let outside = dir.join("outside.skirk");
        write_private_file(&outside, text.as_bytes()).unwrap();
        profile.config_path = outside.display().to_string();
        save_profiles(&paths, &[profile.clone()]).unwrap();
        assert!(runtime.export_saved_profile(&profile.id).is_err());
        fs::remove_dir_all(dir).unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn profile_storage_is_private_on_unix() {
        let dir = std::env::temp_dir().join(format!(
            "skirk-test-private-{}-{}",
            std::process::id(),
            epoch_millis()
        ));
        fs::create_dir_all(&dir).expect("create directory");
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o755))
            .expect("set broad permissions");
        ensure_private_dir(&dir).expect("secure directory");
        assert_eq!(
            fs::metadata(&dir)
                .expect("directory metadata")
                .permissions()
                .mode()
                & 0o777,
            0o700
        );

        let profile_path = dir.join("profile.skirk");
        fs::write(&profile_path, "old").expect("create profile");
        fs::set_permissions(&profile_path, fs::Permissions::from_mode(0o644))
            .expect("set broad permissions");
        write_private_file(&profile_path, b"secret").expect("secure and update profile");
        assert_eq!(
            fs::metadata(&profile_path)
                .expect("profile metadata")
                .permissions()
                .mode()
                & 0o777,
            0o600
        );
        assert_eq!(fs::read(&profile_path).expect("read profile"), b"secret");
        fs::remove_dir_all(dir).expect("remove directory");
    }

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
    fn process_start_error_does_not_dump_log_tail() {
        let dir = std::env::temp_dir().join(format!(
            "skirk-test-process-{}-{}",
            std::process::id(),
            epoch_millis()
        ));
        fs::create_dir_all(&dir).expect("tempdir");
        let log_path = dir.join("skirk-tunnel.log");
        fs::write(
            &log_path,
            "old line\nfirst detailed line\nlast detailed line with lots of context\n",
        )
        .expect("write log");

        let message = process_start_error(&log_path, "process exited early with exit status 1");

        assert!(message.contains("Open Logs for full sidecar details"));
        assert!(message.contains("Last log: last detailed line"));
        assert!(!message.contains("old line"));
        assert!(!message.contains("first detailed line"));
        let _ = fs::remove_dir_all(dir);
    }

    #[test]
    fn read_log_tail_bounds_large_files() {
        let dir = std::env::temp_dir().join(format!(
            "skirk-test-tail-{}-{}",
            std::process::id(),
            epoch_millis()
        ));
        fs::create_dir_all(&dir).expect("tempdir");
        let log_path = dir.join("skirk-client.log");
        let mut content = String::new();
        content.push_str("old-prefix-line\n");
        content.push_str(&"x".repeat((LOG_TAIL_MAX_BYTES as usize) + 4096));
        content.push('\n');
        content.push_str("tail-1\n");
        content.push_str("tail-2\n");
        content.push_str("tail-3\n");
        fs::write(&log_path, content).expect("write log");

        let tail = read_log_tail(&log_path, 2);

        assert_eq!(tail, "tail-2\ntail-3");
        assert!(!tail.contains("old-prefix-line"));
        let _ = fs::remove_dir_all(dir);
    }

    #[test]
    fn tunnel_preview_routes_dns_and_datagrams_only_through_skirk() {
        for ipv6 in [false, true] {
            let options = TunnelPreviewOptions::from_config(&json!({"tunnel": {
                "udp_preview_public_ipv4": true, "udp_preview_remote_dns": true,
                "udp_preview_stable_udp": true, "udp_preview_ipv6": ipv6
            }})).unwrap();
            let config = tunnel_config_with_preview(18080, "/app/skirk", "216.239.38.120", options);
            assert_eq!(config["dns"]["servers"].as_array().unwrap().len(), 1);
            assert_eq!(config["dns"]["servers"][0]["type"], "tcp");
            assert_eq!(config["dns"]["servers"][0]["server"], "198.18.0.2");
            assert_eq!(config["dns"]["servers"][0]["detour"], "proxy");
            assert_eq!(config["route"]["default_domain_resolver"], "exit-dns");
            assert_eq!(config["route"]["final"], "proxy");
            assert_eq!(config["inbounds"][0]["mtu"], 1280);
            assert_eq!(config["inbounds"][0]["address"].as_array().unwrap().len(), 2);
            let udp_rejects: Vec<_> = config["route"]["rules"].as_array().unwrap().iter()
                .filter(|rule| rule["network"] == "udp" && rule["action"] == "reject").collect();
            assert_eq!(udp_rejects.len(), if ipv6 { 0 } else { 1 });
            if !ipv6 { assert_eq!(udp_rejects[0]["ip_version"], 6); }
            assert!(config["outbounds"][0].get("udp_over_tcp").is_none());
            // The pre-existing control-plane escape stays narrowly pinned.
            assert_eq!(config["route"]["rules"][0]["ip_cidr"][0], "216.239.38.120/32");
            assert_eq!(config["route"]["rules"][0]["port"], 443);
            assert_eq!(config["route"]["rules"][1]["rules"][1]["process_path"][0], "/app/skirk");
        }
        assert!(!TunnelPreviewOptions::from_config(&json!({})).unwrap().public_udp);
        for invalid in [
            json!({"tunnel":{"udp_preview_public_ipv4":"true"}}),
            json!({"tunnel":{"udp_preview_remote_dns":true}}),
            json!({"tunnel":{"udp_preview_public_ipv4":true,"udp_preview_ipv6":true}}),
        ] {
            assert!(TunnelPreviewOptions::from_config(&invalid).is_err());
        }
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

    #[test]
    fn route_choice_preserves_imported_mode_and_survives_reload() {
        let dir = std::env::temp_dir().join(format!(
            "skirk-route-choice-{}-{}",
            std::process::id(),
            epoch_millis()
        ));
        fs::create_dir_all(&dir).expect("temp route directory");
        let paths = AppPaths {
            config_dir: dir.clone(),
            runtime_dir: dir.join("runtime"),
            logs_dir: dir.join("logs"),
            profiles_file: dir.join("profiles.json"),
            settings_file: dir.join("settings.json"),
        };
        let mut imported = test_profile(false);
        imported.route_mode = "google_front".into();
        save_profiles(&paths, &[imported]).expect("save imported profile");
        let runtime = DesktopRuntime::new(paths.clone(), None);
        assert_eq!(
            load_profiles(&paths).expect("load imported")[0].route_mode,
            "google_front"
        );

        runtime
            .update_profile_route("profile-test", "direct")
            .expect("select direct");
        assert_eq!(
            load_profiles(&paths).expect("reload direct")[0].route_mode,
            "direct"
        );
        runtime
            .update_profile_route("profile-test", "google_front_pinned")
            .expect("select Google front");
        assert_eq!(
            load_profiles(&paths).expect("reload Google front")[0].route_mode,
            "google_front_pinned"
        );
        assert!(runtime
            .update_profile_route("profile-test", "unexpected")
            .is_err());
        assert_eq!(
            load_profiles(&paths).expect("reload after invalid selection")[0].route_mode,
            "google_front_pinned"
        );
        fs::remove_dir_all(dir).expect("remove route test directory");
    }

    #[test]
    fn new_import_uses_google_front_even_if_kit_route_is_direct() {
        let dir = std::env::temp_dir().join(format!(
            "skirk-import-front-{}-{}",
            std::process::id(),
            epoch_millis()
        ));
        fs::create_dir_all(&dir).expect("temp profile directory");
        let paths = AppPaths {
            config_dir: dir.clone(),
            runtime_dir: dir.join("runtime"),
            logs_dir: dir.join("logs"),
            profiles_file: dir.join("profiles.json"),
            settings_file: dir.join("settings.json"),
        };
        let runtime = DesktopRuntime::new(paths.clone(), None);
        let config = json!({
            "secret": "test-secret",
            "session_id": "test-session",
            "auth": {"client_id": "test-client"},
            "drive": {"folder_id": "test-mailbox"},
            "route": {"mode": "direct", "google_ip": "216.239.38.120"}
        });
        runtime
            .import_config("Imported".into(), config.to_string(), 18080, 18081, false)
            .expect("import old direct kit");
        let saved = load_profiles(&paths).expect("load profiles");
        assert_eq!(saved.len(), 1);
        assert_eq!(saved[0].route_mode, "google_front_pinned");
        runtime
            .update_profile_route(&saved[0].id, "direct")
            .expect("explicit user choice");
        assert_eq!(
            load_profiles(&paths).expect("reload")[0].route_mode,
            "direct"
        );
        fs::remove_dir_all(dir).expect("remove profile directory");
    }

    #[test]
    fn snapshot_falls_back_without_rewriting_profile_selection() {
        let dir = std::env::temp_dir().join(format!(
            "skirk-snapshot-selection-{}-{}",
            std::process::id(),
            epoch_millis()
        ));
        fs::create_dir_all(&dir).expect("temp profile directory");
        let paths = AppPaths {
            config_dir: dir.clone(),
            runtime_dir: dir.join("runtime"),
            logs_dir: dir.join("logs"),
            profiles_file: dir.join("profiles.json"),
            settings_file: dir.join("settings.json"),
        };
        save_profiles(&paths, &[test_profile(false)]).expect("save profile");
        save_settings(
            &paths,
            &Settings {
                selected_profile_id: Some("removed-profile".into()),
                ..Settings::default()
            },
        )
        .expect("save stale selection");
        let before = fs::read(&paths.settings_file).expect("settings before snapshot");
        let snapshot = DesktopRuntime::new(paths.clone(), None)
            .snapshot()
            .expect("snapshot with stale selection");
        assert_eq!(
            snapshot.selected_profile_id.as_deref(),
            Some("profile-test")
        );
        assert_eq!(
            fs::read(&paths.settings_file).expect("settings after snapshot"),
            before
        );
        fs::remove_dir_all(dir).expect("remove profile directory");
    }

    #[test]
    fn profile_changes_are_rejected_during_connection_startup() {
        let dir = std::env::temp_dir().join(format!(
            "skirk-startup-guard-{}-{}",
            std::process::id(),
            epoch_millis()
        ));
        fs::create_dir_all(&dir).expect("temp profile directory");
        let paths = AppPaths {
            config_dir: dir.clone(),
            runtime_dir: dir.join("runtime"),
            logs_dir: dir.join("logs"),
            profiles_file: dir.join("profiles.json"),
            settings_file: dir.join("settings.json"),
        };
        save_profiles(&paths, &[test_profile(false)]).expect("save profile");
        let runtime = DesktopRuntime::new(paths.clone(), None);
        runtime.connect_in_progress.store(true, Ordering::Release);

        assert!(runtime
            .import_config("Other".into(), "{}".into(), 18080, 18081, false)
            .is_err());
        assert!(runtime.delete_profile("profile-test").is_err());
        assert!(runtime
            .update_profile_route("profile-test", "direct")
            .is_err());
        assert!(runtime.select_profile(None).is_err());
        assert!(runtime.set_connection_mode(ConnectionMode::Proxy).is_err());
        assert_eq!(load_profiles(&paths).expect("unchanged profiles").len(), 1);
        fs::remove_dir_all(dir).expect("remove profile directory");
    }

    #[test]
    fn replacing_oauth_config_keeps_tunnel_identity_and_profile_settings() {
        let dir = std::env::temp_dir().join(format!(
            "skirk-replace-profile-{}-{}",
            std::process::id(),
            epoch_millis()
        ));
        fs::create_dir_all(&dir).expect("temp profile directory");
        let paths = AppPaths {
            config_dir: dir.clone(),
            runtime_dir: dir.join("runtime"),
            logs_dir: dir.join("logs"),
            profiles_file: dir.join("profiles.json"),
            settings_file: dir.join("settings.json"),
        };
        let config_path = dir.join("client.json");
        let original = json!({
            "secret": "same-tunnel-secret",
            "session_id": "same-session",
            "auth": {"client_id": "same-oauth-client", "refresh_token": "old-grant"},
            "drive": {"space": "drive", "folder_id": "mailbox-one"},
            "route": {"google_ip": "216.239.38.120"}
        });
        fs::write(&config_path, original.to_string()).expect("old config");
        let mut profile = test_profile(false);
        profile.config_path = config_path.display().to_string();
        profile.socks_port = 21000;
        profile.http_port = 21001;
        profile.route_mode = "direct".into();
        profile.performance = performance_for_preset(PerformancePreset::Responsive);
        profile.drive_space = "drive".into();
        profile.drive_folder_id = "mailbox-one".into();
        save_profiles(&paths, &[profile]).expect("save profile");
        let runtime = DesktopRuntime::new(paths.clone(), None);
        let renewed = json!({
            "secret": "same-tunnel-secret",
            "session_id": "same-session",
            "auth": {"client_id": "same-oauth-client", "refresh_token": "new-grant"},
            "drive": {"space": "drive", "folder_id": "mailbox-one"},
            "route": {"google_ip": "8.8.8.8"},
            "tunnel": {"transport": "muxv5"}
        });
        let mut wrong = renewed.clone();
        *wrong.pointer_mut("/drive/folder_id").expect("folder field") = json!("other-mailbox");
        assert!(runtime
            .replace_profile_config("profile-test", wrong.to_string())
            .is_err());
        assert_eq!(
            fs::read_to_string(&config_path).expect("unchanged config"),
            original.to_string()
        );

        for field in ["/session_id", "/secret", "/auth/client_id"] {
            let mut wrong = renewed.clone();
            *wrong.pointer_mut(field).expect("identity field") = json!("different");
            assert!(runtime
                .replace_profile_config("profile-test", wrong.to_string())
                .is_err());
            assert_eq!(
                fs::read_to_string(&config_path).expect("unchanged after identity mismatch"),
                original.to_string()
            );
        }

        for transport in ["muxv4", "muxv6"] {
            let mut wrong = renewed.clone();
            wrong["tunnel"]["transport"] = json!(transport);
            let error = runtime
                .replace_profile_config("profile-test", wrong.to_string())
                .expect_err("transport change must not replace a profile");
            assert!(error.contains("different Skirk transport version"));
            assert_eq!(
                fs::read_to_string(&config_path).expect("unchanged config"),
                original.to_string()
            );
        }
        for tunnel in [json!(17), json!({"transport": 17})] {
            let mut wrong = renewed.clone();
            wrong["tunnel"] = tunnel;
            assert!(runtime
                .replace_profile_config("profile-test", wrong.to_string())
                .is_err());
            assert_eq!(
                fs::read_to_string(&config_path).expect("unchanged config"),
                original.to_string()
            );
        }
        let mut legacy_v4 = original.clone();
        legacy_v4["tunnel"] = json!({"transport": "muxv4"});
        fs::write(&config_path, legacy_v4.to_string()).expect("v4 config");
        let reverse_error = runtime
            .replace_profile_config("profile-test", renewed.to_string())
            .expect_err("v4 to v5 renewal must be rejected");
        assert!(reverse_error.contains("different Skirk transport version"));
        assert_eq!(
            fs::read_to_string(&config_path).expect("unchanged v4 config"),
            legacy_v4.to_string()
        );
        fs::write(&config_path, original.to_string()).expect("restore original config");

        runtime
            .replace_profile_config("profile-test", renewed.to_string())
            .expect("replace OAuth config");
        let saved = load_profiles(&paths).expect("load replaced profile");
        assert_eq!(saved.len(), 1);
        assert_eq!(saved[0].id, "profile-test");
        assert_eq!(saved[0].socks_port, 21000);
        assert_eq!(saved[0].http_port, 21001);
        assert_eq!(saved[0].route_mode, "direct");
        assert_eq!(saved[0].performance.preset, PerformancePreset::Responsive);
        assert_eq!(saved[0].google_ip, "8.8.8.8");
        assert_eq!(
            fs::read_to_string(&config_path).expect("renewed config"),
            renewed.to_string()
        );
        fs::remove_dir_all(dir).expect("remove profile directory");
    }

    #[test]
    fn new_profile_selection_clears_previous_google_grant_error() {
        let dir = std::env::temp_dir().join(format!(
            "skirk-profile-error-reset-{}-{}",
            std::process::id(),
            epoch_millis()
        ));
        fs::create_dir_all(&dir).expect("temp profile directory");
        let paths = AppPaths {
            config_dir: dir.clone(),
            runtime_dir: dir.join("runtime"),
            logs_dir: dir.join("logs"),
            profiles_file: dir.join("profiles.json"),
            settings_file: dir.join("settings.json"),
        };
        let mut other = test_profile(false);
        other.id = "profile-other".into();
        save_profiles(&paths, &[test_profile(false), other]).expect("save profiles");
        let runtime = DesktopRuntime::new(paths.clone(), None);
        {
            let mut state = runtime.inner.lock().expect("runtime lock");
            state.phase = ConnectionPhase::Error;
            state.message = "Google token refresh failed: invalid_grant".into();
        }
        runtime
            .select_profile(Some("profile-other".into()))
            .expect("select different profile");
        let snapshot = runtime.snapshot().expect("snapshot after selection");
        assert!(matches!(
            snapshot.connection.phase,
            ConnectionPhase::Disconnected
        ));
        assert!(!snapshot.connection.message.contains("invalid_grant"));

        {
            let mut state = runtime.inner.lock().expect("runtime lock");
            state.phase = ConnectionPhase::Error;
            state.message = "Google token refresh failed: invalid_grant".into();
        }
        let config = json!({
            "secret": "new-test-secret",
            "session_id": "new-test-session",
            "auth": {"client_id": "new-test-client"},
            "drive": {"space": "appDataFolder"},
            "route": {"mode": "direct", "google_ip": "216.239.38.120"}
        });
        runtime
            .import_config(
                "New profile".into(),
                config.to_string(),
                18100,
                18101,
                false,
            )
            .expect("import new profile");
        let snapshot = runtime.snapshot().expect("snapshot after import");
        assert!(matches!(
            snapshot.connection.phase,
            ConnectionPhase::Disconnected
        ));
        assert!(!snapshot.connection.message.contains("invalid_grant"));
        assert_eq!(snapshot.profiles.len(), 3);
        fs::remove_dir_all(dir).expect("remove profile directory");
    }

    #[test]
    fn terminal_oauth_exit_shows_profile_recovery() {
        let message = client_exit_message(Some(78), "exit status: 78");
        assert!(message.contains("invalid_grant"));
        assert!(message.contains("replace this profile"));
        assert_eq!(
            client_exit_message(Some(1), "exit status: 1"),
            "Skirk exited: exit status: 1"
        );
    }

    #[test]
    fn snapshot_refresh_keeps_startup_phase_without_client_handle() {
        let paths = AppPaths {
            config_dir: PathBuf::new(),
            runtime_dir: PathBuf::new(),
            logs_dir: PathBuf::new(),
            profiles_file: PathBuf::new(),
            settings_file: PathBuf::new(),
        };
        let mut state = RuntimeState {
            phase: ConnectionPhase::Connecting,
            message: "Starting Skirk sidecar".into(),
            ..RuntimeState::default()
        };
        refresh_state(&mut state, &paths);
        assert!(matches!(state.phase, ConnectionPhase::Connecting));
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
