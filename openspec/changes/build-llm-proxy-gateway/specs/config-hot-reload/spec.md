## ADDED Requirements

### Requirement: 配置文件变更监听
系统 SHALL 使用 fsnotify 监听配置文件（默认 `~/.wing-ai-proxy/config.yaml`）的变更事件，检测到变化后自动触发热加载流程。

#### Scenario: 配置文件修改后自动加载
- **WHEN** 运维修改并保存配置文件
- **THEN** 系统在 300ms debounce 后触发重载，若新配置合法则自动生效，无需重启进程

#### Scenario: 配置文件被删除
- **WHEN** 配置文件被删除
- **THEN** 系统输出 WARN 日志，保持当前配置不变，继续监听（等待文件重新创建）

### Requirement: 热加载原子替换
热加载 SHALL 遵循 "先验证后替换" 原则。新配置 MUST 完整解析和校验通过后，才原子替换内存中的配置。校验失败时保持原配置不变。

#### Scenario: 新配置合法
- **WHEN** 新配置文件语法正确、结构完整
- **THEN** 系统用写锁替换内存中的 Provider 列表、模型映射表、虚拟 Key 集合，输出 INFO 日志说明更新内容（如 "added provider X, removed model Y"）

#### Scenario: 新配置语法错误
- **WHEN** 新配置文件存在 YAML 语法错误
- **THEN** 系统输出 ERROR 日志，详细说明错误位置和原因，保持原配置不变

#### Scenario: 新配置字段缺失
- **WHEN** 新配置文件缺少必要字段（如 provider 缺少 `base_url`）
- **THEN** 系统输出 ERROR 日志，指出缺失字段，保持原配置不变

### Requirement: 可热加载的配置项
热加载 SHALL 仅更新运行时可变更的配置项：`virtual_api_keys`、`providers`（含 models 映射）。`host`、`port`、数据库路径等启动时绑定的配置 MUST NOT 通过热加载变更。

#### Scenario: 修改虚拟 Key 列表
- **WHEN** 配置文件中 `virtual_api_keys` 新增或删除 key
- **THEN** 热加载后新 key 立即生效（可访问），被删除的 key 立即失效（返回 401）

#### Scenario: 修改 host/port
- **WHEN** 配置文件中 `host` 或 `port` 被修改
- **THEN** 热加载忽略这些字段的变更，输出 WARN 日志说明这些配置需要重启生效

### Requirement: Debounce 防抖
热加载 SHALL 对 fsnotify 事件做 300ms debounce，防止编辑器保存时触发多次无意义的重载。

#### Scenario: 编辑器多次触发保存事件
- **WHEN** 编辑器在 200ms 内触发 3 次文件写入事件
- **THEN** 系统仅在最后一次事件后 300ms 执行一次重载
