package baidu

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"mrorsync/internal/config"
)

const (
	userAgent = "pan.baidu.com"
	timeout   = 180 * time.Second
)

var (
	tokenErrnos = map[int]bool{-6: true, 110: true, 111: true}
	retryErrnos = map[int]bool{31034: true, 31363: true}
	errnoText   = map[int]string{
		-8:    "文件或目录已存在",
		110:   "access_token 无效",
		111:   "access_token 已过期",
		31023: "参数错误",
		31024: "没有权限",
		31034: "服务繁忙，请稍后重试",
		31039: "文件已存在",
		31045: "用户不存在",
		31061: "文件已存在",
		31064: "文件不存在",
		31066: "文件名不合法",
		31363: "请求过于频繁",
	}
)

// APIError is a Baidu business error (HTTP 200 with non-zero errno).
type APIError struct {
	Op        string
	Errno     int
	RequestID string
	Message   string
	Fatal     bool
}

func (e *APIError) Error() string {
	detail := errnoText[e.Errno]
	if e.Message != "" {
		detail = e.Message
	}
	suffix := ""
	if detail != "" {
		suffix = "（" + detail + "）"
	}
	rid := ""
	if e.RequestID != "" {
		rid = " request_id=" + e.RequestID
	}
	return fmt.Sprintf("%s errno=%d%s%s", e.Op, e.Errno, suffix, rid)
}

// HTTPError is a transport/HTTP-level failure.
type HTTPError struct {
	Op         string
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s HTTP %d: %s", e.Op, e.Status, e.Body)
}

// Precreate is the subset of the precreate response the uploader needs.
type Precreate struct {
	ReturnType int
	UploadID   string
	BlockList  []string
	FSID       int64
}

// Client speaks the Baidu xpan API. It is safe for concurrent use.
type Client struct {
	cfg     *config.Config
	http    *http.Client
	panAPI  string
	pcsAPI  string
	oauth   string
	Logf    func(format string, args ...any)
	tokMu   sync.RWMutex
	refMu   sync.Mutex
	dirMu   sync.Mutex
	dirs    map[string]bool
	pending map[string]*dirCall
}

type dirCall struct {
	done chan struct{}
	err  error
}

// New builds a client; API endpoints can be overridden with env vars (tests).
func New(cfg *config.Config) *Client {
	c := &Client{
		cfg:     cfg,
		http:    newHTTPClient(cfg.MaxWorkers),
		panAPI:  envOr("BAIDU_PAN_API", "https://pan.baidu.com/rest/2.0/xpan/file"),
		pcsAPI:  envOr("BAIDU_PCS_API", "https://d.pcs.baidu.com/rest/2.0/pcs/superfile2"),
		oauth:   envOr("BAIDU_OAUTH_API", "https://openapi.baidu.com/oauth/2.0/token"),
		Logf:    func(string, ...any) {},
		dirs:    map[string]bool{},
		pending: map[string]*dirCall{},
	}
	return c
}

