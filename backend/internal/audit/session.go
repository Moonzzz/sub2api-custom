package audit

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// NoSessionDir 是无会话请求共用的目录名（位于当天日期目录下）。
const NoSessionDir = "nosession"

var safeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)

// safeName 把任意外部 ID 映射为安全的单级文件名：
// 合法字符的原样保留，否则（或与保留名冲突时）使用 "h-" + sha256 前缀。
func safeName(id string) string {
	id = strings.TrimSpace(id)
	if safeNameRE.MatchString(id) && id != NoSessionDir && !strings.Contains(id, "..") {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "h-" + hex.EncodeToString(sum[:16])
}

func hashKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// SessionFromHeader 按配置的优先级读取会话 ID。
func SessionFromHeader(h http.Header, names []string) (string, string) {
	for _, name := range names {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return v, name
		}
		// Go 会把 session_id 规范化为 Session_id，部分代理会保留原始大小写：兜底精确匹配。
		if vs, ok := h[name]; ok && len(vs) > 0 && strings.TrimSpace(vs[0]) != "" {
			return strings.TrimSpace(vs[0]), name
		}
	}
	return "", ""
}

func dateDir(t time.Time) string {
	return t.Format("2006-01") + "/" + t.Format("02")
}

// sessionDirs 把会话固定到首次出现的日期目录。
//
// 持久化：.sessions/<sha256[:2]>/<sha256> 标记文件记录首个日期目录，
// 以 link(2) 原子创建，重启后 O(1) 读取，不扫描任何历史目录；
// 内存中只保留有界 LRU 缓存。
type sessionDirs struct {
	root  string
	cap   int
	mu    sync.Mutex
	ll    *list.List
	items map[string]*list.Element
}

type sessionEntry struct {
	key string
	dir string
}

func newSessionDirs(root string, capacity int) *sessionDirs {
	if capacity <= 0 {
		capacity = 10000
	}
	return &sessionDirs{root: root, cap: capacity, ll: list.New(), items: make(map[string]*list.Element)}
}

func (d *sessionDirs) cached(key string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if el, ok := d.items[key]; ok {
		if e, ok := el.Value.(*sessionEntry); ok {
			d.ll.MoveToFront(el)
			return e.dir, true
		}
	}
	return "", false
}

func (d *sessionDirs) remember(key, dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if el, ok := d.items[key]; ok {
		if e, ok := el.Value.(*sessionEntry); ok {
			e.dir = dir
		}
		d.ll.MoveToFront(el)
		return
	}
	d.items[key] = d.ll.PushFront(&sessionEntry{key: key, dir: dir})
	for d.ll.Len() > d.cap {
		last := d.ll.Back()
		d.ll.Remove(last)
		if e, ok := last.Value.(*sessionEntry); ok {
			delete(d.items, e.key)
		}
	}
}

func (d *sessionDirs) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ll.Len()
}

// Resolve 返回会话的相对目录（yyyy-MM/dd/<session>）。
func (d *sessionDirs) Resolve(session string, now time.Time) (string, error) {
	if session == "" {
		return dateDir(now) + "/" + NoSessionDir, nil
	}
	key := hashKey(session)
	if dir, ok := d.cached(key); ok {
		return dir, nil
	}
	marker := filepath.Join(d.root, ".sessions", key[:2], key)
	if dir, err := readSessionMarker(marker); err == nil {
		d.remember(key, dir)
		return dir, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	dir := dateDir(now) + "/" + safeName(session)
	if err := os.MkdirAll(filepath.Dir(marker), 0o750); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(marker), ".tmp-*")
	if err != nil {
		return "", err
	}
	_, werr := fmt.Fprintf(tmp, "%s\n", dir)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return "", errors.Join(werr, cerr)
	}
	linkErr := os.Link(tmp.Name(), marker)
	_ = os.Remove(tmp.Name())
	if linkErr != nil {
		if !errors.Is(linkErr, fs.ErrExist) {
			return "", linkErr
		}
		// 并发竞争：另一请求已写入，以其为准。
		existing, err := readSessionMarker(marker)
		if err != nil {
			return "", err
		}
		dir = existing
	}
	d.remember(key, dir)
	return dir, nil
}

func readSessionMarker(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path 由 sha256 派生
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(string(b))
	if dir == "" || strings.Contains(dir, "..") {
		return "", fmt.Errorf("audit: corrupt session marker %s", path)
	}
	return dir, nil
}
