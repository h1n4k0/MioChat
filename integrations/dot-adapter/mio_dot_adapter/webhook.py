"""Standard Webhooks signing and bounded, DNS-pinned HTTPS callbacks.

No requests are made at import/startup. An explicitly configured exact host
allowlist is required in addition to public-address checks. No URL, secret or
application body is logged. Tests substitute the transport, not auth booleans.
"""
from dataclasses import dataclass, field
import base64
import hashlib
import hmac
import http.client
import ipaddress
import json
import secrets
import socket
import ssl
import threading
import time
from urllib.parse import urlsplit

from .errors import CallbackError, InvalidInput, Unavailable


MAX_BODY = 262144
MAX_RESPONSE = 16384


def decode_secret(secret: str) -> bytes:
    try:
        if type(secret) is not str or not secret.startswith("whsec_") or len(secret) > 100:
            raise ValueError()
        key = base64.b64decode(secret[6:], validate=True)
        if not 24 <= len(key) <= 64:
            raise ValueError()
        return key
    except (ValueError, TypeError):
        raise InvalidInput("INVALID_SIGNING_SECRET") from None


def signed_headers(event_id: str, subscription_id: str, body: bytes,
                   signing_secrets: tuple[str, ...], timestamp: int) -> dict[str, str]:
    if (type(body) is not bytes or len(body) > MAX_BODY or not 1 <= len(signing_secrets) <= 2
            or type(timestamp) is not int or timestamp < 0):
        raise InvalidInput("INVALID_EVENT")
    for value in (event_id, subscription_id):
        if type(value) is not str or not value or len(value) > 256 or any(ord(c) < 33 or ord(c) > 126 for c in value):
            raise InvalidInput("INVALID_EVENT")
    message = event_id.encode("ascii") + b"." + str(timestamp).encode("ascii") + b"." + body
    signatures = ["v1," + base64.b64encode(hmac.new(decode_secret(key), message, hashlib.sha256).digest()).decode("ascii")
                  for key in signing_secrets]
    return {"Content-Type": "application/json", "webhook-id": event_id,
            "webhook-timestamp": str(timestamp), "webhook-signature": " ".join(signatures),
            "X-MCP-Subscription-Id": subscription_id}


@dataclass(frozen=True)
class CallbackTarget:
    host: str
    path: str = field(repr=False)
    url: str = field(repr=False)


@dataclass(frozen=True)
class CallbackPolicy:
    allowed_hosts: frozenset[str] = frozenset()
    timeout_seconds: float = 10.0

    def parse(self, url: str) -> CallbackTarget:
        if not self.allowed_hosts:
            raise Unavailable("CALLBACK_HOST_CONFIGURATION_REQUIRED")
        try:
            if (type(url) is not str or not 1 <= len(url) <= 4096 or not url.isascii()
                    or any(ord(c) <= 32 or ord(c) >= 127 for c in url) or "\\" in url):
                raise ValueError()
            parts = urlsplit(url)
            if (parts.scheme != "https" or not parts.hostname or parts.username is not None
                    or parts.password is not None or parts.fragment or parts.port not in (None, 443)
                    or parts.hostname not in self.allowed_hosts or parts.hostname.endswith(".")
                    or parts.hostname.lower() != parts.hostname or "%" in parts.netloc):
                raise ValueError()
            # Literal addresses are not an approved host identity; even public ones.
            try:
                ipaddress.ip_address(parts.hostname)
            except ValueError:
                pass
            else:
                raise ValueError()
            if any(not label or not label[0].isalnum() or not label[-1].isalnum()
                   or any(not (c.isalnum() or c == "-") for c in label)
                   for label in parts.hostname.split(".")):
                raise ValueError()
            path = parts.path or "/"
            if parts.query:
                path += "?" + parts.query
            return CallbackTarget(parts.hostname, path, url)
        except (ValueError, TypeError):
            raise CallbackError("invalid_url") from None


def public_addresses(host: str, resolver=socket.getaddrinfo) -> tuple[tuple[int, str], ...]:
    """Reject the whole answer set if any address is non-public (including mapped v6)."""
    try:
        answers = resolver(host, 443, type=socket.SOCK_STREAM, proto=socket.IPPROTO_TCP)
        found = set()
        for family, socktype, proto, _, addr in answers:
            if family not in (socket.AF_INET, socket.AF_INET6) or socktype != socket.SOCK_STREAM:
                raise ValueError()
            ip = ipaddress.ip_address(addr[0])
            if (not ip.is_global or ip.is_multicast or ip.is_unspecified or ip.is_reserved
                    or ip.is_loopback or ip.is_link_local or getattr(ip, "ipv4_mapped", None)
                    or getattr(ip, "sixtofour", None) or getattr(ip, "teredo", None)):
                raise ValueError()
            found.add((family, str(ip)))
        if not found:
            raise ValueError()
        return tuple(sorted(found))
    except (OSError, ValueError, TypeError):
        raise CallbackError("address_rejected") from None


