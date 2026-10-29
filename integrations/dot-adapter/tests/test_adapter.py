import asyncio
import base64
from dataclasses import replace
from datetime import datetime, timedelta, timezone
import hashlib
import hmac
import json
import socket
import time
import unittest

import jwt
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec, rsa
from cryptography.x509.oid import NameOID, ExtendedKeyUsageOID

from mio_dot_adapter.auth import Authenticator, HostCertificateVerifier, OAuthConfig, OAuthVerifier
from mio_dot_adapter.app import MCPApp
from mio_dot_adapter.contracts import Account, SubscriptionPermit
from mio_dot_adapter.errors import AuthenticationError, AuthorizationError, CallbackError, InvalidInput, Unavailable
from mio_dot_adapter.protocol import Dispatcher, EVENT_NAME, PROTOCOL_VERSION, TOOL_SCOPES
from mio_dot_adapter.webhook import CallbackPolicy, CallbackVerifier, DeliverySender, decode_secret, public_addresses, signed_headers

NOW = int(time.time())
ISSUER = "https://synthetic.invalid/issuer"
AUDIENCE = "https://synthetic.invalid/mcp"
SECRET = "whsec_" + base64.b64encode(b"synthetic-not-a-live-secret-key-123").decode()
HOST = "mtls.prod.connectors.openai.com"


def cert(name, key, issuer_cert=None, issuer_key=None, ca=False, san=None, client=True):
    subject = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, name)])
    issuer = issuer_cert.subject if issuer_cert else subject
    signer = issuer_key or key
    now = datetime.now(timezone.utc)
    builder = (x509.CertificateBuilder().subject_name(subject).issuer_name(issuer)
        .public_key(key.public_key()).serial_number(x509.random_serial_number())
        .not_valid_before(now - timedelta(minutes=5)).not_valid_after(now + timedelta(hours=1))
        .add_extension(x509.BasicConstraints(ca=ca, path_length=1 if ca else None), critical=True)
        .add_extension(x509.SubjectKeyIdentifier.from_public_key(key.public_key()), critical=False)
        .add_extension(x509.AuthorityKeyIdentifier.from_issuer_public_key(signer.public_key()), critical=False)
        .add_extension(x509.KeyUsage(digital_signature=True, content_commitment=False, key_encipherment=False,
            data_encipherment=False, key_agreement=False, key_cert_sign=ca, crl_sign=ca,
            encipher_only=False, decipher_only=False), critical=True))
    if not ca:
        builder = (builder.add_extension(x509.SubjectAlternativeName([x509.DNSName(san or HOST)]), critical=False)
                   .add_extension(x509.ExtendedKeyUsage([ExtendedKeyUsageOID.CLIENT_AUTH if client else ExtendedKeyUsageOID.SERVER_AUTH]), critical=False))
    return builder.sign(signer, hashes.SHA256())


def pem(c):
    return c.public_bytes(serialization.Encoding.PEM)


class Directory:
    def __init__(self):
        self.account = Account("synthetic-installation", "1", "synthetic-profile", 0, 0, True)
    def resolve(self, issuer, subject):
        return self.account if (issuer, subject) == (ISSUER, "subject-1") else None


class SyntheticProduct:
    """Protocol-only spy. Actual Go SQLite behavior is tested by TestDot separately."""
    def __init__(self):
        self.calls = []
        self.authorized = True
        self.generation = 2
        self.subscription = None
    def get_profile(self, p): return {"id": p.profile_id, "name": "Synthetic Alice"}
    def list_bindings(self, p): return {"bindings": []}
    def can_discover_events(self, p): return True
    def read_context(self, p, read_ref, limit):
        self.calls.append(("read", read_ref, limit)); return {"reply_ref": "reply_1", "messages": []}
    def propose_reply(self, p, reply_ref, content, idem):
        self.calls.append(("draft", reply_ref, content, idem)); return {"operation_ref": "operation_1", "status": "AWAITING_CONFIRMATION"}
    def get_operation(self, p, op): return {"operation_ref": op, "status": "AWAITING_CONFIRMATION"}
    def authorize_subscription(self, p, binding):
        self.calls.append(("authorize", binding))
        if not self.authorized: raise AuthorizationError("OWNER_ENABLE_REQUIRED")
        return SubscriptionPermit(binding, self.generation, p.auth_epoch, 1, 1)
    def commit_subscription(self, p, permit, grant):
        if permit.binding_generation != self.generation: raise AuthorizationError("GENERATION_CHANGED")
        self.calls.append(("subscribe", grant.subscription_id)); self.subscription = grant
    def unsubscribe(self, p, binding, sid, digest):
        self.calls.append(("unsubscribe", binding, sid, digest)); self.subscription = None


