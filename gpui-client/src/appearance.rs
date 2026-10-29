use gpui::{Hsla, Window, WindowBackgroundAppearance, rgb, rgba};
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum Appearance {
    #[default]
    Keel,
    Frosted,
    Transparent,
}
impl Appearance {
    pub fn label(self) -> &'static str {
        match self {
            Self::Keel => "默认",
            Self::Frosted => "毛玻璃",
            Self::Transparent => "透明",
        }
    }
    pub fn canvas(self) -> Hsla {
        match self {
            Self::Keel => rgb(0xe9e6df).into(),
            Self::Frosted => rgba(0xe9e6dfdf).into(),
            Self::Transparent => rgba(0xe9e6dfe2).into(),
        }
    }
    pub fn surface(self) -> Hsla {
        match self {
            Self::Keel => rgb(0xf6f4ef).into(),
            Self::Frosted => rgba(0xf6f4efdc).into(),
            Self::Transparent => rgba(0xf6f4efe0).into(),
        }
    }
    pub fn apply(self, window: &Window) -> Result<(), String> {
        let material = match self {
            Self::Keel => WindowBackgroundAppearance::Opaque,
            Self::Frosted => WindowBackgroundAppearance::Blurred,
            Self::Transparent => WindowBackgroundAppearance::Transparent,
        };
        window.set_background_appearance(material);
        if let Err(reason) = native_apply(self, window) {
            window.set_background_appearance(WindowBackgroundAppearance::Opaque);
            return Err(format!("{reason}；已回退 默认"));
        }
        Ok(())
    }
}
#[cfg(not(windows))]
fn native_apply(mode: Appearance, _window: &Window) -> Result<(), String> {
    if mode == Appearance::Keel {
        Ok(())
    } else {
        Err("当前平台材质尚未验证".into())
    }
}
#[cfg(windows)]
fn native_apply(mode: Appearance, window: &Window) -> Result<(), String> {
    use raw_window_handle::{HasWindowHandle, RawWindowHandle};
    use std::ffi::c_void;
    #[repr(C)]
    struct HighContrast {
        size: u32,
        flags: u32,
        scheme: *mut u16,
    }
    #[repr(C)]
    struct Accent {
        state: u32,
        flags: u32,
        color: u32,
        animation: u32,
    }
    #[repr(C)]
    struct Composition {
        attribute: u32,
        data: *mut c_void,
        size: usize,
    }
    #[link(name = "user32")]
    unsafe extern "system" {
        fn SystemParametersInfoW(action: u32, param: u32, data: *mut c_void, flags: u32) -> i32;
    }
    #[link(name = "dwmapi")]
    unsafe extern "system" {
        fn DwmIsCompositionEnabled(enabled: *mut i32) -> i32;
    }
    #[link(name = "kernel32")]
    unsafe extern "system" {
        fn GetModuleHandleW(name: *const u16) -> *mut c_void;
        fn GetProcAddress(module: *mut c_void, name: *const u8) -> *mut c_void;
    }
    #[link(name = "advapi32")]
    unsafe extern "system" {
        fn RegGetValueW(
            key: isize,
            subkey: *const u16,
            value: *const u16,
            flags: u32,
            kind: *mut u32,
            data: *mut c_void,
            size: *mut u32,
        ) -> i32;
    }
    let handle = HasWindowHandle::window_handle(window).map_err(|_| "无法取得本应用窗口句柄")?;
    let RawWindowHandle::Win32(handle) = handle.as_raw() else {
        return Err("不是 Windows 原生窗口".into());
    };
    unsafe {
        if mode != Appearance::Keel {
            let mut enabled = 0;
            if DwmIsCompositionEnabled(&mut enabled) < 0 || enabled == 0 {
                return Err("桌面合成不可用".into());
            }
            let mut contrast = HighContrast {
                size: std::mem::size_of::<HighContrast>() as u32,
                flags: 0,
                scheme: std::ptr::null_mut(),
            };
            if SystemParametersInfoW(0x42, contrast.size, &mut contrast as *mut _ as _, 0) != 0
                && contrast.flags & 1 != 0
            {
                return Err("系统启用了高对比度".into());
            }
            let sub: Vec<u16> =
                "Software\\Microsoft\\Windows\\CurrentVersion\\Themes\\Personalize\0"
                    .encode_utf16()
                    .collect();
            let name: Vec<u16> = "EnableTransparency\0".encode_utf16().collect();
            let mut value = 1_u32;
            let mut bytes = 4;
            if RegGetValueW(
                -2147483647,
                sub.as_ptr(),
                name.as_ptr(),
                0x10,
                std::ptr::null_mut(),
                &mut value as *mut _ as _,
                &mut bytes,
            ) == 0
                && value == 0
            {
                return Err("系统关闭了透明效果".into());
            }
        }
        let module: Vec<u16> = "user32.dll\0".encode_utf16().collect();
        let address = GetProcAddress(
            GetModuleHandleW(module.as_ptr()),
            c"SetWindowCompositionAttribute".as_ptr() as _,
        );
        if address.is_null() {
            return if mode == Appearance::Keel {
                Ok(())
            } else {
                Err("系统没有可用的原生材质接口".into())
            };
        }
        let apply: unsafe extern "system" fn(isize, *mut Composition) -> i32 =
            std::mem::transmute(address);
        let mut accent = Accent {
            state: match mode {
                Appearance::Keel => 0,
                Appearance::Transparent => 2,
                Appearance::Frosted => 4,
            },
            flags: if mode == Appearance::Frosted { 0 } else { 2 },
            color: if mode == Appearance::Frosted {
                0x20e9e6df
            } else {
                0
            },
            animation: 0,
        };
        let mut attribute = Composition {
            attribute: 19,
            data: &mut accent as *mut _ as _,
            size: std::mem::size_of::<Accent>(),
        };
        if apply(handle.hwnd.get(), &mut attribute) == 0 {
            return Err("Windows 拒绝应用窗口材质".into());
        }
    }
    Ok(())
}
