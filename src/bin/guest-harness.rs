use std::fs::{self, OpenOptions};
use std::io::{self, Read, Write};
use std::mem::size_of;
use std::net::{Ipv4Addr, SocketAddrV4, TcpListener, TcpStream};
use std::os::fd::FromRawFd;
use std::process::Command;
use std::thread;
use std::time::Duration;

const HOST_CID: u32 = 2;
const PORT: u32 = 5000;

fn main() {
    unsafe {
        libc::mount(
            b"proc\0".as_ptr().cast(),
            b"/proc\0".as_ptr().cast(),
            b"proc\0".as_ptr().cast(),
            0,
            std::ptr::null(),
        );
    }

    let command_line = fs::read_to_string("/proc/cmdline").expect("read /proc/cmdline");
    let nonce = command_line
        .split_whitespace()
        .find_map(|field| field.strip_prefix("check.nonce="))
        .expect("check.nonce is missing");
    let expect_outbound = command_line
        .split_whitespace()
        .find_map(|field| field.strip_prefix("check.expect_outbound="))
        .map(|value| value == "true")
        .expect("check.expect_outbound is missing");

    let read_only = OpenOptions::new()
        .create(true)
        .write(true)
        .open("/runtime-write-test")
        .is_err_and(|error| error.raw_os_error() == Some(libc::EROFS));
    assert!(Command::new("/sbin/ip")
        .args(["link", "set", "lo", "up"])
        .status()
        .expect("start loopback")
        .success());

    limit(libc::RLIMIT_CPU, 2);
    limit(libc::RLIMIT_FSIZE, 1 << 20);
    limit(libc::RLIMIT_NOFILE, 64);
    limit(libc::RLIMIT_NPROC, 32);

    unsafe {
        if libc::setgroups(0, std::ptr::null()) != 0
            || libc::setgid(1000) != 0
            || libc::setuid(1000) != 0
        {
            panic!("{}", io::Error::last_os_error());
        }
    }

    let loopback = TcpListener::bind((Ipv4Addr::LOCALHOST, 8081)).unwrap();
    thread::spawn(move || serve_loopback(loopback));
    thread::spawn(serve_vsock);

    let outbound = guest_to_host();
    assert_eq!(outbound, expect_outbound);
    let network_denied = TcpStream::connect_timeout(
        &SocketAddrV4::new(Ipv4Addr::new(1, 1, 1, 1), 80).into(),
        Duration::from_millis(200),
    )
    .is_err();

    let mut connection = connect_vsock(PORT, 100).expect("connect result channel");
    writeln!(
        connection,
        "nonce={nonce} readonly={read_only} uid={} outbound={outbound} network_denied={network_denied}",
        unsafe { libc::getuid() }
    )
    .expect("send result");

    loop {
        unsafe { libc::pause() };
    }
}

fn serve_loopback(listener: TcpListener) {
    for _ in 0..3 {
        let (mut stream, _) = listener.accept().unwrap();
        let mut request = [0; 4096];
        let size = stream.read(&mut request).unwrap_or(0);
        if request[..size]
            .windows(9)
            .any(|value| value == b"GET /slow")
        {
            thread::sleep(Duration::from_secs(1));
        }
        let _ = stream.write_all(b"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\npart-one\n");
        thread::sleep(Duration::from_millis(50));
        let _ = stream.write_all(b"part-two\n");
    }
}

fn serve_vsock() {
    let listener = listen_vsock(8080);
    for _ in 0..4 {
        let connection =
            unsafe { libc::accept(listener, std::ptr::null_mut(), std::ptr::null_mut()) };
        if connection < 0 {
            panic!("{}", io::Error::last_os_error());
        }
        let mut connection = unsafe { fs::File::from_raw_fd(connection) };
        let mut request = Vec::new();
        let mut byte = [0; 1];
        while request.len() <= 4096 {
            match connection.read(&mut byte) {
                Ok(0) => break,
                Ok(_) => {
                    request.push(byte[0]);
                    if request.ends_with(b"\r\n\r\n") {
                        break;
                    }
                }
                Err(_) => break,
            }
        }
        if request.len() > 4096 {
            let _ = connection
                .write_all(b"HTTP/1.1 413 Content Too Large\r\nContent-Length: 0\r\n\r\n");
            continue;
        }
        if !request.ends_with(b"\r\n\r\n") {
            continue;
        }
        if let Ok(mut loopback) = TcpStream::connect((Ipv4Addr::LOCALHOST, 8081)) {
            let _ = loopback.write_all(&request);
            let _ = io::copy(&mut loopback, &mut connection);
        }
    }
    unsafe { libc::close(listener) };
}

fn guest_to_host() -> bool {
    let Some(mut connection) = connect_vsock(7001, 20) else {
        return false;
    };
    connection
        .write_all(b"GET /host HTTP/1.1\r\nHost: host\r\n\r\n")
        .unwrap();
    let mut response = String::new();
    connection.read_to_string(&mut response).unwrap();
    response.starts_with("HTTP/1.1 200 OK") && response.ends_with("host-ok")
}

fn connect_vsock(port: u32, attempts: usize) -> Option<fs::File> {
    let socket = unsafe { libc::socket(libc::AF_VSOCK, libc::SOCK_STREAM, 0) };
    if socket < 0 {
        return None;
    }
    let address = libc::sockaddr_vm {
        svm_family: libc::AF_VSOCK as libc::sa_family_t,
        svm_reserved1: 0,
        svm_port: port,
        svm_cid: HOST_CID,
        svm_zero: [0; 4],
    };
    for attempt in 0..attempts {
        if unsafe {
            libc::connect(
                socket,
                (&address as *const libc::sockaddr_vm).cast(),
                size_of::<libc::sockaddr_vm>() as libc::socklen_t,
            )
        } == 0
        {
            return Some(unsafe { fs::File::from_raw_fd(socket) });
        }
        if attempt + 1 < attempts {
            thread::sleep(Duration::from_millis(100));
        }
    }
    unsafe { libc::close(socket) };
    None
}

fn listen_vsock(port: u32) -> libc::c_int {
    let socket = unsafe { libc::socket(libc::AF_VSOCK, libc::SOCK_STREAM, 0) };
    if socket < 0 {
        panic!("{}", io::Error::last_os_error());
    }
    let address = libc::sockaddr_vm {
        svm_family: libc::AF_VSOCK as libc::sa_family_t,
        svm_reserved1: 0,
        svm_port: port,
        svm_cid: u32::MAX,
        svm_zero: [0; 4],
    };
    if unsafe {
        libc::bind(
            socket,
            (&address as *const libc::sockaddr_vm).cast(),
            size_of::<libc::sockaddr_vm>() as libc::socklen_t,
        )
    } != 0
        || unsafe { libc::listen(socket, 8) } != 0
    {
        panic!("{}", io::Error::last_os_error());
    }
    socket
}

fn limit(resource: libc::__rlimit_resource_t, value: libc::rlim_t) {
    let limits = libc::rlimit {
        rlim_cur: value,
        rlim_max: value,
    };
    if unsafe { libc::setrlimit(resource, &limits) } != 0 {
        panic!("{}", io::Error::last_os_error());
    }
}
