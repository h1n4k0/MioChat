use super::*;

fn transport_field(field: Field) -> bool {
    matches!(
        field,
        Field::Host | Field::Port | Field::SshUser | Field::Identity | Field::KeyPassphrase
    )
}
fn input_allowed(
    screen: Screen,
    advanced: bool,
    connecting: bool,
    session: bool,
    ready: bool,
    field: Field,
) -> bool {
    if transport_field(field) {
        return screen == Screen::Connect && advanced && !connecting && !session;
    }
    match screen {
        Screen::Connect => !advanced && matches!(field, Field::Username | Field::Password),
        Screen::Trust => false,
        Screen::Auth => {
            ready
                && matches!(
                    field,
                    Field::Username | Field::Password | Field::Confirm | Field::Invite
                )
        }
        _ => true,
    }
}
impl MioChatApp {
    pub(super) fn clear_login_secrets(&mut self) {
        self.password.clear();
        self.confirm.clear();
        self.invite.clear();
        self.marked_range = None;
        self.selection = 0..0;
    }
    pub(super) fn connection_field_enabled(&self, field: Field) -> bool {
        input_allowed(
            self.screen,
            self.advanced_connection,
            self.connecting,
            self.session.is_some(),
            self.login_gate.can_submit(self.generation) && self.pending_key_login.is_none(),
            field,
        )
    }
    fn toggle_connection_advanced(&mut self, generation: u64, cx: &mut Context<Self>) {
        if generation != self.generation || !matches!(self.screen, Screen::Connect | Screen::Auth) {
            return;
        }
        let advanced = !self.advanced_connection;
        self.cancel_connect(cx);
        self.mock_mode = false;
        self.fixture_driver = None;
        self.advanced_connection = advanced;
        self.field = if advanced {
            Field::Host
        } else {
            Field::Username
        };
        self.reset_selection();
        cx.notify();
    }
    pub(super) fn advanced_connection_button(&self, cx: &mut Context<Self>) -> Stateful<Div> {
        let generation = self.generation;
        Self::button(
            "advanced-connection",
            if self.advanced_connection {
                "返回账户登录"
            } else {
                "高级连接设置"
            },
        )
        .on_click(
            cx.listener(move |this, _, _, cx| this.toggle_connection_advanced(generation, cx)),
        )
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn hidden_transport_fields_and_unready_auth_inputs_are_not_editable() {
        for field in [
            Field::Host,
            Field::Port,
            Field::SshUser,
            Field::Identity,
            Field::KeyPassphrase,
        ] {
            assert!(!input_allowed(
                Screen::Connect,
                false,
                false,
                false,
                false,
                field
            ));
            assert!(input_allowed(
                Screen::Connect,
                true,
                false,
                false,
                false,
                field
            ));
            assert!(!input_allowed(
                Screen::Connect,
                true,
                true,
                false,
                false,
                field
            ));
            assert!(!input_allowed(
                Screen::Connect,
                true,
                false,
                true,
                false,
                field
            ));
            assert!(!input_allowed(Screen::Auth, true, false, true, true, field));
        }
        assert!(input_allowed(
            Screen::Connect,
            false,
            true,
            false,
            false,
            Field::Password
        ));
        assert!(!input_allowed(
            Screen::Trust,
            false,
            false,
            false,
            false,
            Field::Password
        ));
        assert!(!input_allowed(
            Screen::Auth,
            false,
            false,
            true,
            false,
            Field::Password
        ));
        assert!(input_allowed(
            Screen::Auth,
            false,
            false,
            true,
            true,
            Field::Password
        ));
    }
}
