"""Explicit MCP Events 2026-07-28 subset; no claim that mcp 1.29 implements it.

Host-managed delivery is accepted only in events/subscribe, never a model tool.
Owner enable/review/confirm/cancel are deliberately absent from this dispatcher.
"""
from datetime import datetime, timezone
import hashlib
import json
import time

from .contracts import Principal, ProductGateway, SubscriptionGrant
from .errors import AuthorizationError, InvalidInput, Unavailable, StorageInvariantError
from .webhook import CallbackVerifier, decode_secret

PROTOCOL_VERSION = "2026-07-28"
EVENT_NAME = "mio.message.created.v1"
TOOL_SCOPES = {
    "mio.get_profile": "mio.bindings.read",
    "mio.list_bindings": "mio.bindings.read",
    "mio.read_context": "mio.context.read",
    "mio.propose_reply": "mio.reply.draft",
    "mio.get_operation": "mio.operations.read",
}
FIELDS = {
    "mio.get_profile": ({}, set()),
    "mio.list_bindings": ({}, set()),
    "mio.read_context": ({"read_ref": str, "limit": int}, {"read_ref"}),
    "mio.propose_reply": ({"reply_ref": str, "content": str, "idempotency_key": str}, {"reply_ref", "content", "idempotency_key"}),
    "mio.get_operation": ({"operation_ref": str}, {"operation_ref"}),
}
DESCRIPTIONS = {
    "mio.get_profile": "Return the stable profile for this authenticated connection.",
    "mio.list_bindings": "List this owner's visible room-assistant bindings. Cannot enable them.",
    "mio.read_context": "Read the bounded original source message referenced by an accepted event; never current-screen context.",
    "mio.propose_reply": "Create an immutable owner-private draft for the original source. Does not publish. Exact owner review is always required.",
    "mio.get_operation": "Read this owner's operation status and durable receipt, without private draft content.",
}


def object_fields(value, allowed: set[str], required: set[str] = frozenset()):
    if type(value) is not dict or set(value) - allowed or not required.issubset(value):
        raise InvalidInput("INVALID_PARAMS")


def text_field(value, maximum=128):
    if type(value) is not str or not value or len(value) > maximum:
        raise InvalidInput("INVALID_PARAMS")
    return value


def require_scope(principal: Principal, scope: str):
    if scope not in principal.scopes:
        raise AuthorizationError("INSUFFICIENT_SCOPE")


def tool_schemas():
    result = []
    for name, (fields, required) in FIELDS.items():
        properties = {key: ({"type": "integer", "minimum": 1, "maximum": 20} if kind is int else
                            {"type": "string", "minLength": 1, "maxLength": 800 if key == "content" else 128})
                      for key, kind in fields.items()}
        tool = {"name": name, "description": DESCRIPTIONS[name],
                "inputSchema": {"type": "object", "properties": properties,
                                "required": sorted(required), "additionalProperties": False},
                "annotations": {"readOnlyHint": name != "mio.propose_reply", "destructiveHint": False,
                                "idempotentHint": True, "openWorldHint": False},
                "securitySchemes": [{"type": "oauth2", "scopes": [TOOL_SCOPES[name]]}]}
        if name == "mio.get_profile":
            tool["_meta"] = {"openai/profile": True}
            tool["outputSchema"] = {"type": "object", "properties": {
                "id": {"type": "string", "minLength": 1, "pattern": r"\S"},
                **{k: {"type": "string"} for k in ("name", "email", "nickname")}},
                "required": ["id"], "additionalProperties": False}
        result.append(tool)
    return result


def event_schema():
    return {"name": EVENT_NAME, "description": "A new human owner message explicitly mentions dot in this enabled original room; payload contains opaque source references only.",
            "delivery": ["webhook"], "inputSchema": {"type": "object", "properties": {
                "binding_ref": {"type": "string", "minLength": 1, "maxLength": 128}},
                "required": ["binding_ref"], "additionalProperties": False},
            "payloadSchema": {"type": "object", "properties": {"schema_version": {"const": 1},
                **{k: {"type": "string"} for k in ("binding_ref", "event_ref", "read_ref")}},
                "required": ["schema_version", "binding_ref", "event_ref", "read_ref"], "additionalProperties": False}}


def subscription_id(principal: Principal, url: str, binding_ref: str) -> str:
    identity = [principal.issuer, principal.subject, principal.tenant_id, principal.user_uid,
                url, EVENT_NAME, {"binding_ref": binding_ref}]
    return "sub_" + hashlib.sha256(json.dumps(identity, separators=(",", ":"), sort_keys=True).encode()).hexdigest()


