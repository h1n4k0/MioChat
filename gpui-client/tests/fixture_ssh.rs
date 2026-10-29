use miochat_gpui::{
    connection::ConnectionSpec,
    session::{Request, Response, Session, SessionEvent},
};
use std::{
    io::{BufRead, BufReader, Write},
    path::PathBuf,
    process::{Child, Command, Stdio},
    time::{Duration, Instant},
};

struct Fixture(Child);
impl Drop for Fixture {
    fn drop(&mut self) {
        if let Some(mut input) = self.0.stdin.take() {
            let _ = input.write_all(b"stop\n");
        }
        let deadline = Instant::now() + Duration::from_secs(5);
        while Instant::now() < deadline {
            if self.0.try_wait().ok().flatten().is_some() {
                return;
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}
fn read(session: &Session, kind: &str, id: &str) -> Response {
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        match session
            .events()
            .recv_timeout(deadline.saturating_duration_since(Instant::now()))
            .expect("synthetic SSH response timeout")
        {
            SessionEvent::Response { response, .. }
                if response.kind == kind && (id.is_empty() || response.request_id == id) =>
            {
                return *response;
            }
            SessionEvent::Response { response, .. } => assert_ne!(
                response.kind, "error",
                "synthetic SSH error: {}",
                response.error
            ),
            event => panic!("synthetic SSH transport error: {event:?}"),
        }
    }
}

#[test]
#[ignore = "requires explicitly built synthetic server driver and bridge"]
fn windows_client_encrypted_key_bridge_and_real_subsystem() {
    let driver = PathBuf::from(
        std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit synthetic driver"),
    );
    let bridge =
        PathBuf::from(std::env::var_os("MIOCHAT_SSH_BRIDGE").expect("explicit bridge path"));
    let mut fixture = Fixture(
        Command::new(driver)
            .arg("-test.run=^TestGUISSHFixtureDriver$")
            .env("MIOCHAT_LOCAL_FIXTURE", "synthetic-ssh-v1")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .expect("synthetic server start"),
    );
    let stdout = fixture.0.stdout.take().unwrap();
    let mut line = String::new();
    BufReader::new(stdout).read_line(&mut line).unwrap();
    let config: serde_json::Value =
        serde_json::from_str(&line).expect("synthetic startup metadata");
    let spec = ConnectionSpec {
        host: config["host"].as_str().unwrap().into(),
        port: config["port"].as_u64().unwrap().try_into().unwrap(),
        ssh_user: "chat".into(),
    };
    let known = PathBuf::from(config["known_hosts"].as_str().unwrap());
    let identity = PathBuf::from(config["identity"].as_str().unwrap());
    let mut session = Session::connect_identity(
        &spec,
        &known,
        &identity,
        config["passphrase"].as_str().unwrap(),
        &bridge,
        32,
    )
    .expect("bridge spawn");
    assert!(read(&session, "hello", "").device_key.unwrap().bound);
    session.send(&Request::simple("login_key", "key")).unwrap();
    assert_eq!(read(&session, "auth", "key").user, "ssh-preview");
    read(&session, "state", "");
    let mut bind = Request::simple("bind_key", "rebind");
    bind.password = "test-password-123";
    bind.body = "Windows synthetic";
    session.send(&bind).unwrap();
    assert!(
        read(&session, "key_bound", "rebind")
            .device_key
            .unwrap()
            .bound
    );
    session.disconnect();
    // Incorrect passphrase fails in the isolated helper, with no fallback to
    // an agent/default key and no private initializer echoed into diagnostics.
    let mut failure = Session::connect_identity(
        &spec,
        &known,
        &identity,
        "incorrect-synthetic-phrase",
        &bridge,
        33,
    )
    .unwrap();
    let event = failure
        .events()
        .recv_timeout(Duration::from_secs(10))
        .expect("expected unlock failure");
    match event {
        SessionEvent::Error { message, .. } => {
            assert!(!message.contains("incorrect-synthetic-phrase"));
            assert!(message.contains("unlock selected device key"));
        }
        other => panic!("unexpected unlock result: {other:?}"),
    }
    failure.disconnect();
    drop(fixture);
    assert!(
        !identity.exists(),
        "synthetic private-key file was not cleaned up"
    );
    assert!(
        !known.exists(),
        "synthetic known-host file was not cleaned up"
    );
}
