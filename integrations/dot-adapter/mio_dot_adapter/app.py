"""Small ASGI HTTP boundary. Defaults unavailable; never auto-configures trust.

This is the documented Events/tool subset, not a certified full MCP 2.0 server.
A production ASGI TLS extension and actual host interoperability remain gates.
"""
import asyncio
import json
import threading
from urllib.parse import urlsplit, urlunsplit

from .errors import AuthenticationError, AuthorizationError, CallbackError, InvalidInput, Unavailable
from .protocol import PROTOCOL_VERSION

MAX_REQUEST = 65536
MAX_RESPONSE = 131072


def _pairs(items):
    out = {}
    for key, value in items:
        if key in out:
            raise ValueError("duplicate key")
        out[key] = value
    return out


def _reject_constant(_):
    raise ValueError("non-JSON number")


class MCPApp:
    def __init__(self, authenticator=None, dispatcher=None, *, metadata=None, pump=None):
        self.authenticator, self.dispatcher, self.metadata = authenticator, dispatcher, metadata
        self.metadata_url = None
        if metadata is not None:
            try:
                resource = metadata["resource"]
                parts = urlsplit(resource)
                if (type(resource) is not str or not resource.isascii() or any(ord(c) <= 32 for c in resource)
                        or parts.scheme != "https" or not parts.hostname or parts.username or parts.password
                        or parts.query or parts.fragment or not metadata.get("authorization_servers")):
                    raise ValueError()
                self.metadata_url = urlunsplit((parts.scheme, parts.netloc, "/.well-known/oauth-protected-resource", "", ""))
            except (TypeError, ValueError, KeyError):
                raise Unavailable("RESOURCE_METADATA_CONFIGURATION_REQUIRED") from None
        self.pump = pump
        self._slots = asyncio.Semaphore(16)

    async def __call__(self, scope, receive, send):
        if scope["type"] == "lifespan":
            await self._lifespan(receive, send)
            return
        async with self._slots:
            await self._call(scope, receive, send)

    async def _lifespan(self, receive, send):
        stop = threading.Event()
        task = None
        try:
            while True:
                event = await receive()
                if event["type"] == "lifespan.startup":
                    if task is not None:
                        await send({"type": "lifespan.startup.failed", "message": "DOT_RUNTIME_ALREADY_STARTED"})
                        return
                    if self.pump is not None:
                        if self.authenticator is None or self.dispatcher is None or not self.dispatcher.enabled or self.metadata_url is None:
                            await send({"type": "lifespan.startup.failed", "message": "DOT_RUNTIME_UNCONFIGURED"})
                            return
                        task = asyncio.create_task(asyncio.to_thread(self.pump.run, stop))
                        def failed(done):
                            if not done.cancelled() and done.exception() is not None:
                                self.dispatcher.enabled = False
                        task.add_done_callback(failed)
                    await send({"type": "lifespan.startup.complete"})
                elif event["type"] == "lifespan.shutdown":
                    stop.set()
                    if task is not None:
                        try:
                            await asyncio.wait_for(asyncio.shield(task), timeout=15)
                        except Exception:
                            self.dispatcher.enabled = False
                            await send({"type": "lifespan.shutdown.failed", "message": "DOT_WORKER_STOP_UNCERTAIN"})
                            return
                    await send({"type": "lifespan.shutdown.complete"})
                    return
        finally:
            stop.set()

    async def _call(self, scope, receive, send):
        if scope["type"] != "http":
            raise RuntimeError("Only HTTP is supported")
        headers = {b"content-type": b"application/json", b"cache-control": b"no-store", b"x-content-type-options": b"nosniff"}
        async def respond(status, value):
            body = json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode()
            if len(body) > MAX_RESPONSE:
                status, body = 500, b'{"error":"RESPONSE_LIMIT"}'
            await send({"type": "http.response.start", "status": status, "headers": list(headers.items())})
            await send({"type": "http.response.body", "body": body})
        if self.authenticator is None or self.dispatcher is None or not self.dispatcher.enabled or self.metadata_url is None:
            await respond(503, {"error": "FEATURE_DISABLED_OR_INTEGRATION_UNCONFIGURED"}); return
        if scope.get("path") == "/.well-known/oauth-protected-resource" and scope.get("method") == "GET" and self.metadata:
            await respond(200, self.metadata); return
        if scope.get("path") != "/mcp" or scope.get("method") != "POST":
            await respond(404, {"error": "NOT_FOUND"}); return
        seen = {}
        for key, value in scope.get("headers", []):
            key = key.lower()
            if key in seen:
                await respond(400, {"error": "DUPLICATE_HEADER"}); return
            seen[key] = value
        try:
            if seen.get(b"content-type", b"").split(b";", 1)[0].strip() != b"application/json":
                await respond(415, {"error": "JSON_REQUIRED"}); return
            auth = seen.get(b"authorization", b"").decode("ascii")
            # Authentication occurs before request JSON reaches the dispatcher.
            principal = await asyncio.to_thread(self.authenticator.authenticate, scope, auth)
            body = bytearray()
            async with asyncio.timeout(10):
                while True:
                    part = await receive()
                    if part["type"] == "http.disconnect":
                        return
                    if part["type"] != "http.request":
                        raise ValueError()
                    body.extend(part.get("body", b""))
                    if len(body) > MAX_REQUEST:
                        await respond(413, {"error": "REQUEST_LIMIT"}); return
                    if not part.get("more_body", False):
                        break
            payload = json.loads(body, object_pairs_hook=_pairs, parse_constant=_reject_constant)
            if (type(payload) is not dict or set(payload) - {"jsonrpc", "id", "method", "params"}
                    or payload.get("jsonrpc") != "2.0" or "id" not in payload
                    or type(payload["id"]) not in (str, int) or type(payload.get("method")) is not str
                    or type(payload.get("params", {})) is not dict
                    or (type(payload["id"]) is str and len(payload["id"]) > 128)
                    or (type(payload["id"]) is int and abs(payload["id"]) > 9007199254740991)):
                raise ValueError()
            request_id = payload["id"]
            version = seen.get(b"mcp-protocol-version")
            if version is not None and version != PROTOCOL_VERSION.encode():
                await respond(400, {"jsonrpc": "2.0", "id": request_id, "error": {"code": -32602, "message": "UNSUPPORTED_PROTOCOL_VERSION"}}); return
            try:
                result = await asyncio.to_thread(self.dispatcher.dispatch, principal, payload["method"], payload.get("params", {}))
                await respond(200, {"jsonrpc": "2.0", "id": request_id, "result": result})
            except InvalidInput as exc:
                code = -32601 if exc.code == "METHOD_NOT_FOUND" else -32602
                await respond(200, {"jsonrpc": "2.0", "id": request_id, "error": {"code": code, "message": exc.code}})
            except CallbackError as exc:
                await respond(200, {"jsonrpc": "2.0", "id": request_id, "error": {"code": -32015, "message": "CallbackEndpointError", "data": {"reason": exc.code}}})
        except AuthenticationError:
            headers[b"www-authenticate"] = b"Bearer"
            if self.metadata:
                headers[b"www-authenticate"] = ('Bearer resource_metadata="' + self.metadata_url + '"').encode()
            await respond(401, {"error": "UNAUTHENTICATED"})
        except AuthorizationError:
            await respond(403, {"error": "FORBIDDEN"})
        except Unavailable as exc:
            await respond(503, {"error": exc.code})
        except (ValueError, UnicodeError, RecursionError, TimeoutError):
            await respond(400, {"jsonrpc": "2.0", "id": None, "error": {"code": -32600, "message": "INVALID_REQUEST"}})
        except Exception:
            # Includes storage invariants: no reflected exception strings/secrets,
            # no fake success, and no automatic retry of a mutation.
            await respond(500, {"error": "INTERNAL_ERROR_QUERY_OPERATION_STATUS"})


app = MCPApp()  # Importing this module starts no network and leaves feature OFF.
