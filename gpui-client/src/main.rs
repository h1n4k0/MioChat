#![cfg_attr(target_os = "windows", windows_subsystem = "windows")]
use gpui::{
    App, Bounds, Context, Div, Element, ElementId, ElementInputHandler, Entity, EntityInputHandler,
    FocusHandle, Focusable, GlobalElementId, IntoElement, KeyDownEvent, LayoutId, Pixels, Point,
    Stateful, Style, UTF16Selection, Window, WindowBounds, WindowOptions, div, prelude::*, px,
    relative, rgb, size,
};
use miochat_gpui::{
    appearance::Appearance,
    center::Center,
    connection::{
        ConnectionSpec, HostKeyProbe, app_known_hosts_path, is_host_known, probe_host_key,
        trust_host_key,
    },
    editing,
    notifications::Notifications,
    persistence::{Ack, Operation, Persistence, Position, Snapshot},
    preferences::Preferences,
    profile::{EditKind, Profile, ProfileEdit},
    rooms::{RoomAction, RoomIntent},
    session::{
        DeviceKey, Draft, Message, Panel, PanelAction, Request, Response, Room, Session,
        SessionEvent,
    },
};
use std::{
    collections::HashSet,
    ops::Range,
    time::{Duration, Instant},
};
mod affordances_ui;
mod attachments_ui;
mod center_ui;
mod community_ui;
mod completion_ui;
mod composition_text;
mod composition_ui;
mod configuration_ui;
mod context_ui;
mod direct_message_ui;
mod dot_ui;
mod draft_ui;
mod emoji_attachments_ui;
mod emoji_ui;
mod feature_menu;
mod feature_menu_ui;
mod guess_ui;
mod information_ui;
mod input_history;
mod keys_ui;
mod login_ui;
mod maintenance_ui;
mod management_ui;
mod moderation_ui;
mod preferences_ui;
mod profile_ui;
mod reading_ui;
mod report_ui;
mod rooms_ui;
mod sanctions_ui;
mod secret_admin_ui;
mod tic_ui;
mod word_ui;

struct PendingView {
    request_id: String,
    room: String,
    position: Position,
    append: bool,
    started: u64,
}

enum BackgroundEvent {
    NotificationClicked {
        tag: String,
    },
    HostKey {
        generation: u64,
        result: Result<Option<HostKeyProbe>, String>,
    },
}

const BORDER: u32 = 0xdcd7cd;
const TEXT: u32 = 0x22262b;
const MUTED: u32 = 0x555c64;
const ACCENT: u32 = 0x2f5d8a;
const ERROR: u32 = 0xae2a22;

#[derive(Clone, Copy, Eq, PartialEq)]
enum Screen {
    Connect,
    Trust,
    Auth,
    Password,
    KeyBinding,
    Chat,
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum Field {
    CompositionTitle,
    CompositionBody,
    Host,
    Port,
    SshUser,
    Username,
    Password,
    Confirm,
    Invite,
    Identity,
    KeyPassphrase,
    KeyLabel,
    RoomTarget,
    ProfileText,
    DirectTarget,
    DirectBody,
    DirectExtra,
    WordSecret,
}

struct PendingSend {
    edit_message: Option<i64>,
    configuration: Option<miochat_gpui::configuration::ConfigurationIntent>,
    management: Option<miochat_gpui::management::ManagementIntent>,
    sanction: Option<miochat_gpui::sanctions::SanctionIntent>,
    report: Option<miochat_gpui::report::ReportIntent>,
    moderation: Option<miochat_gpui::moderation::ReadTicket>,
    word: Option<miochat_gpui::word::WordIntent>,
    guess: Option<miochat_gpui::guess::GuessIntent>,
    tic: Option<miochat_gpui::tic::TicIntent>,
    community: Option<miochat_gpui::community_mutation::CommunityIntent>,
    simple_action: Option<miochat_gpui::simple_actions::SimpleAction>,
    information: bool,
    one_off: bool,
    one_off_target: String,
    room: String,
    request_id: String,
    body: String,
}

impl PendingSend {
    fn word_start(
        request_id: String,
        room: String,
        intent: miochat_gpui::word::WordIntent,
    ) -> Self {
        Self {
            edit_message: None,
            configuration: None,
            management: None,
            sanction: None,
            report: None,
            moderation: None,
            word: Some(intent),
            guess: None,
            tic: None,
            community: None,
            simple_action: None,
            information: false,
            one_off: false,
            one_off_target: String::new(),
            room,
            request_id,
            body: String::new(),
        }
    }

    fn command(request_id: String, room: String, command: &str, body: String) -> Self {
        Self {
            edit_message: context_ui::edit_id(command),
            configuration: miochat_gpui::configuration::ConfigurationIntent::parse(command),
            management: miochat_gpui::management::ManagementIntent::parse(command, &room),
            sanction: miochat_gpui::sanctions::SanctionIntent::parse(command, &room),
            report: miochat_gpui::report::ReportIntent::parse(command),
            moderation: miochat_gpui::moderation::ReadTicket::unbound(command),
            word: miochat_gpui::word::WordIntent::from_command(command),
            guess: miochat_gpui::guess::GuessIntent::from_command(command),
            tic: miochat_gpui::tic::TicIntent::from_command(command),
            community: miochat_gpui::community_mutation::CommunityIntent::from_command(command),
            simple_action: miochat_gpui::simple_actions::SimpleAction::parse(command),
            information: miochat_gpui::information::is_information_command(command),
            one_off: command
                .split_whitespace()
                .next()
                .is_some_and(|c| c.eq_ignore_ascii_case("/msg")),
            one_off_target: command
                .split_whitespace()
                .nth(1)
                .unwrap_or_default()
                .to_string(),
            request_id,
            room,
            body,
        }
    }
}

// Actual pre-projection gate: errors cannot carry unrelated effects, while an
// unrelated asynchronous state refresh remains allowed during a pending setting.
fn configuration_response_admitted(
    response: &Response,
    pending: Option<&PendingSend>,
    owner: &str,
) -> bool {
    let matching =
        pending.filter(|p| p.request_id == response.request_id && p.configuration.is_some());
    if response.configuration_receipt.is_some() {
        return matching.is_some_and(|p| {
            p.configuration
                .as_ref()
                .unwrap()
                .accepts(response, owner, &p.room)
        });
    }
    if matching.is_some() {
        return miochat_gpui::information::is_plain_error_response(response);
    }
    true
}

// This actual dispatch gate runs before all profile/center/preferences effects.
// A late information-family panel needs its original current pending request,
// even if that request is no longer present; a kind tag is not authorization.
fn information_response_admitted(
    response: &Response,
    pending: Option<&PendingSend>,
    room: &str,
) -> bool {
    let matching = pending.filter(|p| p.request_id == response.request_id);
    let community_pending = matching.is_some_and(|p| {
        p.community.is_some() || p.tic.is_some() || p.guess.is_some() || p.word.is_some()
    });
    let community_receipts = [
        response.community_receipt.is_some(),
        response.tic_receipt.is_some(),
        response.guess_receipt.is_some(),
        response.word_receipt.is_some(),
    ];
    let information_panel = response.panel.as_ref().is_some_and(|p| {
        p.statistics.is_some()
            || !p.server_time.is_empty()
            || p.community.is_some()
            || matches!(
                p.kind.as_str(),
                "stats" | "time" | "art" | "community" | "menu"
            )
    });
    if !matches!(response.kind.as_str(), "action" | "sent") {
        if information_panel || community_receipts.iter().any(|v| *v) {
            return false;
        }
        if matching.is_some_and(|p| p.information) || community_pending {
            // A normal failure may retain the input. It cannot carry unrelated updates.
            return miochat_gpui::information::is_plain_error_response(response);
        }
        return true;
    }
    // Never bypass an information intent merely because an untrusted receipt exists.
    if let Some(p) = matching.filter(|p| p.information) {
        return miochat_gpui::information::accepts_information_ack(response, &p.room, room);
    }
    if community_pending || community_receipts.iter().any(|v| *v) {
        let Some(p) = matching else {
            return false;
        };
        if response.kind != "action"
            || response.profile.is_some()
            || response.center.is_some()
            || response.preferences.is_some()
            || response.device_key.is_some()
            || response.key_reminder
            || !response.form.is_empty()
            || !response.mention.is_empty()
            || !response.rooms.is_empty()
            || response.configuration_receipt.is_some()
            || response.management_receipt.is_some()
            || response.sanction_receipt.is_some()
            || response.report_receipt.is_some()
            || response.direct_target.is_some()
            || community_receipts.iter().filter(|v| **v).count() != 1
            || response.panel.as_ref().is_some_and(|panel| {
                panel.kind != "community"
                    || !miochat_gpui::information::information_panel_payload(panel)
            })
        {
            return false;
        }
        return p
            .community
            .as_ref()
            .is_some_and(|i| i.accepts_ack(response, &p.room))
            || p.tic
                .as_ref()
                .is_some_and(|i| i.accepts_ack(response, &p.room))
            || p.guess
                .as_ref()
                .is_some_and(|i| i.accepts_ack(response, &p.room))
            || p.word
                .as_ref()
                .is_some_and(|i| i.accepts_ack(response, &p.room));
    }
    !information_panel
}

fn discard_word_input(form: &mut Option<miochat_gpui::word::WordStartForm>, field: &mut Field) {
    *form = None;
    if *field == Field::WordSecret {
        *field = Field::Username;
    }
}
fn routes_word_input(
    screen: Screen,
    field: Field,
    form: Option<&miochat_gpui::word::WordStartForm>,
    owner: &str,
    room: &str,
    generation: u64,
) -> bool {
    screen == Screen::Chat
        && field == Field::WordSecret
        && form.is_some_and(|f| f.bound(owner, room, generation))
}
fn direct_slot(field: Field) -> Option<miochat_gpui::direct_message::FormSlot> {
    use miochat_gpui::direct_message::FormSlot;
    match field {
        Field::DirectTarget => Some(FormSlot::Target),
        Field::DirectExtra => Some(FormSlot::Extra),
        Field::DirectBody => Some(FormSlot::Body),
        _ => None,
    }
}
fn direct_field(slot: miochat_gpui::direct_message::FormSlot) -> Field {
    use miochat_gpui::direct_message::FormSlot;
    match slot {
        FormSlot::Target => Field::DirectTarget,
        FormSlot::Extra => Field::DirectExtra,
        FormSlot::Body => Field::DirectBody,
    }
}
fn routes_direct_input(
    screen: Screen,
    field: Field,
    form: Option<&miochat_gpui::direct_message::DirectMessageForm>,
    owner: &str,
    generation: u64,
) -> bool {
    form.is_some_and(|f| {
        f.routes_input(
            screen == Screen::Chat,
            direct_slot(field).is_some_and(|slot| f.slot_enabled(slot)),
            owner,
            generation,
        )
    })
}

fn discard_chat_action_state(
    screen: Screen,
    form: &mut Option<miochat_gpui::direct_message::DirectMessageForm>,
    pending: &mut Option<PendingSend>,
) {
    miochat_gpui::direct_message::discard_outside_chat(form, screen == Screen::Chat);
    if screen != Screen::Chat {
        *pending = None;
    }
}

struct PendingDraft {
    request_id: String,
    kind: String,
    body: String,
    reply_to: i64,
}

struct PendingRoom {
    request_id: String,
    room: String,
}

struct MioChatApp {
    custom_emoji: emoji_attachments_ui::State,
    attachment_ui: attachments_ui::State,
    dot_ui: dot_ui::State,
    emoji: emoji_ui::EmojiState,
    reading_move: Option<reading_ui::ReadingMove>,
    sidebar_visible: bool,
    input_history: input_history::History,
    completion: Option<miochat_gpui::completion::Intent>,
    completion_revision: u64,
    completion_enabled: bool,
    secret_admin: Option<miochat_gpui::secret_admin::Attempt>,
    secret_form_origin: Option<miochat_gpui::composition::Origin>,
    secret_open_input: Option<String>,
    secret_notice: Option<String>,
    secret_cursor: usize,
    maintenance: Option<miochat_gpui::maintenance::Job>,
    maintenance_cursor: usize,
    maintenance_notice: Option<String>,
    session_origin: Option<miochat_gpui::composition::Origin>,
    session_origin_nonce: u64,
    composition_chat_input: Option<String>,
    composition: Option<miochat_gpui::composition::Editor>,
    composition_after: Option<composition_ui::CompositionAfter>,
    composition_layout: Option<composition_text::TextLayout>,
    composition_drag: Option<usize>,
    composition_cursor: Option<(usize, usize)>,
    composition_scroll: gpui::ScrollHandle,
    composition_reveal: bool,
    screen: Screen,
    field: Field,
    spec: ConnectionSpec,
    port_text: String,
    host_key: Option<HostKeyProbe>,
    session: Option<Session>,
    generation: u64,
    request_counter: u64,
    registration_mode: String,
    registering: bool,
    username: String,
    password: String,
    confirm: String,
    invite: String,
    user: String,
    rooms: Vec<Room>,
    room: String,
    pending_room: Option<PendingRoom>,
    messages: Vec<Message>,
    message_ids: HashSet<i64>,
    composer: String,
    moderation_epoch: u64,
    pending_send: Option<PendingSend>,
    quarantine_confirmation: Option<(String, String, u64, i64)>,
    word_start: Option<miochat_gpui::word::WordStartForm>,
    direct_form: Option<miochat_gpui::direct_message::DirectMessageForm>,
    status: String,
    error: Option<String>,
    focus: FocusHandle,
    input_bounds: Option<Bounds<Pixels>>,
    marked_range: Option<Range<usize>>,
    background_tx: crossbeam_channel::Sender<BackgroundEvent>,
    background_rx: crossbeam_channel::Receiver<BackgroundEvent>,
    connecting: bool,
    auto_connect_pending: bool,
    advanced_connection: bool,
    login_gate: miochat_gpui::login::LoginGate,
    mock_mode: bool,
    fixture_driver: Option<std::path::PathBuf>,
    appearance: Appearance,
    material_status: String,
    notifications: Notifications,
    notification_targets: std::collections::HashMap<String, (u64, String)>,
    notification_preview: bool,
    foreground: bool,
    window_handle: Option<gpui::AnyWindowHandle>,
    selection: Range<usize>,
    appearance_menu: bool,
    mock_menu: bool,
    theme_cursor: usize,
    menu_focus: FocusHandle,
    message_focus: FocusHandle,
    message_selection: Option<(i64, Range<usize>)>,
    message_drag: Option<(i64, usize)>,
    feature_panel: Option<Panel>,
    room_form: Option<RoomIntent>,
    pending_room_form: Option<String>,
    room_target: String,
    account_preferences: Preferences,
    preferences_enabled: bool,
    preferences_open: bool,
    preferences_cursor: usize,
    pending_preferences: Option<String>,
    key_reminder_visible: bool,
    key_reminder_cursor: usize,
    center: Center,
    center_enabled: bool,
    center_open: bool,
    center_section: String,
    center_cursor: Option<usize>,
    pending_center: Option<String>,
    center_bootstrapped: bool,
    profile: Option<Profile>,
    profile_enabled: bool,
    presence_enabled: bool,
    pending_profile: Option<String>,
    profile_editor: Option<ProfileEdit>,
    profile_text: String,
    feature_revision: u64,
    feature_filter: Option<feature_menu::Filter>,
    feature_view: Option<(gpui::ScrollHandle, gpui::ScrollAnchor)>,
    feature_filter_scroll: gpui::ScrollHandle,
    must_reset: bool,
    pending_password: Option<String>,
    pending_draft: Option<PendingDraft>,
    draft_version: Option<i64>,
    draft_conflict: bool,
    draft_backup_id: String,
    manual_draft_backups: std::collections::HashMap<String, String>,
    reply_to: i64,
    native_actions: bool,
    conversation_drafts: std::collections::HashMap<String, String>,
    persistence: Persistence,
    persistence_enabled: bool,
    persistence_clock: Instant,
    timeline_scroll: gpui::ScrollHandle,
    message_revision: u64,
    layout_revision: u64,
    layout_epoch: u64,
    restoring_view: Option<(Position, Option<u64>)>,
    pending_view: Option<PendingView>,
    view_unavailable: bool,
    feed_latest: bool,
    timeline_cleared: bool,
    queued_room: Option<String>,
    close_started: Option<u64>,
    close_ready: bool,
    identity_file: String,
    identity_passphrase: String,
    key_label: String,
    device_key: Option<DeviceKey>,
    device_keys_enabled: bool,
    pending_key_bind: Option<String>,
    pending_key_login: Option<String>,
}

impl MioChatApp {
    fn new(cx: &mut Context<Self>) -> Self {
        let arguments: Vec<_> = std::env::args_os().collect();
        let fixture_driver = arguments
            .iter()
            .position(|a| a == "--fixture-driver")
            .and_then(|at| arguments.get(at + 1))
            .map(std::path::PathBuf::from);
        let (background_tx, background_rx) = crossbeam_channel::bounded(8);
        let notify_tx = background_tx.clone();
        cx.on_system_notification_response(move |response, _| {
            let _ = notify_tx.try_send(BackgroundEvent::NotificationClicked {
                tag: response.tag.to_string(),
            });
        });
        let mut result = Self {
            screen: Screen::Connect,
            field: Field::Username,
            sidebar_visible: true,
            reading_move: None,
            attachment_ui: attachments_ui::State::default(),
            dot_ui: dot_ui::State::default(),
            custom_emoji: emoji_attachments_ui::State::default(),
            emoji: emoji_ui::EmojiState::default(),
            spec: ConnectionSpec::public_service(),
            port_text: "22".into(),
            host_key: None,
            session: None,
            generation: 0,
            request_counter: 0,
            registration_mode: String::new(),
            registering: false,
            username: String::new(),
            password: String::new(),
            confirm: String::new(),
            invite: String::new(),
            user: String::new(),
            rooms: Vec::new(),
            room: String::new(),
            pending_room: None,
            messages: Vec::new(),
            message_ids: HashSet::new(),
            composer: String::new(),
            moderation_epoch: 0,
            pending_send: None,
            quarantine_confirmation: None,
            word_start: None,
            input_history: input_history::History::default(),
            completion: None,
            completion_revision: 0,
            completion_enabled: false,
            session_origin: None,
            session_origin_nonce: 0,
            composition_chat_input: None,
            secret_admin: None,
            secret_form_origin: None,
            secret_open_input: None,
            secret_notice: None,
            secret_cursor: 0,
            maintenance: None,
            maintenance_cursor: 0,
            maintenance_notice: None,
            composition: None,
            composition_after: None,
            composition_layout: None,
            composition_drag: None,
            composition_cursor: None,
            composition_scroll: gpui::ScrollHandle::new(),
            composition_reveal: false,
            direct_form: None,
            status: "Disconnected".into(),
            error: None,
            focus: cx.focus_handle(),
            input_bounds: None,
            marked_range: None,
            background_tx,
            background_rx,
            connecting: false,
            auto_connect_pending: true,
            advanced_connection: false,
            login_gate: miochat_gpui::login::LoginGate::default(),
            mock_mode: std::env::args().any(|arg| arg == "--mock"),
            fixture_driver,
            appearance: Appearance::Keel,
            material_status: "默认".into(),
            notifications: Notifications::default(),
            notification_targets: std::collections::HashMap::new(),
            notification_preview: false,
            foreground: true,
            window_handle: None,
            selection: 0..0,
            appearance_menu: false,
            mock_menu: false,
            theme_cursor: 0,
            menu_focus: cx.focus_handle(),
            message_focus: cx.focus_handle(),
            message_selection: None,
            message_drag: None,
            feature_panel: None,
            room_form: None,
            pending_room_form: None,
            room_target: String::new(),
            account_preferences: Preferences::default(),
            preferences_enabled: false,
            preferences_open: false,
            preferences_cursor: 0,
            pending_preferences: None,
            key_reminder_visible: false,
            key_reminder_cursor: 0,
            center: Center::default(),
            center_enabled: false,
            center_open: false,
            center_section: "contacts".into(),
            center_cursor: None,
            pending_center: None,
            center_bootstrapped: false,
            profile: None,
            profile_enabled: false,
            presence_enabled: false,
            pending_profile: None,
            profile_editor: None,
            profile_text: String::new(),
            feature_revision: 0,
            feature_filter: None,
            feature_view: None,
            feature_filter_scroll: gpui::ScrollHandle::new(),
            must_reset: false,
            pending_password: None,
            pending_draft: None,
            draft_version: None,
            draft_conflict: false,
            draft_backup_id: String::new(),
            manual_draft_backups: Default::default(),
            reply_to: 0,
            native_actions: false,
            conversation_drafts: Default::default(),
            persistence: Persistence::default(),
            persistence_enabled: false,
            persistence_clock: Instant::now(),
            timeline_scroll: gpui::ScrollHandle::new(),
            message_revision: 0,
            layout_revision: u64::MAX,
            layout_epoch: 0,
            restoring_view: None,
            pending_view: None,
            view_unavailable: false,
            feed_latest: true,
            timeline_cleared: false,
            queued_room: None,
            close_started: None,
            close_ready: false,
            identity_file: String::new(),
            identity_passphrase: String::new(),
            key_label: "当前设备".into(),
            device_key: None,
            device_keys_enabled: false,
            pending_key_bind: None,
            pending_key_login: None,
        };
        cx.spawn(async move |this, cx| {
            loop {
                cx.background_executor()
                    .timer(Duration::from_millis(16))
                    .await;
                if this.update(cx, |app, cx| app.poll(cx)).is_err() {
                    break;
                }
            }
        })
        .detach();
        if result.mock_mode && result.fixture_driver.is_some() {
            result.error = Some("不能同时指定 mock 和合成服务。".into());
            result.auto_connect_pending = false;
            result.fixture_driver = None;
            result.mock_mode = false;
        } else if result.fixture_driver.is_some() {
            result.username = "preview".into();
            result.open_session();
        } else if result.mock_mode {
            result.username = "本地访客".into();
            result.open_session();
        }
        result
    }

    fn next_request_id(&mut self, prefix: &str) -> String {
        self.request_counter += 1;
        format!("{prefix}:{}:{}", self.generation, self.request_counter)
    }

