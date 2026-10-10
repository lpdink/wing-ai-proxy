# wing-ai-proxy

高性能、可扩展的 LLM 代理网关，透传客户端请求到上游 LLM 提供商，支持全量审计。

## 特性

- **OpenAI 兼容协议** — `/models`、`/chat/completions`（流式 SSE + 非流式）
- **Responses API 桥接** — `/v1/responses` 将 OpenAI Responses API 自动转换为 Chat Completions 转发上游，兼容 Codex CLI 等工具
- **多 Provider 路由** — 支持多个上游（DashScope、OpenAI 等），模型别名映射
- **全量审计** — 异步写入 SQLite（WAL），请求/响应体 gzip 压缩存储；流式响应保存聚合结果，记录 token 用量、首包延迟
- **配置热加载** — 修改 YAML 配置自动生效，无需重启
- **可观测性** — 结构化 JSON 日志、Prometheus `/metrics`、`/health` 健康检查
- **零修改透传** — 不修改任何 header 或 body（仅替换 `model` 字段和上游 `Authorization`）

## 快速开始

### 安装

```bash
# 从 Release 下载二进制，或从源码构建
git clone https://github.com/your-org/wing-ai-proxy.git
cd wing-ai-proxy
make build
```

### 配置

创建 `~/.wing-ai-proxy/config.yaml`：

```yaml
host: "127.0.0.1"
port: 39998

virtual_api_keys:
  - "sk-your-virtual-key"

providers:
  - name: dashscope
    type: openai
    base_url: "https://dashscope.aliyuncs.com/compatible-mode/v1"
    api_key: "sk-your-dashscope-key"
    timeout: 1800s
    models:
      "ds-v4-flash": "deepseek-v4-flash"
      "qwen-turbo": "qwen-turbo"

database:
  driver: sqlite
  dsn: "~/.wing-ai-proxy/sqlite.db"

log_level: "info"
```

### 启动

```bash
./bin/wing-ai-proxy                              # 使用默认配置路径
./bin/wing-ai-proxy /path/to/config.yaml         # 指定配置文件
```

### 使用

```bash
# 列出可用模型
curl -H "Authorization: Bearer sk-your-virtual-key" http://127.0.0.1:39998/models

# 非流式对话
curl -X POST http://127.0.0.1:39998/chat/completions \
  -H "Authorization: Bearer sk-your-virtual-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"ds-v4-flash","messages":[{"role":"user","content":"Hello"}]}'

# 流式对话（SSE）
curl -N -X POST http://127.0.0.1:39998/chat/completions \
  -H "Authorization: Bearer sk-your-virtual-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"ds-v4-flash","messages":[{"role":"user","content":"Hello"}],"stream":true}'
```

也可用 OpenAI Python SDK 直接对接：

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:39998/", api_key="sk-your-virtual-key")
models = client.models.list()
response = client.chat.completions.create(
    model="ds-v4-flash",
    messages=[{"role": "user", "content": "Hello"}],
)
```

## API 端点

| 路径 | 方法 | 鉴权 | 说明 |
|------|------|------|------|
| `/health` | GET | ❌ | 健康检查 |
| `/metrics` | GET | ❌ | Prometheus 指标 |
| `/models` | GET | ✅ | 返回可用模型列表 |
| `/chat/completions` | POST | ✅ | OpenAI 兼容聊天接口 |
| `/v1/chat/completions` | POST | ✅ | 同上（带 /v1 前缀的别名） |
| `/v1/responses` | POST | ✅ | OpenAI Responses API 桥接 |

### Responses API（`/v1/responses`）

桥接 OpenAI Responses API 到上游 Chat Completions，使 Codex CLI 等工具无需上游原生支持即可使用。

**协议转换**：
- 请求：`instructions` → system message，`input` → messages，`developer` role → `system`
- 工具：Responses API 扁平格式自动包装为 Chat Completions 的 `function` 嵌套格式
- 响应：Chat Completions SSE 流转换为完整的 Responses API 事件生命周期

**使用示例（Codex CLI）**：

在 `~/.codex/config.toml` 中配置自定义 provider：

```toml
model = "qwen3.7-max"
model_provider = "wing"

[model_providers.wing]
name = "Wing AI Proxy"
base_url = "http://127.0.0.1:39998/v1"
env_key = "WING_API_KEY"
wire_api = "responses"
```

然后设置环境变量并启动：

```bash
WING_API_KEY="sk-your-virtual-key" codex
```

## 配置详解

### 虚拟 API Key

客户端使用的认证凭证，简单字符串比对。支持多个 Key，热加载支持增删。

### Provider 配置

| 字段 | 必填 | 说明 |
|------|------|------|
| `name` | ✅ | Provider 唯一标识 |
| `type` | ✅ | 协议类型（当前仅 `openai`） |
| `base_url` | ✅ | 上游 API 地址 |
| `api_key` | ✅ | 上游认证凭证 |
| `timeout` | ❌ | 请求超时（默认 30min） |
| `models` | ✅ | `show_name: real_name` 映射表 |

**模型冲突处理**：多个 Provider 配置相同 `show_name` 时，启动时打 WARN 日志，使用配置顺序中第一个匹配的 Provider。

### 热加载

修改配置文件后自动生效，可热加载的字段：
- ✅ `virtual_api_keys` — 增删立即生效
- ✅ `providers` — 增删 Provider、修改模型映射立即生效
- ❌ `host`、`port`、`database` — 需要重启

配置语法错误时保持原配置不变，打 ERROR 日志。

### 审计查询

审计数据存储在 SQLite（WAL 模式），常见查询示例：

```sql
-- 某个 Key 的请求次数和 token 用量
SELECT model_show_name, COUNT(*) as requests,
       SUM(input_tokens) as input, SUM(output_tokens) as output
