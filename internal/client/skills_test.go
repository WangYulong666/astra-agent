package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 草稿闸门的协议翻译测试：路径、方法、Bearer 头、204 无体、错误映射。
// 用 httptest，不打真控制面。

func TestSkillDrafts(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"d1","name":"deploy-helm","status":"draft","description":"发版","created_at":"2026-11-09T01:02:03Z"}]`))
	}))
	defer srv.Close()

	drafts, err := New(srv.URL, "tok").SkillDrafts(context.Background(), 5)
	if err != nil {
		t.Fatalf("SkillDrafts: %v", err)
	}
	if gotPath != "/v1/skills/drafts" || !strings.Contains(gotQuery, "limit=5") {
		t.Fatalf("请求不对: %s?%s", gotPath, gotQuery)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("缺 Bearer 头: %q", gotAuth)
	}
	if len(drafts) != 1 || drafts[0].ID != "d1" || drafts[0].Name != "deploy-helm" || drafts[0].Status != "draft" {
		t.Fatalf("解析不对: %+v", drafts)
	}
}

// 空数组不能因为 out == nil 判失败；也不能把 null 当成 nil slice 漏给上层。
func TestSkillDraftsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`null`))
	}))
	defer srv.Close()

	drafts, err := New(srv.URL, "tok").SkillDrafts(context.Background(), 0)
	if err != nil {
		t.Fatalf("SkillDrafts: %v", err)
	}
	if drafts == nil || len(drafts) != 0 {
		t.Fatalf("应返回空 slice 而不是 nil: %#v", drafts)
	}
}

func TestAcceptSkillDraft(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"path":"/home/u/.astra/skills/deploy-helm/SKILL.md"}`))
	}))
	defer srv.Close()

	path, err := New(srv.URL, "tok").AcceptSkillDraft(context.Background(), "d1")
	if err != nil {
		t.Fatalf("AcceptSkillDraft: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/skills/drafts/d1/accept" {
		t.Fatalf("请求不对: %s %s", gotMethod, gotPath)
	}
	if !strings.HasSuffix(path, "/deploy-helm/SKILL.md") {
		t.Fatalf("返回路径不对: %q", path)
	}
}

// reject 走 204：do() 必须不因空体报错，CLI 才能只回一行确认。
func TestRejectSkillDraftNoContent(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := New(srv.URL, "tok").RejectSkillDraft(context.Background(), "d1"); err != nil {
		t.Fatalf("RejectSkillDraft: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/skills/drafts/d1/reject" {
		t.Fatalf("请求不对: %s %s", gotMethod, gotPath)
	}
}

// 服务端 503（沉淀未启用）要透出可读错误，别只剩一个状态码。
func TestSkillDraftsDistillDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"技能沉淀未启用：检查 DISTILL_ENABLED（总开关）与服务启动日志"}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "tok").SkillDrafts(context.Background(), 0)
	if err == nil {
		t.Fatal("503 应返回错误")
	}
	if !strings.Contains(err.Error(), "未启用") {
		t.Fatalf("错误信息应包含服务端原因: %v", err)
	}
}
