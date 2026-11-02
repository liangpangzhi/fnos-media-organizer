package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 结果行必须与输入条目一一对应且顺序不变：重复 ID + 重复 Source 的条目
// 曾被按内容反查合并成一行，导致计划缺行。
func TestLibraryPlanPreservesOrderAndDuplicates(t *testing.T) {
	src, dst, roots := fixture(t)
	a := video(t, src, "a.mkv")
	b := video(t, src, "b.mkv")
	c := video(t, src, "c.mkv")
	items := []Media{a, b, a, c}
	p, e := buildLibraryPlan(items, []string{src, dst}, roots, mediaExtensions)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Rows) != len(items) {
		t.Fatalf("row count %d，期望 %d", len(p.Rows), len(items))
	}
	for i, m := range items {
		if p.Rows[i].ID != m.ID || p.Rows[i].Source != filepath.Clean(m.Source) {
			t.Fatalf("第 %d 行与输入条目不对应：%s/%s vs %s/%s", i, p.Rows[i].ID, p.Rows[i].Source, m.ID, m.Source)
		}
	}
	// 重复出现必须被判为冲突并保留两行，而不是静默丢弃一行。
	for _, i := range []int{0, 2} {
		if p.Rows[i].Status != "blocked" || p.Rows[i].Reason != "同一源文件出现在多个条目中" {
			t.Fatalf("重复源未被拦截：%+v", p.Rows[i])
		}
	}
}

// 条目跨多个影视库目录、并夹杂库外文件时，顺序仍须与输入一致。
func TestLibraryPlanKeepsInputOrderAcrossLibraries(t *testing.T) {
	src, dst, roots := fixture(t)
	one := video(t, src, "one.mkv")
	one.ID, one.Title = "one", "第一部"
	two := video(t, dst, "two.mkv")
	two.ID, two.Title = "two", "第二部"
	outsideDir := filepath.Join(roots[0], "other")
	if e := os.MkdirAll(outsideDir, 0755); e != nil {
		t.Fatal(e)
	}
	outside := filepath.Join(outsideDir, "three.mkv")
	if e := os.WriteFile(outside, []byte("test media content"), 0600); e != nil {
		t.Fatal(e)
	}
	three := Media{ID: "three", Title: "第三部", Year: "2024", Kind: "movie", Source: outside}
	items := []Media{one, two, three}
	p, e := buildLibraryPlan(items, []string{src, dst}, roots, mediaExtensions)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Rows) != 3 {
		t.Fatalf("row count %d", len(p.Rows))
	}
	if p.Rows[0].ID != "one" || !inside(src, p.Rows[0].Target) {
		t.Fatalf("第一行顺序或目标目录错误：%+v", p.Rows[0])
	}
	if p.Rows[1].ID != "two" || !inside(dst, p.Rows[1].Target) {
		t.Fatalf("第二行顺序或目标目录错误：%+v", p.Rows[1])
	}
	if p.Rows[2].ID != "three" || p.Rows[2].Status != "blocked" || p.Rows[2].Reason != "文件不在所选影视库目录内" {
		t.Fatalf("库外条目未被拦截：%+v", p.Rows[2])
	}
}

// 大条目量下仍要保证一一对应（同时覆盖按目录分组的实现不再做内容反查）。
func TestLibraryPlanLargeInputCoverage(t *testing.T) {
	src, dst, roots := fixture(t)
	items := make([]Media, 0, 300)
	for i := 0; i < 300; i++ {
		m := video(t, src, fmt.Sprintf("video-%03d.mkv", i))
		m.ID = fmt.Sprintf("id-%03d", i)
		m.Title = fmt.Sprintf("影片 %03d", i)
		items = append(items, m)
	}
	p, e := buildLibraryPlan(items, []string{src, dst}, roots, mediaExtensions)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Rows) != len(items) {
		t.Fatalf("row count %d，期望 %d", len(p.Rows), len(items))
	}
	for i := range items {
		if p.Rows[i].ID != items[i].ID {
			t.Fatalf("第 %d 行 ID 错位：%s vs %s", i, p.Rows[i].ID, items[i].ID)
		}
	}
}
