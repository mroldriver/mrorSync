# mrorSync —— 百度网盘整盘增量备份（Go 版）

单 exe、零第三方依赖（纯 Go 标准库）。主程序是 `mrorsync.exe`，
配置在 `config.json`，增量状态在 `.state/` 目录。

## 文件

| 文件 | 作用 |
|---|---|
| `mrorsync.exe` | Go 主程序（`go build` 产物） |
| `main.go` | CLI、`login` 子命令 |
| `internal/config` | config.json 读写、默认值、原子保存（保留未知字段） |
| `internal/baidu` | xpan API：重试、token 续期、目录缓存、分片、删除 |
| `internal/syncer` | 扫描、manifest、哈希缓存、上传引擎 |
| `main_test.go` | mock 服务端端到端测试（11 个场景） |
| `build.ps1` | 定位 Go 工具链并编译，`-Test` 附带跑测试 |
| `config.example.json` | 配置模板 |
| `.state/manifest.json` | 已同步清单 |
| `.state/blocks/<md5>.json` | 分片 md5 缓存 |
| `.state/last-run.json` | 上次运行统计与失败清单 |

`config.json` 和 `.state/` 已在 `.gitignore` 中。

## 构建

本机 Go 1.27.3 放在 `%LOCALAPPDATA%\go-toolchain\go\bin`（未加入 PATH），
`build.ps1` 会自动按 `$env:GO_BIN` → PATH → 该目录的顺序查找。

    .\build.ps1            # 生成 mrorsync.exe
    .\build.ps1 -Test      # 编译 + go test ./...

手动编译：

    go build -trimpath -ldflags "-s -w" -o mrorsync.exe .

无 CGO 依赖，可交叉编译到 Linux/NAS：

    $env:GOOS="linux"; $env:GOARCH="amd64"; go build -trimpath -o mrorsync .

## 用法

    .\mrorsync.exe                         只扫描, 打印要传什么, 不联网
    .\mrorsync.exe --apply --limit 20      小批量试传（此模式不删远端）
    .\mrorsync.exe --apply                 正式同步
    .\mrorsync.exe --apply --full          忽略 mtime 快路径, 全部重算 md5
    .\mrorsync.exe --apply --verify        重算全部 md5，只上传与清单不一致的
    .\mrorsync.exe --apply --only 鸿远电气  只处理某个子目录
    .\mrorsync.exe login                   浏览器授权并写回 token
    .\mrorsync.exe --version

环境变量（测试/私有部署用）：

| 变量 | 作用 |
|---|---|
| BAIDU_PAN_API / BAIDU_PCS_API / BAIDU_OAUTH_API | 覆盖接口地址 |
| BAIDU_SYNC_STATE_DIR | 覆盖 `.state` 目录 |

退出码：0 全部成功，1 有文件失败（下次自动重试），2 配置/参数错误，130 中断。

## 变更判定

1. `size` + `mtime`（精确到毫秒）与清单一致且 `state=ok` -> 跳过，不读盘
2. 否则重新读盘，一次算出整文件 md5 和每 4MB 分片 md5
3. 只有 `size+mtime` 都没变（例如上次上传失败）才复用旧 md5；文件被改过时绝不复用，
   否则会把旧内容“秒传”成新文件
4. `precreate` 返回 `return_type=2` 即秒传；返回 `block_list` 时只上传缺失分片

`--verify` 用于升级后或怀疑远端漂移时做一次核对：重算每个文件的 md5，一致就只补写元数据、
不碰网络；不一致才上传。升级到 Go 版后建议先小批量跑：

    .\mrorsync.exe --apply --verify --limit 200

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
- **删除保护**：远端已不存在按成功处理；`--limit` 不删；超过清单 20% 需 `--force-delete`；
  扫描有目录读取失败时本次不删。
- **兼容**：`mtime_ms` 采用与旧版相同的四舍五入，升级前的 manifest 可直接沿用，不会因 1ms 误差反复重算。

## 测试

    .\build.ps1 -Test
    # 或
    go test ./...

`main_test.go` 在 127.0.0.1 起假 xpan 服务端，用临时目录跑完整流程，覆盖：
首次上传、排除规则、分片复用、二次增量跳过、`--full` 秒传、`--verify` 修复、
分片缓存损坏自愈、token 过期续期、远端清理、`--limit` 不删除、31034 退避、
`--only` 大小写不敏感、参数校验、`last-run.json`。不触碰真实网盘。

`go test -race` 需要 C 编译器（gcc/mingw），本机未装，暂未启用。

## 定时任务

    schtasks /Create /TN "BaiduBackup" /TR "D:\_mroldriver\mrorSync\mrorsync.exe --apply" /SC DAILY /ST 02:00 /RU SYSTEM /RL HIGHEST

切换前先把旧的 `BaiduBackup`（指向旧脚本）删掉：

    schtasks /Delete /TN "BaiduBackup" /F

## 已知限制

- 单文件上限 4GB（普通用户），超过打印 `[skip]`，需要拆包
- 分片固定 4MB，最后一片按实际大小；单文件最多 1024 片
- 删除是移进网盘回收站，不是彻底删除
- 同名文件 `rtype=3` 覆盖，不留历史版本
- 中文路径按 UTF-8 传，manifest 用原样相对路径做 key
- excludes 按“文件名”匹配，支持 `*` 通配（如 `*.class`），不支持路径通配
- 文件在同一毫秒内被改且 size 不变时仍可能漏判（任何基于 mtime 的同步都有此问题）
- 尚未封装 Windows 服务；需要后台常驻可以用计划任务或 NSSM