    fn poll(&mut self, cx: &mut Context<Self>) {
        if miochat_gpui::login::take_startup_attempt(
            &mut self.auto_connect_pending,
            self.mock_mode,
            self.fixture_driver.is_some(),
        ) {
            self.begin_connect(cx);
        }
        self.attachment_tick(cx);
        self.poll_custom_emoji(cx);
        self.dot_tick();
        if self.word_start.as_ref().is_some_and(|f| {
            self.screen != Screen::Chat || !f.bound(&self.user, &self.room, self.generation)
        }) {
            self.cancel_word_start(cx);
        }

        while let Ok(event) = self.background_rx.try_recv() {
            match event {
                BackgroundEvent::NotificationClicked { tag } => {
                    if let Some((generation, room)) = self.notification_targets.get(&tag).cloned()
                        && generation == self.generation
                        && self.screen == Screen::Chat
                        && self.rooms.iter().any(|candidate| candidate.name == room)
                    {
                        if let Some(handle) = self.window_handle {
                            let _ = handle.update(cx, |_, window, _| window.activate_window());
                        }
                        self.select_room(room.clone(), cx);
                        self.notifications.read(&room);
                        cx.dismiss_system_notification(&tag);
                        cx.notify();
                    }
                }
                BackgroundEvent::HostKey { generation, result }
                    if generation == self.generation =>
                {
                    self.connecting = false;
                    match result {
                        Ok(Some(probe)) => {
                            self.host_key = Some(probe);
                            self.set_screen(Screen::Trust);
                            self.status = "Host key confirmation required".into();
                        }
                        Ok(None) => self.open_session(),
                        Err(error) => {
                            self.clear_login_secrets();
                            self.login_gate.clear();
                            self.error = Some(error);
                        }
                    }
                    cx.notify();
                }
                _ => {}
            }
        }
        let mut events = Vec::new();
        if let Some(session) = self.session.as_ref() {
            while events.len() < 64 {
                match session.events().try_recv() {
                    Ok(event) => events.push(event),
                    Err(_) => break,
                }
            }
        }
        let had_events = !events.is_empty();
        for event in events {
            match event {
                SessionEvent::Response {
                    generation,
                    response,
                } if generation == self.generation => self.handle_response(*response, cx),
                SessionEvent::Error {
                    generation,
                    message,
                } if generation == self.generation => {
                    self.login_gate.clear();
                    self.connecting = false;
                    self.attachment_ui.capture_transport_recovery(
                        self.pending_password.is_some()
                            || self.pending_key_bind.is_some()
                            || self.must_reset
                            || matches!(self.screen, Screen::Password | Screen::KeyBinding),
                    );
                    self.error = Some(if self.pending_password.is_some() {
                        "改密期间连接中断，结果待确认；请重新登录核验。".into()
                    } else if self.pending_key_bind.is_some() {
                        "设备密钥绑定连接中断，结果待确认；重新连接后请查看密钥列表。".into()
                    } else if self
                        .pending_send
                        .as_ref()
                        .is_some_and(|p| p.sanction.is_some())
                    {
                        miochat_gpui::sanctions::UNCERTAIN.into()
                    } else if self
                        .pending_send
                        .as_ref()
                        .is_some_and(|p| p.report.is_some())
                    {
                        miochat_gpui::report::UNCERTAIN.into()
                    } else if self
                        .pending_send
                        .as_ref()
                        .is_some_and(|p| p.configuration.is_some())
                    {
                        miochat_gpui::configuration::UNCERTAIN.into()
                    } else if self.pending_send.as_ref().is_some_and(|p| {
                        p.one_off
                            || p.simple_action.is_some()
                            || p.community.is_some()
                            || p.tic.is_some()
                            || p.guess.is_some()
                            || p.word.is_some()
                    }) {
                        "消息动作发送期间连接中断，结果待确认；请重新连接后查看原目标会话，勿直接重复发送。".into()
                    } else {
                        message
                    });
                    self.clear_center();
                    self.clear_profile();
                    self.observe_persistence();
                    self.persistence.disconnected();
                    self.pending_view = None;
                    self.restoring_view = None;
                    self.queued_room = None;
                    self.close_started = None;
                    self.status = "Connection error".into();
                    self.direct_form = None;
                    if let Some(mut session) = self.session.take() {
                        session.disconnect();
                    }
                    self.password.clear();
                    self.confirm.clear();
                    self.invite.clear();
                    self.pending_password = None;
                    self.clear_key_secrets();
                    self.set_feature_panel(None);
                    self.pending_draft = None;
                    self.draft_version = None;
                    self.draft_conflict = false;
                    self.draft_backup_id.clear();
                    self.set_screen(Screen::Connect);
                    self.field = Field::Username;
                    self.reset_selection();
                    self.pending_send = None;
                    self.pending_room = None;
                }
                SessionEvent::Ended { generation } if generation == self.generation => {
                    self.login_gate.clear();
                    self.connecting = false;
                    self.attachment_ui.capture_transport_recovery(
                        self.pending_password.is_some()
                            || self.pending_key_bind.is_some()
                            || self.must_reset
                            || matches!(self.screen, Screen::Password | Screen::KeyBinding),
                    );
                    self.observe_persistence();
                    self.persistence.disconnected();
                    self.pending_view = None;
                    self.restoring_view = None;
                    self.queued_room = None;
                    self.close_started = None;
                    let password_uncertain = self.pending_password.is_some();
                    let key_uncertain = self.pending_key_bind.is_some();
                    let sanction_uncertain = self
                        .pending_send
                        .as_ref()
                        .is_some_and(|p| p.sanction.is_some());
                    let report_uncertain = self
                        .pending_send
                        .as_ref()
                        .is_some_and(|p| p.report.is_some());
                    let configuration_uncertain = self
                        .pending_send
                        .as_ref()
                        .is_some_and(|p| p.configuration.is_some());
                    let direct_uncertain = self.pending_send.as_ref().is_some_and(|p| {
                        p.one_off
                            || p.simple_action.is_some()
                            || p.community.is_some()
                            || p.tic.is_some()
                            || p.guess.is_some()
                            || p.word.is_some()
                    });
                    self.direct_form = None;
                    self.session = None;
                    self.clear_center();
                    self.clear_profile();
                    self.preferences_open = false;
                    self.pending_preferences = None;
                    self.key_reminder_visible = false;
                    self.set_feature_panel(None);
                    self.pending_draft = None;
                    self.draft_version = None;
                    self.draft_conflict = false;
                    self.draft_backup_id.clear();
                    self.password.clear();
                    self.confirm.clear();
                    self.invite.clear();
                    self.pending_password = None;
                    self.clear_key_secrets();
                    self.set_screen(Screen::Connect);
                    self.field = Field::Username;
                    self.reset_selection();
                    self.status = "Disconnected by server".into();
                    self.pending_send = None;
                    self.pending_room = None;
                    self.error.get_or_insert_with(|| {
                        if password_uncertain {
                            "改密期间连接中断，结果待确认；请重新登录核验。".into()
                        } else if key_uncertain {
                            "设备密钥绑定连接中断，结果待确认；重新连接后请查看密钥列表。".into()
                        } else if sanction_uncertain {
 miochat_gpui::sanctions::UNCERTAIN.into()
 } else if report_uncertain {
                            miochat_gpui::report::UNCERTAIN.into()
                        } else if configuration_uncertain {
                            miochat_gpui::configuration::UNCERTAIN.into()
                        } else if direct_uncertain {
                            "消息动作发送期间连接中断，结果待确认；请先查看原目标会话，勿直接重复发送。"
                                .into()
                        } else {
                            "The server closed the SSH session.".into()
                        }
                    });
                }
                _ => {}
            }
        }
        if self.word_start.as_ref().is_some_and(|f| {
            self.screen != Screen::Chat || !f.bound(&self.user, &self.room, self.generation)
        }) {
            self.cancel_word_start(cx);
        }
        self.poll_secret_admin(cx);
        self.poll_maintenance(cx);
        self.poll_composition(cx);
        self.feature_input_poll();
        self.sync_input_history();
        self.poll_local_view(cx);
        self.poll_completion();
        if had_events {
            cx.notify();
        }
    }

    fn handle_response(&mut self, response: Response, cx: &mut Context<Self>) {
        if response.kind == "hello" {
            if response.protocol != 1 {
                self.cancel_connect(cx);
                self.error = Some(
                    "服务器未提供兼容的 MioChat GUI 协议；需管理员升级配套服务端，应用密码未发送。"
                        .into(),
                );
                return;
            }
            if !self
                .login_gate
                .accept_hello(self.generation, response.protocol)
            {
                return;
            }
        }
        if response.kind == "auth"
            && (!self.login_gate.ready(self.generation)
                || !(self.login_gate.finish(&response.request_id)
                    || self.pending_key_login.as_deref() == Some(&response.request_id)))
        {
            return;
        }
        if self.attachment_response(&response, cx) || self.dot_response(&response, cx) {
            return;
        }
        if !context_ui::edit_response_admitted(&response, self.pending_send.as_ref()) {
            return;
        }
        // Exclusive prefix-only read gate precedes every global response effect.
        if self.handle_completion_response(&response, cx) {
            return;
        }
        if matches!(
            response.kind.as_str(),
            "state" | "auth" | "draft" | "sent" | "action" | "reset_required"
        ) {
            self.cancel_completion();
        }
        if self.handle_secret_admin_response(&response, cx) {
            return;
        }
        if self.handle_maintenance_response(&response, cx) {
            return;
        }
        if self.handle_composition_response(&response, cx) {
            return;
        }
        if !configuration_response_admitted(&response, self.pending_send.as_ref(), &self.user) {
            if self
                .pending_send
                .as_ref()
                .is_some_and(|p| p.request_id == response.request_id)
            {
                self.pending_send = None;
                self.finish_direct_form(&response.request_id, false);
            }
            self.error = Some(miochat_gpui::configuration::UNCERTAIN.into());
            return;
        }
        if response.configuration_receipt.is_some() {
            let pending = self.pending_send.take().unwrap(); // Admitted exact matching intent above.
            self.finish_direct_form(&response.request_id, true);
            if self.room == pending.room
                && miochat_gpui::direct_message::clear_unchanged_submission(
                    &mut self.composer,
                    &pending.body,
                )
            {
                self.reset_selection();
                self.observe_persistence();
            }
            self.status = response.status;
            self.error = None;
            return; // Never navigate, alter reply, or render sibling projections.
        }

        if matches!(response.kind.as_str(), "action" | "sent")
            && (response.management_receipt.is_some()
                || self
                    .pending_send
                    .as_ref()
                    .is_some_and(|p| p.request_id == response.request_id && p.management.is_some()))
        {
            let accepted = self.pending_send.as_ref().is_some_and(|p| {
                p.request_id == response.request_id
                    && p.management
                        .as_ref()
                        .is_some_and(|i| i.accepts(&response, &self.user, &p.room))
            });
            if !accepted {
                if self
                    .pending_send
                    .as_ref()
                    .is_some_and(|p| p.request_id == response.request_id)
                {
                    self.pending_send = None;
                    self.finish_direct_form(&response.request_id, false);
                }
                self.error = Some("管理回执范围异常；输入保留，请先核对结果，勿重复提交。".into());
                return;
            }
            let pending = self.pending_send.take().unwrap();
            self.finish_direct_form(&response.request_id, true);
            if self.room == pending.room
                && miochat_gpui::direct_message::clear_unchanged_submission(
                    &mut self.composer,
                    &pending.body,
                )
            {
                self.reset_selection();
                self.observe_persistence();
            }
            self.status = response.status;
            self.error = None;
            return; // A committed receipt never changes scope/reply/timeline or reopens a panel.
        }
        if matches!(response.kind.as_str(), "action" | "sent")
            && (response.sanction_receipt.is_some()
                || self
                    .pending_send
                    .as_ref()
                    .is_some_and(|p| p.request_id == response.request_id && p.sanction.is_some()))
        {
            let accepted = self.pending_send.as_ref().is_some_and(|p| {
                p.request_id == response.request_id
                    && p.sanction
                        .as_ref()
                        .is_some_and(|i| i.accepts(&response, &self.user, &p.room))
            });
            if !accepted {
                if self
                    .pending_send
                    .as_ref()
                    .is_some_and(|p| p.request_id == response.request_id)
                {
                    self.pending_send = None;
                    self.finish_direct_form(&response.request_id, false);
                }
                self.error = Some("管理回执范围异常；输入保留，请先核对结果，勿重复提交。".into());
                return;
            }
            let pending = self.pending_send.take().unwrap();
            self.finish_direct_form(&response.request_id, true);
            if self.room == pending.room
                && miochat_gpui::direct_message::clear_unchanged_submission(
                    &mut self.composer,
                    &pending.body,
                )
            {
                self.reset_selection();
                self.observe_persistence();
            }
            self.status = response.status;
            self.error = None;
            return; // A committed receipt never changes scope/reply/timeline or reopens a panel.
        }
        if matches!(response.kind.as_str(), "action" | "sent")
            && (response.report_receipt.is_some()
                || self
                    .pending_send
                    .as_ref()
                    .is_some_and(|p| p.request_id == response.request_id && p.report.is_some()))
        {
            let accepted = self.pending_send.as_ref().is_some_and(|p| {
                p.request_id == response.request_id
                    && p.report
                        .as_ref()
                        .is_some_and(|i| i.accepts(&response, &self.user, &p.room))
            });
            if !accepted {
                if self
                    .pending_send
                    .as_ref()
                    .is_some_and(|p| p.request_id == response.request_id)
                {
                    self.pending_send = None;
                    self.finish_direct_form(&response.request_id, false);
                }
                self.error = Some("举报回执范围异常；输入保留，请先核对结果，勿重复提交。".into());
                return;
            }
            let pending = self.pending_send.take().unwrap();
            self.finish_direct_form(&response.request_id, true);
            if self.room == pending.room
                && miochat_gpui::direct_message::clear_unchanged_submission(
                    &mut self.composer,
                    &pending.body,
                )
            {
                self.reset_selection();
                self.observe_persistence();
            }
            self.status = response.status;
            self.error = None;
            return; // A committed receipt never changes scope/reply/timeline or reopens a panel.
        }
        {
            let private = miochat_gpui::moderation::requires_private_ticket(
                &response,
                self.pending_send
                    .as_ref()
                    .is_some_and(|p| p.request_id == response.request_id && p.moderation.is_some()),
            );
            if private
                && !self.pending_send.as_ref().is_some_and(|p| {
                    p.request_id == response.request_id
                        && p.moderation.as_ref().is_some_and(|t| {
                            t.accepts(
                                &response,
                                &self.user,
                                &self.room,
                                self.generation,
                                self.moderation_epoch,
                            )
                        })
                })
            {
                if self
                    .pending_send
                    .as_ref()
                    .is_some_and(|p| p.request_id == response.request_id)
                {
                    self.pending_send = None;
                    self.finish_direct_form(&response.request_id, false);
                }
                self.error =
                    Some("管理查询已过期或响应目标不符；未显示内容，请重新核对查询。".into());
                return;
            }
        }
        if !information_response_admitted(&response, self.pending_send.as_ref(), &self.room) {
            if self
                .pending_send
                .as_ref()
                .is_some_and(|p| p.request_id == response.request_id)
            {
                self.pending_send = None;
                self.finish_direct_form(&response.request_id, false);
            }
            self.error =
                Some("信息查询已过期或响应内容不符；未应用任何资料、控制中心或设置更新。".into());
            return;
        }
        if let Some(p) = response.profile.as_ref() {
            let saved = response.kind == "action"
                && self
                    .pending_send
                    .as_ref()
                    .is_some_and(|r| r.request_id == response.request_id);
            let show = saved || self.pending_profile.as_deref() == Some(&response.request_id);
            self.accept_profile(p.clone(), show, saved, cx);
        }
        if let Some(center) = response.center.as_ref() {
            let show = response.kind == "action"
                && self
                    .pending_send
                    .as_ref()
                    .is_some_and(|p| p.request_id == response.request_id);
            self.accept_center(center.clone(), show, cx);
        }
        if response.kind != "auth"
            && let Some(p) = response.preferences.as_ref()
        {
            self.accept_preferences(p.clone(), cx);
        }
        if response.kind == "key_bound" {
            self.finish_key_binding(response);
            return;
        }
        if self.persistence_enabled
            && matches!(
                response.kind.as_str(),
                "draft" | "draft_saved" | "draft_conflict" | "draft_suspended"
            )
        {
            self.handle_persistent_draft(response);
            return;
        }
        if self.persistence_enabled && matches!(response.kind.as_str(), "window" | "window_append")
        {
            self.handle_view(response, cx);
            return;
        }
        match response.kind.as_str() {
            "hello" if response.protocol == 1 => {
                self.connecting = false;
                self.advanced_connection = false;
                self.attachment_set_capabilities(&response.capabilities);
                self.dot_ui.supported = response
                    .capabilities
                    .iter()
                    .any(|c| c == "dot-owner-review-v1");
                self.profile_enabled = response.capabilities.iter().any(|c| c == "profile-v1");
                self.presence_enabled = response.capabilities.iter().any(|c| c == "presence-v1");
                self.center_enabled = response
                    .capabilities
                    .iter()
                    .any(|c| c == "control-center-v1");
                self.preferences_enabled =
                    response.capabilities.iter().any(|c| c == "preferences-v1");
                self.device_keys_enabled =
                    response.capabilities.iter().any(|c| c == "device-keys-v1");
                self.device_key = response.device_key;
                self.persistence_enabled = !self.mock_mode
                    && response
                        .capabilities
                        .iter()
                        .any(|c| c == "draft-position-v1");
                self.completion_enabled =
                    response.capabilities.iter().any(|c| c == "completion-v1");
                self.native_actions = response
                    .capabilities
                    .iter()
                    .any(|c| c == "native-actions-v1");
                self.registration_mode = response.registration_mode;
                self.set_screen(Screen::Auth);
                self.field = Field::Username;
                self.reset_selection();
                self.status = "Secure transport connected".into();
                self.error = None;
                if self.fixture_driver.is_some() {
                    self.username = "preview".into();
                    self.password = "fixture-local-only".into();
                    self.authenticate(false, cx);
                } else if self.device_keys_enabled
                    && self.device_key.as_ref().is_some_and(|k| k.bound)
                {
                    self.authenticate_key(cx);
                }
            }
            "hello" => self.error = Some("Unsupported MioChat GUI protocol version.".into()),
            "auth" => {
                discard_word_input(&mut self.word_start, &mut self.field);
                self.quarantine_confirmation = None;
                self.direct_form = None;
                self.clear_profile();
                self.clear_center();
                self.clear_profile();
                self.account_preferences = Preferences::default();
                self.preferences_open = false;
                self.pending_preferences = None;
                self.key_reminder_visible = false;
                self.pending_key_login = None;
                self.identity_passphrase.clear();
                if self.user == response.user && !self.room.is_empty() && !self.composer.is_empty()
                {
                    if !miochat_gpui::word::reserved_start(&self.composer) {
                        self.conversation_drafts
                            .insert(self.room.clone(), self.composer.clone());
                    }
                } else if self.user != response.user {
                    self.conversation_drafts.clear();
                    self.manual_draft_backups.clear();
                }
                self.composer.clear();
                self.room.clear();
                self.rooms.clear();
                self.attachment_clear_message_assets(cx);
                self.messages.clear();
                self.message_ids.clear();
                self.set_feature_panel(None);
                self.message_selection = None;
                self.pending_draft = None;
                self.draft_version = None;
                self.draft_conflict = false;
                self.draft_backup_id.clear();
                if self.user != response.user {
                    self.notifications = Notifications::default();
                    self.notification_targets.clear();
                }
                self.user = response.user;
                if let Some(p) = response.preferences {
                    self.accept_preferences(p, cx);
                }
                self.key_reminder_visible =
                    response.key_reminder && self.account_preferences.reminder();
                self.key_reminder_cursor = 0;
                self.password.clear();
                self.confirm.clear();
                self.invite.clear();
                self.must_reset = response.status == "password_reset_required";
                self.set_screen(if self.must_reset {
                    Screen::Password
                } else {
                    Screen::Chat
                });
                if self.must_reset {
                    self.field = Field::Password;
                    self.reset_selection();
                }
                self.status = if self.must_reset {
                    "请先修改临时密码".into()
                } else {
                    "Connected".into()
                };
            }
            "reset_required" => {
                self.observe_persistence();
                self.persistence.disconnected();
                self.pending_view = None;
                self.restoring_view = None;
                self.queued_room = None;
                self.must_reset = true;
                self.clear_center();
                self.clear_profile();
                self.preferences_open = false;
                self.pending_preferences = None;
                self.key_reminder_visible = false;
                self.set_screen(Screen::Password);
                self.field = Field::Password;
                self.rooms.clear();
                self.attachment_clear_message_assets(cx);
                self.messages.clear();
                self.message_ids.clear();
                self.set_feature_panel(None);
                self.status = "请先修改临时密码".into();
                self.reset_selection();
            }
            "password_changed" => {
                if self.pending_password.as_deref() != Some(&response.request_id) {
                    return;
                }
                self.pending_password = None;
                self.password.clear();
                self.confirm.clear();
                self.invite.clear();
                self.must_reset = false;
                self.set_screen(Screen::Chat);
                self.status = "密码已修改，当前会话保留".into();
                self.error = None;
                self.reset_selection();
            }
            "draft" | "draft_saved" | "draft_conflict" => {
                if self
                    .pending_draft
                    .as_ref()
                    .is_none_or(|p| p.request_id != response.request_id)
                {
                    return;
                }
                let pending = self.pending_draft.take().unwrap();
                if let Some(draft) = response.draft {
                    if draft.room != self.room {
                        return;
                    }
                    let unchanged =
                        self.composer == pending.body && self.reply_to == pending.reply_to;
                    self.apply_draft(
                        draft,
                        response.kind == "draft" && pending.kind == "draft_load" && unchanged,
                    );
                    self.status = match response.kind.as_str() {
                        "draft_saved" => "草稿已保存".into(),
                        "draft_conflict" => {
                            "另一设备已更新：本次保存内容已私有备份，请选择保留本地或读取远端"
                                .into()
                        }
                        _ if unchanged => "已读取当前对话草稿；原输入可用备份按钮恢复".into(),
                        _ => "草稿版本已读取；当前新编辑保留在输入框".into(),
                    };
                }
            }
            "profile_unavailable" => {
                self.clear_profile();
                self.error = Some(response.status.clone());
            }
            "profile" => {
                if self.pending_profile.as_deref() == Some(&response.request_id) {
                    self.pending_profile = None;
                }
            }
            "control_center" => {
                if self.pending_center.as_deref() == Some(&response.request_id) {
                    self.pending_center = None;
                }
            }
            "preferences" => {
                if self.pending_preferences.as_deref() == Some(&response.request_id) {
                    self.pending_preferences = None;
                    self.preferences_open = true;
                    self.preferences_cursor = 0;
                }
            }
            "state" => {
                miochat_gpui::moderation::invalidate(
                    &mut self.moderation_epoch,
                    &mut self.feature_panel,
                );
                if !self.center_bootstrapped && self.center_enabled {
                    self.center_bootstrapped = true;
                    self.load_center(cx);
                }
                // Keep unsent form text. The intent remains bound to its room;
                // a forced scope change freezes it and every submit rechecks
                // current permissions/session inside the server transaction.
                if self
                    .feature_panel
                    .as_ref()
                    .is_some_and(|p| !p.kind.is_empty())
                {
                    self.set_feature_panel(None);
                }
                let visible: Vec<String> = response.rooms.iter().map(|r| r.name.clone()).collect();
                let pending = self
                    .pending_room
                    .as_ref()
                    .map(|p| (p.request_id.as_str(), p.room.as_str()));
                if !miochat_gpui::state::accepts_room_state(
                    pending,
                    &response.request_id,
                    &response.room,
                    &visible,
                    response.status == "room_access_changed",
                ) {
                    return;
                }
                if response.status == "roster" && response.room == self.room {
                    self.rooms = response.rooms;
                    self.status = format!("{} online", response.online);
                    return;
                }
                self.notifications.history(
                    &response.room,
                    &response.messages,
                    self.room != response.room,
                );
                if self.foreground {
                    self.notifications.read(&response.room);
                }
                if self.room != response.room {
                    self.observe_persistence();
                    self.draft_version = None;
                    self.draft_conflict = false;
                    self.draft_backup_id.clear();
                    self.pending_draft = None;
                    if !miochat_gpui::word::reserved_start(&self.composer) {
                        self.conversation_drafts
                            .insert(self.room.clone(), self.composer.clone());
                    }
                    self.composer = self
                        .conversation_drafts
                        .remove(&response.room)
                        .filter(|s| !miochat_gpui::word::reserved_start(s))
                        .unwrap_or_default();
                    self.marked_range = None;
                    self.selection = self.composer.len()..self.composer.len();
                    self.reply_to = 0;
                    self.set_feature_panel(None);
                }
                if response.status == "room_access_changed" {
                    self.error = Some("Room access changed; moved to an accessible room.".into());
                }
                self.set_feature_panel(None);
                let permitted: std::collections::HashSet<_> =
                    response.rooms.iter().map(|r| r.name.as_str()).collect();
                self.notification_targets.retain(|tag, (_, room)| {
                    let keep = permitted.contains(room.as_str());
                    if !keep {
                        cx.dismiss_system_notification(tag);
                    }
                    keep
                });
                self.reply_to = response.reply_to;
                if response.status == "room_access_changed" {
                    self.set_feature_panel(None);
                }
                self.pending_room = None;
                self.composition_scope_changed(
                    &response.room,
                    response.status == "room_access_changed",
                );
                self.attachment_ui
                    .freeze(response.status == "room_access_changed");
                self.custom_emoji
                    .freeze(response.status == "room_access_changed");
                self.dot_ui.clear();
                self.room = response.room;
                self.rooms = response.rooms;
                self.timeline_cleared = false;
                self.feed_latest = response.at_latest || !self.persistence_enabled;
                self.activate_persistence();
                self.message_revision = self.message_revision.wrapping_add(1);
                self.attachment_clear_message_assets(cx);
                self.messages.clear();
                self.message_ids.clear();
                for message in response.messages {
                    self.merge_message(message, cx);
                }
                self.status = format!("{} online", response.online);
                if self.direct_input_active() {
                    self.reset_selection();
                }
            }
            "message" | "message_updated" => {
                if let Some(message) = response.message {
                    if response.kind == "message_updated"
                        && miochat_gpui::profile::redact_recalled_quotes(
                            &mut self.messages,
                            &message,
                        )
                    {
                        self.message_revision = self.message_revision.wrapping_add(1);
                    }
                    if response.kind == "message"
                        && self.notifications.live_with_preferences(
                            &message,
                            &self.user,
                            &self.room,
                            self.foreground && self.pending_room.is_none(),
                            &self.account_preferences,
                        )
                    {
                        let tag = format!("miochat-{}-{}", self.generation, message.id);
                        self.notification_targets
                            .insert(tag.clone(), (self.generation, message.room.clone()));
                        while self.notification_targets.len() > 512 {
                            if let Some(old) = self
                                .notification_targets
                                .keys()
                                .find(|key| key.as_str() != tag)
                                .cloned()
                            {
                                self.notification_targets.remove(&old);
                            } else {
                                break;
                            }
                        }
                        let body = if self.notification_preview {
                            format!(
                                "#{} · {}：{}",
                                message.room,
                                message.sender,
                                message
                                    .body
                                    .chars()
                                    .filter(|c| !c.is_control())
                                    .take(80)
                                    .collect::<String>()
                            )
                        } else {
                            "打开 MioChat 查看新消息。".into()
                        };
                        cx.show_system_notification(gpui::SystemNotification {
                            tag: tag.into(),
                            title: "MioChat · 新消息".into(),
                            body: body.into(),
                            actions: vec![],
                        });
                    }
                    if message.room.starts_with("dm:")
                        && !self.rooms.iter().any(|r| r.name == message.room)
                    {
                        self.rooms.push(Room {
                            name: message.room.clone(),
                            topic: "私信".into(),
                        });
                    }
                    if message.room == self.room
                        && (self.feed_latest || self.message_ids.contains(&message.id))
                    {
                        let position = self.reading_position();
                        self.merge_message(message, cx);
                        if miochat_gpui::state::should_restore_live_view(
                            self.persistence_enabled,
                            self.timeline_cleared,
                            self.pending_view.is_some(),
                            self.restoring_view.is_some(),
                        ) {
                            self.restoring_view = Some((position, None));
                        }
                    }
                }
            }
            "action" => {
                if self
                    .pending_send
                    .as_ref()
                    .is_none_or(|p| p.request_id != response.request_id)
                {
                    return;
                }
                let pending = self.pending_send.take().unwrap();
                if pending.edit_message.is_some() {
                    if context_ui::consume_edit_submission(
                        &mut self.composer,
                        &pending.body,
                        &pending.room,
                        &self.room,
                    ) {
                        self.selection = 0..0;
                    }
                    self.finish_direct_form(&response.request_id, true);
                    self.status = response.status.clone();
                    return; // Never navigate or clear a draft on an edit confirmation.
                }
                if pending.moderation.is_some() {
                    self.finish_direct_form(&response.request_id, true);
                }
                if pending.information
                    && !miochat_gpui::information::accepts_information_ack(
                        &response,
                        &pending.room,
                        &self.room,
                    )
                {
                    return;
                }
                if pending.one_off {
                    if !miochat_gpui::direct_message::accepts_one_off_ack(
                        &response,
                        &self.user,
                        &pending.one_off_target,
                        &pending.room,
                    ) {
                        self.finish_direct_form(&response.request_id, false);
                        self.error =
                            Some("私信响应范围异常；输入已保留，请先核对发送结果。".into());
                        return;
                    }
                    self.finish_direct_form(&response.request_id, true);
                    if self.room != pending.room {
                        self.status = response.status;
                        return;
                    }
                }
                if let Some(action) = pending.simple_action {
                    if !action.accepts_ack(&response, &self.user, &pending.room) {
                        self.finish_direct_form(&response.request_id, false);
                        self.error = Some("动作响应范围异常；输入保留，请先核对发送结果。".into());
                        return;
                    }
                    self.finish_direct_form(&response.request_id, true);
                    if self.room != pending.room {
                        self.status = response.status;
                        return;
                    }
                }
                if let Some(intent) = &pending.word {
                    if !intent.accepts_ack(&response, &pending.room) {
                        self.finish_direct_form(&response.request_id, false);
                        self.error = Some("猜词回执范围异常；请刷新核对，勿重复提交。".into());
                        return;
                    }
                    self.finish_direct_form(&response.request_id, true);
                    if self.room != pending.room {
                        self.status = response.status;
                        return;
                    }
                }
                if let Some(intent) = &pending.guess {
                    if !intent.accepts_ack(&response, &pending.room) {
                        self.finish_direct_form(&response.request_id, false);
                        self.error =
                            Some("游戏响应范围异常；请刷新核对，勿重复提交。输入保留。".into());
                        return;
                    }
                    self.finish_direct_form(&response.request_id, true);
                    if self.room != pending.room {
                        self.status = response.status;
                        return;
                    }
                }
                if let Some(intent) = &pending.tic {
                    if !intent.accepts_ack(&response, &pending.room) {
                        self.finish_direct_form(&response.request_id, false);
                        self.error =
                            Some("棋盘响应范围异常；先刷新核对，勿重复提交。输入保留。".into());
                        return;
                    }
                    self.finish_direct_form(&response.request_id, true);
                    if self.room != pending.room {
                        self.status = response.status;
                        return;
                    }
                }
                if let Some(intent) = &pending.community {
                    if !intent.accepts_ack(&response, &pending.room) {
                        self.finish_direct_form(&response.request_id, false);
                        self.error = Some(
                            "社区提交响应范围异常；请先刷新核对，勿重复提交。输入保留。".into(),
                        );
                        return;
                    }
                    self.finish_direct_form(&response.request_id, true);
                    if self.room != pending.room {
                        self.status = response.status;
                        return;
                    }
                }
                if response.clear_view
                    && !miochat_gpui::state::accepts_clear_view(
                        (&pending.request_id, &pending.room),
                        (&response.request_id, &response.room),
                        &self.room,
                    )
                {
                    return;
                }
                if self.pending_room_form.as_deref() == Some(&response.request_id) {
                    self.pending_room_form = None;
                    self.cancel_room_form(cx);
                }
                if miochat_gpui::direct_message::clear_unchanged_submission(
                    &mut self.composer,
                    &pending.body,
                ) {
                    self.selection = 0..0;
                }
                if response.room != self.room {
                    self.observe_persistence();
                    self.draft_version = None;
                    self.draft_conflict = false;
                    self.draft_backup_id.clear();
                    self.pending_draft = None;
                    if !miochat_gpui::word::reserved_start(&self.composer) {
                        self.conversation_drafts
                            .insert(self.room.clone(), self.composer.clone());
                    }
                    self.composer = self
                        .conversation_drafts
                        .remove(&response.room)
                        .filter(|s| !miochat_gpui::word::reserved_start(s))
                        .unwrap_or_default();
                    self.selection = self.composer.len()..self.composer.len();
                    discard_word_input(&mut self.word_start, &mut self.field);
                    self.composition_scope_changed(&response.room, false);
                    self.attachment_ui.freeze(false);
                    self.custom_emoji.freeze(false);
                    self.dot_ui.clear();
                    self.room = response.room.clone();
                    self.activate_persistence();
                    self.attachment_clear_message_assets(cx);
                    self.messages.clear();
                    self.message_ids.clear();
                    if !self.rooms.iter().any(|r| r.name == self.room) {
                        self.rooms.push(Room {
                            name: self.room.clone(),
                            topic: String::new(),
                        });
                    }
                }
                let mut action_error = None;
                if !response.mention.is_empty() {
                    if self.marked_range.is_some() {
                        action_error = Some("请完成当前输入法组合后再次提及；草稿已保留。".into());
                    } else if let Some(end) = miochat_gpui::profile::insert_mention(
                        &mut self.composer,
                        self.selection.end,
                        &response.mention,
                    ) {
                        self.selection = end..end;
                        self.clear_profile();
                    } else {
                        action_error = Some("提及未插入，请检查草稿长度；原文已保留。".into());
                    }
                }
                self.reading_response(&response.request_id, &response.room, true);
                let old_position = self.reading_position();
                if response.clear_view {
                    // Drop outstanding window restoration so an older read cannot
                    // undo this local clear. The persisted draft/position is untouched.
                    self.pending_view = None;
                    self.restoring_view = None;
                    self.view_unavailable = false;
                    self.timeline_cleared = true;
                    self.feed_latest = true;
                    self.attachment_clear_message_assets(cx);
                    self.messages.clear();
                    // Retain seen IDs: delayed/replayed old events must not refill
                    // the cleared screen. Explicit history starts a new visible window.
                    self.message_selection = None;
                    self.message_drag = None;
                    self.message_revision = self.message_revision.wrapping_add(1);
                }
                if response.replace {
                    self.timeline_cleared = false;
                    self.feed_latest = response.at_latest;
                    self.attachment_clear_message_assets(cx);
                    self.messages.clear();
                    self.message_ids.clear();
                }
                let visible_messages: Vec<_> = response
                    .messages
                    .into_iter()
                    .filter(|message| {
                        miochat_gpui::direct_message::message_belongs_to_view(
                            &message.room,
                            &self.room,
                        )
                    })
                    .collect();
                self.notifications
                    .history(&response.room, &visible_messages, false);
                let had_messages = !visible_messages.is_empty();
                for message in visible_messages {
                    self.merge_message(message, cx);
                }
                if let Some(message) = response.message
                    && miochat_gpui::direct_message::message_belongs_to_view(
                        &message.room,
                        &self.room,
                    )
                {
                    self.merge_message(message, cx);
                }
                if reading_ui::restore_after_history(
                    self.persistence_enabled,
                    self.reading_move
                        .as_ref()
                        .is_some_and(|p| p.ready_for(&response.request_id)),
                    response.draft.is_some(),
                    had_messages,
                ) {
                    let position = if response.replace {
                        Position {
                            anchor: self.messages.first().map_or(0, |m| m.id),
                            offset: 1,
                            ..old_position
                        }
                    } else {
                        old_position
                    };
                    self.restoring_view = Some((position, None));
                }
                self.reply_to = response.reply_to;
                self.set_feature_panel(response.panel);
                if let Some(draft) = response.draft {
                    self.apply_draft(draft, true);
                }
                if response.form == "password" {
                    self.password.clear();
                    self.confirm.clear();
                    self.invite.clear();
                    self.set_screen(Screen::Password);
                    self.field = Field::Password;
                    self.reset_selection();
                }
                if response.form == "key_bind" {
                    self.password.clear();
                    self.set_screen(Screen::KeyBinding);
                    self.field = Field::KeyLabel;
                    self.reset_selection();
                }
                let graceful_exit = response.status == "bye";
                self.status = response.status;
                self.error = action_error;
                if graceful_exit {
                    self.disconnect(cx);
                    self.status = "已退出".into();
                }
            }
            "sent" => {
                if self
                    .pending_send
                    .as_ref()
                    .is_some_and(|p| p.request_id == response.request_id)
                {
                    if let Some(draft) = response.draft {
                        self.apply_draft(draft, false);
                    }
                    if !response.status.is_empty() {
                        self.status = response.status.clone();
                    }
                }
                if let Some(message) = response.message
                    && message.room == self.room
                {
                    self.merge_message(message, cx);
                    self.reply_to = 0;
                }
                if self
                    .pending_room
                    .as_ref()
                    .is_some_and(|pending| pending.request_id == response.request_id)
                {
                    self.pending_room = None;
                }
                if self
                    .pending_send
                    .as_ref()
                    .is_some_and(|pending| pending.request_id == response.request_id)
                {
                    let pending = self.pending_send.take().unwrap();
                    if miochat_gpui::direct_message::clear_unchanged_submission(
                        &mut self.composer,
                        &pending.body,
                    ) {
                        self.selection = 0..0;
                    }
                    if self.persistence_enabled {
                        self.observe_persistence();
                        if response.draft_changed {
                            self.persistence.invalidate_active();
                        }
                        if !self.feed_latest
                            && let Err(error) = self.request_view(Position::default(), false)
                        {
                            self.error = Some(format!("{error:#}"));
                        }
                    }
                }
            }
            "status" => {
                if !response.status.is_empty() {
                    self.status = response.status;
                }
            }
            "error" => {
                if self.login_gate.finish(&response.request_id) {
                    self.clear_login_secrets();
                }
                self.reading_response(&response.request_id, &response.room, false);
                self.finish_direct_form(&response.request_id, false);
                if self.pending_profile.as_deref() == Some(&response.request_id) {
                    self.pending_profile = None;
                }
                if self.pending_center.as_deref() == Some(&response.request_id) {
                    self.pending_center = None;
                }
                if self.pending_preferences.as_deref() == Some(&response.request_id) {
                    self.pending_preferences = None;
                }
                self.fail_key_request(&response.request_id);
                if self.pending_room_form.as_deref() == Some(&response.request_id) {
                    self.pending_room_form = None;
                }
                if self
                    .persistence
                    .fail(&response.request_id, self.persistence_now())
                {
                    self.queued_room = None;
                    self.sync_draft_flags();
                }
                if self
                    .pending_view
                    .as_ref()
                    .is_some_and(|p| p.request_id == response.request_id)
                {
                    self.pending_view = None;
                    self.view_unavailable = true;
                }
                if self
                    .pending_draft
                    .as_ref()
                    .is_some_and(|p| p.request_id == response.request_id)
                {
                    self.pending_draft = None;
                }
                if self.pending_password.as_deref() == Some(&response.request_id) {
                    self.pending_password = None;
                    self.password.clear();
                    self.confirm.clear();
                    self.invite.clear();
                    self.reset_selection();
                }
                if self
                    .pending_room
                    .as_ref()
                    .is_some_and(|pending| pending.request_id == response.request_id)
                {
                    self.pending_room = None;
                }
                if self
                    .pending_send
                    .as_ref()
                    .is_some_and(|pending| pending.request_id == response.request_id)
                {
                    self.pending_send = None;
                }
                self.error = Some(response.error);
            }
            _ => {}
        }
    }

