// Langfuse 观测出口：直接走 Ingestion HTTP API，不引官方 SDK。
//
// 为什么不用 SDK：SDK 会把「传输层」和「语义层」捆在一起，我们只需要六个
// 事件类型和一条批量上报；为这点功能多一个第三方依赖（以及它的传递依赖）
// 不值得，也和项目「源码可审计、依赖最小」的原则冲突。
//
// 一条 run 在 Langfuse 里的形状：
//
//	trace  (id = runID, name = agent:<label>)   ← 一次任务
//	└ span (id = <runID>-root)                  ← run 的时间跨度（end_time 挂这里）
//	└ generation (id = <runID>-turn-<n>)        ← 一轮对话
//	└ span (id = <runID>-tool-<toolCallID>)     ← 一次工具调用（带审批决策）
//
// 上报语义是「尽力而为」：Langfuse 挂了不能拖慢或拖垮平台。所以事件先入内存
// 队列，后台 goroutine 批量发出；失败重试有限次后丢弃并记日志——观测数据
// 丢一条的代价，远小于把一次任务卡在埋点上。
package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// 上报参数。都是常量而不是配置项：这几个值调大了只会让「观测」变成负担，
// 调小了只是多发几个请求，没有哪种场景需要运维去拨它。
const (
	ingestPath = "/api/public/ingestion"
	tracesPath = "/api/public/traces"

	// flushInterval 是攒批的最长等待。2s 是「用户点开 trace 时数据基本已经在」
	// 和「一次长任务不会在内存里憋几百条」之间的折中。
	flushInterval = 2 * time.Second
	// batchSize 是单次 POST 的事件条数上限。
	batchSize = 20
	// maxQueue 是内存队列上限，超了丢最旧的（见 enqueue）。
	maxQueue = 2000
	// 失败重试：1 次首发 + 3 次重试，退避 100ms/200ms/400ms。
	maxAttempts = 4
	// requestTimeout 是单次 HTTP 的上限，避免慢端点把 worker 拖死。
	requestTimeout = 8 * time.Second
	// maxFieldLen 是单条 input/output 的截断长度。观测是为了定位问题，
	// 不是为了存全文；超长 prompt 会把一批请求撑到几百 KB。
	maxFieldLen = 8 << 10
	// maxRunOutput 是单个 run 累积输出的上限（流式增量拼接用）。
	maxRunOutput = 64 << 10
)

// traceLog 只在「重试也失败」时写一行。故意不走 app 的 logger：
// Config 里没有 logger 字段（接口已冻结），而观测失败不值得为它改契约。
var traceLog = log.New(os.Stderr, "trace: ", log.LstdFlags)

// RunSource 补全事件流里没有的 run 元信息。
//
// store.Event 只带 RunID/Type/Payload——租户、用户、用户指令都不在里面。
// 所以事件桥需要一个回查入口；store.Store 天然满足这个接口。
type RunSource interface {
	GetRun(ctx context.Context, id string) (store.Run, error)
	GetSession(ctx context.Context, id string) (store.Session, error)
}

