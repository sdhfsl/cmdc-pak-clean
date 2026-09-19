# cmdc-pak-clean

本机 Command Code 网关的本地转换代理。读取本机 CLI 登录凭证（`~/.commandcode/auth.json`），
在本地暴露 OpenAI / Anthropic / OpenAI Responses 三种协议接口，转发到官方网关。

无内置校验逻辑，不含任何硬编码凭证，凭证始终来自本机登录状态。

## 特性

- **三协议入口**：`/v1/chat/completions`（OpenAI）、`/v1/messages`（Anthropic）、`/v1/responses`（OpenAI Responses / Codex）
- **思考链透传**：Anthropic 通道强制最大思考强度，`thinking_delta` 逐块 1:1 透传；Chat 通道以 `reasoning_content` 返回
- **工具调用**：`tool_calls` / `tool-call` 全链路转换，工具名大小写映射，畸形参数归一化（null / 数组包裹 / 字符串）
- **模型目录自动解析**：从本机桌面版 harness 解析模型列表、上下文窗口与思考档位（支持 `CMDC_HARNESS_PATH` 覆盖）
- **Web 面板**：`/api/status`、`/api/config`，实时余额 / 窗口用量 / 请求日志 / 模型搜索，本地 127.0.0.1 绑定
- **稳健性**：上游空闲超时（默认 180s）、思考配额下限保护、原子配置写入、单实例检测、端口占用自动顺延

## 构建

```bash
go build -ldflags="-s -w" -o cmdc-pak-clean.exe .
```

## 运行

```bash
./cmdc-pak-clean.exe
```

启动后面板地址：`http://127.0.0.1:8787/`

| 客户端 | Base URL | API Key | 协议 |
|---|---|---|---|
| Z Code / Cursor | `http://localhost:8787/v1` | 任意（`local-proxy`） | Chat Completions |
| Claude 系客户端 | `http://localhost:8787/v1` | 任意 | Anthropic Messages |
| Codex | `http://localhost:8787/v1` | 任意 | Responses |

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `CMDC_PAK_PORT` | 8787 | 监听端口（被占用时自动顺延） |
| `CMDC_PAK_CONFIG_DIR` | `%APPDATA%\cmdc-pak-clean` | 配置与日志目录 |
| `CMDC_PAK_AUTH_FILE` | `~/.commandcode/auth.json` | 凭证文件路径覆盖 |
| `CMDC_PAK_GATEWAY_URL` | 官方网关 | 上游地址覆盖 |
| `CMDC_PAK_IDLE_TIMEOUT` | 180 | 上游流空闲超时（秒） |
| `CMDC_PAK_FORCE_EFFORT` | 开 | 设为 `off` 关闭思考档位拉满 |
| `CMDC_PAK_TOOL_NUDGE` | 关 | 设为 `1` 在 system 末尾追加"优先使用工具"提示 |
| `CMDC_HARNESS_PATH` | 自动探测 | 桌面版 harness 文件路径覆盖 |

## 免责声明

仅供个人学习与自用。使用者需自行确保其使用方式符合相关服务条款。