    fn merge_message(&mut self, mut message: Message, cx: &mut Context<Self>) {
        self.attachment_message_changed(&message, cx);
        if self.account_preferences.blocks(&message.sender) {
            return;
        }
        if self.account_preferences.blocks(&message.reply_name) {
            message.reply_body.clear();
            message.reply_name.clear();
        }
        if miochat_gpui::state::merge_timeline_message(
            &mut self.messages,
            &mut self.message_ids,
            message,
        ) {
            self.message_revision = self.message_revision.wrapping_add(1);
        }
    }

    fn parse_port(&mut self) -> bool {
        match self.port_text.parse::<u16>() {
            Ok(port) if port > 0 => {
                self.spec.port = port;
                true
            }
            _ => {
                self.error = Some("Port must be between 1 and 65535.".into());
                false
            }
        }
    }

    fn begin_connect(&mut self, cx: &mut Context<Self>) {
        if self.screen != Screen::Connect || self.connecting || self.session.is_some() {
            return;
        }
        self.auto_connect_pending = false;
        self.login_gate.clear();
        self.clear_login_secrets();
        if self.mock_mode {
            self.open_session();
            cx.notify();
            return;
        }
        self.error = None;
        if !self.parse_port() {
            cx.notify();
            return;
        }
        if let Err(error) = self.spec.validate() {
            self.error = Some(format!("{error:#}"));
            return;
        }
        self.generation += 1;
        self.advanced_connection = false;
        self.field = Field::Username;
        self.reset_selection();
        let generation = self.generation;
        let spec = self.spec.clone();
        let tx = self.background_tx.clone();
        self.connecting = true;
        self.status = "Checking server identity…".into();
        std::thread::spawn(move || {
            let result = (|| {
                let known_hosts = app_known_hosts_path()?;
                if is_host_known(&spec, &known_hosts)? {
                    Ok(None)
                } else {
                    Ok(Some(probe_host_key(&spec)?))
                }
            })()
            .map_err(|error: anyhow::Error| format!("{error:#}"));
            let _ = tx.send(BackgroundEvent::HostKey { generation, result });
        });
        cx.notify();
    }

    fn accept_host_key(&mut self, generation: u64, probe: &HostKeyProbe, cx: &mut Context<Self>) {
        if self.screen != Screen::Trust
            || !miochat_gpui::login::trust_confirmation_matches(
                self.generation,
                generation,
                self.host_key.as_ref(),
                probe,
            )
        {
            return;
        }
        let result = (|| {
            let path = app_known_hosts_path()?;
            let probe = self
                .host_key
                .as_ref()
                .ok_or_else(|| anyhow::anyhow!("host key missing"))?;
            trust_host_key(&path, probe)
        })();
        match result {
            Ok(()) => {
                self.host_key = None;
                self.open_session();
            }
            Err(error) => self.error = Some(format!("Could not save host key: {error:#}")),
        }
        cx.notify();
    }

    fn open_session(&mut self) {
        if let Some(mut prior) = self.session.take() {
            prior.disconnect();
        }
        self.login_gate.begin(self.generation);
        self.connecting = true;
        self.cancel_completion();
        self.completion_enabled = false;
        self.input_history.clear();
        self.attachment_ui.begin_session();
        self.custom_emoji.freeze(true);
        self.attachment_set_capabilities(&[]);
        self.dot_ui.supported = false;
        self.dot_ui.clear();
        self.session_origin = None;
        self.session_origin_nonce = self.session_origin_nonce.wrapping_add(1);
        let generation = self.generation;
        if let Some(driver) = self.fixture_driver.as_ref() {
            match Session::fixture(driver, generation) {
                Ok(session) => {
                    self.session_origin = Some(miochat_gpui::composition::Origin::synthetic(
                        &format!("fixture:{}", driver.display()),
                        self.session_origin_nonce,
                    ));
                    self.session = Some(session);
                    self.status = "本地合成联调 · 无网络".into();
                }
                Err(error) => {
                    self.login_gate.clear();
                    self.connecting = false;
                    self.error = Some(format!("{error:#}"));
                }
            }
            return;
        }
        if self.mock_mode {
            self.session_origin = Some(miochat_gpui::composition::Origin::synthetic(
                "mock",
                self.session_origin_nonce,
            ));
            self.session = Some(Session::mock(generation));
            self.status = "本地模拟 · 无网络连接".into();
            self.user.clear();
            self.room.clear();
            self.rooms.clear();
            self.attachment_ui.clear_message_assets();
            self.messages.clear();
            self.message_ids.clear();
            self.pending_room = None;
            self.pending_send = None;
            return;
        }
        let connection = app_known_hosts_path().and_then(|path| {
            let context = miochat_gpui::connection::trusted_host_context(&self.spec, &path)?;
            let origin =
                miochat_gpui::composition::Origin::ssh(&self.spec, context, &self.identity_file)
                    .ok_or_else(|| anyhow::anyhow!("trusted connection identity unavailable"))?;
            let session = if self.identity_file.is_empty() {
                Session::connect(&self.spec, &path, generation)
            } else {
                let bridge = std::env::current_exe()?.with_file_name(if cfg!(windows) {
                    "miochat-ssh-bridge.exe"
                } else {
                    "miochat-ssh-bridge"
                });
                Session::connect_identity(
                    &self.spec,
                    &path,
                    std::path::Path::new(&self.identity_file),
                    &self.identity_passphrase,
                    &bridge,
                    generation,
                )
            }?;
            Ok((session, origin))
        });
        self.identity_passphrase.clear();
        match connection {
            Ok((session, origin)) => {
                self.session_origin = Some(origin);
                self.session = Some(session);
                self.status = "Connecting…".into();
                self.error = None;
                self.user.clear();
                self.rooms.clear();
                self.room.clear();
                self.attachment_ui.clear_message_assets();
                self.messages.clear();
                self.message_ids.clear();
                self.pending_room = None;
                self.pending_send = None;
            }
            Err(error) => {
                self.login_gate.clear();
                self.connecting = false;
                self.clear_login_secrets();
                self.error = Some(format!("{error:#}"));
            }
        }
    }

    fn authenticate(&mut self, register: bool, cx: &mut Context<Self>) {
        if self.screen != Screen::Auth
            || self.session.is_none()
            || !self.login_gate.can_submit(self.generation)
            || self.pending_key_login.is_some()
        {
            self.error = Some("连接与协议尚未就绪，或登录请求正在处理；密码未重复发送。".into());
            cx.notify();
            return;
        }
        let request_id = self.next_request_id(if register { "register" } else { "login" });
        if !self.login_gate.submit(self.generation, &request_id) {
            return;
        }
        let request = Request {
            attachment: None,
            dot: None,
            completion: None,
            secret_admin: None,
            maintenance: None,
            composition: None,
            self_ip_force: None,
            read_after: 0,
            read_until: None,
            word_mode: "",
            expected_room: "",
            tic_version: None,
            game_id: 0,
            kind: if register { "register" } else { "login" },
            request_id: &request_id,
            username: &self.username,
            password: &self.password,
            new_password: "",
            confirm: &self.confirm,
            invite: &self.invite,
            room: "",
            body: "",
            version: 0,
            backup_id: "",
            position: None,
            before: 0,
            reply_to: 0,
        };
        if let Err(error) = self.send_request(&request) {
            self.login_gate.finish(&request_id);
            self.clear_login_secrets();
            self.error = Some(format!("Request failed: {error:#}"));
        } else {
            self.status = "Authenticating…".into();
        }
        cx.notify();
    }

    fn select_room(&mut self, room: String, cx: &mut Context<Self>) {
        self.cancel_completion();
        if room != self.room && self.composition.is_some() {
            if self
                .composition
                .as_ref()
                .is_some_and(|e| e.owner == self.user && e.frozen && e.room == room)
            {
                self.select_room_now(room, cx);
                return;
            }
            self.close_composition(composition_ui::CompositionAfter::Room(room), cx);
            return;
        }
        self.begin_feature_navigation();
        if self.persistence_enabled && room != self.room {
            self.observe_persistence();
            if self.persistence.conflicted() {
                self.error =
                    Some("请先解决当前草稿冲突，再切换对话；输入不会发往其它房间。".into());
                cx.notify();
                return;
            }
            if !self.persistence.ready() || self.persistence.pending() || self.persistence.dirty() {
                self.queued_room = Some(room);
                cx.notify();
                return;
            }
        }
        self.select_room_now(room, cx);
    }
    fn select_room_now(&mut self, room: String, cx: &mut Context<Self>) {
        if self.pending_draft.is_some()
            || self.pending_room.is_some()
            || self.pending_send.is_some()
            || room == self.room
        {
            return;
        }
        let request_id = self.next_request_id("room");
        let request = Request {
            attachment: None,
            dot: None,
            completion: None,
            secret_admin: None,
            maintenance: None,
            composition: None,
            self_ip_force: None,
            read_after: 0,
            read_until: None,
            word_mode: "",
            expected_room: "",
            tic_version: None,
            game_id: 0,
            kind: "select_room",
            request_id: &request_id,
            username: "",
            password: "",
            new_password: "",
            confirm: "",
            invite: "",
            room: &room,
            body: "",
            version: 0,
            backup_id: "",
            position: None,
            before: 0,
            reply_to: 0,
        };
        if self.send_request(&request).is_ok() {
            self.pending_room = Some(PendingRoom { request_id, room });
        } else {
            self.error = Some("Could not queue the room request.".into());
            cx.notify();
        }
    }

    fn send_message(&mut self, cx: &mut Context<Self>) {
        if self.custom_emoji_context_occupied() || self.dot_visible() || self.emoji_open() {
            return;
        }
        self.cancel_completion();
        if miochat_gpui::word::reserved_start(&self.composer) || self.persistence.quarantined() {
            self.error = Some(miochat_gpui::word::RESERVED_NOTICE.into());
            cx.notify();
            return;
        }
        if self.persistence_enabled {
            self.observe_persistence();
            if !self.persistence.ready() || self.persistence.pending() || self.queued_room.is_some()
            {
                return;
            }
            self.sync_draft_flags();
        }
        if self.screen != Screen::Chat
            || self.room.is_empty()
            || self.pending_room.is_some()
            || self.pending_send.is_some()
            || self.pending_draft.is_some()
            || self.composer.trim().is_empty()
        {
            return;
        }
        // Only this explicit ordinary-composer submission route records attempts.
        // Menu/form/programmatic commands do not pass through this hook.
        self.record_input_attempt();
        if self.composer.trim_start().starts_with('/') && !self.composer.starts_with("/fail") {
            let command = self.composer.clone();
            self.run_command(command, false, cx);
            return;
        }
        let request_id = self.next_request_id("send");
        let body = self.composer.clone();
        let request = Request {
            attachment: None,
            dot: None,
            completion: None,
            secret_admin: None,
            maintenance: None,
            composition: None,
            self_ip_force: None,
            read_after: 0,
            read_until: None,
            word_mode: "",
            expected_room: "",
            tic_version: None,
            game_id: 0,
            kind: "send_message",
            request_id: &request_id,
            username: "",
            password: "",
            new_password: "",
            confirm: "",
            invite: "",
            room: &self.room,
            body: &body,
            version: self.draft_version.unwrap_or(0),
            backup_id: "",
            position: None,
            before: 0,
            reply_to: self.reply_to,
        };
        if self.send_request(&request).is_ok() {
            self.pending_send = Some(PendingSend {
                edit_message: None,
                configuration: None,
                management: None,
                sanction: None,
                report: None,
                moderation: None,
                word: None,
                tic: None,
                guess: None,
                community: None,
                simple_action: None,
                information: false,
                one_off: false,
                one_off_target: String::new(),
                request_id,
                body,
                room: self.room.clone(),
            });
        } else {
            self.error = Some("Could not queue the message.".into());
            cx.notify();
        }
    }