FROM audit_records
WHERE virtual_api_key = 'sk-xxx'
  AND request_start BETWEEN '2024-01-01' AND '2024-01-31'
GROUP BY model_show_name;

-- 平均首包延迟和 TPS
SELECT model_show_name,
       AVG(julianday(first_byte_at) - julianday(request_start)) * 86400 as avg_ttfb_sec,
       AVG(output_tokens / (julianday(request_end) - julianday(request_start)) / 86400) as avg_tps
FROM audit_records
WHERE provider_name = 'dashscope'
GROUP BY model_show_name;

-- Tool call 统计
SELECT json_each.value as tool_name, COUNT(*) as call_count
FROM audit_records, json_each(audit_records.tool_calls)
GROUP BY tool_name ORDER BY call_count DESC;
```

### 审计存储格式

- `request_body` / `response_body`：非空内容以 **gzip 压缩后的 BLOB** 存储；空内容仍为空字符串。（提示：几十字节级的极小 body 经 gzip 后可能略大于原文，固定开销约 25 字节，量级可忽略。）
- 流式请求（`/chat/completions`）的 `response_body` 保存**聚合后的 JSON**（不再保存原始 SSE 流），形如：
  ```json
  {"content":"...","reasoning_content":"...","tool_calls":[{"id":"...","name":"...","arguments":"..."}],"finish_reason":"stop","usage":{"prompt_tokens":0,"completion_tokens":0,"cached_tokens":0}}
  ```
  已知限制：上游 `n > 1` 的多个 choice 会合并为单条 message（本网关非目标场景）。
- 非 SSE 响应（如上游错误 JSON、HTML 错误页）：流式请求的 `response_body` 保存**原始响应字节（最多前 1MB）**，避免审计里只剩空聚合体、看不到失败原因。
- 已知差异：`/v1/responses` 的流式审计保存聚合后的纯文本 content（不做 JSON 包装），与 `/chat/completions` 的聚合 JSON 形态不同。
- `truncated` 列仅为兼容历史数据保留，新写入恒为 0。
- 升级前写入的历史行仍是明文 TEXT。可用 `typeof(request_body)` 区分：`'text'` = 明文（或空串），`'blob'` = 已压缩；执行一次 `compact`（见下）可把历史数据全部压缩。

读回请求体/响应体（新格式为 gzip BLOB）：

```bash
# 导出并解压（sqlite3 CLI 需带 fileio 扩展，常见发行版默认包含）
sqlite3 audit.db "SELECT writefile('/tmp/body.gz', request_body) FROM audit_records WHERE id = 1;" && gunzip -c /tmp/body.gz
```

```python
# Python 3 读回，自动区分历史明文与新 BLOB
import gzip, sqlite3
body = sqlite3.connect("audit.db").execute(
    "SELECT request_body FROM audit_records WHERE id = 1").fetchone()[0]
print(gzip.decompress(body).decode() if isinstance(body, bytes) else body)
```

### 审计库维护（compact 子命令）

升级后建议执行一次 `compact`，把历史明文的请求/响应体压缩回写。可在服务运行中执行：分批（每批 500 行、累计 body 上限 32MB）+ 每批一个事务，仅处理 `typeof(...) = 'text'` 的历史行，不会影响正在写入的新数据。

```bash
./bin/wing-ai-proxy compact                      # 使用默认配置路径
./bin/wing-ai-proxy compact /path/to/config.yaml # 指定配置文件
./bin/wing-ai-proxy compact --vacuum             # 压缩完成后执行 VACUUM 回收磁盘空间
```

- 逐批打印进度（已处理行数、输入/输出字节累计）。
- 默认不回收磁盘空间，结束时提示可加 `--vacuum`；`VACUUM` 需要相当于库文件大小的临时磁盘空间，且需要数据库空闲——若 WAL checkpoint 因并发读者无法完成（busy），会提示先在业务低峰期重试。
- 已知限制：历史流式响应体压缩后仍是原始 SSE 文本，不会重新聚合。

## 开发

### 前置要求

- Go 1.21+

### 命令

```bash
make build    # 编译二进制到 bin/
make test     # 运行测试（含 race 检测）
make lint     # 静态检查 (go vet)
make fmt      # 代码格式化
make run      # 编译并运行
make clean    # 清理构建产物
```

### 项目结构

```
wing-ai-proxy/
├── cmd/wing-ai-proxy/main.go   # 入口：组装依赖、启动服务、优雅关闭（compact 子命令见 compact.go）
├── internal/
│   ├── audit/                   # 审计：Record + SQLite Store（gzip 压缩）+ AsyncWriter + SSE 聚合 + compact
│   ├── config/                  # 配置：YAML 加载/校验 + fsnotify 热加载
│   ├── metrics/                 # Prometheus 指标定义
│   ├── middleware/               # RequestID + Auth 中间件
│   ├── provider/                # Provider 接口 + OpenAI 实现 + Registry
│   └── proxy/                   # HTTP Handler
│       ├── handler.go           # /models, /chat/completions
│       └── responses.go         # /v1/responses (Responses API → Chat Completions 桥接)
├── migrations/                  # SQLite Schema
├── config.example.yaml          # 示例配置
└── Makefile
```

### 扩展新协议

实现 `provider.Provider` 接口并注册到工厂函数即可：

```go
type Provider interface {
    Name() string
    Type() string
    ChatCompletion(ctx context.Context, body []byte, clientHeaders http.Header) (*http.Response, error)
    ListModels(ctx context.Context) []ModelInfo
}
```

## License

MIT
