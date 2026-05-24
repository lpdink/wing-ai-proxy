## ADDED Requirements

### Requirement: 虚拟 API Key 鉴权
系统 SHALL 在每个请求到达路由前校验 `Authorization: Bearer <key>` 中的 API Key。Key MUST 与配置中的 `virtual_api_keys` 列表进行精确字符串匹配。

#### Scenario: 合法 Key 通过鉴权
- **WHEN** 客户端发送请求，`Authorization` header 包含 `Bearer sk-abcdefg`，且 `sk-abcdefg` 存在于 `virtual_api_keys` 配置中
- **THEN** 请求正常路由到下游 handler，返回非 401 状态码

#### Scenario: 非法 Key 被拒绝
- **WHEN** 客户端发送请求，`Authorization` header 包含 `Bearer sk-invalid`，且 `sk-invalid` 不存在于配置中
- **THEN** 系统返回 `401 Unauthorized`，响应体包含错误信息

#### Scenario: 缺失 Authorization header
- **WHEN** 客户端发送请求，未携带 `Authorization` header
- **THEN** 系统返回 `401 Unauthorized`

### Requirement: GET /models 返回模型别名列表
系统 SHALL 在 `GET /models` 端点返回所有已配置的 `show_name` 列表，格式遵循 OpenAI `/models` 响应规范。

#### Scenario: 正常返回模型列表
- **WHEN** 客户端通过合法 Key 请求 `GET /models`
- **THEN** 系统返回 JSON，`data` 数组中包含所有已配置的 `show_name`，每个条目包含 `id`（即 show_name）、`object`（值为 "model"）、`owned_by`（即 provider name）

#### Scenario: 无模型配置时返回空列表
- **WHEN** 配置中未定义任何 provider 或 provider 的 models 为空
- **THEN** 系统返回 JSON，`data` 为空数组

### Requirement: POST /chat/completions 非流式透传
系统 SHALL 在 `POST /chat/completions` 端点接收 OpenAI 兼容格式的请求，将 `model` 字段替换为真实模型名后，原样透传所有 headers 和 body 到上游 Provider，等待完整响应后返回给客户端。

#### Scenario: 非流式请求成功透传
- **WHEN** 客户端发送 `POST /chat/completions`，`stream` 字段为 `false` 或缺失，`model` 为已配置的 show_name
- **THEN** 系统将请求体中 `model` 替换为对应的 real_name，转发到匹配的 Provider 的 `base_url`，使用 Provider 的 `api_key` 替换 `Authorization` header，将上游完整响应原样返回给客户端

#### Scenario: 上游返回 4xx/5xx
- **WHEN** 上游 Provider 返回 4xx 或 5xx 状态码
- **THEN** 系统将上游的状态码和响应体原样透传给客户端

#### Scenario: 上游超时
- **WHEN** 上游 Provider 在配置的超时时间内未返回响应
- **THEN** 系统返回 `504 Gateway Timeout`，响应体包含超时错误信息

### Requirement: POST /chat/completions 流式 SSE 透传
系统 SHALL 在 `stream=true` 时使用 Server-Sent Events 协议逐 chunk 转发上游响应，每个 chunk 收到后立即转发给客户端。

#### Scenario: 流式请求正常转发
- **WHEN** 客户端发送 `POST /chat/completions`，`stream` 字段为 `true`
- **THEN** 系统逐 chunk 将上游 SSE 事件转发给客户端，每个 chunk 收到后立即写入并 flush，客户端收到的 SSE 格式与上游完全一致

#### Scenario: 流式传输中上游断开
- **WHEN** 流式传输过程中上游连接意外断开
- **THEN** 系统向客户端发送 SSE error event 并关闭连接，审计记录已收到的部分数据和错误信息

### Requirement: 模型不存在时返回 400
系统 SHALL 在客户端请求的 `model` 字段（show_name）未匹配任何已配置模型时返回错误。

#### Scenario: 请求未配置的模型
- **WHEN** 客户端发送 `POST /chat/completions`，`model` 为 `unknown-model`，该名称未在任何 provider 中配置
- **THEN** 系统返回 `400 Bad Request`，响应体包含当前可用的模型列表

### Requirement: 请求完全透传不修改
系统 SHALL 保持 HTTP method、自定义 headers（`X-*` 等）、request body 不变。除 `model` 字段映射和 `Authorization` 替换外，不添加、删除或修改任何其他内容。

#### Scenario: 自定义 header 被透传
- **WHEN** 客户端发送请求包含自定义 header `X-Custom-Header: test-value`
- **THEN** 转发给上游的请求中包含相同的 `X-Custom-Header: test-value`

#### Scenario: 请求体原样透传
- **WHEN** 客户端发送请求体包含 `temperature`、`top_p`、`tools` 等非标准字段
- **THEN** 上游收到的请求体中这些字段保持原值不变（除 `model` 字段外）
