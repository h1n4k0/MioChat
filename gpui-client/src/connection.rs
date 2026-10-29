use anyhow::{Context as _, Result, bail};
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};

/// Values needed by the upstream `ssh -t -p PORT chat@HOST` contract.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConnectionSpec {
    pub host: String,
    pub port: u16,
    pub ssh_user: String,
}

impl Default for ConnectionSpec {
    fn default() -> Self {
        Self {
            host: "127.0.0.1".into(),
            port: 2222,
            ssh_user: "chat".into(),
        }
    }
}

impl ConnectionSpec {
    /// Normal first-use endpoint; the fixture default remains loopback-only.
    pub fn public_service() -> Self {
        Self {
            host: "chat.mio.blue".into(),
            port: 22,
            ssh_user: "chat".into(),
        }
    }

    pub fn validate(&self) -> Result<()> {
        validate_atom("host", &self.host, 253, |ch| {
            ch.is_ascii_alphanumeric() || matches!(ch, '.' | '-' | ':' | '[' | ']')
        })?;
        validate_atom("SSH user", &self.ssh_user, 63, |ch| {
            ch.is_ascii_alphanumeric() || matches!(ch, '_' | '-')
        })?;
        if self.port == 0 {
            bail!("port must be between 1 and 65535");
        }
        Ok(())
    }

    pub fn destination(&self) -> String {
        format!("{}@{}", self.ssh_user, self.host)
    }
}

fn validate_atom(
    label: &str,
    value: &str,
    max_len: usize,
    allowed: impl Fn(char) -> bool,
) -> Result<()> {
    if value.is_empty() || value.len() > max_len {
        bail!("{label} must contain 1-{max_len} characters");
    }
    if value.starts_with('-') || !value.chars().all(allowed) {
        bail!("{label} contains unsupported characters");
    }
    Ok(())
}

pub fn app_known_hosts_path() -> Result<PathBuf> {
    #[cfg(windows)]
    let base = std::env::var_os("LOCALAPPDATA").map(PathBuf::from);
    #[cfg(not(windows))]
    let base = std::env::var_os("XDG_DATA_HOME")
        .map(PathBuf::from)
        .or_else(|| std::env::var_os("HOME").map(|path| PathBuf::from(path).join(".local/share")));
    let directory = base
        .context("cannot determine application data directory")?
        .join("MioChat GPUI");
    std::fs::create_dir_all(&directory)
        .with_context(|| format!("cannot create {}", directory.display()))?;
    Ok(directory.join("known_hosts"))
}

#[cfg(windows)]
const NULL_CONFIG: &str = "NUL";
#[cfg(not(windows))]
const NULL_CONFIG: &str = "/dev/null";

