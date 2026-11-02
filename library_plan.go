package main

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Prefer the most specific configured library directory. Do not infer roots from files.
// 结果行与 items 一一对应且保持输入顺序：按条目下标分组，不再按内容反查，
// 既避免重复 ID/Source 条目被合并丢失，也避免 O(n²) 的反查开销。
func buildLibraryPlan(items []Media, dirs, authorized []string, formats map[string]bool, rules ...Rules) (*Plan, error) {
	if len(dirs) == 0 {
		return nil, errors.New("请先读取影视库目录")
	}
	roots := make([]string, len(items))
	groups := map[string][]int{}
	groupOrder := make([]string, 0, len(items))
	for i, m := range items {
		root := ""
		for _, d := range dirs {
			d = filepath.Clean(d)
			if filepath.IsAbs(d) && inside(d, m.Source) && len(d) > len(root) {
				root = d
			}
		}
		roots[i] = root
		if root == "" {
			continue
		}
		if _, ok := groups[root]; !ok {
			groupOrder = append(groupOrder, root)
		}
		groups[root] = append(groups[root], i)
	}
	p := &Plan{ID: randomID(), Created: time.Now(), Rows: make([]Row, len(items))}
	filled := make([]bool, len(items))
	for i, m := range items {
		if roots[i] != "" {
			continue
		}
		p.Rows[i] = Row{Media: m, Status: "blocked", Reason: "文件不在所选影视库目录内"}
		filled[i] = true
	}
	for _, root := range groupOrder {
		indices := groups[root]
		group := make([]Media, len(indices))
		for k, i := range indices {
			group[k] = items[i]
		}
		plan, e := buildPlan(group, root, root, authorized, formats, rules...)
		if e == nil && len(plan.Rows) != len(indices) {
			e = errors.New("规划结果与条目数量不一致")
		}
		if e != nil {
			for _, i := range indices {
				row := Row{Media: items[i], Status: "blocked", Reason: e.Error()}
				if errors.Is(e, errDirectoryGrant) || os.IsPermission(e) {
					row = metadataRuleRow(items[i], root, formats, rules...)
				}
				p.Rows[i] = row
				filled[i] = true
			}
			continue
		}
		for k, i := range indices {
			p.Rows[i] = plan.Rows[k]
			filled[i] = true
		}
	}
	for i := range filled {
		if !filled[i] {
			p.Rows[i] = Row{Media: items[i], Status: "blocked", Reason: "无法生成整理计划"}
		}
	}
	// Metadata-only rows must apply the same duplicate checks as disk-backed rows.

	targets, sources := map[string][]int{}, map[string][]int{}
	for i, row := range p.Rows {
		if row.Target != "" {
			targets[row.Target] = append(targets[row.Target], i)
		}
		sources[row.Source] = append(sources[row.Source], i)
	}
	for _, indices := range targets {
		if len(indices) > 1 {
			for _, i := range indices {
				p.Rows[i].Status = "blocked"
				p.Rows[i].Reason = "多份视频指向同一目标，请先选择保留的版本"
			}
		}
	}
	for _, indices := range sources {
		if len(indices) > 1 {
			for _, i := range indices {
				p.Rows[i].Status = "blocked"
				p.Rows[i].Reason = "同一源文件出现在多个条目中"
			}
		}
	}
	return p, nil
}

// The media API already provides paths and recognition data. Comparing naming
// rules does not grant file access or verify filesystem conflicts.
func metadataRuleRow(m Media, root string, formats map[string]bool, rules ...Rules) Row {
	m.Source = filepath.Clean(m.Source)
	row := Row{Media: m, Status: "ready", libraryRoot: root}
	target, err := targetPath(m, root, formats, rules...)
	if err != nil {
		row.Status = "blocked"
		row.Reason = err.Error()
		return row
	}
	row.Target = target
	if target == m.Source {
		row.Status = "unchanged"
	}
	return row
}
