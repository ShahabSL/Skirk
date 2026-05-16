#[cfg(windows)]
use serde::{Deserialize, Serialize};

use crate::AppPaths;

#[cfg(windows)]
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct SystemProxyBackup {
    proxy_enable: Option<u32>,
    proxy_server: Option<String>,
    proxy_override: Option<String>,
    auto_config_url: Option<String>,
    auto_detect: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    skirk_proxy_server: Option<String>,
}

pub struct SystemProxyManager;

impl SystemProxyManager {
    pub fn enable(paths: &AppPaths, http_port: u16) -> Result<(), String> {
        if !cfg!(windows) {
            return Err("system proxy mode is only available on Windows".into());
        }
        windows_impl::enable(paths, http_port)
    }

    pub fn disable(paths: &AppPaths) -> Result<(), String> {
        if !cfg!(windows) {
            return Ok(());
        }
        windows_impl::disable(paths)
    }

    pub fn cleanup_stale_proxy(paths: &AppPaths) -> Result<(), String> {
        if !cfg!(windows) {
            return Ok(());
        }
        windows_impl::cleanup_stale_proxy(paths)
    }
}

#[cfg(windows)]
fn backup_path(paths: &AppPaths) -> std::path::PathBuf {
    paths.runtime_dir.join("system-proxy-backup.json")
}

#[cfg(windows)]
mod windows_impl {
    use std::fs;

    use windows_sys::Win32::Networking::WinInet::{
        InternetSetOptionW, INTERNET_OPTION_REFRESH, INTERNET_OPTION_SETTINGS_CHANGED,
    };
    use winreg::{enums::*, RegKey};

    use super::{backup_path, AppPaths, SystemProxyBackup};

    const INTERNET_SETTINGS_KEY: &str =
        r"Software\Microsoft\Windows\CurrentVersion\Internet Settings";

    pub fn enable(paths: &AppPaths, http_port: u16) -> Result<(), String> {
        let proxy_server = format!("http=127.0.0.1:{http_port};https=127.0.0.1:{http_port}");
        if !backup_path(paths).exists() {
            let mut snapshot = read_snapshot()?;
            snapshot.skirk_proxy_server = Some(proxy_server.clone());
            write_backup(paths, &snapshot)?;
        }

        if let Err(error) = apply_enable(&proxy_server) {
            let rollback = disable_inner(paths, true).err();
            return Err(match rollback {
                Some(rollback) => {
                    format!("failed to enable Windows system proxy: {error}; rollback failed: {rollback}")
                }
                None => format!("failed to enable Windows system proxy: {error}; previous proxy settings were restored"),
            });
        }
        Ok(())
    }

    fn apply_enable(proxy_server: &String) -> Result<(), String> {
        let hkcu = RegKey::predef(HKEY_CURRENT_USER);
        let (key, _) = hkcu
            .create_subkey(INTERNET_SETTINGS_KEY)
            .map_err(|error| format!("failed to open Windows internet settings: {error}"))?;
        key.set_value("ProxyEnable", &1u32)
            .map_err(|error| format!("failed to enable Windows proxy: {error}"))?;
        key.set_value("ProxyServer", proxy_server)
            .map_err(|error| format!("failed to set Windows proxy endpoint: {error}"))?;
        key.set_value(
            "ProxyOverride",
            &"<local>;localhost;127.*;10.*;172.16.*;172.17.*;172.18.*;172.19.*;172.20.*;172.21.*;172.22.*;172.23.*;172.24.*;172.25.*;172.26.*;172.27.*;172.28.*;172.29.*;172.30.*;172.31.*;192.168.*",
        )
        .map_err(|error| format!("failed to set Windows proxy bypass list: {error}"))?;
        key.set_value("AutoDetect", &0u32)
            .map_err(|error| format!("failed to disable Windows proxy auto-detect: {error}"))?;
        let _ = key.delete_value("AutoConfigURL");
        refresh();
        Ok(())
    }

    pub fn disable(paths: &AppPaths) -> Result<(), String> {
        disable_inner(paths, false)
    }

