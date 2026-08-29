use std::fs;
use std::io::{self, Read, Write};
use std::mem::size_of;
use std::net::{Ipv4Addr, TcpStream};
use std::os::fd::FromRawFd;
use std::os::unix::process::CommandExt;
use std::process::Command;
use std::thread;
use std::time::{Duration, Instant};

fn main() {
    mount(b"proc\0", b"/proc\0", Some(b"proc\0"), 0);
    assert!(Command::new("/sbin/ip")
        .args(["link", "set", "lo", "up"])
        .status()
        .unwrap()
        .success());

    mount(b"/dev/vdb\0", b"/mnt\0", Some(b"ext4\0"), libc::MS_RDONLY);
    mount(b"/dev\0", b"/mnt/dev\0", None, libc::MS_BIND);
    mount(b"proc\0", b"/mnt/proc\0", Some(b"proc\0"), 0);

    let mut workload = unsafe {
        Command::new("/usr/local/bin/python")
            .arg("/agent/__main__.py")
            .env("HOME", "/tmp")
            .pre_exec(|| {
                if libc::chroot(b"/mnt\0".as_ptr().cast()) != 0
                    || libc::chdir(b"/agent\0".as_ptr().cast()) != 0
                    || libc::setgroups(0, std::ptr::null()) != 0
                    || libc::setgid(1000) != 0
                    || libc::setuid(1000) != 0
                {
                    return Err(io::Error::last_os_error());
                }
                Ok(())
            })
            .spawn()
            .expect("start OCI workload")
    };

    let status = fs::read_to_string(format!("/proc/{}/status", workload.id())).unwrap();
    assert!(status
        .lines()
        .any(|line| line.starts_with("Uid:\t1000\t1000")));

    let deadline = Instant::now() + Duration::from_secs(20);
    while TcpStream::connect((Ipv4Addr::LOCALHOST, 9999)).is_err() {
        assert!(Instant::now() < deadline, "A2A workload did not start");
        thread::sleep(Duration::from_millis(50));
    }

    let listener = listen_vsock(9999);
    let connection = unsafe { libc::accept(listener, std::ptr::null_mut(), std::ptr::null_mut()) };
    assert!(connection >= 0);
    let mut connection = unsafe { fs::File::from_raw_fd(connection) };
    let mut request = Vec::new();
    let mut byte = [0; 1];
    while request.len() <= 4096 && !request.ends_with(b"\r\n\r\n") {
        if connection.read(&mut byte).unwrap() == 0 {
            break;
        }
        request.push(byte[0]);
    }
    assert!(request.len() <= 4096 && request.ends_with(b"\r\n\r\n"));

    let mut agent = TcpStream::connect((Ipv4Addr::LOCALHOST, 9999)).unwrap();
    agent.write_all(&request).unwrap();
    io::copy(&mut agent, &mut connection).unwrap();
    drop(connection);
    unsafe { libc::close(listener) };

    workload.kill().unwrap();
    workload.wait().unwrap();
    loop {
        unsafe { libc::pause() };
    }
}

fn mount(source: &[u8], target: &[u8], filesystem: Option<&[u8]>, flags: libc::c_ulong) {
    let filesystem = filesystem.map_or(std::ptr::null(), |value| value.as_ptr().cast());
    let result = unsafe {
        libc::mount(
            source.as_ptr().cast(),
            target.as_ptr().cast(),
            filesystem,
            flags,
            std::ptr::null(),
        )
    };
    assert_eq!(result, 0, "mount failed: {}", io::Error::last_os_error());
}

fn listen_vsock(port: u32) -> libc::c_int {
    let socket = unsafe { libc::socket(libc::AF_VSOCK, libc::SOCK_STREAM, 0) };
    assert!(socket >= 0);
    let address = libc::sockaddr_vm {
        svm_family: libc::AF_VSOCK as libc::sa_family_t,
        svm_reserved1: 0,
        svm_port: port,
        svm_cid: u32::MAX,
        svm_zero: [0; 4],
    };
    assert_eq!(
        unsafe {
            libc::bind(
                socket,
                (&address as *const libc::sockaddr_vm).cast(),
                size_of::<libc::sockaddr_vm>() as libc::socklen_t,
            )
        },
        0
    );
    assert_eq!(unsafe { libc::listen(socket, 1) }, 0);
    socket
}