/// Constructs an argv-only OpenSSH invocation. `-F` prevents a user's SSH
/// config from adding a ProxyCommand, forwarding, or local command. Host keys
/// remain strict and are stored separately from the user's ordinary SSH file.
pub fn ssh_args(spec: &ConnectionSpec, known_hosts: &Path) -> Result<Vec<String>> {
    spec.validate()?;
    let known_hosts = known_hosts
        .to_str()
        .context("known_hosts path is not valid Unicode")?;
    Ok(vec![
        "-F".into(),
        NULL_CONFIG.into(),
        "-T".into(),
        "-o".into(),
        "ClearAllForwardings=yes".into(),
        "-o".into(),
        "ForwardAgent=no".into(),
        "-o".into(),
        "PermitLocalCommand=no".into(),
        "-o".into(),
        "PreferredAuthentications=keyboard-interactive".into(),
        "-o".into(),
        "PubkeyAuthentication=no".into(),
        "-o".into(),
        "PasswordAuthentication=no".into(),
        "-o".into(),
        "KbdInteractiveAuthentication=yes".into(),
        "-o".into(),
        "StrictHostKeyChecking=yes".into(),
        "-o".into(),
        format!(
            "UserKnownHostsFile=\"{}\"",
            known_hosts
                .replace('\\', "/")
                .replace('%', "%%")
                .replace('"', "\\\"")
        ),
        "-o".into(),
        format!("GlobalKnownHostsFile={NULL_CONFIG}"),
        "-o".into(),
        "IdentityAgent=none".into(),
        "-o".into(),
        "UpdateHostKeys=no".into(),
        "-o".into(),
        "ServerAliveInterval=30".into(),
        "-o".into(),
        "ServerAliveCountMax=3".into(),
        "-o".into(),
        "ConnectTimeout=30".into(),
        "-o".into(),
        "LogLevel=ERROR".into(),
        "-p".into(),
        spec.port.to_string(),
        "-s".into(),
        "--".into(),
        spec.destination(),
        "miochat-gui-v1".into(),
    ])
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct HostKeyProbe {
    pub line: String,
    pub fingerprint: String,
}

pub fn host_key_lookup_name(spec: &ConnectionSpec) -> String {
    if spec.port == 22 {
        spec.host.clone()
    } else {
        format!("[{}]:{}", spec.host, spec.port)
    }
}

pub fn is_host_known(spec: &ConnectionSpec, known_hosts: &Path) -> Result<bool> {
    spec.validate()?;
    if !known_hosts.exists() {
        return Ok(false);
    }
    let status = Command::new("ssh-keygen")
        .arg("-F")
        .arg(host_key_lookup_name(spec))
        .arg("-f")
        .arg(known_hosts)
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .context("cannot run ssh-keygen to inspect known hosts")?;
    Ok(status.success())
}

/// Read only the dedicated known-host entries selected by the exact lookup
/// name used by SSH. This is a recovery identity context, not negotiation data.
pub fn trusted_host_context(spec: &ConnectionSpec, known_hosts: &Path) -> Result<String> {
    spec.validate()?;
    if std::fs::metadata(known_hosts)?.len() > 1024 * 1024 {
        bail!("known host context is too large");
    }
    let out = Command::new("ssh-keygen")
        .arg("-F")
        .arg(host_key_lookup_name(spec))
        .arg("-f")
        .arg(known_hosts)
        .stderr(Stdio::null())
        .output()
        .context("cannot read trusted host context")?;
    if !out.status.success() || out.stdout.len() > 1024 * 1024 {
        bail!("trusted host context unavailable");
    }
    let text = String::from_utf8(out.stdout)?;
    trusted_context_lines(&text)
}
fn trusted_context_lines(text: &str) -> Result<String> {
    let mut keys = Vec::new();
    for line in text
        .lines()
        .filter(|line| !line.starts_with('#') && !line.trim().is_empty())
    {
        let fields: Vec<_> = line.split_whitespace().collect();
        // App trust_host_key writes ordinary host/keytype/base64 entries.
        // Unsupported markers fail closed, never fall back to the username.
        if fields.len() < 3 || fields[0].starts_with('@') {
            bail!("unsupported trusted host context");
        }
        keys.push(format!("{} {}", fields[1], fields[2]));
    }
    keys.sort();
    keys.dedup();
    if keys.is_empty() {
        bail!("no matched trusted host keys");
    }
    Ok(keys.join("\n"))
}

pub fn probe_host_key(spec: &ConnectionSpec) -> Result<HostKeyProbe> {
    spec.validate()?;
    let port = spec.port.to_string();
    let output = Command::new("ssh-keyscan")
        .args(["-T", "5", "-p", &port, "--", &spec.host])
        .output()
        .context("cannot run ssh-keyscan")?;
    if !output.status.success() && output.stdout.is_empty() {
        bail!("could not retrieve the server host key");
    }
    let text = String::from_utf8(output.stdout).context("ssh-keyscan returned invalid UTF-8")?;
    let line = text
        .lines()
        .find(|line| !line.starts_with('#') && !line.trim().is_empty())
        .context("server returned no usable host key")?
        .to_string();
    let mut child = Command::new("ssh-keygen")
        .args(["-lf", "-"])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .context("cannot run ssh-keygen to fingerprint host key")?;
    child
        .stdin
        .take()
        .context("ssh-keygen stdin unavailable")?
        .write_all(line.as_bytes())?;
    let output = child.wait_with_output()?;
    if !output.status.success() {
        bail!("ssh-keygen could not fingerprint the host key");
    }
    let fingerprint = String::from_utf8(output.stdout)?.trim().to_string();
    Ok(HostKeyProbe { line, fingerprint })
}

pub fn trust_host_key(known_hosts: &Path, probe: &HostKeyProbe) -> Result<()> {
    if let Some(parent) = known_hosts.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let mut file = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(known_hosts)?;
    writeln!(file, "{}", probe.line)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validates_connection_values() {
        let mut spec = ConnectionSpec::default();
        assert!(spec.validate().is_ok());
        spec.host = "-oProxyCommand=bad".into();
        assert!(spec.validate().is_err());
        spec.host = "example.com evil".into();
        assert!(spec.validate().is_err());
        spec.host = "[2001:db8::1]".into();
        assert!(spec.validate().is_ok());
    }

    #[test]
    fn ssh_argv_is_isolated_and_strict() {
        let args = ssh_args(
            &ConnectionSpec::default(),
            Path::new("/tmp/mio known_hosts"),
        )
        .expect("valid argv");
        assert_eq!(args[args.len() - 2], "chat@127.0.0.1");
        assert_eq!(args.last().unwrap(), "miochat-gui-v1");
        assert!(args.iter().any(|arg| arg == "StrictHostKeyChecking=yes"));
        assert!(args.iter().any(|arg| arg == "ClearAllForwardings=yes"));
        assert!(args.iter().any(|arg| arg == "PubkeyAuthentication=no"));
        assert!(
            !args
                .iter()
                .any(|arg| arg.contains("StrictHostKeyChecking=no"))
        );
    }
    #[test]
    fn recovery_trusted_context_uses_matched_keys_not_comments_or_empty_fallback() {
        let input = "# Host line\nexample ssh-ed25519 SYNTHETIC_KEY\nexample ssh-rsa SYNTHETIC_RSA\nexample ssh-ed25519 SYNTHETIC_KEY\n";
        assert_eq!(
            trusted_context_lines(input).unwrap(),
            "ssh-ed25519 SYNTHETIC_KEY\nssh-rsa SYNTHETIC_RSA"
        );
        assert!(trusted_context_lines("# empty\n").is_err());
        assert!(trusted_context_lines("@cert-authority host key value").is_err());
        assert!(trusted_context_lines("invalid").is_err());
    }
}

#[cfg(test)]
mod public_endpoint_tests {
    use super::*;
    #[test]
    fn default_service_uses_anonymous_strict_transport_not_application_credentials() {
        let spec = ConnectionSpec::public_service();
        assert_eq!(spec.destination(), "chat@chat.mio.blue");
        assert_eq!(spec.port, 22);
        let args = ssh_args(&spec, Path::new("synthetic-known-hosts")).unwrap();
        assert!(args.iter().any(|s| s == "StrictHostKeyChecking=yes"));
        assert!(args.iter().any(|s| s == "PasswordAuthentication=no"));
        assert_eq!(args.last().map(String::as_str), Some("miochat-gui-v1"));
        assert_eq!(ConnectionSpec::default().host, "127.0.0.1");
    }
}
