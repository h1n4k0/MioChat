# Composition drafts: versioned save and recovery

This document describes the shared TUI and native editor contract. It is not a claim that the current candidate has passed complete validation; see [current failures and limits](VALIDATION_STATUS.md).

## Data and authorization

Schema 5 adds `composition_versions` while preserving the original six composition columns. Existing drafts receive a positive version; absent drafts initially have real version 0. Inserts, updates and deletes advance durable versions, including tombstones after deletion. Version decrement/deletion and integer overflow are rejected. Legacy six-column composition inserts participate in version advancement.

Actual TUI load/reserve/save/suspend/publish passes the current `m.client.ID`. Mutations recheck session ownership, credential/key generation, reset/ban state and current policy inside the same writer. Normal draft save requires readable origin but not permission to send; suspended save may update only the same owner's preexisting exact-version draft and never project a remote body after access loss. Publication additionally checks sending policy. Empty-session unversioned wrappers remain only for compatible fixtures, not actual TUI editing paths.

Opening is load then exact-version reserve, so title prefill cannot overwrite an intervening save. Publication creates the object and clears only the expected draft in one transaction. Required affected-row checks, draft/version/object readback and rollback prevent ignored writes from falsely succeeding. Commit uncertainty does not clear the local editor or automatically retry publication. Announcements remain best-effort after a successful commit.

## TUI state

The editor retains immutable room/kind and raw input separately from the canonical stored snapshot/version. Normalization of tabs, ANSI/control characters and trailing newlines does not cause every autosave tick to rewrite identical raw input.

- F2: preview/edit; F4: publish; Esc: save successfully then close.
- F6: read the current authorized remote version number without replacing local input (remote text is not previewed); distinct F8 rechecks authority and that same version before explicitly reloading. Repeated F6 never discards local input.
- F7 after F6: keep local text against the captured remote version. Another intervening save conflicts again; no automatic newest-version retry.
- F9 then F10: explicitly discard local editor state only; durable drafts are not deleted. Different confirmation keys prevent repeating/holding the first key from confirming. Other keys cancel the armed discard. Ctrl+D retains ordinary text-editor behavior.
- Ctrl+C refuses to quit while a dirty composition save fails; the editor remains available for recovery or explicit local discard.
- Pointer controls use the actual rendered row and display-cell label ranges, dispatching the same key handlers.

Original 32-line, 160-rune line and 4096-rune nonnewline limits remain. Art drafts can contain Chinese; publication requires printable ASCII. Forced room/ACL changes cannot retarget content to lobby or a DM. A network/OS force-kill still cannot guarantee recovery of unacknowledged input.

## Native editor

The dedicated `composition` frame carries operation, immutable original room/kind,
optional expected version (explicit zero is meaningful), and local edit generation.
Neither body nor title travels through `command`, chat-draft, or generic PendingSend.
Reads require the actual channel; the only cross-scope action is a session-authorized
update of an already-existing own suspended draft. No remembered channel is used.

The native editor loads then reserves, permits one outstanding operation, and
keeps raw submitted text distinct from the server's canonical stored snapshot.
A save confirms only its captured edit generation; newer input remains dirty.
Publication atomically creates and clears, never retries an unknown result, and
cannot clear input entered after submission. Ordinary room navigation and window
close await exact-generation save confirmation. A failed save retains the editor;
forced credential/connection changes retain owner-isolated local text, do not render
it on credential/other-account screens, and require explicit origin recovery.
This is in-memory recovery only, not a guarantee after process kill or power loss.

F2 preview, F4 publish, Ctrl+S save, Esc save-and-return, F6 remote preview,
F7 overwrite the captured remote version, F8 reload that version, and distinct
F9/F10 local-discard confirmation have pointer equivalents. IME composition and
held control keys do not publish or confirm. Body Tab inserts four spaces;
Shift+Tab/Ctrl+Tab switch fields. The multiline element uses GPUI shaped lines for
rendering, mouse selection, arrows, horizontal scrolling and platform IME geometry;
no estimated 7/13-pixel cursor is used for the new body input. Explicit newlines and
160-column ASCII are not auto-wrapped/truncated. Server limits/canonicalization and
printable-ASCII publication remain authoritative.

## Scope changes and recovery

Accepted scope-change routes synchronously freeze the editor before processing the
next protocol frame, cancel read/reserve intents and clear remote preview. They retain
only eligible submitted save/publication confirmations. Unmatched old frames do not
cancel a newer close intent. Mouse-down and drag share the UTF-8 selection/anchor/caret
helper, including reversed selection. Typed opening commands are consumed only after
an accepted matching reserve and only if the original chat input and scope remain
unchanged; menus preserve the conversation draft.

Recovery additionally requires the exact frozen session connection identity: validated
host spelling/port/SSH user, authentication mode/identity path, and the matched trusted
key set in the application's dedicated known_hosts. This is connection and locally
trusted identity context, not a claim to the SSH negotiated fingerprint. It is frozen
at open_session, not recomputed from editable connection fields by the renderer.
Unknown/empty context hides and refuses recovery; another endpoint, key context or
same-named account on a different server cannot display/copy/rebind the text. Mock and
fixture domains have separate tags and per-open nonces. No network probing or private
key read is added by this recovery mechanism; OpenSSH strict checking remains intact.
