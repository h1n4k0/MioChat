//! Pure close policy: auxiliary windows never own application lifetime.
//! Register the primary window exactly once. Its closure requests application
//! shutdown once; startup, unknown and repeated auxiliary closures are harmless.

#[derive(Debug)]
pub struct WindowLifecycle<Id> {
    primary: Option<Id>,
    quit_requested: bool,
}

impl<Id> Default for WindowLifecycle<Id> {
    fn default() -> Self {
        Self {
            primary: None,
            quit_requested: false,
        }
    }
}

impl<Id: Copy + Eq> WindowLifecycle<Id> {
    pub fn register_primary(&mut self, id: Id) -> bool {
        if self.primary.is_some() || self.quit_requested {
            return false;
        }
        self.primary = Some(id);
        true
    }

    pub fn closed(&mut self, id: Id) -> bool {
        if self.primary != Some(id) || self.quit_requested {
            return false;
        }
        self.primary = None;
        self.quit_requested = true;
        true
    }
}

#[cfg(test)]
mod tests {
    use super::WindowLifecycle;

    #[test]
    fn closing_contrast_fixture_preserves_primary() {
        let mut policy = WindowLifecycle::default();
        assert!(policy.register_primary(7_u64));
        assert!(!policy.closed(8));
        assert!(!policy.closed(8));
        assert!(policy.closed(7));
    }

    #[test]
    fn startup_and_unknown_window_closures_do_not_quit() {
        let mut policy = WindowLifecycle::default();
        assert!(!policy.closed(1_u64));
        assert!(policy.register_primary(7));
        assert!(!policy.closed(999));
        assert!(policy.closed(7));
    }

    #[test]
    fn primary_requests_shutdown_once_and_cannot_be_rebound() {
        let mut policy = WindowLifecycle::default();
        assert!(policy.register_primary(7_u64));
        assert!(!policy.register_primary(8));
        assert!(policy.closed(7));
        assert!(!policy.closed(7));
        assert!(!policy.closed(8));
        assert!(!policy.register_primary(9));
    }
}
