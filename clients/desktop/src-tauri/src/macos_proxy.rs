use serde::{Deserialize, Serialize};
use std::{fs, process::Command};

use crate::{write_private_file, AppPaths};

const NETWORKSETUP: &str = "/usr/sbin/networksetup";
const ROUTE: &str = "/sbin/route";
const LOCAL_PROXY_HOST: &str = "127.0.0.1";

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
struct ProxyState {
    enabled: bool,
    server: String,
    port: u16,
    authenticated: bool,
}

#[derive(Debug, Deserialize, Serialize)]
struct Backup {
    service: String,
    web: ProxyState,
    secure_web: ProxyState,
    expected_port: u16,
}

#[derive(Debug, Eq, PartialEq)]
struct RestorePlan {
    web: bool,
    secure_web: bool,
}

fn backup_path(paths: &AppPaths) -> std::path::PathBuf {
    paths.runtime_dir.join("macos-system-proxy-backup.json")
}

pub fn restore_pending(paths: &AppPaths) -> bool {
    backup_path(paths).exists()
}

fn run(program: &str, args: &[&str]) -> Result<String, String> {
    let output = Command::new(program).args(args).output().map_err(|error| {
        format!(
            "failed to run {}: {error}",
            program.rsplit('/').next().unwrap_or(program)
        )
    })?;
    if !output.status.success() {
        let detail = String::from_utf8_lossy(&output.stderr).trim().to_string();
        let detail = if detail.is_empty() {
            String::from_utf8_lossy(&output.stdout).trim().to_string()
        } else {
            detail
        };
        return Err(format!(
            "{} {} failed: {}",
            program.rsplit('/').next().unwrap_or(program),
            args.first().copied().unwrap_or(""),
            if detail.is_empty() {
                "unknown error"
            } else {
                &detail
            }
        ));
    }
    String::from_utf8(output.stdout)
        .map_err(|_| "macOS network settings output was not UTF-8".into())
}

fn networksetup(args: &[&str]) -> Result<String, String> {
    run(NETWORKSETUP, args)
}

fn active_service() -> Result<String, String> {
    let route = run(ROUTE, &["-n", "get", "default"])?;
    let order = networksetup(&["-listnetworkserviceorder"])?;
    parse_active_service(&route, &order).ok_or_else(|| {
        "could not match the active default network interface to a macOS network service".into()
    })
}

fn parse_active_service(route: &str, order: &str) -> Option<String> {
    let interface = route
        .lines()
        .find_map(|line| line.trim().strip_prefix("interface:").map(str::trim))?;
    let mut service = None;
    for line in order.lines().map(str::trim) {
        if line.starts_with('(') && !line.starts_with("(Hardware Port:") {
            let name = line.split_once(") ").map(|(_, name)| name.trim());
            service = name.filter(|name| !name.starts_with('*') && !name.is_empty());
        } else if let Some((_, device)) = line.rsplit_once("Device: ") {
            if device.trim_end_matches(')') == interface {
                return service.map(str::to_owned);
            }
        }
    }
    None
}

fn field<'a>(output: &'a str, key: &str) -> Option<&'a str> {
    output
        .lines()
        .find_map(|line| line.trim().strip_prefix(key).map(str::trim))
}

fn parse_on_off(value: &str) -> Option<bool> {
    match value.to_ascii_lowercase().as_str() {
        "yes" | "on" | "1" => Some(true),
        "no" | "off" | "0" => Some(false),
        _ => None,
    }
}

fn parse_proxy(output: &str) -> Result<ProxyState, String> {
    let enabled = field(output, "Enabled:")
        .and_then(parse_on_off)
        .ok_or_else(|| "could not read macOS proxy enabled state".to_string())?;
    let server = field(output, "Server:")
        .ok_or_else(|| "could not read macOS proxy server".to_string())?
        .to_owned();
    let port = field(output, "Port:")
        .and_then(|value| value.parse::<u16>().ok())
        .ok_or_else(|| "could not read macOS proxy port".to_string())?;
    let authenticated = field(output, "Authenticated Proxy Enabled:")
        .and_then(parse_on_off)
        .ok_or_else(|| "could not read macOS proxy authentication state".to_string())?;
    Ok(ProxyState {
        enabled,
        server,
        port,
        authenticated,
    })
}