    fn run_command(&mut self, command: String, preserve_draft: bool, cx: &mut Context<Self>) {
        self.cancel_completion();
        if self.activate_secret_admin(&command, preserve_draft, cx) {
            return;
        }
        if self.activate_maintenance(&command, preserve_draft, cx) {
            return;
        }
        if self.activate_composition(&command, preserve_draft, cx) {
            return;
        }
        if self.composition_visible() {
            self.error = Some("请先保存或处理长文编辑器，再执行其他聊天操作。".into());
            cx.notify();
            return;
        }
        let (command, read_bounds) =
            if command == "local:revisions-next" {
                let Some(next) = self.feature_panel.as_ref().and_then(|p| {
                    miochat_gpui::moderation::revision_next(p, &self.user, &self.room)
                }) else {
                    return;
                };
                next
            } else {
                (command, miochat_gpui::moderation::ReadBounds::default())
            };
        if command == "local:quarantine-review" {
            if self.screen != Screen::Chat {
                return;
            }
            self.begin_feature_navigation();
            if self.persistence.conflicted() {
                self.draft_request("draft_load", cx);
                return;
            }
            if self.persistence.quarantined()
                && let Some(version) = self.persistence.version()
            {
                self.quarantine_confirmation = Some((
                    self.user.clone(),
                    self.room.clone(),
                    self.generation,
                    version,
                ));

                self.set_feature_panel(Some(Panel {
                    title: "核对已保存的敏感草稿".into(),
                    scope: self.room.clone(),
                    lines: vec![
                        miochat_gpui::word::QUARANTINE_NOTICE.into(),
                        format!(
                            "当前账号 {} · 原会话 {} · 版本 {}。确认只清除此版本的当前服务器草稿；旧备份/其它副本不会删除。",
                            self.user, self.room, version
                        ),
                    ],
                    actions: vec![
                        PanelAction {
                            label: "确认清除此版本的服务器草稿".into(),
                            command: "local:quarantine-clear-confirm".into(),
                            fill: false,
                        },
                        PanelAction {
                            label: "重新读取，不清除".into(),
                            command: "/draft-reload".into(),
                            fill: false,
                        },
                    ],
                    ..Default::default()
                }));
            }
            cx.notify();
            return;
        }
        if command == "local:quarantine-clear-confirm" {
            self.begin_feature_navigation();
            if self.screen != Screen::Chat {
                return;
            }
            let captured = self.quarantine_confirmation.take();
            if let Some((owner, room, generation, version)) = captured
                && owner == self.user
                && room == self.room
                && generation == self.generation
            {
                if let Some(op) = self.persistence.clear_quarantined(version) {
                    if let Err(e) = self.queue_persistence(op) {
                        self.error = Some(format!("{e:#}"));
                    }
                } else {
                    self.error = Some("草稿状态已变化，未清除；请重新读取核对。".into());
                }
            }
            self.set_feature_panel(None);
            cx.notify();
            return;
        }

        let explicit_public_word = self.direct_form.as_ref().and_then(|f| {
            f.public_word_submission(&self.user, &self.room, self.generation, &command)
        });
        if miochat_gpui::word::reserved_start(&command) && explicit_public_word.is_none() {
            self.error = Some(miochat_gpui::word::RESERVED_NOTICE.into());
            cx.notify();
            return;
        }
        if self.persistence_enabled
            && matches!(
                command.trim(),
                "/draft-keep" | "/draft-reload" | "/draft-retry"
            )
        {
            if command.trim() == "/draft-retry" {
                if !self.persistence.pending() {
                    self.persistence.invalidate_active();
                }
                cx.notify();
                return;
            }
            self.draft_request(
                if command.trim() == "/draft-reload" {
                    "draft_load"
                } else {
                    "draft_keep"
                },
                cx,
            );
            return;
        }
        if command == "local:cancel-close" {
            self.begin_feature_navigation();
            self.close_started = None;
            self.set_feature_panel(None);
            cx.notify();
            return;
        }
        if command == "local:copy-close" {
            self.begin_feature_navigation();
            self.observe_persistence();
            cx.write_to_clipboard(gpui::ClipboardItem::new_string(
                self.persistence.recovery_text(),
            ));
            self.close_ready = true;
            if let Some(handle) = self.window_handle {
                let _ = handle.update(cx, |_, window, _| window.remove_window());
            }
            return;
        }
        if self.persistence_enabled {
            self.observe_persistence();
            if !self.persistence.ready() || self.persistence.pending() || self.queued_room.is_some()
            {
                return;
            }
        }
        if self.pending_draft.is_some()
            || self.pending_send.is_some()
            || self.pending_room.is_some()
        {
            return;
        }
        if !self.native_actions && !self.mock_mode {
            self.error = Some("服务器未提供原生功能协议，请使用配套候选服务器。".into());
            cx.notify();
            return;
        }
        let request_id = self.next_request_id("action");
        let mut request = Request::simple("command", &request_id);
        request.room = &self.room;
        request.body = &command;
        request.read_after = read_bounds.after;
        request.read_until = read_bounds.until;
        let management_intent = self
            .direct_form
            .as_ref()
            .filter(|f| {
                self.screen == Screen::Chat
                    && f.command(&self.user, &self.room, self.generation)
                        .ok()
                        .as_deref()
                        == Some(command.as_str())
            })
            .and_then(|f| f.management_intent())
            .and_then(|i| {
                miochat_gpui::management::ManagementIntent::parse(&command, &self.room)
                    .filter(|parsed| parsed.command == i.command && parsed.room == i.room)
            });
        if let Some(i) = &management_intent {
            request.expected_room = &i.room;
        }
        let sanction_intent = self.direct_form.as_ref().and_then(|f| {
            f.sanction_submission(
                self.screen == Screen::Chat,
                &self.user,
                &self.room,
                self.generation,
                &command,
            )
        });
        if let Some(i) = &sanction_intent {
            request.expected_room = &i.room;
            request.self_ip_force = i.self_ip_force;
        }
        let word_intent = explicit_public_word;
        if let Some(i) = &word_intent {
            request.word_mode = "guess";
            request.expected_room = &i.room;
            request.game_id = i.object_id;
        }
        let guess_intent = self
            .direct_form
            .as_ref()
            .and_then(|f| f.guess_intent())
            .and_then(|i| i.bind_submission(&command));
        if let Some(intent) = &guess_intent {
            request.expected_room = &intent.room;
            request.game_id = intent.object_id;
        }
        let tic_intent = self
            .direct_form
            .as_ref()
            .and_then(|f| f.tic_intent())
            .and_then(|i| i.bind_submission(&command));
        if let Some(intent) = &tic_intent {
            request.expected_room = &intent.room;
            request.tic_version = intent.revision;
        }
        let community_intent = self
            .direct_form
            .as_ref()
            .and_then(|f| f.community_intent())
            .filter(|intent| {
                miochat_gpui::community_mutation::CommunityIntent::from_command(&command)
                    .is_some_and(|parsed| {
                        parsed.action == intent.action
                            && parsed.object_id == intent.object_id
                            && parsed.choice == intent.choice
                    })
            })
            .cloned();
        if let Some(intent) = &community_intent {
            request.expected_room = &intent.room;
        }
        request.before = self
            .messages
            .iter()
            .map(|m| m.id)
            .filter(|id| *id > 0)
            .min()
            .unwrap_or(0);
        if let Err(error) = self.send_request(&request) {
            self.error = Some(format!("{error:#}"));
        } else {
            self.pending_send = Some(PendingSend::command(
                request_id,
                self.room.clone(),
                &command,
                if preserve_draft {
                    String::new()
                } else {
                    self.composer.clone()
                },
            ));
            if let Some(ticket) = self
                .pending_send
                .as_mut()
                .and_then(|p| p.moderation.as_mut())
            {
                miochat_gpui::moderation::invalidate(
                    &mut self.moderation_epoch,
                    &mut self.feature_panel,
                );
                ticket.bounds = read_bounds;
                ticket.bind(
                    &self.user,
                    &self.room,
                    self.generation,
                    self.moderation_epoch,
                );
            }
            if let Some(intent) = management_intent {
                self.pending_send.as_mut().unwrap().management = Some(intent);
            }
            if let Some(intent) = sanction_intent {
                self.pending_send.as_mut().unwrap().sanction = Some(intent);
            }
            if let Some(intent) = word_intent {
                self.pending_send.as_mut().unwrap().word = Some(intent);
            }
            if let Some(intent) = guess_intent {
                self.pending_send.as_mut().unwrap().guess = Some(intent);
            }
            if let Some(intent) = tic_intent {
                self.pending_send.as_mut().unwrap().tic = Some(intent);
            }
            if let Some(intent) = community_intent {
                self.pending_send.as_mut().unwrap().community = Some(intent);
            }
        }
        cx.notify();
    }
    fn show_features(&mut self, cx: &mut Context<Self>) {
        self.begin_feature_navigation();
        self.clear_profile();
        self.center_open = false;
        self.preferences_open = false;
        if self.mock_mode {
            self.set_feature_panel(Some(Panel {
                title: "功能 · 外观预览".into(),
                lines: vec![
                    "当前为内存外观预览。以下真实功能需要配套候选服务器；不会连接生产服务器。"
                        .into(),
                ],
                actions: vec![
                    PanelAction {
                        label: "私聊".into(),
                        command: "/dm ".into(),
                        fill: true,
                    },
                    PanelAction {
                        label: "搜索当前对话".into(),
                        command: "/search ".into(),
                        fill: true,
                    },
                    PanelAction {
                        label: "加载历史".into(),
                        command: "/history".into(),
                        fill: false,
                    },
                    PanelAction {
                        label: "收藏".into(),
                        command: "/bookmarks".into(),
                        fill: false,
                    },
                ],
                ..Panel::default()
            }));
            cx.notify();
        } else {
            self.run_command("/menu".into(), true, cx);
        }
    }
    fn apply_draft(&mut self, draft: Draft, replace: bool) {
        if !self.persistence_enabled && miochat_gpui::word::reserved_start(&draft.body) {
            self.error = Some(miochat_gpui::word::QUARANTINE_NOTICE.into());
            return;
        }

        if draft.room != self.room {
            return;
        }
        if self.persistence_enabled {
            let snapshot = Snapshot {
                body: draft.body,
                reply_to: draft.reply_to,
                position: draft.position,
            };
            if replace {
                self.persistence.restore_conflict(
                    draft.version,
                    snapshot.clone(),
                    draft.backup_id,
                    self.persistence_now(),
                );
                self.apply_snapshot(snapshot.clone());
                self.restore_position(snapshot.position);
            } else {
                self.persistence
                    .sent_baseline(&draft.room, draft.version, snapshot);
            }
            self.sync_draft_flags();
            return;
        }
        self.draft_version = Some(draft.version);
        self.draft_conflict = draft.conflict;
        self.draft_backup_id = draft.backup_id;
        if replace {
            if !self.composer.is_empty() && self.composer != draft.body {
                self.manual_draft_backups
                    .insert(self.room.clone(), self.composer.clone());
            }
            self.composer = draft.body;
            self.reply_to = draft.reply_to;
            self.reset_selection();
        }
    }
    fn draft_request(&mut self, kind: &str, cx: &mut Context<Self>) {
        if kind != "draft_load" && miochat_gpui::word::reserved_start(&self.composer) {
            self.error = Some(miochat_gpui::word::RESERVED_NOTICE.into());
            cx.notify();
            return;
        }

        if self.persistence_enabled {
            self.observe_persistence();
            if let Some(op) = self.persistence.manual(kind == "draft_load")
                && let Err(error) = self.queue_persistence(op)
            {
                self.error = Some(format!("{error:#}"));
            }
            cx.notify();
            return;
        }
        if self.pending_draft.is_some()
            || self.pending_send.is_some()
            || self.pending_room.is_some()
        {
            return;
        }
        if self.mock_mode {
            self.error = Some("当前为内存外观预览；草稿 CAS 在独立本地协议测试中验证。".into());
            cx.notify();
            return;
        }
        if kind != "draft_load" && self.draft_version.is_none() {
            self.error = Some("请先读取此对话草稿并确认版本，再保存。当前输入会保留备份。".into());
            cx.notify();
            return;
        }
        let request_id = self.next_request_id("draft");
        let mut request = Request::simple(kind, &request_id);
        request.room = &self.room;
        request.body = &self.composer;
        request.reply_to = self.reply_to;
        request.version = self.draft_version.unwrap_or(0);
        request.backup_id = &self.draft_backup_id;
        if let Err(error) = self.send_request(&request) {
            self.error = Some(format!("{error:#}"));
        } else {
            self.pending_draft = Some(PendingDraft {
                request_id,
                kind: kind.into(),
                body: self.composer.clone(),
                reply_to: self.reply_to,
            });
        }
        cx.notify();
    }
    fn change_password(&mut self, cx: &mut Context<Self>) {
        if self.pending_password.is_some() || self.screen != Screen::Password {
            return;
        }
        if self.confirm != self.invite {
            self.error = Some("新密码不一致".into());
            cx.notify();
            return;
        }
        let request_id = self.next_request_id("password");
        let mut request = Request::simple("change_password", &request_id);
        request.password = &self.password;
        request.new_password = &self.confirm;
        request.confirm = &self.invite;
        if let Err(error) = self.send_request(&request) {
            self.error = Some(format!("{error:#}"));
        } else {
            self.pending_password = Some(request_id);
            self.status = "正在更新密码…".into();
        }
        cx.notify();
    }
    fn cancel_password(&mut self, cx: &mut Context<Self>) {
        if self.pending_password.is_some() {
            return;
        }
        self.password.clear();
        self.confirm.clear();
        self.invite.clear();
        if self.must_reset {
            self.disconnect(cx);
        } else {
            self.set_screen(Screen::Chat);
            self.reset_selection();
            cx.notify();
        }
    }
    fn send_request(&self, request: &Request<'_>) -> anyhow::Result<()> {
        self.session
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("not connected"))?
            .send(request)
    }

    fn disconnect(&mut self, cx: &mut Context<Self>) {
        self.login_gate.clear();
        self.auto_connect_pending = false;
        self.attachment_ui.freeze(true);
        discard_word_input(&mut self.word_start, &mut self.field);
        self.direct_form = None;
        self.clear_center();
        self.clear_profile();
        self.account_preferences = Preferences::default();
        self.preferences_open = false;
        self.key_reminder_visible = false;
        self.pending_preferences = None;
        self.clear_key_secrets();
        self.observe_persistence();
        self.persistence.disconnected();
        self.pending_view = None;
        self.restoring_view = None;
        self.queued_room = None;
        self.close_started = None;
        if let Some(mut session) = self.session.take() {
            let request_id = self.next_request_id("disconnect");
            let _ = session.send(&Request::simple("disconnect", &request_id));
            session.disconnect();
        }
        self.generation += 1;
        self.password.clear();
        self.confirm.clear();
        self.invite.clear();
        self.pending_send = None;
        self.pending_room = None;
        self.set_feature_panel(None);
        self.conversation_drafts.clear();
        self.room_form = None;
        self.pending_room_form = None;
        self.room_target.clear();
        self.reply_to = 0;
        self.pending_password = None;
        self.pending_draft = None;
        self.draft_version = None;
        self.draft_conflict = false;
        self.draft_backup_id.clear();
        self.must_reset = false;
        self.connecting = false;
        self.set_screen(Screen::Connect);
        self.field = Field::Username;
        self.reset_selection();
        self.status = "Disconnected".into();
        cx.notify();
    }

    fn cancel_connect(&mut self, cx: &mut Context<Self>) {
        self.login_gate.clear();
        self.auto_connect_pending = false;
        self.clear_login_secrets();
        if let Some(mut session) = self.session.take() {
            session.disconnect();
        }
        self.attachment_ui.freeze(true);
        self.clear_key_secrets();
        self.generation += 1;
        self.connecting = false;
        self.host_key = None;
        self.set_screen(Screen::Connect);
        self.field = Field::Username;
        self.reset_selection();
        self.status = "Disconnected".into();
        cx.notify();
    }

    fn on_key_down(&mut self, event: &KeyDownEvent, window: &mut Window, cx: &mut Context<Self>) {
        if self.attachment_recovery_key(event, cx) {
            return;
        }
        if self.attachment_key(event, window, cx) {
            return;
        }
        if self.dot_key(event, window, cx) {
            return;
        }
        if self.emoji_key(event, window, cx) {
            return;
        }
        if self.completion_key(event, window, cx) {
            return;
        }
        // Any other key abandons this exact input revision before other routes.
        self.cancel_completion();
        if self.secret_admin_key(event, cx) {
            return;
        }
        if self.maintenance_key(event, cx) {
            return;
        }
        if self.composition_key(event, window, cx) {
            return;
        }
        if self.word_start.is_some() {
            if !self.word_input_active() {
                self.cancel_word_start(cx);
                cx.stop_propagation();
                return;
            }
            match event.keystroke.key.as_str() {
                "escape" => {
                    self.cancel_word_start(cx);
                    cx.stop_propagation();
                    return;
                }
                "tab" => {
                    cx.stop_propagation();
                    return;
                }
                "enter" if self.marked_range.is_none() => {
                    self.submit_word_start(cx);
                    cx.stop_propagation();
                    return;
                }
                _ => {}
            }
        }

        self.selection = editing::valid_selection(self.current_value(), self.selection.clone());
        if self.screen == Screen::Chat && self.direct_form.is_some() {
            if !self.direct_input_active()
                || self
                    .direct_form
                    .as_ref()
                    .is_some_and(|f| !f.bound_to(&self.user, &self.room, self.generation))
            {
                // An ACL fallback freezes the retained one-off input. Never
                // route arrows/backspace/Enter to another scope's composer.
                let command =
                    event.keystroke.modifiers.control || event.keystroke.modifiers.platform;
                match event.keystroke.key.as_str() {
                    "escape" => self.cancel_direct_form(cx),
                    "a" if command => self.selection = 0..self.current_value().len(),
                    "c" if command && !self.secret_field() => {
                        if let Some(text) = self.current_value().get(self.selection.clone()) {
                            cx.write_to_clipboard(gpui::ClipboardItem::new_string(
                                text.to_string(),
                            ));
                        }
                    }
                    _ => {}
                }
                cx.stop_propagation();
                cx.notify();
                return;
            }
            let active = self.direct_input_active();
            if sanctions_ui::handle_force_key(
                event,
                active,
                self.marked_range.is_some(),
                self.direct_form.as_mut(),
                (&self.user, &self.room, self.generation),
            ) {
                cx.stop_propagation();
                cx.notify();
                return;
            }
            match event.keystroke.key.as_str() {
                "escape" => self.cancel_direct_form(cx),
                "tab" if self.marked_range.is_none() => {
                    let form = self.direct_form.as_ref().unwrap();
                    self.field = direct_field(form.next_slot(
                        direct_slot(self.field).unwrap(),
                        event.keystroke.modifiers.shift,
                    ));
                    self.reset_selection();
                }
                "enter" if self.marked_range.is_none() && !event.keystroke.modifiers.shift => {
                    self.submit_direct_form(cx)
                }
                _ => {}
            }
            if matches!(event.keystroke.key.as_str(), "escape" | "tab")
                || event.keystroke.key == "enter" && !event.keystroke.modifiers.shift
                || self
                    .direct_form
                    .as_ref()
                    .is_some_and(|f| direct_slot(self.field).is_none_or(|slot| f.limit(slot) == 0))
            {
                cx.stop_propagation();
                cx.notify();
                return;
            }
        }
        if self.profile.is_some() {
            if event.keystroke.key == "escape" {
                if self.profile_editor.is_some() {
                    self.profile_editor = None;
                    self.profile_text.clear();
                    self.reset_selection();
                } else {
                    self.clear_profile();
                }
                cx.stop_propagation();
                cx.notify();
                return;
            }
            if self.profile_editor.is_none() {
                cx.stop_propagation();
                return;
            }
            if event.keystroke.key == "enter" && self.marked_range.is_none() {
                self.submit_profile_edit(cx);
                cx.stop_propagation();
                return;
            }
        }
        if self.center_open {
            let actions = self.center.actions(&self.center_section);
            let count = actions.len();
            match event.keystroke.key.as_str() {
                "escape" | "tab" => {
                    self.center_open = false;
                    self.reset_selection();
                    self.focus.focus(window, cx);
                }
                "down" if count > 0 => {
                    self.center_cursor = Some(self.center_cursor.map_or(0, |i| (i + 1) % count))
                }
                "up" if count > 0 => {
                    self.center_cursor =
                        Some(self.center_cursor.map_or(0, |i| (i + count - 1) % count))
                }
                "enter" | "space" => {
                    if let Some(command) = self.center_cursor.and_then(|i| actions.get(i)) {
                        self.activate_center_command(command.clone(), cx);
                    }
                }
                _ => {}
            }
            cx.stop_propagation();
            cx.notify();
            return;
        }
        if self.key_reminder_active() {
            match event.keystroke.key.as_str() {
                "escape" => self.key_reminder_visible = false,
                "up" => self.key_reminder_cursor = (self.key_reminder_cursor + 2) % 3,
                "down" | "tab" => self.key_reminder_cursor = (self.key_reminder_cursor + 1) % 3,
                "enter" | "space" => self.activate_key_reminder(self.key_reminder_cursor, cx),
                _ => {}
            }
            cx.stop_propagation();
            cx.notify();
            return;
        }
        if self.preferences_open {
            match event.keystroke.key.as_str() {
                "escape" | "tab" => {
                    self.preferences_open = false;
                    self.reset_selection();
                    self.focus.focus(window, cx);
                }
                "up" => self.preferences_cursor = (self.preferences_cursor + 14) % 15,
                "down" => self.preferences_cursor = (self.preferences_cursor + 1) % 15,
                "enter" | "space" => self.activate_preference_cursor(cx),
                _ => {}
            }
            cx.stop_propagation();
            cx.notify();
            return;
        }
        if let Some(form) = self.room_form.as_ref() {
            match event.keystroke.key.as_str() {
                "escape" => self.cancel_room_form(cx),
                "enter" if self.marked_range.is_none() => self.submit_room_form(cx),
                _ if form.action.needs_input() && form.room == self.room => {}
                _ => {
                    cx.stop_propagation();
                    return;
                }
            }
            if matches!(event.keystroke.key.as_str(), "escape" | "enter") {
                cx.stop_propagation();
                return;
            }
        }
        if self.screen == Screen::KeyBinding && event.keystroke.key == "escape" {
            self.cancel_key_binding(cx);
            return;
        }
        if self.screen == Screen::Password && event.keystroke.key == "escape" {
            self.cancel_password(cx);
            cx.stop_propagation();
            return;
        }
        if self.feature_key(event, window, cx) {
            return;
        }
        if !matches!(
            self.screen,
            Screen::Chat | Screen::Auth | Screen::Password | Screen::KeyBinding | Screen::Connect
        ) {
            return;
        }
        let key = event.keystroke.key.as_str();
        if self.message_focus.is_focused(window) && self.message_selection.is_some() {
            if let Some((id, range)) = self.message_selection.as_mut()
                && let Some(message) = self.messages.iter().find(|m| m.id == *id)
            {
                if event.keystroke.modifiers.control && key == "a" {
                    *range = 0..message.body.len();
                }
                if event.keystroke.modifiers.control
                    && key == "c"
                    && let Some(text) = message.body.get(range.clone())
                {
                    cx.write_to_clipboard(gpui::ClipboardItem::new_string(text.to_string()));
                }
            }
            if key == "escape" || key == "tab" {
                self.message_selection = None;
                self.focus.focus(window, cx);
            }
            cx.notify();
            cx.stop_propagation();
            return;
        }

        if event.keystroke.modifiers.control && event.keystroke.modifiers.shift && key == "a" {
            self.toggle_appearance(window, cx);
            cx.stop_propagation();
            return;
        }
        if self.appearance_menu {
            match key {
                "up" => self.theme_cursor = (self.theme_cursor + 2) % 3,
                "down" => self.theme_cursor = (self.theme_cursor + 1) % 3,
                "enter" | "space" => self.set_appearance(
                    [
                        Appearance::Keel,
                        Appearance::Frosted,
                        Appearance::Transparent,
                    ][self.theme_cursor],
                    window,
                    cx,
                ),
                "escape" | "tab" => {
                    self.appearance_menu = false;
                    self.focus.focus(window, cx);
                }
                _ => {}
            }
            cx.notify();
            cx.stop_propagation();
            return;
        }
        if self.mock_menu && key == "escape" {
            self.mock_menu = false;
            cx.notify();
            cx.stop_propagation();
            return;
        }

        if self.reading_key(event, cx) {
            return;
        }
        if self.affordance_key(event, cx) {
            return;
        }
        if self.history_key(event, window, cx) {
            return;
        }
        let command = event.keystroke.modifiers.control || event.keystroke.modifiers.platform;
        if self.marked_range.is_some()
            && !command
            && matches!(key, "backspace" | "delete" | "enter" | "left" | "right")
        {
            return;
        }
        if command && key == "v" {
            if let Some(text) = cx.read_from_clipboard().and_then(|item| item.text()) {
                self.replace_current(None, &text);
            }
        } else if command && key == "a" {
            self.selection = 0..self.current_value().len();
        } else if command && matches!(key, "c" | "x") {
            let secret =
                self.secret_field() || miochat_gpui::word::reserved_start(self.current_value());
            if !secret && !self.selection.is_empty() {
                cx.write_to_clipboard(gpui::ClipboardItem::new_string(
                    self.current_value()[self.selection.clone()].to_string(),
                ));
                if key == "x" {
                    self.replace_current(None, "");
                }
            }
        } else {
            match key {
                "tab" if self.screen != Screen::Chat => {
                    self.advance_field(event.keystroke.modifiers.shift)
                }
                "enter"
                    if matches!(self.screen, Screen::Connect | Screen::Auth)
                        && (event.is_held
                            || event.prefer_character_input
                            || self.marked_range.is_some()) => {}
                "enter" if self.screen == Screen::Connect => self.begin_connect(cx),
                "enter" if self.screen == Screen::Auth => self.authenticate(self.registering, cx),
                "enter" if self.screen == Screen::Password => self.change_password(cx),
                "enter" if self.screen == Screen::KeyBinding => self.bind_device_key(cx),
                "enter" if event.keystroke.modifiers.shift => {
                    self.replace_current(None, "\n");
                }
                "enter"
                    if self.screen == Screen::Chat
                        && !completion_ui::ordinary_chat_enter_is_press(event) => {}
                "enter" => self.send_message(cx),
                "backspace" => {
                    let old_selection = self.selection.clone();
                    if self.selection.is_empty() {
                        self.selection.start =
                            editing::previous(self.current_value(), self.selection.start);
                    }
                    if !self.replace_current(None, "") {
                        self.selection = old_selection;
                    }
                }
                "delete" => {
                    let old_selection = self.selection.clone();
                    if self.selection.is_empty() {
                        self.selection.end =
                            editing::next(self.current_value(), self.selection.end);
                    }
                    if !self.replace_current(None, "") {
                        self.selection = old_selection;
                    }
                }
                "left" | "right" | "home" | "end" => {
                    let position = match key {
                        "left" => editing::previous(self.current_value(), self.selection.start),
                        "right" => editing::next(self.current_value(), self.selection.end),
                        "home" => 0,
                        _ => self.current_value().len(),
                    };
                    if event.keystroke.modifiers.shift {
                        self.selection =
                            self.selection.start.min(position)..self.selection.end.max(position);
                    } else {
                        self.selection = position..position;
                    }
                }
                _ => return,
            }
        }
        cx.notify();
        cx.stop_propagation();
    }

