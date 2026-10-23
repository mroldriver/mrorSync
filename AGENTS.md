# AGENTS.md

给在本仓库工作的 AI / 自动化代理的说明。**用户文档看 [README.md](README.md)**，
这里只写“怎么改、不能破坏什么”。

## 项目概要

- 百度网盘整盘增量备份工具，Go ≥ 1.21，**纯标准库、零第三方依赖**（`go.mod` 没有
  `require`，也没有 `go.sum`）——这是卖点，不要引入任何外部包。
- 入口 `main.go` → `internal/syncer.Run`；产物是单文件 `mrorsync.exe`（约 7 MB）。
- 平台：主要面向 Windows，代码可交叉编译到 Linux/NAS。

## 环境

- shell 用 PowerShell；Go 可能不在 PATH，本机在
  `%LOCALAPPDATA%\go-toolchain\go\bin\go.exe`，`build.ps1` 会自动按
  `$env:GO_BIN` → PATH → 该目录查找。
- Node/npm 已移除，不要再引用 `.mjs` 或加 `package.json`。
- 没有 C 编译器，`go test -race` 跑不了；**不要声称跑过**。

## 常用命令

| 目的 | 命令 |
|---|---|
| 构建 | `.\build.ps1` |
| 构建 + 测试 | `.\build.ps1 -Test` |
| 测试 | `go test ./...` |
| 格式化 | `go fmt ./...` |
| 静态检查 | `go vet ./...` |
| 全部检查 | `.\ci.ps1 -Build` |
| 只扫描不联网 | `.\mrorsync.exe` |
| 版本 | `.\mrorsync.exe --version` |

## 目录与职责

```text
main.go                CLI 解析、login 子命令、退出码
internal/config        config.json 读写、默认值、校验、原子保存（保留未知字段）
internal/baidu         xpan API：重试、token 续期、目录缓存、分片上传、删除
internal/fsutil        WriteFileAtomic / WriteFileFast、运行锁
internal/syncer        扫描、manifest、分片哈希缓存、上传引擎、last-run
main_test.go           端到端测试：httptest 假 xpan + 临时目录
build.ps1              找 Go 工具链并编译（注入提交号）
ci.ps1                 gofmt / vet / test（CI 可直接调用）
```

## 不可破坏的约定

1. **状态格式兼容**：`manifest.json` 的 key 是 `/` 分隔的相对路径，记录字段
   `size`/`mtime`/`mtime_ms`/`md5`/`fs_id`/`state`/`utime`。改语义会让用户升级后
   全量重传。
2. **毫秒取整**：`mtime_ms` 必须走 `roundMillis`（模拟 JS `Math.round`）。直接
   `time.UnixMilli` 会截断，和旧记录差 1 ms，导致每次全量重算 md5。
3. **配置容错**：`Config` 用 `raw` 保留未知字段，token 续期写回不能丢键；新增字段
   必须允许缺失（`Normalize` 补默认值），旧 config 要能直接跑。
4. **原子写**：config、manifest、blocks 缓存、`last-run.json` 一律
   `fsutil.WriteFileAtomic`（缓存可用 `WriteFileFast`）；manifest 每 1000 条 `Put`
   落盘一次，锁内只快照、锁外序列化写盘，退出前 `Flush`。
5. **token 安全**：任何日志/错误都不能打印 access_token、refresh_token；`config.json`
   已在 `.gitignore`，不要提交，也不要放进测试夹具。
6. **退出码契约**：`0` 全成功，`1` 有文件失败（下次重试），`2` 配置/参数错误，
   `130` Ctrl+C。改行为要同步 README。
7. **删除保护**：仅当 `--only` 为空、未 `--no-delete`、扫描无目录错误时才删；待删除
   超过 `max(tuning.delete_protect_min, 清单*tuning.delete_protect_percent%)` 且没有
   `--force-delete` 时整体放弃删除；`--limit` 模式永不删远端。不要放宽这些条件。
8. **重试语义**：网络错误/5xx 最多 5 次（1s/2s/4s/8s 起，带 jitter，上限 8s）；
   `31034`/`31363` 走 5s/10s/20s/40s 的独立退避（上限 40s）；HTTP 429 读 `Retry-After`
   （秒数或 HTTP-date）；普通 4xx 不重试；`-6`/`110`/`111` 先 refresh 再
   **重建请求**重发；并发 refresh 要互斥去重（refresh_token 是一次性的）。
9. **并发**：文件级 `max_workers`（1–16），文件内分片 `part_workers`（1–8），全部
   用 `context`；Ctrl+C 要取消在途请求并落盘 manifest。
10. **分片规则**：固定 4 MB，末片按实际大小；单文件最多 1024 片；超过
    `max_file_size`（默认 4 GB）打印 `[skip]` 跳过。
11. **单实例锁**：`Run` 必须在 state 目录持有 `.state/run.lock`，并在所有返回路径释放；
    新增提前返回的分支不要绕过 `defer lock.Release()`。锁超过
    `tuning.lock_stale_hours`（默认 24h）视为残留可接管。
12. **认证错误短路**：`baidu.IsAuthError`（errno -6/110/111、HTTP 401）必须终止整轮并
    跳过删除，不能降级成“每个文件失败一次”；HTTP 403 不算认证失败（PCS 用它表示瞬时
    IDC 问题），文件级错误（如 31066）仍只记单个失败。
13. **日志**：所有输出走 `logf`（带时间戳、加锁）；不要在业务代码里直接写 stdout，
    也不要按字节截断错误响应文本。
