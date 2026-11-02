package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// 操作日志超过阈值必须轮转，避免 operations.jsonl 无界增长。
func TestJournalRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "operations.jsonl")
	previous := journalMaxBytes
	journalMaxBytes = 1000
	defer func() { journalMaxBytes = previous }()
	j := Journal{Path: path}
	payload := map[string]string{"action": "move", "detail": "0123456789"}
	for i := 0; i < 40; i++ {
		if e := j.Write(payload); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := os.Stat(path + ".1"); e != nil {
		t.Fatalf("日志未轮转：%v", e)
	}
	info, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	if info.Size() >= journalMaxBytes {
		t.Fatalf("当前日志未按阈值轮转：%d 字节", info.Size())
	}
}

// 目标目录创建后搬移失败，必须逆序删除本次新建的空目录，且保留源文件。
func TestMoveRollbackRemovesCreatedDirectories(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("依赖 Windows 文件名非法字符制造搬移失败")
	}
	src, _, roots := fixture(t)
	m := video(t, src, "a.mkv")
	info, e := os.Stat(m.Source)
	if e != nil {
		t.Fatal(e)
	}
	// 冒号在 Windows 文件名中非法：父目录链会被创建，但最终 link 必然失败。
	row := Row{Media: m, Target: filepath.Join(src, "电影库", "Season 01", "a:b.mkv"), info: info, Status: "ready"}
	out, e := moveFile(row, roots)
	if e == nil {
		t.Fatal("非法目标名不应搬移成功")
	}
	if out.Mode != moveMode() {
		t.Fatalf("mode 未记录实际机制：%q", out.Mode)
	}
	if len(out.RolledBackDirectories) == 0 {
		t.Fatal("失败后未回滚新建目录")
	}
	for _, d := range []string{filepath.Join(src, "电影库", "Season 01"), filepath.Join(src, "电影库")} {
		if _, err := os.Lstat(d); !os.IsNotExist(err) {
			t.Fatalf("新建目录未清理：%s", d)
		}
	}
	if _, err := os.Stat(m.Source); err != nil {
		t.Fatal("源文件丢失")
	}
}

// 成功搬移要如实记录实际使用的机制与本次新建的目录。
func TestMoveRecordsCreatedDirectories(t *testing.T) {
	src, _, roots := fixture(t)
	m := video(t, src, "b.mkv")
	info, e := os.Stat(m.Source)
	if e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(src, "电影库", "示例 (2024).mkv")
	row := Row{Media: m, Target: target, info: info, Status: "ready"}
	out, e := moveFile(row, roots)
	if e != nil {
		t.Fatal(e)
	}
	if out.Mode == "" || out.Mode != moveMode() {
		t.Fatalf("mode 记录错误：%q", out.Mode)
	}
	if len(out.CreatedDirectories) != 1 || out.CreatedDirectories[0] != filepath.Join(src, "电影库") {
		t.Fatalf("新建目录记录错误：%+v", out.CreatedDirectories)
	}
	if _, e = os.Stat(target); e != nil {
		t.Fatal("目标文件不存在")
	}
	if len(out.RolledBackDirectories) != 0 {
		t.Fatalf("成功搬移不应有回滚记录：%+v", out.RolledBackDirectories)
	}
}

