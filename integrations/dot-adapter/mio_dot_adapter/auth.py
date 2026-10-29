"""Actual certificate/JWT checks; no synthetic verified booleans or forwarded headers.

TLS evidence must be supplied by the ASGI server's TLS extension, never an HTTP
header. Deployment is blocked until the selected server's extension is verified.
Static JWKS snapshots are operator-provisioned, bounded-life trust configuration;
this code never fetches URLs or follows jku/x5u supplied by a token.
"""
from dataclasses import dataclass
from datetime import datetime, timezone
import time
import hashlib
import base64
import json
from typing import Mapping

import jwt
from cryptography import x509
from cryptography.hazmat.primitives import serialization
from cryptography.x509.oid import ExtendedKeyUsageOID
from cryptography.x509.verification import PolicyBuilder, Store, VerificationError

from .contracts import IdentityDirectory, Principal
from .errors import AuthenticationError, Unavailable


@dataclass(frozen=True)
class OAuthConfig:
    issuer: str
    audience: str
    jwks: Mapping
    keys_valid_until: int
    algorithm: str = "RS256"
    max_token_seconds: int = 3600


def _unique_object(pairs):
    obj = {}
    for key, value in pairs:
        if key in obj:
            raise ValueError("duplicate JWT member")
        obj[key] = value
    return obj


def _reject_number(_):
    raise ValueError("invalid JWT number")


class OAuthVerifier:
    def __init__(self, config: OAuthConfig, directory: IdentityDirectory):
        if (not config.issuer.startswith("https://") or not config.audience.startswith("https://")
                or config.algorithm not in {"RS256", "ES256"}
                or type(config.max_token_seconds) is not int or not 1 <= config.max_token_seconds <= 86400):
            raise Unavailable("AUTH_CONFIGURATION_REQUIRED")
        self.config, self.directory = config, directory
        self.keys = {}
        try:
            for item in config.jwks["keys"]:
                if (not isinstance(item, dict) or item.get("alg") != config.algorithm
                        or item.get("use", "sig") != "sig"
                        or item.get("key_ops", ["verify"]) != ["verify"]
                        or any(x in item for x in ("d", "p", "q", "dp", "dq", "qi", "oth", "k"))):
                    raise ValueError()
                kid = item.get("kid")
                if type(kid) is not str or not kid or kid in self.keys:
                    raise ValueError()
                self.keys[kid] = jwt.PyJWK.from_dict(dict(item), algorithm=config.algorithm).key
                if config.algorithm == "RS256" and self.keys[kid].key_size < 2048:
                    raise ValueError()
            if not self.keys:
                raise ValueError()
        except (KeyError, TypeError, ValueError, jwt.PyJWTError):
            raise Unavailable("AUTH_CONFIGURATION_REQUIRED") from None

    def verify(self, authorization: str, now: int | None = None) -> Principal:
        now = int(time.time()) if now is None else now
        cfg = self.config
        if type(cfg.keys_valid_until) is not int or now >= cfg.keys_valid_until:
            raise Unavailable("AUTH_KEYS_STALE")
        if (type(authorization) is not str or not authorization.startswith("Bearer ")
                or len(authorization) > 16384 or authorization.count(" ") != 1):
            raise AuthenticationError("UNAUTHENTICATED")
        token = authorization[7:]
        try:
            parts = token.split(".")
            if len(parts) != 3:
                raise ValueError()
            for part in parts[:2]:
                decoded = json.loads(base64.urlsafe_b64decode(part + "=" * (-len(part) % 4)),
                                     object_pairs_hook=_unique_object, parse_constant=_reject_number)
                if type(decoded) is not dict:
                    raise ValueError()
            header = jwt.get_unverified_header(token)
            if (type(header) is not dict or header.get("alg") != cfg.algorithm or header.get("kid") not in self.keys
                    or set(header) - {"alg", "kid", "typ"}
                    or header.get("typ", "JWT") not in {"JWT", "at+jwt"}):
                raise ValueError()
            claims = jwt.decode(token, self.keys[header["kid"]], algorithms=[cfg.algorithm],
                                audience=cfg.audience, issuer=cfg.issuer,
                                options={"require": ["iss", "aud", "sub", "exp", "iat"],
                                         "verify_exp": False, "verify_iat": False,
                                         "verify_nbf": False, "strict_aud": True})
            # NumericDate booleans/floats/NaN are not accepted; exact issuer/audience.
            for name in ("exp", "iat"):
                if type(claims[name]) is not int:
                    raise ValueError()
            if "nbf" in claims and type(claims["nbf"]) is not int:
                raise ValueError()
            if (claims["iat"] > now or claims.get("nbf", 0) > now or claims["exp"] <= now
                    or claims["exp"] <= claims["iat"]
                    or claims["exp"] - claims["iat"] > cfg.max_token_seconds):
                raise ValueError()
            sub, scope = claims["sub"], claims.get("scope", "")
            if (type(sub) is not str or not sub or len(sub) > 512 or type(scope) is not str
                    or len(scope) > 4096 or any(ord(c) < 32 for c in sub)):
                raise ValueError()
            account = self.directory.resolve(cfg.issuer, sub)
            if (account is None or type(account.active) is not bool or not account.active or type(account.auth_epoch) is not int
                    or account.auth_epoch < 0 or type(account.tokens_valid_after) is not int
                    or claims["iat"] < account.tokens_valid_after
                    or any(type(x) is not str or not x.strip() for x in
                           (account.tenant_id, account.user_uid, account.profile_id))):
                raise ValueError()
            return Principal(cfg.issuer, sub, account.tenant_id, account.user_uid,
                             account.profile_id, account.auth_epoch, frozenset(scope.split()),
                             claims["exp"], claims["iat"])
        except (jwt.PyJWTError, ValueError, TypeError, KeyError, RecursionError):
            raise AuthenticationError("UNAUTHENTICATED") from None