// newHTTPClient keeps enough idle connections for the upload workers; the
// default of two per host makes concurrent uploads pay for a new TLS handshake
// on almost every request.
func newHTTPClient(maxWorkers int) *http.Client {
	if maxWorkers < 4 {
		maxWorkers = 4
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 100
	tr.MaxIdleConnsPerHost = maxWorkers
	tr.IdleConnTimeout = 90 * time.Second
	return &http.Client{Timeout: timeout, Transport: tr}
}

func envOr(key, dflt string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return dflt
}

// AccessToken returns the current token (refreshed under the hood).
func (c *Client) AccessToken() string {
	c.tokMu.RLock()
	defer c.tokMu.RUnlock()
	return c.cfg.AccessToken
}

func (c *Client) token() string { return c.AccessToken() }

// refresh renews the access token. oldToken guards against a concurrent refresh:
// if another goroutine already rotated the token we reuse its result instead of
// burning the (single-use) refresh token again.
func (c *Client) refresh(ctx context.Context, oldToken string) bool {
	c.refMu.Lock()
	defer c.refMu.Unlock()

	if tok := c.AccessToken(); tok != oldToken && tok != "" {
		return true
	}
	c.tokMu.RLock()
	refreshToken, appKey, secret := c.cfg.RefreshToken, c.cfg.AppKey, c.cfg.SecretKey
	c.tokMu.RUnlock()
	if refreshToken == "" || appKey == "" {
		c.Logf("[token] 缺少 refresh_token/app_key，无法自动续期")
		return false
	}

	q := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {appKey},
		"client_secret": {secret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.oauth+"?"+q.Encode(), nil)
	if err != nil {
		c.Logf("[token] 构造请求失败: %v", err)
		return false
	}
	req.Header.Set("User-Agent", userAgent)
	res, err := c.http.Do(req)
	if err != nil {
		c.Logf("[token] 刷新异常: %v", err)
		return false
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		c.Logf("[token] 刷新响应非 JSON: %s", clip(body, 200))
		return false
	}
	tok := str(m["access_token"])
	if tok == "" {
		c.Logf("[token] 刷新失败: %s", clip(body, 300))
		return false
	}

	c.tokMu.Lock()
	c.cfg.AccessToken = tok
	if rt := str(m["refresh_token"]); rt != "" {
		c.cfg.RefreshToken = rt
	}
	saveErr := c.cfg.Save()
	c.tokMu.Unlock()
	if saveErr != nil {
		c.Logf("[token] access_token 已刷新，但写回配置失败: %v", saveErr)
	} else {
		c.Logf("[token] access_token 已刷新")
	}
	return true
}

// do runs one logical API call with rebuild-per-attempt semantics: a token
// refresh changes the request body, so the request must be recreated.
func (c *Client) do(ctx context.Context, build func(context.Context) (*http.Request, error), op string) (map[string]any, error) {
	var lastErr error
	const tries = 5
	for attempt := 0; attempt < tries; attempt++ {
		req, err := build(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)

		res, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if attempt == tries-1 {
				break
			}
			if !sleepCtx(ctx, backoff(attempt)) {
				return nil, ctx.Err()
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if attempt == tries-1 {
				break
			}
			if !sleepCtx(ctx, backoff(attempt)) {
				return nil, ctx.Err()
			}
			continue
		}

		if res.StatusCode >= 400 {
			he := &HTTPError{Op: op, Status: res.StatusCode, Body: clip(body, 300)}
			if d := retryAfter(res.Header.Get("Retry-After")); d > 0 {
				he.RetryAfter = d
			}
			retryable := res.StatusCode == http.StatusRequestTimeout ||
				res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500
			if !retryable {
				return nil, he
			}
			lastErr = he
			if attempt == tries-1 {
				break
			}
			wait := backoff(attempt)
			if he.RetryAfter > 0 {
				wait = min(he.RetryAfter, time.Minute)
			}
			if !sleepCtx(ctx, wait) {
				return nil, ctx.Err()
			}
			continue
		}

		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			lastErr = &HTTPError{Op: op, Status: res.StatusCode, Body: "响应非 JSON: " + clip(body, 200)}
			if attempt == tries-1 {
				break
			}
			if !sleepCtx(ctx, backoff(attempt)) {
				return nil, ctx.Err()
			}
			continue
		}

		errno := int(num(m["errno"]))
		if errno == 0 {
			return m, nil
		}
		ae := &APIError{Op: op, Errno: errno, RequestID: str(m["request_id"])}

		if tokenErrnos[errno] {
			old := c.token()
			if c.refresh(ctx, old) {
				continue
			}
			ae.Message = "认证失败，请运行 mrorsync.exe login 重新授权"
			ae.Fatal = true
			return nil, ae
		}
		if retryErrnos[errno] {
			lastErr = ae
			if attempt == tries-1 {
				break
			}
			if !sleepCtx(ctx, busyBackoff(attempt)) {
				return nil, ctx.Err()
			}
			continue
		}
		ae.Fatal = true
		return nil, ae
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%s 连续失败", op)
	}
	return nil, lastErr
}