class Dispatcher:
    def __init__(self, product: ProductGateway, callbacks: CallbackVerifier, *, enabled=False, clock=time.time):
        self.product, self.callbacks, self.enabled, self.clock = product, callbacks, enabled, clock

    def dispatch(self, principal: Principal, method: str, params: dict):
        if not self.enabled:
            raise Unavailable("FEATURE_DISABLED")
        if principal.expires_at <= self.clock():
            raise AuthorizationError("AUTHORIZATION_EXPIRED")
        if method == "server/discover":
            # The Events documentation specifies this response, not mcp 1.x initialize.
            object_fields(params, {"versions"})
            if "versions" in params and (type(params["versions"]) is not list or
                    any(type(x) is not str for x in params["versions"]) or PROTOCOL_VERSION not in params["versions"]):
                raise InvalidInput("UNSUPPORTED_PROTOCOL_VERSION")
            return {"resultType": "complete", "supportedVersions": [PROTOCOL_VERSION],
                    "capabilities": {"tools": {}, "events": {}}}
        if method == "tools/list":
            object_fields(params, {"cursor"})
            if params.get("cursor") is not None:
                raise InvalidInput("INVALID_CURSOR")
            return {"tools": [t for t in tool_schemas() if TOOL_SCOPES[t["name"]] in principal.scopes]}
        if method == "tools/call":
            object_fields(params, {"name", "arguments"}, {"name", "arguments"})
            name = params["name"]
            if type(name) is not str or name not in FIELDS:
                raise InvalidInput("UNKNOWN_TOOL")
            require_scope(principal, TOOL_SCOPES[name])
            arguments = params["arguments"]
            fields, required = FIELDS[name]
            object_fields(arguments, set(fields), required)
            for key, value in arguments.items():
                if type(value) is not fields[key]:
                    raise InvalidInput("INVALID_PARAMS")
                if type(value) is str:
                    text_field(value, 800 if key == "content" else 128)
            if "limit" in arguments and not 1 <= arguments["limit"] <= 20:
                raise InvalidInput("INVALID_PARAMS")
            if name == "mio.get_profile":
                result = self.product.get_profile(principal)
                object_fields(result, {"id", "name", "email", "nickname"}, {"id"})
                if (result["id"] != principal.profile_id or not result["id"].strip()
                        or any(type(v) is not str for v in result.values())):
                    raise StorageInvariantError("PROFILE_MAPPING_MISMATCH")
            elif name == "mio.list_bindings":
                result = self.product.list_bindings(principal)
            elif name == "mio.read_context":
                result = self.product.read_context(principal, arguments["read_ref"], arguments.get("limit", 20))
            elif name == "mio.propose_reply":
                result = self.product.propose_reply(principal, arguments["reply_ref"], arguments["content"], arguments["idempotency_key"])
            else:
                result = self.product.get_operation(principal, arguments["operation_ref"])
            return {"isError": False, "structuredContent": result,
                    "content": [{"type": "text", "text": json.dumps(result, ensure_ascii=False, separators=(",", ":"))}]}
        if method == "events/list":
            require_scope(principal, "mio.events.subscribe")
            object_fields(params, {"cursor"})
            if params.get("cursor") is not None:
                raise InvalidInput("INVALID_CURSOR")
            return {"events": [event_schema()] if self.product.can_discover_events(principal) else []}
        if method in {"events/subscribe", "events/unsubscribe"}:
            require_scope(principal, "mio.events.subscribe")
            subscribing = method == "events/subscribe"
            object_fields(params, {"name", "arguments", "delivery", "cursor", "ttlMs"} if subscribing else
                          {"name", "arguments", "delivery"}, {"name", "arguments", "delivery"})
            if params["name"] != EVENT_NAME or params.get("cursor") is not None:
                raise InvalidInput("INVALID_EVENT_OR_CURSOR")
            object_fields(params["arguments"], {"binding_ref"}, {"binding_ref"})
            binding_ref = text_field(params["arguments"]["binding_ref"])
            delivery = params["delivery"]
            object_fields(delivery, {"mode", "url", "secret"} if subscribing else {"mode", "url"},
                          {"mode", "url", "secret"} if subscribing else {"mode", "url"})
            if delivery["mode"] != "webhook":
                raise InvalidInput("UNSUPPORTED_DELIVERY")
            url = text_field(delivery["url"], 4096)
            sid = subscription_id(principal, url, binding_ref)
            if not subscribing:
                self.product.unsubscribe(principal, binding_ref, sid, hashlib.sha256(url.encode()).hexdigest())
                return {}
            self.callbacks.policy.parse(url)
            ttl = params.get("ttlMs")
            if ttl is not None and (type(ttl) is not int or not 1 <= ttl <= 2147483647):
                raise InvalidInput("INVALID_TTL")
            decode_secret(delivery["secret"])
            # Authorize before any callback I/O, then recheck under the commit tx.
            permit = self.product.authorize_subscription(principal, binding_ref)
            expires = min(principal.expires_at, int(self.clock()) + (900 if ttl is None else min(900, ttl // 1000)))
            if expires <= self.clock():
                raise InvalidInput("TTL_TOO_SHORT")
            self.callbacks.verify((principal.issuer, principal.subject, principal.tenant_id, principal.user_uid),
                                  sid, url, delivery["secret"], expires)
            self.product.commit_subscription(principal, permit, SubscriptionGrant(sid, EVENT_NAME, url, delivery["secret"], expires))
            return {"id": sid, "refreshBefore": datetime.fromtimestamp(expires, timezone.utc).isoformat().replace("+00:00", "Z"),
                    "cursor": None, "truncated": False}
        raise InvalidInput("METHOD_NOT_FOUND")

    def deliver_once(self, binding_ref: str, sender, *, stop=None):
        if not self.enabled:
            raise Unavailable("FEATURE_DISABLED")
        if stop is not None and stop.is_set():
            return {"status": "STOPPED"}
        lease = self.product.claim_delivery(binding_ref)
        if lease is None:
            return {"status": "IDLE_OR_IN_FLIGHT"}
        # claim_delivery has already committed mandatory SENDING intent. Any
        # failure from finish_delivery must propagate, not trigger another send.
        if stop is not None and stop.is_set():
            # Do not start a callback after observing shutdown. Conservatively
            # retire the durable claim without requeueing it. This is cooperative
            # stopping, not hard cancellation of an already-started send.
            return self.product.finish_delivery(lease.lease_id, "UNKNOWN")
        outcome = sender.send(lease)
        # Shutdown during send must not discard or replace its actual outcome.
        return self.product.finish_delivery(lease.lease_id, outcome)
