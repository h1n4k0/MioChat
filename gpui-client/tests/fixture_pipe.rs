use miochat_gpui::{
    persistence::{Ack, Operation, Persistence, Position, Snapshot},
    session::{Request, Response, Session, SessionEvent},
};
use std::{
    path::PathBuf,
    time::{Duration, Instant},
};

fn read(session: &Session, kind: &str, id: &str) -> Response {
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        match session
            .events()
            .recv_timeout(deadline.saturating_duration_since(Instant::now()))
            .expect("fixture response timed out")
        {
            SessionEvent::Response { response, .. }
                if response.kind == kind && (id.is_empty() || response.request_id == id) =>
            {
                return *response;
            }
            SessionEvent::Response { response, .. } => {
                assert_ne!(response.kind, "error", "fixture error: {}", response.error)
            }
            event => panic!("fixture transport failed: {event:?}"),
        }
    }
}
fn snapshot(response: Response) -> (i64, Snapshot, String) {
    let draft = response.draft.expect("typed draft");
    (
        draft.version,
        Snapshot {
            body: draft.body,
            reply_to: draft.reply_to,
            position: draft.position,
        },
        draft.backup_id,
    )
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_secret_admin_one_time_results_never_touch_chat_drafts() {
    use miochat_gpui::{
        composition::Origin,
        secret_admin::{Attempt, Binding, Kind, State},
    };
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 73).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "secret-admin-login");
    login.username = "fixture-admin";
    login.password = "fixture-admin-only";
    session.send(&login).unwrap();
    read(&session, "auth", "secret-admin-login");
    read(&session, "state", "");
    let mut load = Request::simple("draft_load", "secret-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = snapshot(read(&session, "draft", "secret-before"));
    for (id, kind, target, body) in [
        ("secret-invite", Kind::Invitecode, "1d", "2"),
        ("secret-reset", Kind::Resetpass, "fixture-peer", ""),
    ] {
        std::thread::sleep(Duration::from_millis(750));
        let data = kind.data(target, body).unwrap();
        let mut attempt = Attempt::new(
            data.clone(),
            id.into(),
            Binding {
                owner: "fixture-admin".into(),
                room: "lobby".into(),
                generation: 73,
                origin: Origin::synthetic("fixture", 73),
                epoch: 1,
            },
            0,
            None,
        );
        let mut request = Request::simple("secret_admin", id);
        request.room = "lobby";
        request.secret_admin = Some(&data);
        session.send(&request).unwrap();
        let response = read(&session, "secret_admin", id);
        assert!(attempt.receive(&response, 1));
        assert_eq!(attempt.state, State::Shown);
        assert_eq!(attempt.value(1).unwrap().len(), 32);
        assert!(!format!("{response:?}").contains(attempt.value(1).unwrap()));
        assert!(attempt.expire(60_001));
        assert!(attempt.value(60_001).is_none());
        assert!(!attempt.receive(&response, 60_002));
    }
    load.request_id = "secret-after";
    session.send(&load).unwrap();
    let after = snapshot(read(&session, "draft", "secret-after"));
    assert_eq!(before, after);
    std::thread::sleep(Duration::from_millis(750));
    let mut generic = Request::simple("command", "no-generic-secret");
    generic.room = "lobby";
    generic.body = "/resetpass fixture-peer";
    session.send(&generic).unwrap();
    let rejected = read(&session, "error", "no-generic-secret");
    assert!(rejected.secret_admin.is_none());
    assert!(rejected.panel.is_none());
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_one_off_dm_keeps_active_scope_and_draft() {
    use miochat_gpui::direct_message::message_belongs_to_view;
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 47).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "one-off-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "one-off-login");
    let initial = read(&session, "state", "");
    let mut load = Request::simple("draft_load", "one-off-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = snapshot(read(&session, "draft", "one-off-before"));
    std::thread::sleep(Duration::from_millis(750));
    let mut send = Request::simple("command", "one-off-send");
    send.room = "lobby";
    send.username = "fixture-peer";
    send.body = "/msg fixture-peer synthetic one-off DM";
    session.send(&send).unwrap();
    let result = read(&session, "action", "one-off-send");
    assert_eq!(result.room, "lobby");
    assert!(!result.replace && result.draft.is_none());
    let target = result.direct_target.as_ref().unwrap();
    assert_eq!(target.requested, "fixture-peer");
    assert_eq!(target.canonical, "fixture-peer");
    let message = result.message.unwrap();
    assert_eq!(message.sender, "preview");
    assert_eq!(message.recipient, "fixture-peer");
    assert_eq!(message.room, "dm:fixture-peer");
    assert!(!message_belongs_to_view(&message.room, &result.room));
    std::thread::sleep(Duration::from_millis(750));
    let sync = Request::simple("sync", "one-off-still-lobby");
    session.send(&sync).unwrap();
    let state = read(&session, "state", "one-off-still-lobby");
    assert_eq!(state.room, "lobby");
    assert_eq!(state.messages, initial.messages);
    std::thread::sleep(Duration::from_millis(750));
    load.request_id = "one-off-after";
    session.send(&load).unwrap();
    assert_eq!(snapshot(read(&session, "draft", "one-off-after")), before);
    std::thread::sleep(Duration::from_millis(750));
    let mut history = Request::simple("command", "one-off-dm");
    history.room = "lobby";
    history.body = "/dm fixture-peer";
    session.send(&history).unwrap();
    assert_eq!(
        read(&session, "action", "one-off-dm").room,
        "dm:fixture-peer"
    );
    let state = read(&session, "state", "");
    assert!(
        state
            .messages
            .iter()
            .any(|m| m.id == message.id && m.body == message.body)
    );
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_clear_preserves_draft_and_restores_history() {
    let path = PathBuf::from(
        std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture driver"),
    );
    let mut session = Session::fixture(&path, 43).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "clear-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "clear-login");
    let initial = read(&session, "state", "");
    assert!(!initial.messages.is_empty());
    let mut load = Request::simple("draft_load", "clear-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = snapshot(read(&session, "draft", "clear-before"));
    for id in ["clear-first", "clear-repeat"] {
        std::thread::sleep(Duration::from_millis(750));
        let mut clear = Request::simple("command", id);
        clear.room = "lobby";
        clear.body = "/clear";
        session.send(&clear).unwrap();
        let result = read(&session, "action", id);
        assert!(result.clear_view);
        assert!(result.at_latest);
        assert_eq!(result.room, "lobby");
        assert!(result.messages.is_empty());
        assert!(result.message.is_none());
        assert!(result.draft.is_none());
    }
    std::thread::sleep(Duration::from_millis(750));
    let mut load = Request::simple("draft_load", "clear-after");
    load.room = "lobby";
    session.send(&load).unwrap();
    assert_eq!(snapshot(read(&session, "draft", "clear-after")), before);
    std::thread::sleep(Duration::from_millis(750));
    let mut history = Request::simple("command", "clear-restore");
    history.room = "lobby";
    history.body = "/history";
    session.send(&history).unwrap();
    let restored = read(&session, "action", "clear-restore");
    assert!(!restored.clear_view);
    assert!(restored.replace);
    assert!(restored.at_latest);
    assert_eq!(restored.messages, initial.messages);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_drafts_positions_conflict_and_scope_switch() {
    let path = PathBuf::from(
        std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture driver path"),
    );
    let mut session = Session::fixture(&path, 19).expect("fixture start");
    let hello = read(&session, "hello", "");
    assert!(hello.capabilities.iter().any(|c| c == "draft-position-v1"));
    let mut login = Request::simple("login", "login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "login");
    let state = read(&session, "state", "");
    assert_eq!(state.messages.len(), 100);
    assert!(state.at_latest);
    let mut p = Persistence::default();
    p.activate("synthetic-pipe", "lobby", 0);
    let op = p.next(0).unwrap();
    p.begin("load".into(), op.clone(), 0);
    session.send_persistence(&op, "load").unwrap();
    let (version, original, backup) = snapshot(read(&session, "draft", "load"));
    assert_eq!(original.body, "合成草稿第一行\n第二行");
    assert!(original.position.anchor > 0);
    assert_eq!(original.position.offset, 1);
    let update = p.acknowledge(
        ("load", "lobby"),
        Ack::Loaded,
        version,
        original.clone(),
        backup,
        1,
    );
    assert!(update.restore_position);
    assert_eq!(update.apply, Some(original.clone()));
    let mut window = Request::simple("view_window", "window");
    window.room = "lobby";
    window.position = Some(&original.position);
    session.send(&window).unwrap();
    let restored = read(&session, "window", "window");
    assert_eq!(restored.messages.len(), 200);
    assert_eq!(restored.messages[0].id, original.position.anchor);
    assert!(!restored.at_latest);
    let mut local = original.clone();
    local.body = "changed first\n  changed second\t ".into();
    local.position.offset = 3;
    p.observe(local.clone(), 10);
    let op = p.next(810).unwrap();
    p.begin("save".into(), op.clone(), 810);
    session.send_persistence(&op, "save").unwrap();
    let (version, saved, backup) = snapshot(read(&session, "draft_saved", "save"));
    assert_eq!(saved, local);
    p.acknowledge(("save", "lobby"), Ack::Saved, version, saved, backup, 811);
    assert!(!p.dirty());
    let stale = Operation::Save {
        room: "lobby".into(),
        version: 0,
        snapshot: Snapshot {
            body: "stale other device".into(),
            ..Default::default()
        },
        keep: false,
        backup_id: String::new(),
    };
    session.send_persistence(&stale, "stale").unwrap();
    let conflict = read(&session, "draft_conflict", "stale");
    assert!(conflict.draft.unwrap().conflict);
    // Respect the unchanged production protocol's six-token, 1.5/s bucket.
    std::thread::sleep(Duration::from_millis(1400));
    let mut select = Request::simple("select_room", "dev");
    select.room = "dev";
    session.send(&select).unwrap();
    read(&session, "state", "dev");
    assert!(p.activate("synthetic-pipe", "dev", 900).body.is_empty());
    let op = p.next(900).unwrap();
    session.send_persistence(&op, "dev-load").unwrap();
    let (_, dev, _) = snapshot(read(&session, "draft", "dev-load"));
    assert!(dev.body.is_empty());
    std::thread::sleep(Duration::from_millis(1400));
    let mut select = Request::simple("select_room", "back");
    select.room = "lobby";
    session.send(&select).unwrap();
    read(&session, "state", "back");
    session
        .send_persistence(
            &Operation::Load {
                room: "lobby".into(),
                manual: false,
            },
            "back-load",
        )
        .unwrap();
    let (_, reloaded, _) = snapshot(read(&session, "draft", "back-load"));
    assert_eq!(reloaded, local);
    let mut latest = Request::simple("view_window", "latest");
    latest.room = "lobby";
    let position = Position::default();
    latest.position = Some(&position);
    session.send(&latest).unwrap();
    let page = read(&session, "window", "latest");
    assert!(page.at_latest);
    assert_eq!(page.messages.len(), 100);
    let mut exit = Request::simple("command", "native-exit");
    exit.room = "lobby";
    exit.body = "/exit";
    session.send(&exit).unwrap();
    assert_eq!(read(&session, "action", "native-exit").status, "bye");
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_room_components_permissions_and_leave() {
    let path = PathBuf::from(
        std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture driver"),
    );
    let mut session = Session::fixture(&path, 23).expect("fixture start");
    let hello = read(&session, "hello", "");
    assert!(hello.capabilities.iter().any(|c| c == "native-actions-v1"));
    let mut login = Request::simple("login", "login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "login");
    read(&session, "state", "");
    let command = |body: &str, room: &str, id: &str, kind: &str| {
        std::thread::sleep(Duration::from_millis(750));
        let mut request = Request::simple("command", id);
        request.room = room;
        request.body = body;
        session.send(&request).unwrap();
        read(&session, kind, id)
    };
    let panel = command("/rooms", "lobby", "directory", "action")
        .panel
        .unwrap();
    assert_eq!(panel.kind, "rooms");
    let room = panel
        .rooms
        .iter()
        .find(|r| r.name == "fixture-room")
        .expect("invited private room");
    assert!(room.private);
    assert!(!room.can_manage);
    assert!(room.can_leave);
    assert_eq!(
        command("/join fixture-room", "lobby", "join", "action").room,
        "fixture-room"
    );
    read(&session, "state", "");
    let panel = command("/members", "fixture-room", "members", "action")
        .panel
        .unwrap();
    assert_eq!(panel.scope, "fixture-room");
    assert_eq!(panel.members.len(), 2);
    assert!(panel.members.iter().all(|m| !m.can_remove));
    command("/invite fixture-peer", "fixture-room", "denied", "error");
    assert_eq!(
        command("/leave", "fixture-room", "leave", "action").room,
        "lobby"
    );
    read(&session, "state", "");
    command("/join fixture-room", "lobby", "revoked", "error");
    let panel = command("/rooms", "lobby", "after-leave", "action")
        .panel
        .unwrap();
    assert!(panel.rooms.iter().all(|r| r.name != "fixture-room"));
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_account_preferences_and_draft_isolation() {
    let path = PathBuf::from(
        std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture driver"),
    );
    let mut session = Session::fixture(&path, 27).unwrap();
    assert!(
        read(&session, "hello", "")
            .capabilities
            .iter()
            .any(|c| c == "preferences-v1")
    );
    let mut login = Request::simple("login", "pref-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    let auth = read(&session, "auth", "pref-login");
    assert!(auth.key_reminder);
    let prefs = auth.preferences.unwrap();
    assert_eq!(prefs.user, "preview");
    assert_eq!(prefs.settings.len(), 9);
    assert_eq!(prefs.key_count, 0);
    read(&session, "state", "");
    let mut load = Request::simple("draft_load", "draft-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = read(&session, "draft", "draft-before").draft.unwrap();
    for (index, body) in [
        "/dnd on",
        "/pref compact on",
        "/theme sakura",
        "/pref key_reminder off",
    ]
    .iter()
    .enumerate()
    {
        std::thread::sleep(Duration::from_millis(750));
        let id = format!("pref-{index}");
        let mut request = Request::simple("command", &id);
        request.room = "lobby";
        request.body = body;
        request.username = "fixture-peer";
        session.send(&request).unwrap();
        let p = read(&session, "action", &id).preferences.unwrap();
        assert_eq!(p.user, "preview");
    }
    std::thread::sleep(Duration::from_millis(750));
    let mut request = Request::simple("preferences", "pref-current");
    request.username = "fixture-peer";
    session.send(&request).unwrap();
    let prefs = read(&session, "preferences", "pref-current")
        .preferences
        .unwrap();
    assert!(prefs.on("dnd"));
    assert!(prefs.on("compact"));
    assert_eq!(prefs.theme, "sakura");
    assert!(!prefs.reminder());
    let mut load = Request::simple("draft_load", "draft-after");
    load.room = "lobby";
    session.send(&load).unwrap();
    let after = read(&session, "draft", "draft-after").draft.unwrap();
    assert_eq!(
        (after.body, after.version, after.reply_to),
        (before.body, before.version, before.reply_to)
    );
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_control_center_contacts_read_history_and_draft() {
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 31).unwrap();
    assert!(
        read(&session, "hello", "")
            .capabilities
            .iter()
            .any(|c| c == "control-center-v1")
    );
    let mut login = Request::simple("login", "center-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "center-login");
    read(&session, "state", "");
    let mut load = Request::simple("control_center", "load-center");
    load.username = "fixture-peer";
    session.send(&load).unwrap();
    let center = read(&session, "control_center", "load-center")
        .center
        .unwrap();
    assert_eq!(center.user, "preview");
    assert!(
        center
            .contacts
            .iter()
            .any(|c| c.group == "requests" && c.name == "fixture-peer")
    );
    let count = center.notices.len();
    assert!(center.unread > 0);
    let mut load = Request::simple("draft_load", "center-draft-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = read(&session, "draft", "center-draft-before")
        .draft
        .unwrap();
    for (i, body) in ["/accept fixture-peer", "/notifications", "/notifyclear"]
        .into_iter()
        .enumerate()
    {
        std::thread::sleep(Duration::from_millis(750));
        let id = format!("center-action-{i}");
        let mut request = Request::simple("command", &id);
        request.room = "lobby";
        request.body = body;
        request.username = "fixture-peer";
        session.send(&request).unwrap();
        let center = read(&session, "action", &id).center.unwrap();
        assert_eq!(center.user, "preview");
        assert!(
            center
                .contacts
                .iter()
                .any(|c| c.group == "friends" && c.name == "fixture-peer")
        );
        assert!(!center.contacts.iter().any(|c| c.group == "requests"));
        if i == 2 {
            assert_eq!(center.unread, 0);
            assert_eq!(center.notices.len(), count);
            assert!(center.notices.iter().all(|n| n.seen));
        }
    }
    let mut load = Request::simple("draft_load", "center-draft-after");
    load.room = "lobby";
    session.send(&load).unwrap();
    let after = read(&session, "draft", "center-draft-after").draft.unwrap();
    assert_eq!(
        (before.body, before.version, before.reply_to),
        (after.body, after.version, after.reply_to)
    );
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_empty_control_center() {
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 37).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "empty-login");
    login.username = "fixture-peer";
    login.password = "fixture-peer-only";
    session.send(&login).unwrap();
    read(&session, "auth", "empty-login");
    read(&session, "state", "");
    session
        .send(&Request::simple("control_center", "empty-center"))
        .unwrap();
    let c = read(&session, "control_center", "empty-center")
        .center
        .unwrap();
    assert_eq!(c.user, "fixture-peer");
    assert_eq!(c.unread, 0);
    assert!(c.contacts.is_empty() && c.notices.is_empty());
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_profile_private_note_blocking_and_own_edit() {
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 41).unwrap();
    assert!(
        read(&session, "hello", "")
            .capabilities
            .iter()
            .any(|c| c == "profile-v1")
    );
    let mut login = Request::simple("login", "profile-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "profile-login");
    read(&session, "state", "");
    let mut load = Request::simple("draft_load", "profile-draft-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = read(&session, "draft", "profile-draft-before")
        .draft
        .unwrap();
    for (i, body) in [
        "/note fixture-peer only my note",
        "/follow fixture-peer",
        "/friend fixture-peer",
        "/ignore fixture-peer",
        "/unignore fixture-peer",
        "/bio own edited bio",
    ]
    .into_iter()
    .enumerate()
    {
        std::thread::sleep(Duration::from_millis(750));
        let id = format!("profile-change-{i}");
        let mut request = Request::simple("command", &id);
        request.room = "lobby";
        request.body = body;
        request.username = "fixture-peer";
        session.send(&request).unwrap();
        let p = read(&session, "action", &id).profile.unwrap();
        assert_eq!(p.viewer, "preview");
        if i < 5 {
            assert_eq!(p.name, "fixture-peer");
            assert!(!p.own);
            assert_eq!(p.note, "only my note");
        } else {
            assert_eq!(p.name, "preview");
            assert!(p.own);
            assert_eq!(p.bio, "own edited bio");
        }
        if i == 3 {
            assert!(p.blocked);
        }
        if i == 4 {
            assert!(!p.blocked);
        }
    }
    let mut request = Request::simple("profile", "peer-after");
    request.body = "fixture-peer";
    request.username = "fixture-peer";
    session.send(&request).unwrap();
    let p = read(&session, "profile", "peer-after").profile.unwrap();
    assert_eq!(p.viewer, "preview");
    assert_eq!(p.bio, "");
    assert_eq!(p.note, "only my note");
    std::thread::sleep(Duration::from_millis(750));
    let mut load = Request::simple("draft_load", "profile-draft-after");
    load.room = "lobby";
    session.send(&load).unwrap();
    let after = read(&session, "draft", "profile-draft-after")
        .draft
        .unwrap();
    assert_eq!(
        (before.body, before.version, before.reply_to),
        (after.body, after.version, after.reply_to)
    );
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_avatar_afk_badges_mention_poke_and_draft() {
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 43).unwrap();
    assert!(
        read(&session, "hello", "")
            .capabilities
            .iter()
            .any(|c| c == "presence-v1")
    );
    let mut login = Request::simple("login", "presence-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "presence-login");
    read(&session, "state", "");
    let mut load = Request::simple("draft_load", "presence-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = read(&session, "draft", "presence-before").draft.unwrap();
    for (i, body) in [
        "/avatar ident",
        "/afk local test",
        "/badges",
        "/mention fixture-peer",
        "/poke fixture-peer",
        "/poke fixture-peer",
        "/back",
    ]
    .into_iter()
    .enumerate()
    {
        std::thread::sleep(Duration::from_millis(750));
        let id = format!("presence-{i}");
        let mut request = Request::simple("command", &id);
        request.room = "lobby";
        request.username = "fixture-peer";
        request.body = body;
        session.send(&request).unwrap();
        let response = read(&session, if i == 5 { "error" } else { "action" }, &id);
        match i {
            0 => assert_eq!(response.profile.unwrap().avatar_kind, "ident"),
            1 => assert_eq!(response.profile.unwrap().status, "AFK · local test"),
            2 => assert!(
                response
                    .profile
                    .unwrap()
                    .badges
                    .iter()
                    .any(|b| b == "synthetic-achievement")
            ),
            3 => {
                assert_eq!(response.mention, "fixture-peer");
                assert!(response.message.is_none());
                let mut text = before.body.clone();
                let cursor = text.len();
                assert!(
                    miochat_gpui::profile::insert_mention(&mut text, cursor, &response.mention)
                        .is_some()
                );
                assert!(text.starts_with(&before.body) && text.ends_with("@fixture-peer "));
            }
            4 => assert!(response.status.contains("fixture-peer")),
            5 => assert!(response.error.contains("30")),
            _ => assert_eq!(response.room, "lobby"),
        }
    }
    let mut profile = Request::simple("profile", "presence-final");
    profile.body = "preview";
    session.send(&profile).unwrap();
    assert!(
        read(&session, "profile", "presence-final")
            .profile
            .unwrap()
            .status
            .is_empty()
    );
    std::thread::sleep(Duration::from_millis(750));
    let mut load = Request::simple("draft_load", "presence-after");
    load.room = "lobby";
    session.send(&load).unwrap();
    let after = read(&session, "draft", "presence-after").draft.unwrap();
    assert_eq!(
        (before.body, before.version, before.reply_to),
        (after.body, after.version, after.reply_to)
    );
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_room_management_transfer_flags_and_draft() {
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 47).unwrap();
    assert!(
        read(&session, "hello", "")
            .capabilities
            .iter()
            .any(|c| c == "room-management-v1")
    );
    let mut login = Request::simple("login", "management-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "management-login");
    read(&session, "state", "");
    let mut load = Request::simple("draft_load", "management-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = read(&session, "draft", "management-before").draft.unwrap();
    let mut scope = "lobby".to_string();
    for (i, body) in [
        "/create managed-native private",
        "/roomset rules actual room rules",
        "/roomset slow 12",
        "/roommod fixture-peer",
        "/unmod fixture-peer",
        "/roompin on",
        "/subscribe on",
        "/roommute on",
        "/archive",
        "/unarchive",
        "/roomset transfer fixture-peer",
        "/leave",
    ]
    .into_iter()
    .enumerate()
    {
        std::thread::sleep(Duration::from_millis(750));
        let id = format!("management-{i}");
        let mut request = Request::simple("command", &id);
        request.room = &scope;
        request.body = body;
        request.username = "fixture-peer";
        session.send(&request).unwrap();
        let response = read(&session, "action", &id);
        scope = response.room;
        if i == 1 {
            assert_eq!(
                response
                    .panel
                    .as_ref()
                    .unwrap()
                    .details
                    .as_ref()
                    .unwrap()
                    .rules,
                "actual room rules"
            );
        }
        if i == 7 {
            let p = response
                .preferences
                .as_ref()
                .unwrap()
                .rooms
                .get("managed-native")
                .unwrap();
            assert!(p.pinned && p.subscribed && p.muted);
        }
        if i == 8 {
            assert!(
                response
                    .panel
                    .as_ref()
                    .unwrap()
                    .details
                    .as_ref()
                    .unwrap()
                    .archived
            );
        }
        if i == 9 {
            assert!(
                !response
                    .panel
                    .as_ref()
                    .unwrap()
                    .details
                    .as_ref()
                    .unwrap()
                    .archived
            );
        }
        if i == 10 {
            let room = response.panel.as_ref().unwrap().details.as_ref().unwrap();
            assert_eq!(room.owner, "fixture-peer");
            assert!(!room.can_manage && room.can_leave);
        }
    }
    assert_eq!(scope, "lobby");
    std::thread::sleep(Duration::from_millis(750));
    let mut load = Request::simple("draft_load", "management-after");
    load.room = "lobby";
    session.send(&load).unwrap();
    let after = read(&session, "draft", "management-after").draft.unwrap();
    assert_eq!((before.body, before.version), (after.body, after.version));
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_information_panels_preserve_chat_and_draft() {
    use miochat_gpui::information::accepts_information_ack;
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 49).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "information-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "information-login");
    let initial = read(&session, "state", "");
    let mut load = Request::simple("draft_load", "information-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = snapshot(read(&session, "draft", "information-before"));
    for command in ["/stats", "/time", "/art"] {
        std::thread::sleep(Duration::from_millis(750));
        let mut request = Request::simple("command", command);
        request.room = "lobby";
        request.body = command;
        session.send(&request).unwrap();
        let response = read(&session, "action", command);
        assert!(accepts_information_ack(&response, "lobby", "lobby"));
        let panel = response.panel.unwrap();
        assert_eq!(panel.kind, &command[1..]);
        match command {
            "/stats" => {
                let stats = panel.statistics.unwrap();
                assert!(
                    stats.users >= 2
                        && stats.messages > 0
                        && stats.active_sessions > 0
                        && stats.online_sessions > 0
                );
            }
            "/time" => {
                assert!(panel.server_time.ends_with(" CST") && panel.server_time.len() == 23)
            }
            "/art" => {
                assert_eq!(panel.lines.len(), 1);
                assert!(panel.lines[0].starts_with("        ."));
                assert!(panel.lines[0].contains("M I O C H A T   C A M P"));
            }
            _ => unreachable!(),
        }
    }
    std::thread::sleep(Duration::from_millis(750));
    session
        .send(&Request::simple("sync", "information-after-state"))
        .unwrap();
    let state = read(&session, "state", "information-after-state");
    assert_eq!(state.room, initial.room);
    assert_eq!(state.messages, initial.messages);
    std::thread::sleep(Duration::from_millis(750));
    load.request_id = "information-after";
    session.send(&load).unwrap();
    assert_eq!(
        snapshot(read(&session, "draft", "information-after")),
        before
    );
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_simple_actions_keep_draft_and_original_scope() {
    use miochat_gpui::simple_actions::SimpleAction;
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 51).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "play-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "play-login");
    read(&session, "state", "");
    let mut load = Request::simple("draft_load", "play-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = snapshot(read(&session, "draft", "play-before"));
    for command in [
        "/me synthetic action",
        "/roll 2",
        "/coin",
        "/8ball synthetic question",
        "/shrug",
    ] {
        std::thread::sleep(Duration::from_millis(750));
        let mut request = Request::simple("command", command);
        request.room = "lobby";
        request.body = command;
        session.send(&request).unwrap();
        let response = read(&session, "action", command);
        let action = SimpleAction::parse(command).unwrap();
        assert!(action.accepts_ack(&response, "preview", "lobby"));
        assert!(!action.accepts_ack(&response, "preview", "dm:fixture-peer"));
        let message = response.message.unwrap();
        match action {
            SimpleAction::Me => assert_eq!(message.body, "synthetic action"),
            SimpleAction::Roll => assert!(
                message.body == "掷出了 1（2 面骰）" || message.body == "掷出了 2（2 面骰）"
            ),
            SimpleAction::Coin => {
                assert!(message.body == "抛硬币：正面" || message.body == "抛硬币：反面")
            }
            SimpleAction::EightBall => assert!(
                message
                    .body
                    .starts_with("问：synthetic question / 八号球：")
            ),
            SimpleAction::Shrug => assert_eq!(message.body, "¯\\_(ツ)_/¯"),
        }
    }
    std::thread::sleep(Duration::from_millis(750));
    load.request_id = "play-after";
    session.send(&load).unwrap();
    assert_eq!(snapshot(read(&session, "draft", "play-after")), before);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_community_typed_reads_and_explicit_daily_insertion() {
    use miochat_gpui::{community::insert_daily_topic, information::accepts_information_ack};
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 53).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "community-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "community-login");
    let initial = read(&session, "state", "");
    let mut load = Request::simple("draft_load", "community-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = snapshot(read(&session, "draft", "community-before"));
    let command = |session: &Session, body: &str| {
        std::thread::sleep(Duration::from_millis(750));
        let mut request = Request::simple("command", body);
        request.room = "lobby";
        request.body = body;
        session.send(&request).unwrap();
        let response = read(session, "action", body);
        assert!(accepts_information_ack(&response, "lobby", "lobby"));
        response.panel.unwrap()
    };
    let board = command(&session, "/board");
    let post = board.community.unwrap().objects[0].id;
    let detail = command(&session, &format!("/object {post}"));
    assert_eq!(
        detail.community.unwrap().detail.unwrap().body,
        "fixture community body"
    );
    let gallery = command(&session, "/gallery");
    assert_eq!(gallery.community.unwrap().objects[0].kind, "art");
    let polls = command(&session, "/results");
    let poll = polls.community.unwrap().objects[0].id;
    let results = command(&session, &format!("/results {poll}"));
    assert_eq!(results.community.unwrap().detail.unwrap().options.len(), 2);
    let events = command(&session, "/events");
    let event = events.community.unwrap().objects[0].id;
    let participants = command(&session, &format!("/participants {event}"));
    assert_eq!(
        participants
            .community
            .unwrap()
            .detail
            .unwrap()
            .participant_count,
        0
    );
    let objects = command(&session, "/participants");
    let word = objects
        .community
        .unwrap()
        .objects
        .into_iter()
        .find(|o| o.kind == "word")
        .unwrap()
        .id;
    let game = command(&session, &format!("/object {word}"));
    assert!(
        game.community
            .unwrap()
            .detail
            .unwrap()
            .game
            .unwrap()
            .secret
            .is_empty()
    );
    let daily = command(&session, "/daily");
    assert_eq!(daily.actions[0].command, "local:insert-daily");
    let topic = daily.community.unwrap().daily_topic;
    let mut draft = "kept existing draft".to_string();
    let mut selection = draft.len()..draft.len();
    assert!(insert_daily_topic(
        &mut draft,
        &mut selection,
        &topic,
        false,
        "lobby",
        "lobby"
    ));
    assert!(draft.starts_with("kept existing draft"));
    let menu = command(&session, "/commands");
    assert!(menu.actions.iter().any(|a| a.command.trim() == "/board"));
    std::thread::sleep(Duration::from_millis(750));
    load.request_id = "community-after";
    session.send(&load).unwrap();
    assert_eq!(snapshot(read(&session, "draft", "community-after")), before);
    std::thread::sleep(Duration::from_millis(750));
    session
        .send(&Request::simple("sync", "community-state"))
        .unwrap();
    let state = read(&session, "state", "community-state");
    assert_eq!(state.messages, initial.messages);
    assert_eq!(state.room, initial.room);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_community_mutations_receipts_and_preserved_draft() {
    use miochat_gpui::community_mutation::CommunityIntent;
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 59).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "write-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "write-login");
    read(&session, "state", "");
    let mut load = Request::simple("draft_load", "write-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = snapshot(read(&session, "draft", "write-before"));
    let command = |body: &str| {
        std::thread::sleep(Duration::from_millis(750));
        let mut request = Request::simple("command", body);
        request.room = "lobby";
        request.expected_room = "lobby";
        request.body = body;
        session.send(&request).unwrap();
        let result = read(&session, "action", body);
        let mut intent = CommunityIntent::from_command(body).unwrap();
        intent.room = "lobby".into();
        assert!(intent.accepts_ack(&result, "lobby"));
        assert!(result.draft.is_none());
        result
    };
    let created = command("/poll Native synthetic poll|First|Second");
    let poll = created.community_receipt.unwrap().object_id;
    let voted = command(&format!("/vote {poll} 2"));
    assert_eq!(
        voted
            .panel
            .unwrap()
            .community
            .unwrap()
            .detail
            .unwrap()
            .options[1]
            .votes,
        1
    );
    let commented = command(&format!("/comment {poll} 中文独立评论"));
    assert_eq!(
        commented
            .panel
            .unwrap()
            .community
            .unwrap()
            .detail
            .unwrap()
            .comments[0]
            .body,
        "中文独立评论"
    );
    let event =
        command("/event Native synthetic event|2099-01-01 20:00|description | retains pipe")
            .community_receipt
            .unwrap()
            .object_id;
    let joined = command(&format!("/rsvp {event}"));
    assert_eq!(
        joined
            .panel
            .unwrap()
            .community
            .unwrap()
            .detail
            .unwrap()
            .participant_count,
        1
    );
    let left = command(&format!("/unrsvp {event}"));
    assert_eq!(
        left.panel
            .unwrap()
            .community
            .unwrap()
            .detail
            .unwrap()
            .participant_count,
        0
    );
    let closed = command(&format!("/close {poll}"));
    assert!(
        closed
            .panel
            .unwrap()
            .community
            .unwrap()
            .detail
            .unwrap()
            .card
            .closed
    );
    std::thread::sleep(Duration::from_millis(750));
    load.request_id = "write-after";
    session.send(&load).unwrap();
    assert_eq!(snapshot(read(&session, "draft", "write-after")), before);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_native_tic_challenge_and_revision_zero_denial() {
    use miochat_gpui::tic::TicIntent;
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 61).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "tic-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "tic-login");
    read(&session, "state", "");
    let mut load = Request::simple("draft_load", "tic-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = snapshot(read(&session, "draft", "tic-before"));
    std::thread::sleep(Duration::from_millis(750));
    let mut request = Request::simple("command", "tic-create");
    request.room = "lobby";
    request.expected_room = "lobby";
    request.body = "/ttt fixture-peer";
    session.send(&request).unwrap();
    let created = read(&session, "action", "tic-create");
    let mut intent = TicIntent::from_command(request.body).unwrap();
    intent.room = "lobby".into();
    assert!(intent.accepts_ack(&created, "lobby"));
    let receipt = created.tic_receipt.unwrap();
    let game = created
        .panel
        .unwrap()
        .community
        .unwrap()
        .detail
        .unwrap()
        .game
        .unwrap();
    assert_eq!(game.cells.len(), 9);
    assert_eq!(game.x, "preview");
    assert_eq!(game.o, "fixture-peer");
    assert!(!game.can_accept && !game.can_move && game.revision == 0);
    std::thread::sleep(Duration::from_millis(750));
    let accept = format!("/ttt accept {}", receipt.object_id);
    request.request_id = "tic-wrong-accept";
    request.tic_version = Some(0);
    request.body = &accept;
    session.send(&request).unwrap();
    let denied = read(&session, "error", "tic-wrong-accept");
    assert!(denied.error.contains("被邀请者"));
    std::thread::sleep(Duration::from_millis(750));
    load.request_id = "tic-after";
    session.send(&load).unwrap();
    assert_eq!(snapshot(read(&session, "draft", "tic-after")), before);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_numeric_game_overview_guess_and_no_active_secret() {
    use miochat_gpui::guess::{GuessAction, GuessIntent};
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 67).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "number-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "number-login");
    read(&session, "state", "");
    let mut load = Request::simple("draft_load", "number-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = snapshot(read(&session, "draft", "number-before"));
    std::thread::sleep(Duration::from_millis(750));
    let mut request = Request::simple("command", "number-view");
    request.room = "lobby";
    request.body = "/guess";
    session.send(&request).unwrap();
    let view = read(&session, "action", "number-view");
    assert!(miochat_gpui::information::accepts_information_ack(
        &view, "lobby", "lobby"
    ));
    assert!(
        view.panel
            .unwrap()
            .actions
            .iter()
            .any(|a| a.command == "local:guess-start")
    );
    std::thread::sleep(Duration::from_millis(750));
    request.request_id = "number-start";
    request.body = "/guess start 2";
    request.expected_room = "lobby";
    session.send(&request).unwrap();
    let made = read(&session, "action", "number-start");
    let mut start = GuessIntent::from_command(request.body).unwrap();
    start.room = "lobby".into();
    assert!(start.accepts_ack(&made, "lobby"));
    let id = made.guess_receipt.unwrap().object_id;
    let game = made
        .panel
        .unwrap()
        .community
        .unwrap()
        .detail
        .unwrap()
        .game
        .unwrap();
    assert!(game.secret.is_empty() && game.can_guess && game.max == 2);
    for value in ["1", "2"] {
        std::thread::sleep(Duration::from_millis(750));
        let body = format!("/guess {value}");
        let mut request = Request::simple("command", value);
        request.room = "lobby";
        request.expected_room = "lobby";
        request.body = &body;
        request.game_id = id;
        session.send(&request).unwrap();
        let response = read(&session, "action", value);
        let intent = GuessIntent {
            action: GuessAction::Guess,
            object_id: id,
            room: "lobby".into(),
            maximum: 2,
            requested: value.into(),
        };
        assert!(intent.accepts_ack(&response, "lobby"));
        let result = response.guess_receipt.unwrap();
        let game = response
            .panel
            .unwrap()
            .community
            .unwrap()
            .detail
            .unwrap()
            .game
            .unwrap();
        if result.closed {
            assert!(!game.secret.is_empty());
            break;
        }
        assert!(game.secret.is_empty() && !result.feedback.contains("答案"));
        assert_eq!(value, "1");
    }
    std::thread::sleep(Duration::from_millis(750));
    load.request_id = "number-after";
    session.send(&load).unwrap();
    assert_eq!(snapshot(read(&session, "draft", "number-after")), before);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_secret_word_start_receipt_and_draft_rejection() {
    use miochat_gpui::word::{WordIntent, WordStartForm};
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 71).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "word-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "word-login");
    read(&session, "state", "");
    let mut load = Request::simple("draft_load", "word-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = snapshot(read(&session, "draft", "word-before"));
    std::thread::sleep(Duration::from_millis(750));
    let mut view = Request::simple("command", "word-overview");
    view.room = "lobby";
    view.body = "/word";
    session.send(&view).unwrap();
    let panel = read(&session, "action", "word-overview").panel.unwrap();
    let old = panel.community.as_ref().unwrap().detail.as_ref().unwrap();
    assert!(old.game.as_ref().unwrap().secret.is_empty());
    assert!(miochat_gpui::word::native_intent("local:word-start", &panel, "lobby").is_none());
    std::thread::sleep(Duration::from_millis(750));
    {
        let body = format!("/close {}", old.card.id);
        let mut close = Request::simple("command", "word-close-seed");
        close.room = "lobby";
        close.expected_room = "lobby";
        close.body = &body;
        session.send(&close).unwrap();
    }
    let closed = read(&session, "action", "word-close-seed");
    assert!(closed.community_receipt.as_ref().unwrap().committed);
    std::thread::sleep(Duration::from_millis(750));
    view.request_id = "word-empty";
    session.send(&view).unwrap();
    let panel = read(&session, "action", "word-empty").panel.unwrap();
    let intent = miochat_gpui::word::native_intent("local:word-start", &panel, "lobby").unwrap();
    let mut form = WordStartForm::new("preview".into(), "lobby".into(), 71, intent.clone());
    form.replace("synthetic猫咪".into());
    {
        let command = form.take_command().unwrap();
        assert!(form.value().is_empty());
        assert!(WordIntent::from_command(&command).is_none());
        std::thread::sleep(Duration::from_millis(750));
        let mut create = Request::simple("command", "word-create");
        create.room = "lobby";
        create.word_mode = "start";
        create.expected_room = &intent.room;
        create.body = &command;
        session.send(&create).unwrap();
    }
    let made = read(&session, "action", "word-create");
    assert!(intent.accepts_ack(&made, "lobby"));
    let id = made.word_receipt.as_ref().unwrap().object_id;
    let game = made
        .panel
        .unwrap()
        .community
        .unwrap()
        .detail
        .unwrap()
        .game
        .unwrap();
    assert!(game.secret.is_empty() && !game.masked.contains("synthetic") && !game.can_guess);
    assert!(
        session
            .send_persistence(
                &Operation::Save {
                    room: "lobby".into(),
                    version: before.0,
                    snapshot: Snapshot {
                        body: "/word start should-not-save".into(),
                        ..Default::default()
                    },
                    keep: false,
                    backup_id: String::new()
                },
                "blocked-locally"
            )
            .is_err()
    );
    std::thread::sleep(Duration::from_millis(750));
    let mut malicious = Request::simple("draft_save", "word-draft-denied");
    malicious.room = "lobby";
    malicious.body = "/word start should-not-save";
    malicious.version = before.0;
    session.send(&malicious).unwrap();
    let denied = read(&session, "error", "word-draft-denied");
    assert!(!denied.error.contains("should-not-save"));
    std::thread::sleep(Duration::from_millis(750));
    let mut own_guess = Request::simple("command", "word-owner-denied");
    own_guess.room = "lobby";
    own_guess.expected_room = "lobby";
    own_guess.game_id = id;
    own_guess.body = "/word s";
    session.send(&own_guess).unwrap();
    assert!(
        read(&session, "error", "word-owner-denied")
            .error
            .contains("发起者")
    );
    std::thread::sleep(Duration::from_millis(750));
    load.request_id = "word-after";
    session.send(&load).unwrap();
    assert_eq!(snapshot(read(&session, "draft", "word-after")), before);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_private_moderation_reads_and_own_login_alias() {
    use miochat_gpui::moderation::ReadTicket;
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 61).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "moderation-login");
    login.username = "fixture-admin";
    login.password = "fixture-admin-only";
    session.send(&login).unwrap();
    read(&session, "auth", "moderation-login");
    let initial = read(&session, "state", "");
    let mut load = Request::simple("draft_load", "moderation-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = snapshot(read(&session, "draft", "moderation-before"));
    for (index, command) in [
        "/admin",
        "/whois preview",
        "/ip",
        "/logins",
        "/ipusers 192.0.2.222",
    ]
    .iter()
    .enumerate()
    {
        std::thread::sleep(Duration::from_millis(750));
        let id = format!("private-read-{index}");
        let mut req = Request::simple("command", &id);
        req.room = "lobby";
        req.body = command;
        session.send(&req).unwrap();
        let response = read(&session, "action", &id);
        let mut ticket = ReadTicket::unbound(command).unwrap();
        ticket.bind("fixture-admin", "lobby", 61, 1);
        assert!(ticket.accepts(&response, "fixture-admin", "lobby", 61, 1));
        let v = response
            .panel
            .as_ref()
            .unwrap()
            .moderation
            .as_ref()
            .unwrap();
        if *command == "/ip" || *command == "/logins" {
            assert!(v.logins.iter().any(|r| r.ip == "192.0.2.222" && r.ok));
        }
        if *command == "/ipusers 192.0.2.222" {
            assert!(v.audited && v.users.iter().any(|u| u.name == "fixture-admin"));
        }
    }
    std::thread::sleep(Duration::from_millis(750));
    load.request_id = "moderation-after";
    session.send(&load).unwrap();
    assert_eq!(
        snapshot(read(&session, "draft", "moderation-after")),
        before
    );
    std::thread::sleep(Duration::from_millis(750));
    let sync = Request::simple("sync", "moderation-state");
    session.send(&sync).unwrap();
    let state = read(&session, "state", "moderation-state");
    assert_eq!(state.room, initial.room);
    assert_eq!(state.messages, initial.messages);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_moderation_content_bound_report_and_complete_revision_pages() {
    use miochat_gpui::moderation::{ReadTicket, revision_next};
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 62).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "content-login");
    login.username = "fixture-admin";
    login.password = "fixture-admin-only";
    session.send(&login).unwrap();
    read(&session, "auth", "content-login");
    let initial = read(&session, "state", "");
    let mut load = Request::simple("draft_load", "content-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = snapshot(read(&session, "draft", "content-before"));
    let mut message_id = 0;
    for (index, command) in [
        "/bans",
        "/audit",
        "/recalls",
        "/reports",
        "/reportview 7001",
    ]
    .iter()
    .enumerate()
    {
        std::thread::sleep(Duration::from_millis(750));
        let id = format!("content-list-{index}");
        let mut req = Request::simple("command", &id);
        req.room = "lobby";
        req.body = command;
        session.send(&req).unwrap();
        let result = read(&session, "action", &id);
        let mut ticket = ReadTicket::unbound(command).unwrap();
        ticket.bind("fixture-admin", "lobby", 62, 1);
        assert!(ticket.accepts(&result, "fixture-admin", "lobby", 62, 1));
        if let Some(m) = result
            .panel
            .as_ref()
            .unwrap()
            .moderation
            .as_ref()
            .unwrap()
            .reported
            .as_ref()
        {
            assert_eq!(m.report_id, 7001);
            assert_ne!(m.message_id, m.report_id);
            message_id = m.message_id;
            assert!(!m.body.contains("synthetic historical body"));
        }
    }
    assert!(message_id > 0);
    let command = format!("/revisions {message_id}");
    std::thread::sleep(Duration::from_millis(750));
    let mut req = Request::simple("command", "revision-first");
    req.room = "lobby";
    req.body = &command;
    session.send(&req).unwrap();
    let first = read(&session, "action", "revision-first");
    let mut ticket = ReadTicket::unbound(&command).unwrap();
    ticket.bind("fixture-admin", "lobby", 62, 1);
    assert!(ticket.accepts(&first, "fixture-admin", "lobby", 62, 1));
    let page = first
        .panel
        .as_ref()
        .unwrap()
        .moderation
        .as_ref()
        .unwrap()
        .revisions
        .as_ref()
        .unwrap();
    assert_eq!(page.rows.len(), 100);
    let first_last = page.rows.last().unwrap().id;
    let (next, bounds) =
        revision_next(first.panel.as_ref().unwrap(), "fixture-admin", "lobby").unwrap();
    std::thread::sleep(Duration::from_millis(750));
    req.request_id = "revision-next";
    req.body = &next;
    req.read_after = bounds.after;
    req.read_until = bounds.until;
    session.send(&req).unwrap();
    let second = read(&session, "action", "revision-next");
    ticket.bounds = bounds;
    assert!(ticket.accepts(&second, "fixture-admin", "lobby", 62, 1));
    let page = second
        .panel
        .as_ref()
        .unwrap()
        .moderation
        .as_ref()
        .unwrap()
        .revisions
        .as_ref()
        .unwrap();
    assert_eq!(page.rows.len(), 5);
    assert!(page.rows[0].id > first_last);
    assert_eq!(page.next_after, 0);
    std::thread::sleep(Duration::from_millis(750));
    req.request_id = "revision-zero";
    req.read_after = 0;
    req.read_until = Some(0);
    session.send(&req).unwrap();
    let zero = read(&session, "action", "revision-zero");
    assert!(
        zero.panel
            .as_ref()
            .unwrap()
            .moderation
            .as_ref()
            .unwrap()
            .revisions
            .as_ref()
            .unwrap()
            .rows
            .is_empty()
    );
    std::thread::sleep(Duration::from_millis(750));
    load.request_id = "content-after";
    session.send(&load).unwrap();
    assert_eq!(snapshot(read(&session, "draft", "content-after")), before);
    std::thread::sleep(Duration::from_millis(750));
    let sync = Request::simple("sync", "content-state");
    session.send(&sync).unwrap();
    let state = read(&session, "state", "content-state");
    assert_eq!(state.messages, initial.messages);
    assert_eq!(state.room, initial.room);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_report_resolve_receipts_and_preserved_chat_state() {
    use miochat_gpui::report::ReportIntent;
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 63).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "report-login");
    login.username = "fixture-admin";
    login.password = "fixture-admin-only";
    session.send(&login).unwrap();
    read(&session, "auth", "report-login");
    let initial = read(&session, "state", "");
    let message_id = initial.messages.last().unwrap().id;
    let mut save = Request::simple("draft_save", "report-save");
    save.room = "lobby";
    save.body = "ordinary draft stays separate";
    save.version = 0;
    session.send(&save).unwrap();
    let before = snapshot(read(&session, "draft_saved", "report-save"));
    std::thread::sleep(Duration::from_millis(750));
    let command = format!("/report {message_id} synthetic native reason");
    let mut request = Request::simple("command", "report-submit");
    request.room = "lobby";
    request.body = &command;
    session.send(&request).unwrap();
    let reported = read(&session, "action", "report-submit");
    let intent = ReportIntent::parse(&command).unwrap();
    assert!(intent.accepts(&reported, "fixture-admin", "lobby"));
    let id = reported.report_receipt.as_ref().unwrap().report_id;
    assert!(id > 7001);
    assert!(reported.panel.is_none() && reported.message.is_none());
    for (index, status) in ["resolved", "rejected", "rejected"].iter().enumerate() {
        std::thread::sleep(Duration::from_millis(750));
        let command = format!("/resolve {id} {status}");
        let key = format!("report-resolve-{index}");
        let mut req = Request::simple("command", &key);
        req.room = "lobby";
        req.body = &command;
        session.send(&req).unwrap();
        let ack = read(&session, "action", &key);
        assert!(
            ReportIntent::parse(&command)
                .unwrap()
                .accepts(&ack, "fixture-admin", "lobby")
        );
    }
    std::thread::sleep(Duration::from_millis(750));
    let mut load = Request::simple("draft_load", "report-after");
    load.room = "lobby";
    session.send(&load).unwrap();
    assert_eq!(snapshot(read(&session, "draft", "report-after")), before);
    std::thread::sleep(Duration::from_millis(750));
    let sync = Request::simple("sync", "report-state");
    session.send(&sync).unwrap();
    let state = read(&session, "state", "report-state");
    assert_eq!(state.messages, initial.messages);
    assert_eq!(state.room, initial.room);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_six_sanctions_exact_receipts_and_unchanged_chat() {
    use miochat_gpui::sanctions::SanctionIntent;
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 64).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "sanction-login");
    login.username = "fixture-admin";
    login.password = "fixture-admin-only";
    session.send(&login).unwrap();
    read(&session, "auth", "sanction-login");
    let initial = read(&session, "state", "");
    let mut save = Request::simple("draft_save", "sanction-save");
    save.room = "lobby";
    save.body = "ordinary draft unaffected by sanctions";
    save.version = 0;
    session.send(&save).unwrap();
    let before = snapshot(read(&session, "draft_saved", "sanction-save"));
    for (index, command) in [
        "/mute fixture-peer 7d synthetic channel reason",
        "/unmute fixture-peer",
        "/gmute fixture-peer permanent synthetic global reason",
        "/ungmute fixture-peer",
        "/ban fixture-peer 7d synthetic account reason",
        "/unban fixture-peer",
    ]
    .iter()
    .enumerate()
    {
        std::thread::sleep(Duration::from_millis(750));
        let key = format!("sanction-{index}");
        let mut req = Request::simple("command", &key);
        req.room = "lobby";
        req.body = command;
        if index < 2 {
            req.expected_room = "lobby"
        };
        session.send(&req).unwrap();
        let ack = read(&session, "action", &key);
        assert!(SanctionIntent::parse(command, "lobby").unwrap().accepts(
            &ack,
            "fixture-admin",
            "lobby"
        ));
        assert!(ack.panel.is_none() && ack.message.is_none() && ack.draft.is_none());
    }
    std::thread::sleep(Duration::from_millis(750));
    let mut load = Request::simple("draft_load", "sanction-after");
    load.room = "lobby";
    session.send(&load).unwrap();
    assert_eq!(snapshot(read(&session, "draft", "sanction-after")), before);
    std::thread::sleep(Duration::from_millis(750));
    session
        .send(&Request::simple("sync", "sanction-state"))
        .unwrap();
    let state = read(&session, "state", "sanction-state");
    assert_eq!(state.messages, initial.messages);
    assert_eq!(state.room, initial.room);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_ip_force_false_is_not_omitted_and_does_not_trust_reason() {
    use miochat_gpui::sanctions::SanctionIntent;
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 65).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "ip-login");
    login.username = "fixture-admin";
    login.password = "fixture-admin-only";
    session.send(&login).unwrap();
    read(&session, "auth", "ip-login");
    let initial = read(&session, "state", "");
    let mut req = Request::simple("command", "ip-self-denied");
    req.room = "lobby";
    req.body = "/banip 192.0.2.0/24 1h reason --force";
    req.self_ip_force = Some(false);
    session.send(&req).unwrap();
    let denied = read(&session, "error", "ip-self-denied");
    assert!(denied.error.contains("当前聊天室连接"));
    assert!(denied.sanction_receipt.is_none());
    for (index, command) in [
        "/banip ::ffff:198.51.100.0/120 1h synthetic",
        "/unbanip 198.51.100.0/24",
    ]
    .iter()
    .enumerate()
    {
        std::thread::sleep(Duration::from_millis(750));
        let key = format!("ip-action-{index}");
        let mut req = Request::simple("command", &key);
        req.room = "lobby";
        req.body = command;
        req.self_ip_force = if index == 0 { Some(false) } else { None };
        session.send(&req).unwrap();
        let ack = read(&session, "action", &key);
        let mut intent = SanctionIntent::parse(command, "lobby").unwrap();
        intent.self_ip_force = req.self_ip_force;
        assert!(intent.accepts(&ack, "fixture-admin", "lobby"));
        assert_eq!(
            ack.sanction_receipt.as_ref().unwrap().target,
            "198.51.100.0/24"
        );
    }
    std::thread::sleep(Duration::from_millis(750));
    session.send(&Request::simple("sync", "ip-state")).unwrap();
    let state = read(&session, "state", "ip-state");
    assert_eq!(state.messages, initial.messages);
    assert_eq!(state.room, initial.room);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_management_three_receipts_do_not_change_conversation() {
    use miochat_gpui::management::ManagementIntent;
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 66).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "management-login");
    login.username = "fixture-owner";
    login.password = "fixture-owner-only";
    session.send(&login).unwrap();
    read(&session, "auth", "management-login");
    let before = read(&session, "state", "");
    for (index, command) in [
        "/slowmode 3600",
        "/slowmode 0",
        "/role preview moderator",
        "/role preview member",
        "/kick PREVIEW",
    ]
    .iter()
    .enumerate()
    {
        std::thread::sleep(Duration::from_millis(750));
        let key = format!("management-{index}");
        let mut req = Request::simple("command", &key);
        req.body = command;
        req.room = "lobby";
        let intent = ManagementIntent::parse(command, "lobby").unwrap();
        req.expected_room = &intent.room;
        session.send(&req).unwrap();
        let ack = read(&session, "action", &key);
        assert!(intent.accepts(&ack, "fixture-owner", "lobby"), "{ack:?}");
        assert!(ack.message.is_none() && ack.panel.is_none() && ack.draft.is_none());
    }
    std::thread::sleep(Duration::from_millis(750));
    session
        .send(&Request::simple("sync", "management-state"))
        .unwrap();
    let after = read(&session, "state", "management-state");
    assert_eq!(after.room, before.room);
    assert_eq!(after.messages, before.messages);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_errors_health_are_private_no_input_reads() {
    use miochat_gpui::moderation::ReadTicket;
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    for (user, password, allowed) in [
        ("fixture-admin", "fixture-admin-only", true),
        ("preview", "fixture-local-only", false),
    ] {
        let mut session = Session::fixture(&path, 67).unwrap();
        read(&session, "hello", "");
        let mut login = Request::simple("login", "ops-login");
        login.username = user;
        login.password = password;
        session.send(&login).unwrap();
        read(&session, "auth", "ops-login");
        let before = read(&session, "state", "");
        for command in ["/errors", "/health"] {
            std::thread::sleep(Duration::from_millis(750));
            let mut req = Request::simple("command", command);
            req.body = command;
            req.room = "lobby";
            session.send(&req).unwrap();
            let r = read(&session, if allowed { "action" } else { "error" }, command);
            if allowed {
                let mut t = ReadTicket::unbound(command).unwrap();
                t.bind(user, "lobby", 67, 1);
                assert!(t.accepts(&r, user, "lobby", 67, 1), "{r:?}");
                let p = r.panel.unwrap();
                assert!(p.actions.is_empty() && p.lines.is_empty() && p.statistics.is_none());
                let v = p.moderation.unwrap();
                assert_eq!(v.health.is_some(), command == "/health");
                assert!(!v.audited);
            } else {
                assert!(r.panel.is_none());
            }
        }
        std::thread::sleep(Duration::from_millis(750));
        session.send(&Request::simple("sync", "ops-after")).unwrap();
        let after = read(&session, "state", "ops-after");
        assert_eq!(before.room, after.room);
        assert_eq!(before.messages, after.messages);
        session.disconnect();
    }
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_configuration_two_receipts_and_draft_isolation() {
    use miochat_gpui::configuration::ConfigurationIntent;
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 68).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "configuration-login");
    login.username = "fixture-admin";
    login.password = "fixture-admin-only";
    session.send(&login).unwrap();
    read(&session, "auth", "configuration-login");
    let before = read(&session, "state", "");
    let mut load = Request::simple("draft_load", "configuration-draft-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let draft = snapshot(read(&session, "draft", "configuration-draft-before"));
    for (index, command) in [
        "/config registration closed",
        "/config registration invite",
        "/config registration open",
        "/config registration_quota +001",
        "/config retention permanent",
        "/maintenance on synthetic notice",
        "/maintenance off",
        "/config maintenance_notice",
    ]
    .iter()
    .enumerate()
    {
        std::thread::sleep(Duration::from_millis(750));
        let key = format!("configuration-{index}");
        let mut req = Request::simple("command", &key);
        req.body = command;
        req.room = "lobby";
        session.send(&req).unwrap();
        let ack = read(&session, "action", &key);
        assert!(
            ConfigurationIntent::parse(command)
                .unwrap()
                .accepts(&ack, "fixture-admin", "lobby"),
            "{ack:?}"
        );
        let changes = &ack.configuration_receipt.as_ref().unwrap().changes;
        if index == 5 {
            assert_eq!(changes.len(), 2);
            assert_eq!(changes[1].value, "synthetic notice");
        }
        if index == 6 {
            assert_eq!(changes.len(), 1);
        }
        if index == 7 {
            assert_eq!(changes.len(), 1);
            assert_eq!(changes[0].value, "");
        }
    }
    std::thread::sleep(Duration::from_millis(750));
    load.request_id = "configuration-draft-after";
    session.send(&load).unwrap();
    assert_eq!(
        snapshot(read(&session, "draft", "configuration-draft-after")),
        draft
    );
    std::thread::sleep(Duration::from_millis(750));
    session
        .send(&Request::simple("sync", "configuration-after"))
        .unwrap();
    let after = read(&session, "state", "configuration-after");
    assert_eq!(before.room, after.room);
    assert_eq!(before.messages, after.messages);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_composition_cas_editor_keeps_chat_draft_and_publishes_once() {
    use miochat_gpui::composition::{Editor, Effect, Operation as COp, Slot};
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 83).unwrap();
    let hello = read(&session, "hello", "");
    assert!(hello.capabilities.iter().any(|v| v == "composition-cas-v1"));
    let mut login = Request::simple("login", "composition-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", "composition-login");
    read(&session, "state", "");
    let mut draft = Request::simple("draft_load", "composition-chat-before");
    draft.room = "lobby";
    session.send(&draft).unwrap();
    let before = snapshot(read(&session, "draft", draft.request_id));
    let mut sequence = 0;
    let mut run = |e: &mut Editor, op: COp| {
        std::thread::sleep(Duration::from_millis(750));
        sequence += 1;
        let id = format!("composition-{sequence}");
        let payload = e
            .begin(op, id.clone(), sequence * 1000)
            .expect("one serial intent");
        let mut request = Request::simple("composition", &id);
        request.room = "lobby";
        request.composition = Some(&payload);
        session.send(&request).unwrap();
        let r = read(&session, "composition", &id);
        assert!(e.admits(&r, "preview", 83));
        assert!(r.message.is_none() && r.panel.is_none() && r.draft.is_none());
        let effect = e.accept(&r);
        (r, effect)
    };
    for kind in ["post", "art"] {
        let mut editor = Editor::new(
            "preview".into(),
            "lobby".into(),
            kind.into(),
            "native synthetic title".into(),
            83,
        )
        .unwrap();
        assert_eq!(run(&mut editor, COp::Load).1, Some(Effect::NeedReserve));
        assert_eq!(run(&mut editor, COp::Reserve).1, Some(Effect::Ready));
        let old = editor.snapshot.as_ref().unwrap().version;
        assert!(editor.edit(Slot::Body, "A\tB\nsecond line\n".into(), 2));
        assert_eq!(run(&mut editor, COp::Save).1, Some(Effect::Saved));
        assert!(!editor.dirty());
        assert_eq!(editor.body, "A\tB\nsecond line\n");
        assert_eq!(
            editor.snapshot.as_ref().unwrap().body,
            "A    B\nsecond line"
        );
        assert!(editor.snapshot.as_ref().unwrap().version > old);
        assert_eq!(run(&mut editor, COp::Remote).1, Some(Effect::Remote));
        assert_eq!(run(&mut editor, COp::Reload).1, Some(Effect::Reloaded));
        assert_eq!(editor.body, "A    B\nsecond line");
        let (r, effect) = run(&mut editor, COp::Publish);
        let id = r.composition.as_ref().unwrap().object_id;
        assert_eq!(
            effect,
            Some(Effect::Published {
                object_id: id,
                newer_input: false
            })
        );
        assert!(id > 0 && editor.body.is_empty());
        assert!(
            !editor.admits(&r, "preview", 83),
            "duplicate ACK must not republish or reopen"
        );
        assert_eq!(run(&mut editor, COp::Remote).1, Some(Effect::Remote));
        assert!(editor.remote.as_ref().unwrap().body.is_empty());
    }
    std::thread::sleep(Duration::from_millis(750));
    draft.request_id = "composition-chat-after";
    session.send(&draft).unwrap();
    assert_eq!(snapshot(read(&session, "draft", draft.request_id)), before);
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_background_maintenance_is_private_bound_and_preserves_chat_draft() {
    use miochat_gpui::{
        composition::Origin,
        maintenance::{Job, Kind, Operation, State},
    };
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 72).unwrap();
    read(&session, "hello", "");
    let mut login = Request::simple("login", "jobs-login");
    login.username = "fixture-admin";
    login.password = "fixture-admin-only";
    session.send(&login).unwrap();
    read(&session, "auth", "jobs-login");
    read(&session, "state", "");
    let mut load = Request::simple("draft_load", "jobs-draft-before");
    load.room = "lobby";
    session.send(&load).unwrap();
    let before = snapshot(read(&session, "draft", "jobs-draft-before"));
    let mut job = Job::new(
        "fixture-admin".into(),
        72,
        Origin::synthetic("fixture", 72),
        "lobby".into(),
        Kind::Check,
        1,
        None,
    );
    std::thread::sleep(Duration::from_millis(750));
    let data = job
        .begin(Operation::Start, "jobs-start".into(), 1, 0)
        .unwrap();
    let mut request = Request::simple("maintenance", "jobs-start");
    request.room = "lobby";
    request.maintenance = Some(&data);
    session.send(&request).unwrap();
    let start = read(&session, "maintenance", "jobs-start");
    assert_eq!(
        start.maintenance.as_ref().unwrap().operation,
        Operation::Start
    );
    assert_eq!(job.accept(&start, 1, 1), Some(true));
    let done = read(&session, "maintenance", "jobs-start");
    assert!(done.maintenance.as_ref().unwrap().lines.is_empty());
    assert!(job.accept(&done, 1, 2).is_some());
    assert_eq!(job.state, Some(State::Complete));
    assert!(job.lines.is_empty() && job.warning.is_empty());
    job.hide();
    job.open(2);
    std::thread::sleep(Duration::from_millis(750));
    let data = job
        .begin(Operation::View, "jobs-view".into(), 2, 3)
        .unwrap();
    let mut request = Request::simple("maintenance", "jobs-view");
    request.room = "lobby";
    request.maintenance = Some(&data);
    session.send(&request).unwrap();
    let view = read(&session, "maintenance", "jobs-view");
    assert!(job.accept(&view, 2, 4).is_some());
    assert_eq!(job.lines.len(), 4);
    assert!(job.lines[0].contains("quick_check"));
    assert!(view.profile.is_none() && view.panel.is_none() && view.preferences.is_none());
    job.hide();
    assert!(job.lines.is_empty());
    assert!(!job.admits(&view, 3));
    std::thread::sleep(Duration::from_millis(750));
    load.request_id = "jobs-draft-after";
    session.send(&load).unwrap();
    assert_eq!(
        snapshot(read(&session, "draft", "jobs-draft-after")),
        before
    );
    std::thread::sleep(Duration::from_millis(750));
    let mut chat = Request::simple("send_message", "jobs-chat");
    chat.room = "lobby";
    chat.body = "synthetic background job chat";
    session.send(&chat).unwrap();
    assert_eq!(
        read(&session, "sent", "jobs-chat").message.unwrap().body,
        chat.body
    );
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_prefix_completion_is_exclusive_authenticated_and_preserves_draft() {
    use miochat_gpui::{
        completion::{Context, Data, Intent, Kind, token},
        composition::Origin,
    };
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 74).unwrap();
    let hello = read(&session, "hello", "");
    assert!(hello.capabilities.iter().any(|c| c == "completion-v1"));
    let data = Data {
        kind: Kind::User,
        prefix: "@fixture-p".into(),
    };
    let mut request = Request::simple("completion", "before-auth");
    request.room = "lobby";
    request.completion = Some(&data);
    session.send(&request).unwrap();
    read(&session, "error", "before-auth");
    let mut login = Request::simple("login", "completion-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", login.request_id);
    read(&session, "state", "");
    let mut draft = Request::simple("draft_load", "completion-draft-before");
    draft.room = "lobby";
    session.send(&draft).unwrap();
    let before = snapshot(read(&session, "draft", draft.request_id));
    let origin = Origin::synthetic("fixture", 74);
    for (id, text, kind, prefix, want) in [
        ("complete-command", "/tim", Kind::Command, "/tim", "/time"),
        ("complete-room", "#lob", Kind::Room, "#lob", "#lobby"),
        (
            "complete-user",
            "@fixture-p",
            Kind::User,
            "@fixture-p",
            "@fixture-peer",
        ),
    ] {
        std::thread::sleep(Duration::from_millis(750));
        let data = Data {
            kind,
            prefix: prefix.into(),
        };
        let mut request = Request::simple("completion", id);
        request.room = "lobby";
        request.completion = Some(&data);
        let i = Intent::new(
            id.into(),
            token(text, text.len()..text.len(), true).unwrap(),
            Context {
                enabled: true,
                owner: "preview",
                room: "lobby",
                origin: Some(&origin),
                generation: 74,
                revision: 1,
            },
            0,
        )
        .unwrap();
        session.send(&request).unwrap();
        let out = read(&session, "completion", id);
        assert!(i.admits(&out));
        assert!(out.completion.unwrap().choices.iter().any(|c| c == want));
    }
    std::thread::sleep(Duration::from_millis(750));
    draft.request_id = "completion-draft-after";
    session.send(&draft).unwrap();
    assert_eq!(snapshot(read(&session, "draft", draft.request_id)), before);
    std::thread::sleep(Duration::from_millis(750));
    let mut bad = Request::simple("completion", "completion-mixed");
    bad.room = "lobby";
    bad.body = "never sent";
    bad.completion = Some(&data);
    session.send(&bad).unwrap();
    let e = read(&session, "error", bad.request_id);
    assert!(e.completion.is_none());
    session.disconnect();
}

#[test]
#[ignore = "requires the explicitly built synthetic Go pipe driver"]
fn real_go_pipe_image_upload_reference_quota_unknown_and_recall() {
    use miochat_gpui::{
        attachments::{
            self, Attachments, Binding, Data, Operation as Op, Phase, Prepared, Purpose,
        },
        composition::Origin,
        emoji_attachments::Catalog,
    };
    let path =
        PathBuf::from(std::env::var_os("MIOCHAT_GUI_FIXTURE_DRIVER").expect("explicit fixture"));
    let mut session = Session::fixture(&path, 81).unwrap();
    let hello = read(&session, "hello", "");
    assert!(hello.capabilities.iter().any(|c| c == "attachments-v1"));
    assert!(
        hello
            .capabilities
            .iter()
            .any(|c| c == "attachment-references-v1")
    );
    let mut login = Request::simple("login", "image-login");
    login.username = "preview";
    login.password = "fixture-local-only";
    session.send(&login).unwrap();
    read(&session, "auth", login.request_id);
    read(&session, "state", "");
    let mut draft = Request::simple("draft_load", "image-draft-before");
    draft.room = "lobby";
    session.send(&draft).unwrap();
    let before = snapshot(read(&session, "draft", draft.request_id));
    let bytes = vec![
        137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82, 0, 0, 0, 1, 0, 0, 0, 1, 8, 2,
        0, 0, 0, 144, 119, 83, 222, 0, 0, 0, 12, 73, 68, 65, 84, 120, 156, 99, 248, 207, 192, 0, 0,
        3, 1, 1, 0, 201, 254, 146, 239, 0, 0, 0, 0, 73, 69, 78, 68, 174, 66, 96, 130,
    ];
    let binding = Binding::new(
        Origin::synthetic("fixture", 81),
        "preview".into(),
        "lobby".into(),
        81,
    )
    .unwrap();
    let mut uploads = Attachments::default();
    uploads.supported = true;
    let ticket = uploads
        .begin_selection(binding.clone(), Purpose::CustomEmoji)
        .unwrap();
    uploads
        .finish_selection(
            &ticket,
            &binding,
            Prepared::from_bytes("synthetic.png".into(), bytes.clone()).unwrap(),
            "synthetic-image-upload-81".into(),
        )
        .unwrap();
    let exchange = |id: &str, data: &Data| {
        let mut q = Request::simple("attachment", id);
        q.room = "lobby";
        q.attachment = Some(data);
        session.send(&q).unwrap();
        read(&session, "attachment", id)
    };
    let mut committed = None;
    for (id, op) in [
        ("image-intent", Op::Intent),
        ("image-chunk", Op::Chunk),
        ("image-commit", Op::Commit),
    ] {
        std::thread::sleep(Duration::from_millis(75));
        let data = uploads.begin(op, id.into(), &binding).unwrap();
        let out = exchange(id, &data);
        let receipt = out.attachment.as_ref().unwrap();
        assert_ne!(receipt.status, "error", "{receipt:?}");
        assert!(uploads.accept(id, &binding, receipt));
        if op == Op::Commit {
            committed = Some(receipt.clone());
        }
    }
    assert_eq!(uploads.upload.as_ref().unwrap().phase, Phase::Committed);
    let original = committed.unwrap();
    let used = original.quota.as_ref().unwrap().used;
    assert_eq!(used, bytes.len() as u64);
    let mut catalog = Catalog::default();
    assert!(catalog.remember_committed(Purpose::CustomEmoji, &binding, &original));
    catalog
        .select(
            binding.clone(),
            original.message_id,
            catalog.revision,
            "synthetic-image-reference-81".into(),
        )
        .unwrap();
    std::thread::sleep(Duration::from_millis(750));
    let data = catalog
        .begin(Op::Reference, "image-reference".into(), &binding)
        .unwrap();
    let out = exchange("image-reference", &data);
    // Simulate a lost application ACK: retain committed server result but recover only by original key.
    let expected = out.attachment.unwrap();
    assert_eq!(expected.status, "committed");
    assert_eq!(expected.accepted_offset, 0);
    assert_eq!(expected.quota.as_ref().unwrap().used, used);
    catalog.transport_uncertain("image-reference");
    assert!(
        catalog
            .begin(Op::Reference, "must-not-resend".into(), &binding)
            .is_err()
    );
    let data = catalog
        .begin(Op::Status, "image-reference-status".into(), &binding)
        .unwrap();
    assert!(data.upload_id.is_empty());
    let out = exchange("image-reference-status", &data);
    assert!(catalog.accept(
        "image-reference-status",
        &binding,
        out.attachment.as_ref().unwrap()
    ));
    assert_eq!(
        catalog.reference.as_ref().unwrap().message_id,
        expected.message_id
    );
    let fetch = Data {
        op: Some(Op::Fetch),
        message_id: expected.message_id,
        ..Default::default()
    };
    let out = exchange("image-fetch", &fetch);
    let receipt = out.attachment.unwrap();
    assert_eq!(receipt.status, "fetched");
    assert_eq!(
        attachments::decode_base64(&receipt.data_b64, 20_000_000).unwrap(),
        bytes
    );
    assert_eq!(
        receipt.attachment.unwrap().sha256,
        attachments::sha256_hex(&bytes)
    );
    std::thread::sleep(Duration::from_millis(750));
    let body = format!("/recall {}", original.message_id);
    let mut recall = Request::simple("command", "image-recall");
    recall.room = "lobby";
    recall.body = &body;
    session.send(&recall).unwrap();
    read(&session, "action", recall.request_id);
    let denied = exchange("image-fetch-recalled", &fetch);
    assert_eq!(denied.attachment.unwrap().status, "error");
    std::thread::sleep(Duration::from_millis(750));
    draft.request_id = "image-draft-after";
    session.send(&draft).unwrap();
    assert_eq!(snapshot(read(&session, "draft", draft.request_id)), before);
    session.disconnect();
}
