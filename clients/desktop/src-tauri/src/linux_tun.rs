use serde_json::Value;

#[cfg(target_os = "linux")]
use std::process::Command;
#[cfg(any(target_os = "linux", all(test, unix)))]
use std::{
    process::{Child, Command as SignalCommand, Stdio},
    thread,
    time::{Duration, Instant},
};

pub(crate) const ROUTE_TABLE: u64 = 2022;
pub(crate) const FIRST_RULE: u64 = 9000;
// sing-tun clears this full range when auto_route starts and stops.
const LAST_RULE: u64 = FIRST_RULE + 10;

#[cfg(target_os = "linux")]
pub(crate) fn require_available(interface: &str) -> Result<(), String> {
    let links = ip_json(&["link", "show"])?;
    if interface_taken(&links, interface) {
        return Err(format!(
            "VPN needs the unused network interface {interface}."
        ));
    }
    for family in ["-4", "-6"] {
        let rules = ip_json(&[family, "rule", "show"])?;
        let routes = ip_json(&[family, "route", "show", "table", "all"])?;
        check_policy(&rules, &routes)?;
    }
    Ok(())
}

#[cfg(target_os = "linux")]
fn ip_json(args: &[&str]) -> Result<Vec<Value>, String> {
    let output = Command::new("ip")
        .args(["-j", "-N"])
        .args(args)
        .output()
        .map_err(|error| format!("Could not inspect Linux VPN routing with iproute2: {error}"))?;
    if !output.status.success() {
        return Err(format!(
            "Could not inspect Linux VPN routing with iproute2 ({}).",
            output.status
        ));
    }
    serde_json::from_slice(&output.stdout)
        .map_err(|error| format!("Could not read Linux VPN routing from iproute2: {error}"))
}

fn interface_taken(links: &[Value], interface: &str) -> bool {
    links
        .iter()
        .any(|link| link["ifname"].as_str() == Some(interface))
}

fn numeric(value: &Value) -> Option<u64> {
    value.as_u64().or_else(|| value.as_str()?.parse().ok())
}

fn check_policy(rules: &[Value], routes: &[Value]) -> Result<(), String> {
    for rule in rules {
        let Some(priority) = numeric(&rule["priority"]) else {
            return Err("Could not read a Linux policy rule priority from iproute2.".into());
        };
        if (FIRST_RULE..=LAST_RULE).contains(&priority) {
            return Err(format!("VPN needs unused Linux policy rule priorities {FIRST_RULE}–{LAST_RULE}. Priority {priority} is in use."));
        }
    }
    if rules
        .iter()
        .chain(routes)
        .any(|row| numeric(&row["table"]) == Some(ROUTE_TABLE))
    {
        return Err(format!("VPN needs unused Linux route table {ROUTE_TABLE}."));
    }
    Ok(())
}

#[cfg(any(target_os = "linux", all(test, unix)))]
pub(crate) fn stop_owned(child: &mut Child, grace: Duration) -> Result<(), String> {
    let status = || "The Linux VPN process status could not be checked.";
    if child.try_wait().map_err(|_| status())?.is_some() {
        return Ok(());
    }
    // The process remains our unreaped child while the signal is sent.
    let signalled = SignalCommand::new("kill")
        .args(["-TERM", &child.id().to_string()])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .is_ok_and(|result| result.success());
    let deadline = Instant::now() + grace;
    loop {
        if child.try_wait().map_err(|_| status())?.is_some() {
            return Ok(());
        }
        if !signalled || Instant::now() >= deadline {
            break;
        }
        thread::sleep(Duration::from_millis(25));
    }
    if child.kill().is_err() {
        return Err("Could not stop the Linux VPN process.".into());
    }
    child
        .wait()
        .map_err(|_| "Could not reap the Linux VPN process.")?;
    Err(
        "The Linux VPN process had to be stopped. Check its route cleanup before reconnecting."
            .into(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn allows_out_of_range_rules_and_routes_in_unrelated_tables() {
        let rules = [
            json!({"priority": 0, "table": "255"}),
            json!({"priority": 32768, "table": 32108}),
        ];
        let routes = [json!({"dst": "default"}), json!({"table": "32108"})];
        assert!(check_policy(&rules, &routes).is_ok());
    }

    #[test]
    fn detects_interface_rule_range_and_table_conflicts() {
        assert!(interface_taken(
            &[json!({"ifname": "skirk-tun0"})],
            "skirk-tun0"
        ));
        for priority in [FIRST_RULE, LAST_RULE] {
            let rules = [json!({"priority": priority, "table": 32108})];
            assert!(check_policy(&rules, &[]).is_err());
        }
        let rules = [json!({"priority":"12000", "table": ROUTE_TABLE})];
        assert!(check_policy(&rules, &[]).is_err());
        let routes = [json!({"table": ROUTE_TABLE.to_string()})];
        assert!(check_policy(&[], &routes).is_err());
        assert!(check_policy(&[json!({"priority": "unknown"})], &[]).is_err());
    }

    #[cfg(unix)]
    #[test]
    fn graceful_stop_only_signals_the_owned_process() {
        use std::io::{BufRead, BufReader};
        let mut sentinel = SignalCommand::new("sleep").arg("30").spawn().unwrap();
        let mut owned = SignalCommand::new("sh")
            .args([
                "-c",
                "trap 'exit 0' TERM; printf 'ready\\n'; while :; do sleep 0.1; done",
            ])
            .stdout(Stdio::piped())
            .spawn()
            .unwrap();
        let mut ready = String::new();
        BufReader::new(owned.stdout.take().unwrap())
            .read_line(&mut ready)
            .unwrap();
        assert_eq!(ready.trim(), "ready");
        let stopped = stop_owned(&mut owned, Duration::from_secs(1));
        let other_still_running = sentinel.try_wait().unwrap().is_none();
        let _ = sentinel.kill();
        let _ = sentinel.wait();
        assert!(stopped.is_ok());
        assert!(owned.try_wait().unwrap().unwrap().success());
        assert!(other_still_running);
    }
}