14. **配置分层**：优先级固定为「CLI > `MRORSYNC_*` 环境变量 > `config.json` > 默认值」。
    新增配置字段必须给默认值、允许旧文件缺失，并同步 `Normalize`/`applyEnv`/README；
    运维阈值放 `tuning`，不要再把数字写死在代码里。
15. **两种排除**：`excludes` 按文件名/目录名匹配（任意层级同名都命中）；`exclude_paths`
    按相对 `root` 的路径匹配（命中目录则跳过整棵子树）。两者语义不要合并。

## 测试要求

- 改 `internal/syncer`、`internal/baidu` 的行为，必须跑 `go test ./...`。
- **不准访问真实网盘**：测试一律用 `httptest` 假服务端 + `t.TempDir()`，通过
  `BAIDU_PAN_API` / `BAIDU_PCS_API` / `BAIDU_OAUTH_API` / `BAIDU_SYNC_STATE_DIR`
  注入，沿用 `main_test.go` 里 `env` 的做法。
- 测试按场景拆分为独立 `TestXxx`，每个复用 `setup(t)` 获得自己的临时目录；新增场景
  请新建测试，不要恢复成共享状态的巨型用例，也不要让测试访问真实网盘。
- 覆盖重点：首次上传、二次跳过、`--full`、`--verify`、坏分片缓存自愈、token 过期、
  删除保护、`--limit`、限流退避、`--only`、参数校验、`last-run.json`。

## 代码风格

- 提交前 `gofmt`；注释解释“为什么”，尤其兼容性和安全相关的逻辑。
- 用户可见文案用中文，沿用 `[skip]` / `[保护]` / `[提示]` 前缀；错误优先带上下文
  （路径、errno），不要吞错。
- 业务代码不要直接 `os.Exit` 或 `fmt.Print`：输出走 `opts.Out`（`io.Writer`），
  便于测试；错误往上返回。
- 路径：内部逻辑用 `/` 分隔的相对路径，落盘/系统调用前再 `filepath.FromSlash`。

## Git 工作流

- 当前在 `develop`（Gitee 默认分支）；`master` 只用于发布，别直接改。
- 提交前：`.\build.ps1 -Test` 通过。
- 提交信息建议 `<type>: <说明>`（`feat` / `fix` / `docs` / `refactor` / `test` /
  `chore`），一个提交一件事。
- 不 commit：`config.json`、`.state/`、`mrorsync.exe`（都已在 `.gitignore`）。
- 不 force push，不改写已推送历史。

## 常见坑

- **百度 API 契约**：`method`、`access_token` 必须放 query string；`filemanager` 的
  `opera`/`async` 也必须在 query，只有 `filelist` 放 body。放错位置分别得到
  HTTP 400 `Unsupported open api` 和 `errno=2`。
- PCS 分片上传的 `403 -11015 dest idc equal src idc` 是瞬时 IDC 路由问题，可重试；
  不要把 HTTP 403 当成认证失败（只有 401 和 errno -6/110/111 才短路整轮）。
- `precreate` 返回的 `block_list` 是**待上传的分片序号**（数字），不是 md5；旧版/测试
  返回 md5 列表，解析时按元素类型区分。
- CRLF 警告来自 Windows 换行，不影响构建。
- Windows 上 0600 权限由 ACL 决定，别在文档里承诺 POSIX 权限。
- `precreate` 返回 `return_type=2` 是秒传；非 2 且没有 `uploadid` 是错误；空文件用
  `EmptyMD5` 当作唯一分片。
- 同一远端目录只 `create` 一次，并发请求按路径去重等待，不要改成重复创建。
- 扫描跳过符号链接/junction，防止链接环卡死。
- 状态目录优先级：`config.state_dir` > `MRORSYNC_STATE_DIR` > `BAIDU_SYNC_STATE_DIR`
  > `<exe>/.state`。
- `--verify` 必须配合 `--apply`，`Options.Validate` 会拒绝单独使用。

## 改完自检

- [ ] `go fmt ./...`、`go vet ./...` 干净
- [ ] `go test ./...` 通过（或直接 `.\ci.ps1 -Build`）
- [ ] 改 CLI/配置/状态/行为 → 同步更新 README
- [ ] 改版本 → `internal/syncer/engine.go` 的 `Version` + README 变更记录
- [ ] 交付 exe → `.\build.ps1` 重新构建
- [ ] 新增约定 → 更新本文件

## 变更

- 2026-10-22：初版，对应 3.0.0。
- 2026-10-22：3.0.1，补充单实例锁、认证短路、日志格式约定。
- 2026-10-22：3.0.2，manifest 批量刷盘、缓存无 fsync + 清理、退避 jitter。
- 2026-10-22：3.0.3，删除计数、panic 兜底、实传统计、重试上限。
- 2026-10-22：3.0.4，版本注入提交号、ci.ps1、拆分 upload.go、清理死代码。
- 2026-10-22：3.0.5，测试按场景拆分、--limit 日志顺序、OKRels 单次遍历。
- 2026-10-22：3.0.6，配置版本/tuning/环境变量/CLI 覆盖、print-config/check-config。
- 2026-10-22：3.0.7，真实 API 契约修正（query 参数、block_list 序号），完成真机上传/删除验证。
- 2026-10-23：3.0.8，新增按路径排除（`exclude_paths` / `--exclude-path` / 环境变量）。
- 2026-10-23：3.0.9，修复 403 误判认证失败，-11015 按瞬时错误重试。
