use std::fs::{self, OpenOptions};
use std::io::{self, Write};
use std::process::Command;

const MINIMUM_FREE_DISK: u64 = 10 << 30;

struct Check {
    name: &'static str,
    run: fn() -> Result<String, String>,
}

fn main() {
    let arguments: Vec<_> = std::env::args().collect();
    if arguments.len() != 2 || arguments[1] != "host-check" {
        eprintln!("usage: microorchestrator host-check");
        std::process::exit(2);
    }
    std::process::exit(report(&mut io::stdout(), &host_checks()));
}

fn host_checks() -> Vec<Check> {
    vec![
        Check {
            name: "host",
            run: check_host,
        },
        Check {
            name: "kvm",
            run: check_kvm,
        },
        Check {
            name: "cgroup v2",
            run: check_cgroup_v2,
        },
        Check {
            name: "firecracker",
            run: || check_binary("firecracker"),
        },
        Check {
            name: "jailer",
            run: || check_binary("jailer"),
        },
        Check {
            name: "vsock",
            run: check_vsock,
        },
        Check {
            name: "disk",
            run: check_disk,
        },
    ]
}

fn report(output: &mut dyn Write, checks: &[Check]) -> i32 {
    let mut failed = false;
    for check in checks {
        match (check.run)() {
            Ok(detail) => writeln!(output, "OK   {:<12} {}", check.name, detail).unwrap(),
            Err(error) => {
                failed = true;
                writeln!(output, "FAIL {:<12} {}", check.name, error).unwrap();
            }
        }
    }
    i32::from(failed)
}

fn check_host() -> Result<String, String> {
    let kernel = command_output("uname", &["-r"])?;
    if std::env::consts::OS != "linux" || std::env::consts::ARCH != "x86_64" {
        return Err(format!(
            "requires linux/x86_64, found {}/{} ({kernel})",
            std::env::consts::OS,
            std::env::consts::ARCH
        ));
    }
    Ok(format!("linux/x86_64 kernel {kernel}"))
}

fn check_kvm() -> Result<String, String> {
    OpenOptions::new()
        .read(true)
        .write(true)
        .open("/dev/kvm")
        .map_err(|error| format!("open /dev/kvm read-write: {error}"))?;
    Ok("/dev/kvm is read-write".into())
}

fn check_cgroup_v2() -> Result<String, String> {
    let controllers = fs::read_to_string("/sys/fs/cgroup/cgroup.controllers")
        .map_err(|error| format!("read cgroup v2 controllers: {error}"))?;
    Ok(format!(
        "controllers: {}",
        controllers.split_whitespace().collect::<Vec<_>>().join(",")
    ))
}

fn check_binary(name: &str) -> Result<String, String> {
    let path = command_output("sh", &["-c", &format!("command -v {name}")])?;
    let version = command_output(&path, &["--version"])?;
    let hash = command_output("sha256sum", &[&path])?
        .split_whitespace()
        .next()
        .ok_or_else(|| format!("sha256sum returned no hash for {path}"))?
        .to_owned();
    Ok(format!("{version}; sha256:{hash}"))
}

fn check_vsock() -> Result<String, String> {
    let socket = unsafe { libc::socket(libc::AF_VSOCK, libc::SOCK_STREAM, 0) };
    if socket < 0 {
        return Err(format!(
            "create AF_VSOCK socket: {}",
            io::Error::last_os_error()
        ));
    }
    unsafe { libc::close(socket) };
    Ok("AF_VSOCK stream socket available".into())
}

fn check_disk() -> Result<String, String> {
    let path = b".\0";
    let mut stat = std::mem::MaybeUninit::<libc::statvfs>::uninit();
    if unsafe { libc::statvfs(path.as_ptr().cast(), stat.as_mut_ptr()) } != 0 {
        return Err(io::Error::last_os_error().to_string());
    }
    let stat = unsafe { stat.assume_init() };
    let free = stat.f_bavail * stat.f_frsize;
    if free < MINIMUM_FREE_DISK {
        return Err(format!(
            "need 10 GiB free, found {:.1} GiB",
            free as f64 / (1 << 30) as f64
        ));
    }
    Ok(format!(
        "{:.1} GiB free (10 GiB required)",
        free as f64 / (1 << 30) as f64
    ))
}

fn command_output(command: &str, arguments: &[&str]) -> Result<String, String> {
    let output = Command::new(command)
        .args(arguments)
        .output()
        .map_err(|error| format!("{command}: {error}"))?;
    if !output.status.success() {
        return Err(format!("{command} exited {}", output.status));
    }
    let mut text = String::from_utf8_lossy(&output.stdout).trim().to_owned();
    let stderr = String::from_utf8_lossy(&output.stderr);
    if !stderr.trim().is_empty() {
        if !text.is_empty() {
            text.push('\n');
        }
        text.push_str(stderr.trim());
    }
    Ok(text)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn good() -> Result<String, String> {
        Ok("ready".into())
    }

    fn bad() -> Result<String, String> {
        Err("missing thing".into())
    }

    #[test]
    fn report_fails_with_useful_reason() {
        let mut output = Vec::new();
        let exit = report(
            &mut output,
            &[
                Check {
                    name: "good",
                    run: good,
                },
                Check {
                    name: "bad",
                    run: bad,
                },
            ],
        );
        let output = String::from_utf8(output).unwrap();
        assert_eq!(exit, 1);
        assert!(output.contains("FAIL bad"));
        assert!(output.contains("missing thing"));
    }
}
