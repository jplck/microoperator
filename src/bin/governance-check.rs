use std::fs;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::Path;
use std::process::Command;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::thread;
use std::time::Duration;

fn main() {
    let policy = Path::new("policies/model.rego");
    let digest = command("sha256sum", &[policy.to_str().unwrap()])
        .split_whitespace()
        .next()
        .unwrap()
        .to_owned();

    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let address = listener.local_addr().unwrap();
    let requests = Arc::new(AtomicUsize::new(0));
    let request_count = Arc::clone(&requests);
    let upstream = thread::spawn(move || {
        let deadline = std::time::Instant::now() + Duration::from_secs(3);
        while std::time::Instant::now() < deadline {
            match listener.accept() {
                Ok((mut stream, _)) => {
                    let mut request = [0; 1024];
                    stream.read(&mut request).unwrap();
                    request_count.fetch_add(1, Ordering::SeqCst);
                    stream
                        .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK")
                        .unwrap();
                }
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                    thread::sleep(Duration::from_millis(10));
                }
                Err(error) => panic!("{error}"),
            }
        }
    });

    for (index, (model, timeout, expected)) in [
        ("allowed", "1s", "allow"),
        ("denied", "1s", "deny"),
        ("allowed", "0.001s", "deny"),
    ]
    .into_iter()
    .enumerate()
    {
        let trace_id = format!(
            "{:032x}",
            ((std::process::id() as u128) << 32) | index as u128
        );
        let action = evaluate(policy, model, timeout, &trace_id);
        assert_eq!(action, expected);
        if action == "allow" {
            dispatch(address);
        }
        println!(
            "{{\"trace_id\":\"{trace_id}\",\"policy_digest\":\"sha256:{digest}\",\"action\":\"{action}\"}}"
        );
    }

    upstream.join().unwrap();
    assert_eq!(
        requests.load(Ordering::SeqCst),
        1,
        "denied traffic reached the fake upstream"
    );
    println!("governance spike passed");
}

fn evaluate(policy: &Path, model: &str, timeout: &str, trace_id: &str) -> String {
    let snapshot = format!(
        r#"{{
  "spec_version": "acs/v0.1",
  "intervention_point": "pre_model_call",
  "trace_id": "{trace_id}",
  "source": {{"id": "agent-1", "verified": true}},
  "target": {{"id": "model-gateway"}},
  "request": {{"model": "{model}", "operation": "responses.create"}}
}}"#
    );
    let path = std::env::temp_dir().join(format!(
        "microorchestrator-governance-{}-{model}.json",
        std::process::id()
    ));
    fs::write(&path, snapshot).unwrap();
    let output = Command::new("timeout")
        .args([
            timeout,
            "opa",
            "eval",
            "--format",
            "raw",
            "--data",
            policy.to_str().unwrap(),
            "--input",
            path.to_str().unwrap(),
            "data.microorchestrator.action",
        ])
        .output();
    fs::remove_file(path).unwrap();

    match output {
        Ok(output) if output.status.success() => {
            let action = String::from_utf8_lossy(&output.stdout).trim().to_owned();
            match action.as_str() {
                "allow" | "deny" => action,
                _ => "deny".into(),
            }
        }
        _ => "deny".into(),
    }
}

fn dispatch(address: std::net::SocketAddr) {
    let mut stream = TcpStream::connect(address).unwrap();
    stream
        .write_all(b"POST /v1/responses HTTP/1.1\r\nHost: fake\r\nContent-Length: 0\r\n\r\n")
        .unwrap();
    let mut response = Vec::new();
    stream.read_to_end(&mut response).unwrap();
    assert!(response.starts_with(b"HTTP/1.1 200 OK"));
}

fn command(program: &str, arguments: &[&str]) -> String {
    let output = Command::new(program).args(arguments).output().unwrap();
    assert!(output.status.success(), "{program} failed");
    String::from_utf8(output.stdout).unwrap()
}
