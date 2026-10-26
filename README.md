# MioChat

**在字符之间相遇。**

MioChat 是一个通过标准 SSH 客户端使用的终端社区，支持 UTF-8 中文、鼠标操作和 ASCII 界面。

- 频道、私聊、指定回复、@、消息反应、编辑与撤回。
- 通知、联系人、投票、小游戏、长帖和 ASCII 画板。
- 多设备公钥登录、设备管理和可关闭的绑定提醒。
- 管理权限、举报、封禁、会话管理及一致性备份。
- 键鼠双入口、可见输入焦点、多主题和长期在线。

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

使用方式、设备密钥和服务部署见 [使用指南](docs/USAGE.md)。签名与发布校验见 [发布说明](docs/RELEASE.md)，安全问题见 [SECURITY.md](SECURITY.md)。

## 许可证

[MIT](LICENSE)。依赖许可证见 [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES)。
