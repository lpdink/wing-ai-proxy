## Context

全新项目，从零构建。目标是一个部署在内网的高性能 LLM 代理网关，日均请求量预估在万级到十万级。客户端通过虚拟 API Key 访问，网关负责鉴权、路由、透传和审计。上游主要是 DashScope（阿里云）和 OpenAI 兼容接口。

约束条件：
- Go 1.24，Gin 框架
- 单机部署，不引入分布式组件
- SQLite 作为第一版存储，预留 PostgreSQL 切换能力
- 代理本身增加的延迟 < 10ms

## Goals / Non-Goals

**Goals:**
- 实现完整的 OpenAI 兼容代理，支持流式/非流式透传
- 全量审计所有请求，异步写入不影响主链路延迟
- 配置热加载，运维友好
- 清晰的 Provider 抽象层，新协议只需新增实现
- Prometheus 指标暴露，支持 Grafana 可视化

**Non-Goals:**
- 不做负载均衡或限流（第一版单机直连，后续按需添加）
- 不做 Anthropic 等非 OpenAI 协议的实际实现（仅预留接口）
- 不做 Web 管理界面
- 不做自动数据清理
- 不做集群部署或高可用方案

## Decisions

### 1. 项目结构

```
wing-ai-proxy/
├── cmd/wing-ai-proxy/main.go    # 入口：初始化、组装依赖、启动服务
├── internal/
│   ├── config/                   # 配置加载、校验、热加载
│   ├── proxy/                    # HTTP handler + 代理核心逻辑
│   ├── provider/                 # Provider 接口与实现
│   ├── audit/                    # 审计记录结构 + 存储接口 + SQLite 实现
│   ├── metrics/                  # Prometheus 指标定义
│   └── middleware/               # Gin 中间件（鉴权、request_id）
├── migrations/                   # SQLite schema SQL 文件（embed）
├── Makefile
└── go.mod
```

**理由**：`internal/` 防止外部导入，保持 API 边界清晰。`cmd/` 遵循 Go 标准项目布局。按职责分包，每个包足够小便于单独测试。

### 2. HTTP 框架：Gin

**选择**：Gin  
**替代方案**：标准库 `net/http` + chi  
**理由**：需求明确指定 Gin。Gin 的路由、中间件机制成熟，SSE 支持通过 `c.Stream()` 实现，生态丰富。

### 3. SQLite 驱动：modernc.org/sqlite

**选择**：`modernc.org/sqlite`（纯 Go 实现）  
**替代方案**：`mattn/go-sqlite3`（CGo）  
**理由**：纯 Go 实现无需 CGo，交叉编译简单，部署无额外依赖。性能差异在异步写入场景下可忽略。使用标准 `database/sql` 接口，切换 PostgreSQL 只需更换驱动和少量 SQL 方言。

### 4. 日志：slog（标准库）

**选择**：Go 1.21+ 内置 `log/slog`  
**替代方案**：zerolog  
**理由**：slog 是标准库，零依赖，支持 JSON 输出和结构化字段。Go 1.24 的 slog 已经足够成熟，且 `slog.With("request_id", id)` 天然支持 request-scoped 日志。无需引入第三方库。

### 5. 审计异步写入：channel + 单 worker

**选择**：buffered channel（容量 4096） + 单 goroutine worker  
**替代方案**：worker pool 多并发写入  
**理由**：SQLite 单写者模型天然适合单 worker。channel 做缓冲，主请求路径 `select + default` 非阻塞投递（队列满时降级为 WARN 日志丢弃）。单 worker 避免锁竞争，实现简单且性能足够。批量写入用事务包裹提升吞吐。

### 6. 配置热加载：fsnotify + 原子替换

**选择**：`fsnotify` 监听配置文件 + `sync.RWMutex` 保护的原子指针替换  
**替代方案**：SIGHUP 信号触发重载  
**理由**：fsnotify 跨平台，自动感知文件变化，运维无需手动发信号。重载流程：读文件 → 解析 → 校验 → 成功则写锁替换内存配置，失败则打 error 日志保持原配置不变。加 debounce（300ms）防止编辑器多次保存触发重复加载。

### 7. Provider 抽象

```go
type Provider interface {
    Name() string
    Type() string
    ChatCompletion(ctx context.Context, req *http.Request) (*http.Response, error)
    ListModels(ctx context.Context) ([]ModelInfo, error)
}
```

每个 Provider 实现此接口。`provider.Registry` 维护 `show_name → Provider` 的映射表。新协议（如 Anthropic）只需新增 `type Provider` 的实现和对应的工厂函数。

### 8. 流式 SSE 处理

代理对上游响应使用 `io.Copy` 风格的逐 chunk 转发：
1. 读取上游 response body，按 `\n\n` 分割 SSE event
2. 每收到一个 event，立即 `fmt.Fprintf(clientWriter, ...)` + `Flush()`
3. 同时将 event 追加到内存 buffer
4. 上游结束后，将 buffer 交给 SSE aggregator 解析合并成完整 response JSON
5. 审计记录异步写入

### 9. 鉴权模型

简单的虚拟 API Key 字符串比对。通过 Gin 中间件在路由前置拦截，从 `Authorization: Bearer <key>` 提取 key，在内存 map 中 O(1) 查找。不存在则返回 401。

## Risks / Trade-offs

- **[SQLite 写入瓶颈]** → 单 worker + 事务批量写入，理论吞吐 ~5000 writes/s，远超单机代理需求。若未来超限，切换 PostgreSQL。
- **[SSE 聚合内存占用]** → 超长对话（如 100K+ tokens 的响应）可能占用大量内存。对聚合 buffer 设置上限（默认 10MB），超出部分截断并标记。
- **[fsnotify 编辑器噪声]** → 某些编辑器保存时会先删除再创建文件，触发多次事件。加 300ms debounce 解决。
- **[Provider 上游超时]** → 默认 30min 超时（匹配 LLM 长推理场景），客户端可自行设置更短超时。代理统一返回 504。
- **[虚拟 Key 明文存储]** → 内网环境可接受。未来若需加密，仅修改鉴权中间件。
