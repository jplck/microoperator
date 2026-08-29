use std::io::{Read, Write};
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::thread;
use std::time::{Duration, Instant};

fn main() {
    let path = std::env::args()
        .nth(1)
        .expect("usage: oci-check VSOCK_PATH");
    let path = Path::new(&path);
    let deadline = Instant::now() + Duration::from_secs(20);

    let mut stream = loop {
        if let Ok(mut stream) = UnixStream::connect(path) {
            writeln!(stream, "CONNECT 9999").unwrap();
            let mut acknowledgement = Vec::new();
            while !acknowledgement.ends_with(b"\n") {
                let mut byte = [0; 1];
                if stream.read(&mut byte).unwrap() == 0 {
                    break;
                }
                acknowledgement.push(byte[0]);
            }
            if acknowledgement.starts_with(b"OK ") {
                break stream;
            }
        }
        assert!(Instant::now() < deadline, "OCI guest did not become ready");
        thread::sleep(Duration::from_millis(50));
    };

    stream
        .write_all(
            b"GET /.well-known/agent-card.json HTTP/1.1\r\nHost: agent\r\nConnection: close\r\n\r\n",
        )
        .unwrap();
    let mut response = String::new();
    stream.read_to_string(&mut response).unwrap();
    assert!(response.starts_with("HTTP/1.1 200"));
    assert!(response.contains("\"name\":\"Hello World Agent\""));
    println!("unmodified OCI A2A agent passed");
}