fn read_proxy(service: &str, secure: bool) -> Result<ProxyState, String> {
    let flag = if secure {
        "-getsecurewebproxy"
    } else {
        "-getwebproxy"
    };
    parse_proxy(&networksetup(&[flag, service])?)
}

fn read_enabled(service: &str, flag: &str) -> Result<bool, String> {
    let output = networksetup(&[flag, service])?;
    let key = if flag == "-getproxyautodiscovery" {
        "Auto Proxy Discovery:"
    } else {
        "Enabled:"
    };
    field(&output, key)
        .and_then(parse_on_off)
        .ok_or_else(|| format!("could not read macOS {flag} state"))
}

fn ensure_no_conflicting_proxy(
    service: &str,
    web: &ProxyState,
    secure_web: &ProxyState,
) -> Result<(), String> {
    let socks = read_proxy_type(service, "-getsocksfirewallproxy")?;
    let pac = read_enabled(service, "-getautoproxyurl")?;
    let auto = read_enabled(service, "-getproxyautodiscovery")?;
    if web.enabled || secure_web.enabled || socks.enabled || pac || auto {
        return Err(format!(
            "{service} already has a proxy or automatic proxy configuration; choose local Proxy mode to keep it unchanged"
        ));
    }
    if web.authenticated || secure_web.authenticated {
        return Err(format!(
            "{service} has stored authenticated proxy settings that Skirk cannot safely restore"
        ));
    }
    Ok(())
}

fn read_proxy_type(service: &str, flag: &str) -> Result<ProxyState, String> {
    parse_proxy(&networksetup(&[flag, service])?)
}

fn apply_proxy(service: &str, state: &ProxyState, secure: bool) -> Result<(), String> {
    let set = if secure {
        "-setsecurewebproxy"
    } else {
        "-setwebproxy"
    };
    let set_state = if secure {
        "-setsecurewebproxystate"
    } else {
        "-setwebproxystate"
    };
    let port = state.port.to_string();
    networksetup(&[set, service, &state.server, &port])?;
    networksetup(&[set_state, service, if state.enabled { "on" } else { "off" }])?;
    Ok(())
}

fn expected(port: u16) -> ProxyState {
    ProxyState {
        enabled: true,
        server: LOCAL_PROXY_HOST.into(),
        port,
        authenticated: false,
    }
}

fn restore_plan(backup: &Backup, web: &ProxyState, secure_web: &ProxyState) -> RestorePlan {
    let owned = expected(backup.expected_port);
    RestorePlan {
        web: web.server == owned.server && web.port == owned.port && !web.authenticated,
        secure_web: secure_web.server == owned.server
            && secure_web.port == owned.port
            && !secure_web.authenticated,
    }
}

fn read_backup(paths: &AppPaths) -> Result<Option<Backup>, String> {
    let path = backup_path(paths);
    if !path.exists() {
        return Ok(None);
    }
    let data =
        fs::read(&path).map_err(|error| format!("failed to read macOS proxy backup: {error}"))?;
    serde_json::from_slice(&data)
        .map(Some)
        .map_err(|error| format!("failed to parse macOS proxy backup: {error}"))
}

fn write_backup(paths: &AppPaths, backup: &Backup) -> Result<(), String> {
    let data = serde_json::to_vec(backup)
        .map_err(|error| format!("failed to serialize macOS proxy backup: {error}"))?;
    write_private_file(&backup_path(paths), &data)
}

