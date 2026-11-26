# slackctl

A fast, single-binary Slack CLI for AI agents and humans. Written in Go.

Website: https://blog.cluas.eu.org/slackctl/

## Features

- **Single binary** — no Node.js, no runtime dependencies; macOS, Linux & Windows (amd64/arm64)
- **Auto-auth** — extracts tokens from Slack Desktop, Chrome, Brave, Firefox (cross-platform: macOS Keychain, Linux Secret Service, Windows DPAPI)
- **Browser auth** — works with `xoxc` + `xoxd` cookie (no bot token required)
- **AI-friendly** — JSON output, designed for agent tool use
- **Full Slack API** — messages, channels, users, search, canvas, reactions

## Install

### Homebrew (macOS / Linux)

```bash
brew tap cluas/tap
brew install slackctl
```

### Scoop (Windows)

```powershell
scoop bucket add cluas https://github.com/cluas/scoop-bucket
scoop install slackctl
```

### Prebuilt binaries (macOS / Linux / Windows)

Download an archive for your OS/arch from the [releases page](https://github.com/cluas/slackctl/releases),
extract it, and put `slackctl` (or `slackctl.exe` on Windows) on your `$PATH`.

### Go install

```bash
go install github.com/cluas/slackctl/cmd/slackctl@latest
```

### From source

```bash
git clone https://github.com/cluas/slackctl.git
cd slackctl
make build            # binary for the current platform
make cross            # cross-compile for all platforms into dist/
```

Manual single-target cross-compile (no Make):

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o slackctl.exe ./cmd/slackctl/
```

## Quick start

```bash
# Import credentials from Slack Desktop
slackctl auth import-desktop

# Or from browser
slackctl auth import-chrome
slackctl auth import-firefox
slackctl auth import-brave

# Or add manually
slackctl auth add --workspace-url https://myteam.slack.com --xoxc xoxc-... --xoxd xoxd-...

# Verify
slackctl auth test
```

## Usage

```
slackctl
├── auth
│   ├── whoami              # Show configured workspaces
│   ├── test                # Verify credentials (auth.test)
│   ├── add                 # Add token or xoxc/xoxd manually
│   ├── import-desktop      # Extract from Slack Desktop app
│   ├── import-chrome       # Extract from Chrome (macOS)
│   ├── import-brave        # Extract from Brave (macOS)
│   ├── import-firefox      # Extract from Firefox
│   ├── parse-curl          # Extract from cURL command (stdin)
│   ├── set-default         # Set default workspace
│   └── remove              # Remove a workspace
├── message
│   ├── get <target> [ts]   # Fetch a single message
│   ├── list <target>       # List channel history or thread
│   ├── send <target> <text># Post a message
│   ├── edit <target> <ts>  # Edit a message
│   ├── delete <target> [ts]# Delete a message
│   ├── react add|remove    # Manage reactions
│   └── saved               # List saved-for-later messages (Later)
├── canvas
│   └── get <url-or-id>     # Fetch canvas as Markdown
├── search
│   ├── messages <query>    # Search messages
│   └── files <query>       # Search files
├── user
│   ├── list                # List workspace users
│   └── get <id-or-handle>  # Get user info
├── channel
│   ├── list                # List conversations
│   ├── new <name>          # Create a channel
│   ├── invite <ch> <users> # Invite users
│   └── mark <ch> <ts>      # Mark as read
└── listen                  # Stream real-time events as JSON Lines
```

### listen: real-time event stream

`slackctl listen` opens a real-time websocket via `rtm.connect` (works with
browser `xoxc` + `d` cookie auth) and prints one
JSON object per line on stdout. Diagnostics go to stderr. It is read-only:
it never sends messages, marks anything read, or reacts.

Not supported on Enterprise Grid org-level sessions
(`*.enterprise.slack.com`): Slack refuses `rtm.connect` there with
`enterprise_is_restricted`, and `listen` exits with a `_connect_error` line.

How it works:

1. Calls `rtm.connect` to get a one-time `wss://` URL and your own identity.
2. Dials the websocket with the same TLS fingerprint, `d` cookie and
   User-Agent used for API calls, so browser-session credentials work.
3. Prints `_connected` on the first `hello`, then one line per event that
   passes the filters below, until you press Ctrl-C (exit 0).

```bash
# Every new message in conversations you are in
slackctl listen

# Pick a workspace when you have several
slackctl listen --workspace myteam

# Only DMs, group DMs, and messages that @-mention you
slackctl listen --mentions-or-dms | jq -r --unbuffered \
  'select(.type=="message") | "\(.channel) \(.user): \(.text)"'

# Several event types; edits/deletes and bot messages filtered as you like
slackctl listen --types message,reaction_added --all-subtypes --include-bots=false

# Everything Slack sends (empty --types), saved for later analysis
slackctl listen --types "" > events.jsonl

# Extract button / link URLs from bot messages (attachments + blocks)
slackctl listen | jq -r --unbuffered \
  '.attachments[]?.actions[]?.url // empty, .blocks[]?.elements[]?.url // empty'
```

| Flag | Default | Description |
|---|---|---|
| `--types` | `message` | Comma-separated event types to emit; empty string = all |
| `--mentions-or-dms` | `false` | Only DMs (`D…` / `channel_type: im`), group DMs (`is_mpim`, resolved via `conversations.info` and cached), or text that mentions you exactly (`<@your-id>` or `<@your-id\|label>`) |
| `--include-bots` | `true` | Include bot messages (`--include-bots=false` to drop). A message counts as a bot message if it has a `bot_id` or subtype `bot_message`, so app messages with an empty subtype are dropped too |
| `--all-subtypes` | `false` | Also emit `message_changed`, `message_deleted`, joins, etc. |

All filtering happens locally: Slack streams every event your account can
see over the websocket, and `slackctl` decides what to print. RTM has no
per-channel subscription, so filters reduce output, not network traffic; in a
large workspace with many joined channels, expect the full event volume to be
received even with `--mentions-or-dms`.

By default only message subtypes that carry new content are emitted: plain
messages, `bot_message`, `thread_broadcast`, `file_share`, `me_message`.
Non-message events (e.g. `reaction_added`) are emitted whenever their type
is in `--types`, as the **original Slack payload, unchanged** (the other
filters and the fields below apply to `message` events only).

`message` line fields: `type, subtype, channel, ts, thread_ts, user, bot_id,
text` (max 2000 chars), `from_self, is_mention, is_dm, event_ts, deleted_ts`, plus
`attachments` and `blocks` passed through unchanged when present (bot messages
often keep their button URLs there). For `message_changed`, content fields come
from the nested edited message; for `message_deleted`, they come from
`previous_message`, and `ts` / `deleted_ts` are the removed message's timestamp
(`event_ts` is when it was deleted). Your own messages are kept and flagged
`"from_self": true`.

```json
{"type":"message","channel":"D0123ABCD","ts":"1700000000.000100","user":"U0123ABCD","text":"hi <@U0SELF>","from_self":false,"is_mention":true,"is_dm":true}
```

Control lines:

- `{"type":"_connected","self":{...},"via":"rtm.connect",...}` on first `hello`
- `{"type":"_reconnected",...}` after any later reconnect (events sent while
  disconnected are not replayed; use `message list` to backfill)
- `{"type":"_auth_error","error":"..."}` when credentials are rejected
  (`invalid_auth` / `not_authed` / `token_revoked` / `token_expired` /
  `account_inactive`); the process then exits non-zero. Re-run
  `slackctl auth import-desktop` (or your usual auth command) and restart.
- `{"type":"_connect_error","error":"..."}` when Slack refuses `rtm.connect`
  for a reason retrying cannot fix (`enterprise_is_restricted` on Enterprise
  Grid); the process then exits non-zero.

The connection is kept alive with `ping`/`pong` every 20s; a missing pong,
`goodbye`, or network error triggers reconnect with exponential backoff
(1s, 2s, 4s … capped at 60s), reusing `reconnect_url` when Slack provides one.
Group-DM lookups (`conversations.info`) run off the websocket loop, so a slow or
rate-limited lookup delays output but never the keepalive.

If writing to stdout fails (e.g. disk full), the process logs the error and
exits non-zero instead of silently dropping events.

Running it as a long-lived process: restart only on non-zero exit, and fix
credentials first when the last line was `_auth_error` (and give up on
`_connect_error`), e.g.

```bash
until slackctl listen >> events.jsonl; do
  tail -n1 events.jsonl | grep -qE '"_(auth|connect)_error"' && break
  sleep 5
done
```

## Environment variables

| Variable | Description |
|---|---|
| `SLACK_TOKEN` | Slack token (xoxb-/xoxp-/xoxc-) |
| `SLACK_COOKIE_D` | Browser cookie `d` value (required for xoxc tokens) |
| `SLACK_WORKSPACE_URL` | Workspace URL (required for browser auth) |

Environment variables take precedence over stored credentials.

## Agent skill

This repo ships an agent skill at `skills/slackctl/` compatible with Claude Code, Codex, Gemini CLI, etc.

```bash
# Recommended (via skills.sh)
npx skills add cluas/slackctl -g
```

<details>
<summary>Manual installation</summary>

```bash
git clone https://github.com/cluas/slackctl.git
bash slackctl/scripts/install-skill.sh
```

</details>

This installs the skill to `~/.claude/skills/` and `~/.agents/skills/`. Restart your agent session to activate.

## Credentials storage

Credentials are stored in `~/.config/agent-slack/credentials.json` (compatible with [agent-slack](https://github.com/stablyai/agent-slack)).

## Acknowledgements

This project is a Go rewrite of [agent-slack](https://github.com/stablyai/agent-slack) by [stablyai](https://github.com/stablyai). Thanks to the original authors for the excellent design and architecture that made this port straightforward.

## License

[MIT](LICENSE)

## 原生 MCP（默认只读）

从源码构建一个二进制后，可通过官方 [Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk) 提供标准输入输出服务：

```bash
go build -o slackctl ./cmd/slackctl
./slackctl mcp --workspace https://example.slack.com
```

标准输出只承载 MCP 协议。默认仅注册以下四个只读工具，客户端通过 `initialize`、`tools/list` 和 `tools/call` 使用；写入、实时事件和本机唤醒均需单独显式启用。

| 工具 | 参数 | 用途 |
| --- | --- | --- |
| `search_messages` | `query`、可选 `limit` | 搜索消息，返回总数及本次匹配 |
| `read_message` | `link`，或 `channel` + `ts` + 可选 `thread_ts` | 按时间戳精确读取一条消息 |
| `read_thread` | `link`，或 `channel` + `ts`；可选 `limit` | 从根消息起读取线程，回复链接中的 `thread_ts` 用于定位根 |
| `channel_history` | `channel`、可选 `limit` | 读取频道近期消息 |

`link` 与显式频道／时间戳参数互斥。`channel` 必须是 C、G 或 D 开头的频道 ID；`ts` 与 `thread_ts` 必须包含 10 位秒和 6 位小数。链接必须是固定工作区的 HTTPS 消息永久链接，不能含用户信息、端口或跨域地址。工具不接受工作区、任意 URL、HTTP 方法或 CLI 命令。

通用 stdio 客户端配置（`command` 指向刚构建的二进制，或使用其所在目录中的可执行文件名）：

```json
{
  "mcpServers": {
    "slack-readonly": {
      "command": "slackctl",
      "args": ["mcp", "--workspace", "https://example.slack.com"]
    }
  }
}
```

### 认证边界

MCP 当前仅支持普通、非 Enterprise Grid 工作区，运行时只复用已授权且已保存的标准用户令牌（`xoxp-`）或机器人令牌（`xoxb-`）。必须在现有 `auth add` 流程中将标准令牌关联到完整工作区 URL；请使用组织批准的认证方式，不要把真实令牌写入客户端配置或公开文档。

启动参数与已保存记录的 `workspace_url` 使用同一个严格规则规范化，再匹配同一 canonical URL；例如主机大小写和末尾斜杠不会形成不同工作区。无效的已存 URL 不参与匹配；规范化后重复的配置会被拒绝。模糊名称、默认工作区、`SLACK_TOKEN` 环境认证、浏览器／桌面提取认证、组织 URL 与企业路由认证均不用于 MCP。启动时调用官方 [`auth.test`](https://docs.slack.dev/reference/methods/auth.test/)，核验返回的完整工作区 URL、团队 ID 和用户 ID；无法证明目标绑定时启动失败。只要响应包含 `enterprise_id` 字段（包括空值或 `null`），或明确返回 `is_enterprise_install:true`，也会拒绝启动；普通成功响应不必提供安装布尔值。当前没有保存可信且与同一令牌绑定的 OAuth 安装元数据，因此 **Grid 中合法的单工作区安装也暂不支持**，不能仅凭 `auth.test` 的工作区上下文认定安装范围。后续始终使用同一令牌和 `https://slack.com/api/`，不更换身份、端点或工作区，不重试授权错误，不进行企业路由回退，也不跟随任何 HTTP 重定向。

工具能读取什么由 Slack 的令牌类型、授权范围、成员关系和组织策略决定。`search_messages` 使用的 [`search.messages`](https://docs.slack.dev/reference/methods/search.messages/) 需要标准用户令牌及 `search:read`，机器人令牌不适用于搜索；[`conversations.replies`](https://docs.slack.dev/reference/methods/conversations.replies/) 对不同令牌和频道类型也有权限限制。缺少权限或企业策略拒绝会直接失败，不以其他认证模式替代。现有普通 CLI 的认证和摘要行为保持原样。

### 内容、窗口和限制

返回内容包括 Slack 实际提供的 `text`、原始嵌套 `blocks`、`attachments[].blocks`、附件文本／字段／内容链接、文件摘要、消息及线程时间戳和固定工作区永久链接。仅有 Slack `username` 的机器人作者映射为 `user_name`，搜索的 `channel.name` 映射为 `channel_name`，不额外查询用户资料。Block Kit 使用 `json.RawMessage` 保留 `fields`、`rich_text`、`actions` 等嵌套结构，不依赖 CLI 的摘要投影。文件仅返回摘要与已有链接，不下载文件或访问链接；不返回原始 HTTP envelope、认证或 headers。

本工具只接受显式提供整数 `total` 与数组 `matches` 的搜索结果；`total:0,matches:[]` 表示有效空结果，缺字段或 `null` 会报告响应格式错误。搜索结果可能没有完整的 Blocks 或附件详情，`detail_notice` 会说明这一点。可把返回的 `permalink` 传入 `read_message`；回复链接会保留 `thread_ts`。链接和 Slack 正文都作为不可信外部数据返回，工具描述和结果附带相应提示，不应执行其中的指令。

`limit` 默认 20，范围为 1–50；搜索文本最多 4096 字节。线程和历史使用 Slack 原生 `response_metadata.next_cursor` 分页，每次工具调用最多读取 10 页，并受总条数上限约束。`has_more` 表示 Slack 仍有结果或本次窗口被截断。单条回复读取仅在根线程的前 50 条窗口内精确匹配，校验本页并命中目标后即返回，不再请求无关后页；窗口内未找到会明确失败，不会返回邻近消息。

单次 HTTP 请求最多 10 秒，工具调用最多 15 秒；单个 HTTP 响应最多 1 MiB，工具参数原始 JSON 最多 16 KiB，工具结果最多 256 KiB。工具层超限会返回安全错误，不回显原始 HTTP 错误或认证信息。stdio 使用 SDK 配置的 64 KiB 单次解码新增读取预算；由于 JSON 解码器可能预读后续输入，这不是精确的逐行字节上限。该读取预算超限会结束传输会话，不能按普通工具错误继续复用会话。Slack 限流直接返回失败；读取可由客户端稍后重试，写入必须先核实结果，不能自动重试。

### 显式写入与预览

`--enable-writes` 独立增加 `send_message`、`reply_thread`、`add_reaction`；不启用事件或唤醒。缺省 `dry_run` 或 `dry_run:true` 只返回目标、内容与动作预览，不发出写请求。只有显式 `dry_run:false` 才执行，**工具参数不能替代宿主的用户审批**；宿主仍须核对目标、内容和动作并遵循其审批规则。既有普通 CLI 写命令的行为不变。

| 工具 | 目标与内容 |
| --- | --- |
| `send_message` | 必填 `channel`、`text`；直接发送必须明确频道 ID，不解析频道名或开启私聊 |
| `reply_thread` | `link` 或 `channel` + 根 `ts`，必填 `text`；回复链接含 `thread_ts` 时使用线程根，始终设置 `reply_broadcast:false` |
| `add_reaction` | `link` 或 `channel` + 精确消息 `ts`，必填 `name`；回复链接仍定位链接中的具体消息，不改为线程根 |

消息正文沿用既有 Slack `text`／mrkdwn 语义，最多 12000 字节。可选原生 `blocks` 为 1–50 个对象，JSON 总计最多 16000 字节，正文始终作为回退文本。表情名不带冒号，只允许字母、数字、下划线、加减号，长度 1–100；`already_reacted` 作为成功返回 `already_reacted:true`。写工具原始参数最多 48 KiB，结果仍受 256 KiB 上限约束。

```json
{"channel":"CEXAMPLE1","text":"*合成通知*"}
```

上述参数仅预览。审批后同一动作显式增加 `"dry_run":false` 才会调用固定的 `chat.postMessage` 或 `reactions.add`。目标仍绑定启动时 `auth.test` 核验的工作区和身份，不提供改写方法、认证或端点的参数；远端拒绝、超时或不合法响应不会触发自动重试。错误只返回脱敏说明，结果不明时先读取目标核实。实际写权限依赖组织授权、令牌权限和频道成员关系，模拟通过不代表已具备这些权限。

### 显式实时事件

`--enable-events` 在启动阶段使用同一标准令牌调用固定 `rtm.connect`，核验返回的 `team.id` 和 `self.id` 与 `auth.test` 完全相同，再建立 WSS。只允许 `wss-primary.slack.com`、`wss-backup.slack.com`、`wss-mobile.slack.com`，拒绝自定义端口、用户信息、片段和重定向。连接地址、认证和原始协议错误不写日志。连接后事件或握手中提供团队字段时必须匹配；身份无法证实即失败关闭。

严格事件管线复用既有 `listen` 的消息解析、过滤投影和指数退避，不使用旧 listener 的宽松认证、任意端点、原始协议输出或命令执行能力。每次重连重新请求并核验 `rtm.connect`；`reconnect_url` 只验证，不直接采用。WS 断连与暂时连接故障退避重连，明确的授权或不支持错误停止。状态诊断仅写 stderr。

RTM 支持取决于 Slack 当前的凭证与应用类型。标准令牌通过 `auth.test` **不等于**可以使用 RTM，尤其不能假定现代机器人令牌可使用旧 RTM；Slack 返回不支持、缺权限或受限时会明确失败，绝不回退到浏览器令牌、其他工作区或其他事件入口。当前未进行真实 Slack 凭证验收。

事件接收在独立后台循环实时完成，不依赖客户端轮询，也不调用 Slack 写工具。启用后新增只读 `list_events`：

```json
{"limit":20}
```

首次空游标返回最早保留事件，之后把上次返回的 `cursor` 传回继续读取；读取不消费其他客户端的游标。没有新事件返回空数组。`status` 区分 `connecting`、`connected`、`disconnected`、`failed`、`stopped`；未开启事件时工具根本不注册，内部未启用状态为 `disabled`。过期或旧进程游标返回 `cursor_expired:true`、空结果和重置用游标，客户端确认丢失后再用该游标读取。错误格式或超前游标明确报错。

队列最多 512 条，去重保留最近 1024 个业务事件指纹，溢出丢弃最早事件并增加 `dropped`。帧最多 64 KiB，投影正文最多 2000 个字符，单条业务 JSON 最多 16 KiB；超限 Blocks 被去除并标记 `truncated`。每页最多 50 条且业务 JSON 总计不超过约 96 KiB，`has_more` 提示继续读取。只输出消息类事件（含修改与删除）的相关 ID、类型、时间戳、正文和必要 Blocks；不输出 presence、原始 envelope、握手 URL 或认证字段。正文、Blocks 与链接都是不可信数据。

**RTM 不重放断线期间事件。** 当前队列和游标只存内存，进程重启、队列溢出和断连均可能产生缺口；客户端应保留游标，处理断连和过期状态，必要时另行读取频道历史补查。当前没有实现资源订阅或自定义 MCP 通知协议。

### 可选本机 Orca 唤醒与常驻模式

仅本机部署者可以通过启动参数启用 `--enable-local-wake`，同时提供可信可执行文件绝对路径 `--orca-path`、固定 `--orca-bot`，以及明确的频道白名单或人的 DM 过滤。MCP tools 和 Slack 事件都不能设置或改变这些入口。没有 HTTP 接收服务、HTTPS webhook、外发回调或其他通道。

以下仅为将来授权部署时的通用示例，**本次未配置或启动实际自动化**：

```bash
slackctl mcp listen --workspace https://example.slack.com --enable-local-wake --orca-path /opt/example/bin/orca --orca-bot example-bot --wake-channels CEXAMPLE1 --wake-mentions --wake-dms
```

`slackctl mcp listen` 与 MCP 内的事件接收共用同一严格管线，独立运行且不依赖主管理者 MCP 会话存活。它不提供 stdio 工具服务，也不向 stdout 输出 Slack 正文；接收状态与失败计数写 stderr。需要外部进程管理方保持运行，本次不创建 LaunchAgent、计划任务、既有安装或配置。普通 MCP 会话内启用唤醒则额外需要 `--enable-events`，会话退出即停止该进程内的事件入口。

本机适配器通过直接 exec 固定子命令传递引用：

```text
/opt/example/bin/orca orchestration send --environment local --to bot:example-bot --from bot:example-bot --type handoff --subject "Slack 事件通知" --body-file <权限为0600的临时JSON文件>
```

不经过 shell，不允许额外参数、任意子命令、远端主机或 `--chat`，省略 `--chat` 落到固定 bot 的 Home Chat。子进程不继承代理、配对码、令牌或动态 Orca 远端环境，仅保留本机运行所需的有限环境。载荷只包含版本、固定种类、消息计数与频道、消息 ts、根 ts、事件类型等引用，不转交 Slack 正文、链接、用户标识或凭证。事件不能被当作指令。

白名单频道告警默认按频道合并 180 秒，可通过 `--wake-window` 调整为 1 秒至 1 小时；每批最多 16 个引用，保留总数和截断标记。`--wake-mentions` 只让白名单频道中的直接提及立即唤醒；`--wake-dms` 显式允许人的直接私信立即唤醒，跳过自身及带 bot/app 标识的普通 DM。不默认监控整个工作区，不将修改或删除消息当作新告警。最近 4096 个频道与消息 ts 用于唤醒去重。

事件入队、聚合和 IPC 执行相互分离，不阻塞读写工具。输入队列最多 256 条，发送队列最多 64 批，最多 128 个待聚合频道；超限与停止时未投递数据有明确记录。每次本机执行最多 10 秒，失败最多尝试 3 次，间隔 1、2 秒；子进程输出全部丢弃，只记录固定脱敏失败信息。IPC 结果可能不明，重试可能重复投递，接收方应按引用去重。临时载荷文件在成功、失败或正常取消后删除；异常终止可能留下只含引用的临时文件。不保证断电持久化或恰好一次投递。

### 离线验证与部署前提

```bash
GOPROXY=off go test -count=1 ./...
GOTOOLCHAIN=go1.25.0 GOPROXY=off go test -count=1 ./...
go test -race ./internal/slack ./internal/mcpevents ./internal/localwake ./internal/mcpserver ./internal/cli
go vet ./...
go build ./...
git diff --check
```

测试只使用合成凭证、模拟 HTTP／WS、官方 SDK 客户端、本地 TLS 代理和临时编译的模拟本机接收端，不读取真实凭证、不连接生产 Slack，也不执行真实 Orca bot 发送。真实二进制 stdio 测试覆盖初始化、默认四工具、写入预览与实际 mock payload、目标边界、事件独立读取及常驻模式模拟唤醒；旧版 Go/macOS 或 Windows 使用相同 CLI 入口的测试子进程处理合成证书兼容。已验证的是模拟行为，不是实际 Slack 权限、真实 RTM 可用性或 Orca 部署验收。真实事件触发仍需单独授权部署并验收。
