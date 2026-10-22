package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"mrorsync/internal/config"
	"mrorsync/internal/syncer"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stdin))
}

func run(ctx context.Context, args []string, out io.Writer, in io.Reader) int {
	if len(args) > 0 && args[0] == "login" {
		return runLogin(args[1:], out, in)
	}

	fs := flag.NewFlagSet("mrorsync", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		o       syncer.Options
		version bool
	)
	fs.BoolVar(&o.Apply, "apply", false, "真正写入远端")
	fs.BoolVar(&o.Full, "full", false, "忽略 size/mtime 快路径")
	fs.BoolVar(&o.Verify, "verify", false, "重算全部 md5，只上传不一致文件")
	fs.StringVar(&o.Only, "only", "", "只处理某个子目录")
	fs.IntVar(&o.Limit, "limit", 0, "最多上传 N 个文件（不删除）")
	fs.BoolVar(&o.NoDelete, "no-delete", false, "不删除远端多余文件")
	fs.BoolVar(&o.ForceDelete, "force-delete", false, "跳过删除比例保护")
	fs.StringVar(&o.ConfigPath, "config", config.DefaultPath(), "配置文件路径")
	fs.StringVar(&o.Root, "root", "", "一次性覆盖 config.root")
	fs.StringVar(&o.RemoteBase, "remote-base", "", "一次性覆盖 config.remote_base")
	fs.IntVar(&o.MaxWorkers, "max-workers", 0, "一次性覆盖 max_workers")
	fs.IntVar(&o.PartWorkers, "part-workers", 0, "一次性覆盖 part_workers")
	fs.StringVar(&o.StateDir, "state-dir", "", "一次性覆盖 state_dir")
	fs.BoolVar(&o.PrintConfig, "print-config", false, "打印生效配置（密钥打码）后退出")
	fs.BoolVar(&o.CheckConfig, "check-config", false, "只校验配置后退出")
	fs.BoolVar(&version, "version", false, "打印版本")
	fs.Usage = func() {} // --help is rendered by printHelp below

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(out)
			return 0
		}
		return 2
	}
	if version {
		if syncer.BuildCommit != "" {
			fmt.Fprintf(out, "%s (%s)\n", syncer.Version, syncer.BuildCommit)
		} else {
			fmt.Fprintln(out, syncer.Version)
		}
		return 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(out, "未知参数: %s\n", fs.Arg(0))
		return 2
	}
	o.Out = out
	return syncer.Run(ctx, o)
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `用法: mrorsync.exe [选项]
       mrorsync.exe login [--config 路径]

  --apply             真正写入远端（默认只扫描，dry-run）
  --full              忽略 size/mtime 快路径，全部重新计算 md5
  --verify            配合 --apply：重算全部 md5，只上传与清单不一致的文件
  --only <子目录>     只处理某个子目录（相对 root）
  --limit <N>         本次最多上传 N 个文件（此模式不执行远端删除）
  --no-delete         不删除远端多余文件
  --force-delete      跳过“删除超过 N%”保护（N 见 tuning.delete_protect_percent）
  --config <路径>     指定配置文件；默认 $MRORSYNC_CONFIG 或 exe 同目录 config.json
  --root <路径>       一次性覆盖 config.root
  --remote-base <路径> 一次性覆盖 config.remote_base
  --max-workers <N>   一次性覆盖 max_workers
  --part-workers <N>  一次性覆盖 part_workers
  --state-dir <路径>  一次性覆盖 state_dir
  --print-config      打印生效配置（token/secret 打码）后退出
  --check-config      只校验配置并打印摘要，不扫描不联网
  --version           打印版本
  -h, --help          帮助

优先级: 命令行 > MRORSYNC_* 环境变量 > config.json > 内置默认值

环境变量:
  MRORSYNC_CONFIG                                   指定配置文件路径
  MRORSYNC_ROOT / MRORSYNC_REMOTE_BASE              覆盖对应配置
  MRORSYNC_MAX_WORKERS / MRORSYNC_PART_WORKERS      并发数
  MRORSYNC_STATE_DIR                                状态目录
  MRORSYNC_EXCLUDES                                 逗号分隔的排除项
  MRORSYNC_ACCESS_TOKEN / MRORSYNC_REFRESH_TOKEN    覆盖 token（CI 用）
  MRORSYNC_RETRY_TRIES / MRORSYNC_HTTP_TIMEOUT_SECONDS 等 tuning 项
  BAIDU_PAN_API / BAIDU_PCS_API / BAIDU_OAUTH_API   覆盖接口地址（兼容旧版）
  BAIDU_SYNC_STATE_DIR                              旧版状态目录变量（兼容）
`)
}

// runLogin implements `mrorsync.exe login`: browser authorization + code exchange,
// and writes the tokens back to config.json.
func runLogin(args []string, out io.Writer, in io.Reader) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(out)
	var cfgPath string
	fs.StringVar(&cfgPath, "config", config.DefaultPath(), "配置文件路径")
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(out)
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(out, "未知参数: %s\n", fs.Arg(0))
		return 2
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(out, err)
		return 2
	}
	if cfg.AppKey == "" || cfg.SecretKey == "" {
		fmt.Fprintln(out, "先填 app_key 和 secret_key")
		return 2
	}

	q := url.Values{
		"response_type": {"code"},
		"client_id":     {cfg.AppKey},
		"redirect_uri":  {"oob"},
		"display":       {"page"},
	}
	authURL := "https://openapi.baidu.com/oauth/2.0/authorize?" + q.Encode() + "&scope=basic,netdisk"
	fmt.Fprintf(out, "1) 浏览器会打开授权页；没自动开就手动复制：\n\n%s\n\n", authURL)
	openBrowser(authURL)

	fmt.Fprint(out, "2) 登录并同意后，把页面上的授权码粘到这里: ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	code := strings.TrimSpace(line)
	if code == "" {
		fmt.Fprintln(out, "没有输入授权码")
		return 2
	}

	oauth := strings.TrimSpace(os.Getenv("BAIDU_OAUTH_API"))
	if oauth == "" {
		oauth = "https://openapi.baidu.com/oauth/2.0/token"
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {cfg.AppKey},
		"client_secret": {cfg.SecretKey},
		"redirect_uri":  {"oob"},
	}
	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.PostForm(oauth, form)
	if err != nil {
		fmt.Fprintf(out, "换 token 失败: %v\n", err)
		return 1
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		fmt.Fprintf(out, "换 token 响应非 JSON: %s\n", string(body))
		return 1
	}
	token, _ := m["access_token"].(string)
	if token == "" {
		fmt.Fprintf(out, "换 token 失败: %s\n", string(body))
		return 1
	}
	cfg.AccessToken = token
	if rt, _ := m["refresh_token"].(string); rt != "" {
		cfg.RefreshToken = rt
	} else {
		cfg.RefreshToken = ""
	}
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(out, "写入配置失败: %v\n", err)
		return 1
	}

	days := "?"
	if exp, ok := m["expires_in"].(float64); ok && exp > 0 {
		days = fmt.Sprint(int(exp / 86400))
	}
	fmt.Fprintf(out, "\nOK 已写入 %s\n", cfgPath)
	fmt.Fprintf(out, "access_token 有效期 %s 天，过期后同步程序会用 refresh_token 自动续期\n", days)
	return 0
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}
