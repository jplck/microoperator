use std::fs;
use std::io::{self, Read, Write};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::{Path, PathBuf};
use std::thread;
use std::time::{Duration, Instant};

fn main() {
    let arguments: Vec<_> = std::env::args().collect();
    assert_eq!(
        arguments.len(),
        4,
        "usage: vsock-check JAIL_ROOT NONCE MODE"
    );
    let root = Path::new(&arguments[1]);
    let nonce = &arguments[2];
    let primary = arguments[3] == "primary";

    let result_listener = bind(root.join("vsock.sock_5000"));
    let host_service = primary.then(|| {
        let listener = bind(root.join("vsock.sock_7001"));
        thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let request = read_headers(&mut stream, 4096).unwrap();
            assert!(request.starts_with(b"GET /host HTTP/1.1"));
            stream
                .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\nhost-ok")
                .unwrap();
        })
    });

    let vsock = root.join("vsock.sock");
    wait_for(&vsock);

    let mut cancelled = connect_guest(&vsock, 8080);
    cancelled.write_all(b"GET /").unwrap();
    drop(cancelled);

    let mut oversized = connect_guest(&vsock, 8080);
    oversized.write_all(&vec![b'A'; 4097]).unwrap();
    let mut response = String::new();
    oversized.read_to_string(&mut response).unwrap();
    assert!(
        response.starts_with("HTTP/1.1 413"),
        "unexpected oversized response: {response:?}"
    );

    let mut streaming = connect_guest(&vsock, 8080);
    streaming
        .write_all(b"GET /stream HTTP/1.1\r\nHost: guest\r\n\r\n")
        .unwrap();
    response.clear();
    streaming.read_to_string(&mut response).unwrap();
    assert!(
        response.ends_with("part-one\npart-two\n"),
        "unexpected streaming response: {response:?}"
    );

    let mut slow = connect_guest(&vsock, 8080);
    slow.write_all(b"GET /slow HTTP/1.1\r\nHost: guest\r\n\r\n")
        .unwrap();
    slow.set_read_timeout(Some(Duration::from_millis(100)))
        .unwrap();
    let mut byte = [0; 1];
    assert!(matches!(
        slow.read(&mut byte),
        Err(error)
            if error.kind() == io::ErrorKind::WouldBlock
                || error.kind() == io::ErrorKind::TimedOut
    ));

    let (mut result, _) = result_listener.accept().unwrap();
    let message = String::from_utf8(read_headers(&mut result, 4096).unwrap()).unwrap();
    let expected =
        format!("nonce={nonce} readonly=true uid=1000 outbound={primary} network_denied=true\n");
    assert_eq!(message, expected);

    if let Some(service) = host_service {
        service.join().unwrap();
    }
    print!("{message}");
}

fn bind(path: PathBuf) -> UnixListener {
    let _ = fs::remove_file(&path);
    let listener = UnixListener::bind(&path).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o666)).unwrap();
    listener
}

fn wait_for(path: &Path) {
    let deadline = Instant::now() + Duration::from_secs(20);
    while !path.exists() {
        assert!(
            Instant::now() < deadline,
            "{} was not created",
            path.display()
        );
        thread::sleep(Duration::from_millis(20));
    }
}

fn connect_guest(path: &Path, port: u32) -> UnixStream {
    for attempt in 0..100 {
        if let Ok(mut stream) = UnixStream::connect(path) {
            writeln!(stream, "CONNECT {port}").unwrap();
            let acknowledgement = read_headers(&mut stream, 64).unwrap();
            if acknowledgement.starts_with(b"OK ") {
                return stream;
            }
        }
        assert!(attempt < 99, "guest port {port} did not become ready");
        thread::sleep(Duration::from_millis(50));
    }
    unreachable!()
}

fn read_headers(stream: &mut impl Read, maximum: usize) -> io::Result<Vec<u8>> {
    let mut message = Vec::new();
    let mut byte = [0; 1];
    while message.len() <= maximum {
        if stream.read(&mut byte)? == 0 {
            break;
        }
        message.push(byte[0]);
        if message.ends_with(b"\n") {
            break;
        }
    }
    if message.len() > maximum {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "message too large",
        ));
    }
    Ok(message)
}
