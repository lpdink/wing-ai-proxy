## Why

团队需要一个统一的 LLM 请求入口，能对接多家上游提供商（DashScope、OpenAI 等），同时实现全量审计和可观测性。现有直连方式无法集中管控 API Key、统计 token 消耗和追踪请求链路。本项目从零构建一个高性能 Go 代理网关，解决这些问题。

## What Changes

- 新建 Go 项目（Go 1.24 + Gin），实现完整的 LLM 代理网关
- 对外暴露 OpenAI 兼容接口：`GET /models`、`POST /chat/completions`（含 SSE 流式）
- 实现 Provider 抽象层，支持多上游配置、模型别名映射和路由
- 实现全量审计系统，异步写入 SQLite，流式响应需在内存聚合后存储
- 实现配置文件（YAML）热加载，监听文件变化并原子更新内存状态
- 暴露 `/health` 和 `/metrics`（Prometheus 格式）端点
- 结构化 JSON 日志，`request_id` 贯穿全链路
- 提供 Makefile（build / test / lint / fmt / run）

## Capabilities

### New Capabilities

- `request-proxy`: OpenAI 兼容的 HTTP 代理核心，处理 `/models` 和 `/chat/completions` 请求，支持流式（SSE）与非流式透传，模型名映射，虚拟 API Key 鉴权
- `provider-management`: 多 Provider 配置管理，包含上游连接、模型别名映射、冲突检测、Provider 路由选择
- `audit-system`: 全量请求审计，异步队列写入 SQLite，流式 SSE 响应内存聚合，支持完整的请求/响应体存储和派生指标查询
- `config-hot-reload`: YAML 配置文件监听与热加载，原子更新 Provider 列表、模型映射、虚拟 Key 集合
- `observability`: 结构化 JSON 日志、`/health` 健康检查、`/metrics` Prometheus 指标暴露

### Modified Capabilities

（无，全新项目）

## Impact

- **代码**：从零构建整个项目，涉及 Go module 初始化、目录结构设计、Makefile 编写
- **依赖**：Gin（HTTP 框架）、SQLite 驱动（go-sqlite3 或 modernc.org/sqlite）、Prometheus client、YAML 解析、文件监听（fsnotify）、日志库（zerolog 或 slog）
- **外部系统**：需要访问上游 LLM API（DashScope、OpenAI 等），开发测试时需配置代理
- **运维**：配置文件路径 `~/.wing-ai-proxy/config.yaml`，SQLite 数据库 `~/.wing-ai-proxy/sqlite.db`，监听端口 39998
