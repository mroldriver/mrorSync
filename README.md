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

`config.json` 默认与 exe 同目录，可用 `--config` 指定。未知字段会原样保留（token 续期
写回时不会丢配置）。路径支持 `D:/` 或 `D:\`。

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `root` | string | `D:\` | 要备份的本地根目录 |
| `remote_base` | string | `/备份/D盘` | 远端根路径；`/` 表示直接放网盘根（模板示例即 `/`） |
| `app_id` | string | 空 | 应用 ID，`login` 用；同步不用 |
| `app_key` | string | 空 | 应用的 API Key（即 client_id） |
| `secret_key` | string | 空 | 应用的 Secret Key |
| `sign_key` | string | 空 | 预留：秒传签名，目前未使用 |
| `access_token` | string | 空 | 授权后由 `login` 写入 |
| `refresh_token` | string | 空 | 同上，用于自动续期 |
| `max_workers` | int | `2` | 同时上传的文件数，范围 1–16；>3 会提示限流风险 |
| `part_workers` | int | `2` | 单个文件内并发上传的分片数，范围 1–8 |
| `max_file_size` | int | `4294967296` | 单文件上限（字节），默认 4 GB |
| `state_dir` | string | `<exe>/.state` | 状态目录；也可用环境变量覆盖 |
| `excludes` | string[] | 见下 | 扫描时跳过的目录/文件名 |

未配置 `excludes` 时代码内置：`$RECYCLE.BIN`、`System Volume Information`、
`node_modules`、`.git`、`.state`；`config.example.json` 模板额外包含 `pagefile.sys`、
`hiberfil.sys`，可直接按需增删。

匹配规则：按**文件名**（不是完整路径）匹配，大小写不敏感，支持 `*` 通配
（如 `*.class`、`*.tmp`）；不支持 `dir/**` 这类路径通配。

环境变量（测试/私有部署用）：

| 变量 | 作用 |
|---|---|
| `BAIDU_PAN_API` | 覆盖网盘 API 基地址 |
| `BAIDU_PCS_API` | 覆盖上传 API 基地址 |
| `BAIDU_OAUTH_API` | 覆盖 OAuth token 地址 |
| `BAIDU_SYNC_STATE_DIR` | 覆盖状态目录（优先级：`state_dir` > 环境变量 > `.state`） |

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
| `--force-delete` | 跳过“删除超过 20%”保护 |
| `--config <路径>` | 配置文件路径，默认 exe 同目录 `config.json` |
| `--version` | 打印版本号 |
| `-h`, `--help` | 帮助 |

组合规则（代码里显式校验，不会静默）：

- `--verify` 不带 `--apply` 直接报错退出（退出码 2）
- `--limit` 隐含不删除远端；`--only` 模式同样不删除（避免只看到子树就误删其他目录）
- `--force-delete` 只解除比例保护，不解除 `--limit` / `--only` 的限制

## 退出码

| 码 | 含义 | 后续动作 |
|---|---|---|
| `0` | 全部成功 | 无 |
| `1` | 有文件失败 | 失败清单在 `last-run.json`，下次自动重试 |
| `2` | 配置/参数错误 | 修配置后重跑 |
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
（秒传）、`checked`（验证一致）、`failed`、`parts`（上传分片）、`deleted`、`bytes`、
`seconds`、`failures[]`（`rel` + `error`）。

## 可靠性设计

- **重试**：网络错误/5xx 最多 4 次，1s/2s/4s 退避；31034/31363 自动重试；
  HTTP 429 读取 `Retry-After`；普通 4xx 不重试。
- **token 续期**：-6/110/111 触发 refresh 并原子写回 `config.json`；刷新后**重建请求**
  再发；并发刷新用互斥 + token 比对去重，避免把一次性 refresh_token 用两次。
- **并发**：文件级 `max_workers`，文件内分片 `part_workers`；全部走 `context`，
  Ctrl+C 会取消在途请求并落盘 manifest。
- **目录缓存**：同一远端目录只 `create` 一次，并发请求按路径去重等待。
- **状态原子写**：manifest、分片缓存、`last-run.json`、config 都是临时文件 + rename。
- **坏缓存自愈**：分片缓存损坏/尺寸不符时当作没有，重新读盘。
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

`main_test.go` 在 127.0.0.1 起假 xpan 服务端，用临时目录跑完整流程，覆盖 11 个场景：
首次上传、排除规则、分片复用、二次增量跳过、`--full` 秒传、`--verify` 修复、
分片缓存损坏自愈、token 过期续期、远端清理、`--limit` 不删除、31034 退避、
`--only` 大小写不敏感、`--verify` 缺 `--apply` 报错、`last-run.json` 落盘。
不触碰真实网盘。

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
├── build.ps1            构建脚本（自动找 Go）
├── config.example.json  配置模板
└── AGENTS.md            给 AI/自动化代理的开发约定
```

版本号在 `internal/syncer/engine.go` 的 `Version`；改完跑 `.\build.ps1 -Test` 再提交。

## 故障排查

| 现象 | 原因 | 处理 |
|---|---|---|
| `认证失败…请运行 mrorsync.exe login` | token 过期且 refresh 失败（errno -6/110/111） | `.\mrorsync.exe login` 重新授权 |
| `errno=31034` / `31363` / HTTP 429 | 请求过密被限流 | 降低 `max_workers`/`part_workers`（建议 ≤3），稍后重跑 |
| `[保护] 待删除 N 个…超过 20% 阈值` | 本地大批文件没了（拔盘、误删、`root` 配错） | 确认后加 `--force-delete` |
| `[保护] --limit 模式下跳过远端删除` | 正常行为 | 去掉 `--limit` 正式跑 |
| `分片数超过 1024，文件过大` | 单文件超 4 GB（1024 × 4 MB） | 拆包，或改用其他工具传该文件 |
| `manifest 损坏，无法解析` | 文件被截断/手改 | 从备份恢复；没有就删掉重跑（会重算 md5，秒传兜底，不会重复传数据） |
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

### 3.0.0

- Go 重写，单 exe、纯标准库，移除 Node 运行时依赖
- 保持与旧版相同的 `config.json` / `.state` 格式，可直接沿用
- 新增 `--verify`、`--no-delete`、`--force-delete`、`--only`、`--limit`
- 11 个端到端测试场景，全部走 mock 服务端
