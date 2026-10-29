# 终端与原生客户端命令参考

本表保留139个原命令目录入口和9个补充交互命令，仅为公开功能参考。
目录数量不等于独立功能数量，也不代表测试通过或可发布。
当前源码仍有真实回归失败，见[验证状态](../docs/VALIDATION_STATUS.md)。

原生 GUI 需要配套 `miochat-gui-v1` 服务端；原标准 SSH+PTY 路径仍保留。
权限标记只是入口说明，实际请求始终由服务端当前权限决定。
非命令交互和平台限制见[交互范围](INTERACTION_PARITY.md)。
本恢复备份不含内部逐片审查日志或验收台账，且不触发 CI 或部署。

| 命令 | 参数 | 说明 | 分组 | 管理入口 |
| --- | --- | --- | --- | --- |
| /canceljob |  | 取消当前连接的后台维护作业 | OPERATIONS | 是 |
| /draft-conflicts |  | 找回本人的冲突草稿 | ACCOUNT | 否 |
| /keys |  | 我的设备密钥 | ACCOUNT | 否 |
| /key-add |  | 确认绑定当前连接的公钥 | ACCOUNT | 否 |
| /key-guide |  | 为其它设备添加密钥、选择算法 | ACCOUNT | 否 |
| /key-remove | 密钥编号 | 确认撤销自己的设备密钥 | ACCOUNT | 否 |
| /key-rename | 密钥编号 新名称 | 重命名自己的设备密钥 | ACCOUNT | 否 |
| /ipusers | IP | 同IP成功登录账户（仅线索） | MODERATION | 是 |
| /rooms |  | 频道列表 | CHANNELS | 否 |
| /join | 频道 | 切换频道 | CHANNELS | 否 |
| /create | 频道 [private] | 创建公开/私密频道 | CHANNELS | 否 |
| /roominfo |  | 频道介绍、规则与公告 | CHANNELS | 否 |
| /roomset | 设置 值 | 设置 private/readonly/category/rules/announcement/slow/transfer | CHANNELS | 否 |
| /topic | 话题 | 设置频道话题 | CHANNELS | 否 |
| /members |  | 频道成员与版主 | CHANNELS | 否 |
| /invite | 用户 | 邀请加入当前频道 | CHANNELS | 否 |
| /remove | 用户 | 移除频道成员 | CHANNELS | 否 |
| /roommod | 用户 | 设置频道版主 | CHANNELS | 否 |
| /unmod | 用户 | 取消频道版主 | CHANNELS | 否 |
| /leave |  | 离开频道 | CHANNELS | 否 |
| /archive |  | 归档当前频道 | CHANNELS | 否 |
| /unarchive |  | 恢复归档频道 | CHANNELS | 否 |
| /archives |  | 包含归档的频道列表 | CHANNELS | 否 |
| /roompin | on/off | 个人频道置顶 | CHANNELS | 否 |
| /subscribe | on/off | 订阅频道未读 | CHANNELS | 否 |
| /roommute | on/off | 频道通知静音 | CHANNELS | 否 |
| /dm | 用户 | 打开私聊，支持离线消息 | MESSAGES | 否 |
| /msg | 用户 内容 | 发送私信 | MESSAGES | 否 |
| /back |  | 返回大厅并清除离开状态 | MESSAGES | 否 |
| /reply | 编号 [内容] | 选中或发送指定回复 | MESSAGES | 否 |
| /cancelreply |  | 取消当前回复 | MESSAGES | 否 |
| /mention | 用户 | 把 @用户 插入输入框 | MESSAGES | 否 |
| /recall | 编号 | 撤回自己的消息（5分钟） | MESSAGES | 否 |
| /edit | 编号 内容 | 编辑自己的消息（5分钟） | MESSAGES | 否 |
| /react | 编号 like/laugh/question/heart/wow/sad | 切换消息反应 | MESSAGES | 否 |
| /history | [数量] | 最近记录（最大200） | HISTORY | 否 |
| /older |  | 加载更早记录 | HISTORY | 否 |
| /search | 关键词 [from:用户] [after:日期] [before:日期] | 全文筛选当前会话 | HISTORY | 否 |
| /jump | 编号 | 跳转消息所在会话 | HISTORY | 否 |
| /read | 编号 | 展开阅读消息 | HISTORY | 否 |
| /bookmark | 编号 | 收藏消息 | HISTORY | 否 |
| /unbookmark | 编号 | 取消收藏 | HISTORY | 否 |
| /bookmarks |  | 查看收藏 | HISTORY | 否 |
| /pin | 编号 | 频道置顶（管理者） | HISTORY | 否 |
| /unpin | 编号 | 取消置顶 | HISTORY | 否 |
| /pins |  | 频道置顶列表 | HISTORY | 否 |
| /clear |  | 仅清理自己的屏幕 | HISTORY | 否 |
| /notifications |  | 提及、回复、私信、戳与好友通知 | SOCIAL | 否 |
| /notifyclear |  | 全部通知标为已读（不删除） | SOCIAL | 否 |
| /poke | 用户 | 戳一戳，30秒冷却 | SOCIAL | 否 |
| /profile | [用户] | 用户卡片 | SOCIAL | 否 |
| /who |  | 在线用户卡片列表 | SOCIAL | 否 |
| /contacts |  | 好友、关注、申请与最近会话 | SOCIAL | 否 |
| /recent |  | 最近私聊 | SOCIAL | 否 |
| /follow | 用户 | 关注 | SOCIAL | 否 |
| /unfollow | 用户 | 取消关注 | SOCIAL | 否 |
| /following |  | 关注列表 | SOCIAL | 否 |
| /friend | 用户 | 申请好友 | SOCIAL | 否 |
| /accept | 用户 | 接受好友申请 | SOCIAL | 否 |
| /decline | 用户 | 拒绝好友申请 | SOCIAL | 否 |
| /unfriend | 用户 | 解除双向好友关系 | SOCIAL | 否 |
| /friends |  | 好友列表 | SOCIAL | 否 |
| /note | 用户 备注 | 设置只对自己可见的备注 | SOCIAL | 否 |
| /unnote | 用户 | 删除私有备注 | SOCIAL | 否 |
| /ignore | 用户 | 持久屏蔽 | SOCIAL | 否 |
| /unignore | 用户 | 取消屏蔽 | SOCIAL | 否 |
| /bio | 简介 | 修改简介 | ACCOUNT | 否 |
| /status | 状态 | 修改状态文字 | ACCOUNT | 否 |
| /afk | [原因] | 暂时离开 | ACCOUNT | 否 |
| /theme | aurora/sakura/amber/mono/light/contrast | 配色主题 | ACCOUNT | 否 |
| /pref | 设置 on/off | poke/motion/bell/dnd/compact/ascii/welcome/contrast | ACCOUNT | 否 |
| /dnd | on/off | 勿扰模式 | ACCOUNT | 否 |
| /avatar | camp/cat/robot/planet/flower/ident | ASCII头像 | ACCOUNT | 否 |
| /badges | [用户] | 活动成就 | ACCOUNT | 否 |
| /passwd |  | 隐藏输入的改密表单 | ACCOUNT | 否 |
| /logins | [用户] | 自己的登录历史；管理员可查他人 | ACCOUNT | 否 |
| /sessions | [用户] | 会话管理 | ACCOUNT | 否 |
| /disconnect | 会话ID | 断开有权管理的一个会话 | ACCOUNT | 否 |
| /logoutothers |  | 退出自己的其他设备 | ACCOUNT | 否 |
| /logout |  | 退出当前连接 | ACCOUNT | 否 |
| /poll | 问题\|选项1\|选项2 | 创建频道投票 | COMMUNITY | 否 |
| /vote | 编号 选项 | 投票或修改选择 | COMMUNITY | 否 |
| /results | 编号 | 投票结果 | COMMUNITY | 否 |
| /board |  | 长帖留言板 | COMMUNITY | 否 |
| /post | 标题 或 编号 | 编写长帖或阅读旧帖 | COMMUNITY | 否 |
| /comment | 编号 内容 | 评论社区条目 | COMMUNITY | 否 |
| /draw | 标题 | ASCII画板（F2预览/F4发布） | COMMUNITY | 否 |
| /gallery |  | ASCII作品展示 | COMMUNITY | 否 |
| /event | 标题\|YYYY-MM-DD HH:MM\|介绍 | 创建活动（北京时间） | COMMUNITY | 否 |
| /events |  | 频道活动 | COMMUNITY | 否 |
| /rsvp | 编号 | 报名活动 | COMMUNITY | 否 |
| /unrsvp | 编号 | 取消报名 | COMMUNITY | 否 |
| /participants | 编号 | 参与名单 | COMMUNITY | 否 |
| /close | 编号 | 结束自己的投票/活动/对局 | COMMUNITY | 否 |
| /daily |  | 每日讨论话题 | COMMUNITY | 否 |
| /ttt | 用户 或 accept 编号 或 编号 格子 | 邀请/接受/进行井字棋 | PLAY | 否 |
| /guess | start [上限] 或 数字 | 猜数字 | PLAY | 否 |
| /word | start 答案 或 猜测 | 猜词 | PLAY | 否 |
| /me | 动作 | 动作消息 | PLAY | 否 |
| /roll | [面数] | 掷骰子 | PLAY | 否 |
| /coin |  | 抛硬币 | PLAY | 否 |
| /8ball | 问题 | 神奇八号球 | PLAY | 否 |
| /shrug |  | 耸肩 | PLAY | 否 |
| /art |  | ASCII营地 | PLAY | 否 |
| /report | 消息编号 原因 | 向管理员举报该条内容 | MODERATION | 否 |
| /mute | 用户 期限 [原因] | 当前频道禁言 | MODERATION | 否 |
| /unmute | 用户 | 解除频道禁言 | MODERATION | 否 |
| /slowmode | 秒数 | 频道慢速，0关闭 | MODERATION | 否 |
| /admin |  | 管理控制台 | MODERATION | 是 |
| /whois | 用户/UID | 账户详情 | MODERATION | 是 |
| /ip | 用户 | 近期登录IP | MODERATION | 是 |
| /kick | 用户 | 断开账户连接 | MODERATION | 是 |
| /ban | 用户 [期限] [原因] | 封号，默认永久 | MODERATION | 是 |
| /unban | 用户 | 解封账户 | MODERATION | 是 |
| /banip | IP/CIDR 期限 [原因] | 仅聊天室封IP | MODERATION | 是 |
| /unbanip | IP/CIDR | 解封IP | MODERATION | 是 |
| /bans |  | 封禁与禁言列表 | MODERATION | 是 |
| /gmute | 用户 期限 [原因] | 全站禁言 | MODERATION | 是 |
| /ungmute | 用户 | 解除全站禁言 | MODERATION | 是 |
| /audit |  | 管理员操作日志 | MODERATION | 是 |
| /recalls |  | 公开频道撤回目录 | MODERATION | 是 |
| /revisions | 消息编号 | 授权查看修订/撤回原文 | MODERATION | 否 |
| /reports |  | 举报队列 | MODERATION | 是 |
| /reportview | 举报编号 | 查看主动举报的一条消息 | MODERATION | 是 |
| /resolve | 举报编号 resolved/rejected | 处理举报 | MODERATION | 是 |
| /role | 用户 admin/moderator/member | 站长分配角色 | MODERATION | 是 |
| /resetpass | 用户 | 临时密码重置并强制改密 | MODERATION | 是 |
| /config | 设置 值 | 注册/配额/维护/永久保留配置 | OPERATIONS | 是 |
| /invitecode | [期限] [次数] | 生成注册邀请码 | OPERATIONS | 是 |
| /maintenance | on/off [公告] | 维护模式 | OPERATIONS | 是 |
| /backup |  | 创建一致性备份 | OPERATIONS | 是 |
| /check |  | 数据库与配置校验 | OPERATIONS | 是 |
| /errors |  | 异常与限流记录 | OPERATIONS | 是 |
| /health |  | 运行资源看板 | OPERATIONS | 是 |
| /stats |  | 公开运行统计 | INFO | 否 |
| /time |  | 北京时间 | INFO | 否 |
| /help |  | 可搜索、可点击的命令指南 | INFO | 否 |
| /menu |  | 功能菜单 | INFO | 否 |
| /quit |  | 退出聊天室 | INFO | 否 |
| /exit | 见上游源码 | 未列入 commandCatalog 的别名/交互子命令 | ADDITIONAL | 否 |
| /draft-retry | 见上游源码 | 未列入 commandCatalog 的别名/交互子命令 | ADDITIONAL | 否 |
| /draft-keep | 见上游源码 | 未列入 commandCatalog 的别名/交互子命令 | ADDITIONAL | 否 |
| /draft-reload | 见上游源码 | 未列入 commandCatalog 的别名/交互子命令 | ADDITIONAL | 否 |
| /draft-restore | 见上游源码 | 未列入 commandCatalog 的别名/交互子命令 | ADDITIONAL | 否 |
| /key-bind | 见上游源码 | 未列入 commandCatalog 的别名/交互子命令 | ADDITIONAL | 否 |
| /key-remove-confirm | 见上游源码 | 未列入 commandCatalog 的别名/交互子命令 | ADDITIONAL | 否 |
| /commands |  | 原TUI菜单别名 | ADDITIONAL | 否 |
| /object | 编号 | 原TUI社区详情子入口 | ADDITIONAL | 否 |
