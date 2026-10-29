//! Private owner review protocol. Correlation fields never grant authority.
use serde::{Deserialize, Serialize};
#[derive(Clone, Default, Serialize, PartialEq, Eq)]
pub struct Data {
    pub operation: String,
    pub room: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub binding_ref: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub operation_ref: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub expected_generation: Option<i64>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub review_token: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub body_sha256: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub audience_sha256: String,
    #[serde(skip_serializing_if = "is_zero")]
    pub source_id: i64,
    pub owner_origin: String,
    pub edit_generation: u64,
    pub confirmed: bool,
}
fn is_zero(v: &i64) -> bool {
    *v == 0
}
#[derive(Clone, Default, Deserialize)]
pub struct Binding {
    pub binding_ref: String,
    pub room: String,
    pub status: String,
    #[serde(default)]
    pub reason: String,
    pub generation: i64,
    pub owner_uid: i64,
}
#[derive(Clone, Default, Deserialize)]
pub struct Operation {
    pub operation_ref: String,
    pub binding_ref: String,
    pub status: String,
    pub source_id: i64,
    #[serde(default)]
    pub message_id: i64,
}
#[derive(Clone, Default, Deserialize)]
pub struct Review {
    #[serde(flatten)]
    pub operation: Operation,
    pub room: String,
    pub body: String,
    pub body_sha256: String,
    pub audience_sha256: String,
    pub audience_label: String,
    pub audience_uids: Vec<i64>,
    pub generation: i64,
    pub owner_uid: i64,
    pub review_token: String,
    pub expires: i64,
}
#[derive(Clone, Default, Deserialize)]
pub struct Receipt {
    pub operation: String,
    pub owner_origin: String,
    pub edit_generation: u64,
    #[serde(default)]
    pub bindings: Vec<Binding>,
    pub binding: Option<Binding>,
    #[serde(default)]
    pub pending: Vec<Operation>,
    pub review: Option<Review>,
    pub result: Option<Operation>,
}
impl std::fmt::Debug for Receipt {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("DotReceipt")
            .field("private_payload", &"[redacted]")
            .finish()
    }
}
pub fn exclusive(r: &crate::session::Response) -> bool {
    let mut c = r.clone();
    c.dot = None;
    c.kind = "error".into();
    r.kind == "dot"
        && r.protocol == 0
        && r.user.is_empty()
        && r.registration_mode.is_empty()
        && r.capabilities.is_empty()
        && r.status.is_empty()
        && r.error.is_empty()
        && r.online == 0
        && r.reply_to == 0
        && !r.at_latest
        && crate::information::is_plain_error_response(&c)
}
pub fn matches(d: &Data, r: &Receipt) -> bool {
    receipt_shape(r)
        && d.operation == r.operation
        && d.owner_origin == r.owner_origin
        && d.edit_generation == r.edit_generation
        && r.bindings
            .iter()
            .all(|b| b.room == d.room && b.owner_uid > 0)
        && r.binding.as_ref().is_none_or(|b| {
            b.room == d.room
                && b.owner_uid > 0
                && (d.binding_ref.is_empty() || b.binding_ref == d.binding_ref)
        })
        && r.review.as_ref().is_none_or(|v| {
            v.room == d.room
                && v.owner_uid > 0
                && v.operation.operation_ref == d.operation_ref
                && v.operation.source_id > 0
                && !v.review_token.is_empty()
                && v.review_token.len() <= 256
                && v.body.chars().count() <= 800
                && crate::attachments::sha256_hex(v.body.as_bytes()) == v.body_sha256
                && v.audience_uids.iter().all(|uid| *uid > 0)
                && v.expires > 0
        })
        && r.result
            .as_ref()
            .is_none_or(|v| d.operation_ref.is_empty() || v.operation_ref == d.operation_ref)
}
fn receipt_shape(r: &Receipt) -> bool {
    let empty_lists = r.bindings.is_empty() && r.pending.is_empty();
    match r.operation.as_str() {
        "list" => {
            r.binding.is_none()
                && r.review.is_none()
                && r.result.is_none()
                && r.pending.is_empty()
                && r.bindings.len() <= 100
        }
        "create" | "enable" | "pause" => {
            empty_lists && r.binding.is_some() && r.review.is_none() && r.result.is_none()
        }
        "pending" => {
            r.binding.is_none()
                && r.review.is_none()
                && r.result.is_none()
                && r.bindings.is_empty()
                && r.pending.len() <= 100
        }
        "review" => {
            empty_lists && r.binding.is_none() && (r.review.is_some() != r.result.is_some())
        }
        "confirm" | "cancel" | "status" => {
            empty_lists && r.binding.is_none() && r.review.is_none() && r.result.is_some()
        }
        _ => false,
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn dot_private_receipt_is_exclusive_and_correlated() {
        let d = Data {
            operation: "list".into(),
            room: "r".into(),
            owner_origin: "o".into(),
            edit_generation: 1,
            operation_ref: "p".into(),
            ..Default::default()
        };
        let mut r = Receipt {
            operation: d.operation.clone(),
            owner_origin: d.owner_origin.clone(),
            edit_generation: 1,
            ..Default::default()
        };
        assert!(matches(&d, &r));
        r.edit_generation = 2;
        assert!(!matches(&d, &r));
        let mut frame = crate::session::Response {
            kind: "dot".into(),
            dot: Some(r),
            ..Default::default()
        };
        assert!(exclusive(&frame));
        frame.preferences = Some(crate::preferences::Preferences::default());
        assert!(!exclusive(&frame));
    }
    #[test]
    fn dot_debug_never_contains_private_body_or_token() {
        let r = Receipt {
            review: Some(Review {
                body: "synthetic-private".into(),
                review_token: "synthetic-token".into(),
                ..Default::default()
            }),
            ..Default::default()
        };
        let text = format!("{r:?}");
        assert!(!text.contains("synthetic"));
    }
}
