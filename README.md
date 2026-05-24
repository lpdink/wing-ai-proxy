# wing-ai-proxy

高性能、可扩展的 LLM 代理网关，透传客户端请求到上游 LLM 提供商，支持全量审计。

## 特性

- **OpenAI 兼容协议** — `/models`、`/chat/completions`（流式 SSE + 非流式）
- **多 Provider 路由** — 支持多个上游（DashScope、OpenAI 等），模型别名映射
- **全量审计** — 异步写入 SQLite，记录请求/响应体、token 用量、首包延迟
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

审计数据存储在 SQLite，常见查询示例：

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
├── cmd/wing-ai-proxy/main.go   # 入口：组装依赖、启动服务、优雅关闭
├── internal/
│   ├── audit/                   # 审计：Record + SQLite Store + AsyncWriter + SSE Aggregator
│   ├── config/                  # 配置：YAML 加载/校验 + fsnotify 热加载
│   ├── metrics/                 # Prometheus 指标定义
│   ├── middleware/               # RequestID + Auth 中间件
│   ├── provider/                # Provider 接口 + OpenAI 实现 + Registry
│   └── proxy/                   # HTTP Handler（/models, /chat/completions）
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