class _PinnedHTTPSConnection(http.client.HTTPSConnection):
    def __init__(self, host: str, family: int, address: str, timeout: float):
        super().__init__(host, 443, timeout=timeout, context=ssl.create_default_context())
        self._family, self._address = family, address

    def connect(self):
        # Do not re-resolve the hostname or let proxy environment variables apply.
        raw = socket.socket(self._family, socket.SOCK_STREAM)
        raw.settimeout(self.timeout)
        try:
            dest = (self._address, 443, 0, 0) if self._family == socket.AF_INET6 else (self._address, 443)
            raw.connect(dest)
            self.sock = self._context.wrap_socket(raw, server_hostname=self.host)
        except BaseException:
            raw.close()
            raise


class PublicHTTPS:
    """One connection only; never retries internally or follows redirects."""
    def __init__(self, policy: CallbackPolicy):
        if not 0 < policy.timeout_seconds <= 10:
            raise Unavailable("INVALID_CALLBACK_TIMEOUT")
        self.policy = policy

    def post(self, url: str, body: bytes, headers: dict[str, str]) -> tuple[int, bytes]:
        target = self.policy.parse(url)
        if len(body) > MAX_BODY:
            raise CallbackError("payload_too_large")
        # Validate ALL DNS answers at every connection, then dial one pinned IP.
        addresses = public_addresses(target.host)
        family, address = addresses[0]
        connection = _PinnedHTTPSConnection(target.host, family, address, self.policy.timeout_seconds)
        try:
            connection.request("POST", target.path, body=body, headers=headers)
            response = connection.getresponse()
            result = response.read(MAX_RESPONSE + 1)
            if len(result) > MAX_RESPONSE:
                raise CallbackError("response_too_large")
            return response.status, result
        except (OSError, http.client.HTTPException):
            # Could have reached receiver. This is not proof of safe-to-retry failure.
            raise CallbackError("transport_unknown") from None
        finally:
            connection.close()


class CallbackVerifier:
    def __init__(self, transport, policy: CallbackPolicy, clock=time.time, cache_seconds=60):
        if not 0 <= cache_seconds <= 300:
            raise ValueError("cache_seconds")
        self.transport, self.policy, self.clock = transport, policy, clock
        self.cache_seconds = cache_seconds
        self._cache = {}
        self._lock = threading.Lock()

    def verify(self, principal_key: tuple[str, ...], subscription_id: str, url: str,
               signing_secret: str, expires_at: int) -> None:
        self.policy.parse(url)
        key = decode_secret(signing_secret)
        now = int(self.clock())
        if expires_at <= now:
            raise CallbackError("subscription_expired")
        cache_key = (principal_key, url, hashlib.sha256(key).digest())
        with self._lock:
            self._cache = {k: expiry for k, expiry in self._cache.items() if expiry > now}
            if self._cache.get(cache_key, 0) > now:
                return
        challenge = secrets.token_urlsafe(32)
        body = json.dumps({"type": "verification", "challenge": challenge}, separators=(",", ":")).encode()
        event_id = "verify_" + secrets.token_hex(16)
        headers = signed_headers(event_id, subscription_id, body, (signing_secret,), now)
        try:
            status, response = self.transport.post(url, body, headers)
            parsed = json.loads(response)
            if (not 200 <= status < 300 or type(parsed) is not dict
                    or set(parsed) != {"challenge"} or type(parsed["challenge"]) is not str
                    or not hmac.compare_digest(parsed["challenge"].encode(), challenge.encode())
                    or self.clock() - now > 10):
                raise ValueError()
        except CallbackError:
            raise
        except (ValueError, TypeError, UnicodeError):
            raise CallbackError("challenge_failed") from None
        with self._lock:
            # Cap cache size; a miss simply causes a new fresh challenge.
            if len(self._cache) >= 1024:
                self._cache.clear()
            self._cache[cache_key] = min(expires_at, now + self.cache_seconds)


class DeliverySender:
    def __init__(self, transport, clock=time.time):
        self.transport, self.clock = transport, clock

    def send(self, lease) -> str:
        try:
            headers = signed_headers(lease.event_id, lease.subscription_id, lease.body,
                                     lease.signing_secrets, int(self.clock()))
            status, _ = self.transport.post(lease.callback_url, lease.body, headers)
        except Exception:
            return "UNKNOWN"
        if 200 <= status < 300:
            return "ACCEPTED"
        if status == 410:
            return "GONE"
        if status == 413:
            return "TOO_LARGE"
        if status in {408, 429, 500, 502, 503, 504}:
            return "TEMPORARY_FAILURE"
        return "PERMANENT_FAILURE"
