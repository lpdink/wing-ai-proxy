当前项目还啥都没有，只有docs（也就是这篇文档），我们从零开始。

## 项目目标

构建一个高性能、可扩展的 LLM 代理网关（Go 实现）。  
- **核心能力**：透传客户端请求到上游 LLM 提供商，并支持全量审计。  
- **协议扩展性**：第一版仅支持 **OpenAI 兼容协议**（`/models`、`/chat/completions`），但架构需预留支持 Anthropic 等其它协议的能力。  
- **面向未来**：通过抽象 Provider 层、配置热加载、可插拔存储，轻松支持新协议和新后端。

---

## 功能需求

### 1. 对外暴露的接口（第一版）

| 路径 | 方法 | 说明 |
|------|------|------|
| `/models` | GET | 返回所有 `show_name` 列表（客户端看到的模型别名） |
| `/chat/completions` | POST | OpenAI 兼容的聊天接口，支持流式（SSE）和非流式 |

- 请求和响应格式完全遵循 OpenAI API 规范，不修改任何字段。
- 其他路径（如 `/v1/...`）可按需重定向或返回 404。

### 2. 请求透传规则

- **完全透传**：HTTP method、headers（包括 `Authorization`、`X-*` 等）、request body 原样转发给上游。
- **不修改**：不添加、删除、修改任何 header 或 body 内容（审计需要额外记录，但不改动原文）。
- **流式处理**：
  - 若客户端请求中 `stream=true`，则代理必须使用 SSE 流式返回，每个 chunk 立即转发。
  - 若 `stream=false`，则等待上游完整响应后一次性返回。
  - 注意：流式情况下仍需在内存中聚合完整响应，以便审计保存。

### 3. 上游 Provider 配置与路由

#### 配置文件结构（YAML）
```yaml
# ~/.wing-ai-proxy/config.yaml
host: "127.0.0.1"
port: 39998

# 虚拟 API Key – 简单字符串比对，不检查格式
virtual_api_keys:
  - "sk-abcdefg"
  - "sk-12345678"

providers:
  - name: dashscope          # provider 唯一标识
    type: openai             # 协议类型，第一版仅 "openai"
    base_url: "https://dashscope.aliyuncs.com/compatible-mode/v1"
    api_key: "sk-xxxx"
    timeout: 1800 # 我们默认也是超时30min，这个时间虽然比较长，但是让客户端自己做更短的超时。给客户端留足余量。
    models:
      "ds-v4-flash": "deepseek-v4-flash"    # show_name: real_name
      "qwen-turbo": "qwen-turbo"

  - name: openai
    type: openai
    base_url: "https://api.openai.com/v1"
    api_key: "sk-yyyy"
    models:
      "gpt-4": "gpt-4-turbo"
      "gpt-3.5": "gpt-3.5-turbo"
```
这个不全，还得补充一下存储数据库配置

- **模型映射**：客户端请求中的 `model` 字段（如 `ds-v4-flash`）会被映射到真实模型名（`deepseek-v4-flash`）再发给上游。
- **冲突处理**：多个 provider 配置了相同的 `show_name` → 启动或热重载时打 Warning，始终使用**第一个**匹配到的 provider（配置顺序决定）。
- **热加载**：修改配置文件后，无需重启进程即可生效（模型列表、API Key、Provider 增删等）。如果配置文件语法有错误就不更新，打个error说明哪里不对。

### 4. 审计系统

#### 存储要求
- 默认使用 **SQLite**（文件模式，位置可配置，默认~/.wing-ai-proxy/sqlite.db），同时设计抽象存储层，后续可切换 PostgreSQL。
- 第一版仅实现 SQLite。

