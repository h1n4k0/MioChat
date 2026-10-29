"""Explicit runtime assembly. Importing this module starts nothing.

The HTTP ASGI host and trusted product supervisor are separately approved
infrastructure. Only a configured, explicitly enabled lifespan starts EventPump.
"""
import threading

from .app import MCPApp
from .errors import AuthorizationError, Unavailable
from .protocol import Dispatcher
from .webhook import CallbackPolicy, CallbackVerifier, DeliverySender, PublicHTTPS


class EventPump:
    def __init__(self, dispatcher, sender, *, interval_seconds=2.0):
        if not 1 <= interval_seconds <= 60:
            raise ValueError("interval_seconds")
        self.dispatcher, self.sender, self.interval = dispatcher, sender, interval_seconds
        self._run_lock = threading.Lock()

    def tick(self, stop=None):
        if stop is not None and stop.is_set():
            return []
        if not self.dispatcher.enabled:
            raise Unavailable("FEATURE_DISABLED")
        refs = self.dispatcher.product.pending_bindings()
        if (type(refs) is not list or len(refs) > 64
                or any(type(ref) is not str or not 1 <= len(ref) <= 128 for ref in refs)):
            raise Unavailable("PRODUCT_DELIVERY_QUEUE_INVALID")
        outcomes = []
        for ref in refs:
            if stop is not None and stop.is_set():
                break
            try:
                outcomes.append(self.dispatcher.deliver_once(ref, self.sender, stop=stop))
            except AuthorizationError:
                # The product either denied before claim or retained a durable
                # SENDING/UNKNOWN intent. It alone decides future eligibility.
                outcomes.append({"status": "PRODUCT_DENIED_OR_UNCERTAIN"})
        return outcomes

    def run(self, stop: threading.Event):
        if not self._run_lock.acquire(blocking=False):
            raise Unavailable("DELIVERY_WORKER_ALREADY_RUNNING")
        try:
            while not stop.is_set():
                self.tick(stop)
                stop.wait(self.interval)
        finally:
            self._run_lock.release()


def create_runtime(authenticator, product, metadata, callback_policy: CallbackPolicy, *, enabled=False):
    """No implicit trust, subprocess, credentials, OAuth registration or plugin.

    `product` must use supervisor-owned private pipes and explicitly provisioned
    encrypted vault. Authenticator must receive real TLS extension evidence from
    the selected ASGI host, not forwarded HTTP headers or synthetic booleans.
    """
    if not enabled:
        return MCPApp()
    if authenticator is None or product is None or not callback_policy.allowed_hosts:
        raise Unavailable("RUNTIME_CONFIGURATION_REQUIRED")
    transport = PublicHTTPS(callback_policy)
    dispatcher = Dispatcher(product, CallbackVerifier(transport, callback_policy), enabled=True)
    return MCPApp(authenticator, dispatcher, metadata=metadata, pump=EventPump(dispatcher, DeliverySender(transport)))
