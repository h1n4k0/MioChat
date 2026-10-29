"""Bounded-life AES-GCM delivery material store with mandatory write/readback.

No key is generated or loaded implicitly. A deployment key and a private storage
directory must be explicitly provisioned outside this source-only experiment.
"""
import hashlib
import json
import os
from pathlib import Path
import secrets
import sqlite3
import stat
import threading
import time

from cryptography.hazmat.primitives.ciphers.aead import AESGCM

from .errors import StorageInvariantError, Unavailable
from .webhook import decode_secret


class EncryptedDeliveryVault:
    def __init__(self, path, encryption_key: bytes, *, clock=time.time):
        if type(encryption_key) is not bytes or len(encryption_key) != 32:
            raise Unavailable("VAULT_KEY_CONFIGURATION_REQUIRED")
        path = Path(path)
        parent = path.parent.stat()
        if (not stat.S_ISDIR(parent.st_mode) or parent.st_uid != os.geteuid()
                or stat.S_IMODE(parent.st_mode) & 0o077):
            raise Unavailable("PRIVATE_VAULT_DIRECTORY_REQUIRED")
        flags = os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0)
        fd = os.open(path, flags, 0o600)
        try:
            meta = os.fstat(fd)
            if not stat.S_ISREG(meta.st_mode) or meta.st_uid != os.geteuid() or stat.S_IMODE(meta.st_mode) & 0o077:
                raise Unavailable("PRIVATE_VAULT_FILE_REQUIRED")
        finally:
            os.close(fd)
        self.db = sqlite3.connect(path, isolation_level=None, check_same_thread=False)
        self.db.execute("PRAGMA busy_timeout=1000")
        self.db.execute("CREATE TABLE IF NOT EXISTS material(handle TEXT PRIMARY KEY,owner TEXT NOT NULL,subscription_id TEXT NOT NULL,callback_hash TEXT NOT NULL,expires INTEGER NOT NULL,ciphertext BLOB NOT NULL)")
        self.aead, self.clock, self._lock = AESGCM(encryption_key), clock, threading.RLock()

    def close(self):
        self.db.close()

    @staticmethod
    def _owner(p):
        return hashlib.sha256(json.dumps([p.issuer, p.subject, p.tenant_id, p.user_uid], separators=(",", ":")).encode()).hexdigest()

    def _decode(self, row):
        handle, owner, sid, digest, expires, ciphertext = row
        if expires <= self.clock():
            raise Unavailable("DELIVERY_MATERIAL_EXPIRED")
        try:
            aad = json.dumps([handle, owner, sid, digest, expires], separators=(",", ":")).encode()
            data = json.loads(self.aead.decrypt(ciphertext[:12], ciphertext[12:], aad))
            if hashlib.sha256(data["url"].encode()).hexdigest() != digest:
                raise ValueError()
            if data.get("previous_until", 0) <= self.clock():
                data["secrets"] = data["secrets"][:1]
            for key in data["secrets"]:
                decode_secret(key)
            return data
        except Exception:
            raise StorageInvariantError("VAULT_READBACK_FAILED") from None

    def put(self, p, permit, grant):
        owner = self._owner(p)
        digest = hashlib.sha256(grant.callback_url.encode()).hexdigest()
        handle = "delivery_" + secrets.token_hex(24)
        now = int(self.clock())
        if not now < grant.expires_at <= min(p.expires_at, now + 900):
            raise Unavailable("INVALID_DELIVERY_EXPIRY")
        decode_secret(grant.signing_secret)
        values = {"url": grant.callback_url, "secrets": [grant.signing_secret], "previous_until": 0}
        with self._lock:
            self.db.execute("BEGIN IMMEDIATE")
            try:
                if permit.previous_delivery_handle:
                    old = self.db.execute("SELECT * FROM material WHERE handle=? AND owner=? AND subscription_id=?", (permit.previous_delivery_handle, owner, grant.subscription_id)).fetchone()
                    if old is None:
                        raise Unavailable("PREVIOUS_DELIVERY_MATERIAL_UNAVAILABLE")
                    previous = self._decode(old)
                    if previous["url"] != grant.callback_url:
                        raise Unavailable("CALLBACK_CHANGE_REQUIRES_OWNER_PAUSE")
                    if previous["secrets"][0] != grant.signing_secret:
                        values["secrets"].append(previous["secrets"][0]); values["previous_until"] = min(grant.expires_at, now + 60)
                aad = json.dumps([handle, owner, grant.subscription_id, digest, grant.expires_at], separators=(",", ":")).encode()
                nonce = secrets.token_bytes(12)
                ciphertext = nonce + self.aead.encrypt(nonce, json.dumps(values, separators=(",", ":")).encode(), aad)
                row = (handle, owner, grant.subscription_id, digest, grant.expires_at, ciphertext)
                cursor = self.db.execute("INSERT INTO material VALUES(?,?,?,?,?,?)", row)
                actual = self.db.execute("SELECT * FROM material WHERE handle=?", (handle,)).fetchone()
                if cursor.rowcount != 1 or actual != row or self._decode(actual)["url"] != grant.callback_url:
                    raise StorageInvariantError("VAULT_WRITE_READBACK_FAILED")
                self.db.commit()
            except BaseException:
                self.db.rollback(); raise
        return handle

    def get(self, handle, subscription_id):
        with self._lock:
            row = self.db.execute("SELECT * FROM material WHERE handle=? AND subscription_id=?", (handle, subscription_id)).fetchone()
            if row is None: raise Unavailable("DELIVERY_MATERIAL_UNAVAILABLE")
            return self._decode(row)

