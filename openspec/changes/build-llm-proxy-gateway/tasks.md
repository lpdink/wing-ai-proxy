## 1. 项目初始化

- [x] 1.1 `go mod init` 初始化模块，创建 `cmd/wing-ai-proxy/main.go` 入口文件
- [x] 1.2 创建完整目录结构：`internal/config`、`internal/proxy`、`internal/provider`、`internal/audit`、`internal/metrics`、`internal/middleware`、`migrations/`
- [x] 1.3 添加依赖：gin、modernc.org/sqlite、fsnotify、prometheus/client_golang、google/uuid、gopkg.in/yaml.v3
- [x] 1.4 编写 Makefile（build、test、lint、fmt、run 目标）
- [x] 1.5 配置 `golangci-lint` 或 `go vet` 静态检查

## 2. 配置层

- [x] 2.1 定义配置结构体（Config、ProviderConfig），包含 YAML tag，支持所有字段（host、port、virtual_api_keys、providers、数据库路径等）
- [x] 2.2 实现 `config.Load(path string) (*Config, error)` — 读取并解析 YAML 配置文件，包含完整校验逻辑（必填字段、格式校验）
- [x] 2.3 实现默认配置文件路径逻辑（`~/.wing-ai-proxy/config.yaml`），配置目录不存在时自动创建
- [x] 2.4 编写配置加载的单元测试（合法配置、语法错误、字段缺失）

## 3. Provider 抽象层

- [x] 3.1 定义 `Provider` 接口（Name、Type、ChatCompletion、ListModels）和 `ModelInfo` 结构体
- [x] 3.2 实现 `OpenAIProvider` — 构造上游请求（model 替换、api_key 替换），使用独立 `http.Client`（配置 timeout、连接池参数）
- [x] 3.3 实现 `ProviderRegistry` — 管理 show_name → Provider 映射表，支持冲突检测（WARN 日志），提供 `Resolve(showName string) (Provider, realName, error)` 方法
- [x] 3.4 实现 Provider 工厂函数 `NewProvider(cfg ProviderConfig) (Provider, error)`，按 type 字段分发
- [x] 3.5 编写 Provider 层单元测试（mock HTTP server 验证请求转发、model 映射、超时处理）

## 4. 审计系统

- [x] 4.1 定义 `AuditRecord` 结构体，包含所有审计字段（request_id、timestamps、tokens、request/response body 等）
- [x] 4.2 编写 SQLite schema migration SQL 文件（`migrations/001_create_audit_table.sql`），包含审计表和查询优化索引
- [x] 4.3 定义 `AuditStore` 接口（`Insert(record AuditRecord) error`），实现 `SQLiteAuditStore`（使用 `embed` 内嵌 migration SQL，启动时自动建表）
- [x] 4.4 实现异步写入器 `AsyncAuditWriter` — buffered channel（4096）+ 单 worker goroutine，事务批量写入，队列满时非阻塞丢弃并 WARN
- [x] 4.5 实现 SSE 响应聚合器 `SSEAggregator` — 逐 chunk 收集 delta，合并 tool_calls，提取 usage，生成完整 response JSON
- [x] 4.6 编写审计系统单元测试（schema 创建、同步写入/读取、异步写入吞吐、队列满降级、SSE 聚合正确性）

## 5. 核心代理 Handler

- [x] 5.1 实现 `GET /models` handler — 从 ProviderRegistry 获取所有 show_name，返回 OpenAI 兼容的 models 列表 JSON
- [x] 5.2 实现 `POST /chat/completions` 非流式 handler — 解析请求、model 映射、调用 Provider、等待响应、构造审计记录、异步写入
- [x] 5.3 实现 `POST /chat/completions` 流式 handler — 解析请求、model 映射、调用 Provider、逐 chunk SSE 转发（立即 flush）、同时用 SSEAggregator 收集完整响应、请求结束后异步审计写入
- [x] 5.4 实现错误处理逻辑 — 上游超时返回 504、上游 4xx/5xx 透传、模型不存在返回 400（附可用模型列表）
- [x] 5.5 编写 handler 层单元测试（mock Provider，验证非流式/流式透传、错误码、model 映射）

## 6. 中间件

- [x] 6.1 实现 `RequestIDMiddleware` — 生成 UUID v4，注入 gin.Context 和 slog logger，写入响应 header `X-Request-Id`
- [x] 6.2 实现 `AuthMiddleware` — 从 Authorization header 提取 Bearer token，在虚拟 Key map 中查找，不存在返回 401
- [x] 6.3 实现白名单路径（`/health`、`/metrics`）跳过鉴权
- [x] 6.4 编写中间件单元测试

## 7. 配置热加载

- [x] 7.1 实现 `config.Watcher` — 使用 fsnotify 监听配置文件，300ms debounce
- [x] 7.2 实现热加载流程 — 文件变更 → 重新解析 → 校验 → 成功则 `sync.RWMutex` 写锁替换内存配置（ProviderRegistry + KeySet），失败则 ERROR 日志
- [x] 7.3 实现不可热加载字段检测 — host/port/数据库路径变更时输出 WARN 日志
- [x] 7.4 编写热加载单元测试（合法重载、语法错误保持原配置、debounce 行为）

## 8. 可观测性

- [x] 8.1 配置 slog JSON handler，支持日志级别配置，全局 logger 初始化
- [x] 8.2 实现 `GET /health` handler — 返回 `{"status": "ok"}`，无需鉴权
- [x] 8.3 定义 Prometheus 指标 — 请求总数（counter，按 provider/model/status 标签）、请求延迟（histogram）、上游失败次数（counter）、审计队列长度（gauge）、首字节延迟（histogram）
- [x] 8.4 在代理 handler 中埋点 — 请求开始/结束记录延迟，上游响应记录首字节时间，失败计数
- [x] 8.5 挂载 `/metrics` 端点（promhttp.Handler()）
- [x] 8.6 编写可观测性单元测试（指标注册、health 端点）

## 9. 组装与集成

- [x] 9.1 在 `main.go` 中组装所有组件 — 加载配置、初始化 logger、创建 ProviderRegistry、初始化 AuditStore + AsyncWriter、注册 Gin 路由和中间件、启动 config watcher
- [x] 9.2 实现优雅关闭 — 监听 SIGINT/SIGTERM，关闭 config watcher、drain 审计队列、关闭数据库连接
- [x] 9.3 编写示例配置文件 `config.example.yaml`，包含详细注释
- [x] 9.4 端到端冒烟测试 — 启动服务，用 curl 验证 /health、/models、/chat/completions（需要可用的上游 API）
