# MioChat native GPUI client

> **Recovery backup — work paused, local regressions remain FAIL.** This is not a completed release or an upstream PR. See [current validation and open failures](../docs/VALIDATION_STATUS.md). Saving this branch does not run CI.

> 当前新恢复候选：默认自动连接 chat.mio.blue:22，兼容 hello 后显式应用登录；高级设置隐藏 SSH 传输选项。配套服务端与旧 SSH+PTY 共存、备份/迁移及未验范围见 [配套升级说明](../docs/PAIRED_UPGRADE.md)。当前候选仍有回归失败，未部署线上。

The server keeps its original SSH/PTY interface. Native rooms, messages and account forms use the optional miochat-gui-v1 structured SSH subsystem. A server offering only the original terminal interface cannot serve this client without the optional server change. Normal launch automatically attempts chat.mio.blue:22 once. Only explicit --mock or --fixture-driver mode avoids public-server discovery and connection. Application credentials are sent only after trusted transport, a compatible current hello, and an explicit login submission.

## Windows build and local preview

Use the MSVC Rust toolchain, Windows SDK and system OpenSSH tools. See BASELINE.md for the dependency baseline and explicit build toolchain.

```powershell
cd gpui-client
rustup toolchain install 1.99.1-x86_64-pc-windows-msvc --profile minimal --component rustfmt --component clippy
cargo +1.99.1 fmt --all -- --check
cargo +1.99.1 test --locked
cargo +1.99.1 clippy --locked --all-targets -- -D warnings
cargo +1.99.1 build --locked
cargo +1.99.1 run --locked -- --mock
```

The explicitly labelled local mock uses only disposable in-memory fixture events: no sockets, real accounts, password files or persisted credentials. Its controls exercise new messages, delayed messages, edits, recalls, room access loss and disconnect. A fixture send containing /fail is rejected so draft preservation can be inspected. This preview is separate from real SSH compatibility and server tests.

## Appearance and notifications

The toolbar switches between warm Keel paper, native Windows Acrylic and transparent composition. Paper, graphite text, restrained blue focus and compact spacing follow the requested Keel visual direction. High contrast, disabled system transparency, unavailable composition or a rejected native material cause an explicit fallback to opaque paper. No global system setting is changed. Actual effects must be checked on the target Windows session.

New received chat events can post Windows notifications when the app is inactive or the message belongs to another room. History snapshots, own sends, replayed IDs and edits/recalls do not notify. The current protocol subscribes to the selected room; it does not promise notifications for every inactive room. Notifications default to a generic body. The preview switch is in-memory and opt-in. Click activation is checked against the current connection generation and accessible room list. Windows notification settings and Focus Assist may suppress delivery; a successful API call alone does not prove display in the notification center.

## Real connections

Start normally for one automatic connection attempt to `chat.mio.blue:22`. The normal screen accepts the MioChat application username and password only after strict host-key verification and a compatible current server hello. Use Advanced only to change the endpoint, SSH transport user, or optional device key; these are not the application account. Verify any newly scanned host fingerprint with the administrator before explicitly trusting it. An incompatible or unreachable server never receives a queued password. A failed or cancelled connection does not automatically replay credentials. Without a selected device key, OpenSSH uses strict app-local known hosts, no user SSH configuration, no agent/default-private-key authentication, no forwarding or local commands. Passwords are sent only inside the verified SSH connection and are not persisted by the client. This implementation does not open the server database or interpret terminal screen text.

Device-key mode requires the companion `miochat-ssh-bridge.exe` beside the native client executable. Build it from the repository root with `go build -o YOUR_OUTPUT_DIRECTORY/miochat-ssh-bridge.exe ./cmd/miochat-ssh-bridge`. It reuses existing Go SSH dependencies. Select a specific private key in the native connection form; encrypted keys accept a masked passphrase for this connection only. The private initializer travels over the child process's stdin, never command-line arguments, environment variables or a settings file. The bridge enforces the same known-host pins, signs with only the selected key, and opens only `miochat-gui-v1`; it does not discover keys, use an agent, forward ports or run a shell. Unknown/changed host keys and wrong passphrases fail without a fallback identity.

Tests and mock preview use synthetic local accounts and do not log into or change a production server. Deployment, real account operations and live dot setup remain separate from a source pull request.

## Window lifetime

Only the primary MioChat window owns application lifetime. Closing the optional synthetic contrast fixture closes that helper without exiting the main window. Closing the main window exits the application and its remaining helpers. Unknown or duplicate close events cannot trigger another shutdown.

## Current paired-client scope and verification limits

The paired server preserves standard SSH + PTY application login, chat and original commands alongside the structured native subsystem. The native component includes the 139-entry command catalog plus its documented aliases, message/profile actions, paging, searchable menus, input history/Tab completion, post/draw editors, moderation/maintenance and sensitive account forms. Catalog coverage is not a claim of physical GUI or deployment acceptance.

New features include Unicode Emoji insertion, explicit image/file selection or image paste, PNG/JPEG/GIF validation and preview, explicit downloads, custom image references, per-item 20,000,000-byte and per-account daily 100,000,000-byte server limits. Daily time-zone/accounting details and backup/rollback requirements are in [the paired upgrade guide](../docs/PAIRED_UPGRADE.md). These require the matching server, not a terminal-only server or a client-side quota promise.

The default-off dot adapter and private review UI are included as code; [live host/authentication/subscription setup remains separate](../docs/DOT_ADAPTER.md). Cloud synthetic tests do not verify actual Windows appearance, IME, notification delivery or current production configuration. The Windows CI job compiles and runs explicit synthetic fixtures; its presence alone is not a successful run. Consult the exact saved commit and its validation status; this backup does not create a PR or run CI.
