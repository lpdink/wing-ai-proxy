好的，我已经完整 review 了整个项目的所有源文件、测试、配置和构建脚本。以下是我从代码架构到每一行具体实现的系统性审查结果。

🏗 架构与设计
整体评价：结构清晰，遵循了 Go 标准项目布局和 design.md 的设计决策。各个包职责分明，接口抽象合理。

wing-ai-proxy/
├── cmd/wing-ai-proxy/main.go    ✓ 入口整洁，组件组装清晰
├── internal/
│   ├── config/                  ✓ 加载、校验、热加载分离
│   ├── proxy/                   ✓ handler + 代理核心
│   ├── provider/                ✓ 接口+实现+注册表
│   ├── audit/                   ✓ record + store + sse聚合
│   ├── metrics/                 ✓ Prometheus 指标
│   └── middleware/              ✓ 鉴权 + request_id
├── migrations/                  ✓ SQL schema
├── Makefile                     ✓ 完整
└── go.mod                       ✓ 依赖管理

🔴 严重问题 (Critical)
1. SSE Flusher 类型断言会 panic
文件: internal/proxy/handler.go:130

forwarder := audit.NewStreamForwarder(resp.Body, c.Writer, c.Writer.(interface{ Flush() }))
c.Writer.(interface{ Flush() }) 是硬类型断言，如果 c.Writer 不实现 Flush() 方法会 panic。Gin 的 c.Writer 在 ReleaseMode 下是 *gin.responseWriter，它包装了 http.ResponseWriter，而 http.ResponseWriter 接口本身不保证实现 http.Flusher。
修复方案：使用 ok-idiom 的安全断言 + fallback：

flusher, _ := c.Writer.(interface{ Flush() })
if flusher == nil {
    // HTTP/1.1 代理场景下通常有 Flusher，但安全起见做 guard
    logger.Warn("response writer does not support flushing")
}
forwarder := audit.NewStreamForwarder(resp.Body, c.Writer, flusher)
2. Provider 接口泄漏实现细节 — getModelMap 破坏抽象
文件: internal/provider/provider.go:247-252

func getModelMap(p Provider) string {
    // Type-assert to get the internal model map
    if op, ok := p.(*openAIProvider); ok {
        return op.models
    }
    return nil
}
这是一个 OCP 违反。每添加一个新 Provider 类型，都必须修改这个函数。Registry 不应该知道 Provider 的内部实现细节。
修复方案：在 Provider 接口上增加 Models() map[string]string 方法：

type Provider interface {
    Name() string
    Type() string
    Models() map[string]string  // show_name → real_name
    ChatCompletion(...) ...
    ListModels(...) ...
}
3. go.mod 中 go 1.25.0 指向不存在的 Go 版本
文件: go.mod:3

go 1.25.0
截至知识截止日期，Go 1.25 尚未发布。这会导致某些 CI 工具链解析失败。应改为当前稳定版本 1.22 或 1.23。
影响：actions/setup-go@v5 使用 go-version-file: go.mod 读取此版本号，在 CI 中可能因为找不到对应 Go 版本而失败。

🟠 中等问题 (Major)
4. 迁移 SQL 双重维护 — migration SQL 文件被忽略
文件: internal/audit/store.go:13 vs migrations/001_create_audit_table.sql
两处维护完全相同的 SQL 语句。迁移文件 (migrations/001_create_audit_table.sql) 从未被代码使用。违背了 design.md 中的决策（"embed 内嵌 migration SQL"）。
修复方案：使用 //go:embed 引用迁移文件，消除冗余：

//go:embed ../../migrations/001_create_audit_table.sql
var migrationSQL string
5. TestIsTimeoutError 测试中存在无意义断言
文件: internal/provider/provider_test.go:280-286

if IsTimeoutError(context.DeadlineExceeded) {
    // Actually this should return true  ← 注释代替了断言！
}
if !IsTimeoutError(context.DeadlineExceeded) {
    t.Error("DeadlineExceeded should be timeout")
}
第一个 if 块什么都不做。这段代码让人困惑，应该合并为：

