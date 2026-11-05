package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/WangYulong666/astra-agent/internal/store"
	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

// fakeAPI 只实现本包用到的五个方法，用来断言「传给 Milvus 的 expr 是什么」。
// 这是本模块最重要的测试点：expr 泄漏 = 跨用户读数据，且不能靠人眼看代码保证。
type fakeAPI struct {
	insertPart  string
	insertCols  []entity.Column
	insertCalls int

	searchExpr   string
	searchOut    []string
	searchTopK   int
	searchVecLen int
	searchRes    []client.SearchResult

	queryExpr   string
	queryResult client.ResultSet
	queryErr    error

	deletePks   []string
	deleteCalls int

	closed bool
}

func (f *fakeAPI) Insert(_ context.Context, _ string, part string, cols ...entity.Column) (entity.Column, error) {
	f.insertCalls++
	f.insertPart = part
	f.insertCols = cols
	return entity.NewColumnVarChar(fieldID, []string{"pk"}), nil
}

func (f *fakeAPI) Search(_ context.Context, _ string, _ []string, expr string, out []string,
	vecs []entity.Vector, _ string, _ entity.MetricType, topK int,
	_ entity.SearchParam, _ ...client.SearchQueryOptionFunc) ([]client.SearchResult, error) {
	f.searchExpr = expr
	f.searchOut = out
	f.searchTopK = topK
	if len(vecs) > 0 {
		f.searchVecLen = vecs[0].Dim()
	}
	return f.searchRes, nil
}

func (f *fakeAPI) Query(_ context.Context, _ string, _ []string, expr string, _ []string,
	_ ...client.SearchQueryOptionFunc) (client.ResultSet, error) {
	f.queryExpr = expr
	return f.queryResult, f.queryErr
}

func (f *fakeAPI) DeleteByPks(_ context.Context, _ string, _ string, ids entity.Column) error {
	f.deleteCalls++
	for i := 0; i < ids.Len(); i++ {
		if v, err := ids.Get(i); err == nil {
			if s, ok := v.(string); ok {
				f.deletePks = append(f.deletePks, s)
			}
		}
	}
	return nil
}

func (f *fakeAPI) Close() error { f.closed = true; return nil }

func newTestStore(api milvusAPI, dim int) *milvusStore {
	return &milvusStore{api: api, coll: "astra_memory", dim: dim, emb: newHashEmbedder(dim)}
}

// ── 检索表达式 ──────────────────────────────────────────────