#### 审计的数据字段（最少）
| 字段 | 说明 |
|------|------|
| `request_id` | 全局唯一（UUID），用于日志关联 |
| `virtual_api_key` | 客户端使用的虚拟 Key（明文存储，内网环境可接受） |
| `provider_name` | 实际处理请求的上游 Provider 名 |
| `model_show_name` | 客户端请求的模型别名 |
| `model_real_name` | 转换后的真实模型名 |
| `request_start_at` | 请求到达时间（RFC3339 纳秒） |
| `first_byte_at` | 收到上游第一个响应字节的时间（用于首包延迟） |
| `request_end_at` | 请求完全结束时间 |
| `input_tokens` | 从上游响应中解析的 usage.prompt_tokens |
| `output_tokens` | 从上游响应中解析的 usage.completion_tokens |
| `cache_hit_tokens` | 若上游支持（如 Anthropic），记录缓存命中 token 数 |
| `tool_calls` | 本次请求中调用的工具名称列表（JSON 数组） |
| `is_stream` | 是否为流式请求 |
| `request_body` | 原始请求体（文本，SSE 情况需聚合完整 body） |
| `response_body` | 原始响应体（文本，非流式为完整 JSON，流式为聚合后的完整内容） |
| `status_code` | 上游返回的 HTTP 状态码 |
| `error_message` | 若失败，记录错误信息 |

#### 可计算的派生指标（通过 SQL 查询实现）
- 某虚拟 Key 在时间范围内的：请求次数、总输入/输出 tokens、平均首包延迟、平均 TPS（tokens/s）。
- 某模型的 Tool call 频率、失败率等。

#### 注意点
- **SSE 聚合**：流式响应需在内存中收集所有 chunks，重新组装成完整的响应 JSON（OpenAI 流式格式中每个 chunk 的 `delta` 合并即可）。注意内存占用，超长响应可做截断或单独存储文件。
- **存储性能**：审计写入不能阻塞主请求链路 → 使用异步队列写入 SQLite。
- **数据保留**：不自动清理，由运维自行处理。

---

## 非功能需求

### 可观测性
- **结构化日志**（JSON 格式），级别：DEBUG, INFO, WARN, ERROR。
- 每个请求必须包含 `request_id` 贯穿所有日志行。
- 暴露 `/health` 和 `/metrics`（Prometheus 格式）端点。
  - 指标示例：请求总数、延迟分布、上游失败次数、审计队列长度。

### 配置热加载

- 监听配置文件变化。
- 更新内存中的配置（Provider 列表、模型映射、虚拟 Key 集合）（不包括存储数据库，host和port这种一看就知道没法热重载的）。
- 这个需求不用考虑的太复杂，符合要求就更新，不符合要求就不更新，打个error出来。

### 错误处理

- 上游超时：统一返回 `504 Gateway Timeout`，并记录审计。（上游超时时间可以配置
- 上游 4xx/5xx：透传状态码和 body 给客户端。
- 虚拟 Key 不存在：返回 `401 Unauthorized`。
- 模型不存在（show_name 未映射）：返回 `400 Bad Request`，提示可用模型列表。

### 性能目标

- 单机代理增加延迟 < 10ms（不含上游网络）。
- 支持并发 500+ 请求。

---

## 技术约束与实现建议

- **语言与工具**：Go 1.21+，gin
- **项目结构**：良好抽象和架构，为了长期维护考虑。
- **静态检查**：`go vet`
- **Makefile 目标**：
  - `make build` – 编译二进制
  - `make test` – 单元测试
  - `make lint` – 运行静态检查
  - `make fmt` – 代码格式化
  - `make run` – 本地运行

注意数据库建好索引，我们会执行的审计查询，例如：

哪个虚拟api key，走的哪个真实的provider，在哪段时间范围内，用了哪些模型，请求了多少次？用了多少输入tokens？多少输出tokens？多少缓存命中？某个模型的平均首包是多少？TPS是多少？（tokens/s）模型下发了多少个tool call？调用的工具名。

本地当前的go版本是：
go version：
go version go1.24.5 darwin/arm64
本地的go我忘记有没有换源了，没换记得换一下，如果需要访问外网，代理在
export http_proxy="http://127.0.0.1:7892"
export https_proxy="http://127.0.0.1:7892"
