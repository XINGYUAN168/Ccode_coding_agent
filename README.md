# Ccode

Ccode 是一个使用 Go 编写的终端编程 Agent。它提供交互式 TUI、非交互输出、远程 Web 界面，以及文件操作、命令执行、代码检索、MCP、Skills、记忆、子 Agent、团队协作和 Git 工作树能力。

## 环境要求

- Go 1.25 或更高版本
- Git
- 可用的模型服务 API 密钥
- 可选：tmux 或 macOS iTerm2，用于在独立窗格中运行团队成员

## 构建

```bash
git clone <your-repository-url>
cd Ccode
go build -o ccode ./cmd/ccode
```

也可以直接运行：

```bash
go run ./cmd/ccode
```

## 配置

Ccode 按以下顺序加载配置，后加载的配置覆盖前面的配置：

1. `~/.ccode/config.yaml`
2. `<工作目录>/.ccode/config.yaml`
3. `<工作目录>/.ccode/config.local.yaml`

可从仓库中的 `.ccode/config.yaml.example` 或 `config.example.yaml` 开始：

```bash
mkdir -p .ccode
cp .ccode/config.yaml.example .ccode/config.yaml
```

请把示例密钥替换为自己的密钥，并避免提交包含真实密钥的配置文件。`api_key` 可省略，此时 Ccode 会读取对应厂商的环境变量；`context_window` 和 `max_output_tokens` 可用于覆盖自动值：

```yaml
providers:
  - name: deepseek
    protocol: openai-compat
    base_url: https://api.deepseek.com/v1/chat/completions
    api_key: your-api-key
    model: deepseek-chat
    thinking: false
```

仓库内的两个配置示例给出了六类服务的完整写法：

| 服务 | `protocol` | `base_url` |
|---|---|---|
| Anthropic | `anthropic` | 留空时使用官方端点 |
| OpenAI | `openai` | 留空时使用官方 Responses API |
| Google Gemini | `openai-compat` | 使用兼容 OpenAI Chat Completions 的完整地址 |
| Kimi | `openai-compat` | 使用兼容 OpenAI Chat Completions 的完整地址 |
| 智谱 GLM | `openai-compat` | 使用兼容 OpenAI Chat Completions 的完整地址 |
| DeepSeek | `openai-compat` | 使用兼容 OpenAI Chat Completions 的完整地址 |

`thinking` 控制该模型是否启用思考能力。`anthropic` 和 `openai` 使用各自官方协议，`openai-compat` 用于兼容 Chat Completions 的厂商。密钥环境变量依次支持 `ANTHROPIC_API_KEY`、`OPENAI_API_KEY`、`GEMINI_API_KEY`、`MOONSHOT_API_KEY`、`ZHIPUAI_API_KEY` 和 `DEEPSEEK_API_KEY`。配置中的第一个 Provider 是默认选择，交互界面中可以切换到其它已配置 Provider。

常用顶层配置包括：

```yaml
permission_mode: default
enable_fork: true
enable_coordinator_mode: false

sandbox:
  enabled: false
  auto_allow: false
  network_enabled: false
```

权限模式支持 `default`、`acceptEdits`、`plan` 和 `bypassPermissions`。即使选择自动放行模式，路径沙箱和显式拒绝规则仍会生效。

## 运行方式

启动交互式终端界面：

```bash
./ccode
```

执行单次任务并输出纯文本：

```bash
./ccode -p "检查当前目录中的测试失败"
printf '%s\n' '总结这个仓库' | ./ccode --print
```

输出逐行 JSON 事件：

```bash
./ccode -p "运行测试并说明结果" --output-format stream-json
```

启动远程 Web 界面，默认监听 `:18888`：

```bash
./ccode --remote
./ccode --remote 127.0.0.1:18888
```

远程模式没有内置身份认证。只在可信网络中使用；仅供本机访问时应显式绑定 `127.0.0.1`。

## 交互命令

进入 TUI 后输入 `/help` 查看当前可用命令。内置命令包括：

- `/status`：查看模型、权限、token、工具和目录状态
- `/clear`：清空当前对话
- `/compact`：压缩对话上下文
- `/plan`：进入只读计划模式
- `/resume`：恢复历史会话
- `/rewind`：把对话和文件恢复到检查点
- `/memory [list|clear]`：查看或清除自动记忆
- `/skills`、`/skills reload`：查看或重新加载 Skill
- `/mcp`：查看 MCP 连接状态
- `/sandbox`：调整命令执行沙箱
- `/review`：检查当前代码变更

## 工具与权限

基础工具包括读取、写入和编辑文件，执行 shell 命令，以及 Glob 和 Grep 检索。写入前会校验文件状态，减少覆盖外部修改的风险。工具调用还会经过路径范围、规则文件、当前权限模式和人工确认检查。

项目级权限规则可放在 `.ccode/permissions.yaml` 和 `.ccode/permissions.local.yaml`。命令执行沙箱支持控制网络访问；实际隔离能力取决于当前操作系统。

## Skills

Skill 使用目录形式保存，入口文件为 `SKILL.md`：

- 用户级：`~/.ccode/skills/<skill-name>/SKILL.md`
- 项目级：`.ccode/skills/<skill-name>/SKILL.md`

项目级同名 Skill 的优先级高于用户级。`/skills reload` 可以在不重启程序的情况下重新扫描目录。Skill 支持 `inline` 和 `fork` 执行模式，也可以携带 `references/`、`scripts/` 等辅助资源。

## MCP

Ccode 支持 stdio、Streamable HTTP 和兼容旧服务的 HTTP+SSE 传输。stdio 示例：

```yaml
mcp_servers:
  - name: context7
    command: npx
    args: ["-y", "@upstash/context7-mcp"]
```

HTTP 服务使用 `url` 和可选的 `transport`、`headers`、`env` 字段。请求头和环境变量值支持读取运行环境中的变量。

## 记忆、会话与项目指令

- 项目记忆：`.ccode/memory/`
- 用户记忆：`~/.ccode/memory/`
- 会话记录：`.ccode/sessions/`
- 文件回退记录：`.ccode/file-history/`
- 项目指令：`CCODE.md`、`AGENTS.md`、`.ccode/CCODE.md`

自动记忆会在会话中提取和召回信息；对话过长时可通过 `/compact` 压缩。`/rewind` 使用文件历史检查点恢复代码或对话。

## 团队与工作树

启用 `enable_coordinator_mode` 后，Lead 可以创建团队、分配任务、接收队员消息并管理共享任务板。程序在 tmux 或 iTerm2 会话中可使用对应窗格后端，否则使用进程内后端。

Agent 可在 `.ccode/worktrees/` 下创建隔离的 Git 工作树。工作树会继承本地设置、钩子和 `.worktreeinclude` 指定的忽略文件；有未提交变更或未推送提交时，自动清理会采用保守策略保留工作树。

## 测试

```bash
gofmt -l cmd internal
go mod tidy -diff
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
go build ./...
```

跨平台构建可在关闭 CGO 后分别设置 `GOOS` 和 `GOARCH`。真实模型测试需要自行提供仓库外的环境配置；测试日志中不要输出 API 密钥。
