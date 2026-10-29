use serde::Deserialize;
use std::collections::BTreeMap;

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Eq)]
pub struct RoomPreferences {
    pub pinned: bool,
    pub subscribed: bool,
    pub muted: bool,
    pub unread: usize,
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct Preferences {
    #[serde(default)]
    pub rooms: BTreeMap<String, RoomPreferences>,
    #[serde(default)]
    pub blocked: Vec<String>,
    pub user: String,
    pub theme: String,
    pub settings: BTreeMap<String, String>,
    pub key_count: usize,
}
impl Preferences {
    pub fn blocks(&self, name: &str) -> bool {
        self.blocked.iter().any(|s| s == name)
    }
    pub fn belongs_to(&self, user: &str) -> bool {
        !user.is_empty() && self.user == user
    }
    pub fn on(&self, key: &str) -> bool {
        self.settings.get(key).is_some_and(|v| v == "on")
    }
    pub fn alerts(&self) -> bool {
        !self.on("dnd")
    }
    pub fn reminder(&self) -> bool {
        self.key_count == 0 && self.settings.get("key_reminder").is_none_or(|v| v != "off")
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn account_preferences_are_scoped_and_reminders_follow_original_gates() {
        let mut p = Preferences {
            user: "alice".into(),
            ..Default::default()
        };
        assert!(p.belongs_to("alice"));
        assert!(!p.belongs_to("bob"));
        assert!(!p.belongs_to(""));
        assert!(p.alerts());
        assert!(p.reminder());
        p.settings.insert("dnd".into(), "on".into());
        assert!(!p.alerts());
        p.settings.insert("key_reminder".into(), "off".into());
        assert!(!p.reminder());
        p.settings.insert("key_reminder".into(), "on".into());
        p.key_count = 1;
        assert!(!p.reminder());
    }
}