class HostCertificateVerifier:
    """Validate chain to provisioned OpenAI root(s), client EKU and exact SAN.

    No root bundle is downloaded or selected implicitly. Trust roots must be
    approved deployment configuration. ASGI `client_cert_chain` is leaf-first PEM.
    """
    def __init__(self, root_pems: tuple[bytes, ...], intermediate_pems: tuple[bytes, ...], expected_dns: str = "mtls.prod.connectors.openai.com"):
        if not root_pems or not intermediate_pems or expected_dns != "mtls.prod.connectors.openai.com":
            raise Unavailable("HOST_TRUST_CONFIGURATION_REQUIRED")
        try:
            self.roots = tuple(x509.load_pem_x509_certificate(pem) for pem in root_pems)
            self.intermediates = tuple(x509.load_pem_x509_certificate(pem) for pem in intermediate_pems)
            self.intermediate_keys = {self._public_key_hash(cert) for cert in self.intermediates}
            for root in self.roots + self.intermediates:
                if not root.extensions.get_extension_for_class(x509.BasicConstraints).value.ca:
                    raise ValueError()
        except (ValueError, x509.ExtensionNotFound):
            raise Unavailable("HOST_TRUST_CONFIGURATION_REQUIRED") from None
        self.expected_dns = expected_dns

    @staticmethod
    def _public_key_hash(cert):
        return hashlib.sha256(cert.public_key().public_bytes(serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)).digest()

    def verify_scope(self, scope: dict, now: int | None = None) -> None:
        now = int(time.time()) if now is None else now
        tls = scope.get("extensions", {}).get("tls", {})
        chain = tls.get("client_cert_chain")
        if (scope.get("scheme") != "https" or not isinstance(chain, (list, tuple))
                or not 1 <= len(chain) <= 8 or tls.get("client_cert_error") not in (None, "")):
            raise AuthenticationError("HOST_UNAUTHENTICATED")
        try:
            certs = []
            for pem in chain:
                if type(pem) is str:
                    pem = pem.encode("ascii")
                if type(pem) is not bytes or len(pem) > 16384:
                    raise ValueError()
                certs.append(x509.load_pem_x509_certificate(pem))
            leaf = certs[0]
            eku = leaf.extensions.get_extension_for_class(x509.ExtendedKeyUsage).value
            names = leaf.extensions.get_extension_for_class(x509.SubjectAlternativeName).value
            if (ExtendedKeyUsageOID.CLIENT_AUTH not in eku
                    or self.expected_dns not in names.get_values_for_type(x509.DNSName)):
                raise ValueError()
            verifier = (PolicyBuilder().store(Store(list(self.roots)))
                        .time(datetime.fromtimestamp(now, timezone.utc)).max_chain_depth(4)
                        .build_client_verifier())
            verified = verifier.verify(leaf, certs[1:] + list(self.intermediates))
            if not any(self._public_key_hash(cert) in self.intermediate_keys for cert in verified.chain[1:-1]):
                raise ValueError()
        except (ValueError, TypeError, UnicodeError, x509.ExtensionNotFound, VerificationError):
            raise AuthenticationError("HOST_UNAUTHENTICATED") from None


class Authenticator:
    def __init__(self, host: HostCertificateVerifier, oauth: OAuthVerifier):
        self.host, self.oauth = host, oauth

    def authenticate(self, scope: dict, authorization: str) -> Principal:
        self.host.verify_scope(scope)
        return self.oauth.verify(authorization)
