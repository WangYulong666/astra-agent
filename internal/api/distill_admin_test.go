package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/WangYulong666/astra-agent/internal/capability"
	"github.com/WangYulong666/astra-agent/internal/distill"
	"github.com/WangYulong666/astra-agent/internal/skillhub"
)

// ── 假实现：只关心「accept/reject 后 Capabilities.Refresh 有没有被调用」 ──

type refreshCounter struct{ calls int }

type fakeCapRegistry struct {
	c   *refreshCounter
	err error
}

func (f *fakeCapRegistry) List(context.Context, bool) ([]capability.Item, error) {
	return nil, nil
}
func (f *fakeCapRegistry) Get(context.Context, string) (capability.Item, error) {
	return capability.Item{}, capability.ErrNotFound
}
func (f *fakeCapRegistry) Refresh(context.Context) error {
	f.c.calls++
	return f.err
}

var _ capability.Registry = (*fakeCapRegistry)(nil)

type fakeSkillHub struct{}

func (fakeSkillHub) List(context.Context) ([]skillhub.Skill, error) { return nil, nil }
func (fakeSkillHub) Get(context.Context, string) (skillhub.Skill, error) {
	return skillhub.Skill{}, nil
}
func (fakeSkillHub) Refresh(context.Context) error { return nil }
func (fakeSkillHub) Publish(context.Context, string, string, []string) (string, error) {
	return "", nil
}

var _ skillhub.Hub = fakeSkillHub{}

type fakeDistiller struct{}

func (fakeDistiller) Enabled() bool { return true }
func (fakeDistiller) Distill(context.Context, distill.Request) (distill.Draft, error) {
	return distill.Draft{}, nil
}
func (fakeDistiller) List(context.Context, string) ([]distill.Draft, error) { return nil, nil }
func (fakeDistiller) Accept(context.Context, string, string) (string, error) {
	return "/skills/x/SKILL.md", nil
}
func (fakeDistiller) Reject(context.Context, string, string) error { return nil }

var _ distill.Distiller = fakeDistiller{}

func newDistillHandler(reg capability.Registry) *handler {
	return &handler{
		cfg: Config{Distiller: fakeDistiller{}, SkillHub: fakeSkillHub{}, Capabilities: reg},
		log: log.New(io.Discard, "", 0),
	}
}

func callDraftAction(t *testing.T, h *handler, fn http.HandlerFunc, action string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/drafts/d1/"+action, nil)
	req.SetPathValue("id", "d1")
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec.Code
}

func TestAcceptAndRejectRefreshCapabilities(t *testing.T) {
	c := &refreshCounter{}
	h := newDistillHandler(&fakeCapRegistry{c: c})

	for _, tc := range []struct {
		name       string
		fn         http.HandlerFunc
		wantStatus int
	}{
		{"accept", h.acceptSkillDraft, http.StatusOK},
		{"reject", h.rejectSkillDraft, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.calls = 0
			if code := callDraftAction(t, h, tc.fn, tc.name); code != tc.wantStatus {
				t.Fatalf("状态码 = %d, want %d", code, tc.wantStatus)
			}
			if c.calls != 1 {
				t.Fatalf("%s 成功后应触发 1 次 Capabilities.Refresh, got %d", tc.name, c.calls)
			}
		})
	}
}

// Refresh 报错只记日志，不能把一次成功的 accept/reject 改成 500。
func TestRefreshErrorDoesNotFailResponse(t *testing.T) {
	c := &refreshCounter{}
	h := newDistillHandler(&fakeCapRegistry{c: c, err: errors.New("collect 失败")})

	for _, tc := range []struct {
		name       string
		fn         http.HandlerFunc
		wantStatus int
	}{
		{"accept", h.acceptSkillDraft, http.StatusOK},
		{"reject", h.rejectSkillDraft, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := callDraftAction(t, h, tc.fn, tc.name); code != tc.wantStatus {
				t.Fatalf("Refresh 失败不应变 5xx: 状态码 = %d, want %d", code, tc.wantStatus)
			}
		})
	}
}

// Capabilities 为 nil（能力注册表未启用）时不能 panic，也不能影响响应。
func TestRefreshNilCapabilitiesIsSafe(t *testing.T) {
	h := newDistillHandler(nil)
	if code := callDraftAction(t, h, h.acceptSkillDraft, "accept"); code != http.StatusOK {
		t.Fatalf("nil Capabilities 状态码 = %d, want 200", code)
	}
	if code := callDraftAction(t, h, h.rejectSkillDraft, "reject"); code != http.StatusNoContent {
		t.Fatalf("nil Capabilities 状态码 = %d, want 204", code)
	}
}
