# MioChat

**在字符之间相遇。**

MioChat 是一个通过标准 SSH 客户端使用的终端社区，支持 UTF-8 中文、鼠标操作和 ASCII 界面。

- 频道、私聊、指定回复、@、消息反应、编辑与撤回。
- 通知、联系人、投票、小游戏、长帖和 ASCII 画板。
- 多设备公钥登录、设备管理和可关闭的绑定提醒。
- 管理权限、举报、封禁、会话管理及一致性备份。
- 键鼠双入口、可见输入焦点、多主题和长期在线。
- 可选 GPUI 原生桌面客户端（SSH 子系统，保留原终端客户端兼容性）。

## 配套原生客户端候选

正常启动自动连接 `chat.mio.blue:22`，验证主机密钥和当前 GUI 协议后，
再明确提交应用账户密码；传输选项在高级设置。需要配套服务端升级，
不是现有普通 TUI 服务自动具有 GUI 协议。详见 [升级、备份与兼容边界](docs/PAIRED_UPGRADE.md)。
**恢复备份，当前暂停，尚未整版通过。** 已有部分真实自动化通过，但当前原生协议集成仍有
会话健康检查失败。详见 [验证状态、已过范围与发布阻断](docs/VALIDATION_STATUS.md)。
本次仅保存到 fork 的独立备份分支，不创建上游 PR、不启动 CI，不把源码备份当成完成交付。

## 快速开始

从 [Releases](https://github.com/mio-qwq/MioChat/releases/latest) 下载对应平台的压缩包并解压。Linux 下运行：

```sh
./miochat -listen 127.0.0.1:2222 -state .state
```

另开一个终端连接：

```sh
ssh -t -p 2222 chat@127.0.0.1
```

Windows 使用 `miochat.exe`。首次进入可注册聊天室账号；它与操作系统账号独立。状态目录保存数据库和 SSH 主机密钥，请持久保留。

终端至少需要 40×24，推荐 100×30，并使用包含中文字形的字体。

## 从源码构建

需要 Go 1.27.3 或更新版本。

```sh
git clone https://github.com/mio-qwq/MioChat.git
cd MioChat
go build -trimpath -o miochat .
go test ./...
```

Windows 可将输出文件名改为 `miochat.exe`。

原生桌面客户端位于 [`gpui-client`](gpui-client/README.md)。它使用同一
SSH 端口的 `miochat-gui-v1` 子系统，不开放额外网络端口；服务器仍完整
支持上述标准终端连接。

使用方式、设备密钥和服务部署见 [使用指南](docs/USAGE.md)。签名与发布校验见 [发布说明](docs/RELEASE.md)，安全问题见 [SECURITY.md](SECURITY.md)。

## 许可证

[MIT](LICENSE)。依赖许可证见 [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES)。

## Optional dot adapter

The complete default-off Python adapter is included in `integrations/dot-adapter/`. See [package, synthetic tests and deployment limits](docs/DOT_ADAPTER.md). Real dot host/authentication/subscription setup remains separate.
