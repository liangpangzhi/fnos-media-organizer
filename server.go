package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/*
var web embed.FS

func randomID() string {
	b := make([]byte, 24)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}

type TaskProgress struct {
	Total     int      `json:"total"`
	Completed int      `json:"completed"`
	Succeeded int      `json:"succeeded"`
	Failed    int      `json:"failed"`
	Current   string   `json:"current"`
	CurrentID string   `json:"current_id,omitempty"`
	FileIDs   []string `json:"file_ids"`
	Active    bool     `json:"active"`
}

type State struct {
	Revision    string        `json:"revision"`
	Task        *TaskProgress `json:"task,omitempty"`
	Busy        bool          `json:"busy"`
	Progress    string        `json:"progress"`
	Error       string        `json:"error"`
	Version     object        `json:"version"`
	Libraries   []object      `json:"libraries"`
	Items       []Media       `json:"items"`
	Warnings    []string      `json:"warnings"`
	Plan        *Plan         `json:"plan"`
	Inspection  *Plan         `json:"inspection"`
	Results     []Row         `json:"results"`
	Directories []string      `json:"directories"`
	Account     string        `json:"account"`
}

// StateSummary 是轮询用的轻量状态：不含 items/inspection/plan/results 等大字段。
// 前端只在 revision 变化时才去取完整状态，数千条目场景下可把每轮轮询的
// JSON 序列化与网络传输量降到常数级。
type StateSummary struct {
	Revision    string        `json:"revision"`
	Task        *TaskProgress `json:"task,omitempty"`
	Busy        bool          `json:"busy"`
	Progress    string        `json:"progress"`
	Error       string        `json:"error"`
	Warnings    []string      `json:"warnings"`
	Directories []string      `json:"directories"`
	Account     string        `json:"account"`
}

// stateRevision 汇总会改变前端视图的关键量；调用方需持有 session 锁。
func stateRevision(st *State) string {
	inspectionID, planID := "", ""
	if st.Inspection != nil {
		inspectionID = st.Inspection.ID
	}
	if st.Plan != nil {
		planID = st.Plan.ID
	}
	completed, active, current := 0, false, ""
	if st.Task != nil {
		completed, active, current = st.Task.Completed, st.Task.Active, st.Task.CurrentID
	}
	return fmt.Sprintf("%d/%d/%s/%s/%d/%t/%s/%t",
		len(st.Items), len(st.Results), inspectionID, planID, completed, active, current, st.Busy)
}

// summarizeTask 裁掉摘要用不到的大字段（file_ids 在整库整理时可达数千条）。
func summarizeTask(t *TaskProgress) *TaskProgress {
	if t == nil {
		return nil
	}
	c := *t
	c.FileIDs = nil
	return &c
}

type Session struct {
	mu      sync.Mutex
	expires time.Time
	fn      *FnTV
	state   State
}
type App struct {
	mu           sync.Mutex
	sessions     map[string]*Session
	oauth        map[string]oauthPending
	mediaBase    string
	roots        []string
	settings     Settings
	settingsPath string
	journal      Journal
	moveMu       sync.Mutex
	jobs         sync.WaitGroup
	ctx          context.Context
	cacheMu      sync.Mutex
	verification *ttlCache
	posters      *ttlCache
}

// 会话校验缓存有效期。前端在整理期间每 800ms 轮询一次 /api/state，
// 每次都会触发影视 /user/info 校验；缓存把回源压缩为每 TTL 一次。
// 取值刻意很短，使影视管理员权限的变更仍能在数秒内生效；0 表示关闭缓存。
var sessionVerifyTTL = 5 * time.Second

// 会话回收间隔。会话上限 8 小时，分钟级回收即可，避免 Map 无界增长。
const sessionGCInterval = time.Minute

// 封面内存缓存：只在会话存续期内复用，限制条目数与总字节数，避免无限增长。
const (
	posterCacheEntries = 256
	posterCacheBytes   = 32 << 20
	posterCacheTTL     = 10 * time.Minute
)

type posterImage struct {
	Kind string
	Body []byte
}

// userVerification 是影视 /user/info 的校验结论。
type userVerification struct {
	Admin bool
	Name  string
}

func (a *App) verifyCache() *ttlCache {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if a.verification == nil {
		a.verification = newTTLCache(1024, 0)
	}
	return a.verification
}

func (a *App) posterCache() *ttlCache {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if a.posters == nil {
		a.posters = newTTLCache(posterCacheEntries, posterCacheBytes)
	}
	return a.posters
}

func jsonResponse(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
func errorResponse(w http.ResponseWriter, e error) {
	status := errorStatus(e)
	jsonResponse(w, status, object{"error": e.Error(), "status": status})
}

// apiError 携带明确的 HTTP 状态语义，供 errorResponse 分类返回。
type apiError struct {
	status int
	err    error
}

func (e *apiError) Error() string { return e.err.Error() }
func (e *apiError) Unwrap() error { return e.err }

func apiErrorf(status int, format string, args ...any) error {
	return &apiError{status: status, err: fmt.Errorf(format, args...)}
}

// errorStatus 把内部错误映射为 HTTP 状态：参数问题 400、授权目录不足 403、
// 资源不存在 404、状态冲突 409、预览过期 410。前端据此给出不同的提示。
func errorStatus(e error) int {
	var ae *apiError
	if errors.As(e, &ae) && ae.status >= 400 && ae.status <= 599 {
		return ae.status
	}
	if errors.Is(e, errDirectoryGrant) || os.IsPermission(e) || errors.Is(e, os.ErrPermission) {
		return http.StatusForbidden
	}
	if errors.Is(e, os.ErrNotExist) {
		return http.StatusNotFound
	}
	// 路径包含软链接是明确的安全拒绝，按 403 返回，便于前端提示成因。
	if errors.Is(e, errSymlinkInPath) {
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return errors.New("请求必须为 JSON")
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// Session credentials are exchanged via NAS OAuth and kept only in memory.
func (a *App) session(r *http.Request) (*Session, error) {
	u, e := identity(r)
	if e != nil {
		return nil, e
	}
	a.mu.Lock()
	s := a.sessions[u.ID]
	a.mu.Unlock()
	if s == nil || time.Now().After(s.expires) {
		return nil, errors.New("请使用 NAS 账号授权连接影视")
	}
	verify, e := a.mediaUser(r.Context(), u.ID, s.fn)
	if e != nil {
		a.dropSession(u.ID)
		return nil, errors.New("影视授权已失效，请重新授权")
	}
	if !verify.Admin || verify.Name != u.Name {
		return nil, errors.New("当前账号没有影视管理员权限")
	}
	return s, nil
}

// mediaUser 校验影视账号身份与管理员权限，成功结论按 sessionVerifyTTL 缓存。
// 缓存键是网关身份 ID，因此同一网关账号的不同请求可共享结论，
// 但缓存值里的用户名仍会逐次比对，账号被顶替时立即失效。
func (a *App) mediaUser(ctx context.Context, id string, fn *FnTV) (userVerification, error) {
	cache := a.verifyCache()
	if value, ok := cache.Get(id); ok {
		if v, valid := value.(userVerification); valid {
			return v, nil
		}
	}
	if fn == nil {
		return userVerification{}, errors.New("影视会话不存在")
	}
	user, e := fn.Call(ctx, "GET", "/v/api/v1/user/info", nil)
	if e != nil {
		cache.Delete(id)
		return userVerification{}, e
	}
	info := obj(user)
	admin, _ := info["is_admin"].(bool)
	if !admin && num(info, "is_admin") == 1 {
		admin = true
	}
	name := str(info, "username")
	if name == "" {
		name = str(info, "name")
	}
	v := userVerification{Admin: admin, Name: name}
	// 只缓存"已是管理员"的结论：非管理员账号不缓存，避免授权开通后仍被拦。
	if admin {
		cache.Put(id, v, 0, sessionVerifyTTL)
	}
	return v, nil
}

// dropSession 清理指定网关身份的会话与校验缓存，用于退登、授权失效与停机。
func (a *App) dropSession(id string) {
	a.mu.Lock()
	if s := a.sessions[id]; s != nil && s.fn != nil {
		s.fn.Token = ""
	}
	delete(a.sessions, id)
	a.mu.Unlock()
	a.verifyCache().Delete(id)
}

// purgeSessions 清理已过期会话，返回清理数量。
func (a *App) purgeSessions() int {
	now := time.Now()
	removed := 0
	a.mu.Lock()
	for id, s := range a.sessions {
		if s == nil || now.After(s.expires) {
			if s != nil && s.fn != nil {
				s.fn.Token = ""
			}
			delete(a.sessions, id)
			removed++
		}
	}
	a.mu.Unlock()
	if removed > 0 {
		a.verifyCache().Clear()
	}
	return removed
}

// startSessionGC 定期回收过期会话；ctx 取消后退出。间隔远小于会话 8 小时上限。
func (a *App) startSessionGC(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.purgeSessions()
			}
		}
	}()
}

// shutdown 在进程退出前清空全部会话凭据与缓存。
func (a *App) shutdown() {
	a.mu.Lock()
	for id, s := range a.sessions {
		if s != nil && s.fn != nil {
			s.fn.Token = ""
		}
		delete(a.sessions, id)
	}
	a.mu.Unlock()
	a.verifyCache().Clear()
	a.posterCache().Clear()
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'self'; object-src 'none'; base-uri 'none'")
	if r.URL.Path == "/" && r.Method == "GET" {
		b, _ := web.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
		return
	}
	if (r.URL.Path == "/app.js" || r.URL.Path == "/style.css" || r.URL.Path == "/app-icon.png") && r.Method == "GET" {
		b, _ := web.ReadFile("web" + r.URL.Path)
		if strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		} else if strings.HasSuffix(r.URL.Path, ".png") {
			w.Header().Set("Content-Type", "image/png")
		} else {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		w.Write(b)
		return
	}
	// The gateway replaces Host. Require a non-simple request header instead:
	// cross-origin browsers must preflight, and this app never grants CORS.
	if r.Method == "POST" && (r.Header.Get("X-Organizer-Request") != "1" || r.Header.Get("Sec-Fetch-Site") == "cross-site") {
		jsonResponse(w, 403, object{"error": "来源不匹配"})
		return
	}
	if r.URL.Path == "/api/auth/start" && r.Method == "POST" {
		a.oauthStart(w, r)
		return
	}
	if r.URL.Path == "/oauth/callback" && r.Method == "GET" {
		a.oauthCallback(w, r)
		return
	}
	s, authErr := a.session(r)
	if authErr != nil {
		jsonResponse(w, 401, object{"error": authErr.Error()})
		return
	}
	if r.URL.Path == "/api/poster" && r.Method == "GET" {
		s.mu.Lock()
		var poster string
		for _, m := range s.state.Items {
			if m.ID == r.URL.Query().Get("id") {
				poster = m.Poster
				break
			}
		}
		fn := s.fn
		s.mu.Unlock()
		path, e := posterPath(poster)
		if e != nil || fn == nil {
			http.NotFound(w, r)
			return
		}
		// 列表滚动与轮询会重复请求同一张封面，这里用短周期内存缓存复用结果；
		// 键内包含影视地址与当前 token，避免不同会话之间串用凭据。
		cacheKey := fn.Base + "\x00" + fn.Token + "\x00" + path
		if value, ok := a.posterCache().Get(cacheKey); ok {
			if img, valid := value.(posterImage); valid {
				w.Header().Set("Content-Type", img.Kind)
				w.Write(img.Body)
				return
			}
		}
		req, e := http.NewRequestWithContext(r.Context(), "GET", fn.Base+path, nil)
		if e != nil {
			http.NotFound(w, r)
			return
		}
		req.Header.Set("Authorization", fn.Token)
		resp, e := fn.HTTP.Do(req)
		if e != nil {
			http.NotFound(w, r)
			return
		}
		defer resp.Body.Close()
		kind := strings.Split(resp.Header.Get("Content-Type"), ";")[0]
		if resp.StatusCode != 200 || (kind != "image/jpeg" && kind != "image/png" && kind != "image/webp" && kind != "image/gif") {
			http.NotFound(w, r)
			return
		}
		b, e := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
		if e != nil || len(b) > 8<<20 {
			http.NotFound(w, r)
			return
		}
		a.posterCache().Put(cacheKey, posterImage{Kind: kind, Body: b}, int64(len(b)), posterCacheTTL)
		w.Header().Set("Content-Type", kind)
		w.Write(b)
		return
	}
	if r.URL.Path == "/api/state" && r.Method == "GET" {
		a.mu.Lock()
		settings := a.settings
		roots := append([]string(nil), a.roots...)
		a.mu.Unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		revision := stateRevision(&s.state)
		// 摘要模式：只回传进度与计数，前端凭 revision 判断是否需要拉取完整状态。
		if r.URL.Query().Get("summary") != "" {
			brief := StateSummary{
				Revision:    revision,
				Task:        summarizeTask(s.state.Task),
				Busy:        s.state.Busy,
				Progress:    s.state.Progress,
				Error:       s.state.Error,
				Warnings:    s.state.Warnings,
				Directories: s.state.Directories,
				Account:     s.state.Account,
			}
			jsonResponse(w, 200, object{"state": brief, "roots": roots, "missing_directories": missingDirectories(s.state.Items, s.state.Directories, roots)})
			return
		}
		st := s.state
		st.Revision = revision
		jsonResponse(w, 200, object{"state": st, "roots": roots, "settings": settings, "missing_directories": missingDirectories(st.Items, st.Directories, roots)})
		return
	}
	if r.Method != "POST" {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Busy {
		errorResponse(w, apiErrorf(http.StatusConflict, "任务运行中，请等待完成"))
		return
	}
	switch r.URL.Path {
	case "/api/settings":
		var in Settings
		if e := decode(w, r, &in); e != nil {
			errorResponse(w, e)
			return
		}
		in.Rules = normalizedRules(in.Rules)
		e := validateRules(in.Rules)
		if e != nil {
			errorResponse(w, e)
			return
		}
		a.mu.Lock()
		e = saveSettings(a.settingsPath, in)
		if e == nil {
			a.settings = in
		}
		a.mu.Unlock()
		if e != nil {
			errorResponse(w, apiErrorf(http.StatusInternalServerError, "设置保存失败，请检查应用数据目录权限"))
			return
		}
		s.state.Plan = nil
		s.state.Inspection = nil
		if len(s.state.Items) > 0 {
			p, checkErr := buildLibraryPlan(s.state.Items, s.state.Directories, a.roots, nil, in.Rules)
			if checkErr != nil {
				s.state.Error = checkErr.Error()
			} else {
				s.state.Inspection = p
				s.state.Progress = "规则已保存，目录规范已重新检查"
			}
		}
		jsonResponse(w, 200, object{"ok": true})
	case "/api/scan":
		var in struct {
			Library string `json:"library"`
		}
		if e := decode(w, r, &in); e != nil {
			errorResponse(w, e)
			return
		}
		if s.fn == nil {
			errorResponse(w, errors.New("请先连接影视"))
			return
		}
		valid := false
		for _, lib := range s.state.Libraries {
			if str(lib, "guid") == in.Library {
				valid = true
			}
		}
		if !valid {
			errorResponse(w, apiErrorf(http.StatusNotFound, "媒体库不存在"))
			return
		}
		fn := s.fn
		a.mu.Lock()
		rules := a.settings.Rules
		a.mu.Unlock()
		s.state.Busy = true
		s.state.Error = ""
		s.state.Items = nil
		s.state.Plan = nil
		s.state.Inspection = nil
		s.state.Results = nil
		s.state.Task = nil
		s.state.Warnings = nil
		a.jobs.Add(1)
		go func() {
			defer a.jobs.Done()
			dirs, e := fn.LibraryDirectories(a.ctx, in.Library)
			var items []Media
			var warnings []string
			if e == nil {
				items, warnings, e = fn.Scan(a.ctx, in.Library, func(msg string) { s.mu.Lock(); s.state.Progress = msg; s.mu.Unlock() })
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			s.state.Busy = false
			if e != nil {
				s.state.Error = e.Error()
				s.state.Progress = "读取失败"
				return
			}
			inspection, checkErr := buildLibraryPlan(items, dirs, a.roots, nil, rules)
			if checkErr != nil {
				s.state.Error = checkErr.Error()
			}
			s.state.Inspection = inspection
			s.state.Directories = dirs
			s.state.Items = items
			s.state.Warnings = warnings
			s.state.Progress = fmt.Sprintf("已读取并检查，共 %d 个文件", len(items))
		}()
		jsonResponse(w, 202, object{"ok": true})
	case "/api/preview", "/api/check":
		var in struct {
			IDs []string `json:"ids"`
		}
		if e := decode(w, r, &in); e != nil {
			errorResponse(w, e)
			return
		}
		a.mu.Lock()
		rules := a.settings.Rules
		a.mu.Unlock()
		wanted := map[string]bool{}
		for _, id := range in.IDs {
			wanted[id] = true
		}
		items := []Media{}
		for _, m := range s.state.Items {
			if wanted[m.ID] {
				items = append(items, m)
			}
		}
		if len(items) == 0 {
			errorResponse(w, errors.New("请选择至少一个视频"))
			return
		}
		p, e := buildLibraryPlan(items, s.state.Directories, a.roots, nil, rules)
		if e != nil {
			errorResponse(w, e)
			return
		}
		if r.URL.Path == "/api/preview" {
			if issues := permissionIssues(p.Rows, s.state.Directories, a.roots); len(issues) > 0 {
				jsonResponse(w, 403, object{"error": "请先为所选影视目录添加读写权限", "permission_issues": issues})
				return
			}
		}
		if r.URL.Path == "/api/check" {
			s.state.Inspection = p
			s.state.Plan = nil
		} else {
			s.state.Plan = p
		}
		s.state.Results = nil
		s.state.Task = nil
		s.state.Error = ""
		if r.URL.Path == "/api/check" {
			s.state.Progress = "目录规范检查完成，未移动文件"
		} else {
			s.state.Progress = "预览已生成，尚未移动文件"
		}
		jsonResponse(w, 200, p)
	case "/api/execute":
		var in struct {
			PlanID  string `json:"plan_id"`
			Indices []int  `json:"indices"`
		}
		if e := decode(w, r, &in); e != nil {
			errorResponse(w, e)
			return
		}
		p := s.state.Plan
		if p == nil || p.ID != in.PlanID || time.Since(p.Created) > 30*time.Minute {
			errorResponse(w, apiErrorf(http.StatusGone, "预览已失效，请重新预览"))
			return
		}
		rows := []Row{}
		seen := map[int]bool{}
		for _, i := range in.Indices {
			if i < 0 || i >= len(p.Rows) || p.Rows[i].Status != "ready" || seen[i] {
				errorResponse(w, apiErrorf(http.StatusConflict, "所选条目无法执行，请重新预览"))
				return
			}
			seen[i] = true
			rows = append(rows, p.Rows[i])
		}
		if len(rows) == 0 {
			errorResponse(w, errors.New("没有可移动的条目"))
			return
		}
		if issues := permissionIssues(rows, s.state.Directories, a.roots); len(issues) > 0 {
			jsonResponse(w, 403, object{"error": "目录权限不足，尚未开始整理", "permission_issues": issues})
			return
		}
		if !a.moveMu.TryLock() {
			errorResponse(w, apiErrorf(http.StatusConflict, "另一个整理任务正在运行"))
			return
		}
		s.state.Plan = nil
		s.state.Busy = true
		s.state.Results = []Row{}
		s.state.Task = &TaskProgress{Total: len(rows), Active: true}
		for _, row := range rows {
			s.state.Task.FileIDs = append(s.state.Task.FileIDs, row.ID)
		}
		s.state.Progress = "正在准备整理"
		s.state.Error = ""
		a.jobs.Add(1)
		go func() {
			defer a.jobs.Done()
			defer a.moveMu.Unlock()
			for i, row := range rows {
				select {
				case <-a.ctx.Done():
					s.mu.Lock()
					s.state.Error = "服务正在停止，未开始的文件已保留"
					s.mu.Unlock()
					goto done
				default:
				}
				s.mu.Lock()
				s.state.Task.Current = filepath.Base(row.Source)
				s.state.Task.CurrentID = row.ID
				s.state.Progress = fmt.Sprintf("正在整理 %d/%d：%s", i+1, len(rows), row.Title)
				s.mu.Unlock()
				if e := a.journal.Write(object{"time": time.Now(), "plan": p.ID, "phase": "intent", "row": row}); e != nil {
					s.mu.Lock()
					s.state.Error = "操作日志无法写入，任务停止"
					s.mu.Unlock()
					break
				}
				out, e := moveFile(row, a.roots)
				row.Status = "done"
				row.Reason = "移动完成"
				if e != nil {
					row.Status = "failed"
					row.Reason = e.Error()
					if os.IsPermission(e) {
						row.IssueCode = "permission"
						row.Reason = "目录权限不足，原文件已保留。请为目录添加读写权限后重试。"
					}
				}
				if e == nil {
					cleaned, cleanupErr := cleanupEmptySourceDirs(row, a.roots)
					row.CleanedDirectories = cleaned
					if cleanupErr != nil {
						row.CleanupWarning = "文件已整理，空目录未能完全清理：" + cleanupErr.Error()
					}
				}
				journalErr := a.journal.Write(object{"time": time.Now(), "plan": p.ID, "phase": "result", "mode": out.Mode, "created_directories": out.CreatedDirectories, "rolled_back_directories": out.RolledBackDirectories, "row": row})
				s.mu.Lock()
				s.state.Results = append(s.state.Results, row)
				s.state.Task.Completed++
				if row.Status == "done" {
					s.state.Task.Succeeded++
				} else {
					s.state.Task.Failed++
				}
				if row.Status == "done" {
					for n := range s.state.Items {
						if s.state.Items[n].ID == row.ID && s.state.Items[n].Source == row.Source {
							s.state.Items[n].Source = row.Target
						}
					}
				}
				if journalErr != nil {
					s.state.Error = "结果日志写入失败，停止后续移动"
				}
				s.mu.Unlock()
				if journalErr != nil {
					break
				}
			}
		done:
			s.mu.Lock()
			s.state.Busy = false
			s.state.Task.Active = false
			s.state.Task.Current = ""
			s.state.Task.CurrentID = ""
			a.mu.Lock()
			currentRules := a.settings.Rules
			a.mu.Unlock()
			s.state.Inspection, _ = buildLibraryPlan(s.state.Items, s.state.Directories, a.roots, nil, currentRules)
			if s.state.Task.Completed < s.state.Task.Total {
				s.state.Progress = "整理已停止，请检查执行结果"
			} else {
				s.state.Progress = "整理完成，请在飞牛影视重新扫描媒体库"
			}
			s.mu.Unlock()
		}()
		jsonResponse(w, 202, object{"ok": true})
	default:
		http.NotFound(w, r)
	}
}
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	socket := flag.String("socket", filepath.Join(envOr("TRIM_APPDEST", "."), "app.sock"), "统一网关 Unix socket")
	data := flag.String("data", envOr("TRIM_PKGVAR", "./data"), "数据目录")
	rootsValue := flag.String("roots", os.Getenv("TRIM_DATA_ACCESSIBLE_PATHS"), "获授权目录列表")
	mediaBase := flag.String("media-base", envOr("ORGANIZER_MEDIA_BASE", "http://127.0.0.1:5666"), "本机影视服务地址")
	flag.Parse()
	if e := os.MkdirAll(*data, 0700); e != nil {
		log.Fatal(e)
	}
	roots := []string{}
	for _, root := range filepath.SplitList(*rootsValue) {
		if filepath.IsAbs(root) {
			roots = append(roots, filepath.Clean(root))
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settingsPath := filepath.Join(*data, "settings.json")
	a := &App{sessions: map[string]*Session{}, mediaBase: *mediaBase, roots: roots, journal: Journal{filepath.Join(*data, "operations.jsonl")}, ctx: ctx, settingsPath: settingsPath, settings: loadSettings(settingsPath)}
	// 后台定期回收过期会话，避免 sessions Map 随授权次数无界增长。
	a.startSessionGC(ctx, sessionGCInterval)
	if info, e := os.Lstat(*socket); e == nil {
		if info.Mode()&os.ModeSocket == 0 {
			log.Fatal("网关路径已被非 socket 文件占用")
		}
		if e = os.Remove(*socket); e != nil {
			log.Fatal(e)
		}
	}
	listener, e := net.Listen("unix", *socket)
	if e != nil {
		log.Fatal(e)
	}
	defer listener.Close()
	defer os.Remove(*socket)
	// Only the package user and privileged NAS gateway can open this socket.
	if e = os.Chmod(*socket, 0600); e != nil {
		log.Fatal(e)
	}
	server := &http.Server{Handler: gatewayHandler(a), ConnContext: func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, gatewayContextKey{}, c)
	}, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		cancel()
		shutdown, release := context.WithTimeout(context.Background(), 15*time.Second)
		defer release()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("影视整理器已启动 %s，获授权目录数 %d", *socket, len(roots))
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	a.jobs.Wait()
	// 停机时清空会话凭据与内存缓存，影视 token 只存在于内存中。
	a.shutdown()
}

// Only the fixed local media image endpoint is allowed. No URL or disk proxy.
func posterPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("缺少封面")
	}
	u, e := url.Parse(p)
	if e != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("封面路径无效")
	}
	p = strings.TrimPrefix(u.Path, "/v/api/v1/sys/img/")
	p = strings.TrimPrefix(p, "/")
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
			return "", errors.New("封面路径无效")
		}
	}
	return "/v/api/v1/sys/img/" + p + "?w=160", nil
}
