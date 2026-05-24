## ADDED Requirements

### Requirement: YAML 配置文件定义 Provider
系统 SHALL 从 YAML 配置文件加载 Provider 定义，每个 Provider 包含 `name`（唯一标识）、`type`（协议类型）、`base_url`、`api_key`、`timeout` 和 `models`（show_name → real_name 映射）。

#### Scenario: 正常加载配置
- **WHEN** 配置文件包含合法的 provider 定义
- **THEN** 系统解析所有 provider，建立 show_name → (provider, real_name) 的映射表，provider 的 HTTP client 使用配置的 timeout

#### Scenario: 配置语法错误
- **WHEN** 配置文件存在 YAML 语法错误
- **THEN** 系统启动失败并输出明确的错误信息，指出错误位置

### Requirement: 模型映射冲突检测
系统 SHALL 在启动或热加载时检测多个 Provider 配置了相同 show_name 的情况。

#### Scenario: 存在 show_name 冲突
- **WHEN** Provider A 和 Provider B 都配置了 show_name `gpt-4`
- **THEN** 系统输出 WARN 日志，列出冲突的 provider 名称，使用配置文件中**先出现**的 provider 的映射

#### Scenario: 无冲突
- **WHEN** 所有 provider 的 show_name 互不重叠
- **THEN** 系统正常加载，无冲突告警

### Requirement: Provider 路由选择
系统 SHALL 根据请求中的 `model`（show_name）查找对应的 Provider，将请求转发到该 Provider 的 `base_url`，使用 Provider 的 `api_key` 作为上游认证凭证。

#### Scenario: 按 show_name 路由到正确 Provider
- **WHEN** 请求 `model` 为 `ds-v4-flash`，该 show_name 映射到 provider `dashscope`
- **THEN** 系统将请求体中 `model` 替换为 `deepseek-v4-flash`，转发到 dashscope 的 `base_url`，Authorization 使用 dashscope 的 `api_key`

### Requirement: Provider 抽象接口
系统 SHALL 定义 Provider 接口，包含 `Name()`、`Type()`、`ChatCompletion()`、`ListModels()` 方法。第一版仅实现 `openai` 类型，但架构 MUST 支持新增协议类型只需实现该接口。

#### Scenario: 新增 Provider 类型
- **WHEN** 开发者需要支持新的 LLM 协议（如 Anthropic）
- **THEN** 只需实现 Provider 接口并注册到工厂函数，无需修改核心代理逻辑

### Requirement: Provider 连接池复用
每个 Provider SHALL 复用底层 HTTP 连接，使用独立的 `http.Client` 实例，配置合理的连接池参数。

#### Scenario: 高并发下连接复用
- **WHEN** 500 个并发请求路由到同一个 Provider
- **THEN** 系统复用 TCP 连接，不出现连接泄漏或 excessive connection creation