class ChallengeTransport:
    def __init__(self):
        self.calls = []
        self.wrong = False
        self.on_post = None
    def post(self, url, body, headers):
        self.calls.append((url, body, headers))
        if self.on_post: self.on_post()
        request = json.loads(body)
        return 200, json.dumps({"challenge": "wrong" if self.wrong else request["challenge"]}).encode()


class AdapterTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
        cls.jwk = json.loads(jwt.algorithms.RSAAlgorithm.to_jwk(cls.key.public_key()))
        cls.jwk.update(kid="synthetic-key-1", alg="RS256", use="sig")
        root_key, intermediate_key, leaf_key = [ec.generate_private_key(ec.SECP256R1()) for _ in range(3)]
        cls.root = cert("Synthetic root", root_key, ca=True)
        cls.intermediate = cert("Synthetic intermediate", intermediate_key, cls.root, root_key, ca=True)
        cls.leaf = cert("Synthetic host", leaf_key, cls.intermediate, intermediate_key)
        cls.bad_leaf = cert("Synthetic wrong host", leaf_key, cls.intermediate, intermediate_key, san="attacker.invalid")
        cls.bad_eku = cert("Synthetic wrong purpose", leaf_key, cls.intermediate, intermediate_key, client=False)
        cls.scope = {"type": "http", "scheme": "https", "extensions": {"tls": {"client_cert_chain": [pem(cls.leaf), pem(cls.intermediate)]}}}
        cls.host = HostCertificateVerifier((pem(cls.root),), (pem(cls.intermediate),))

    def setUp(self):
        self.directory = Directory()
        self.oauth = OAuthVerifier(OAuthConfig(ISSUER, AUDIENCE, {"keys": [self.jwk]}, NOW + 3600), self.directory)
        self.auth = Authenticator(self.host, self.oauth)
        self.product = SyntheticProduct()
        self.transport = ChallengeTransport()
        self.policy = CallbackPolicy(frozenset({"receiver.synthetic.invalid"}))
        self.callbacks = CallbackVerifier(self.transport, self.policy)
        self.dispatcher = Dispatcher(self.product, self.callbacks, enabled=True)
        self.principal = self.oauth.verify("Bearer " + self.token())

    def token(self, **changes):
        claims = {"iss": ISSUER, "aud": AUDIENCE, "sub": "subject-1", "iat": NOW - 10,
                  "exp": NOW + 600, "scope": " ".join(set(TOOL_SCOPES.values()) | {"mio.events.subscribe"})}
        claims.update(changes)
        return jwt.encode(claims, self.key, algorithm="RS256", headers={"kid": "synthetic-key-1"})

    def subscription(self, **changes):
        params = {"name": EVENT_NAME, "arguments": {"binding_ref": "binding_1"},
                  "delivery": {"mode": "webhook", "url": "https://receiver.synthetic.invalid/callback", "secret": SECRET}, "cursor": None}
        params.update(changes)
        return params

    def test_real_synthetic_crypto_host_and_oauth_positive(self):
        self.host.verify_scope(self.scope)
        p = self.auth.authenticate(self.scope, "Bearer " + self.token())
        self.assertEqual((p.user_uid, p.profile_id, p.auth_epoch), ("1", "synthetic-profile", 0))
        self.assertNotIn("owner.review", p.scopes)

    def test_host_forged_headers_wrong_san_eku_and_missing_chain_denied(self):
        bad = [{"type": "http", "scheme": "https", "headers": [(b"x-host-verified", b"true")]},
               {**self.scope, "scheme": "http"}]
        for c in (self.bad_leaf, self.bad_eku):
            bad.append({**self.scope, "extensions": {"tls": {"client_cert_chain": [pem(c), pem(self.intermediate)]}}})
        for scope in bad:
            with self.subTest(scope=scope.get("scheme")), self.assertRaises(AuthenticationError):
                self.host.verify_scope(scope)

    def test_oauth_wrong_signature_issuer_audience_expiry_and_unmapped_subject(self):
        for changes in ({"iss": ISSUER + "/"}, {"aud": "https://wrong.invalid"}, {"exp": NOW-1},
                        {"nbf": NOW+100}, {"iat": True}, {"sub": "unknown"}, {"aud": [AUDIENCE]}):
            with self.subTest(changes=changes), self.assertRaises(AuthenticationError):
                self.oauth.verify("Bearer " + self.token(**changes))
        parts = self.token().split("."); parts[2] = ("A" if parts[2][0] != "A" else "B") + parts[2][1:]
        with self.assertRaises(AuthenticationError): self.oauth.verify("Bearer " + ".".join(parts))
        self.directory.account = replace(self.directory.account, active=False)
        with self.assertRaises(AuthenticationError): self.oauth.verify("Bearer " + self.token())

    def test_auth_config_never_defaults_to_trusted_and_stale_keys_deny(self):
        with self.assertRaises(Unavailable): HostCertificateVerifier((), ())
        with self.assertRaises(Unavailable): OAuthVerifier(OAuthConfig(ISSUER, AUDIENCE, {"keys": []}, NOW+1), self.directory)
        stale = OAuthVerifier(OAuthConfig(ISSUER, AUDIENCE, {"keys": [self.jwk]}, NOW-1), self.directory)
        with self.assertRaises(Unavailable): stale.verify("Bearer " + self.token())

    def test_discovery_tool_metadata_and_no_owner_model_actions(self):
        discovery = self.dispatcher.dispatch(self.principal, "server/discover", {})
        self.assertEqual(discovery["supportedVersions"], [PROTOCOL_VERSION])
        tools = self.dispatcher.dispatch(self.principal, "tools/list", {})["tools"]
        self.assertEqual(len(tools), 5)
        self.assertTrue(next(t for t in tools if t["name"] == "mio.get_profile")["_meta"]["openai/profile"])
        for name in ("mio.confirm_reply", "mio.enable_binding", "mio.send_message", "owner.review"):
            with self.assertRaises(InvalidInput):
                self.dispatcher.dispatch(self.principal, "tools/call", {"name": name, "arguments": {}})
        result = self.dispatcher.dispatch(self.principal, "tools/call", {"name": "mio.get_profile", "arguments": {}})
        self.assertEqual(result["structuredContent"]["id"], "synthetic-profile")

    def test_tool_strict_types_scopes_and_no_target_or_callback_override(self):
        for arguments in ({"read_ref": "read_1", "limit": True}, {"read_ref": "read_1", "room": "elsewhere"}, {"read_ref": "read_1", "callback": "https://wrong.invalid"}):
            with self.assertRaises(InvalidInput):
                self.dispatcher.dispatch(self.principal, "tools/call", {"name": "mio.read_context", "arguments": arguments})
        denied = replace(self.principal, scopes=frozenset({"owner.review"}))
        with self.assertRaises(AuthorizationError): self.dispatcher.dispatch(denied, "tools/call", {"name": "mio.propose_reply", "arguments": {"reply_ref": "r", "content": "c", "idempotency_key": "i"}})
        self.assertEqual(self.product.calls, [])

    def test_subscription_wire_verification_ttl_and_deterministic_refresh(self):
        args = self.subscription(ttlMs=100000)
        a = self.dispatcher.dispatch(self.principal, "events/subscribe", args)
        b = self.dispatcher.dispatch(self.principal, "events/subscribe", args)
        self.assertEqual(a["id"], b["id"])
        self.assertEqual(len(self.transport.calls), 1)  # bounded same-owner URL/key cache
        self.assertLessEqual(self.product.subscription.expires_at, int(time.time()) + 100)
        self.assertEqual(a["cursor"], None)
        stop = {k: v for k, v in args.items() if k in {"name", "arguments", "delivery"}}
        stop["delivery"] = {"mode": "webhook", "url": args["delivery"]["url"]}
        self.assertEqual(self.dispatcher.dispatch(self.principal, "events/unsubscribe", stop), {})
        self.assertEqual(self.product.calls[-1][2], a["id"])

    def test_unauthorized_subscription_no_callback_and_race_rechecked(self):
        self.product.authorized = False
        with self.assertRaises(AuthorizationError): self.dispatcher.dispatch(self.principal, "events/subscribe", self.subscription())
        self.assertEqual(self.transport.calls, [])
        self.product.authorized = True
        self.transport.on_post = lambda: setattr(self.product, "generation", 3)
        with self.assertRaises(AuthorizationError): self.dispatcher.dispatch(self.principal, "events/subscribe", self.subscription())
        self.assertIsNone(self.product.subscription)

    def test_failed_challenge_and_invalid_protocol_values_do_not_activate(self):
        self.transport.wrong = True
        with self.assertRaises(CallbackError): self.dispatcher.dispatch(self.principal, "events/subscribe", self.subscription())
        self.assertIsNone(self.product.subscription)
        for ttl in (True, 0, -1, "1000", 999):
            with self.assertRaises(InvalidInput): self.dispatcher.dispatch(self.principal, "events/subscribe", self.subscription(ttlMs=ttl))
        with self.assertRaises(InvalidInput): self.dispatcher.dispatch(self.principal, "events/subscribe", self.subscription(cursor="replay-not-supported"))

    def test_callback_ssrf_no_redirect_and_public_dns_filter(self):
        for url in ("http://receiver.synthetic.invalid/x", "https://receiver.synthetic.invalid:444/x", "https://evil.invalid/x", "https://user@receiver.synthetic.invalid/x", "https://receiver.synthetic.invalid/x#fragment", "https://127.0.0.1/x", "https://receiver.synthetic.invalid\\@evil.invalid/x"):
            with self.assertRaises(CallbackError): self.policy.parse(url)
        def resolver(ip): return lambda *a, **k: [(socket.AF_INET6 if ":" in ip else socket.AF_INET, socket.SOCK_STREAM, socket.IPPROTO_TCP, "", (ip, 443))]
        for ip in ("127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "::ffff:127.0.0.1", "224.0.0.1"):
            with self.assertRaises(CallbackError): public_addresses("synthetic.invalid", resolver(ip))
        self.assertEqual(public_addresses("synthetic.invalid", resolver("93.184.216.34"))[0][1], "93.184.216.34")

    def test_standard_webhooks_exact_bytes_dual_rotation_and_unknown_delivery(self):
        body = b'{"data":"synthetic"}'
        headers = signed_headers("event_1", "sub_1", body, (SECRET, SECRET), NOW)
        expected = base64.b64encode(hmac.new(decode_secret(SECRET), b"event_1."+str(NOW).encode()+b"."+body, hashlib.sha256).digest()).decode()
        self.assertEqual(headers["webhook-signature"], "v1,"+expected+" v1,"+expected)
        class Broken:
            def post(self, *a): raise TimeoutError("synthetic lost acknowledgement")
        from mio_dot_adapter.contracts import DeliveryLease
        lease = DeliveryLease("lease", "event_1", "sub_1", "https://receiver.synthetic.invalid/callback", (SECRET,), body)
        self.assertEqual(DeliverySender(Broken()).send(lease), "UNKNOWN")

    def test_asgi_default_off_authentication_and_strict_json(self):
        async def invoke(app, raw, authenticated=True):
            scope = {**self.scope, "method": "POST", "path": "/mcp", "headers": [(b"content-type", b"application/json")]}
            if authenticated: scope["headers"].append((b"authorization", ("Bearer "+self.token()).encode()))
            events = []
            async def receive(): return {"type": "http.request", "body": raw}
            async def send(event): events.append(event)
            await app(scope, receive, send)
            return events[0]["status"], json.loads(events[1]["body"])
        raw = b'{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}'
        self.assertEqual(asyncio.run(invoke(MCPApp(), raw))[0], 503)
        app = MCPApp(self.auth, self.dispatcher, metadata={"resource": AUDIENCE, "authorization_servers": [ISSUER]})
        self.assertEqual(asyncio.run(invoke(app, raw, False))[0], 401)
        status, body = asyncio.run(invoke(app, raw)); self.assertEqual(status, 200); self.assertIn("events", body["result"]["capabilities"])
        for malformed in (b'[{"jsonrpc":"2.0"}]', b'{"jsonrpc":"2.0","id":1,"id":2,"method":"tools/list"}', b'{"jsonrpc":"2.0","id":true,"method":"tools/list"}'):
            self.assertEqual(asyncio.run(invoke(app, malformed))[0], 400)
