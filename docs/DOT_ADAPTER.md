# Optional dot adapter: code package, not a configured service

The complete Python adapter lives in [`integrations/dot-adapter`](../integrations/dot-adapter/). Its 14 source/config/test files are included with the Go server and native client rather than being an external download. Python >= 3.12 and the exact requirements in `pyproject.toml` are required: `PyJWT[crypto]==2.13.0` and `cryptography==50.0.0`.

The integration is **off by default**. Shipping this code does not configure a real dot, OAuth issuer, host subscription, signing key, encrypted delivery vault, or room binding. Real host/authentication/subscription setup and end-to-end delivery are not validated by the synthetic tests. Do not advertise the client as already connected to dot.

## Product boundary

The Go server has an explicitly opt-in inherited-pipe interface (`-dot-private-pipes`, `-dot-read-fd`, and `-dot-write-fd`). The descriptors must be trusted pipe descriptors supplied by a supervisor; this is not a public listener or an SSH subsystem, and the server does not spawn an adapter or create credentials. The Python package supplies adapter/runtime classes, not an automatically launched service. A separate reviewed deployment is required to connect them to a real host.

Room replies are proposed privately to the human owner. The native review must display the exact body, source, binding generation, and audience policy before explicit confirmation. The audience is not a permanent recipient allowlist: future users who gain channel access may also read channel history. An uncertain commit is looked up by its existing operation identifier, not automatically resubmitted. The server's final transaction rechecks session, account/credential identity, binding and current room authority.

## Reproducible synthetic checks

From a Python environment satisfying the pinned requirements:

```sh
cd integrations/dot-adapter
python run_tests.py
```

The test driver explicitly rejects network connections, DNS, bind operations, and subprocess creation. These checks cover synthetic adapter contracts, storage/error behavior and cooperative stopping; they do not exercise a real provider. Stopping prevents the next claim/send; it is not a hard cancellation or rollback of an already in-flight operation. Actual in-flight outcomes are retained, and uncertain outcomes must remain uncertain.

Server Go tests and native protocol/UI tests are part of the main repository's validation. Packaging the Python files does not turn any previously unrun physical GUI, production service, or provider integration into a pass.