// Langfuse 是 Tracer 的 HTTP 实现。
type Langfuse struct {
	endpoint   string
	publicKey  string
	secretKey  string
	dashboard  string // trace 详情页前缀，默认同 endpoint
	agentLabel string
	rate       float64
	client     *http.Client

	// ── 队列（worker 独占消费）──
	mu      sync.Mutex
	queue   []ingestEvent
	dropped uint64
	seq     uint64
	outputs map[string]string // runID → 最近一轮输出，供 RunEnded 写 trace.output

	// ── 采样 ──
	rndMu sync.Mutex
	rnd   *rand.Rand
	// sampled 缓存「这个 run 抽中了吗」，避免同一 run 的不同事件各掷一次骰子。
	sampleMu sync.Mutex
	sampled  map[string]bool

	// ── Attach 的 run 状态机 ──
	src   RunSource
	runMu sync.Mutex
	runs  map[string]*runTrace

	bridgeDropped atomic.Uint64

	wake      chan struct{}
	flushReq  chan chan error
	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// ── 装配 ──────────────────────────────────────────────────────

func openLangfuse(cfg Config) (Tracer, error) {
	ep := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if ep == "" {
		return nil, fmt.Errorf("trace: Langfuse endpoint 为空")
	}
	u, err := url.Parse(ep)
	if err != nil {
		return nil, fmt.Errorf("trace: LANGFUSE_HOST 非法 %q: %w", ep, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("trace: LANGFUSE_HOST 需要 http:// 或 https:// 前缀，收到 %q", ep)
	}
	if cfg.PublicKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("trace: Langfuse 缺少凭据（LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY 都不能为空）")
	}

	rate := cfg.SampleRate
	switch {
	case rate > 1:
		rate = 1
	case rate < 0:
		rate = 0
	}

	dashboard := strings.TrimRight(strings.TrimSpace(os.Getenv("LANGFUSE_DASHBOARD_URL")), "/")
	if dashboard == "" {
		dashboard = ep
	}
	label := strings.TrimSpace(os.Getenv("ASTRA_AGENT_NAME"))
	if label == "" {
		label = "astra"
	}

	l := &Langfuse{
		endpoint:   ep,
		publicKey:  cfg.PublicKey,
		secretKey:  cfg.SecretKey,
		dashboard:  dashboard,
		agentLabel: label,
		rate:       rate,
		client: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		outputs:  map[string]string{},
		rnd:      rand.New(rand.NewSource(time.Now().UnixNano())),
		sampled:  map[string]bool{},
		runs:     map[string]*runTrace{},
		wake:     make(chan struct{}, 1),
		flushReq: make(chan chan error, 1),
		stopCh:   make(chan struct{}),
	}
	l.wg.Add(1)
	go l.loop()
	return l, nil
}

func (l *Langfuse) Enabled() bool { return l != nil }

// ── 事件映射 ──────────────────────────────────────────────────

func (l *Langfuse) RunStarted(ctx context.Context, r Run) {
	if !l.Enabled() || r.RunID == "" || !l.shouldSample(r.RunID) {
		return
	}
	started := r.StartedAt
	if started.IsZero() {
		started = time.Now().UTC()
	}
	ts := rfc3339(started)
	label := r.Agent
	if label == "" {
		label = l.agentLabel
	}

	l.emit(r.RunID, "trace-create", traceBody{
		ID:        r.RunID,
		Name:      "agent:" + label,
		Timestamp: ts,
		Input:     clip(r.Input),
		SessionID: r.SessionID,
		UserID:    r.UserID,
		Metadata: map[string]any{
			"tenant":   r.TenantID,
			"user":     r.UserID,
			"session":  r.SessionID,
			"provider": r.Provider,
			"run_id":   r.RunID,
		},
	})
	// 根 span 只为拿一个准确的 end_time：Langfuse 的 trace 本身没有结束时间，
	// 时长是它的子 observation 的并集。没有这个 span，run 详情页的耗时会显示为空。
	l.emit(r.RunID, "span-create", spanBody{
		ID:        rootSpanID(r.RunID),
		TraceID:   r.RunID,
		Name:      "run",
		StartTime: ts,
		Input:     clip(r.Input),
	})
}

func (l *Langfuse) RunEnded(ctx context.Context, runID, status, stopReason string) {
	if !l.Enabled() || runID == "" || !l.shouldSample(runID) {
		return
	}
	now := time.Now().UTC()
	md := map[string]any{"status": status}
	if stopReason != "" {
		md["stopReason"] = stopReason
	}
	// 一次取出、两处使用：takeOutput 会删掉这条记录，分两次读第二次必然是空的。
	out := clip(l.takeOutput(runID))

	l.emit(runID, "span-update", spanUpdateBody{
		ID:        rootSpanID(runID),
		TraceID:   runID,
		EndTime:   rfc3339(now),
		Output:    out,
		Metadata:  md,
		Level:     levelOf(status),
		StatusMsg: "",
	})
	// trace-create 是 upsert：同 id 再发一次就是「补字段」，这是 Langfuse 的
	// 既有语义（没有独立的 trace-update 类型）。
	l.emit(runID, "trace-create", traceBody{
		ID:       runID,
		Output:   out,
		Metadata: md,
	})
}

func (l *Langfuse) TurnRecorded(ctx context.Context, t Turn) {
	if !l.Enabled() || t.RunID == "" || !l.shouldSample(t.RunID) {
		return
	}
	start := t.StartedAt
	if start.IsZero() {
		start = time.Now().UTC()
	}
	end := t.EndedAt
	if end.IsZero() {
		end = time.Now().UTC()
	}
	l.rememberOutput(t.RunID, t.Output)

	l.emit(t.RunID, "generation-create", generationBody{
		ID:        fmt.Sprintf("%s-turn-%d", t.RunID, t.TurnIndex),
		TraceID:   t.RunID,
		Name:      "turn",
		StartTime: rfc3339(start),
		EndTime:   rfc3339(end),
		Input:     clip(t.Input),
		Output:    clip(t.Output),
		Metadata:  map[string]any{"index": t.TurnIndex, "run_id": t.RunID},
	})
}

func (l *Langfuse) ToolCallRecorded(ctx context.Context, tc ToolCall) {
	if !l.Enabled() || tc.RunID == "" || tc.ToolCallID == "" || !l.shouldSample(tc.RunID) {
		return
	}
	start := tc.StartedAt
	if start.IsZero() {
		start = time.Now().UTC()
	}
	end := tc.EndedAt
	if end.IsZero() {
		end = start
	}
	md := map[string]any{"run_id": tc.RunID}
	if d := normalizeDecision(tc.Decision); d != "" {
		md["decision"] = d
	}
	name := tc.Name
	if name == "" {
		name = "tool"
	}
	l.emit(tc.RunID, "span-create", spanBody{
		ID:        fmt.Sprintf("%s-tool-%s", tc.RunID, tc.ToolCallID),
		TraceID:   tc.RunID,
		Name:      name,
		StartTime: rfc3339(start),
		EndTime:   rfc3339(end),
		Input:     clip(tc.Input),
		Output:    clip(tc.Output),
		Metadata:  md,
	})
}

// ── 查询 ──────────────────────────────────────────────────────

func (l *Langfuse) QueryTraces(ctx context.Context, f Filter) ([]Summary, error) {
	if !l.Enabled() {
		return nil, ErrDisabled
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if f.RunID != "" {
		var t apiTrace
		status, err := l.getJSON(ctx, l.endpoint+tracesPath+"/"+url.PathEscape(f.RunID), &t)
		if err != nil {
			return nil, err
		}
		if status == http.StatusNotFound {
			// Langfuse 还没收到这条 trace（可能是刚发生的 run）——空列表比 500 诚实。
			return []Summary{}, nil
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("trace: 查询 Langfuse trace %s 失败: HTTP %d", f.RunID, status)
		}
		return []Summary{t.toSummary(l.dashboard)}, nil
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	var list struct {
		Data []apiTrace `json:"data"`
	}
	status, err := l.getJSON(ctx, fmt.Sprintf("%s%s?limit=%d", l.endpoint, tracesPath, limit), &list)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("trace: 查询 Langfuse traces 失败: HTTP %d", status)
	}
	out := make([]Summary, 0, len(list.Data))
	for _, t := range list.Data {
		out = append(out, t.toSummary(l.dashboard))
	}
	return out, nil
}

// apiTrace 是 Langfuse trace 响应里我们用得到的字段。
// 刻意只映射子集：上游字段多且会变，多解析一个就多一处会烂的耦合。
type apiTrace struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Timestamp string          `json:"timestamp"`
	Latency   *float64        `json:"latency"` // 秒
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output"`
	Metadata  map[string]any  `json:"metadata"`
}

func (t apiTrace) toSummary(dashboard string) Summary {
	s := Summary{ID: t.ID, Name: t.Name, RunID: t.ID, Input: rawText(t.Input, 300)}
	if t.Metadata != nil {
		if v, ok := t.Metadata["status"].(string); ok {
			s.Status = v
		}
		if v, ok := t.Metadata["run_id"].(string); ok && v != "" {
			s.RunID = v
		}
	}
	if t.Latency != nil {
		s.LatencyMS = int64(*t.Latency * 1000)
	}
	if ts, err := time.Parse(time.RFC3339Nano, t.Timestamp); err == nil {
		s.StartedAt = ts
	}
	if dashboard != "" && t.ID != "" {
		s.URL = dashboard + "/trace/" + t.ID
	}
	return s
}

func (l *Langfuse) getJSON(ctx context.Context, url string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("trace: 构造查询请求: %w", err)
	}
	req.SetBasicAuth(l.publicKey, l.secretKey)
	req.Header.Set("Accept", "application/json")

	resp, err := l.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("trace: 查询 Langfuse 失败: %w", err)
	}
	defer resp.Body.Close()
	// 限制读取体积：响应异常时不要把整个 body 读进内存。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("trace: 读取 Langfuse 响应: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return resp.StatusCode, fmt.Errorf("trace: 解析 Langfuse 响应: %w", err)
	}
	return resp.StatusCode, nil
}

