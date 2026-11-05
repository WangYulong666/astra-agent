package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// fakeAdmin 只实现 InsertAudit，其余方法靠嵌入的 nil 接口占位。
//
// 为什么可以这样：测试只走 InsertAudit 这一条路径，其他方法被调用会 nil panic
// ——而这本身就是有效的断言（说明审计器越界调用了别的接口）。
type fakeAdmin struct {
	store.AdminStore
	called chan store.AuditLog
	err    error
	panicV any
}

func (f *fakeAdmin) InsertAudit(_ context.Context, a store.AuditLog) error {
	if f.called != nil {
		select {
		case f.called <- a:
		default:
		}
	}
	if f.panicV != nil {
		panic(f.panicV)
	}
	return f.err
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// 写入失败不能 panic（要求里最硬的一条：审计不是主流程的失败点）。
func TestAuditorWriteErrorDoesNotPanic(t *testing.T) {
	called := make(chan store.AuditLog, 1)
	a := NewAuditor(&fakeAdmin{called: called, err: errors.New("db is down")}, quietLogger())
	defer a.Close()

	// 故意连调多次：worker 池 + 队列满的路径都要走到。
	for i := 0; i < auditQueueSize+10; i++ {
		a.Audit(context.Background(), store.AuditLog{
			Action: "session.create", TenantID: "t1", TargetID: "s1",
		})
	}

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("审计从未被写入（worker 没跑起来）")
	}
}

// 存储实现 panic 时，worker 必须活着把后续审计也照常处理。
func TestAuditorStorePanicIsContained(t *testing.T) {
	called := make(chan store.AuditLog, 16)
	a := NewAuditor(&fakeAdmin{called: called, panicV: "boom"}, quietLogger())
	defer a.Close()

	a.Audit(context.Background(), store.AuditLog{Action: "a"})
	a.Audit(context.Background(), store.AuditLog{Action: "b"})

	deadline := time.After(2 * time.Second)
	got := 0
	for got < 2 {
		select {
		case <-called:
			got++
		case <-deadline:
			t.Fatalf("只处理了 %d 条审计，worker 可能被 panic 打死", got)
		}
	}
}

// 没有治理层存储时也不能崩：file store 模式下 Admin 为 nil。
func TestAuditorNilStore(t *testing.T) {
	a := NewAuditor(nil, quietLogger())
	defer a.Close()
	a.Audit(context.Background(), store.AuditLog{Action: "x"})

	// nil receiver 也要安全：api 在 Auditor 未配置时会直接不调用，
	// 但测试里显式验一下，免得将来有人写成 var auditor *Auditor; auditor.Audit(...)
	var nilAuditor *Auditor
	nilAuditor.Audit(context.Background(), store.AuditLog{})
}

// 审计必须落到结构化日志里：库不可用时，日志是唯一的事实来源。
func TestAuditorLogsStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, nil))
	a := NewAuditor(nil, lg)
	defer a.Close()

	a.Audit(context.Background(), store.AuditLog{
		Action: "approval.resolve", TargetType: "run", TargetID: "r1",
		TenantID: "t1", UserID: "u1", ActorEmail: "a@b.c", IP: "10.0.0.1",
	})

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("审计日志不是合法 JSON: %v (%q)", err, buf.String())
	}
	if line["action"] != "approval.resolve" || line["tenant"] != "t1" {
		t.Fatalf("审计日志字段不对: %v", line)
	}
}

// /metrics 至少要有 run 终态这条——验收点，也是最能说明「指标接上了」的一条。
func TestHandlerExposesRunMetrics(t *testing.T) {
	h := Handler()
	if h == nil {
		t.Fatal("Handler() 返回 nil，指标被意外关闭")
	}

	// 先走一次带指标的请求，让 http_* 两条指标有 label 组合（它们无从预热）。
	MetricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	RecordRunFinished(StatusCompleted, 3*time.Second)
	RecordRunFinished(StatusFailed, 100*time.Millisecond)
	RecordApproval(DecisionDeny)
	RecordLLMUsage("deepseek-chat", 100, 200, 42)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics 状态码 = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"astra_runs_total",
		"astra_run_duration_seconds",
		"astra_active_sessions",
		"astra_active_sandboxes",
		"astra_approvals_total",
		"astra_llm_tokens_total",
		"astra_llm_cost_micro_usd_total",
		"astra_http_requests_total",
		"astra_http_request_duration_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics 输出缺少 %q", want)
		}
	}
	if !strings.Contains(body, `astra_runs_total{status="completed"} 1`) {
		t.Errorf("astra_runs_total 没记到 completed: %s", grep(body, "astra_runs_total"))
	}
}

// 中间件要用路由模板做 label，而不是真实路径——否则每个会话 id 一条时间序列。
func TestMetricsMiddlewareUsesRoutePattern(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sessions/{id}/runs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	h := MetricsMiddleware(mux)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/realsessionid/runs", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	want := `astra_http_requests_total{method="GET",path="/v1/sessions/:id/runs",status="201"}`
	if !strings.Contains(body, want) {
		t.Fatalf("缺少 %s\n%s", want, grep(body, "astra_http_requests_total"))
	}
	if strings.Contains(body, "realsessionid") {
		t.Fatal("真实 id 泄漏进了指标 label")
	}
}

// 包一层指标不能把 SSE 打坏：handler 会断言 w.(http.Flusher)。
func TestMetricsMiddlewareKeepsFlusher(t *testing.T) {
	var sawFlusher bool
	h := MetricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, sawFlusher = w.(http.Flusher)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/runs/x/events", nil))
	if !sawFlusher {
		t.Fatal("ResponseWriter 丢失了 http.Flusher，SSE 会被攒着不发")
	}
}

func TestSetEnabled(t *testing.T) {
	defer SetEnabled(true)
	SetEnabled(false)
	if Handler() != nil {
		t.Fatal("关闭后 Handler() 应返回 nil（让 api 回 501）")
	}
	// 关闭时埋点函数也不能 panic。
	RecordRunFinished(StatusCompleted, time.Second)
	SetEnabled(true)
	if Handler() == nil {
		t.Fatal("重新开启后 Handler() 不该是 nil")
	}
}

// grep 只取含关键字的行，失败信息才看得懂。
func grep(body, kw string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, kw) && !strings.HasPrefix(line, "#") {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}