    fn disable_inner(paths: &AppPaths, force: bool) -> Result<(), String> {
        let backup = read_backup(paths)?;
        let hkcu = RegKey::predef(HKEY_CURRENT_USER);
        let (key, _) = hkcu
            .create_subkey(INTERNET_SETTINGS_KEY)
            .map_err(|error| format!("failed to open Windows internet settings: {error}"))?;

        if let Some(snapshot) = backup {
            if !force && !current_is_skirk_proxy(&snapshot)? {
                let _ = fs::remove_file(backup_path(paths));
                return Ok(());
            }
            write_or_delete_u32(&key, "ProxyEnable", snapshot.proxy_enable)?;
            write_or_delete_string(&key, "ProxyServer", snapshot.proxy_server)?;
            write_or_delete_string(&key, "ProxyOverride", snapshot.proxy_override)?;
            write_or_delete_string(&key, "AutoConfigURL", snapshot.auto_config_url)?;
            write_or_delete_u32(&key, "AutoDetect", snapshot.auto_detect)?;
            let _ = fs::remove_file(backup_path(paths));
        } else {
            write_or_delete_u32(&key, "ProxyEnable", Some(0))?;
            write_or_delete_string(&key, "ProxyServer", None)?;
        }
        refresh();
        Ok(())
    }

    pub fn cleanup_stale_proxy(paths: &AppPaths) -> Result<(), String> {
        if backup_path(paths).exists() {
            disable(paths)?;
        }
        Ok(())
    }

    fn read_snapshot() -> Result<SystemProxyBackup, String> {
        let hkcu = RegKey::predef(HKEY_CURRENT_USER);
        let key = hkcu
            .open_subkey(INTERNET_SETTINGS_KEY)
            .map_err(|error| format!("failed to read Windows internet settings: {error}"))?;
        Ok(SystemProxyBackup {
            proxy_enable: key.get_value("ProxyEnable").ok(),
            proxy_server: key.get_value("ProxyServer").ok(),
            proxy_override: key.get_value("ProxyOverride").ok(),
            auto_config_url: key.get_value("AutoConfigURL").ok(),
            auto_detect: key.get_value("AutoDetect").ok(),
            skirk_proxy_server: None,
        })
    }

    fn current_is_skirk_proxy(snapshot: &SystemProxyBackup) -> Result<bool, String> {
        let current = read_snapshot()?;
        if current.proxy_enable != Some(1) {
            return Ok(false);
        }
        match (
            &snapshot.skirk_proxy_server,
            current.proxy_server.as_deref(),
        ) {
            (Some(expected), Some(actual)) => Ok(actual == expected),
            (None, Some(actual)) => Ok(actual.contains("127.0.0.1:")),
            _ => Ok(false),
        }
    }

    fn write_backup(paths: &AppPaths, snapshot: &SystemProxyBackup) -> Result<(), String> {
        let content = serde_json::to_string_pretty(snapshot)
            .map_err(|error| format!("failed to serialize proxy backup: {error}"))?;
        fs::write(backup_path(paths), content)
            .map_err(|error| format!("failed to write proxy backup: {error}"))
    }

    fn read_backup(paths: &AppPaths) -> Result<Option<SystemProxyBackup>, String> {
        let path = backup_path(paths);
        if !path.exists() {
            return Ok(None);
        }
        let content = fs::read_to_string(path)
            .map_err(|error| format!("failed to read proxy backup: {error}"))?;
        let backup = serde_json::from_str::<SystemProxyBackup>(&content)
            .map_err(|error| format!("failed to parse proxy backup: {error}"))?;
        Ok(Some(backup))
    }

    fn write_or_delete_u32(key: &RegKey, name: &str, value: Option<u32>) -> Result<(), String> {
        match value {
            Some(value) => key
                .set_value(name, &value)
                .map_err(|error| format!("failed to set {name}: {error}")),
            None => delete_value(key, name),
        }
    }

    fn write_or_delete_string(
        key: &RegKey,
        name: &str,
        value: Option<String>,
    ) -> Result<(), String> {
        match value {
            Some(value) => key
                .set_value(name, &value)
                .map_err(|error| format!("failed to set {name}: {error}")),
            None => delete_value(key, name),
        }
    }

    fn delete_value(key: &RegKey, name: &str) -> Result<(), String> {
        match key.delete_value(name) {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(format!("failed to delete {name}: {error}")),
        }
    }

    fn refresh() {
        unsafe {
            let _ = InternetSetOptionW(
                std::ptr::null(),
                INTERNET_OPTION_SETTINGS_CHANGED,
                std::ptr::null_mut(),
                0,
            );
            let _ = InternetSetOptionW(
                std::ptr::null(),
                INTERNET_OPTION_REFRESH,
                std::ptr::null_mut(),
                0,
            );
        }
    }
}

#[cfg(not(windows))]
mod windows_impl {
    use super::AppPaths;

    pub fn enable(_paths: &AppPaths, _http_port: u16) -> Result<(), String> {
        Err("system proxy mode is only available on Windows".into())
    }

    pub fn disable(_paths: &AppPaths) -> Result<(), String> {
        Ok(())
    }

    pub fn cleanup_stale_proxy(_paths: &AppPaths) -> Result<(), String> {
        Ok(())
    }
}
