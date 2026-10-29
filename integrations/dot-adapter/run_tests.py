"""Pure synthetic checks; explicitly deny network connects/DNS and subprocesses."""
import sys
import unittest


def guard(event, args):
    if event in {"socket.connect", "socket.getaddrinfo", "socket.bind", "subprocess.Popen", "os.system"}:
        raise RuntimeError("SYNTHETIC_TEST_NETWORK_OR_SUBPROCESS_DENIED")

sys.addaudithook(guard)
suite = unittest.defaultTestLoader.discover("tests")
result = unittest.TextTestRunner(verbosity=2).run(suite)
raise SystemExit(not result.wasSuccessful())
