"""Concrete private-pipe client for product-overlay/dot_bridge.go.

No subprocess is launched. A separately approved supervisor supplies connected
private binary pipes; never connect these to an untrusted client or public port.
No live factory creates a process, account mapping, key or subscription here.
"""
import base64
import hashlib
import json
import threading
import uuid

from .contracts import Account, DeliveryLease, SubscriptionPermit
from .errors import AuthorizationError, StorageInvariantError, Unavailable


def _strict_pairs(items):
    result = {}
    for key, value in items:
        if key in result:
            raise ValueError("duplicate product key")
        result[key] = value
    return result


class PrivatePipeTransport:
    """One request at a time; uncertain I/O permanently breaks this connection.

    Deadline/termination is owned by the deployment supervisor: these are
    inherited local pipes, not sockets to arbitrary caller-specified addresses.
    """
    def __init__(self, reader, writer):
        if reader is None or writer is None:
            raise Unavailable("PRODUCT_TRANSPORT_UNCONFIGURED")
        self.reader, self.writer = reader, writer
        self._lock = threading.Lock()
        self._broken = False

    def exchange(self, method, identity=None, **arguments):
        with self._lock:
            if self._broken:
                raise Unavailable("PRODUCT_CONNECTION_UNCERTAIN")
            request_id = uuid.uuid4().hex
            request = {"id": request_id, "method": method, "identity": identity or {}, **arguments}
            body = json.dumps(request, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode() + b"\n"
            if len(body) > 65536:
                raise Unavailable("PRODUCT_REQUEST_LIMIT")
            try:
                count = self.writer.write(body)
                if count != len(body):
                    raise OSError("short write")
                self.writer.flush()
                response = self.reader.readline(131073)
                if not response.endswith(b"\n") or len(response) > 131072:
                    raise ValueError("bounded response required")
                reply = json.loads(response, object_pairs_hook=_strict_pairs)
                if type(reply) is not dict or set(reply) - {"id", "result", "error"} or reply.get("id") != request_id:
                    raise ValueError("product response mismatch")
            except Exception:
                self._broken = True
                raise Unavailable("PRODUCT_CONNECTION_UNCERTAIN") from None
            if reply.get("error"):
                raise AuthorizationError("PRODUCT_DENIED_OR_UNCERTAIN")
            return reply.get("result")


def identity(principal):
    return {"issuer": principal.issuer, "subject": principal.subject,
            "issued_at": principal.issued_at, "expires": principal.expires_at}


class MioIdentityDirectory:
    def __init__(self, transport):
        self.transport = transport

    def resolve(self, issuer, subject):
        try:
            data = self.transport.exchange("identity.resolve", {"issuer": issuer, "subject": subject})
        except AuthorizationError:
            return None
        required = {"tenant_id", "user_uid", "profile_id", "auth_epoch", "tokens_valid_after", "active"}
        if type(data) is not dict or set(data) != required:
            raise Unavailable("PRODUCT_IDENTITY_MAPPING_INVALID")
        return Account(**data)


class MioProductGateway:
    def __init__(self, transport, vault):
        self.transport, self.vault = transport, vault

    def get_profile(self, p): return self.transport.exchange("profile", identity(p))
    def list_bindings(self, p): return self.transport.exchange("bindings", identity(p))
    def read_context(self, p, read_ref, limit):
        # Current Mio v1 exposes only the trigger message, never a made-up thread.
        return self.transport.exchange("context", identity(p), read_ref=read_ref)
    def propose_reply(self, p, reply_ref, content, idempotency_key):
        return self.transport.exchange("propose", identity(p), reply_ref=reply_ref, content=content, idempotency_key=idempotency_key)
    def get_operation(self, p, operation_ref):
        return self.transport.exchange("operation", identity(p), operation_ref=operation_ref)
    def can_discover_events(self, p): return bool(self.list_bindings(p)["bindings"])

    def authorize_subscription(self, p, binding_ref):
        data = self.transport.exchange("subscription.prepare", identity(p), binding_ref=binding_ref)
        if data.get("binding_ref") != binding_ref or data.get("owner_uid") != int(p.user_uid):
            raise StorageInvariantError("SUBSCRIPTION_OWNER_MISMATCH")
        return SubscriptionPermit(binding_ref, data["generation"], data["owner_auth_epoch"],
                                  data["room_acl_epoch"], data["room_acl_epoch"], data.get("previous_delivery_handle"))

    def commit_subscription(self, p, permit, grant):
        # Persist encrypted delivery material first. A failed/uncertain product
        # commit leaves only an inaccessible orphan, never an active subscription.
        handle = self.vault.put(p, permit, grant)
        self.transport.exchange("subscription.commit", identity(p), subscription={
            "id": grant.subscription_id, "binding_ref": permit.binding_ref,
            "callback_hash": hashlib.sha256(grant.callback_url.encode()).hexdigest(),
            "delivery_handle": handle, "generation": permit.binding_generation, "expires": grant.expires_at})

    def unsubscribe(self, p, binding_ref, subscription_id, callback_hash):
        # Uses the original host-supplied URL digest, not vault availability.
        # Thus repeated stops of an absent subscription remain idempotent even
        # after vault cleanup; the product still authorizes exact owner/identity.
        self.transport.exchange("subscription.stop", identity(p), binding_ref=binding_ref,
                                subscription_id=subscription_id, callback_hash=callback_hash)

    def claim_delivery(self, binding_ref):
        data = self.transport.exchange("delivery.claim", binding_ref=binding_ref)
        if data is None:
            return None
        try:
            target = self.vault.get(data["delivery_handle"], data["subscription_id"])
            body = base64.b64decode(data["body"], validate=True)
            payload = json.loads(body, object_pairs_hook=_strict_pairs)
            if (set(payload) != {"eventId", "name", "timestamp", "data", "cursor"}
                    or payload["eventId"] != data["event_id"] or payload["name"] != "mio.message.created.v1"
                    or set(payload["data"]) != {"schema_version", "binding_ref", "event_ref", "read_ref"}
                    or payload["data"]["binding_ref"] != binding_ref or payload["data"]["event_ref"] != data["event_id"]
                    or payload["cursor"] is not None or len(body) > 262144):
                raise ValueError()
            return DeliveryLease(data["lease_id"], data["event_id"], data["subscription_id"],
                                 target["url"], tuple(target["secrets"]), body)
        except Exception:
            # Do not attempt to send when stored delivery material cannot be read.
            self.transport.exchange("delivery.finish", lease_id=data["lease_id"], outcome="UNKNOWN")
            raise Unavailable("DELIVERY_MATERIAL_UNAVAILABLE") from None

    def finish_delivery(self, lease_id, observed_outcome):
        return self.transport.exchange("delivery.finish", lease_id=lease_id, outcome=observed_outcome)

    def pending_bindings(self):
        return self.transport.exchange("delivery.bindings")

    def recover_interrupted(self):
        return self.transport.exchange("delivery.recover")
