# mrorSync

> 百度网盘整盘增量备份：扫描本地目录，只上传变化的文件，把本地删掉的文件同步删除。

单文件 exe、纯 Go 标准库、无运行时依赖。支持秒传与 4 MB 分片去重、token 自动续期、
断点续传、误删保护、原子写状态。当前版本 **3.0.0**，仓库：
<https://gitee.com/mroldriver/mrorSync>

## 特性

| 特性 | 说明 |
|---|---|
| 增量同步 | 以 `size` + `mtime`（毫秒）判定，未变化直接跳过，不读盘 |
| 秒传 | `precreate` 命中已有分片时 `return_type=2`，不传数据 |
| 分片去重 | 4 MB 分片 md5 缓存在 `.state/blocks/`，只补传缺失分片 |
| 断点续传 | 失败文件不进 manifest 的 `ok`，下次自动重试 |
| 自动续期 | access_token 过期时用 refresh_token 换新，原子写回 `config.json` |
| 删除保护 | 删除比例超过清单 20% 时拒绝执行，需显式 `--force-delete` |
| 单文件部署 | 无 CGO，可交叉编译到 Linux/NAS |
| 可测试 | 内置假 xpan 服务端端到端测试，不触碰真实网盘 |

## 工作原理

```
扫描 root ──► 与 .state/manifest.json 比对（size + mtime_ms）
                   │
        ┌──────────┴───────────┐
     未变化                 变化 / 新增
        │                      │
      跳过            一次读盘：整文件 md5 + 每 4MB 分片 md5
                               │
                     precreate（提交 size + block_list）
                               │
              return_type=2 ───┴──► 秒传，不传数据
                               │
                        返回缺失分片 ──► 并发上传 → superfile2 合并
                               │
                       写 manifest（state=ok, fs_id, utime）
```

扫描完成后做远端清理：远端存在、本地已删除、且清单里 `state=ok` 的条目会被删除
（网盘回收站，不是彻底删除）。

## 快速开始

### 1. 申请应用

