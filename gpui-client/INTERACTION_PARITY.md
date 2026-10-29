# Native interaction and compatibility scope

This recovery branch is paused and is not a completed release. The following
features are present in the candidate source; the list is not a claim that every
interaction or platform has passed acceptance. Current regression failures and
unverified areas are disclosed in [validation status](../docs/VALIDATION_STATUS.md).

## Client interactions

- Native rooms, messages, profile and message-action menus are GPUI components,
  not a terminal embedded in a desktop window.
- F1/F4 open help/menu, F2 notifications, F3 contacts and F8 administration when
  available. Ctrl+B toggles the sidebar. PageUp/PageDown move through the reading
  window, and Ctrl+End returns to the latest messages.
- Searchable menus filter action labels and commands using Unicode lowercase
  matching. This is not full Unicode case folding or full-text search of every
  card's metadata. Command/history search remains a separate server feature.
- Input history recalls text without sending it. Tab completion requests a
  bounded token prefix rather than the complete draft; selecting a completion
  does not send a message.
- Reply, edit, recall, report and profile actions use their dedicated controls.
  Sensitive account and moderation forms are separate from the chat composer.
  Server permissions still determine whether an action is available.
- Post/draw editors keep their original target and distinguish editing,
  previewing, saving a draft and publishing. Losing target access must not
  silently publish to another room.
- Unicode Emoji selection explicitly inserts text. Recent Emoji are local
  account-scoped selections, not a record of arbitrary input.

## Media and optional integration

The paired server and native client contain negotiated attachment transfers,
PNG/JPEG and bounded GIF preview, explicit downloads and custom-image references.
Per-item and daily account quotas are enforced by the paired server, not by an
older terminal-only server. Image recent entries are metadata/confirmation, not
a thumbnail gallery. A recall can block later server reads but cannot erase an
already downloaded external file.

The dot adapter and owner-private review interface are off by default. Shipping
these files does not configure a real provider, authentication or subscription.
See [the optional adapter boundary](../docs/DOT_ADAPTER.md).

## Compatibility and known limitations

The matching server preserves ordinary SSH+PTY TUI login/chat/commands alongside
its native SSH subsystem. A terminal-only deployment cannot provide native GUI
login merely because ordinary SSH works. Native appearance modes and Windows
notifications are not identical to terminal palettes and bells.

Real Windows keyboard/IME, visible materials, notification delivery, current
production deployment and live dot behavior remain unverified. The latest
native-to-server regression suite has real session-health failures; no command
catalog count or feature list substitutes for resolving them. This backup does
not start tests or CI, open a pull request, merge, or deploy.
