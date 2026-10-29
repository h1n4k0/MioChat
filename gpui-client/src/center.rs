use serde::{Deserialize, Deserializer};

fn null_list<'de, D, T>(d: D) -> Result<Vec<T>, D::Error>
where
    D: Deserializer<'de>,
    T: Deserialize<'de>,
{
    Ok(Option::<Vec<T>>::deserialize(d)?.unwrap_or_default())
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Contact {
    pub name: String,
    pub group: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Notice {
    pub id: i64,
    pub at: i64,
    pub kind: String,
    pub actor: String,
    pub body: String,
    pub seen: bool,
    pub command: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Center {
    pub user: String,
    pub section: String,
    #[serde(default, deserialize_with = "null_list")]
    pub contacts: Vec<Contact>,
    #[serde(default, deserialize_with = "null_list")]
    pub notices: Vec<Notice>,
    pub unread: usize,
}
impl Center {
    pub fn belongs_to(&self, user: &str) -> bool {
        !user.is_empty() && self.user == user
    }
    pub fn contacts_for(&self, section: &str) -> Vec<Contact> {
        self.contacts
            .iter()
            .filter(|c| !matches!(section, "friends" | "following") || c.group == section)
            .cloned()
            .collect()
    }
    pub fn actions(&self, section: &str) -> Vec<String> {
        if section == "notifications" {
            let mut a = vec!["/notifyclear".into()];
            a.extend(
                self.notices
                    .iter()
                    .filter(|n| !n.command.is_empty())
                    .map(|n| n.command.clone()),
            );
            return a;
        }
        self.contacts_for(section)
            .iter()
            .flat_map(|c| {
                if c.group == "requests" {
                    vec![
                        format!("/accept {}", c.name),
                        format!("/decline {}", c.name),
                    ]
                } else {
                    vec![format!("/profile {}", c.name)]
                }
            })
            .collect()
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn empty_account_wire_accepts_null_or_omitted_lists() {
        for json in [
            r#"{"user":"alice","section":"all","contacts":null,"notices":null,"unread":0}"#,
            r#"{"user":"alice","section":"all","unread":0}"#,
        ] {
            let c: Center = serde_json::from_str(json).unwrap();
            assert!(c.contacts.is_empty() && c.notices.is_empty());
        }
        assert!(
            serde_json::from_str::<Center>(
                r#"{"user":"alice","section":"all","contacts":"bad","unread":0}"#
            )
            .is_err()
        );
    }
    #[test]
    fn owner_scoping_and_inaccessible_notice_actions() {
        let c = Center {
            user: "alice".into(),
            contacts: vec![
                Contact {
                    name: "bob".into(),
                    group: "requests".into(),
                },
                Contact {
                    name: "carol".into(),
                    group: "friends".into(),
                },
            ],
            notices: vec![
                Notice {
                    command: String::new(),
                    ..Default::default()
                },
                Notice {
                    command: "/jump 12".into(),
                    ..Default::default()
                },
            ],
            ..Default::default()
        };
        assert!(c.belongs_to("alice"));
        assert!(!c.belongs_to("bob"));
        assert_eq!(c.actions("friends"), vec!["/profile carol"]);
        assert_eq!(
            c.actions("contacts"),
            vec!["/accept bob", "/decline bob", "/profile carol"]
        );
        assert_eq!(c.actions("notifications"), vec!["/notifyclear", "/jump 12"]);
    }
}
