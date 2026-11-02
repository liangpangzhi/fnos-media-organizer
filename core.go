package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

type Media struct {
	ID           string `json:"id"`
	Poster       string `json:"poster,omitempty"`
	Title        string `json:"title"`
	Year         string `json:"year"`
	Kind         string `json:"kind"`
	Season       int    `json:"season"`
	Episode      int    `json:"episode"`
	EpisodeTitle string `json:"episode_title,omitempty"`
	Source       string `json:"source"`
}
type Row struct {
	Media
	Target             string `json:"target"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	IssueCode          string `json:"issue_code,omitempty"`
	info               os.FileInfo
	libraryRoot        string
	CleanedDirectories []string `json:"cleaned_directories,omitempty"`
	CleanupWarning     string   `json:"cleanup_warning,omitempty"`
}
type Plan struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Rows    []Row     `json:"rows"`
}

var invalidName = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
var yearPattern = regexp.MustCompile(`^(18|19|20|21)\d{2}$`)
var mediaExtensions = map[string]bool{".mkv": true, ".mp4": true, ".avi": true, ".mov": true, ".m4v": true, ".wmv": true, ".ts": true, ".m2ts": true, ".mpg": true, ".mpeg": true, ".webm": true, ".flv": true, ".iso": true}
var formatPattern = regexp.MustCompile(`^\.[a-z0-9]{1,8}$`)

func parseFormats(values []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, v := range values {
		v = strings.ToLower(strings.TrimSpace(v))
		if !strings.HasPrefix(v, ".") {
			v = "." + v
		}
		if !formatPattern.MatchString(v) {
			return nil, errors.New("扩展名格式无效")
		}
		out[v] = true
	}
	if len(out) == 0 {
		return nil, errors.New("请至少选择一种文件格式")
	}
	return out, nil
}

func safeName(s string) (string, error) {
	s = strings.Trim(strings.TrimSpace(invalidName.ReplaceAllString(s, "_")), ". ")
	if s == "" || len([]byte(s)) > 160 {
		return "", errors.New("名称为空或过长")
	}
	return s, nil
}
func inside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Reject symlinks in every existing component, including parent directories.
// 实现见 cache.go：叶子实时检查，祖先目录使用带短 TTL 的进程内缓存，
// 保证大库规划阶段不做重复的逐级 Lstat。
func noSymlinks(p string) error {
	return symlinkFree(p)
}

var errDirectoryGrant = errors.New("路径不在应用获授权的目录内；请在系统应用设置中授权目录并重启应用")

func allowedPath(p string, roots []string) error {
	if !filepath.IsAbs(p) {
		return errors.New("需要 NAS 上的绝对路径")
	}
	p = filepath.Clean(p)
	for _, root := range roots {
		if filepath.IsAbs(root) && inside(filepath.Clean(root), p) {
			return noSymlinks(p)
		}
	}
	return errDirectoryGrant
}
func targetPath(m Media, root string, formats map[string]bool, rules ...Rules) (string, error) {
	if formats != nil && !formats[strings.ToLower(filepath.Ext(m.Source))] {
		return "", errors.New("文件格式未选中")
	}
	r := defaultRules()
	if len(rules) > 0 {
		r = rules[0]
	}
	return ruleTarget(m, root, r)
}
func buildPlan(items []Media, sourceRoot, targetRoot string, roots []string, formats map[string]bool, rules ...Rules) (*Plan, error) {
	if err := allowedPath(sourceRoot, roots); err != nil {
		return nil, err
	}
	if err := allowedPath(targetRoot, roots); err != nil {
		return nil, err
	}
	srcInfo, err := os.Stat(sourceRoot)
	if os.IsPermission(err) {
		return nil, err
	}
	if err != nil || !srcInfo.IsDir() {
		return nil, errors.New("源目录不存在或不是目录")
	}
	targetInfo, err := os.Stat(targetRoot)
	if os.IsPermission(err) {
		return nil, err
	}
	if err != nil || !targetInfo.IsDir() {
		return nil, errors.New("目标根目录必须已存在且为目录")
	}
	plan := &Plan{ID: randomID(), Created: time.Now(), Rows: []Row{}}
	targets := map[string][]int{}
	sources := map[string][]int{}
	for _, m := range items {
		m.Source = filepath.Clean(m.Source)
		row := Row{Media: m, Status: "ready", libraryRoot: sourceRoot}
		var e error
		switch {
		case !filepath.IsAbs(m.Source) || !inside(sourceRoot, m.Source):
			e = errors.New("不在所选源目录内")
		default:
			e = allowedPath(m.Source, roots)
			if e == nil {
				row.info, e = os.Stat(m.Source)
			}
			if e == nil && !row.info.Mode().IsRegular() {
				e = errors.New("不是普通文件")
			}
			if e == nil && !sameVolume(row.info, targetInfo, m.Source, targetRoot) {
				e = errors.New("跨存储空间，已跳过；仅支持同一存储空间内整理")
			}
			if e == nil {
				row.Target, e = targetPath(m, targetRoot, formats, rules...)
			}
			if e == nil {
				e = allowedPath(row.Target, roots)
			}
			if e == nil && row.Target == m.Source {
				row.Status = "unchanged"
				row.Reason = "已符合命名规则"
			}
			if e == nil && row.Status == "ready" {
				_, statErr := os.Lstat(row.Target)
				if statErr == nil {
					e = errors.New("目标文件已存在，不覆盖")
				}
				if statErr != nil && !os.IsNotExist(statErr) {
					e = statErr
				}
			}
		}
		if e != nil {
			row.Status = "blocked"
			row.Reason = e.Error()
			if errors.Is(e, errDirectoryGrant) || os.IsPermission(e) {
				row = metadataRuleRow(m, targetRoot, formats, rules...)
			}
		}
		i := len(plan.Rows)
		plan.Rows = append(plan.Rows, row)
		if row.Target != "" {
			targets[row.Target] = append(targets[row.Target], i)
		}
		sources[m.Source] = append(sources[m.Source], i)
	}
	for _, indices := range targets {
		if len(indices) > 1 {
			for _, i := range indices {
				plan.Rows[i].Status = "blocked"
				plan.Rows[i].Reason = "多份视频指向同一目标，请先选择保留的版本"
			}
		}
	}
	for _, indices := range sources {
		if len(indices) > 1 {
			for _, i := range indices {
				plan.Rows[i].Status = "blocked"
				plan.Rows[i].Reason = "同一源文件出现在多个条目中"
			}
		}
	}
	return plan, nil
}
func fingerprintUnchanged(p string, original os.FileInfo) error {
	info, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || original == nil || !os.SameFile(info, original) || info.Size() != original.Size() || !info.ModTime().Equal(original.ModTime()) {
		return errors.New("预览后文件发生变化，请重新预览")
	}
	return nil
}
func syncDir(p string) {
	f, e := os.Open(p)
	if e == nil {
		_ = f.Sync()
		_ = f.Close()
	}
}

// moveOutcome 描述一次移动实际发生了什么，用于写入操作日志。
// Mode 记录真实的搬移机制，CreatedDirectories 记录本次新建的目标目录，
// RolledBackDirectories 记录移动失败时被撤销的新建空目录。
type moveOutcome struct {
	Mode                  string   `json:"mode"`
	CreatedDirectories    []string `json:"created_directories,omitempty"`
	RolledBackDirectories []string `json:"rolled_back_directories,omitempty"`
}

// moveMode 返回当前平台实际使用的"不覆盖"搬移机制，便于事后审计。
func moveMode() string {
	if runtime.GOOS == "linux" {
		return "renameat2-noreplace"
	}
	return "link+unlink"
}

// makeTargetDirs 逐级创建目标目录，并返回本次真正新建的目录（自顶向下顺序）。
// 已存在的目录不会被记录，因此失败回滚时不会误删用户原有目录。
func makeTargetDirs(dir string) ([]string, error) {
	dir = filepath.Clean(dir)
	missing := []string{}
	for p := dir; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		missing = append(missing, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	created := make([]string, 0, len(missing))
	for i := len(missing) - 1; i >= 0; i-- {
		created = append(created, missing[i])
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return created, err
	}
	return created, nil
}

// rollbackCreatedDirs 逆序删除本次新建的空目录；遇到非空目录立即停止，
// 避免误删他人并发写入的内容。返回实际删除的目录。
func rollbackCreatedDirs(created []string) []string {
	removed := []string{}
	for i := len(created) - 1; i >= 0; i-- {
		err := removeEmptyDirectory(created[i])
		if err == nil {
			removed = append(removed, created[i])
			continue
		}
		if os.IsNotExist(err) {
			continue
		}
		break
	}
	return removed
}

// Same-filesystem move without replacing an existing target.
func moveFile(row Row, roots []string) (moveOutcome, error) {
	out := moveOutcome{Mode: moveMode()}
	if err := allowedPath(row.Source, roots); err != nil {
		return out, err
	}
	if err := allowedPath(row.Target, roots); err != nil {
		return out, err
	}
	if err := fingerprintUnchanged(row.Source, row.info); err != nil {
		return out, err
	}
	parent := filepath.Dir(row.Target)
	for {
		if _, e := os.Stat(parent); e == nil {
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			return out, errors.New("目标父目录不可访问")
		}
		parent = next
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return out, err
	}
	if !sameVolume(row.info, parentInfo, row.Source, parent) {
		return out, errors.New("跨存储空间，拒绝移动")
	}
	created, err := makeTargetDirs(filepath.Dir(row.Target))
	out.CreatedDirectories = created
	if err != nil {
		out.RolledBackDirectories = rollbackCreatedDirs(created)
		return out, fmt.Errorf("无法创建目标目录: %w", err)
	}
	// 目录创建之后的任何失败都必须撤销本次新建的空目录，避免留下空壳目录。
	fail := func(e error) (moveOutcome, error) {
		out.RolledBackDirectories = rollbackCreatedDirs(created)
		if len(out.RolledBackDirectories) > 0 {
			out.CreatedDirectories = nil
			e = fmt.Errorf("%w（已回滚本次新建目录 %d 个）", e, len(out.RolledBackDirectories))
		}
		return out, e
	}
	if err := allowedPath(row.Target, roots); err != nil {
		return fail(err)
	}
	if _, err := os.Lstat(row.Target); err == nil {
		return fail(errors.New("目标已经存在，已跳过"))
	} else if !os.IsNotExist(err) {
		return fail(err)
	}
	if err := fingerprintUnchanged(row.Source, row.info); err != nil {
		return fail(err)
	}
	if err := moveNoReplace(row.Source, row.Target); err != nil {
		return fail(fmt.Errorf("移动失败，原文件保留: %w", err))
	}
	syncDir(filepath.Dir(row.Target))
	syncDir(filepath.Dir(row.Source))
	return out, nil
}

// journalMaxBytes 是 operations.jsonl 的轮转阈值，超过后改名为 .1 并重新开始，
// 保证长期运行不会无限占用应用数据目录。
var journalMaxBytes int64 = 4 << 20

type Journal struct{ Path string }

// Write 以追加方式写入一条操作记录；文件超过阈值时先轮转。
// 轮转只保留一份历史（operations.jsonl.1），不删除任何用户文件。
func (j Journal) Write(value any) error {
	if err := rotateJournal(j.Path, journalMaxBytes); err != nil {
		return err
	}
	f, err := os.OpenFile(j.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = json.NewEncoder(f).Encode(value); err != nil {
		return err
	}
	return f.Sync()
}

// rotateJournal 在文件超过 maxBytes 时把现有日志改名为 <path>.1（覆盖旧的历史文件）。
// maxBytes <= 0 表示不轮转。
func rotateJournal(path string, maxBytes int64) error {
	if maxBytes <= 0 || path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Size() < maxBytes {
		return nil
	}
	backup := path + ".1"
	if err := os.Rename(path, backup); err != nil {
		// 轮转失败不应阻断操作日志写入：继续向原文件追加。
		return nil
	}
	syncDir(filepath.Dir(path))
	return nil
}
