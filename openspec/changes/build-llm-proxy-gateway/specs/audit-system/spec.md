## ADDED Requirements

### Requirement: 审计记录数据结构
系统 SHALL 为每个 `/chat/completions` 请求生成审计记录，包含以下字段：`request_id`（UUID）、`virtual_api_key`、`provider_name`、`model_show_name`、`model_real_name`、`request_start_at`（RFC3339 纳秒）、`first_byte_at`、`request_end_at`、`input_tokens`、`output_tokens`、`cache_hit_tokens`、`tool_calls`（JSON 数组）、`is_stream`、`request_body`、`response_body`、`status_code`、`error_message`。

#### Scenario: 非流式请求审计完整
- **WHEN** 一个非流式 `/chat/completions` 请求完成
- **THEN** 审计记录包含完整的请求体、响应体、从 `usage` 字段解析的 token 数、所有时间戳和状态码

#### Scenario: 流式请求审计完整
- **WHEN** 一个流式（SSE）`/chat/completions` 请求完成
- **THEN** 审计记录的 `response_body` 包含聚合后的完整响应内容（合并所有 SSE chunk 的 delta），token 数从最后一个 chunk 的 `usage` 字段解析

#### Scenario: 请求失败时审计
- **WHEN** 上游返回错误或请求超时
- **THEN** 审计记录中 `error_message` 包含错误详情，`status_code` 记录上游状态码（超时则为 504），`response_body` 记录已收到的部分数据或为空

### Requirement: 异步审计写入
审计写入 SHALL 不阻塞主请求链路。系统使用 buffered channel（容量 4096）+ 单 worker goroutine 异步写入 SQLite。

#### Scenario: 审计写入不影响请求延迟
- **WHEN** 高并发请求涌入
- **THEN** 审计记录投递到 channel 后立即返回，主请求处理不等待 SQLite 写入完成

#### Scenario: 审计队列满时降级
- **WHEN** channel 已满（4096 条记录堆积）
- **THEN** 新审计记录被丢弃，系统输出 WARN 日志（包含 request_id），不阻塞请求

### Requirement: SQLite 存储
系统 SHALL 使用 SQLite 文件数据库存储审计记录。数据库文件路径可配置（默认 `~/.wing-ai-proxy/sqlite.db`）。建表时 MUST 包含合理索引以支持常见审计查询。

#### Scenario: 数据库自动初始化
- **WHEN** 系统启动时 SQLite 文件不存在
- **THEN** 系统自动创建数据库文件并执行 schema migration，创建审计表和索引

#### Scenario: 支持常见审计查询
- **WHEN** 运维执行审计查询（按虚拟 Key、时间范围、Provider、模型筛选；统计请求次数、输入/输出 tokens、平均首包延迟、TPS）
- **THEN** 查询利用索引高效执行，响应时间在毫秒级

### Requirement: SSE 响应聚合
流式响应 SHALL 在内存中聚合所有 SSE chunk，重组为完整的响应 JSON 用于审计存储。

#### Scenario: 正常聚合 SSE 响应
- **WHEN** 上游返回 100 个 SSE chunk
- **THEN** 系统将每个 chunk 的 `choices[].delta` 合并为完整的 `choices[].message`，最终聚合结果包含完整的 assistant 回复内容

#### Scenario: 超长响应截断
- **WHEN** 聚合 buffer 超过配置的上限（默认 10MB）
- **THEN** 系统截断 `response_body` 并标记 `truncated: true`，防止 OOM

#### Scenario: Tool calls 提取
- **WHEN** 流式响应中包含 tool_calls（分散在多个 chunk 中）
- **THEN** 系统合并 tool_calls，提取工具名称列表存入审计记录的 `tool_calls` 字段

### Requirement: 存储抽象层
系统 SHALL 定义存储接口（`AuditStore`），SQLite 实现为第一版唯一实现。接口 MUST 支持后续切换到 PostgreSQL。

#### Scenario: 切换到 PostgreSQL
- **WHEN** 运维需要将存储切换为 PostgreSQL
- **THEN** 只需实现 `AuditStore` 接口的 PostgreSQL 版本，修改配置和工厂函数，核心逻辑无需改动