// ── 批量上报 ──────────────────────────────────────────────────

type ingestEvent struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Body      any    `json:"body"`
}

// 三种 body 的字段是 Langfuse ingestion 的公开契约。
// 故意不放 endTime 到 trace body：trace 没有这个字段，多塞未定义字段
// 在严格校验的版本上会让整批 400，而整批失败等于这一批 trace 全丢。
type traceBody struct {
	ID        string         `json:"id"`
	Name      string         `json:"name,omitempty"`
	Timestamp string         `json:"timestamp,omitempty"`
	Input     string         `json:"input,omitempty"`
	Output    string         `json:"output,omitempty"`
	SessionID string         `json:"sessionId,omitempty"`
	UserID    string         `json:"userId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type spanBody struct {
	ID        string         `json:"id"`
	TraceID   string         `json:"traceId,omitempty"`
	Name      string         `json:"name,omitempty"`
	StartTime string         `json:"startTime,omitempty"`
	EndTime   string         `json:"endTime,omitempty"`
	Input     string         `json:"input,omitempty"`
	Output    string         `json:"output,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type spanUpdateBody struct {
	ID        string         `json:"id"`
	TraceID   string         `json:"traceId,omitempty"`
	EndTime   string         `json:"endTime,omitempty"`
	Output    string         `json:"output,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Level     string         `json:"level,omitempty"`
	StatusMsg string         `json:"statusMessage,omitempty"`
}

type generationBody struct {
	ID        string         `json:"id"`
	TraceID   string         `json:"traceId,omitempty"`
	Name      string         `json:"name,omitempty"`
	StartTime string         `json:"startTime,omitempty"`
	EndTime   string         `json:"endTime,omitempty"`
	Input     string         `json:"input,omitempty"`
	Output    string         `json:"output,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

func (l *Langfuse) emit(runID, typ string, body any) {
	l.enqueue(ingestEvent{
		ID:        l.nextEventID(runID),
		Type:      typ,
		Timestamp: rfc3339(time.Now().UTC()),
		Body:      body,
	})
}

func (l *Langfuse) nextEventID(runID string) string {
	l.mu.Lock()
	l.seq++
	n := l.seq
	l.mu.Unlock()
	if runID == "" {
		runID = "trace"
	}
	// 事件 id 必须全局唯一：Langfuse 按 id 幂等去重，重试时重复投递才不会
	// 变成两条一样的 trace。runID 前缀是给人看的，序号才是唯一性来源。
	return runID + "-" + strconv.FormatUint(n, 10)
}

func (l *Langfuse) enqueue(ev ingestEvent) {
	l.mu.Lock()
	if len(l.queue) >= maxQueue {
		// 丢最旧：新事件更接近「现在发生了什么」，排障时更有价值。
		l.queue = l.queue[1:]
		l.dropped++
	}
	l.queue = append(l.queue, ev)
	n := len(l.queue)
	l.mu.Unlock()

	if n >= batchSize {
		select {
		case l.wake <- struct{}{}:
		default: // 已经有人叫醒 worker 了，不重复排队
		}
	}
}

func (l *Langfuse) loop() {
	defer l.wg.Done()
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	drain := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		return l.drain(ctx)
	}

	for {
		select {
		case <-l.stopCh:
			drain()
			return
		case <-ticker.C:
			drain()
		case <-l.wake:
			drain()
		case done := <-l.flushReq:
			done <- drain()
		}
	}
}

