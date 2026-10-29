//! Room forms retain their original scope and never use the chat composer.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum RoomAction {
    Invite,
    Remove(String),
    Leave,
    Setting(String),
    Toggle { key: String, on: bool },
    Moderator { name: String, grant: bool },
}
impl RoomAction {
    pub fn needs_input(&self) -> bool {
        matches!(self, Self::Invite | Self::Setting(_))
    }
}
#[derive(Clone, Debug)]
pub struct RoomIntent {
    pub room: String,
    pub action: RoomAction,
}
impl RoomIntent {
    pub fn command(&self, current: &str, target: &str) -> Option<String> {
        if self.room != current || current.starts_with("dm:") || current.is_empty() {
            return None;
        }
        let token =
            |s: &str| !s.is_empty() && !s.chars().any(|c| c.is_whitespace() || c.is_control());
        match &self.action {
            RoomAction::Invite if token(target.trim()) => {
                Some(format!("/invite {}", target.trim()))
            }
            RoomAction::Remove(name) if token(name) => Some(format!("/remove {name}")),
            RoomAction::Leave => Some("/leave".into()),
            RoomAction::Setting(key) => {
                let value = target.trim();
                if value.chars().any(char::is_control) || value.chars().count() > 240 {
                    return None;
                }
                let valid = match key.as_str() {
                    "category" | "rules" | "announcement" => true,
                    "transfer" => token(value),
                    "slow" => value.parse::<u32>().is_ok_and(|n| n <= 3600),
                    _ => false,
                };
                valid.then(|| format!("/roomset {key} {value}"))
            }
            RoomAction::Toggle { key, on }
                if ["private", "readonly", "archive"].contains(&key.as_str()) =>
            {
                if key == "archive" {
                    Some(if *on { "/archive" } else { "/unarchive" }.into())
                } else {
                    Some(format!("/roomset {key} {}", if *on { "on" } else { "off" }))
                }
            }
            RoomAction::Moderator { name, grant } if token(name) => Some(format!(
                "{} {name}",
                if *grant { "/roommod" } else { "/unmod" }
            )),
            _ => None,
        }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn room_forms_cannot_cross_scopes_or_inject_commands() {
        let form = RoomIntent {
            room: "private-room".into(),
            action: RoomAction::Invite,
        };
        assert_eq!(
            form.command("private-room", " #123 "),
            Some("/invite #123".into())
        );
        assert_eq!(form.command("other", "alice"), None);
        assert_eq!(form.command("private-room", "alice\n/remove bob"), None);
        assert_eq!(form.command("private-room", ""), None);
        let remove = RoomIntent {
            room: "private-room".into(),
            action: RoomAction::Remove("alice".into()),
        };
        assert_eq!(
            remove.command("private-room", "ignored"),
            Some("/remove alice".into())
        );
        assert_eq!(remove.command("lobby", "alice"), None);
        let leave = RoomIntent {
            room: "dm:alice".into(),
            action: RoomAction::Leave,
        };
        assert_eq!(leave.command("dm:alice", ""), None);
    }
    #[test]
    fn settings_and_role_confirmation_bind_scope_and_validate_values() {
        let mut f = RoomIntent {
            room: "private-room".into(),
            action: RoomAction::Setting("rules".into()),
        };
        assert_eq!(
            f.command("private-room", ""),
            Some("/roomset rules ".into())
        );
        assert_eq!(f.command("private-room", "rule\n/archive"), None);
        assert_eq!(f.command("other", "safe"), None);
        f.action = RoomAction::Setting("transfer".into());
        assert_eq!(
            f.command("private-room", "#2"),
            Some("/roomset transfer #2".into())
        );
        assert_eq!(f.command("private-room", "a b"), None);
        f.action = RoomAction::Setting("slow".into());
        assert_eq!(f.command("private-room", "3601"), None);
        f.action = RoomAction::Toggle {
            key: "archive".into(),
            on: true,
        };
        assert_eq!(f.command("private-room", ""), Some("/archive".into()));
    }
}
