package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ttlCache 是进程内的短周期缓存：按条目数或字节数双重限额，
// 过期条目在查询和写入时惰性清理，避免限制只增不减。
// 用途仅限"可由上游重新获取"的只读结果（账号校验结论、封面小图、目录软链接判定），
// 不缓存任何文件系统写操作的结果。
type ttlCache struct {
	mu       sync.Mutex
	items    map[string]cacheEntry
	limit    int
	maxBytes int64
	bytes    int64
	seq      uint64
}

type cacheEntry struct {
	value   any
	expires time.Time
	size    int64
	seq     uint64
}

func newTTLCache(limit int, maxBytes int64) *ttlCache {
	return &ttlCache{items: map[string]cacheEntry{}, limit: limit, maxBytes: maxBytes}
}

func (c *ttlCache) Get(key string) (any, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(item.expires) {
		c.removeLocked(key)
		return nil, false
	}
	return item.value, true
}

func (c *ttlCache) Put(key string, value any, size int64, ttl time.Duration) {
	if c == nil || ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked()
	if _, ok := c.items[key]; ok {
		c.removeLocked(key)
	}
	c.seq++
	c.items[key] = cacheEntry{value: value, expires: time.Now().Add(ttl), size: size, seq: c.seq}
	c.bytes += size
	c.evictLocked()
}

func (c *ttlCache) Delete(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(key)
}

func (c *ttlCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = map[string]cacheEntry{}
	c.bytes = 0
}

func (c *ttlCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked()
	return len(c.items)
}

func (c *ttlCache) removeLocked(key string) {
	if item, ok := c.items[key]; ok {
		c.bytes -= item.size
		delete(c.items, key)
	}
}

func (c *ttlCache) purgeLocked() {
	now := time.Now()
	for key, item := range c.items {
		if now.After(item.expires) {
			c.bytes -= item.size
			delete(c.items, key)
		}
	}
}

func (c *ttlCache) evictLocked() {
	for (c.limit > 0 && len(c.items) > c.limit) || (c.maxBytes > 0 && c.bytes > c.maxBytes) {
		oldest := ""
		var oldestSeq uint64
		for key, item := range c.items {
			if oldest == "" || item.seq < oldestSeq {
				oldest, oldestSeq = key, item.seq
			}
		}
		if oldest == "" {
			return
		}
		c.removeLocked(oldest)
	}
}

// 目录软链接判定缓存：只缓存"目录"（祖先链）结论，叶子节点每次实时检查。
// 有效期取很短，兼顾大库规划期成千上万次 Lstat 的开销与目录被替换为软链接的风险。
var symlinkDirTTL = 10 * time.Second
var symlinkDirCache = newTTLCache(4096, 0)

// 仅用于诊断与测试：真实执行的 Lstat 次数。
var symlinkStatCalls atomic.Int64

var errSymlinkInPath = errors.New("路径包含软链接，已跳过")

// symlinkFree 校验路径中每一级已存在的成分都不是软链接。
// 叶子节点（可能是文件，也可能是最终目录）每次都实时检查，
// 祖先目录走短 TTL 缓存，避免同一批规划中重复 stat 相同目录。
func symlinkFree(p string) error {
	p = filepath.Clean(p)
	info, err := os.Lstat(p)
	symlinkStatCalls.Add(1)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return symlinkPathError()
	}
	parent := filepath.Dir(p)
	if parent == p {
		return nil
	}
	return symlinkSafeDir(parent)
}

// symlinkSafeDir 自下而上检查目录链，命中缓存即停止。
// 只有在整条链都通过校验后才写入"安全"结论，避免父级不安全的目录被缓存成安全。
func symlinkSafeDir(dir string) error {
	chain := []string{}
	for {
		dir = filepath.Clean(dir)
		if value, ok := symlinkDirCache.Get(dir); ok {
			if safe, _ := value.(bool); safe {
				break
			}
			return symlinkPathError()
		}
		info, err := os.Lstat(dir)
		symlinkStatCalls.Add(1)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			symlinkDirCache.Put(dir, false, 0, symlinkDirTTL)
			return symlinkPathError()
		}
		chain = append(chain, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for _, d := range chain {
		symlinkDirCache.Put(d, true, 0, symlinkDirTTL)
	}
	return nil
}

func symlinkPathError() error { return errSymlinkInPath }