// drain 把队列按批次发完，返回最后一次错误。
// 只有 worker 调用它——并发安全靠「单一消费者」而不是锁。
func (l *Langfuse) drain(ctx context.Context) error {
	var lastErr error
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l.mu.Lock()
		if len(l.queue) == 0 {
			l.queue = nil
			l.mu.Unlock()
			return lastErr
		}
		n := len(l.queue)
		if n > batchSize {
			n = batchSize
		}
		batch := make([]ingestEvent, n)
		copy(batch, l.queue[:n])
		l.queue = l.queue[n:]
		l.mu.Unlock()

		if err := l.send(ctx, batch); err != nil {
			lastErr = err
		}
	}
}

func (l *Langfuse) send(ctx context.Context, batch []ingestEvent) error {
	backoff := 100 * time.Millisecond
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = l.post(ctx, batch)
		if err == nil {
			return nil
		}
		if attempt == maxAttempts {
			break
		}
		select {
		case <-time.After(backoff):
		case <-l.stopCh:
			return fmt.Errorf("trace: 进程退出，放弃上报 %d 条: %w", len(batch), err)
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2
	}
	// 最终失败只记日志：观测数据的价值不足以阻塞业务，也不值得重试到天荒地老。
	traceLog.Printf("上报 %d 条事件失败（已丢弃）: %v", len(batch), err)
	return err
}