func TestSearchExprScopedToTenantAndUser(t *testing.T) {
	api := &fakeAPI{
		searchRes: []client.SearchResult{{
			IDs:    entity.NewColumnVarChar(fieldID, []string{"m1"}),
			Fields: client.ResultSet{entity.NewColumnVarChar(fieldText, []string{"hello"})},
			Scores: []float32{0.5},
		}},
	}
	s := newTestStore(api, 8)

	items, err := s.Search(context.Background(), Query{
		TenantID: "tenant-a",
		UserID:   "user-1",
		Text:     "上次那个方案",
		K:        5,
		Kind:     KindFact,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := `tenant_id == "tenant-a" and user_id == "user-1" and kind == "fact"`
	if api.searchExpr != want {
		t.Fatalf("expr = %s\nwant = %s", api.searchExpr, want)
	}
	if api.searchTopK != 5 {
		t.Fatalf("topK = %d, want 5", api.searchTopK)
	}
	if api.searchVecLen != 8 {
		t.Fatalf("查询向量维度 = %d, want 8", api.searchVecLen)
	}
	for _, f := range []string{fieldText, fieldKind, fieldSessionID, fieldCreatedAt, fieldMeta, fieldUserID, fieldTenantID} {
		if !contains(api.searchOut, f) {
			t.Fatalf("output_fields 缺少 %s：%v", f, api.searchOut)
		}
	}
	if len(items) != 1 || items[0].ID != "m1" || items[0].Text != "hello" {
		t.Fatalf("结果解析错误: %+v", items)
	}
}

func TestSearchExprWithoutKind(t *testing.T) {
	api := &fakeAPI{}
	s := newTestStore(api, 4)
	if _, err := s.Search(context.Background(), Query{TenantID: "t", UserID: "u", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	want := `tenant_id == "t" and user_id == "u"`
	if api.searchExpr != want {
		t.Fatalf("expr = %s, want %s", api.searchExpr, want)
	}
}

func TestSearchRejectsMissingOwner(t *testing.T) {
	api := &fakeAPI{}
	s := newTestStore(api, 4)
	for _, q := range []Query{
		{UserID: "u", Text: "x"},               // 缺 tenant
		{TenantID: "t", Text: "x"},             // 缺 user
		{TenantID: "t", UserID: "u", Text: ""}, // 缺查询文本
	} {
		if _, err := s.Search(context.Background(), q); err == nil {
			t.Fatalf("缺字段的查询应当报错: %+v", q)
		}
	}
	if api.searchExpr != "" {
		t.Fatalf("校验失败时不应调用 Milvus，实际 expr = %q", api.searchExpr)
	}
}

// 最关键的一条：过滤值里出现双引号时，必须拒绝而不是「尽力转义」。
// 一旦透传，攻击者能拼出 user_id == "x" or user_id != "" 读走别人的记忆。
func TestSearchRejectsExprInjection(t *testing.T) {
	api := &fakeAPI{}
	s := newTestStore(api, 4)
	bad := []string{
		`t" or user_id != "`,
		`t\`,
		"t\n",
		`t" and kind == "fact`,
	}
	for _, v := range bad {
		if _, err := s.Search(context.Background(), Query{TenantID: v, UserID: "u", Text: "x"}); err == nil {
			t.Fatalf("tenant_id %q 应被拒绝", v)
		}
		if _, err := s.Search(context.Background(), Query{TenantID: "t", UserID: v, Text: "x"}); err == nil {
			t.Fatalf("user_id %q 应被拒绝", v)
		}
		if _, err := s.Search(context.Background(), Query{TenantID: "t", UserID: "u", Text: "x", Kind: v}); err == nil {
			t.Fatalf("kind %q 应被拒绝", v)
		}
	}
	if api.searchExpr != "" {
		t.Fatalf("注入尝试不应触达 Milvus，实际 expr = %q", api.searchExpr)
	}
}

// ── 删除的归属校验 ──────────────────────────────────────────

func TestDeleteRefusesForeignKey(t *testing.T) {
	api := &fakeAPI{queryResult: client.ResultSet{}}
	s := newTestStore(api, 4)

	err := s.Delete(context.Background(), "tenant-a", "user-1", "someone-elses-id")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if api.deleteCalls != 0 {
		t.Fatalf("未确认归属就调用了 DeleteByPks（%d 次）", api.deleteCalls)
	}
	want := `tenant_id == "tenant-a" and user_id == "user-1" and id == "someone-elses-id"`
	if api.queryExpr != want {
		t.Fatalf("校验 expr = %s, want %s", api.queryExpr, want)
	}
}

func TestDeleteRequiresOwnershipThenDeletes(t *testing.T) {
	api := &fakeAPI{queryResult: client.ResultSet{entity.NewColumnVarChar(fieldID, []string{"mine"})}}
	s := newTestStore(api, 4)

	if err := s.Delete(context.Background(), "t", "u", "mine"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if api.deleteCalls != 1 {
		t.Fatalf("DeleteByPks 调用 %d 次, want 1", api.deleteCalls)
	}
	if len(api.deletePks) != 1 || api.deletePks[0] != "mine" {
		t.Fatalf("删除的主键 = %v", api.deletePks)
	}
}

func TestDeleteRejectsInjection(t *testing.T) {
	api := &fakeAPI{}
	s := newTestStore(api, 4)
	if err := s.Delete(context.Background(), "t", "u", `x" or id != "`); err == nil {
		t.Fatal("非法 id 应被拒绝")
	}
	if api.deleteCalls != 0 || api.queryExpr != "" {
		t.Fatal("非法 id 不应触达 Milvus")
	}
}

// ── schema / 索引 / 写入 ────────────────────────────────────

func TestBuildSchemaFields(t *testing.T) {
	s := buildSchema("astra_memory", 768)
	if s.CollectionName != "astra_memory" {
		t.Fatalf("集合名 = %s", s.CollectionName)
	}
	if s.AutoID {
		t.Fatal("id 由我们生成，不能开 AutoID")
	}
	want := []struct {
		name string
		typ  entity.FieldType
	}{
		{fieldID, entity.FieldTypeVarChar},
		{fieldTenantID, entity.FieldTypeVarChar},
		{fieldUserID, entity.FieldTypeVarChar},
		{fieldSessionID, entity.FieldTypeVarChar},
		{fieldKind, entity.FieldTypeVarChar},
		{fieldText, entity.FieldTypeVarChar},
		{fieldVector, entity.FieldTypeFloatVector},
		{fieldCreatedAt, entity.FieldTypeInt64},
		{fieldMeta, entity.FieldTypeJSON},
	}
	if len(s.Fields) != len(want) {
		t.Fatalf("字段数 = %d, want %d", len(s.Fields), len(want))
	}
	for i, w := range want {
		f := s.Fields[i]
		if f.Name != w.name || f.DataType != w.typ {
			t.Fatalf("第 %d 个字段 = %s(%d), want %s(%d)", i, f.Name, f.DataType, w.name, w.typ)
		}
	}
	if s.PKFieldName() != fieldID {
		t.Fatalf("主键 = %s, want %s", s.PKFieldName(), fieldID)
	}
	// tenant_id 必须是 partition key：多租户物理隔离靠的是这个，不是靠代码自觉
	for _, f := range s.Fields {
		if f.Name == fieldTenantID && !f.IsPartitionKey {
			t.Fatal("tenant_id 未标记为 partition key")
		}
		if f.Name != fieldTenantID && f.IsPartitionKey {
			t.Fatalf("%s 不应是 partition key", f.Name)
		}
	}
	// 其余字段（除向量外）都不是 partition key，且只有一个
	if got, ok := schemaVectorDim(s); !ok || got != 768 {
		t.Fatalf("向量维度 = %d/%v, want 768/true", got, ok)
	}
	if s.Fields[2].TypeParams[entity.TypeParamMaxLength] != "64" {
		t.Fatalf("user_id max_length = %s", s.Fields[2].TypeParams[entity.TypeParamMaxLength])
	}
	if s.Fields[5].TypeParams[entity.TypeParamMaxLength] != "8192" {
		t.Fatalf("text max_length = %s", s.Fields[5].TypeParams[entity.TypeParamMaxLength])
	}
}

func TestBuildIndexSelection(t *testing.T) {
	idx, hnsw, err := buildIndex(768)
	if err != nil {
		t.Fatal(err)
	}
	if hnsw || idx.IndexType() != entity.IvfFlat {
		t.Fatalf("dim=768 应使用 IVF_FLAT，实际 %s", idx.IndexType())
	}
	if !strings.Contains(idx.Params()["params"], `"nlist":"1024"`) {
		t.Fatalf("nlist 参数 = %s, want nlist=1024", idx.Params()["params"])
	}
	if idx.Params()["metric_type"] != string(entity.L2) {
		t.Fatalf("metric = %s, want L2", idx.Params()["metric_type"])
	}

	idx, hnsw, err = buildIndex(1536)
	if err != nil {
		t.Fatal(err)
	}
	if !hnsw || idx.IndexType() != entity.HNSW {
		t.Fatalf("dim=1536 应使用 HNSW，实际 %s", idx.IndexType())
	}
	if p := idx.Params()["params"]; !strings.Contains(p, `"M":"16"`) || !strings.Contains(p, `"efConstruction":"200"`) {
		t.Fatalf("HNSW 参数 = %s", p)
	}
}

func TestIngestColumns(t *testing.T) {
	api := &fakeAPI{}
	s := newTestStore(api, 8)

	it, err := s.Ingest(context.Background(), Item{
		TenantID: "t1",
		UserID:   "u1",
		Kind:     KindFact,
		Text:     "预算上限是 30 万",
		Meta:     map[string]any{"src": "chat"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if it.ID == "" || len(it.ID) > idMaxLen {
		t.Fatalf("id = %q（长度 %d）", it.ID, len(it.ID))
	}
	if it.CreatedAt.IsZero() {
		t.Fatal("created_at 未回填")
	}
	if api.insertPart != "" {
		// partition key 集合由服务端按 tenant_id 哈希路由，手工指定分区名会被拒
		t.Fatalf("insert partition = %q, want 空（partition key 模式）", api.insertPart)
	}
	got := map[string]bool{}
	for _, c := range api.insertCols {
		got[c.Name()] = true
	}
	for _, name := range []string{fieldID, fieldTenantID, fieldUserID, fieldSessionID, fieldKind, fieldText, fieldVector, fieldCreatedAt, fieldMeta} {
		if !got[name] {
			t.Fatalf("insert 缺少列 %s", name)
		}
	}
	if v, err := api.insertCols[6].Get(0); err != nil {
		t.Fatal(err)
	} else if _, ok := v.([]float32); !ok {
		t.Fatalf("vector 列类型 = %T", v)
	}
}

func TestIngestRejectsEmptyTextAndOwner(t *testing.T) {
	s := newTestStore(&fakeAPI{}, 4)
	if _, err := s.Ingest(context.Background(), Item{TenantID: "t", UserID: "u", Text: "  "}); err == nil {
		t.Fatal("空文本应被拒绝")
	}
	if _, err := s.Ingest(context.Background(), Item{UserID: "u", Text: "x"}); err == nil {
		t.Fatal("缺 tenant_id 应被拒绝")
	}
}

func TestIngestTruncatesLongText(t *testing.T) {
	api := &fakeAPI{}
	s := newTestStore(api, 4)
	long := strings.Repeat("记", textMaxLen) // 3 字节/字，必然超上限
	if _, err := s.Ingest(context.Background(), Item{TenantID: "t", UserID: "u", Text: long}); err != nil {
		t.Fatal(err)
	}
	var stored string
	for _, c := range api.insertCols {
		if c.Name() == fieldText {
			v, _ := c.Get(0)
			stored, _ = v.(string)
		}
	}
	if len(stored) > textMaxLen {
		t.Fatalf("text 字节长度 %d 超过上限 %d", len(stored), textMaxLen)
	}
	if !utf8.ValidString(stored) {
		t.Fatal("截断产生了非法 UTF-8")
	}
}

func TestStatsCount(t *testing.T) {
	api := &fakeAPI{queryResult: client.ResultSet{entity.NewColumnInt64("count(*)", []int64{7})}}
	s := newTestStore(api, 16)

	st, err := s.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !st.Enabled || st.Backend != "milvus" || st.Collection != "astra_memory" || st.Dim != 16 {
		t.Fatalf("stats = %+v", st)
	}
	if st.Count != 7 {
		t.Fatalf("count = %d, want 7", st.Count)
	}
	if st.Embedder != "hash" {
		t.Fatalf("embedder = %s", st.Embedder)
	}
}

func TestClose(t *testing.T) {
	api := &fakeAPI{}
	s := newTestStore(api, 4)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !api.closed {
		t.Fatal("Close 未关闭连接")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
