"""The product-side trusted boundary, not model-facing JSON or authentication.

All returned identities/permissions must come from authoritative Mio persistence.
No implementation is selected by default. See PRODUCT_CONTRACT.md for transaction
requirements; implementing Protocol methods alone is not a conformance proof.
"""
from dataclasses import dataclass, field
from typing import Any, Protocol


@dataclass(frozen=True)
class Account:
    tenant_id: str
    user_uid: str
    profile_id: str
    auth_epoch: int
    tokens_valid_after: int
    active: bool


@dataclass(frozen=True)
class Principal:
    issuer: str
    subject: str
    tenant_id: str
    user_uid: str
    profile_id: str
    auth_epoch: int
    scopes: frozenset[str]
    expires_at: int
    issued_at: int


class IdentityDirectory(Protocol):
    def resolve(self, issuer: str, subject: str) -> Account | None:
        """Fresh lookup; exact stable association only, never names/email or JIT linking."""
        ...


@dataclass(frozen=True)
class SubscriptionPermit:
    binding_ref: str
    binding_generation: int
    owner_auth_epoch: int
    room_acl_epoch: int
    thread_acl_epoch: int
    previous_delivery_handle: str | None = None


@dataclass(frozen=True)
class SubscriptionGrant:
    subscription_id: str
    name: str
    callback_url: str = field(repr=False)
    signing_secret: str = field(repr=False)
    expires_at: int


@dataclass(frozen=True)
class DeliveryLease:
    """Already-durable SENDING intent. Product generated exact event body bytes.

    A lease is returned only after mandatory writes and post-write same-transaction
    readback, including source, ACL, epoch and no-loop checks. Single-flight is
    enforced in product persistence, not by a Python in-memory mutex.
    """
    lease_id: str
    event_id: str
    subscription_id: str
    callback_url: str = field(repr=False)
    signing_secrets: tuple[str, ...] = field(repr=False)
    body: bytes = field(repr=False)


class ProductGateway(Protocol):
    def get_profile(self, principal: Principal) -> dict[str, str]: ...
    def list_bindings(self, principal: Principal) -> dict[str, Any]: ...
    def read_context(self, principal: Principal, read_ref: str, limit: int) -> dict[str, Any]: ...
    def propose_reply(self, principal: Principal, reply_ref: str, content: str,
                      idempotency_key: str) -> dict[str, Any]: ...
    def get_operation(self, principal: Principal, operation_ref: str) -> dict[str, Any]: ...
    def can_discover_events(self, principal: Principal) -> bool: ...
    def authorize_subscription(self, principal: Principal, binding_ref: str) -> SubscriptionPermit:
        """Require owner-enabled, live ACL/identity, and no expired/paused resurrection."""
        ...
    def commit_subscription(self, principal: Principal, permit: SubscriptionPermit,
                            grant: SubscriptionGrant) -> None:
        """Recheck all permit epochs in one tx; persist encrypted secret + subscription.

        A replacement secret gets a short, bounded dual-signing rotation window.
        URL changes require explicit owner pause/enable; refresh cannot resume.
        Mandatory writes/readback/rollback apply. Never return before durable commit.
        """
        ...
    def unsubscribe(self, principal: Principal, binding_ref: str,
                    subscription_id: str, callback_hash: str) -> None:
        """Authorize original principal+URL+filter identity; idempotently pause matching
        subscription, invalidate refs/drafts, and never affect a different URL.
        """
        ...
    def pending_bindings(self) -> list[str]: ...
    def claim_delivery(self, binding_ref: str) -> DeliveryLease | None: ...
    def finish_delivery(self, lease_id: str, observed_outcome: str) -> dict[str, Any]:
        """Persist outcome with rechecked ACL; preserve invalidation and receipt separately.
        UNKNOWN never requeues. Transient retries retain the exact event ID/body.
        """
        ...