if !IsTimeoutError(context.DeadlineExceeded) {
    t.Error("DeadlineExceeded should be timeout")
}
6. mockProvider 在测试文件中定义了但未实现 Provider 接口
文件: internal/proxy/handler_test.go:30-45

func (m *mockProvider) ChatCompletion(_ interface{ Done() <-chan struct{} }, body []byte, _ http.Header) (*http.Response, error) {
方法签名与 Provider 接口不匹配（参数类型为 interface{ Done() <-chan struct{} } 而不是 context.Context）。虽然这段代码因为未使用而没触发编译错误，但它是死代码，且给后续阅读者造成误导。
建议：直接删除这个未使用的 mock struct，因为实际测试使用的是 setupTestRouter + httptest.Server 方案。
7. EnsureConfigDir 总是创建默认目录，即使指定了自定义配置路径
文件: cmd/wing-ai-proxy/main.go:34-36

if err := config.EnsureConfigDir(); err != nil {
    slog.Warn("failed to create config directory", "error", err)
}
这一行在加载配置之前执行，且总是创建 ~/.wing-ai-proxy/。如果用户指定了 wing-ai-proxy /etc/myapp/config.yaml，这个默认目录仍会被创建，造成副作用。
修复方案：将此调用延迟到确定使用默认路径之后，或者仅在默认路径场景下调用。
8. initLogger 调用滞后于日志记录
文件: cmd/wing-ai-proxy/main.go:34-38

config.EnsureConfigDir()           // ← 使用默认 slog，文本格式
...
cfg, err := config.Load(configPath)
...
initLogger(cfg.LogLevel)           // ← JSON handler 在之后才设置
EnsureConfigDir 中的 slog.Warn 以默认文本格式输出，而项目规范要求全 JSON 日志。应该先初始化 logger 再做其他操作，或者在 run() 的最开始设置一个 JSON fallback logger。

🟡 值得关注 (Minor)
9. 非流式 handler 不检查 c.Writer.Write 返回值
文件: internal/proxy/handler.go:138

c.Writer.Write(respBody)
返回值被丢弃。如果写入过程中客户端断连，这个错误被静默忽略了。虽然不是严重问题，但至少应该记录日志：

if _, err := c.Writer.Write(respBody); err != nil {
    logger.Warn("failed to write response", "error", err)
}
10. StreamForwarder.Run() 没有 context 感知
文件: internal/audit/sse.go:223-245
当客户端提前断开连接时，Gin 会取消 c.Request.Context()，但 StreamForwarder.Run() 没有接收 context 参数，无法感知取消信号。它会继续从上游读取数据（虽然上游请求会因 context 取消而终止），但客户端写入会失败并被忽略。
建议：让 StreamForwarder.Run(ctx context.Context) 在每次迭代中检查 ctx.Done()：

select {
case <-ctx.Done():
    return ctx.Err()
default:
}
11. 所有上游响应头透传可能泄露内部信息
文件: internal/proxy/handler.go:130-137 和 internal/proxy/handler.go:183-189

for key, vals := range resp.Header {
    for _, v := range vals {
        c.Header(key, v)
    }
}
Set-Cookie、Server、X-Powered-By 等上游响应头可能会透传给客户端。应该建立白名单或至少过滤掉 Set-Cookie。
12. AsyncWriter 的 worker 没有 panic recovery
文件: internal/audit/store.go:154-203
如果 worker() 中 panic（例如 SQLite 写入 panic），整个 goroutine 会崩溃，且无法恢复。建议在 worker 中添加 defer recover()：

func (w *AsyncWriter) worker() {
    defer close(w.done)
    defer func() {
        if r := recover(); r != nil {
            slog.Error("audit worker panicked", "recover", r)
        }
    }()
    // ...
}
13. Debounce 使用 time.AfterFunc 而非 timer.Reset
文件: internal/config/watcher.go:86-89

if timer != nil {
    timer.Stop()
}
timer = time.AfterFunc(w.debounce, w.reload)
高频文件变更场景下，反复创建和销毁 Timer 效率不高。标准的 debounce 实现是复用单个 Timer + Reset()：

if timer == nil {
    timer = time.NewTimer(w.debounce)
}
timer.Stop()  // don't forget to drain
timer.Reset(w.debounce)
14. 审计表 first_byte_at 和 request_end 在 SQL 中定义为 NOT NULL，但错误路径可能设置为零值
文件: internal/audit/record.go / migrations/001_create_audit_table.sql
当上游请求在获得任何响应之前就失败时（如 DNS 解析失败），ar.FirstByteAt 保持零值。插入 SQLite 时 time.Time{} 的 .Format() 会输出 "0001-01-01T00:00:00Z"，虽然不违反 NOT NULL 约束，但语义上是错误数据。
建议：要么将 SQL 中 first_byte_at 和 request_end 改为允许 NULL，要么在插入时对零值做特殊处理（如使用 NULL）。
15. go.mod 中的可疑间接依赖

go.mongodb.org/mongo-driver/v2 v2.5.0      # 非标准路径
go.yaml.in/yaml/v2 v2.4.2                   # 非标准路径
这些看起来像是依赖混淆攻击的潜在目标。需要确认它们是否为 modernc.org/sqlite 或 goccy/go-yaml 的合法传递依赖。建议运行 go mod why -m <module> 追溯引入原因，如果确实是传递依赖，考虑是否值得信任。

⚪ 风格与最佳实践 (Style)
16. 响应头处理 handleNonStream 中的顺序微妙
handleNonStream 先 c.Header() 设置头，然后 c.Status() 写入状态码，最后 c.Writer.Write() 写体。这在 Gin 中是正确的，因为 c.Status() 在调用 WriteHeader 的时机正确，但代码的视觉顺序让人不安。建议加一个注释说明 Intent。
17. SSE 聚合器不追踪 finish_reason
文件: internal/audit/sse.go
流式响应中 choice.FinishReason（"stop" / "length" / "tool_calls"）被忽略。这可以用于诊断流式响应是否被截断。
18. IsTimeoutError 用字符串匹配作为 fallback

return strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline exceeded")
国际化场景下可能失败（某些 Go 版本/平台的消息不同）。建议仅依赖 context.DeadlineExceeded 和 net.Error.Timeout()。
19. ReadBody 函数存在但从未使用
文件: internal/provider/provider.go:271-274

func ReadBody(resp *http.Response) ([]byte, error) {
    defer resp.Body.Close()
    return io.ReadAll(resp.Body)
}
proxy/handler.go 中每次都手动 io.ReadAll(resp.Body) + defer resp.Body.Close()。要么删除这个未使用函数，要么在 handler 中复用。
20. 测试函数 setupTestRouter 使用 :memory: SQLite
虽然 :memory: 支持并发连接（WAL 模式下），但 SQLite :memory: 默认是独立的连接，多个连接看到不同的数据库。setupTestRouter 只创建一个连接，所以没问题。但需要注意这个模式的限制。

✅ 做得很好的地方

 1.  项目结构 → 完美遵循 Go 标准布局，internal/ 隔离，cmd/ 入口。
 2.  配置验证 → 覆盖了所有边界情况（空字段、重复、端口范围、模型映射空值）。
 3.  热加载 → debounce、不可变字段检测、原子替换，实现质量高。
 4.  审计异步写入 → channel + 单 worker、队列满降级、批量事务，架构设计优秀。
 5.  SSE 聚合器 → 正确处理 tool_calls delta 合并、usage 提取、截断保护。
 6.  Prometheus 指标 → 合理的维度标签（provider/model/status），首字节延迟 histogram 有价值。
 7.  优雅关闭 → SIGINT/SIGTERM 处理、server shutdown timeout、audit drain、资源清理顺序正确。
 8.  测试覆盖 → 各层都有单元测试、mock HTTP server、异步写入正确性验证、SSE 聚合验证、竞态检测。
 9.  Makefile → 完整的 build/test/lint/fmt/run/clean/release 目标。

总体评价：这是一个架构清晰、实现质量优秀、测试覆盖率高的 Go 项目。核心设计理念（抽象层、异步审计、热加载、可观测性）得到了正确实现。上述问题多数是边界情况和防御性编程的细节，不影响核心功能，但修复后可以使项目达到生产级健壮性。