func (c *Client) call(ctx context.Context, method string, params url.Values) (map[string]any, error) {
	return c.do(ctx, func(ctx context.Context) (*http.Request, error) {
		v := url.Values{}
		for k, vs := range params {
			v[k] = vs
		}
		v.Set("method", method)
		v.Set("access_token", c.token())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.panAPI, strings.NewReader(v.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req, nil
	}, method)
}

// EnsureDir creates every missing path segment, deduplicating concurrent calls.
func (c *Client) EnsureDir(ctx context.Context, remoteDir string) error {
	cur := ""
	for _, seg := range strings.Split(strings.Trim(remoteDir, "/"), "/") {
		if seg == "" {
			continue
		}
		cur += "/" + seg
		if err := c.ensureOne(ctx, cur); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) ensureOne(ctx context.Context, dir string) error {
	c.dirMu.Lock()
	if c.dirs[dir] {
		c.dirMu.Unlock()
		return nil
	}
	if call, ok := c.pending[dir]; ok {
		c.dirMu.Unlock()
		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	call := &dirCall{done: make(chan struct{})}
	c.pending[dir] = call
	c.dirMu.Unlock()

	err := c.createDir(ctx, dir)
	c.dirMu.Lock()
	call.err = err
	delete(c.pending, dir)
	if err == nil {
		c.dirs[dir] = true
	}
	c.dirMu.Unlock()
	close(call.done)
	return err
}

func (c *Client) createDir(ctx context.Context, dir string) error {
	_, err := c.call(ctx, "create", url.Values{
		"path":  {dir},
		"isdir": {"1"},
		"rtype": {"0"},
	})
	var ae *APIError
	if errors.As(err, &ae) && ae.Errno == -8 { // 已存在
		return nil
	}
	return err
}

// Precreate asks Baidu whether the file can be deduplicated (秒传) or which
// blocks already exist.
func (c *Client) Precreate(ctx context.Context, remote string, size int64, blocks []string) (*Precreate, error) {
	blockJSON, err := json.Marshal(blocks)
	if err != nil {
		return nil, err
	}
	m, err := c.call(ctx, "precreate", url.Values{
		"path":       {remote},
		"size":       {fmt.Sprint(size)},
		"isdir":      {"0"},
		"autoinit":   {"1"},
		"block_list": {string(blockJSON)},
	})
	if err != nil {
		return nil, err
	}
	p := &Precreate{
		ReturnType: int(num(m["return_type"])),
		UploadID:   str(m["uploadid"]),
		BlockList:  stringSlice(m["block_list"]),
	}
	if info, ok := m["info"].(map[string]any); ok {
		p.FSID = num(info["fs_id"])
	}
	return p, nil
}

// UploadPart uploads one 4MB block via superfile2 and verifies the returned md5.
func (c *Client) UploadPart(ctx context.Context, remote, uploadID string, seq int, data []byte) error {
	want := md5Hex(data)
	m, err := c.do(ctx, func(ctx context.Context) (*http.Request, error) {
		q := url.Values{
			"method":       {"upload"},
			"access_token": {c.token()},
			"type":         {"tmpfile"},
			"path":         {remote},
			"uploadid":     {uploadID},
			"partseq":      {fmt.Sprint(seq)},
		}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, err := mw.CreateFormFile("file", "blob")
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write(data); err != nil {
			return nil, err
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.pcsAPI+"?"+q.Encode(), &buf)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		return req, nil
	}, fmt.Sprintf("superfile2#%d", seq))
	if err != nil {
		return err
	}
	if got := str(m["md5"]); got != "" && got != want {
		return fmt.Errorf("superfile2 分片 md5 不一致 partseq=%d", seq)
	}
	return nil
}

// Create merges uploaded blocks into the final file.
func (c *Client) Create(ctx context.Context, remote string, size int64, uploadID string, blocks []string) (int64, error) {
	blockJSON, err := json.Marshal(blocks)
	if err != nil {
		return 0, err
	}
	m, err := c.call(ctx, "create", url.Values{
		"path":       {remote},
		"size":       {fmt.Sprint(size)},
		"isdir":      {"0"},
		"rtype":      {"3"},
		"uploadid":   {uploadID},
		"block_list": {string(blockJSON)},
	})
	if err != nil {
		// create is not idempotent: if the first attempt succeeded but its
		// response was lost, the retry reports "already exists". Treating that
		// as a failure would mark an uploaded file as failed; the path already
		// holds our content (rtype=3 overwrites).
		var ae *APIError
		if errors.As(err, &ae) && (ae.Errno == 31039 || ae.Errno == 31061) {
			c.Logf("[提示] create 返回文件已存在，按成功处理: %s", remote)
			return 0, nil
		}
		return 0, err
	}
	return num(m["fs_id"]), nil
}

// Delete removes remote paths in batches of 100. Already-missing files count as
// success so pruning stays idempotent.
func (c *Client) Delete(ctx context.Context, paths []string) (int, error) {
	removed := 0
	for start := 0; start < len(paths); start += 100 {
		end := min(start+100, len(paths))
		batch := paths[start:end]
		list, err := json.Marshal(batch)
		if err != nil {
			return removed, err
		}
		m, err := c.call(ctx, "filemanager", url.Values{
			"opera":    {"delete"},
			"async":    {"0"},
			"filelist": {string(list)},
		})
		if err != nil {
			return removed, err
		}
		if info, ok := m["info"].([]any); ok {
			for _, item := range info {
				one, ok := item.(map[string]any)
				if !ok {
					continue
				}
				errno := int(num(one["errno"]))
				switch {
				case errno == 0:
					removed++
				case errno == -9 || errno == 31064:
					// already gone: idempotent success, but not a deletion
				default:
					return removed, fmt.Errorf("删除部分失败: %s(errno=%d)", str(one["path"]), errno)
				}
			}
		}
	}
	return removed, nil
}

// ---------------------------------------------------------------- helpers

func num(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		var i int64
		fmt.Sscan(n, &i)
		return i
	}
	return 0
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, it := range arr {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func clip(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	s = s[:n]
	// Do not cut a multi-byte rune in half: trim invalid trailing bytes so the
	// log line stays valid UTF-8.
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	// RFC 7231 also allows an HTTP-date.
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// backoff grows 1s/2s/4s/8s with jitter and a 8s cap, so concurrent workers do
// not retry in lockstep.
func backoff(attempt int) time.Duration {
	d := time.Duration(1<<attempt) * time.Second
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return withJitter(d)
}

// busyBackoff is for 31034/31363: Baidu's throttle needs a longer window than a
// transient network error, so 5s/10s/20s/40s with a 40s cap.
func busyBackoff(attempt int) time.Duration {
	d := time.Duration(5<<attempt) * time.Second
	if d > 40*time.Second {
		d = 40 * time.Second
	}
	return withJitter(d)
}

func withJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ErrnoOf extracts a Baidu errno from an error, or 0.
func ErrnoOf(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Errno
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return -he.Status
	}
	return 0
}

// IsFatal reports whether retrying this error is pointless.
func IsFatal(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Fatal
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status >= 400 && he.Status < 500 && he.Status != 408 && he.Status != 429
	}
	return false
}

// IsAuthError reports whether err means the access token is no longer usable and
// the whole run must stop until the user authorizes again. Unlike IsFatal this
// is about the run, not one file: a bad filename (31066) is file-level.
func IsAuthError(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Errno == -6 || ae.Errno == 110 || ae.Errno == 111
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden
	}
	return false
}