    fn current_value(&self) -> &str {
        if !self.connection_field_enabled(self.field) {
            return "";
        }
        if self.secret_admin_visible() || self.maintenance_visible() {
            return "";
        }
        if self.composition_visible() {
            return composition_ui::composition_slot(self.field)
                .map_or("", |slot| self.composition.as_ref().unwrap().value(slot));
        }
        if self.word_input_active() {
            return self.word_start.as_ref().unwrap().value();
        }
        if let Some(form) = self
            .direct_form
            .as_ref()
            .filter(|_| self.direct_input_active())
        {
            return form.value(direct_slot(self.field).unwrap());
        }
        if self.profile_editor.is_some() {
            return &self.profile_text;
        }
        if self.room_form.is_some() {
            return &self.room_target;
        }
        if self.screen == Screen::Chat {
            return &self.composer;
        }
        match self.field {
            Field::Host => &self.spec.host,
            Field::Port => &self.port_text,
            Field::SshUser => &self.spec.ssh_user,
            Field::Username => &self.username,
            Field::Password => &self.password,
            Field::Confirm => &self.confirm,
            Field::Invite => &self.invite,
            Field::Identity => &self.identity_file,
            Field::KeyPassphrase => &self.identity_passphrase,
            Field::KeyLabel => &self.key_label,
            Field::RoomTarget => &self.room_target,
            Field::ProfileText => &self.profile_text,
            Field::DirectTarget
            | Field::DirectBody
            | Field::DirectExtra
            | Field::WordSecret
            | Field::CompositionTitle
            | Field::CompositionBody => "",
        }
    }
    fn reset_selection(&mut self) {
        let end = self.current_value().len();
        self.selection = end..end;
        self.marked_range = None;
    }
    fn replace_current(&mut self, range: Option<Range<usize>>, text: &str) -> bool {
        if !self.connection_field_enabled(self.field) {
            return false;
        }
        self.cancel_completion();
        if self.custom_emoji_context_occupied() || self.dot_visible() || self.emoji_open() {
            return false;
        }
        if self.feature_panel.is_some() {
            return false;
        }
        if self.secret_admin_visible() || self.maintenance_visible() {
            return false;
        }
        if self.composition_visible() {
            if !self.composition_input_active() {
                return false;
            }
            let slot = composition_ui::composition_slot(self.field).unwrap();
            let mut value = self.current_value().to_string();
            let mut selection = self.selection.clone();
            let mut marked = self.marked_range.clone();
            if !editing::replace(&mut value, &mut selection, &mut marked, range, text, 32768) {
                return false;
            }
            let now = self.persistence_now();
            if !self.composition.as_mut().unwrap().edit(slot, value, now) {
                return false;
            }
            self.composition_after = None;
            self.composition_reveal = true;
            self.composition_cursor = None;
            self.selection = selection;
            self.marked_range = marked;
            return true;
        }
        let word_input = self.word_input_active();
        if self.word_start.is_some() && !word_input {
            return false;
        }
        let direct_input = self.direct_input_active();
        if self.screen == Screen::Chat
            && self.direct_form.as_ref().is_some_and(|f| {
                !direct_input
                    || f.secret_admin_kind().is_some() && f.pending()
                    || !f.bound_to(&self.user, &self.room, self.generation)
                    || direct_slot(self.field).is_none_or(|slot| f.limit(slot) == 0)
            })
        {
            return false;
        }
        if self.profile.is_some() && self.profile_editor.is_none() {
            return false;
        }
        if self.center_open {
            return false;
        }
        if self.key_reminder_active() {
            return false;
        }
        if self.preferences_open {
            return false;
        }
        if let Some(form) = self.room_form.as_ref()
            && (!form.action.needs_input() || form.room != self.room)
        {
            return false;
        }
        if self.screen == Screen::Chat
            && self.persistence_enabled
            && (!self.persistence.ready()
                || self.queued_room.is_some()
                || self.close_started.is_some()
                || self.persistence.quarantined()
                    && !word_input
                    && !direct_input
                    && self.profile_editor.is_none()
                    && self.room_form.is_none())
        {
            return false;
        }
        if self.screen == Screen::Chat
            && !word_input
            && !direct_input
            && self.profile_editor.is_none()
            && self.room_form.is_none()
        {
            let accepted = miochat_gpui::word::replace_composer(
                &mut self.composer,
                &mut self.selection,
                &mut self.marked_range,
                range,
                text,
            );
            if !accepted {
                self.error = Some(miochat_gpui::word::RESERVED_NOTICE.into());
            }
            return accepted;
        }
        let mut value = self.current_value().to_string();
        let mut selection = self.selection.clone();
        let mut marked = self.marked_range.clone();
        if !editing::replace(
            &mut value,
            &mut selection,
            &mut marked,
            range,
            text,
            if word_input {
                24
            } else if direct_input {
                self.direct_form
                    .as_ref()
                    .unwrap()
                    .limit(direct_slot(self.field).unwrap())
            } else if let Some(e) = self.profile_editor.as_ref() {
                if e.kind == EditKind::Note { 100 } else { 160 }
            } else if self.screen == Screen::Chat {
                1600
            } else if matches!(self.field, Field::Identity | Field::KeyPassphrase) {
                4096
            } else {
                256
            },
        ) {
            return false;
        }
        if word_input {
            self.word_start.as_mut().unwrap().replace(value);
        } else if direct_input {
            let form = self.direct_form.as_mut().unwrap();
            *form.value_mut(direct_slot(self.field).unwrap()) = value;
        } else if self.profile_editor.is_some() {
            self.profile_text = value;
        } else if self.room_form.is_some() {
            self.room_target = value;
        } else if self.screen == Screen::Chat {
            if miochat_gpui::word::reserved_start(&value) {
                self.error = Some(miochat_gpui::word::RESERVED_NOTICE.into());
                return false;
            }
            self.composer = value;
        } else {
            *self.active_value_mut() = value;
        }
        self.selection = selection;
        self.marked_range = marked;
        true
    }

    fn active_value_mut(&mut self) -> &mut String {
        match self.field {
            Field::Host => &mut self.spec.host,
            Field::Port => &mut self.port_text,
            Field::SshUser => &mut self.spec.ssh_user,
            Field::Username => &mut self.username,
            Field::Password => &mut self.password,
            Field::Confirm => &mut self.confirm,
            Field::Invite => &mut self.invite,
            Field::Identity => &mut self.identity_file,
            Field::KeyPassphrase => &mut self.identity_passphrase,
            Field::KeyLabel => &mut self.key_label,
            Field::RoomTarget => &mut self.room_target,
            Field::ProfileText => &mut self.profile_text,
            // These fields are handled through the separate form above.
            Field::DirectTarget
            | Field::DirectBody
            | Field::DirectExtra
            | Field::WordSecret
            | Field::CompositionTitle
            | Field::CompositionBody => {
                unreachable!("direct-message form field without form")
            }
        }
    }

    fn advance_field(&mut self, reverse: bool) {
        let fields: &[Field] = match self.screen {
            Screen::Connect if !self.advanced_connection => &[Field::Username, Field::Password],
            Screen::Connect => &[
                Field::Host,
                Field::Port,
                Field::SshUser,
                Field::Identity,
                Field::KeyPassphrase,
            ],
            Screen::KeyBinding => &[Field::KeyLabel, Field::Password],
            Screen::Password => &[Field::Password, Field::Confirm, Field::Invite],
            Screen::Auth if self.mock_mode => &[Field::Username],
            _ if self.registering && self.registration_mode == "invite" => &[
                Field::Username,
                Field::Password,
                Field::Confirm,
                Field::Invite,
            ],
            _ if self.registering => &[Field::Username, Field::Password, Field::Confirm],
            _ => &[Field::Username, Field::Password],
        };
        let index = fields
            .iter()
            .position(|field| *field == self.field)
            .unwrap_or(0);
        self.field = fields[if reverse {
            (index + fields.len() - 1) % fields.len()
        } else {
            (index + 1) % fields.len()
        }];
        self.reset_selection();
    }