pub fn enable(paths: &AppPaths, http_port: u16) -> Result<(), String> {
    let service = active_service()?;
    enable_service(paths, service, http_port)
}

fn enable_service(paths: &AppPaths, service: String, http_port: u16) -> Result<(), String> {
    if backup_path(paths).exists() {
        disable(paths)?;
        if backup_path(paths).exists() {
            return Err(
                "could not restore the previous macOS system proxy; retry cleanup first".into(),
            );
        }
    }
    let web = read_proxy(&service, false)?;
    let secure_web = read_proxy(&service, true)?;
    ensure_no_conflicting_proxy(&service, &web, &secure_web)?;
    let backup = Backup {
        service,
        web,
        secure_web,
        expected_port: http_port,
    };
    write_backup(paths, &backup)?;

    // networksetup has no compare-and-swap operation. Recheck after persisting
    // the backup to avoid overwriting a proxy edit made during setup.
    let unchanged = (|| {
        let web = read_proxy(&backup.service, false)?;
        let secure_web = read_proxy(&backup.service, true)?;
        ensure_no_conflicting_proxy(&backup.service, &web, &secure_web)?;
        Ok::<bool, String>(web == backup.web && secure_web == backup.secure_web)
    })();
    if !matches!(unchanged, Ok(true)) {
        fs::remove_file(backup_path(paths))
            .map_err(|error| format!("failed to discard unused macOS proxy backup: {error}"))?;
        return Err(match unchanged {
            Ok(false) => {
                "macOS proxy settings changed during connection; retry or use local Proxy mode"
                    .into()
            }
            Err(error) => error,
            Ok(true) => unreachable!(),
        });
    }

    let result: Result<(), String> = (|| {
        let local = expected(http_port);
        apply_proxy(&backup.service, &local, false)?;
        apply_proxy(&backup.service, &local, true)?;
        if read_proxy(&backup.service, false)? != local
            || read_proxy(&backup.service, true)? != local
        {
            return Err("macOS did not retain the local system proxy settings".into());
        }
        Ok(())
    })();
    if let Err(error) = result {
        return Err(match disable(paths) {
            Ok(()) => {
                format!("failed to enable macOS system proxy: {error}; previous settings restored")
            }
            Err(rollback) => {
                format!("failed to enable macOS system proxy: {error}; rollback failed: {rollback}")
            }
        });
    }
    Ok(())
}

pub fn disable(paths: &AppPaths) -> Result<(), String> {
    let Some(backup) = read_backup(paths)? else {
        return Ok(());
    };
    let web = read_proxy(&backup.service, false)?;
    let secure_web = read_proxy(&backup.service, true)?;
    let plan = restore_plan(&backup, &web, &secure_web);
    if plan.web {
        apply_proxy(&backup.service, &backup.web, false)?;
    }
    if plan.secure_web {
        apply_proxy(&backup.service, &backup.secure_web, true)?;
    }
    fs::remove_file(backup_path(paths))
        .map_err(|error| format!("failed to remove macOS proxy backup: {error}"))
}