到[百度网盘开放平台](https://pan.baidu.com/union/console/applist)创建应用，拿到
`app_key` / `secret_key`，回调地址填 `oob`（授权码模式）。

### 2. 写配置

```powershell
Copy-Item config.example.json config.json
notepad config.json
```

至少填 `app_key`、`secret_key`；按需改 `root`、`remote_base`。

### 3. 授权

```powershell
.\mrorsync.exe login
```

浏览器打开授权页，把授权码粘回终端。token 写回 `config.json`（原子写，0600），
之后会自动续期，一般不需要再手动登录。

### 4. 先看再做

```powershell
.\mrorsync.exe                       # 只扫描打印，不联网
.\mrorsync.exe --apply --limit 20    # 小批量试传，不删远端
.\mrorsync.exe --apply               # 正式同步
```

升级或怀疑远端漂移时做一次核对：

```powershell
.\mrorsync.exe --apply --verify --limit 200
```

## 构建

需要 Go ≥ 1.21。本机 Go 1.27.3 在 `%LOCALAPPDATA%\go-toolchain\go\bin`（未加入 PATH），
`build.ps1` 按 `$env:GO_BIN` → PATH → 该目录的顺序查找。

```powershell
.\build.ps1            # 生成 mrorsync.exe
.\build.ps1 -Test      # 编译 + go test ./...
.\ci.ps1 -Build        # 格式检查 + vet + 测试 + 构建（CI 可直接调用）
```

手动编译：

```powershell
go build -trimpath -ldflags "-s -w" -o mrorsync.exe .
```

交叉编译到 Linux/NAS（无 CGO）：

```powershell
$env:GOOS="linux"; $env:GOARCH="amd64"; go build -trimpath -o mrorsync .
```

## 配置项

优先级：**命令行 > `MRORSYNC_*` 环境变量 > `config.json` > 内置默认值**。
配置文件默认取 `$MRORSYNC_CONFIG`，否则 exe 同目录的 `config.json`，`--config` 可再次
覆盖。未知字段会原样保留（token 续期写回时不会丢）。路径支持 `D:/` 或 `D:\`。
`config_version` 当前为 1；未来不兼容的格式会因为版本号被拒绝，而不是被误读。

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `config_version` | int | `1` | 配置格式版本；旧文件缺省按 1 处理 |
| `root` | string | `D:\` | 要备份的本地根目录 |
| `remote_base` | string | `/备份/D盘` | 远端根路径；`/` 表示直接放网盘根（模板示例即 `/`） |
| `app_key` | string | 空 | 应用的 API Key（即 client_id） |
| `secret_key` | string | 空 | 应用的 Secret Key |
| `access_token` | string | 空 | 授权后由 `login` 写入 |
| `refresh_token` | string | 空 | 同上，用于自动续期 |
| `max_workers` | int | `2` | 同时上传的文件数，范围 1–16；>3 会提示限流风险 |
| `part_workers` | int | `2` | 单个文件内并发上传的分片数，范围 1–8 |
| `max_file_size` | int | `4294967296` | 单文件上限（字节），默认 4 GB |
| `state_dir` | string | `<exe>/.state` | 状态目录；也可用环境变量覆盖 |
| `excludes` | string[] | 见下 | 按文件名/目录名跳过（任意层级） |
| `exclude_paths` | string[] | 空 | 按相对路径跳过某个目录/文件（可精确到子树） |
| `tuning` | object | 见下 | 运维调参，可选；不写就用默认值 |

`app_id`、`sign_key` 仍会被保留以免旧配置报错，但当前版本不使用。

### tuning（可选）

旧配置不写这一段也能直接运行；越界值会被夹取并在启动时打印 `[警告]`，不再静默修改。

| 字段 | 默认 | 范围 | 说明 |
|---|---|---|---|
| `retry_tries` | `5` | 1–10 | 单次请求最多尝试次数 |
| `backoff_max_seconds` | `8` | 1–300 | 普通重试退避上限 |
| `busy_backoff_max_seconds` | `40` | 1–600 | 31034/31363 限流退避上限 |
| `http_timeout_seconds` | `180` | 5–3600 | 单次 HTTP 请求超时 |
| `delete_protect_percent` | `20` | 1–100 | 待删除超过清单该比例时中止删除 |
| `delete_protect_min` | `20` | 0–1000000 | 比例保护的绝对下限（个数） |
| `lock_stale_hours` | `24` | 1–720 | 运行锁视为残留的小时数 |
| `cache_gc_hours` | `24` | 1–8760 | 只清理超过该年龄的孤立分片缓存 |
| `verify_progress_every` | `2000` | 0–1000000 | `--verify` 进度日志间隔；0 关闭 |

未配置 `excludes` 时代码内置：`$RECYCLE.BIN`、`System Volume Information`、
`node_modules`、`.git`、`.state`；`config.example.json` 模板额外包含 `pagefile.sys`、
`hiberfil.sys`，可直接按需增删。

匹配规则：按**文件名**（不是完整路径）匹配，大小写不敏感，支持 `*` 通配
（如 `*.class`、`*.tmp`）；不支持 `dir/**` 这类路径通配。

### 按路径排除

`excludes` 是“名字”匹配，任何层级出现同名目录都会被跳过；如果只想排除某一个具体目录，
用 `exclude_paths`（相对 `root`，`/` 或 `\` 均可，大小写不敏感）：

```json
"exclude_paths": ["BaiduSyncdisk", "项目A/临时"]
```

- 命中目录 → 整个子树都不扫描；命中文件 → 只跳过该文件
- 目录名相同但位置不同的其他目录不受影响（例如 `D:\项目A\临时` 被排除，`D:\项目B\临时` 不会）
- 绝对路径若位于 `root` 下会自动转成相对路径；不在 `root` 下的条目会打印警告并忽略
- 命令行可临时追加（可重复）：`.\mrorsync.exe --exclude-path 项目A/临时`
- 环境变量：`MRORSYNC_EXCLUDE_PATHS`（逗号/分号分隔）

### 环境变量

| 变量 | 作用 |
|---|---|
| `MRORSYNC_CONFIG` | 配置文件路径 |
| `MRORSYNC_ROOT` / `MRORSYNC_REMOTE_BASE` | 覆盖 `root` / `remote_base` |
| `MRORSYNC_MAX_WORKERS` / `MRORSYNC_PART_WORKERS` | 覆盖并发数 |
| `MRORSYNC_MAX_FILE_SIZE` | 覆盖单文件上限（字节） |
| `MRORSYNC_STATE_DIR` | 状态目录；优先级低于 `state_dir` |
| `MRORSYNC_EXCLUDES` | 逗号/分号分隔的排除项（按名字） |
| `MRORSYNC_EXCLUDE_PATHS` | 逗号/分号分隔的排除路径（相对 `root`） |
| `MRORSYNC_APP_KEY` / `MRORSYNC_SECRET_KEY` | 覆盖应用凭证 |
| `MRORSYNC_ACCESS_TOKEN` / `MRORSYNC_REFRESH_TOKEN` | 覆盖 token（适合 CI，注意环境安全） |
| `MRORSYNC_RETRY_TRIES` 等 | 覆盖对应 `tuning` 字段（变量名为 `MRORSYNC_` + 字段名大写） |
| `BAIDU_PAN_API` / `BAIDU_PCS_API` / `BAIDU_OAUTH_API` | 覆盖接口地址（兼容旧版） |
| `BAIDU_SYNC_STATE_DIR` | 旧版状态目录变量（兼容） |

## 命令行

```text
mrorsync.exe [选项]
mrorsync.exe login [--config 路径]
```

| 选项 | 说明 |
|---|---|
| `--apply` | 真正写入远端；不加时只扫描打印（dry-run，不联网） |
| `--full` | 忽略 size/mtime 快路径，全部重新计算 md5 |
| `--verify` | 重算全部 md5，与清单不一致才上传；**必须配合 `--apply`** |
| `--only <子目录>` | 只处理某个子目录（相对 root，大小写不敏感） |
| `--limit <N>` | 本次最多上传 N 个文件；**此模式不执行远端删除** |
| `--no-delete` | 不删除远端多余文件 |
| `--force-delete` | 跳过“删除超过 N%”保护（N 见 `tuning.delete_protect_percent`） |
| `--config <路径>` | 配置文件路径，默认 `$MRORSYNC_CONFIG` 或 exe 同目录 `config.json` |
| `--root <路径>` | 一次性覆盖 `root`（配合计划任务/临时目录） |
| `--remote-base <路径>` | 一次性覆盖 `remote_base` |
| `--max-workers <N>` | 一次性覆盖 `max_workers` |
| `--part-workers <N>` | 一次性覆盖 `part_workers` |
| `--state-dir <路径>` | 一次性覆盖 `state_dir` |
| `--exclude-path <路径>` | 额外排除某个目录/文件（相对 `root`，可重复） |
| `--print-config` | 打印生效配置（token/secret 打码）后退出，便于排查 |
| `--check-config` | 只校验配置并打印摘要，不扫描、不联网 |
| `--version` | 打印版本号（含构建提交） |
| `-h`, `--help` | 帮助 |

组合规则（代码里显式校验，不会静默）：

- `--verify` 不带 `--apply` 直接报错退出（退出码 2）
- `--limit` 隐含不删除远端；`--only` 模式同样不删除（避免只看到子树就误删其他目录）
- `--force-delete` 只解除比例保护，不解除 `--limit` / `--only` 的限制

## 退出码

| 码 | 含义 | 后续动作 |
|---|---|---|
| `0` | 全部成功 | 无 |
| `1` | 有文件失败，或认证失效提前终止 | 失败清单在 `last-run.json`，下次自动重试 |
| `2` | 配置/参数错误，或已有实例在运行 | 修配置 / 等另一实例结束 |
| `130` | Ctrl+C 中断 | manifest 已落盘，重新运行即可续传 |

## 变更判定

1. `size` + `mtime`（精确到毫秒）与清单一致且 `state=ok` → 跳过，不读盘
2. 否则重新读盘，一次算出整文件 md5 和每 4 MB 分片 md5
3. 只有 `size+mtime` 都没变（例如上次上传失败）才复用旧 md5；文件被改过时绝不复用，
   否则会把旧内容“秒传”成新文件
4. `precreate` 返回 `return_type=2` 即秒传；返回 `block_list` 时只上传缺失分片

`--full` 与 `--verify` 都会要求重新读盘：

- `--full`：重算后仍按正常流程走 `precreate`，能秒传就秒传
- `--verify`：重算后与清单里的 md5 比对；一致只补写元数据（`fs_id`/`utime`），
  不碰网络；不一致才上传

## 远端目录结构

远端路径 = `remote_base` + 本地相对路径。`remote_base` 为 `/` 时，`D:\docs\a.txt`
会落到 `/docs/a.txt`；为 `/备份/D盘` 时落到 `/备份/D盘/docs/a.txt`。

中文路径按 UTF-8 原样传输，manifest 用原样相对路径（`/` 分隔）作 key。

## 状态文件

全部在 `state_dir`（默认 `<exe>/.state`），都是临时文件 + rename 原子写。

| 文件 | 内容 |
|---|---|
| `manifest.json` | 已同步清单，key 为相对路径 |
| `blocks/<md5>.json` | 分片缓存，按整文件 md5 命名 |
| `last-run.json` | 上次运行的统计与失败清单 |
| `run.lock` | 运行中的单实例锁，含 PID 与开始时间；正常退出即删除 |

`manifest.json` 单条记录：

| 字段 | 说明 |
|---|---|
| `size` | 字节数 |
| `mtime` | 修改时间（秒） |
| `mtime_ms` | 毫秒部分，兼容旧版的 `Math.round` |
| `md5` | 整文件 md5 |
| `fs_id` | 远端文件 ID |
| `state` | `ok` 才算同步完成；失败的不写 `ok`，下次重试 |
| `utime` | 上传完成时间 |

`last-run.json` 字段：`at`、`root`、`remote_base`、`scanned`、`todo`、`ok`、`rapid`
（秒传）、`checked`（验证一致）、`failed`、`parts`（上传分片）、`deleted`、`bytes`
（逻辑大小）、`bytes_net`（实传字节）、`seconds`、`failures[]`（`rel` + `error`）。

## 可靠性设计

- **重试**：网络错误/5xx 最多 5 次，1s/2s/4s/8s 退避并加 jitter（上限 8s）；
  31034/31363 单独走 5s/10s/20s/40s 的窗口（上限 40s）；HTTP 429 读 `Retry-After`
  （支持秒数与 HTTP-date）；普通 4xx 不重试。
- **token 续期**：-6/110/111 触发 refresh 并原子写回 `config.json`；刷新后**重建请求**
  再发；并发刷新用互斥 + token 比对去重，避免把一次性 refresh_token 用两次。
- **并发**：文件级 `max_workers`，文件内分片 `part_workers`；全部走 `context`，
  Ctrl+C 会取消在途请求并落盘 manifest。
- **目录缓存**：同一远端目录只 `create` 一次，并发请求按路径去重等待。
- **状态原子写**：manifest、分片缓存、`last-run.json`、config 都是临时文件 + rename。
- **manifest 批量刷盘**：每 1000 条拍一次快照，锁内只拷贝 map，序列化与写盘在锁外完成，
  避免并发 worker 被 flush 阻塞；写入量随文件数线性增长。
- **缓存清理**：成功跑完后删除 manifest 不再引用的分片缓存（24 小时内的保留，
  避免误删在途写入）；缓存写入不 fsync，丢了只会重算一次。
- **坏缓存自愈**：分片缓存损坏/尺寸不符时当作没有，重新读盘。
- **单实例**：`.state/run.lock` 独占锁，第二个实例直接退出（退出码 2）；异常退出留下的锁
  超过 24 小时会被自动接管。
- **认证失败短路**：errno -6/110/111 或 HTTP 401/403 时立即终止整轮（不再逐个文件重试），
  并提示重新 `login`，同时跳过远端删除。
- **重试幂等**：`create` 若因重试收到 31039「文件已存在」，按成功处理，避免“实际已上传却报失败”。
- **日志**：每行带 `2006-01-02 15:04:05` 时间戳，并发写入加锁，截断错误响应时保证 UTF-8 完整。
- **删除保护**：远端已不存在按成功处理；`--limit` 不删；`--only` 不删；
  超过清单 20% 需 `--force-delete`；扫描有目录读取失败时本次不删。
- **兼容**：`mtime_ms` 采用与旧版相同的四舍五入，升级前的 manifest 可直接沿用，
  不会因 1 ms 误差反复重算。

## 安全

- `config.json` 含 `access_token` / `refresh_token`，**不要提交、不要放共享目录**；
  已在 `.gitignore` 中；程序保存时用 0600 写入（Windows 上权限实际由 ACL 决定，
  建议把目录放在个人用户下）。
- 怀疑泄露：到[百度账号授权管理](https://passport.baidu.com/accountbind)取消授权，
  再重新 `mrorsync.exe login`。
- 状态目录含完整文件清单和分片哈希，同样建议只留在本机。

## 定时任务

每天 02:00 以 SYSTEM 身份同步：

```powershell
schtasks /Create /TN "BaiduBackup" /TR "D:\_mroldriver\mrorSync\mrorsync.exe --apply" /SC DAILY /ST 02:00 /RU SYSTEM /RL HIGHEST
```

带日志：

```powershell
schtasks /Create /TN "BaiduBackup" /TR "cmd /c \"D:\_mroldriver\mrorSync\mrorsync.exe --apply >> D:\_mroldriver\mrorSync\sync.log 2>&1\"" /SC DAILY /ST 02:00 /RU SYSTEM /RL HIGHEST
```

查看/删除：

```powershell
schtasks /Query /TN "BaiduBackup" /V /FO LIST
schtasks /Run   /TN "BaiduBackup"      # 立即跑一次验证
schtasks /Delete /TN "BaiduBackup" /F  # 切换前先删旧的（指向旧脚本的任务）
```

需要常驻/服务化可以用 [NSSM](https://nssm.cc/) 把 `mrorsync.exe --apply` 包成 Windows 服务。

## 测试

```powershell
.\build.ps1 -Test
# 或
go test ./...
```

`main_test.go` 在 127.0.0.1 起假 xpan 服务端，每个场景一个独立测试、各自用
`t.TempDir()`，互不污染。覆盖：首次上传与分片复用、二次增量跳过、`--full` 秒传、
`--verify` 修复、分片缓存损坏自愈、token 过期续期、远端清理、`--limit` 不删除、
31034 退避、`--only` 大小写不敏感、`--verify` 缺 `--apply` 报错、`last-run.json`。
不触碰真实网盘。

另有：认证失效短路、`create` 已存在按成功、单实例锁、manifest 并发刷盘、
孤立缓存清理、UTF-8 截断、退避上下界、删除保护阈值、`--no-delete`、HTTP 429 重试。

`go test -race` 需要 C 编译器（gcc/mingw），本机未装，暂未启用。

## 开发

分支模型（Gitee 默认分支为 `develop`）：

| 分支 | 用途 |
|---|---|
| `develop` | 日常开发，默认分支 |
| `master` | 稳定/发布，只在发版时从 `develop` 合入 |

```powershell
git switch develop
git add -A
git commit -m "说明"
git push
```

发布：`develop` 合并到 `master`，在 Gitee 建 Release/tag。

目录结构：

```text
.
├── main.go              CLI、login 子命令
├── main_test.go         端到端测试（假 xpan 服务端）
├── internal/
│   ├── config/          config.json 读写、校验、原子保存
│   ├── baidu/           xpan API：重试、续期、目录缓存、分片、删除
│   ├── fsutil/          原子写文件
│   └── syncer/          扫描、manifest、哈希缓存、上传引擎
├── build.ps1            构建脚本（自动找 Go，注入提交号）
├── ci.ps1               本地/CI 检查：gofmt、vet、test
├── config.example.json  配置模板
└── AGENTS.md            给 AI/自动化代理的开发约定
```

版本号在 `internal/syncer/engine.go` 的 `Version`；构建时 `build.ps1` 把当前提交注入
`BuildCommit`，`--version` 会输出 `3.0.7 (abc1234)`。改完跑 `.\ci.ps1 -Build` 再提交。

## 故障排查

| 现象 | 原因 | 处理 |
|---|---|---|
| `认证失败…请运行 mrorsync.exe login` | token 过期且 refresh 失败（errno -6/110/111） | `.\mrorsync.exe login` 重新授权 |
| `errno=31034` / `31363` / HTTP 429 | 请求过密被限流 | 降低 `max_workers`/`part_workers`（建议 ≤3），稍后重跑 |
| `[保护] 待删除 N 个…超过 20% 阈值` | 本地大批文件没了（拔盘、误删、`root` 配错） | 确认后加 `--force-delete` |
| `[保护] --limit 模式下跳过远端删除` | 正常行为 | 去掉 `--limit` 正式跑 |
| `分片数超过 1024，文件过大` | 单文件超 4 GB（1024 × 4 MB） | 拆包，或改用其他工具传该文件 |
| `manifest 损坏，无法解析` | 文件被截断/手改 | 从备份恢复；没有就删掉重跑（会重算 md5，秒传兜底，不会重复传数据） |
| `已有实例在运行` | 计划任务与手动执行同时启动 | 等前一个跑完；确认没有进程后锁超过 24 小时才会自动接管 |
| `缺少配置文件` | exe 同目录没有 `config.json` | 复制模板，或用 `--config` 指定 |
| 中文显示乱码 | 控制台代码页 | `chcp 65001` |
| 每次运行都在重算大量 md5 | `mtime_ms` 精度不一致 | 保留旧 manifest，不要删除 `.state` |

## 已知限制

- 单文件上限 4 GB（普通用户），超过打印 `[skip]`，需要拆包
- 分片固定 4 MB，最后一片按实际大小；单文件最多 1024 片
- 删除是移进网盘回收站，不是彻底删除
- 同名文件 `rtype=3` 覆盖，不留历史版本
- 中文路径按 UTF-8 传，manifest 用原样相对路径做 key
- excludes 按“文件名”匹配，支持 `*` 通配（如 `*.class`），不支持路径通配
- 文件在同一毫秒内被改且 size 不变时仍可能漏判（任何基于 mtime 的同步都有此问题）
- 尚未封装 Windows 服务；需要后台常驻可以用计划任务或 NSSM
- 仓库暂未声明开源许可证

## 变更记录

### 3.0.8

- 新增 `exclude_paths`（配置）、`--exclude-path`（CLI，可重复）、`MRORSYNC_EXCLUDE_PATHS`：
  按相对 `root` 的路径排除指定目录或文件，命中目录时整棵子树跳过
- `excludes` 保持按名字匹配；两者可组合使用
- 绝对路径位于 `root` 下时自动转相对路径，`root` 外的条目告警后忽略

### 3.0.7

- 适配百度真实 API：`method`/`access_token` 必须放在 query string，放 form body 会返回
  HTTP 400 `Unsupported open api`；`filemanager` 的 `opera`/`async` 同样必须在 query，
  否则返回 `errno=2`
- `precreate` 的 `block_list` 按“待传分片序号”解析（同时兼容旧版/测试里的 md5 列表），
  恢复分片复用
- 首次真实环境验证：单文件上传、5 MB 分片上传、删除、`last-run.json` 全部通过
- 测试 mock 改为同样强制 query 契约，防止回归

### 3.0.6

- 配置标准化：`config_version`、可选 `tuning` 段（重试/退避/超时/删除保护/锁与缓存年龄等），
  旧配置不写也能用
- 优先级统一为「命令行 > `MRORSYNC_*` 环境变量 > `config.json` > 默认值」，新增
  `--root`/`--remote-base`/`--max-workers`/`--part-workers`/`--state-dir` 一次性覆盖
- 新增 `--print-config`（密钥打码）、`--check-config`（只校验不扫描）
- 越界配置不再静默夹取，改为打印 `[警告]`；`config_version` 过高会直接报错

### 3.0.5

- 端到端测试从单个 `TestSyncFlow` 拆成 17 个独立场景测试，各自临时目录、互不污染
- `--limit` 先截断再打印“待上传”，日志与实际处理数量一致
- `OKRels` 每次运行只遍历一次 manifest

### 3.0.4

- `build.ps1` 注入提交号，`--version` 显示 `版本 (提交)`
- 新增 `ci.ps1`：gofmt/vet/test（`-Build` 附带构建），可作为 Gitee Go 流水线命令
- 删除未使用的 `ErrnoOf`/`IsFatal`；上传逻辑拆到 `internal/syncer/upload.go`

### 3.0.3

- 删除统计只计真正删掉的条目，已不存在的远端文件不再虚增 `deleted`
- worker 增加 panic 兜底，单个文件异常不再带崩整轮
- 统计区分逻辑大小与实传字节，`last-run.json` 新增 `bytes_net`
- `--only -h` 不再被误判为帮助；`login` 拒绝多余参数
- 重试次数 4→5，退避上限改为可达的 8s / 40s

### 3.0.2

- manifest 改为每 1000 条快照刷盘：锁内只拷贝 map，序列化/写盘移出锁，写入量从平方级降为线性
- 分片缓存改用无 fsync 的原子写；成功跑完后清理孤立缓存（24h 内的保留）
- HTTP 连接池按 `max_workers` 配置；退避加 jitter；31034/31363 窗口放宽到最长 60s；
  `Retry-After` 支持 HTTP-date
- 新增 manifest 并发/刷盘/清理测试与退避边界测试

### 3.0.1

- 单实例运行锁 `.state/run.lock`，避免计划任务与手动执行并发写 manifest
- 认证失效（-6/110/111、401/403）立即终止整轮并跳过远端删除，不再逐文件重试
- `create` 重试收到「文件已存在」（31039/31061）按成功处理
- 日志增加时间戳与并发互斥；错误响应截断不再破坏 UTF-8
- 新增锁、认证短路、已存在重试、UTF-8 截断等测试

### 3.0.0

- Go 重写，单 exe、纯标准库，移除 Node 运行时依赖
- 保持与旧版相同的 `config.json` / `.state` 格式，可直接沿用
- 新增 `--verify`、`--no-delete`、`--force-delete`、`--only`、`--limit`
- 11 个端到端测试场景，全部走 mock 服务端