    fn canvas_color(&self) -> gpui::Hsla {
        if self.account_preferences.on("contrast") {
            return rgb(0xffffff).into();
        }
        self.appearance.canvas()
    }
    fn surface_color(&self) -> gpui::Hsla {
        if self.account_preferences.on("contrast") {
            return rgb(0xffffff).into();
        }
        self.appearance.surface()
    }
    fn raised_color(&self) -> gpui::Hsla {
        if self.account_preferences.on("contrast") {
            return rgb(0xffffff).into();
        }
        if self.appearance == Appearance::Keel {
            rgb(0xfdfcf9).into()
        } else {
            gpui::rgba(0xfdfcf9f2).into()
        }
    }
    fn toggle_appearance(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.composition_blocks_navigation(cx) {
            return;
        }
        self.begin_feature_navigation();
        self.clear_profile();
        self.center_open = false;
        self.preferences_open = false;
        self.set_feature_panel(None);
        self.appearance_menu = !self.appearance_menu;
        self.mock_menu = false;
        self.theme_cursor = match self.appearance {
            Appearance::Keel => 0,
            Appearance::Frosted => 1,
            Appearance::Transparent => 2,
        };
        if self.appearance_menu {
            self.menu_focus.focus(window, cx);
        } else {
            self.focus.focus(window, cx);
        }
        cx.notify();
    }
    fn set_appearance(&mut self, mode: Appearance, window: &mut Window, cx: &mut Context<Self>) {
        match mode.apply(window) {
            Ok(()) => {
                self.appearance = mode;
                self.material_status = mode.label().into();
            }
            Err(reason) => {
                self.appearance = Appearance::Keel;
                self.material_status = reason;
            }
        }
        self.appearance_menu = false;
        self.focus.focus(window, cx);
        cx.notify();
    }
    fn mock_request(&mut self, kind: &str, cx: &mut Context<Self>) {
        if !self.mock_mode {
            return;
        }
        let id = self.next_request_id("mock");
        if let Err(error) = self.send_request(&Request::simple(kind, &id)) {
            self.error = Some(error.to_string());
        }
        self.mock_menu = false;
        cx.notify();
    }
    fn toolbar(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let title = if self.screen == Screen::Chat {
            self.room
                .strip_prefix("dm:")
                .map(|peer| format!("@ {peer}"))
                .unwrap_or_else(|| format!("# {}", self.room))
        } else {
            match self.screen {
                Screen::Connect => "连接".into(),
                Screen::Trust => "验证服务器".into(),
                _ => "欢迎使用".into(),
            }
        };
        div()
            .h(px(44.))
            .flex_none()
            .flex()
            .items_center()
            .border_b_1()
            .border_color(self.preference_border())
            .bg(self.surface_color())
            .when(self.screen == Screen::Chat, |d| {
                d.child(
                    Self::button(
                        "toggle-sidebar",
                        if self.sidebar_visible {
                            "隐藏侧栏 · Ctrl+B"
                        } else {
                            "显示侧栏 · Ctrl+B"
                        },
                    )
                    .ml(px(8.))
                    .flex_none()
                    .on_click(cx.listener(|this, _, _, cx| this.toggle_sidebar(cx))),
                )
            })
            .child(
                div()
                    .pl(px(24.))
                    .pr(px(18.))
                    .text_size(px(13.))
                    .font_weight(gpui::FontWeight::MEDIUM)
                    .child(title),
            )
            .child(
                div()
                    .flex_1()
                    .h_full()
                    .window_control_area(gpui::WindowControlArea::Drag),
            )
            .when(
                self.screen == Screen::Chat && self.maintenance.is_some(),
                |d| {
                    d.child(
                        Self::button("maintenance-badge", "后台作业")
                            .mr(px(10.))
                            .on_click(cx.listener(|this, _, _, cx| this.open_maintenance_view(cx))),
                    )
                },
            )
            .when(self.screen == Screen::Chat, |d| {
                d.child(
                    Self::button("features", "功能")
                        .mr(px(10.))
                        .on_click(cx.listener(|this, _, _, cx| this.show_features(cx))),
                )
            })
            .child(
                div()
                    .id("appearance-button")
                    .role(gpui::Role::Button)
                    .aria_label("外观")
                    .h(px(28.))
                    .px(px(10.))
                    .mr(px(10.))
                    .flex()
                    .items_center()
                    .gap(px(8.))
                    .rounded(px(6.))
                    .cursor_pointer()
                    .hover(|s| s.bg(rgb(0xe9e6df)))
                    .child("◐")
                    .on_click(cx.listener(|this, _, window, cx| {
                        cx.stop_propagation();
                        this.toggle_appearance(window, cx);
                    })),
            )
            .when(self.screen == Screen::Chat && self.center_enabled, |d| {
                d.child(
                    Self::button("open-control-center", format!("◎ {}", self.center.unread))
                        .mr(px(10.))
                        .aria_label("控制中心")
                        .on_click(
                            cx.listener(|this, _, _, cx| this.open_center("notifications", cx)),
                        ),
                )
            })
            .when(self.screen == Screen::Chat && self.profile_enabled, |d| {
                d.child(
                    Self::button("own-profile", "◉")
                        .mr(px(8.))
                        .aria_label("我的资料")
                        .on_click(
                            cx.listener(|this, _, _, cx| this.open_profile(this.user.clone(), cx)),
                        ),
                )
            })
            .child(self.caption("minimize", "最小化", "−", gpui::WindowControlArea::Min, cx))
            .child(self.caption(
                "maximize",
                "最大化或还原",
                "□",
                gpui::WindowControlArea::Max,
                cx,
            ))
            .child(self.caption("close", "关闭", "×", gpui::WindowControlArea::Close, cx))
    }
    fn caption(
        &self,
        id: &'static str,
        label: &'static str,
        face: &'static str,
        area: gpui::WindowControlArea,
        cx: &mut Context<Self>,
    ) -> impl IntoElement {
        div()
            .id(id)
            .role(gpui::Role::Button)
            .aria_label(label)
            .window_control_area(area)
            .w(px(44.))
            .h(px(44.))
            .flex()
            .items_center()
            .justify_center()
            .text_size(px(18.))
            .cursor_pointer()
            .hover(move |s| s.bg(rgb(if id == "close" { 0xf1d8d4 } else { 0xe9e6df })))
            .on_click(cx.listener(move |this, _, window, cx| match area {
                gpui::WindowControlArea::Min => window.minimize_window(),
                gpui::WindowControlArea::Max => window.zoom_window(),
                gpui::WindowControlArea::Close if this.request_close(cx) => window.remove_window(),
                _ => {}
            }))
            .on_a11y_action(gpui::AccessibleAction::Click, {
                let entity = cx.entity();
                move |_, window, cx| match area {
                    gpui::WindowControlArea::Min => window.minimize_window(),
                    gpui::WindowControlArea::Max => window.zoom_window(),
                    gpui::WindowControlArea::Close
                        if entity.update(cx, |this, cx| this.request_close(cx)) =>
                    {
                        window.remove_window()
                    }
                    _ => {}
                }
            })
            .child(face)
    }
    fn sidebar(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let visible_rooms = if self.screen == Screen::Chat {
            self.rooms.clone()
        } else {
            Vec::new()
        };
        div()
            .w(px(232.))
            .flex_none()
            .h_full()
            .flex()
            .flex_col()
            .bg(self.canvas_color())
            .border_r_1()
            .border_color(self.preference_border())
            .when(self.screen == Screen::Chat && self.native_actions, |d| {
                d.child(
                    div()
                        .px(px(12.))
                        .py(px(8.))
                        .flex()
                        .gap(px(6.))
                        .child(Self::button("open-directory", "房间").on_click(cx.listener(
                            |this, _, _, cx| this.run_command("/rooms".into(), true, cx),
                        )))
                        .when(!self.room.starts_with("dm:"), |d| {
                            d.child(Self::button("open-members", "成员").on_click(cx.listener(
                                |this, _, _, cx| this.run_command("/members".into(), true, cx),
                            )))
                        }),
                )
            })
            .child(
                div()
                    .h(px(44.))
                    .px(px(20.))
                    .flex()
                    .items_center()
                    .window_control_area(gpui::WindowControlArea::Drag)
                    .child(
                        div()
                            .text_size(px(15.))
                            .font_weight(gpui::FontWeight::SEMIBOLD)
                            .child("MioChat"),
                    ),
            )
            .child(
                div()
                    .px(px(20.))
                    .pt(px(26.))
                    .pb(px(10.))
                    .text_size(px(11.))
                    .text_color(rgb(MUTED))
                    .child("聊天室"),
            )
            .child(
                div()
                    .px(px(10.))
                    .flex_1()
                    .flex()
                    .flex_col()
                    .gap(px(3.))
                    .children(visible_rooms.into_iter().map(|room| {
                        let selected = room.name == self.room;
                        let name = room.name.clone();
                        let unread = self.notifications.unread.get(&name).copied().unwrap_or(0);
                        div()
                            .id(gpui::SharedString::from(format!("room-{name}")))
                            .role(gpui::Role::Button)
                            .aria_label(format!("房间 {}", name))
                            .h(px(36.))
                            .px(px(12.))
                            .rounded(px(7.))
                            .flex()
                            .items_center()
                            .justify_between()
                            .cursor_pointer()
                            .bg(if selected {
                                self.raised_color()
                            } else {
                                gpui::transparent_black()
                            })
                            .hover(|s| s.bg(rgb(0xe4e0d8)))
                            .child(
                                div()
                                    .flex()
                                    .gap(px(10.))
                                    .items_center()
                                    .child(div().text_color(rgb(MUTED)).child("#"))
                                    .child(room.name),
                            )
                            .when(unread > 0, |d| {
                                d.child(
                                    div()
                                        .text_size(px(11.))
                                        .text_color(rgb(ACCENT))
                                        .child(unread.to_string()),
                                )
                            })
                            .on_click(
                                cx.listener(move |this, _, _, cx| {
                                    this.select_room(name.clone(), cx)
                                }),
                            )
                    }))
                    .when(self.screen != Screen::Chat || self.rooms.is_empty(), |d| {
                        d.child(
                            div()
                                .px(px(10.))
                                .py(px(8.))
                                .text_size(px(12.))
                                .text_color(rgb(MUTED))
                                .child("连接后显示可访问的房间"),
                        )
                    }),
            )
            .child(
                div()
                    .px(px(18.))
                    .py(px(16.))
                    .flex()
                    .flex_col()
                    .gap(px(8.))
                    .child(
                        div()
                            .text_size(px(12.))
                            .font_weight(gpui::FontWeight::MEDIUM)
                            .child(if self.screen != Screen::Chat || self.user.is_empty() {
                                "尚未登录".to_string()
                            } else {
                                self.user.clone()
                            }),
                    )
                    .child(div().text_size(px(11.)).text_color(rgb(MUTED)).child(
                        if self.fixture_driver.is_some() {
                            "本地合成联调 · 无网络".to_string()
                        } else if self.mock_mode {
                            "本地预览 · 无网络连接".to_string()
                        } else {
                            self.status.clone()
                        },
                    ))
                    .when(self.mock_mode, |d| {
                        d.child(
                            div()
                                .id("demo-tools")
                                .role(gpui::Role::Button)
                                .aria_label("演示工具")
                                .text_size(px(12.))
                                .text_color(rgb(MUTED))
                                .cursor_pointer()
                                .child("演示工具  ···")
                                .on_click(cx.listener(|this, _, _, cx| {
                                    cx.stop_propagation();
                                    this.mock_menu = !this.mock_menu;
                                    this.appearance_menu = false;
                                    cx.notify();
                                })),
                        )
                    })
                    .when(self.screen != Screen::Connect, |d| {
                        d.child(
                            div()
                                .id("disconnect")
                                .role(gpui::Role::Button)
                                .aria_label("断开连接")
                                .text_size(px(12.))
                                .text_color(rgb(MUTED))
                                .cursor_pointer()
                                .child("断开连接")
                                .on_click(cx.listener(|this, _, _, cx| this.disconnect(cx))),
                        )
                    }),
            )
    }
    fn appearance_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .id("appearance-menu")
            .role(gpui::Role::Menu)
            .aria_label("选择外观")
            .track_focus(&self.menu_focus)
            .absolute()
            .top(px(42.))
            .right(px(148.))
            .w(px(224.))
            .p(px(6.))
            .rounded(px(10.))
            .border_1()
            .border_color(self.preference_border())
            .bg(rgb(0xfdfcf9))
            .shadow_md()
            .occlude()
            .on_click(|_, _, cx| cx.stop_propagation())
            .child(
                div()
                    .px(px(10.))
                    .py(px(8.))
                    .text_size(px(11.))
                    .text_color(rgb(MUTED))
                    .child("外观"),
            )
            .children(
                [
                    Appearance::Keel,
                    Appearance::Frosted,
                    Appearance::Transparent,
                ]
                .into_iter()
                .enumerate()
                .map(|(index, mode)| {
                    div()
                        .id(gpui::SharedString::from(format!("theme-{mode:?}")))
                        .role(gpui::Role::MenuItem)
                        .aria_label(mode.label())
                        .h(px(36.))
                        .px(px(10.))
                        .rounded(px(6.))
                        .flex()
                        .items_center()
                        .justify_between()
                        .cursor_pointer()
                        .bg(if self.theme_cursor == index {
                            rgb(0xeeeae3).into()
                        } else {
                            gpui::transparent_black()
                        })
                        .hover(|s| s.bg(rgb(0xeeeae3)))
                        .child(mode.label())
                        .child(if self.appearance == mode { "✓" } else { "" })
                        .on_click(cx.listener(move |this, _, window, cx| {
                            cx.stop_propagation();
                            this.set_appearance(mode, window, cx);
                        }))
                }),
            )
            .child(
                div()
                    .mt(px(6.))
                    .pt(px(10.))
                    .pb(px(5.))
                    .px(px(10.))
                    .border_t_1()
                    .border_color(self.preference_border())
                    .text_size(px(11.))
                    .text_color(rgb(MUTED))
                    .child("消息通知"),
            )
            .when(
                self.screen == Screen::Chat && self.preferences_enabled,
                |d| {
                    d.child(
                        Self::button("open-preferences", "账户偏好")
                            .on_click(cx.listener(|this, _, _, cx| this.show_preferences(cx))),
                    )
                },
            )
            .child(
                div()
                    .id("preview-toggle")
                    .role(gpui::Role::Button)
                    .aria_label("切换通知正文预览")
                    .px(px(10.))
                    .py(px(8.))
                    .rounded(px(6.))
                    .hover(|s| s.bg(rgb(0xeeeae3)))
                    .cursor_pointer()
                    .text_size(px(12.))
                    .child(if self.notification_preview {
                        "☑ 显示通知正文"
                    } else {
                        "□ 显示通知正文"
                    })
                    .on_click(cx.listener(|this, _, _, cx| {
                        cx.stop_propagation();
                        this.notification_preview = !this.notification_preview;
                        cx.notify();
                    })),
            )
            .child(
                div()
                    .px(px(10.))
                    .py(px(6.))
                    .text_size(px(10.5))
                    .text_color(rgb(MUTED))
                    .child(self.material_status.clone()),
            )
    }
    fn demo_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .id("demo-menu")
            .absolute()
            .left(px(16.))
            .bottom(px(112.))
            .w(px(206.))
            .p(px(6.))
            .bg(rgb(0xfdfcf9))
            .border_1()
            .border_color(self.preference_border())
            .rounded(px(10.))
            .shadow_md()
            .occlude()
            .on_click(|_, _, cx| cx.stop_propagation())
            .children(
                [
                    ("mock_incoming", "模拟新消息"),
                    ("mock_delayed", "5 秒后收到消息"),
                    ("mock_edit", "模拟编辑"),
                    ("mock_recall", "模拟撤回"),
                    ("mock_acl", "模拟权限变化"),
                    ("mock_drop", "模拟断线"),
                ]
                .into_iter()
                .map(|(kind, label)| {
                    div()
                        .id(kind)
                        .role(gpui::Role::MenuItem)
                        .aria_label(label)
                        .px(px(12.))
                        .py(px(8.))
                        .rounded(px(6.))
                        .cursor_pointer()
                        .hover(|s| s.bg(rgb(0xeeeae3)))
                        .child(label)
                        .on_click(cx.listener(move |this, _, _, cx| {
                            cx.stop_propagation();
                            this.mock_request(kind, cx);
                        }))
                }),
            )
    }
    fn activate_feature_action(
        &mut self,
        action: &PanelAction,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.activate_message_edit(&action.command, window, cx) {
            return;
        }
        if self.activate_secret_admin(&action.command, true, cx) {
            self.focus.focus(window, cx);
            return;
        }
        if self.activate_composition(&action.command, true, cx) {
            self.focus.focus(window, cx);
            return;
        }
        if self.composition_visible() {
            return;
        }
        if action.command == "local:insert-daily" {
            self.insert_daily_from_panel(window, cx);
            return;
        }
        if self.activate_configuration_action(&action.command, window, cx) {
            return;
        }
        if self.activate_management_action(&action.command, window, cx) {
            return;
        }
        if self.activate_sanction_action(&action.command, window, cx) {
            return;
        }
        if self.activate_report_action(&action.command, window, cx) {
            return;
        }
        if self.activate_moderation_action(&action.command, window, cx) {
            return;
        }
        if self.activate_word_action(&action.command, window, cx) {
            return;
        }
        if self.activate_guess_action(&action.command, window, cx) {
            return;
        }
        if self.activate_tic_action(&action.command, window, cx) {
            return;
        }
        if self.activate_community_action(&action.command, window, cx) {
            return;
        }
        if miochat_gpui::community::is_read_command(&action.command) {
            self.run_command(action.command.trim().to_string(), true, cx);
            return;
        }
        if let Some(simple) = miochat_gpui::simple_actions::SimpleAction::parse(&action.command) {
            self.open_simple_form(simple, cx);
            self.focus.focus(window, cx);
            return;
        }
        if action.command.split_whitespace().next() == Some("/msg") {
            self.open_direct_form(cx);
            self.focus.focus(window, cx);
            return;
        }
        if action.fill
            && [
                "/profile",
                "/bio",
                "/status",
                "/follow",
                "/unfollow",
                "/friend",
                "/unfriend",
                "/ignore",
                "/unignore",
                "/note",
                "/unnote",
                "/avatar",
                "/afk",
                "/badges",
                "/mention",
                "/poke",
            ]
            .iter()
            .any(|c| action.command.split_whitespace().next() == Some(c))
        {
            if ["/bio", "/status", "/avatar", "/afk", "/badges"]
                .iter()
                .any(|c| action.command.split_whitespace().next() == Some(c))
            {
                self.open_profile(self.user.clone(), cx);
            } else {
                self.open_center("contacts", cx);
            }
            return;
        }
        if let Some(rest) = action.command.strip_prefix("/roomset ") {
            let (key, value) = rest.split_once(' ').unwrap_or((rest, ""));
            let intent = if ["private", "readonly", "archive"].contains(&key) {
                RoomAction::Toggle {
                    key: key.into(),
                    on: value.trim() == "on",
                }
            } else {
                RoomAction::Setting(key.into())
            };
            self.open_room_form(intent, cx);
            self.focus.focus(window, cx);
            return;
        }
        if ["/archive", "/unarchive"].contains(&action.command.as_str()) {
            self.open_room_form(
                RoomAction::Toggle {
                    key: "archive".into(),
                    on: action.command == "/archive",
                },
                cx,
            );
            return;
        }
        if action.command.starts_with("/join ") {
            self.select_room(action.command.trim_start_matches("/join ").to_string(), cx);
            return;
        }
        if action.command.trim() == "/invite" || action.command == "/leave" {
            self.open_room_form(
                if action.command == "/leave" {
                    RoomAction::Leave
                } else {
                    RoomAction::Invite
                },
                cx,
            );
            self.focus.focus(window, cx);
            return;
        }
        if let Some(target) = action.command.strip_prefix("/remove ") {
            if target.trim().is_empty() {
                self.run_command("/members".into(), true, cx);
                return;
            }
            self.open_room_form(RoomAction::Remove(target.to_string()), cx);
            return;
        }
        if action.fill && miochat_gpui::word::reserved_start(&action.command) {
            self.error = Some(miochat_gpui::word::RESERVED_NOTICE.into());
            cx.notify();
            return;
        }
        if action.fill {
            if !self.composer.is_empty() {
                self.error = Some("当前输入框还有草稿，请先发送或清空，再填写此功能。".into());
                cx.notify();
                return;
            }
            self.begin_feature_navigation();
            self.composer = action.command.clone();
            self.selection = self.composer.len()..self.composer.len();
            self.set_feature_panel(None);
            self.focus.focus(window, cx);
            cx.notify();
        } else {
            self.run_command(action.command.clone(), true, cx);
        }
    }
    fn feature_popup(&self, panel: Panel, cx: &mut Context<Self>) -> impl IntoElement {
        let room_content = self.room_content(&panel, cx);
        let revision = self.feature_revision;
        let selected = self.feature_filter.as_ref().map_or(0, |f| f.cursor);
        let action_rows: Vec<_> = self
            .feature_filter
            .as_ref()
            .filter(|_| self.feature_filter_active(revision))
            .map(|f| {
                feature_menu::visible_actions(&panel, &f.value)
                    .into_iter()
                    .enumerate()
                    .filter(|(_, original)| !feature_menu::native_card_action(&panel, *original))
                    .map(|(visible, original)| (visible, original, panel.actions[original].clone()))
                    .collect()
            })
            .unwrap_or_default();
        let moderation = self.moderation_content(&panel);
        let information = self.information_content(&panel);
        let community = self.community_content(&panel, cx);
        let art = panel.kind == "art";
        div()
            .id("feature-panel")
            .track_focus(&self.menu_focus)
            .absolute()
            .top(px(52.))
            .right(px(12.))
            .bottom(px(12.))
            .w(px(360.))
            .p(px(16.))
            .rounded(px(12.))
            .bg(self.raised_color())
            .border_1()
            .border_color(self.preference_border())
            .shadow_lg()
            .flex()
            .flex_col()
            .gap(px(12.))
            .on_click(|_, _, cx| cx.stop_propagation())
            .child(
                div()
                    .flex()
                    .justify_between()
                    .items_center()
                    .child(
                        div()
                            .font_weight(gpui::FontWeight::MEDIUM)
                            .child(panel.title),
                    )
                    .child(Self::button("close-features", "关闭").on_click(cx.listener(
                        |this, _, _, cx| {
                            this.dismiss_feature_panel();
                            cx.notify();
                        },
                    ))),
            )
            .child(self.feature_filter_input(cx))
            .child(
                div()
                    .id("feature-content")
                    .when_some(self.feature_view.as_ref(), |d, (handle, _)| {
                        d.track_scroll(handle)
                    })
                    .flex_1()
                    .min_h(px(0.))
                    .overflow_y_scroll()
                    .flex()
                    .flex_col()
                    .gap(px(8.))
                    .child(room_content)
                    .child(information)
                    .child(moderation)
                    .child(community)
                    .children(panel.lines.into_iter().map(move |line| {
                        div()
                            .text_size(px(13.))
                            .when(art, |d| d.font_family("Consolas"))
                            .child(line)
                    }))
                    .children(action_rows.into_iter().map(|(visible, original, action)| {
                        div()
                            .id(gpui::SharedString::from(format!(
                                "feature-action-{original}"
                            )))
                            .role(gpui::Role::Button)
                            .aria_label(action.label.clone())
                            .anchor_scroll(
                                self.feature_anchor_for(&feature_menu::Item::Action(original)),
                            )
                            .when(visible == selected, |d| d.bg(rgb(0xe9e6df)))
                            .focusable()
                            .p(px(9.))
                            .rounded(px(6.))
                            .cursor_pointer()
                            .hover(|s| s.bg(rgb(0xe9e6df)))
                            .focus_visible(|s| s.bg(rgb(0xd5e1ec)))
                            .child(action.label)
                            .on_click(cx.listener(move |this, _, window, cx| {
                                this.activate_visible_feature(revision, original, window, cx);
                            }))
                    })),
            )
    }
    fn input(
        &self,
        label: &'static str,
        value: &str,
        field: Field,
        secret: bool,
        cx: &mut Context<Self>,
    ) -> impl IntoElement {
        let mut shown = if secret {
            "•".repeat(value.chars().count())
        } else {
            value.to_string()
        };
        if self.field == field {
            let mut end = self.selection.end.min(value.len());
            while !value.is_char_boundary(end) {
                end -= 1;
            }
            let cursor = if secret {
                "•".repeat(value[..end].chars().count()).len()
            } else {
                end
            };
            shown.insert(cursor, '│');
        }
        div()
            .flex()
            .flex_col()
            .gap(px(7.))
            .when(
                self.reply_to > 0
                    && !secret
                    && !matches!(field, Field::CompositionTitle | Field::CompositionBody),
                |d| {
                    d.child(
                        div()
                            .flex()
                            .justify_between()
                            .text_size(px(12.))
                            .child(format!("回复 #{}", self.reply_to))
                            .child(
                                Self::button("cancel-reply", "取消回复").on_click(cx.listener(
                                    |this, _, _, cx| {
                                        this.run_command("/cancelreply".into(), true, cx)
                                    },
                                )),
                            ),
                    )
                },
            )
            .child(div().text_size(px(12.)).text_color(rgb(MUTED)).child(label))
            .child(
                div()
                    .id(label)
                    .relative()
                    .role(if secret {
                        gpui::Role::PasswordInput
                    } else {
                        gpui::Role::TextInput
                    })
                    .aria_label(label)
                    .h(px(if field == Field::CompositionBody {
                        260.
                    } else if field == Field::DirectBody {
                        120.
                    } else {
                        42.
                    }))
                    .px(px(12.))
                    .rounded(px(8.))
                    .border_1()
                    .border_color(rgb(if self.field == field {
                        0x8297ad
                    } else {
                        BORDER
                    }))
                    .bg(self.raised_color())
                    .flex()
                    .items_center()
                    .when(
                        matches!(field, Field::DirectBody | Field::CompositionBody),
                        |d| d.items_start().py(px(8.)).overflow_y_scroll(),
                    )
                    .when(field == Field::CompositionBody, |d| {
                        d.overflow_x_scroll()
                            .font_family("Consolas")
                            .whitespace_nowrap()
                    })
                    .cursor_text()
                    .on_click(cx.listener(move |this, _, window, cx| {
                        if !this.connection_field_enabled(field) {
                            return;
                        }
                        this.field = field;
                        this.reset_selection();
                        this.focus.focus(window, cx);
                        cx.notify();
                    }))
                    .child(
                        div()
                            .when(field == Field::DirectBody, |d| {
                                d.w_full().min_w(px(0.)).whitespace_normal()
                            })
                            .when(field == Field::CompositionBody, |d| d.whitespace_nowrap())
                            .child(if shown.is_empty() { " ".into() } else { shown }),
                    )
                    .when(self.field == field, |d| {
                        d.child(InputCapture {
                            entity: cx.entity(),
                            message_revision: None,
                        })
                    }),
            )
    }
    fn button(
        id: impl Into<gpui::ElementId>,
        label: impl Into<gpui::SharedString>,
    ) -> Stateful<Div> {
        let label = label.into();
        div()
            .id(id)
            .role(gpui::Role::Button)
            .aria_label(label.to_string())
            .h(px(38.))
            .px(px(16.))
            .flex()
            .items_center()
            .justify_center()
            .rounded(px(8.))
            .bg(rgb(0x292d32))
            .text_color(rgb(0xfdfcf9))
            .font_weight(gpui::FontWeight::MEDIUM)
            .cursor_pointer()
            .hover(|s| s.bg(rgb(0x3c4148)))
            .child(label)
    }
    fn panel(&self) -> Div {
        div().w(px(420.)).flex().flex_col().gap(px(18.))
    }
    fn centered(&self, child: impl IntoElement) -> impl IntoElement {
        div()
            .size_full()
            .p(px(32.))
            .flex()
            .items_center()
            .justify_center()
            .child(child)
    }
    fn heading(&self, title: &'static str, subtitle: &'static str) -> impl IntoElement {
        div()
            .mb(px(8.))
            .flex()
            .flex_col()
            .gap(px(10.))
            .child(
                div()
                    .text_size(px(24.))
                    .line_height(px(32.))
                    .font_weight(gpui::FontWeight::SEMIBOLD)
                    .child(title),
            )
            .child(
                div()
                    .text_size(px(13.))
                    .line_height(px(22.))
                    .text_color(rgb(MUTED))
                    .child(subtitle),
            )
    }
    fn error_box(&self, error: String) -> impl IntoElement {
        div()
            .px(px(12.))
            .py(px(10.))
            .rounded(px(8.))
            .bg(rgb(0xf7e6e1))
            .text_size(px(12.))
            .text_color(rgb(ERROR))
            .child(error)
    }
    fn render_connect(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let generation = self.generation;
        div()
            .id("connect-form-scroll")
            .size_full()
            .overflow_y_scroll()
            .flex()
            .flex_col()
            .items_center()
            .p(px(24.))
            .child(
                self.panel()
                    .flex_none()
                    .child(self.heading(
                        "MioChat",
                        "启动后自动连接 chat.mio.blue；SSH 与应用账户是两个独立阶段。",
                    ))
                    .child(div().text_size(px(12.)).child(self.status.clone()))
                    .when(!self.advanced_connection, |d| {
                        d.child(self.input(
                            "应用用户名",
                            &self.username,
                            Field::Username,
                            false,
                            cx,
                        ))
                        .child(self.input("应用密码", &self.password, Field::Password, true, cx))
                        .child(
                            div().text_size(px(12.)).child(
                                "连接和协议就绪后，请明确点击登录；不会排队或自动重送密码。",
                            ),
                        )
                    })
                    .when(self.advanced_connection, |d| {
                        d.child(self.input("服务器", &self.spec.host, Field::Host, false, cx))
                            .child(self.input("端口", &self.port_text, Field::Port, false, cx))
                            .child(self.input(
                                "SSH 用户",
                                &self.spec.ssh_user,
                                Field::SshUser,
                                false,
                                cx,
                            ))
                            .child(self.identity_controls(cx))
                    })
                    .when_some(self.error.clone(), |d, e| d.child(self.error_box(e)))
                    .child(
                        Self::button(
                            "connect",
                            if self.connecting {
                                "连接中…"
                            } else {
                                "连接 / 重试"
                            },
                        )
                        .on_click(cx.listener(move |this, _, _, cx| {
                            if this.generation == generation {
                                this.begin_connect(cx);
                            }
                        })),
                    )
                    .when(self.connecting, |d| {
                        d.child(
                            Self::button("cancel-connect", "取消连接").on_click(cx.listener(
                                move |this, _, _, cx| {
                                    if this.generation == generation {
                                        this.cancel_connect(cx);
                                    }
                                },
                            )),
                        )
                    })
                    .child(self.advanced_connection_button(cx))
                    .child(
                        Self::button("start-mock", "本地预览（不联网）").on_click(cx.listener(
                            move |this, _, _, cx| {
                                if this.generation != generation || this.screen != Screen::Connect {
                                    return;
                                }
                                this.cancel_connect(cx);
                                this.mock_mode = true;
                                this.advanced_connection = false;
                                this.username = "本地访客".into();
                                this.open_session();
                                cx.notify();
                            },
                        )),
                    ),
            )
    }
    fn render_auth(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let generation = self.generation;
        self.centered(
            self.panel()
                .child(self.heading(
                    if self.registering {
                        "创建账户"
                    } else {
                        "欢迎回来"
                    },
                    if self.mock_mode {
                        "这是无网络的本地预览，可直接进入，无需真实密码。"
                    } else {
                        "登录你的 MioChat 账户。密码不会保存在本机。"
                    },
                ))
                .child(self.input("用户名", &self.username, Field::Username, false, cx))
                .when(!self.mock_mode, |d| {
                    d.child(self.input("密码", &self.password, Field::Password, true, cx))
                })
                .when(self.registering, |d| {
                    d.child(self.input("确认密码", &self.confirm, Field::Confirm, true, cx))
                })
                .when(
                    self.registering && self.registration_mode == "invite",
                    |d| d.child(self.input("邀请码", &self.invite, Field::Invite, true, cx)),
                )
                .when_some(self.error.clone(), |d, e| d.child(self.error_box(e)))
                .child(
                    Self::button(
                        "authenticate",
                        if self.mock_mode {
                            "进入本地预览"
                        } else if self.registering {
                            "注册"
                        } else {
                            "登录"
                        },
                    )
                    .on_click(cx.listener(move |this, _, _, cx| {
                        if this.generation == generation {
                            this.authenticate(this.registering, cx);
                        }
                    })),
                )
                .when(!self.mock_mode && self.registration_mode != "closed", |d| {
                    d.child(
                        div()
                            .id("toggle-register")
                            .text_size(px(12.))
                            .text_color(rgb(MUTED))
                            .cursor_pointer()
                            .child(if self.registering {
                                "已有账户？登录"
                            } else {
                                "还没有账户？注册"
                            })
                            .on_click(cx.listener(move |this, _, _, cx| {
                                if this.generation != generation
                                    || this.screen != Screen::Auth
                                    || !this.login_gate.can_submit(this.generation)
                                    || this.pending_key_login.is_some()
                                {
                                    return;
                                }
                                this.clear_login_secrets();
                                this.registering = !this.registering;
                                this.field = Field::Username;
                                this.reset_selection();
                                cx.notify();
                            })),
                    )
                })
                .child(self.advanced_connection_button(cx)),
        )
    }
    fn render_password(&self, cx: &mut Context<Self>) -> impl IntoElement {
        self.centered(
            self.panel()
                .gap(px(14.))
                .child(self.heading(
                    "修改密码",
                    if self.must_reset {
                        "当前只可改密或退出；修改后才会加载聊天。"
                    } else {
                        "当前会话会保留，其他会话将退出。"
                    },
                ))
                .child(self.input("当前密码", &self.password, Field::Password, true, cx))
                .child(self.input("新密码", &self.confirm, Field::Confirm, true, cx))
                .child(self.input("确认新密码", &self.invite, Field::Invite, true, cx))
                .child(
                    Self::button(
                        "change-password",
                        if self.pending_password.is_some() {
                            "更新中…"
                        } else {
                            "更新密码"
                        },
                    )
                    .on_click(cx.listener(|this, _, _, cx| this.change_password(cx))),
                )
                .when(self.pending_password.is_none(), |d| {
                    d.child(
                        Self::button(
                            "cancel-password",
                            if self.must_reset {
                                "退出连接"
                            } else {
                                "取消"
                            },
                        )
                        .on_click(cx.listener(|this, _, _, cx| this.cancel_password(cx))),
                    )
                })
                .when_some(self.error.clone(), |d, error| {
                    d.child(self.error_box(error))
                }),
        )
    }
    fn render_trust(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let generation = self.generation;
        let shown_probe = self.host_key.clone();
        let fingerprint = self
            .host_key
            .as_ref()
            .map(|k| k.fingerprint.clone())
            .unwrap_or_default();
        self.centered(
            self.panel()
                .child(self.heading("确认服务器身份", "先与管理员核对指纹，再决定是否信任。"))
                .child(
                    div()
                        .p(px(16.))
                        .rounded(px(8.))
                        .bg(self.raised_color())
                        .text_size(px(12.))
                        .child(fingerprint),
                )
                .when_some(self.error.clone(), |d, e| d.child(self.error_box(e)))
                .child(Self::button("trust", "信任并连接").on_click(cx.listener(
                    move |this, _, _, cx| {
                        if let Some(probe) = shown_probe.as_ref() {
                            this.accept_host_key(generation, probe, cx);
                        }
                    },
                )))
                .child(
                    div()
                        .id("cancel-trust")
                        .cursor_pointer()
                        .text_color(rgb(MUTED))
                        .child("返回")
                        .on_click(cx.listener(|this, _, _, cx| this.cancel_connect(cx))),
                ),
        )
    }
    fn message_bubble(&self, message: Message, cx: &mut Context<Self>) -> Div {
        let attachment = self.render_message_attachment(&message, cx);
        let id = message.id;
        let own = message.sender == self.user;
        let body = if message.recalled != 0 {
            "这条消息已撤回".to_string()
        } else {
            message.body
        };
        let selected = self
            .message_selection
            .as_ref()
            .filter(|(selected, _)| *selected == id)
            .map(|(_, r)| r.clone())
            .filter(|r| r.end <= body.len());
        let text = gpui::StyledText::new(body.clone()).with_highlights(
            selected.clone().filter(|r| !r.is_empty()).map(|range| {
                (
                    range,
                    gpui::HighlightStyle {
                        background_color: Some(gpui::rgba(0x2f5d8a38).into()),
                        ..Default::default()
                    },
                )
            }),
        );
        let down = text.layout().clone();
        let movement = down.clone();
        let copy = selected
            .and_then(|r| body.get(r).map(str::to_string))
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| body.clone());
        let time = if message.at < 1_000_000_000 {
            "预览".to_string()
        } else {
            format!(
                "{:02}:{:02} UTC",
                (message.at / 3600) % 24,
                (message.at / 60) % 60
            )
        };
        div()
            .w_full()
            .flex()
            .flex_col()
            .when(own, |d| d.items_end())
            .when(!own, |d| d.items_start())
            .child(
                div()
                    .max_w(px(740.))
                    .min_w(px(150.))
                    .px(px(16.))
                    .py(px(if self.account_preferences.on("compact") {
                        6.
                    } else {
                        12.
                    }))
                    .rounded(px(12.))
                    .bg(if own {
                        gpui::rgba(0xe8e4dcec).into()
                    } else {
                        self.raised_color()
                    })
                    .flex()
                    .flex_col()
                    .gap(px(8.))
                    .child(attachment)
                    .child(
                        div()
                            .flex()
                            .gap(px(10.))
                            .items_center()
                            .text_size(px(11.))
                            .text_color(rgb(MUTED))
                            .child(
                                div()
                                    .font_weight(gpui::FontWeight::MEDIUM)
                                    .id(gpui::SharedString::from(format!("profile-author-{id}")))
                                    .role(gpui::Role::Button)
                                    .cursor_pointer()
                                    .child(message.sender.clone())
                                    .on_click(cx.listener(move |this, _, _, cx| {
                                        if message.sender != "[dot]" {
                                            this.open_profile(message.sender.clone(), cx);
                                        }
                                    })),
                            )
                            .child(time)
                            .when(message.edited != 0, |d| d.child("已编辑"))
                            .child(div().flex_1())
                            .child(
                                div()
                                    .id(gpui::SharedString::from(format!("actions-{id}")))
                                    .role(gpui::Role::Button)
                                    .aria_label("消息操作")
                                    .focusable()
                                    .px(px(5.))
                                    .cursor_pointer()
                                    .child("···")
                                    .on_click(cx.listener(move |this, _, _, cx| {
                                        this.begin_feature_navigation();
                                        this.run_command(format!("/read {id}"), true, cx);
                                        cx.stop_propagation();
                                        cx.notify();
                                    })),
                            )
                            .child(
                                div()
                                    .id(gpui::SharedString::from(format!("copy-{id}")))
                                    .role(gpui::Role::Button)
                                    .aria_label("复制消息")
                                    .focusable()
                                    .px(px(5.))
                                    .rounded(px(3.))
                                    .cursor_pointer()
                                    .hover(|s| s.bg(rgb(0xe0dcd4)))
                                    .focus_visible(|s| s.bg(rgb(0xd5e1ec)))
                                    .child("⧉")
                                    .tooltip(|_, cx| {
                                        cx.new(|_| Tip("复制选中文字或整条消息".into())).into()
                                    })
                                    .on_click(move |_, _, cx| {
                                        cx.stop_propagation();
                                        cx.write_to_clipboard(gpui::ClipboardItem::new_string(
                                            copy.clone(),
                                        ));
                                    }),
                            ),
                    )
                    .when(message.reply_to > 0, |d| {
                        d.child(
                            div()
                                .text_size(px(12.))
                                .text_color(rgb(MUTED))
                                .child(format!(
                                    "回复 #{} @{}  {}",
                                    message.reply_to, message.reply_name, message.reply_body
                                )),
                        )
                    })
                    .when(!message.reactions.is_empty(), |d| {
                        d.child(
                            div()
                                .text_size(px(11.))
                                .text_color(rgb(ACCENT))
                                .child(message.reactions.clone()),
                        )
                    })
                    .child(
                        div()
                            .id(gpui::SharedString::from(format!("message-text-{id}")))
                            .track_focus(&self.message_focus)
                            .text_size(px(14.))
                            .line_height(px(23.))
                            .cursor_text()
                            .on_mouse_down(
                                gpui::MouseButton::Left,
                                cx.listener(
                                    move |this, event: &gpui::MouseDownEvent, window, cx| {
                                        let index = down
                                            .index_for_position(event.position)
                                            .unwrap_or_else(|i| i);
                                        this.message_selection = Some((id, index..index));
                                        this.message_drag = Some((id, index));
                                        this.message_focus.focus(window, cx);
                                        cx.stop_propagation();
                                        cx.notify();
                                    },
                                ),
                            )
                            .on_mouse_move(cx.listener(
                                move |this, event: &gpui::MouseMoveEvent, _, cx| {
                                    if event.pressed_button == Some(gpui::MouseButton::Left)
                                        && let Some((active, anchor)) = this.message_drag
                                        && active == id
                                    {
                                        let index = movement
                                            .index_for_position(event.position)
                                            .unwrap_or_else(|i| i);
                                        this.message_selection =
                                            Some((id, anchor.min(index)..anchor.max(index)));
                                        cx.notify();
                                    }
                                },
                            ))
                            .on_mouse_up(
                                gpui::MouseButton::Left,
                                cx.listener(|this, _, _, _| {
                                    this.message_drag = None;
                                }),
                            )
                            .child(text),
                    ),
            )
    }
    fn render_chat(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let timeline = div()
            .id("timeline")
            .track_scroll(&self.timeline_scroll)
            .flex_1()
            .min_h(px(0.))
            .overflow_y_scroll()
            .px(px(30.))
            .py(px(28.))
            .flex()
            .flex_col()
            .gap(px(if self.account_preferences.on("compact") {
                10.
            } else {
                22.
            }))
            .child(self.timeline_tools(cx))
            .when(self.messages.is_empty(), |d| {
                d.child(
                    div()
                        .flex_1()
                        .flex()
                        .flex_col()
                        .items_center()
                        .justify_center()
                        .gap(px(10.))
                        .child(
                            div()
                                .text_size(px(22.))
                                .font_weight(gpui::FontWeight::MEDIUM)
                                .child("开始聊聊"),
                        )
                        .child(div().text_color(rgb(MUTED)).child("这个房间还没有消息")),
                )
            })
            .children(
                self.messages
                    .clone()
                    .into_iter()
                    .map(|message| self.message_bubble(message, cx)),
            );
        let mut draft = self.composer.clone();
        let mut cursor = self.selection.end.min(draft.len());
        while !draft.is_char_boundary(cursor) {
            cursor -= 1;
        }
        draft.insert(cursor, '│');
        let composer = div()
            .child(self.attachment_controls(cx))
            .child(self.custom_emoji_controls(cx))
            .child(self.render_custom_emoji_picker(cx))
            .child(self.render_attachment_preview(cx))
            .child(
                Self::button("open-dot", "dot 私有审阅 · Ctrl+Shift+D")
                    .on_click(cx.listener(|this, _, window, cx| this.dot_open(window, cx))),
            )
            .child(
                Self::button("open-emoji", "😀 表情 · Ctrl+Shift+E")
                    .on_click(cx.listener(|this, _, window, cx| this.open_emoji(window, cx))),
            )
            .when(self.persistence.quarantined(), |d| {
                d.child(
                    div()
                        .p(px(8.))
                        .child(miochat_gpui::word::QUARANTINE_NOTICE)
                        .child(
                            Self::button("quarantine-review", "核对并处理旧敏感草稿").on_click(
                                cx.listener(|this, _, _, cx| {
                                    this.run_command("local:quarantine-review".into(), true, cx)
                                }),
                            ),
                        ),
                )
            })
            .flex_none()
            .px(px(30.))
            .pt(px(8.))
            .pb(px(18.))
            .flex()
            .flex_col()
            .gap(px(7.))
            .child(
                div()
                    .rounded(px(14.))
                    .border_1()
                    .border_color(rgb(0xc9c3b8))
                    .bg(self.raised_color())
                    .p(px(12.))
                    .flex()
                    .gap(px(12.))
                    .items_end()
                    .child(
                        div()
                            .id("composer")
                            .role(gpui::Role::MultilineTextInput)
                            .aria_label("消息输入")
                            .relative()
                            .flex_1()
                            .min_h(px(54.))
                            .line_height(px(23.))
                            .cursor_text()
                            .on_click(cx.listener(|this, _, window, cx| {
                                this.cancel_completion();
                                this.focus.focus(window, cx);
                                cx.notify();
                            }))
                            .child(if self.composer.is_empty() {
                                "写一条消息…".into()
                            } else {
                                draft
                            })
                            .child(InputCapture {
                                entity: cx.entity(),
                                message_revision: Some(self.message_revision),
                            }),
                    )
                    .child(
                        Self::button(
                            "send",
                            if self.pending_send.is_some() {
                                "发送中"
                            } else {
                                "发送"
                            },
                        )
                        .on_click(cx.listener(|this, _, _, cx| this.send_message(cx))),
                    ),
            )
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(12.))
                    .text_size(px(11.))
                    .text_color(rgb(MUTED))
                    .child(
                        div()
                            .id("load-draft")
                            .role(gpui::Role::Button)
                            .aria_label("读取对话草稿")
                            .cursor_pointer()
                            .child("读取草稿")
                            .on_click(
                                cx.listener(|this, _, _, cx| this.draft_request("draft_load", cx)),
                            ),
                    )
                    .child(
                        div()
                            .id("save-draft")
                            .role(gpui::Role::Button)
                            .aria_label("保存当前草稿")
                            .cursor_pointer()
                            .child(if self.draft_conflict {
                                "保留本地"
                            } else {
                                "保存草稿"
                            })
                            .on_click(cx.listener(|this, _, _, cx| {
                                this.draft_request(
                                    if this.draft_conflict {
                                        "draft_keep"
                                    } else {
                                        "draft_save"
                                    },
                                    cx,
                                )
                            })),
                    )
                    .when(
                        self.manual_draft_backups.contains_key(&self.room)
                            || self.persistence_enabled && self.persistence.has_input_backup(),
                        |d| {
                            d.child(
                                div()
                                    .id("restore-input-backup")
                                    .role(gpui::Role::Button)
                                    .aria_label("恢复输入备份")
                                    .cursor_pointer()
                                    .child("恢复输入备份")
                                    .on_click(cx.listener(|this, _, _, cx| {
                                        if this.persistence_enabled {
                                            if let Some(snapshot) = this
                                                .persistence
                                                .restore_input_backup(this.persistence_now())
                                            {
                                                this.apply_snapshot(snapshot);
                                            }
                                            cx.notify();
                                            return;
                                        }
                                        if let Some(backup) =
                                            this.manual_draft_backups.remove(&this.room)
                                        {
                                            this.cancel_completion();
                                            let previous =
                                                std::mem::replace(&mut this.composer, backup);
                                            this.manual_draft_backups
                                                .insert(this.room.clone(), previous);
                                            this.reset_selection();
                                            cx.notify();
                                        }
                                    })),
                            )
                        },
                    )
                    .when(
                        self.pending_draft.is_some() || self.persistence_enabled,
                        |d| {
                            d.child(
                                if self.pending_draft.is_some() || self.persistence.pending() {
                                    "同步中…"
                                } else if self.persistence.conflicted() {
                                    "存在冲突 · 自动保存已暂停"
                                } else if !self.persistence.ready() {
                                    "正在读取版本 · 输入暂存"
                                } else if self.persistence.dirty() {
                                    "等待自动保存"
                                } else {
                                    "自动保存"
                                },
                            )
                        },
                    ),
            )
            .child(
                div()
                    .flex()
                    .justify_between()
                    .text_size(px(10.5))
                    .text_color(rgb(MUTED))
                    .child(
                        div()
                            .flex_1()
                            .min_w(px(0.))
                            .flex()
                            .flex_col()
                            .child("Enter 发送 · Shift+Enter 换行")
                            .child("F1帮助 · F2通知 · F3联系人 · F4菜单 · F8管理"),
                    )
                    .child(if self.fixture_driver.is_some() {
                        "本地合成联调 · 仅内存数据 · 无网络"
                    } else if self.mock_mode {
                        "本地预览，不会发送到真实聊天室"
                    } else {
                        "通过 SSH 加密连接"
                    }),
            );
        div()
            .size_full()
            .flex()
            .flex_col()
            .child(timeline)
            .child(composer)
            .when_some(self.error.clone(), |d, e| {
                d.child(div().px(px(30.)).pb(px(12.)).child(self.error_box(e)))
            })
    }
}

