package syncer

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mrorsync/internal/fsutil"
)

// BlockSize is Baidu's fixed dedup unit (4MB); the last block may be shorter.
const BlockSize = 4 * 1024 * 1024

// EmptyMD5 is the md5 of zero bytes, used as the single block of an empty file.
var EmptyMD5 = md5Hex(nil)

// Hashes is the whole-file md5 plus one md5 per 4MB block.
type Hashes struct {
	MD5    string
	Blocks []string
	Size   int64
}

// BlockCache is the on-disk format of .state/blocks/<md5>.json.
type BlockCache struct {
	V      int      `json:"v"`
	Size   int64    `json:"size"`
	Blocks []string `json:"blocks"`
}

// HashFile reads a file once and returns its whole md5 and per-block md5s.
func HashFile(path string) (Hashes, error) {
	f, err := os.Open(path)
	if err != nil {
		return Hashes{}, err
	}
	defer f.Close()

	buf := make([]byte, BlockSize)
	whole := md5.New()
	blocks := make([]string, 0, 8)
	var size int64
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			whole.Write(chunk)
			blocks = append(blocks, md5Hex(chunk))
			size += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return Hashes{}, rerr
		}
	}
	if size == 0 {
		blocks = append(blocks, EmptyMD5)
	}
	return Hashes{MD5: hex.EncodeToString(whole.Sum(nil)), Blocks: blocks, Size: size}, nil
}

// ReadBlockCache returns nil when the cache is missing, corrupt, or inconsistent
// with the expected size/block count — callers then simply re-hash.
func ReadBlockCache(stateDir, fileMD5 string, expectedSize int64) *BlockCache {
	path := BlockCachePath(stateDir, fileMD5)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c BlockCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	if c.Size != expectedSize || len(c.Blocks) == 0 {
		return nil
	}
	want := int((expectedSize + BlockSize - 1) / BlockSize)
	if want < 1 {
		want = 1
	}
	if len(c.Blocks) != want {
		return nil
	}
	return &c
}

// WriteBlockCache atomically stores block md5s; failures are non-fatal. The
// file is rebuildable, so it is written without fsync.
func WriteBlockCache(stateDir, fileMD5 string, size int64, blocks []string) {
	data, err := json.Marshal(BlockCache{V: 1, Size: size, Blocks: blocks})
	if err != nil {
		return
	}
	_ = fsutil.WriteFileFast(BlockCachePath(stateDir, fileMD5), data, 0o644)
}

// PruneBlockCaches removes block-cache files that no manifest record references,
// plus stale temp files. Files younger than minAge are kept so a concurrent or
// interrupted write is never mistaken for garbage. Returns the number removed.
func PruneBlockCaches(stateDir string, keep map[string]bool, minAge time.Duration) (int, error) {
	dir := filepath.Join(stateDir, "blocks")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	now := time.Now()
	removed := 0
	for _, de := range ents {
		if de.IsDir() {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		if minAge > 0 && now.Sub(info.ModTime()) < minAge {
			continue
		}
		name := de.Name()
		switch {
		case strings.HasSuffix(name, ".json"):
			if keep[strings.TrimSuffix(name, ".json")] {
				continue
			}
		case strings.HasSuffix(name, ".tmp"):
			// leftover from a crashed write
		default:
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err == nil {
			removed++
		}
	}
	return removed, nil
}

// BlockCachePath returns .state/blocks/<md5>.json.
func BlockCachePath(stateDir, fileMD5 string) string {
	return filepath.Join(stateDir, "blocks", fileMD5+".json")
}

// GetHashes prefers the block cache only when the caller proved the file did not
// change (same size+mtime). A changed file must be re-read: reusing a stale md5
// would let Baidu "rapid upload" the old content over the new one.
func GetHashes(stateDir, absPath, knownMD5 string, useKnown bool, expectedSize int64) (Hashes, error) {
	if useKnown && knownMD5 != "" {
		if c := ReadBlockCache(stateDir, knownMD5, expectedSize); c != nil {
			return Hashes{MD5: knownMD5, Blocks: c.Blocks, Size: c.Size}, nil
		}
	}
	h, err := HashFile(absPath)
	if err != nil {
		return Hashes{}, err
	}
	WriteBlockCache(stateDir, h.MD5, h.Size, h.Blocks)
	return h, nil
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// HumanBytes formats a byte count with 1024-based units.
func HumanBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}
