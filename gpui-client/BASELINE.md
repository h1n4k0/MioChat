# Native client dependency baseline

The native client pins `gpui-pre` and `gpui-pre-platform` to version `0.3.7`, with the full resolution recorded in `Cargo.lock`. The package metadata identifies the Zed snapshot as `1a28cff4b409169bac058bca40dfbfeb7621d19b` (Apache-2.0). Consult the locked dependencies and third-party notices when redistributing the client.

`Cargo.toml` declares Rust 1.96 or newer. The documented reproducible commands and optional CI definition use Rust 1.99.1 explicitly; the repository's `rust-toolchain.toml` otherwise selects the moving `stable` channel. Do not assume a different toolchain has passed these checks.

Windows builds require the MSVC target, Windows SDK and OpenSSH tools. Linux/macOS builds and native Windows runtime acceptance are distinct. This recovery backup is not a completed release: current regression failures and unvalidated platforms are listed in [the validation status](../docs/VALIDATION_STATUS.md). A workflow definition or successful component build is not proof of full application acceptance.