impl Focusable for MioChatApp {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus.clone()
    }
}

impl Render for MioChatApp {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        if self.dot_visible()
            || self.emoji_open()
            || self.feature_panel.is_some()
            || self.secret_admin_visible()
            || self.maintenance_visible()
        {
            self.menu_focus.focus(window, cx);
        } else if matches!(self.screen, Screen::Password | Screen::KeyBinding) {
            self.focus.focus(window, cx);
        }
        self.foreground = window.is_window_active();
        self.window_handle = Some(window.window_handle());
        if self.foreground && self.screen == Screen::Chat {
            self.notifications.read(&self.room);
            for (tag, (generation, room)) in &self.notification_targets {
                if *generation == self.generation && *room == self.room {
                    cx.dismiss_system_notification(tag);
                }
            }
        }
        div()
            .id("miochat-root")
            .size_full()
            .flex()
            .relative()
            .font_family("Noto Sans SC")
            .text_size(px(14.))
            .line_height(px(22.))
            .text_color(self.preference_text())
            .track_focus(&self.focus)
            .key_context("MioChat")
            .on_key_down(cx.listener(Self::on_key_down))
            .on_click(cx.listener(|this, _, window, cx| {
                if this.appearance_menu || this.mock_menu {
                    this.appearance_menu = false;
                    this.mock_menu = false;
                    this.focus.focus(window, cx);
                    cx.notify();
                }
            }))
            .when(
                affordances_ui::sidebar_shown(self.screen, self.sidebar_visible),
                |d| d.child(self.sidebar(cx)),
            )
            .child(
                div()
                    .flex_1()
                    .min_w(px(0.))
                    .h_full()
                    .flex()
                    .flex_col()
                    .bg(self.surface_color())
                    .child(self.toolbar(cx))
                    .when_some(self.dot_ui.notice.clone(), |d, notice| {
                        d.child(div().p(px(10.)).child(notice).child(
                            Self::button("dot-notice-dismiss", "已知悉").on_click(cx.listener(
                                |this, _, _, cx| {
                                    this.dot_ui.notice = None;
                                    cx.notify();
                                },
                            )),
                        ))
                    })
                    .when_some(self.secret_notice.clone(), |d, notice| {
                        d.child(div().child(notice).child(
                            Self::button("secret-notice-dismiss", "已知悉").on_click(cx.listener(
                                |this, _, _, cx| {
                                    this.secret_notice = None;
                                    cx.notify();
                                },
                            )),
                        ))
                    })
                    .when_some(self.maintenance_notice.clone(), |d, notice| {
                        d.child(div().p(px(10.)).text_size(px(12.)).child(notice).child(
                            Self::button("maintenance-notice-dismiss", "已知悉").on_click(
                                cx.listener(|this, _, _, cx| {
                                    this.maintenance_notice = None;
                                    cx.notify();
                                }),
                            ),
                        ))
                    })
                    .child(
                        div()
                            .flex_1()
                            .min_h(px(0.))
                            .relative()
                            .child(match self.screen {
                                Screen::Connect => self.render_connect(cx).into_any_element(),
                                Screen::Auth => self.render_auth(cx).into_any_element(),
                                Screen::Password => self.render_password(cx).into_any_element(),
                                Screen::KeyBinding => {
                                    self.render_key_binding(cx).into_any_element()
                                }
                                Screen::Trust => self.render_trust(cx).into_any_element(),
                                Screen::Chat => self.render_chat(cx).into_any_element(),
                            }),
                    ),
            )
            .when(self.dot_visible(), |d| d.child(self.dot_popup(cx)))
            .when(self.emoji_bound(), |d| d.child(self.emoji_popup(cx)))
            .when(self.appearance_menu, |d| d.child(self.appearance_popup(cx)))
            .when(self.profile.is_some() && self.screen == Screen::Chat, |d| {
                d.child(self.profile_popup(cx))
            })
            .when(self.center_open && self.screen == Screen::Chat, |d| {
                d.child(self.center_popup(cx))
            })
            .when(self.preferences_open && self.screen == Screen::Chat, |d| {
                d.child(self.preferences_popup(cx))
            })
            .when(self.key_reminder_active(), |d| {
                d.child(self.key_reminder_popup(cx))
            })
            .when(self.room_form.is_some(), |d| {
                d.child(self.room_form_popup(cx))
            })
            .when(
                self.direct_form.is_some() && self.screen == Screen::Chat,
                |d| d.child(self.direct_form_popup(cx)),
            )
            .when(
                self.word_start.is_some() && self.screen == Screen::Chat,
                |d| d.child(self.word_start_popup(cx)),
            )
            .when(self.mock_menu, |d| d.child(self.demo_popup(cx)))
            .when_some(self.feature_panel.clone(), |d, p| {
                d.child(self.feature_popup(p, cx))
            })
            .when(self.composition_visible(), |d| {
                d.child(self.composition_popup(cx))
            })
            .when(self.secret_admin_visible(), |d| {
                d.child(self.secret_admin_popup(cx))
            })
            .when(self.completion_current(), |d| {
                d.child(self.completion_popup(cx))
            })
            .when(self.maintenance_visible(), |d| {
                d.child(self.maintenance_popup(cx))
            })
            .when(
                self.composition.is_some() && !self.composition_visible(),
                |d| d.child(self.composition_recovery_notice(cx)),
            )
    }
}

impl EntityInputHandler for MioChatApp {
    fn text_for_range(
        &mut self,
        range: Range<usize>,
        actual: &mut Option<Range<usize>>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<String> {
        if self.dot_visible()
            || self.emoji_open()
            || self.feature_panel.is_some()
            || self.secret_field()
            || self.secret_admin_visible()
            || self.maintenance_visible()
        {
            *actual = None;
            return None;
        }
        let bytes = editing::byte_range(self.current_value(), range);
        *actual = Some(editing::utf16_range(self.current_value(), bytes.clone()));
        Some(self.current_value()[bytes].to_string())
    }
    fn selected_text_range(
        &mut self,
        _: bool,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<UTF16Selection> {
        if self.dot_visible()
            || self.emoji_open()
            || self.feature_panel.is_some()
            || self.secret_admin_visible()
            || self.maintenance_visible()
        {
            return None;
        }
        Some(UTF16Selection {
            range: editing::utf16_range(self.current_value(), self.selection.clone()),
            reversed: self.composition_input_active()
                && self
                    .composition_cursor
                    .is_some_and(|(anchor, caret)| anchor > caret),
        })
    }
    fn marked_text_range(&self, _: &mut Window, _: &mut Context<Self>) -> Option<Range<usize>> {
        if self.custom_emoji_context_occupied()
            || self.dot_visible()
            || self.emoji_open()
            || self.feature_panel.is_some()
        {
            return None;
        }
        self.marked_range
            .clone()
            .map(|range| editing::utf16_range(self.current_value(), range))
    }
    fn unmark_text(&mut self, _: &mut Window, _: &mut Context<Self>) {
        if self.custom_emoji_context_occupied()
            || self.dot_visible()
            || self.emoji_open()
            || self.feature_panel.is_some()
        {
            return;
        }
        self.marked_range = None;
    }
    fn replace_text_in_range(
        &mut self,
        range: Option<Range<usize>>,
        text: &str,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.replace_current(range, text);
        cx.notify();
    }
    fn replace_and_mark_text_in_range(
        &mut self,
        range: Option<Range<usize>>,
        text: &str,
        selected: Option<Range<usize>>,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.custom_emoji_context_occupied()
            || self.dot_visible()
            || self.emoji_open()
            || self.feature_panel.is_some()
        {
            return;
        }
        let start = range
            .clone()
            .map(|r| editing::byte_range(self.current_value(), r).start)
            .or_else(|| self.marked_range.as_ref().map(|r| r.start))
            .unwrap_or(self.selection.start);
        if self.replace_current(range, text) {
            self.marked_range = if text.is_empty() {
                None
            } else {
                Some(start..start + text.len())
            };
            self.selection = selected
                .map(|r| {
                    let r = editing::byte_range(text, r);
                    start + r.start..start + r.end
                })
                .unwrap_or(start + text.len()..start + text.len());
            cx.notify();
        }
    }
    fn bounds_for_range(
        &mut self,
        range: Range<usize>,
        fallback: Bounds<Pixels>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<Bounds<Pixels>> {
        if self.custom_emoji_context_occupied()
            || self.dot_visible()
            || self.emoji_open()
            || self.feature_panel.is_some()
        {
            return None;
        }
        if self.composition_visible() && self.field == Field::CompositionBody {
            return self.composition_ime_bounds(range);
        }
        let bounds = self.input_bounds.unwrap_or(fallback);
        let byte = editing::byte_offset(self.current_value(), range.start);
        let width: f32 = self.current_value()[..byte]
            .chars()
            .map(|c| if c.is_ascii() { 7.0 } else { 13.0 })
            .sum();
        Some(Bounds::new(
            gpui::point(
                bounds.origin.x + px(12.) + px(width).min(bounds.size.width - px(30.)),
                bounds.origin.y + bounds.size.height - px(8.),
            ),
            size(px(1.), px(20.)),
        ))
    }
    fn character_index_for_point(
        &mut self,
        point: Point<Pixels>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<usize> {
        if self.custom_emoji_context_occupied()
            || self.dot_visible()
            || self.emoji_open()
            || self.feature_panel.is_some()
        {
            return None;
        }
        if self.composition_visible() && self.field == Field::CompositionBody {
            return self.composition_ime_index(point);
        }
        let bounds = self.input_bounds?;
        let mut x = bounds.origin.x + px(12.);
        for (byte, ch) in self.current_value().char_indices() {
            x += px(if ch.is_ascii() { 7.0 } else { 13.0 });
            if point.x < x {
                return Some(editing::utf16_offset(self.current_value(), byte));
            }
        }
        Some(self.current_value().encode_utf16().count())
    }
}

struct InputCapture {
    entity: Entity<MioChatApp>,
    message_revision: Option<u64>,
}
impl IntoElement for InputCapture {
    type Element = Self;
    fn into_element(self) -> Self {
        self
    }
}
impl Element for InputCapture {
    type RequestLayoutState = ();
    type PrepaintState = ();
    fn id(&self) -> Option<ElementId> {
        None
    }
    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }
    fn request_layout(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        window: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, ()) {
        let mut style = Style {
            position: gpui::Position::Absolute,
            ..Default::default()
        };
        style.size.width = relative(1.).into();
        style.size.height = relative(1.).into();
        (window.request_layout(style, [], cx), ())
    }
    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut (),
        _: &mut Window,
        cx: &mut App,
    ) {
        self.entity.update(cx, |app, _| {
            app.input_bounds = Some(bounds);
            if let Some(revision) = self.message_revision {
                app.layout_revision = revision;
                app.layout_epoch = app.layout_epoch.wrapping_add(1);
            }
        });
    }
    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut (),
        _: &mut (),
        window: &mut Window,
        cx: &mut App,
    ) {
        if !self.entity.read(cx).composition_visible()
            && (self.entity.read(cx).feature_panel.is_some()
                || self.entity.read(cx).appearance_menu)
        {
            return;
        }
        let focus = self.entity.read(cx).focus.clone();
        window.handle_input(
            &focus,
            ElementInputHandler::new(bounds, self.entity.clone()),
            cx,
        );
    }
}

struct Tip(gpui::SharedString);
impl Render for Tip {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        div()
            .px(px(10.))
            .py(px(6.))
            .rounded(px(6.))
            .bg(rgb(0x292d32))
            .text_color(rgb(0xfdfcf9))
            .text_size(px(12.))
            .child(self.0.clone())
    }
}
struct ContrastBackdrop;
impl Render for ContrastBackdrop {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        div().size_full().flex().children(
            [0xfdfdfd, 0x131721, 0xe1dbcc]
                .into_iter()
                .enumerate()
                .map(|(column, color)| {
                    div()
                        .flex_1()
                        .h_full()
                        .bg(rgb(color))
                        .flex()
                        .flex_col()
                        .children((0..24).map(move |row| {
                            div()
                                .h(px(40.))
                                .w_full()
                                .flex()
                                .children((0..10).map(move |cell| {
                                    div().flex_1().h_full().bg(rgb(
                                        if column == 2 && (row + cell) % 2 == 0 {
                                            0x36485a
                                        } else {
                                            color
                                        },
                                    ))
                                }))
                        }))
                }),
        )
    }
}

fn main() {
    gpui_platform::application().run(|cx: &mut App| {
        cx.set_app_identity("org.miochat.native", "MioChat");
        let lifecycle = std::rc::Rc::new(std::cell::RefCell::new(
            miochat_gpui::lifecycle::WindowLifecycle::<gpui::WindowId>::default(),
        ));
        let close_policy = lifecycle.clone();
        cx.on_window_closed(move |cx, closed| {
            // Drop the state borrow before quit can cause other close callbacks.
            let should_quit = close_policy.borrow_mut().closed(closed);
            if should_quit {
                cx.quit();
            }
        })
        .detach();
        if std::env::args().any(|arg| arg == "--backdrop") {
            let background = Bounds::centered(None, size(px(1400.), px(940.)), cx);
            cx.open_window(
                WindowOptions {
                    window_bounds: Some(WindowBounds::Windowed(background)),
                    titlebar: Some(gpui::TitlebarOptions {
                        title: Some("MioChat contrast fixture".into()),
                        ..Default::default()
                    }),
                    ..Default::default()
                },
                |_, cx| cx.new(|_| ContrastBackdrop),
            )
            .expect("contrast fixture window");
        }
        let bounds = Bounds::centered(None, size(px(1180.), px(780.)), cx);
        let primary_window = cx
            .open_window(
                WindowOptions {
                    window_bounds: Some(WindowBounds::Windowed(bounds)),
                    window_min_size: Some(size(px(960.), px(640.))),
                    titlebar: Some(gpui::TitlebarOptions {
                        title: Some("MioChat".into()),
                        appears_transparent: true,
                        ..Default::default()
                    }),
                    ..Default::default()
                },
                |window, cx| {
                    let app = cx.new(MioChatApp::new);
                    let close_app = app.downgrade();
                    window.on_window_should_close(cx, move |_, cx| {
                        close_app
                            .update(cx, |this, cx| this.request_close(cx))
                            .unwrap_or(true)
                    });
                    app.focus_handle(cx).focus(window, cx);
                    app
                },
            )
            .expect("cannot open MioChat window");
        let primary_id = gpui::AnyWindowHandle::from(primary_window).window_id();
        assert!(lifecycle.borrow_mut().register_primary(primary_id));
        cx.activate(true);
    });
}

