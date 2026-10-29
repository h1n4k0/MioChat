import base64
from dataclasses import replace
import io
import hashlib
import json
import os
from pathlib import Path
import tempfile
import threading
import time
import unittest

from mio_dot_adapter.contracts import Principal, SubscriptionPermit, SubscriptionGrant
from mio_dot_adapter.errors import StorageInvariantError, Unavailable
from mio_dot_adapter.product import PrivatePipeTransport
from mio_dot_adapter.vault import EncryptedDeliveryVault

NOW = int(time.time())
SECRET_A = "whsec_" + base64.b64encode(b"synthetic-key-a" * 2).decode()
SECRET_B = "whsec_" + base64.b64encode(b"synthetic-key-b" * 2).decode()
P = Principal("https://synthetic.invalid", "synthetic-subject", "install", "1", "profile", 0, frozenset(), NOW+1000, NOW-1)


class ProductTests(unittest.TestCase):
    def test_encrypted_vault_rotation_expiry_and_wrong_subscription(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "vault.db"
            clock = [NOW]
            vault = EncryptedDeliveryVault(path, hashlib.sha256(b"synthetic key, not a live credential").digest(), clock=lambda: clock[0])
            try:
                permit = SubscriptionPermit("binding", 2, 0, 1, 1)
                grant = SubscriptionGrant("sub", "mio.message.created.v1", "https://receiver.synthetic.invalid/private-callback", SECRET_A, NOW+600)
                first = vault.put(P, permit, grant)
                self.assertEqual(vault.get(first, "sub")["secrets"], [SECRET_A])
                second = vault.put(P, replace(permit, previous_delivery_handle=first), replace(grant, signing_secret=SECRET_B))
                self.assertEqual(vault.get(second, "sub")["secrets"], [SECRET_B, SECRET_A])
                clock[0] += 61
                self.assertEqual(vault.get(second, "sub")["secrets"], [SECRET_B])
                with self.assertRaises(Unavailable): vault.get(second, "wrong-sub")
                raw = path.read_bytes()
                self.assertNotIn(SECRET_A.encode(), raw)
                self.assertNotIn(b"private-callback", raw)
                clock[0] += 600
                with self.assertRaises(Unavailable): vault.get(second, "sub")
            finally: vault.close()

    def test_vault_ignored_write_and_ciphertext_rewrite_fail_closed(self):
        for attack in ("ignore", "rewrite"):
            with self.subTest(attack=attack), tempfile.TemporaryDirectory() as directory:
                vault = EncryptedDeliveryVault(Path(directory)/"vault.db", hashlib.sha256(b"synthetic key, not a live credential").digest())
                try:
                    if attack == "ignore":
                        vault.db.execute("CREATE TRIGGER attack BEFORE INSERT ON material BEGIN SELECT RAISE(IGNORE);END")
                    else:
                        vault.db.execute("CREATE TRIGGER attack AFTER INSERT ON material BEGIN UPDATE material SET ciphertext=X'00' WHERE handle=new.handle;END")
                    with self.assertRaises(StorageInvariantError):
                        vault.put(P, SubscriptionPermit("binding", 2, 0, 1, 1), SubscriptionGrant("sub", "event", "https://receiver.synthetic.invalid/x", SECRET_A, NOW+600))
                    self.assertEqual(vault.db.execute("SELECT count(*) FROM material").fetchone()[0], 0)
                finally: vault.close()

    def test_private_pipe_unknown_io_is_not_automatically_retried(self):
        writer = io.BytesIO()
        transport = PrivatePipeTransport(io.BytesIO(b""), writer)
        with self.assertRaises(Unavailable): transport.exchange("propose", {"issuer": "synthetic"})
        first = writer.getvalue()
        with self.assertRaises(Unavailable): transport.exchange("propose", {"issuer": "synthetic"})
        self.assertEqual(writer.getvalue(), first)
        self.assertEqual(first.count(b"\n"), 1)

class RuntimeTests(unittest.TestCase):
    def test_event_pump_stop_prevents_later_claims_and_keeps_inflight_outcome(self):
        from mio_dot_adapter.runtime import EventPump
        from mio_dot_adapter.protocol import Dispatcher
        from mio_dot_adapter.contracts import DeliveryLease
        for stage in ("already_stopped", "queue", "claim", "ACCEPTED", "UNKNOWN"):
            with self.subTest(stage=stage):
                stop = threading.Event()
                calls, finished = [], []
                class Product:
                    def pending_bindings(self):
                        calls.append("queue")
                        if stage == "queue": stop.set()
                        return ["first", "second"]
                    def claim_delivery(self, ref):
                        calls.append(("claim", ref))
                        if stage == "claim": stop.set()
                        return DeliveryLease("lease", "event", "sub", "https://synthetic.invalid", (SECRET_A,), b"{}")
                    def finish_delivery(self, lease_id, outcome):
                        finished.append((lease_id, outcome))
                        return {"status": outcome}
                class Sender:
                    def send(self, lease):
                        calls.append("send")
                        stop.set()
                        return stage
                if stage == "already_stopped": stop.set()
                EventPump(Dispatcher(Product(), None, enabled=True), Sender()).run(stop)
                expected = [] if stage == "already_stopped" else ["queue"]
                if stage not in ("already_stopped", "queue"):
                    expected.append(("claim", "first"))
                    if stage != "claim": expected.append("send")
                self.assertEqual(calls, expected)
                self.assertEqual(finished, [] if stage in ("already_stopped", "queue")
                                 else [("lease", "UNKNOWN" if stage == "claim" else stage)])

    def test_event_pump_uses_durable_unknown_and_does_not_resend(self):
        from mio_dot_adapter.runtime import EventPump
        from mio_dot_adapter.protocol import Dispatcher
        from mio_dot_adapter.contracts import DeliveryLease
        class Product:
            status = "PENDING"
            def pending_bindings(self): return ["binding"] if self.status == "PENDING" else []
            def claim_delivery(self, ref):
                self.status = "SENDING"
                return DeliveryLease("lease", "event", "sub", "https://synthetic.invalid", (SECRET_A,), b"{}")
            def finish_delivery(self, lease, outcome):
                self.status = outcome
                return {"status": outcome}
        class Sender:
            calls = 0
            def send(self, lease): self.calls += 1; return "UNKNOWN"
        product, sender = Product(), Sender()
        pump = EventPump(Dispatcher(product, None, enabled=True), sender)
        self.assertEqual(pump.tick(), [{"status": "UNKNOWN"}])
        self.assertEqual(pump.tick(), [])
        self.assertEqual(sender.calls, 1)

    def test_runtime_default_off_does_not_build_or_start_connections(self):
        from mio_dot_adapter.runtime import create_runtime
        from mio_dot_adapter.webhook import CallbackPolicy
        app = create_runtime(None, None, None, CallbackPolicy())
        self.assertIsNone(app.pump)
        self.assertIsNone(app.authenticator)
        with self.assertRaises(Unavailable): create_runtime(None, None, None, CallbackPolicy(), enabled=True)
