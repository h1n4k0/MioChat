//! Connection admission only. Never owns an application password or queues login.
use crate::connection::HostKeyProbe;

#[derive(Default, Debug)]
pub struct LoginGate {
    generation: Option<u64>,
    hello: bool,
    pending: Option<String>,
}
impl LoginGate {
    pub fn begin(&mut self, generation: u64) {
        self.generation = Some(generation);
        self.hello = false;
        self.pending = None;
    }
    pub fn clear(&mut self) {
        *self = Self::default();
    }
    pub fn accept_hello(&mut self, generation: u64, protocol: u32) -> bool {
        if self.generation != Some(generation) || self.hello {
            return false;
        }
        if protocol != 1 {
            self.clear();
            return false;
        }
        self.hello = true;
        true
    }
    pub fn ready(&self, generation: u64) -> bool {
        self.generation == Some(generation) && self.hello
    }
    pub fn can_submit(&self, generation: u64) -> bool {
        self.ready(generation) && self.pending.is_none()
    }
    pub fn submit(&mut self, generation: u64, request_id: &str) -> bool {
        if !self.can_submit(generation) || request_id.is_empty() {
            return false;
        }
        self.pending = Some(request_id.to_owned());
        true
    }
    pub fn finish(&mut self, request_id: &str) -> bool {
        if self.pending.as_deref() != Some(request_id) {
            return false;
        }
        self.pending = None;
        true
    }
}

/// Consumes the startup attempt, even for explicit local preview modes.
pub fn take_startup_attempt(pending: &mut bool, mock: bool, fixture: bool) -> bool {
    std::mem::take(pending) && !mock && !fixture
}

pub fn trust_confirmation_matches(
    generation: u64,
    shown_generation: u64,
    current: Option<&HostKeyProbe>,
    shown: &HostKeyProbe,
) -> bool {
    generation == shown_generation && current == Some(shown)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn login_never_submits_before_current_compatible_hello() {
        let mut gate = LoginGate::default();
        assert!(!gate.submit(1, "typed-before-connect"));
        gate.begin(1);
        assert!(!gate.submit(1, "typed-before-hello"));
        assert!(!gate.accept_hello(0, 1));
        assert!(!gate.ready(1));
        assert!(gate.accept_hello(1, 1));
        assert!(!gate.accept_hello(1, 1));
        assert!(gate.submit(1, "explicit-login"));
        assert!(!gate.submit(1, "double-click"));
        assert!(!gate.finish("late-old-id"));
        assert!(!gate.can_submit(1));
        assert!(gate.finish("explicit-login"));
        assert!(gate.can_submit(1));
    }
    #[test]
    fn cancel_reconnect_and_unsupported_hello_never_replay_login() {
        let mut gate = LoginGate::default();
        gate.begin(1);
        assert!(gate.accept_hello(1, 1));
        assert!(gate.submit(1, "old-login"));
        gate.clear();
        assert!(!gate.accept_hello(1, 1));
        assert!(!gate.finish("old-login"));
        gate.begin(2);
        assert!(!gate.accept_hello(1, 1));
        assert!(!gate.accept_hello(2, 9));
        assert!(!gate.submit(2, "unsupported"));
        gate.begin(3);
        assert!(gate.accept_hello(3, 1));
        assert!(gate.can_submit(3));
        assert!(!gate.finish("old-login"));
        assert!(gate.submit(3, "new-explicit-click"));
    }
    #[test]
    fn automatic_connection_is_once_and_never_for_mock_or_fixture() {
        for (mock, fixture) in [(false, false), (true, false), (false, true), (true, true)] {
            let mut pending = true;
            assert_eq!(
                take_startup_attempt(&mut pending, mock, fixture),
                !mock && !fixture
            );
            assert!(!take_startup_attempt(&mut pending, false, false));
        }
    }
    #[test]
    fn stale_trust_button_cannot_confirm_another_key_or_connection() {
        let a = HostKeyProbe {
            line: "host key A".into(),
            fingerprint: "A".into(),
        };
        let b = HostKeyProbe {
            line: "host key B".into(),
            fingerprint: "B".into(),
        };
        assert!(trust_confirmation_matches(4, 4, Some(&a), &a));
        assert!(!trust_confirmation_matches(5, 4, Some(&a), &a));
        assert!(!trust_confirmation_matches(4, 4, Some(&b), &a));
        assert!(!trust_confirmation_matches(4, 4, None, &a));
    }
}
