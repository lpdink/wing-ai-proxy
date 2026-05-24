## ADDED Requirements

### Requirement: 结构化 JSON 日志
系统 SHALL 使用 `log/slog` 输出 JSON 格式的结构化日志，支持 DEBUG、INFO、WARN、ERROR 四个级别。

#### Scenario: 日志输出为 JSON 格式
- **WHEN** 系统输出一条 INFO 级别日志
- **THEN** 输出为合法 JSON，包含 `time`（RFC3339）、`level`、`msg` 字段

#### Scenario: 日志级别过滤
- **WHEN** 配置日志级别为 WARN
- **THEN** DEBUG 和 INFO 级别的日志不输出，WARN 和 ERROR 正常输出

### Requirement: request_id 贯穿全链路
每个请求 SHALL 分配全局唯一的 `request_id`（UUID v4），该 ID MUST 出现在该请求相关的所有日志行中。

#### Scenario: 请求日志包含 request_id
- **WHEN** 一个请求经过鉴权、路由、代理、审计全流程
- **THEN** 该流程中所有日志行都包含相同的 `request_id` 字段

#### Scenario: request_id 唯一性
- **WHEN** 系统处理 10000 个请求
- **THEN** 每个请求的 `request_id` 全局唯一，无重复

### Requirement: /health 健康检查端点
系统 SHALL 暴露 `GET /health` 端点，返回服务健康状态。该端点 MUST NOT 需要鉴权。

#### Scenario: 服务正常运行
- **WHEN** 客户端请求 `GET /health`
- **THEN** 系统返回 `200 OK`，响应体包含 `{"status": "ok"}`

#### Scenario: 健康检查无需鉴权
- **WHEN** 客户端未携带 Authorization header 请求 `GET /health`
- **THEN** 系统正常返回健康状态，不返回 401

### Requirement: /metrics Prometheus 指标端点
系统 SHALL 暴露 `GET /metrics` 端点，返回 Prometheus 格式的指标数据。该端点 MUST NOT 需要鉴权。

#### Scenario: 返回 Prometheus 格式指标
- **WHEN** 客户端请求 `GET /metrics`
- **THEN** 系统返回 `text/plain` 格式的 Prometheus 指标数据

#### Scenario: 核心指标暴露
- **WHEN** 系统已处理若干请求后查询 /metrics
- **THEN** 返回的指标包含：请求总数（按 provider/model/status 分维度）、请求延迟分布（histogram）、上游失败次数、审计队列当前长度、首字节延迟（histogram）
