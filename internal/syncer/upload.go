package syncer

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"

	"mrorsync/internal/baidu"
)

type todoItem struct {
	entry    Entry
	known    string
	useKnown bool
	verify   *Record
}

type result struct {
	State string // "upload" | "rapid" | "same"
	MD5   string
	FSID  int64
	Parts int
	Bytes int64 // bytes actually uploaded (0 for rapid/same)
}

// syncOne uploads (or verifies) one file. It is called from the worker pool, one
// goroutine per file, so everything it touches must be safe for concurrent use.
func (e *Engine) syncOne(ctx context.Context, it todoItem) (result, error) {
	abs := filepath.Join(e.cfg.Root, filepath.FromSlash(it.entry.Rel))
	remote := remoteFor(e.cfg, it.entry.Rel)

	h, err := GetHashes(e.stateDir, abs, it.known, it.useKnown, it.entry.Size)
	if err != nil {
		return result{}, err
	}
	if h.Size != it.entry.Size {
		return result{}, fmt.Errorf("扫描后文件大小已变化，跳过: %s", it.entry.Rel)
	}
	if len(h.Blocks) > 1024 {
		return result{}, fmt.Errorf("分片数超过 1024，文件过大: %s", it.entry.Rel)
	}

	if it.verify != nil && it.verify.State == "ok" && it.verify.Size == h.Size && it.verify.MD5 == h.MD5 {
		return result{State: "same", MD5: h.MD5, FSID: it.verify.FSID}, nil
	}

	if err := e.cli.EnsureDir(ctx, path.Dir(remote)); err != nil {
		return result{}, err
	}
	pre, err := e.cli.Precreate(ctx, remote, h.Size, h.Blocks)
	if err != nil {
		return result{}, err
	}
	if pre.ReturnType == 2 {
		return result{State: "rapid", MD5: h.MD5, FSID: pre.FSID}, nil
	}
	if pre.UploadID == "" {
		return result{}, fmt.Errorf("precreate 未返回 uploadid: %s", it.entry.Rel)
	}

	missing := missingBlocks(pre, h.Blocks)

	parts, upBytes := 0, int64(0)
	if h.Size > 0 && len(missing) > 0 {
		f, err := os.Open(abs)
		if err != nil {
			return result{}, err
		}
		parts, upBytes, err = e.uploadParts(ctx, f, remote, pre.UploadID, missing)
		f.Close()
		if err != nil {
			return result{}, err
		}
	}
	fsid, err := e.cli.Create(ctx, remote, h.Size, pre.UploadID, h.Blocks)
	if err != nil {
		return result{}, err
	}
	return result{State: "upload", MD5: h.MD5, FSID: fsid, Parts: parts, Bytes: upBytes}, nil
}

// missingBlocks interprets precreate's block_list. Baidu currently returns the
// indexes that still need uploading; older responses (and the mock server) return
// md5 strings of blocks already present. An empty list means "upload everything".
func missingBlocks(pre *baidu.Precreate, blocks []string) []int {
	if len(blocks) == 0 {
		return nil
	}
	if len(pre.BlockList) == 0 {
		out := make([]int, len(blocks))
		for i := range blocks {
			out[i] = i
		}
		return out
	}
	if idx, ok := blockIndexes(pre.BlockList); ok {
		out := make([]int, 0, len(idx))
		for _, i := range idx {
			if i >= 0 && i < len(blocks) {
				out = append(out, i)
			}
		}
		return out
	}
	exists := make(map[string]bool, len(pre.BlockList))
	for _, v := range pre.BlockList {
		if s, ok := v.(string); ok {
			exists[s] = true
		}
	}
	out := make([]int, 0, len(blocks))
	for i, b := range blocks {
		if !exists[b] {
			out = append(out, i)
		}
	}
	return out
}

func blockIndexes(list []any) ([]int, bool) {
	out := make([]int, 0, len(list))
	for _, v := range list {
		switch n := v.(type) {
		case float64:
			out = append(out, int(n))
		case int:
			out = append(out, n)
		default:
			return nil, false
		}
	}
	return out, true
}

// uploadParts uploads the missing block indices with bounded parallelism.
func (e *Engine) uploadParts(ctx context.Context, f *os.File, remote, uploadID string, seqs []int) (int, int64, error) {
	workers := min(e.cfg.PartWorkers, len(seqs))
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		next     int64 = -1
		done     int64
		uploaded int64
	)
	setErr := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, BlockSize)
			for {
				if ctx.Err() != nil {
					return
				}
				idx := int(atomic.AddInt64(&next, 1))
				if idx >= len(seqs) {
					return
				}
				seq := seqs[idx]
				n, err := f.ReadAt(buf, int64(seq)*BlockSize)
				if err != nil && err != io.EOF {
					setErr(err)
					return
				}
				if n == 0 {
					setErr(fmt.Errorf("读取分片失败 partseq=%d", seq))
					return
				}
				if err := e.cli.UploadPart(ctx, remote, uploadID, seq, buf[:n]); err != nil {
					setErr(err)
					return
				}
				atomic.AddInt64(&done, 1)
				atomic.AddInt64(&uploaded, int64(n))
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return int(done), uploaded, firstErr
	}
	if ctx.Err() != nil {
		return int(done), uploaded, ctx.Err()
	}
	return int(done), uploaded, nil
}