#[cfg(test)]
mod direct_input_routing_tests {
    use super::*;
    #[test]
    fn actual_guess_pending_view_and_body_routes_remain_distinct() {
        use miochat_gpui::{
            direct_message::{DirectMessageForm, FormPurpose, FormSlot},
            guess::{GuessAction, GuessIntent},
        };
        let view = PendingSend::command("v".into(), "dm:bob".into(), "/guess", String::new());
        assert!(view.information && view.guess.is_none());
        for command in ["/guess start 2", "/guess 05"] {
            let p = PendingSend::command("g".into(), "dm:bob".into(), command, String::new());
            assert!(
                p.guess.is_some() && !p.information && p.tic.is_none() && p.community.is_none()
            );
        }
        let mut f = DirectMessageForm::new("alice".into(), "dm:bob".into(), 4);
        f.purpose = FormPurpose::Guess(GuessIntent {
            action: GuessAction::Guess,
            object_id: 8,
            room: "original-channel".into(),
            maximum: 10,
            requested: String::new(),
        });
        f.body = "05".into();
        assert_eq!(f.limit(FormSlot::Body), 24);
        assert!(routes_direct_input(
            Screen::Chat,
            Field::DirectBody,
            Some(&f),
            "alice",
            4
        ));
        assert!(!routes_direct_input(
            Screen::Chat,
            Field::DirectTarget,
            Some(&f),
            "alice",
            4
        ));
        assert!(!routes_direct_input(
            Screen::Chat,
            Field::DirectExtra,
            Some(&f),
            "alice",
            4
        ));
        assert!(f.command("alice", "lobby", 4).is_err());
        assert!(f.begin("guess".into()));
        assert!(!f.begin("again".into()));
        f.body = "6".into();
        assert!(!f.finish("guess", "alice", 4, true));
        let mut form = Some(f);
        let mut pending = Some(PendingSend::command(
            "g".into(),
            "dm:bob".into(),
            "/guess 05",
            String::new(),
        ));
        discard_chat_action_state(Screen::Password, &mut form, &mut pending);
        assert!(form.is_none() && pending.is_none());
    }
    #[test]
    fn actual_tic_form_slot_routes_and_pending_receipt_guard_are_enabled() {
        use miochat_gpui::{
            direct_message::{DirectMessageForm, FormPurpose, FormSlot},
            tic::{TicAction, TicIntent},
        };
        for command in ["/ttt bob", "/ttt accept 7", "/ttt 7 3"] {
            let p = PendingSend::command("id".into(), "lobby".into(), command, String::new());
            assert!(p.tic.is_some() && p.community.is_none() && !p.one_off && !p.information);
        }
        let mut f = DirectMessageForm::new("alice".into(), "dm:bob".into(), 8);
        f.purpose = FormPurpose::Tic(TicIntent {
            action: TicAction::Challenge,
            object_id: 0,
            cell: 0,
            room: "fixed-channel".into(),
            revision: None,
            requested: String::new(),
        });
        f.target = "opponent".into();
        assert_eq!(f.initial_slot(), FormSlot::Target);
        assert_eq!(f.next_slot(FormSlot::Target, false), FormSlot::Target);
        assert!(!routes_direct_input(
            Screen::Chat,
            Field::DirectBody,
            Some(&f),
            "alice",
            8
        ));
        assert!(f.limit(FormSlot::Target) > 0);
        assert_eq!(f.limit(FormSlot::Body), 0);
        assert!(routes_direct_input(
            Screen::Chat,
            Field::DirectTarget,
            Some(&f),
            "alice",
            8
        ));
        assert!(!routes_direct_input(
            Screen::Chat,
            Field::DirectExtra,
            Some(&f),
            "alice",
            8
        ));
        for screen in [
            Screen::Password,
            Screen::KeyBinding,
            Screen::Auth,
            Screen::Trust,
            Screen::Connect,
        ] {
            assert!(!routes_direct_input(
                screen,
                Field::DirectTarget,
                Some(&f),
                "alice",
                8
            ));
            let mut form = Some(f.clone());
            let mut pending = Some(PendingSend::command(
                "id".into(),
                "dm:bob".into(),
                "/ttt opponent",
                String::new(),
            ));
            discard_chat_action_state(screen, &mut form, &mut pending);
            assert!(form.is_none() && pending.is_none());
        }
    }
    #[test]
    fn actual_community_pending_and_event_field_routes_bind_the_frozen_intent() {
        use miochat_gpui::{
            community_mutation::{CommunityAction, CommunityIntent},
            direct_message::{DirectMessageForm, FormPurpose, FormSlot},
        };
        for command in [
            "/poll Q|A|B",
            "/vote 5 2",
            "/event E|2099-01-01 20:00|D",
            "/rsvp 5",
            "/unrsvp 5",
            "/close 5",
            "/comment 5 中文",
        ] {
            let p = PendingSend::command("write".into(), "dm:bob".into(), command, String::new());
            assert!(p.community.is_some());
            assert!(!p.information && !p.one_off && p.simple_action.is_none());
        }
        let mut f = DirectMessageForm::new("alice".into(), "dm:bob".into(), 7);
        f.purpose = FormPurpose::Community(CommunityIntent {
            action: CommunityAction::Event,
            object_id: 0,
            choice: 0,
            room: "channel-a".into(),
        });
        f.body = "中文介绍".into();
        for slot in [FormSlot::Target, FormSlot::Extra, FormSlot::Body] {
            assert_eq!(direct_slot(direct_field(slot)), Some(slot));
            assert!(routes_direct_input(
                Screen::Chat,
                direct_field(slot),
                Some(&f),
                "alice",
                7
            ));
        }
        for screen in [
            Screen::Password,
            Screen::KeyBinding,
            Screen::Auth,
            Screen::Trust,
            Screen::Connect,
        ] {
            for field in [Field::Password, Field::DirectExtra, Field::DirectBody] {
                assert!(!routes_direct_input(screen, field, Some(&f), "alice", 7));
            }
            let mut old = Some(f.clone());
            let mut pending = Some(PendingSend::command(
                "write".into(),
                "dm:bob".into(),
                "/event E|2099-01-01 20:00|D",
                String::new(),
            ));
            discard_chat_action_state(screen, &mut old, &mut pending);
            assert!(old.is_none() && pending.is_none());
        }
        f.purpose = FormPurpose::Community(CommunityIntent {
            action: CommunityAction::Comment,
            object_id: 5,
            choice: 0,
            room: "channel-a".into(),
        });
        assert!(!routes_direct_input(
            Screen::Chat,
            Field::DirectExtra,
            Some(&f),
            "alice",
            7
        ));
        assert!(!routes_direct_input(
            Screen::Chat,
            Field::DirectTarget,
            Some(&f),
            "alice",
            7
        ));
        assert!(routes_direct_input(
            Screen::Chat,
            Field::DirectBody,
            Some(&f),
            "alice",
            7
        ));
    }
    #[test]
    fn actual_simple_action_pending_and_credential_interrupt_share_safe_routes() {
        use miochat_gpui::{direct_message::DirectMessageForm, simple_actions::SimpleAction};
        for command in ["/me text", "/roll 20", "/coin", "/8ball question", "/shrug"] {
            let action = SimpleAction::parse(command).unwrap();
            let pending =
                PendingSend::command("send".into(), "private-room".into(), command, String::new());
            assert_eq!(pending.simple_action, Some(action));
            assert!(!pending.information && !pending.one_off);
            for screen in [
                Screen::Password,
                Screen::KeyBinding,
                Screen::Auth,
                Screen::Trust,
                Screen::Connect,
            ] {
                let mut form = DirectMessageForm::new("alice".into(), "private-room".into(), 7);
                form.purpose = miochat_gpui::direct_message::FormPurpose::Simple(action);
                form.body = "中文动作".into();
                form.begin("send".into());
                assert!(!routes_direct_input(
                    screen,
                    Field::Password,
                    Some(&form),
                    "alice",
                    7
                ));
                let mut form = Some(form);
                let mut pending = Some(PendingSend::command(
                    "send".into(),
                    "private-room".into(),
                    command,
                    String::new(),
                ));
                discard_chat_action_state(screen, &mut form, &mut pending);
                assert!(form.is_none() && pending.is_none());
            }
        }
    }
    #[test]
    fn actual_pending_command_constructor_enables_scoped_information_guard() {
        for command in [
            "/stats",
            "/time",
            "/ART",
            "/board",
            "/gallery",
            "/events",
            "/results",
            "/participants",
            "/daily",
            "/object 7",
            "/commands",
        ] {
            let pending =
                PendingSend::command("info-id".into(), "lobby".into(), command, String::new());
            assert!(pending.information && !pending.one_off);
            let response: Response = serde_json::from_str(
                r#"{"type":"action","room":"lobby","panel":{"kind":"time","title":"Time"}}"#,
            )
            .unwrap();
            assert!(!miochat_gpui::information::accepts_information_ack(
                &response,
                &pending.room,
                "dm:new"
            ));
            assert!(pending.body.is_empty()); // native menu does not consume the chat draft
        }
        assert!(
            !PendingSend::command("id".into(), "lobby".into(), "/msg bob body", String::new())
                .information
        );
    }
    #[test]
    fn actual_credential_screens_and_fields_never_route_to_hidden_dm_or_old_ack() {
        let mut form = Some(miochat_gpui::direct_message::DirectMessageForm::new(
            "alice".into(),
            "lobby".into(),
            7,
        ));
        form.as_mut().unwrap().body = "未发送中文私信".into();
        let mut pending = Some(PendingSend {
            edit_message: None,
            configuration: None,
            management: None,
            sanction: None,
            report: None,
            moderation: None,
            word: None,
            tic: None,
            guess: None,
            community: None,
            simple_action: None,
            information: false,
            one_off: true,
            one_off_target: "bob".into(),
            room: "lobby".into(),
            request_id: "old".into(),
            body: String::new(),
        });
        for screen in [
            Screen::Password,
            Screen::KeyBinding,
            Screen::Auth,
            Screen::Trust,
            Screen::Connect,
        ] {
            assert!(!routes_direct_input(
                screen,
                Field::DirectBody,
                form.as_ref(),
                "alice",
                7
            ));
        }
        for field in [
            Field::Password,
            Field::Confirm,
            Field::Invite,
            Field::KeyPassphrase,
        ] {
            assert!(!routes_direct_input(
                Screen::Chat,
                field,
                form.as_ref(),
                "alice",
                7
            ));
        }
        assert!(routes_direct_input(
            Screen::Chat,
            Field::DirectBody,
            form.as_ref(),
            "alice",
            7
        ));
        discard_chat_action_state(Screen::Password, &mut form, &mut pending);
        assert!(form.is_none() && pending.is_none()); // old action correlation fails
        let mut password = String::new();
        assert!(editing::replace(
            &mut password,
            &mut (0..0),
            &mut None,
            None,
            "synthetic-password",
            256
        ));
        assert_eq!(password, "synthetic-password");
        assert!(!routes_direct_input(
            Screen::Password,
            Field::Password,
            form.as_ref(),
            "alice",
            7
        ));
        assert!(form.is_none());
    }
    #[test]
    fn actual_secret_route_and_pending_constructor_never_capture_answer() {
        let intent = miochat_gpui::word::WordIntent {
            start: true,
            object_id: 0,
            room: "private".into(),
            requested: String::new(),
        };
        let mut form = miochat_gpui::word::WordStartForm::new(
            "alice".into(),
            "lobby".into(),
            7,
            intent.clone(),
        );
        form.replace("synthetic-answer".into());
        assert!(routes_word_input(
            Screen::Chat,
            Field::WordSecret,
            Some(&form),
            "alice",
            "lobby",
            7
        ));
        for screen in [
            Screen::Auth,
            Screen::Password,
            Screen::KeyBinding,
            Screen::Connect,
        ] {
            assert!(!routes_word_input(
                screen,
                Field::WordSecret,
                Some(&form),
                "alice",
                "lobby",
                7
            ));
        }
        assert!(!routes_word_input(
            Screen::Chat,
            Field::Password,
            Some(&form),
            "alice",
            "lobby",
            7
        ));
        assert!(!routes_word_input(
            Screen::Chat,
            Field::WordSecret,
            Some(&form),
            "alice",
            "other",
            7
        ));
        let mut transient = Some(form);
        let mut field = Field::WordSecret;
        discard_word_input(&mut transient, &mut field);
        assert!(transient.is_none());
        assert!(field == Field::Username);
        let pending = PendingSend::word_start("id".into(), "lobby".into(), intent);
        assert!(pending.body.is_empty());
        assert!(pending.one_off_target.is_empty());
        assert!(pending.word.unwrap().requested.is_empty());
        assert!(
            PendingSend::command("id".into(), "lobby".into(), "/word", String::new()).information
        );
    }
    #[test]
    fn actual_moderation_pending_constructor_and_state_invalidation() {
        for command in [
            "/admin",
            "/whois #7",
            "/ip",
            "/logins peer",
            "/ipusers 192.0.2.1",
        ] {
            let mut p = PendingSend::command("id".into(), "lobby".into(), command, String::new());
            assert!(!p.information && !p.one_off && p.word.is_none() && p.community.is_none());
            let t = p
                .moderation
                .as_mut()
                .expect("real constructor enables private guard");
            t.bind("owner", "lobby", 9, 4);
            assert_eq!(t.intent.command, command.split_whitespace().next().unwrap());
            let mut epoch = 4;
            let mut panel = Some(Panel {
                kind: "moderation".into(),
                ..Default::default()
            });
            miochat_gpui::moderation::invalidate(&mut epoch, &mut panel);
            assert_eq!(epoch, 5);
            assert!(panel.is_none());
            assert_eq!(t.epoch, 4);
        }
        assert!(
            PendingSend::command("id".into(), "lobby".into(), "/stats", String::new())
                .moderation
                .is_none()
        );
    }
    #[test]
    fn actual_report_constructor_and_credential_interrupt_isolate_the_workflow() {
        for command in ["/report 42 synthetic reason", "/resolve 7001 rejected"] {
            let p = PendingSend::command("id".into(), "lobby".into(), command, String::new());
            assert!(p.report.is_some() && p.moderation.is_none() && !p.one_off && !p.information);
            let mut f = miochat_gpui::direct_message::DirectMessageForm::new(
                "owner".into(),
                "lobby".into(),
                4,
            );
            f.purpose = miochat_gpui::direct_message::FormPurpose::Report(
                p.report.as_ref().unwrap().clone(),
            );
            let mut form = Some(f);
            let mut pending = Some(p);
            assert!(!routes_direct_input(
                Screen::Password,
                Field::Password,
                form.as_ref(),
                "owner",
                4
            ));
            discard_chat_action_state(Screen::Password, &mut form, &mut pending);
            assert!(form.is_none() && pending.is_none());
        }
    }
    #[test]
    fn actual_sanction_constructor_and_credential_interrupt_do_not_retain_moderation_input() {
        for command in [
            "/mute target 7d synthetic reason",
            "/unmute target",
            "/gmute target",
            "/ungmute target",
            "/ban target",
            "/unban target",
            "/banip 198.51.100.0/24 1h fixture --force",
            "/unbanip 198.51.100.0/24",
        ] {
            let p = PendingSend::command("id".into(), "lobby".into(), command, String::new());
            assert!(
                p.sanction.is_some()
                    && p.report.is_none()
                    && p.moderation.is_none()
                    && !p.one_off
                    && !p.information
            );
            assert!(p.sanction.as_ref().unwrap().self_ip_force.is_none());
            let mut f = miochat_gpui::direct_message::DirectMessageForm::new(
                "owner".into(),
                "lobby".into(),
                4,
            );
            f.purpose = miochat_gpui::direct_message::FormPurpose::Sanction(
                p.sanction.as_ref().unwrap().clone(),
            );
            let mut form = Some(f);
            let mut pending = Some(p);
            assert!(!routes_direct_input(
                Screen::Password,
                Field::Password,
                form.as_ref(),
                "owner",
                4
            ));
            discard_chat_action_state(Screen::Password, &mut form, &mut pending);
            assert!(form.is_none() && pending.is_none());
        }
    }
    #[test]
    fn actual_management_constructor_routes_fixed_intents_and_credential_cleanup() {
        for command in ["/slowmode 12", "/kick target", "/role target moderator"] {
            let p = PendingSend::command("id".into(), "lobby".into(), command, String::new());
            assert!(
                p.management.is_some()
                    && p.sanction.is_none()
                    && p.moderation.is_none()
                    && !p.one_off
                    && !p.information
            );
            let mut f = miochat_gpui::direct_message::DirectMessageForm::new(
                "owner".into(),
                "lobby".into(),
                4,
            );
            f.purpose = miochat_gpui::direct_message::FormPurpose::Management(
                p.management.as_ref().unwrap().clone(),
            );
            assert!(routes_direct_input(
                Screen::Chat,
                Field::DirectTarget,
                Some(&f),
                "owner",
                4
            ));
            assert!(!routes_direct_input(
                Screen::Password,
                Field::Password,
                Some(&f),
                "owner",
                4
            ));
            let mut form = Some(f);
            let mut pending = Some(p);
            discard_chat_action_state(Screen::Password, &mut form, &mut pending);
            assert!(form.is_none() && pending.is_none());
        }
    }

    #[test]
    fn actual_operations_constructor_private_gate_and_renderer_predicates() {
        use miochat_gpui::moderation::{private_panel_payload, requires_private_ticket};
        for command in ["/errors", "/health"] {
            let p = PendingSend::command("id".into(), "lobby".into(), command, String::new());
            assert!(p.moderation.is_some() && !p.information && !p.one_off);
            let r: Response =
                serde_json::from_str(r#"{"type":"action","request_id":"id","room":"lobby"}"#)
                    .unwrap();
            assert!(requires_private_ticket(
                &r,
                p.request_id == r.request_id && p.moderation.is_some()
            ));
        }
        let p = PendingSend::command("id".into(), "lobby".into(), "/stats", String::new());
        assert!(p.information && p.moderation.is_none());
        let mut r:Response=serde_json::from_str(r#"{"type":"action","request_id":"id","room":"lobby","panel":{"kind":"stats","title":"forged mixed","moderation":{"viewer":"owner","command":"/admin","requested":"","canonical":"","origin_scope":"lobby","window":0,"explanation":"private","audited":false}}}"#).unwrap();
        assert!(requires_private_ticket(&r, false));
        assert!(!private_panel_payload(r.panel.as_ref().unwrap()));
        assert!(!miochat_gpui::information::information_panel_payload(
            r.panel.as_ref().unwrap()
        ));
        assert!(!miochat_gpui::information::accepts_information_ack(
            &r, "lobby", "lobby"
        ));
        r.kind = "state".into();
        assert!(requires_private_ticket(&r, false));
    }

    #[test]
    fn actual_early_information_dispatch_rejects_mixed_effects_wrong_scope_and_old_ack() {
        let p = PendingSend::command("current".into(), "lobby".into(), "/stats", String::new());
        let r:Response=serde_json::from_str(r#"{"type":"action","request_id":"current","room":"lobby","panel":{"kind":"stats","title":"stats","statistics":{"version":"synthetic","users":1,"messages":0,"active_sessions":1,"minute_messages":0,"hour_limited":0,"heap_bytes":0,"goroutines":0,"uptime_seconds":0,"online_sessions":0}}}"#).unwrap();
        assert!(information_response_admitted(&r, Some(&p), "lobby"));
        for field in ["profile", "center", "preferences"] {
            let mut mixed = r.clone();
            match field {
                "profile" => mixed.profile = Some(Default::default()),
                "center" => mixed.center = Some(Default::default()),
                _ => mixed.preferences = Some(Default::default()),
            }
            assert!(
                !information_response_admitted(&mixed, Some(&p), "lobby"),
                "{field}"
            );
        }
        assert!(!information_response_admitted(&r, Some(&p), "dm:other"));
        assert!(!information_response_admitted(&r, None, "lobby"));
        let mut old = r.clone();
        old.request_id = "old".into();
        assert!(!information_response_admitted(&old, Some(&p), "lobby"));
        let unrelated = PendingSend::command(
            "current".into(),
            "lobby".into(),
            "/profile peer",
            String::new(),
        );
        assert!(!information_response_admitted(
            &r,
            Some(&unrelated),
            "lobby"
        ));
        for kind in ["error", "state"] {
            for field in ["profile", "center", "preferences"] {
                let mut mixed = r.clone();
                mixed.kind = kind.into();
                match field {
                    "profile" => mixed.profile = Some(Default::default()),
                    "center" => mixed.center = Some(Default::default()),
                    _ => mixed.preferences = Some(Default::default()),
                }
                assert!(!information_response_admitted(&mixed, Some(&p), "lobby"));
                mixed.panel = None;
                assert!(!information_response_admitted(&mixed, Some(&p), "lobby"));
            }
            let mut old = r.clone();
            old.kind = kind.into();
            old.request_id = "old".into();
            assert!(!information_response_admitted(&old, Some(&p), "lobby"));
        }
        let mut failure = r.clone();
        failure.kind = "error".into();
        failure.panel = None;
        assert!(information_response_admitted(&failure, Some(&p), "lobby"));
        let mut asynchronous = failure.clone();
        asynchronous.kind = "state".into();
        asynchronous.request_id.clear();
        asynchronous.preferences = Some(Default::default());
        assert!(information_response_admitted(
            &asynchronous,
            Some(&p),
            "lobby"
        ));
        let mut mixed_failure = failure.clone();
        mixed_failure.community_receipt = Some(Default::default());
        assert!(!information_response_admitted(
            &mixed_failure,
            Some(&p),
            "lobby"
        ));
    }

    #[test]
    fn actual_early_information_dispatch_preserves_bound_community_commit_only() {
        let p = PendingSend::command(
            "current".into(),
            "dm:peer".into(),
            "/poll question|a|b",
            String::new(),
        );
        assert!(p.community.is_some() && !p.information);
        let mut r:Response=serde_json::from_str(r#"{"type":"action","request_id":"current","room":"dm:peer","community_receipt":{"command":"/poll","object_id":7,"room":"channel-a","origin_scope":"dm:peer","committed":true},"panel":{"kind":"community","scope":"dm:peer","title":"committed; projection temporarily unavailable"}}"#).unwrap();
        assert!(information_response_admitted(&r, Some(&p), "dm:peer"));
        // Late confirmed original-scope mutation may finish, but later real action route never navigates back.
        assert!(information_response_admitted(&r, Some(&p), "lobby"));
        assert!(!information_response_admitted(&r, None, "dm:peer"));
        let info =
            PendingSend::command("current".into(), "dm:peer".into(), "/stats", String::new());
        assert!(!information_response_admitted(&r, Some(&info), "dm:peer"));
        let mut mixed = r.clone();
        mixed.profile = Some(Default::default());
        assert!(!information_response_admitted(&mixed, Some(&p), "dm:peer"));
        let mut mixed = r.clone();
        mixed.panel.as_mut().unwrap().statistics = Some(Default::default());
        assert!(!information_response_admitted(&mixed, Some(&p), "dm:peer"));
        r.community_receipt.as_mut().unwrap().command = "/vote".into();
        assert!(!information_response_admitted(&r, Some(&p), "dm:peer"));
    }
}

#[cfg(test)]
mod configuration_route_tests {
    use super::*;
    #[test]
    fn actual_configuration_constructor_and_early_gate_preserve_other_state() {
        use miochat_gpui::configuration::{ConfigurationChange, ConfigurationReceipt};
        let p = PendingSend::command(
            "setting".into(),
            "lobby".into(),
            "/config registration closed",
            String::new(),
        );
        assert!(p.configuration.is_some());
        assert!(!p.information);
        let good = Response {
            kind: "action".into(),
            request_id: "setting".into(),
            room: "lobby".into(),
            configuration_receipt: Some(ConfigurationReceipt {
                command: "/config".into(),
                requested_command: "/config registration closed".into(),
                actor: "owner".into(),
                origin_scope: "lobby".into(),
                changes: vec![ConfigurationChange {
                    key: "registration".into(),
                    value: "closed".into(),
                }],
                committed: true,
            }),
            ..Default::default()
        };
        assert!(configuration_response_admitted(&good, Some(&p), "owner"));
        assert!(!configuration_response_admitted(&good, None, "owner"));
        assert!(!configuration_response_admitted(&good, Some(&p), "other"));
        let mut wrong = good.clone();
        wrong.request_id = "old".into();
        assert!(!configuration_response_admitted(&wrong, Some(&p), "owner"));
        wrong = good.clone();
        wrong.room = "other".into();
        assert!(!configuration_response_admitted(&wrong, Some(&p), "owner"));
        for kind in ["action", "error", "state"] {
            wrong = good.clone();
            wrong.kind = kind.into();
            wrong.preferences = Some(Default::default());
            assert!(!configuration_response_admitted(&wrong, Some(&p), "owner"));
            wrong.configuration_receipt = None;
            assert!(!configuration_response_admitted(&wrong, Some(&p), "owner"));
        }
        let mut error = Response {
            kind: "error".into(),
            request_id: "setting".into(),
            ..Default::default()
        };
        assert!(configuration_response_admitted(&error, Some(&p), "owner"));
        error.management_receipt = Some(Default::default());
        assert!(!configuration_response_admitted(&error, Some(&p), "owner"));
        let asynchronous = Response {
            kind: "state".into(),
            preferences: Some(Default::default()),
            ..Default::default()
        };
        assert!(configuration_response_admitted(
            &asynchronous,
            Some(&p),
            "owner"
        ));
        // A good confirmation is consumed before the global effects by the real
        // handler; it cannot return to the former scope or clear a separate draft.
        let mut draft = "preserved current draft".to_string();
        assert!(!miochat_gpui::direct_message::clear_unchanged_submission(
            &mut draft, &p.body
        ));
        assert_eq!(draft, "preserved current draft");
    }
}