// 目录软链接判定的短 TTL 缓存应显著减少重复 Lstat 次数。
func TestSymlinkDirCacheReducesStatCalls(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c", "d")
	if e := os.MkdirAll(deep, 0755); e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(deep, "movie.mkv")
	if e := os.WriteFile(target, []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	symlinkDirCache.Clear()
	before := symlinkStatCalls.Load()
	if e := symlinkFree(target); e != nil {
		t.Fatal(e)
	}
	first := symlinkStatCalls.Load() - before
	before = symlinkStatCalls.Load()
	for i := 0; i < 20; i++ {
		if e := symlinkFree(target); e != nil {
			t.Fatal(e)
		}
	}
	second := symlinkStatCalls.Load() - before
	if first < 5 {
		t.Fatalf("首次校验的 Lstat 次数异常：%d", first)
	}
	// 祖先目录命中缓存后，每次调用只剩叶子节点的一次 Lstat。
	if second > 25 {
		t.Fatalf("缓存未生效：首次 %d 次，20 次重复共 %d 次", first, second)
	}
}

// 被替换成软链接的目录在缓存过期后必须重新报错。
func TestSymlinkDirCacheExpires(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	linked := filepath.Join(root, "linked")
	if e := os.MkdirAll(real, 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(real, linked); e != nil {
		t.Skip("当前环境不允许创建软链接")
	}
	defer symlinkDirCache.Clear()
	previous := symlinkDirTTL
	symlinkDirTTL = 10 * time.Millisecond
	defer func() { symlinkDirTTL = previous }()
	leaf := filepath.Join(real, "leaf.mkv")
	if e := os.WriteFile(leaf, []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := symlinkFree(leaf); e != nil {
		t.Fatal(e)
	}
	time.Sleep(25 * time.Millisecond)
	viaLink := filepath.Join(linked, "leaf.mkv")
	if e := symlinkFree(viaLink); e == nil {
		t.Fatal("缓存过期后仍把软链接目录当成安全路径")
	}
}

// 过期会话必须被回收，停机时清空全部会话与凭据。
func TestSessionPurgeAndShutdown(t *testing.T) {
	fn := &FnTV{Token: "live-token"}
	a := &App{
		sessions: map[string]*Session{
			"expired": {expires: time.Now().Add(-time.Minute), fn: fn},
			"alive":   {expires: time.Now().Add(time.Hour), fn: &FnTV{Token: "other"}},
		},
		ctx: context.Background(),
	}
	if n := a.purgeSessions(); n != 1 {
		t.Fatalf("回收数量错误：%d", n)
	}
	if _, ok := a.sessions["alive"]; !ok || len(a.sessions) != 1 {
		t.Fatalf("回收结果错误：%+v", a.sessions)
	}
	if fn.Token != "" {
		t.Fatal("过期会话凭据未清除")
	}
	a.shutdown()
	if len(a.sessions) != 0 {
		t.Fatalf("停机后会话未清空：%d", len(a.sessions))
	}
}

// 封面重复请求应命中内存缓存，只向上游回源一次。
func TestPosterCacheServesRepeatedRequests(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "poster-token" {
			jsonResponse(w, 401, object{})
			return
		}
		switch r.URL.Path {
		case "/v/api/v1/user/info":
			jsonResponse(w, 200, object{"code": 0, "data": object{"is_admin": true, "username": "current-user"}})
		case "/v/api/v1/sys/img/9d/02/poster.webp":
			hits.Add(1)
			w.Header().Set("Content-Type", "image/webp")
			w.Write([]byte("fixture-image"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	fn, _ := newFnTV(upstream.URL)
	fn.Token = "poster-token"
	s := &Session{expires: time.Now().Add(time.Hour), fn: fn, state: State{Items: []Media{{ID: "m1", Poster: "/v/api/v1/sys/img/9d/02/poster.webp"}}}}
	a := &App{sessions: map[string]*Session{"1000": s}, mediaBase: upstream.URL, ctx: context.Background()}
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest("GET", "/api/poster?id=m1", nil)
		r = r.WithContext(context.WithValue(r.Context(), gatewayIdentity{}, gatewayIdentity{"1000", "current-user"}))
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != "fixture-image" {
			t.Fatalf("第 %d 次封面请求失败：%d %q", i+1, w.Code, w.Body.String())
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("封面未走缓存，上游命中 %d 次", hits.Load())
	}
}

// 错误必须按语义返回状态码，供前端区分提示。
func TestErrorResponseStatusCodes(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{apiErrorf(http.StatusConflict, "任务运行中"), http.StatusConflict},
		{apiErrorf(http.StatusGone, "预览已失效"), http.StatusGone},
		{apiErrorf(http.StatusNotFound, "媒体库不存在"), http.StatusNotFound},
		{apiErrorf(http.StatusForbidden, "无权限"), http.StatusForbidden},
		{errDirectoryGrant, http.StatusForbidden},
		{os.ErrNotExist, http.StatusNotFound},
		{os.ErrPermission, http.StatusForbidden},
		{errSymlinkInPath, http.StatusForbidden},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		errorResponse(w, c.err)
		if w.Code != c.want {
			t.Fatalf("%v 返回 %d，期望 %d", c.err, w.Code, c.want)
		}
	}
}
