# Recovery backup: paused, not a release

This candidate is **not fully passing**. The owner requested a fork recovery backup and then a pause. No upstream PR, new CI run, merge or deployment is authorized by this save. Workflows in this backup have manual/reusable entry points only; saving this branch does not start them. A workflow definition is not a successful run.

## Candidate and useful results

The product source contains the paired native GPUI client, compatible old SSH/PTY TUI, negotiated attachments and quotas, Emoji/image references, and default-off dot integration. Normal native launch attempts `chat.mio.blue:22`; strict host-key validation and a compatible current hello precede explicit application credentials. No production service has been upgraded by this work.

The latest backend change retains bounded per-Store prepared statements (32 ordinary-reader and 8 policy-reader entries). It caches parameterized execution plans, not authorization, arguments, or query results. Every operation still performs fresh permission reads under its original context. Errors fail closed; failed preparation is not retried. The change targets measured repeated SQLite preparation cost, **not a proven cause of every timeout**.

- Ten focused Go race tests plus nine subtests passed for this change, including new bindings, changed schema/role/credential epoch, cancellation, capacity and concurrent closure.
- Four subsequent original Go regressions passed: real SSH VT panel transitions, room UID removal across devices and draft preservation, simple actions, and the unchanged 10,000-message DM query. The current Go fixture and SSH bridge compiled.
- Earlier, identical Rust source passed 140 library, 72 application, 30 pipe and one SSH test, and produced the ordinary application. Those are component results for the earlier backend, not proof that the latest backend passes all native integration.
- The exact packaged Python adapter previously passed 18 synthetic tests. Live provider/OAuth/subscription setup is not included in that result.

## Open, release-blocking failures

The latest product integration ran the real Rust executable against the new Go fixture and reported **28 pipe tests passed and two failed**:

- `real_go_pipe_management_three_receipts_do_not_change_conversation`
- `real_go_pipe_secret_word_start_receipt_and_draft_rejection`

Both received a session-health authorization rejection. The subsequent SSH test and final module verification were not reached. Earlier full Go race runs also had history deadlines, protocol EOFs and VT transitions failing. Original two-CPU and four-CPU failures remain failures; neither CPU count nor the environment has been established as their cause. There is no complete passing full-race result for this final backend.

A separate single-case moderation diagnostic reproduced a bounded-read deadline: it reached the query API at about 0.362 ms and returned at 317.834 ms. The measured cache mutex wait was 0.240 microseconds, with no observed DB-pool waiting or runtime GC/STW interval. Runtime `Running` state is not measured CPU service; trace timestamp normalization further limits exact scheduler-duration conclusions. This does not prove OS throttling, SQL execution time, or a repaired defect. Diagnostic hooks, traces, private reviews and execution-workspace logs are **not included in the product source**.

## Independent CI purpose and unchanged safety limits

A possible later, separately authorized CI run could determine how this exact candidate behaves on separate standard Linux/Windows runners. It must retain the local 300 ms/200 ms read budgets, fail-closed authorization, pressure inputs, expected results and full test selection. The Linux race package has a 25-minute total test timeout; jobs have a 45-minute cap. Neither limit changes individual request deadlines. Failed jobs stay failed, and original explicit skips must not be described as executed tests.

A different-environment pass would establish a pass in that configuration; it would not erase the local failures or automatically explain them. A failure remains a blocker rather than a reason to repeatedly rerun unchanged tests.

## Not validated

Actual Windows GUI/IME/materials/notification delivery; current production `chat.mio.blue` compatibility; production backup/restore; real accounts/uploads; and live dot delivery have not been validated. The old server can accept ordinary SSH while rejecting `miochat-gui-v1`; the paired server update is required. See [upgrade and rollback requirements](PAIRED_UPGRADE.md). This draft does not authorize a deployment.