func (l *Langfuse) post(ctx context.Context, batch []ingestEvent) error {
	payload, err := json.Marshal(map[string]any{"batch": batch})
	if err != nil {
		return fmt.Errorf("trace: 序列化 batch: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.endpoint+ingestPath, strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("trace: 构造上报请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(l.publicKey, l.secretKey)

	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("trace: 上报 Langfuse: %w", err)
	}
	defer resp.Body.Close()
	// Langfuse 会返回 207（部分成功）并在 body 里报每条的错误。2xx 一律当成功：
	// 逐条解析只会让「一条格式不对」变成「整批重试」。
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("trace: Langfuse HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func (l *Langfuse) Flush(ctx context.Context) error {
	if !l.Enabled() {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan error, 1)
	select {
	case l.flushReq <- done:
	case <-l.stopCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Langfuse) Close() error {
	if !l.Enabled() {
		return nil
	}
	l.closeOnce.Do(func() { close(l.stopCh) })
	l.wg.Wait()
	l.client.CloseIdleConnections()

	l.mu.Lock()
	dropped := l.dropped
	l.mu.Unlock()
	if dropped > 0 {
		traceLog.Printf("队列满丢弃了 %d 条事件（上限 %d）", dropped, maxQueue)
	}
	return nil
}

// ── 采样 ──────────────────────────────────────────────────────

// shouldSample 按 run 抽样，同一 run 的所有事件共享一次骰子。
//
// rate<=0 表示不采样（一条都不发），rate>=1 表示全采样。注意：Config 的零值是 0，
// 装配层必须显式给 1——app 层用的是 envFloat(..., 1)，符合这个约定。
func (l *Langfuse) shouldSample(runID string) bool {
	if l.rate >= 1 {
		return true
	}
	if l.rate <= 0 {
		return false
	}
	l.sampleMu.Lock()
	defer l.sampleMu.Unlock()
	if v, ok := l.sampled[runID]; ok {
		return v
	}
	l.rndMu.Lock()
	hit := l.rnd.Float64() < l.rate
	l.rndMu.Unlock()
	l.sampled[runID] = hit
	return hit
}

func (l *Langfuse) forgetSample(runID string) {
	l.sampleMu.Lock()
	delete(l.sampled, runID)
	l.sampleMu.Unlock()
}

func (l *Langfuse) rememberOutput(runID, out string) {
	if out == "" {
		return
	}
	l.mu.Lock()
	if l.outputs == nil {
		l.outputs = map[string]string{}
	}
	l.outputs[runID] = clip(out)
	l.mu.Unlock()
}

func (l *Langfuse) takeOutput(runID string) string {
	l.mu.Lock()
	out := l.outputs[runID]
	delete(l.outputs, runID)
	l.mu.Unlock()
	return out
}

// ── 事件桥（Attach）─────────────────────────────────────────

// runTrace 是一个 run 在事件桥里的运行时状态。
type runTrace struct {
	info      Run
	skipped   bool // 没抽中采样：后续事件全部丢弃
	started   bool
	turns     int
	output    strings.Builder
	truncated bool
	tools     map[string]*ToolCall
	decisions map[string]string
}

// Attach 订阅 hub 的事件，把 run 生命周期转成 trace。
// 返回的 stop 函数会等桥 goroutine 收工并 Flush，进程退出前调用。
//
// src 是可选的回查入口（store.Store 即可）：事件流里没有租户、用户和用户指令，
// 不传也能跑，只是 trace 的 input/user 会是空的。
func (l *Langfuse) Attach(hub *eventbus.Hub, src ...RunSource) (stop func()) {
	if !l.Enabled() || hub == nil {
		return func() {}
	}
	if len(src) > 0 && src[0] != nil {
		l.src = src[0]
	}

	events := make(chan store.Event, 256)
	stopCh := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		l.runBridge(events, stopCh)
	}()

	// OnGlobal 的回调在 Publish 的调用方 goroutine 里同步执行，必须快：
	// 这里只做「过滤 + 非阻塞投递」，任何 IO（回查 store、拼 JSON、发 HTTP）
	// 都在 runBridge 里做。
	//
	// 已知限制：Hub 只保留一个全局回调（后注册覆盖先注册）。桌面端 main 里
	// 那处 OnGlobal（审批系统通知）如果在本函数之后注册，会把事件桥顶掉。
	// 服务端（cmd/astra-server）没有这个问题。
	hub.OnGlobal(func(ev store.Event) {
		select {
		case <-stopCh:
			return
		default:
		}

		// 增量文本频率极高（逐 token），不进 channel：直接累加到 run 状态里，
		// 避免把 512 深度的事件通道冲爆。加锁写一个 builder 是 O(len)。
		if ev.Type == store.EventMessageDelta {
			l.collectDelta(ev)
			return
		}
		if !isTraceEvent(ev.Type) {
			return
		}
		select {
		case events <- ev:
		default:
			l.bridgeDropped.Add(1)
		}
	})

	var once sync.Once
	return func() {
		once.Do(func() { close(stopCh) })
		select {
		case <-stopped:
		case <-time.After(requestTimeout):
		}
	}
}

// Attach 是给装配层用的一行入口：tracer 未启用时返回空操作。
//
//	if stop := trace.Attach(hub, tracer, st); stop != nil { defer stop() }
func Attach(hub *eventbus.Hub, t Tracer, src ...RunSource) func() {
	if l, ok := t.(*Langfuse); ok {
		return l.Attach(hub, src...)
	}
	return func() {}
}

func isTraceEvent(t string) bool {
	switch t {
	case store.EventState,
		store.EventToolStart,
		store.EventToolUpdate,
		store.EventApprovalReq,
		store.EventApprovalResult,
		store.EventError:
		return true
	}
	return false
}

func (l *Langfuse) collectDelta(ev store.Event) {
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil || p.Text == "" {
		return
	}
	l.runMu.Lock()
	defer l.runMu.Unlock()
	st := l.runStateLocked(ev.RunID)
	if st.skipped || st.truncated {
		return
	}
	if st.output.Len()+len(p.Text) > maxRunOutput {
		st.output.WriteString(p.Text[:max(0, maxRunOutput-st.output.Len())])
		st.truncated = true
		return
	}
	st.output.WriteString(p.Text)
}

func (l *Langfuse) runBridge(events <-chan store.Event, stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			// 退出前把已入队的事件处理完：否则最后一条 run_state 可能还在路上，
			// trace 就永远停在 running。
			for {
				select {
				case ev := <-events:
					l.handleEvent(ev)
				default:
					ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
					l.Flush(ctx)
					cancel()
					return
				}
			}
		case ev := <-events:
			l.handleEvent(ev)
		}
	}
}