pub fn cleanup_stale_proxy(paths: &AppPaths) -> Result<(), String> {
    disable(paths)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn original() -> ProxyState {
        ProxyState {
            enabled: false,
            server: String::new(),
            port: 0,
            authenticated: false,
        }
    }

    #[test]
    fn selects_active_enabled_service_by_default_route_device() {
        let route = "destination: default\n  interface: en0\n";
        let services = "An asterisk (*) denotes that a network service is disabled.\n(1) *Ethernet\n(Hardware Port: Ethernet, Device: en9)\n(2) Wi-Fi\n(Hardware Port: Wi-Fi, Device: en0)\n(3) USB LAN\n(Hardware Port: USB LAN, Device: en7)\n";
        assert_eq!(
            parse_active_service(route, services).as_deref(),
            Some("Wi-Fi")
        );
        assert_eq!(parse_active_service("interface: utun2", services), None);
    }

    #[test]
    fn parses_proxy_and_rejects_missing_or_invalid_fields() {
        let output = "Enabled: No\nServer: \nPort: 0\nAuthenticated Proxy Enabled: 0\n";
        assert_eq!(parse_proxy(output).unwrap(), original());
        assert!(parse_proxy(
            "Enabled: Yes\nServer: 127.0.0.1\nPort: abc\nAuthenticated Proxy Enabled: 0"
        )
        .is_err());
        assert!(parse_proxy("Enabled: No\nServer: \nPort: 0").is_err());
    }

    #[test]
    fn restores_only_settings_still_owned_by_skirk() {
        let backup = Backup {
            service: "Wi-Fi".into(),
            web: original(),
            secure_web: original(),
            expected_port: 18081,
        };
        let owned = expected(18081);
        let changed = ProxyState {
            enabled: true,
            server: "other.proxy".into(),
            port: 8080,
            authenticated: false,
        };
        assert_eq!(
            restore_plan(&backup, &owned, &owned),
            RestorePlan {
                web: true,
                secure_web: true
            }
        );
        assert_eq!(
            restore_plan(&backup, &owned, &changed),
            RestorePlan {
                web: true,
                secure_web: false
            }
        );
        let mut partly_applied = owned.clone();
        partly_applied.enabled = false;
        assert_eq!(
            restore_plan(&backup, &partly_applied, &changed),
            RestorePlan {
                web: true,
                secure_web: false
            }
        );
        assert_eq!(
            restore_plan(&backup, &changed, &changed),
            RestorePlan {
                web: false,
                secure_web: false
            }
        );
    }

    #[test]
    #[ignore = "creates and removes a temporary macOS network service"]
    fn live_duplicate_service_restores_and_preserves_external_changes() {
        struct ServiceGuard(String);
        impl Drop for ServiceGuard {
            fn drop(&mut self) {
                let _ = networksetup(&["-removenetworkservice", &self.0]);
            }
        }

        let source = active_service().expect("active network service");
        let nonce = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let service = format!("Skirk Proxy QA {} {nonce}", std::process::id());
        networksetup(&["-duplicatenetworkservice", &source, &service])
            .expect("duplicate active service for isolated test");
        let _guard = ServiceGuard(service.clone());
        let root = std::env::temp_dir().join(format!("skirk-mac-proxy-qa-{nonce}"));
        fs::create_dir_all(&root).expect("test runtime directory");
        let paths = AppPaths {
            profiles_file: root.join("profiles.json"),
            settings_file: root.join("settings.json"),
            config_dir: root.clone(),
            runtime_dir: root.clone(),
            logs_dir: root.clone(),
        };
        let before_web = read_proxy(&service, false).expect("original web proxy");
        let before_secure = read_proxy(&service, true).expect("original secure web proxy");
        enable_service(&paths, service.clone(), 18991).expect("enable on duplicate service");
        assert_eq!(read_proxy(&service, false).unwrap(), expected(18991));
        assert_eq!(read_proxy(&service, true).unwrap(), expected(18991));
        cleanup_stale_proxy(&paths).expect("recover after simulated crash");
        assert_eq!(read_proxy(&service, false).unwrap(), before_web);
        assert_eq!(read_proxy(&service, true).unwrap(), before_secure);

        enable_service(&paths, service.clone(), 18991).expect("enable again");
        let changed = ProxyState {
            enabled: true,
            server: "other.proxy.invalid".into(),
            port: 8080,
            authenticated: false,
        };
        apply_proxy(&service, &changed, false).expect("simulate user change");
        disable(&paths).expect("restore only Skirk-owned settings");
        assert_eq!(read_proxy(&service, false).unwrap(), changed);
        assert_eq!(read_proxy(&service, true).unwrap(), before_secure);
        assert!(!backup_path(&paths).exists());
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    #[ignore = "temporarily changes the active macOS HTTP/HTTPS proxy; run only with an independent rollback"]
    fn live_active_service_routes_urlsession_and_restores() {
        use std::{
            io::{Read, Write},
            net::TcpListener,
            thread,
            time::{Duration, Instant},
        };

        let nonce = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let root = std::env::temp_dir().join(format!("skirk-mac-active-proxy-qa-{nonce}"));
        fs::create_dir_all(&root).expect("test runtime directory");
        let paths = AppPaths {
            profiles_file: root.join("profiles.json"),
            settings_file: root.join("settings.json"),
            config_dir: root.clone(),
            runtime_dir: root.clone(),
            logs_dir: root.clone(),
        };
        struct Restore<'a>(&'a AppPaths);
        impl Drop for Restore<'_> {
            fn drop(&mut self) {
                let _ = disable(self.0);
            }
        }
        let _restore = Restore(&paths);
        let service = active_service().expect("active service");
        let before_web = read_proxy(&service, false).expect("initial web proxy");
        let before_secure = read_proxy(&service, true).expect("initial secure web proxy");
        let listener = TcpListener::bind((LOCAL_PROXY_HOST, 18991)).expect("local test proxy port");
        listener
            .set_nonblocking(true)
            .expect("nonblocking listener");
        let token = format!("skirk-proxy-probe-{nonce}");
        let expected_token = token.clone();
        let server = thread::spawn(move || {
            let started = Instant::now();
            loop {
                match listener.accept() {
                    Ok((mut stream, _)) => {
                        stream
                            .set_read_timeout(Some(Duration::from_secs(3)))
                            .unwrap();
                        let mut buf = [0u8; 4096];
                        let size = stream.read(&mut buf).expect("proxy request");
                        let request = String::from_utf8_lossy(&buf[..size]);
                        assert!(
                            request
                                .starts_with(&format!("GET http://example.com/{expected_token} ")),
                            "unexpected request: {request}"
                        );
                        let body = format!("skirk-proxy-ok-{expected_token}");
                        let response = format!(
                            "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                            body.len(),
                            body
                        );
                        stream
                            .write_all(response.as_bytes())
                            .expect("proxy response");
                        return;
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                        assert!(
                            started.elapsed() < Duration::from_secs(15),
                            "URLSession did not contact the local proxy"
                        );
                        thread::sleep(Duration::from_millis(20));
                    }
                    Err(error) => panic!("proxy accept failed: {error}"),
                }
            }
        });

        enable(&paths, 18991).expect("enable active service proxy");
        let swift = format!(
            r#"
import Foundation
let url = URL(string: "http://example.com/{token}")!
let config = URLSessionConfiguration.default
config.timeoutIntervalForRequest = 10
let session = URLSession(configuration: config)
let semaphore = DispatchSemaphore(value: 0)
var result = ""
session.dataTask(with: url) {{ data, _, error in
    if let error = error {{ result = "ERROR: \(error)" }}
    else {{ result = String(data: data ?? Data(), encoding: .utf8) ?? "BAD_UTF8" }}
    semaphore.signal()
}}.resume()
if semaphore.wait(timeout: .now() + 12) == .timedOut {{ print("TIMEOUT") }}
else {{ print(result) }}
"#
        );
        let output = Command::new("/usr/bin/swift")
            .arg("-e")
            .arg(swift)
            .output()
            .expect("run native URLSession probe");
        assert!(
            output.status.success(),
            "Swift failed: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        assert_eq!(
            String::from_utf8_lossy(&output.stdout).trim(),
            format!("skirk-proxy-ok-{token}")
        );
        server.join().expect("proxy listener");
        disable(&paths).expect("restore active service");
        assert_eq!(read_proxy(&service, false).unwrap(), before_web);
        assert_eq!(read_proxy(&service, true).unwrap(), before_secure);
        let _ = fs::remove_dir_all(&root);
    }
}