func (l *Langfuse) handleEvent(ev store.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	switch ev.Type {
	case store.EventState:
		var p struct {
			Status     string `json:"status"`
			StopReason string `json:"stopReason"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil || p.Status == "" {
			return
		}
		l.onState(ctx, ev, p.Status, p.StopReason)

	case store.EventToolStart:
		var p struct {
			ToolCallID string          `json:"toolCallId"`
			Title      string          `json:"title"`
			Kind       string          `json:"kind"`
			RawInput   json.RawMessage `json:"rawInput"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil || p.ToolCallID == "" {
			return
		}
		st := l.withRun(ctx, ev)
		if st == nil {
			return
		}
		st.tools[p.ToolCallID] = &ToolCall{
			RunID:      ev.RunID,
			ToolCallID: p.ToolCallID,
			Name:       firstNonEmpty(p.Title, p.Kind, "tool"),
			Input:      rawText(p.RawInput, maxFieldLen),
			StartedAt:  ev.CreatedAt,
			Decision:   st.decisions[p.ToolCallID],
		}

	case store.EventToolUpdate:
		var p struct {
			ToolCallID string          `json:"toolCallId"`
			Status     string          `json:"status"`
			Title      string          `json:"title"`
			Kind       string          `json:"kind"`
			RawOutput  json.RawMessage `json:"rawOutput"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil || p.ToolCallID == "" {
			return
		}
		l.runMu.Lock()
		st := l.runs[ev.RunID]
		var tc *ToolCall
		if st != nil {
			tc = st.tools[p.ToolCallID]
		}
		l.runMu.Unlock()
		if tc == nil {
			return
		}
		if out := rawText(p.RawOutput, maxFieldLen); out != "" {
			tc.Output = out
		}
		if p.Title != "" {
			tc.Name = p.Title
		} else if tc.Name == "" && p.Kind != "" {
			tc.Name = p.Kind
		}
		if isToolTerminal(p.Status) {
			tc.EndedAt = ev.CreatedAt
			tc.Decision = firstNonEmpty(tc.Decision, l.decisionOf(ev.RunID, p.ToolCallID))
			l.ToolCallRecorded(ctx, *tc)
			l.runMu.Lock()
			if st := l.runs[ev.RunID]; st != nil && st.tools != nil {
				delete(st.tools, p.ToolCallID)
			}
			l.runMu.Unlock()
		}

	case store.EventApprovalReq:
		var p struct {
			ToolCallID string `json:"toolCallId"`
			Decision   string `json:"decision"`
			Pending    bool   `json:"pending"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil || p.ToolCallID == "" {
			return
		}
		if p.Pending || p.Decision == "" || p.Decision == "pending" {
			return
		}
		l.setDecision(ev.RunID, p.ToolCallID, p.Decision)

	case store.EventApprovalResult:
		var p struct {
			ToolCallID string `json:"toolCallId"`
			Decision   string `json:"decision"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil || p.ToolCallID == "" {
			return
		}
		l.setDecision(ev.RunID, p.ToolCallID, p.Decision)

	case store.EventError:
		var p struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil || p.Message == "" {
			return
		}
		// 错误文本进 trace 的 output：失败任务最需要的就是「为什么失败」，
		// 只留一个 status=failed 等于把排障成本推给下一个人。
		l.rememberOutput(ev.RunID, "error: "+p.Message)
	}
}

func (l *Langfuse) onState(ctx context.Context, ev store.Event, status, stopReason string) {
	switch {
	case status == store.RunRunning:
		l.withRun(ctx, ev)
	case store.IsTerminal(status):
		st := l.withRun(ctx, ev)
		if st == nil {
			return
		}
		l.finishRun(ctx, st, ev.RunID, status, stopReason, ev.CreatedAt)
	}
}

// withRun 取（必要时创建并发出 trace-create）一个 run 的状态。
func (l *Langfuse) withRun(ctx context.Context, ev store.Event) *runTrace {
	l.runMu.Lock()
	st := l.runStateLocked(ev.RunID)
	needStart := !st.started && !st.skipped
	if needStart {
		st.started = true
	}
	l.runMu.Unlock()

	if !needStart {
		if st.skipped {
			return nil
		}
		return st
	}

	// 采样在锁外做（内部另有锁），避免和 runMu 形成嵌套。
	if !l.shouldSample(ev.RunID) {
		l.runMu.Lock()
		st.skipped = true
		l.runMu.Unlock()
		return nil
	}

	info := l.enrich(ctx, ev.RunID, ev.CreatedAt)
	l.runMu.Lock()
	st.info = info
	l.runMu.Unlock()

	l.RunStarted(ctx, info)
	return st
}

func (l *Langfuse) finishRun(ctx context.Context, st *runTrace, runID, status, stopReason string, at time.Time) {
	if st.skipped {
		l.dropRun(runID)
		return
	}

	// 1. 还没收到终态的 tool call 也要补齐：span 少了 end_time，详情页会显示
	//    一个永远在跑的工具，比缺这条 span 更误导。
	l.runMu.Lock()
	rest := make([]ToolCall, 0, len(st.tools))
	for _, tc := range st.tools {
		if tc.EndedAt.IsZero() {
			tc.EndedAt = at
		}
		rest = append(rest, *tc)
	}
	st.tools = map[string]*ToolCall{}
	out := st.output.String()
	info := st.info
	turnIdx := st.turns
	st.turns++
	l.runMu.Unlock()

	for _, tc := range rest {
		l.ToolCallRecorded(ctx, tc)
	}

	// 2. 一轮对话 = 一次 prompt。平台里每个 run 就是一次 prompt，所以
	//    turn 与 run 一一对应；index 留 0，将来支持多轮时不用改 id 规则。
	if out != "" || info.Input != "" {
		l.TurnRecorded(ctx, Turn{
			RunID:     runID,
			TurnIndex: turnIdx,
			Input:     info.Input,
			Output:    out,
			StartedAt: info.StartedAt,
			EndedAt:   at,
		})
	}

	// 3. 收口 trace。
	l.RunEnded(ctx, runID, status, stopReason)
	l.dropRun(runID)
	l.forgetSample(runID)
}

func (l *Langfuse) dropRun(runID string) {
	l.runMu.Lock()
	delete(l.runs, runID)
	l.runMu.Unlock()
}

func (l *Langfuse) runStateLocked(runID string) *runTrace {
	if l.runs == nil {
		l.runs = map[string]*runTrace{}
	}
	st, ok := l.runs[runID]
	if !ok {
		st = &runTrace{
			info:      Run{RunID: runID},
			tools:     map[string]*ToolCall{},
			decisions: map[string]string{},
		}
		l.runs[runID] = st
	}
	if st.tools == nil {
		st.tools = map[string]*ToolCall{}
	}
	if st.decisions == nil {
		st.decisions = map[string]string{}
	}
	return st
}

func (l *Langfuse) setDecision(runID, toolCallID, decision string) {
	d := normalizeDecision(decision)
	if d == "" {
		return
	}
	l.runMu.Lock()
	defer l.runMu.Unlock()
	st := l.runStateLocked(runID)
	st.decisions[toolCallID] = d
	if tc, ok := st.tools[toolCallID]; ok {
		tc.Decision = d
	}
}

func (l *Langfuse) decisionOf(runID, toolCallID string) string {
	l.runMu.Lock()
	defer l.runMu.Unlock()
	if st, ok := l.runs[runID]; ok {
		return st.decisions[toolCallID]
	}
	return ""
}

// enrich 用 RunSource 补齐事件里没有的字段。失败就退化成「只有 runID 的 trace」——
// 观测降级不能影响主流程。
func (l *Langfuse) enrich(ctx context.Context, runID string, at time.Time) Run {
	r := Run{RunID: runID, StartedAt: at}
	if l.src != nil {
		if rr, err := l.src.GetRun(ctx, runID); err == nil {
			r.Input = rr.Input
			r.SessionID = rr.SessionID
			if !rr.StartedAt.IsZero() {
				r.StartedAt = rr.StartedAt
			}
		}
		if r.SessionID != "" {
			if s, err := l.src.GetSession(ctx, r.SessionID); err == nil {
				r.TenantID = s.TenantID
				r.UserID = s.UserID
			}
		}
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now().UTC()
	}
	if r.Agent == "" {
		r.Agent = l.agentLabel
	}
	return r
}

// ── 小工具 ────────────────────────────────────────────────────

func rootSpanID(runID string) string { return runID + "-root" }

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func clip(s string) string {
	if len(s) <= maxFieldLen {
		return s
	}
	// 按 rune 边界截断，避免把 UTF-8 切坏导致 Langfuse 解析出乱码。
	cut := maxFieldLen
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…(truncated)"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// normalizeDecision 把 ACP 的选项 kind（allow_once / reject_always …）和
// runner 的 decision（allow/deny/timeout）统一成 allow / deny。
func normalizeDecision(d string) string {
	switch {
	case d == "" || d == "pending":
		return ""
	case d == "allow" || strings.HasPrefix(d, "allow"):
		return "allow"
	case d == "deny" || d == "timeout" || strings.HasPrefix(d, "reject") || strings.HasPrefix(d, "deny"):
		return "deny"
	}
	return d
}

func isToolTerminal(status string) bool {
	return status == "completed" || status == "failed"
}

func levelOf(status string) string {
	switch status {
	case store.RunCompleted:
		return "DEFAULT"
	case store.RunFailed:
		return "ERROR"
	case store.RunCancelled:
		return "WARNING"
	}
	return "DEFAULT"
}

// rawText 把 rawInput/rawOutput 这类「可能是字符串也可能是对象」的字段
// 转成适合放进 trace 的文本。
func rawText(raw json.RawMessage, limit int) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return clipN(s, limit)
	}
	return clipN(string(raw), limit)
}

func clipN(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
