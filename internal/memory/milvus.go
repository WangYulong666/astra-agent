package memory

// Milvus 实现。
//
// 三件事必须守住：
//  1. 检索/删除的过滤表达式**永远**带 tenant_id + user_id，且值不许含引号——
//     表达式是拼字符串，一个没转义的双引号就等于把别人的记忆读出来。
//  2. tenant_id 是 partition key，多租户在物理上聚在一起；配合 expr 可以做分区裁剪。
//  3. 集合维度建好就改不了，所以维度不符要**明确报错**，不能静默写坏。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/WangYulong666/astra-agent/internal/store"
	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

// 字段名集中在这里：expr 里是裸字符串，散落在代码里迟早拼错一个字母，
// 拼错不会编译失败，只会变成「查不到」或者更糟——过滤条件写错仍能查到别人的数据。
const (
	fieldID        = "id"
	fieldTenantID  = "tenant_id"
	fieldUserID    = "user_id"
	fieldSessionID = "session_id"
	fieldKind      = "kind"
	fieldText      = "text"
	fieldVector    = "vector"
	fieldCreatedAt = "created_at"
	fieldMeta      = "meta"
)

const (
	defaultCollection = "astra_memory"
	// idMaxLen 与设计文档一致：VarChar(64)，实际生成 32 位十六进制。
	idMaxLen = 64
	// textMaxLen 是 text 字段上限**字节数**（Milvus 的 max_length 按字节算）。
	textMaxLen = 8192
	// kindMaxLen / sessionMaxLen 只为 schema 声明，取值远小于上限。
	kindMaxLen    = 16
	sessionMaxLen = 64

	defaultTopK = 10
	maxTopK     = 50
	// ivfNlist / hnswM / hnswEfConstruction 见设计文档的索引选型。
	ivfNlist               = 1024
	hnswM                  = 16
	hnswEfConstruction     = 200
	ivfNprobe              = 16
	hnswEfSearch           = 64
	openIndexBuildTimeout  = 120 * time.Second
	searchQueryConsistency = entity.ClStrong
)

// outputFields 是检索要回填的字段。刻意不带 vector：
// 回传 768 维浮点只是为了在网络上多传几十 KB，调用方用不上。
var outputFields = []string{
	fieldText, fieldKind, fieldSessionID, fieldCreatedAt, fieldMeta, fieldUserID, fieldTenantID,
}

// milvusAPI 是 SDK client.Client 里本包真正用到的子集。
//
// 抽成接口不是为了「可替换存储」，而是为了单测能注入假实现去断言 expr——
// expr 泄漏是安全问题，必须能在没有真实 Milvus 的机器上验证。
type milvusAPI interface {
	Insert(ctx context.Context, collName string, partitionName string, columns ...entity.Column) (entity.Column, error)
	Search(ctx context.Context, collName string, partitions []string, expr string, outputFields []string,
		vectors []entity.Vector, vectorField string, metricType entity.MetricType, topK int,
		sp entity.SearchParam, opts ...client.SearchQueryOptionFunc) ([]client.SearchResult, error)
	Query(ctx context.Context, collectionName string, partitionNames []string, expr string,
		outputFields []string, opts ...client.SearchQueryOptionFunc) (client.ResultSet, error)
	DeleteByPks(ctx context.Context, collName string, partitionName string, ids entity.Column) error
	Close() error
}

// milvusStore 是 Store 的 Milvus 实现。
type milvusStore struct {
	api  milvusAPI
	coll string
	dim  int
	emb  Embedder
	// hnsw 决定搜索参数：IVF_FLAT 用 nprobe，HNSW 用 ef，传错参数会直接报错。
	hnsw bool
}

func openMilvus(cfg Config) (Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), openIndexBuildTimeout)
	defer cancel()

	emb, err := newEmbedder(ctx, cfg)
	if err != nil {
		return nil, err
	}

	coll := cfg.Collection
	if coll == "" {
		coll = defaultCollection
	}
	db := cfg.DB
	if db == "" {
		db = "default"
	}

	c, err := client.NewClient(ctx, client.Config{Address: cfg.Addr, DBName: db})
	if err != nil {
		return nil, fmt.Errorf("memory: 连接 Milvus %s: %w", cfg.Addr, err)
	}

	s := &milvusStore{api: c, coll: coll, dim: emb.Dim(), emb: emb}
	if err := s.bootstrap(ctx, c); err != nil {
		// 建表/校验失败就把连接关掉，别留一个半残的 client 在进程里
		_ = c.Close()
		return nil, err
	}
	log.Printf("memory: Milvus 就绪 %s collection=%s dim=%d embedder=%s", cfg.Addr, coll, s.dim, emb.Name())
	return s, nil
}

// bootstrap 保证集合存在、维度匹配、已加载。已存在时只校验不修改：
// 维度不一致是**配置错误**，自动重建等于把用户的历史记忆静默删掉。
func (s *milvusStore) bootstrap(ctx context.Context, c client.Client) error {
	has, err := c.HasCollection(ctx, s.coll)
	if err != nil {
		return fmt.Errorf("memory: 检查集合 %s: %w", s.coll, err)
	}

	if has {
		desc, err := c.DescribeCollection(ctx, s.coll)
		if err != nil {
			return fmt.Errorf("memory: 读取集合 %s 结构: %w", s.coll, err)
		}
		dim, ok := schemaVectorDim(desc.Schema)
		if !ok {
			return fmt.Errorf("memory: 集合 %s 里没有 %s 字段，结构与当前版本不符，请重建集合", s.coll, fieldVector)
		}
		if dim != s.dim {
			return fmt.Errorf("memory: 集合 %s 的向量维度是 %d，当前 embedder 是 %d。维度无法在线修改："+
				"要么把 MEMORY_EMBED_DIM 调回 %d，要么重建集合（DROP COLLECTION %s 后重启，历史记忆会丢失）",
				s.coll, dim, s.dim, dim, s.coll)
		}
		// 索引类型从已建索引读回来，避免「HNSW 集合用 nprobe 搜」这种参数错配
		if idxs, err := c.DescribeIndex(ctx, s.coll, fieldVector); err == nil {
			for _, idx := range idxs {
				if idx.IndexType() == entity.HNSW {
					s.hnsw = true
				}
			}
		}
		if !desc.Loaded {
			if err := c.LoadCollection(ctx, s.coll, false); err != nil {
				return fmt.Errorf("memory: 加载集合 %s: %w", s.coll, err)
			}
		}
		return nil
	}

	schema := buildSchema(s.coll, s.dim)
	if err := c.CreateCollection(ctx, schema, entity.DefaultShardNumber); err != nil {
		return fmt.Errorf("memory: 创建集合 %s: %w", s.coll, err)
	}
	idx, hnsw, err := buildIndex(s.dim)
	if err != nil {
		return fmt.Errorf("memory: 构造向量索引参数: %w", err)
	}
	if err := c.CreateIndex(ctx, s.coll, fieldVector, idx, false); err != nil {
		return fmt.Errorf("memory: 为集合 %s 建索引: %w", s.coll, err)
	}
	s.hnsw = hnsw
	if err := c.LoadCollection(ctx, s.coll, false); err != nil {
		return fmt.Errorf("memory: 加载集合 %s: %w", s.coll, err)
	}
	return nil
}

// buildSchema 按设计文档的字段表建 schema。
// tenant_id 是 partition key：多租户物理聚合，搜索时配合 expr 能裁剪分区。
func buildSchema(name string, dim int) *entity.Schema {
	return entity.NewSchema().
		WithName(name).
		WithDescription("astra 用户长期记忆；tenant_id 为 partition key，检索必须带 tenant+user").
		WithAutoID(false).
		WithField(entity.NewField().WithName(fieldID).
			WithDataType(entity.FieldTypeVarChar).WithIsPrimaryKey(true).WithMaxLength(idMaxLen)).
		WithField(entity.NewField().WithName(fieldTenantID).
			WithDataType(entity.FieldTypeVarChar).WithMaxLength(idMaxLen).WithIsPartitionKey(true)).
		WithField(entity.NewField().WithName(fieldUserID).
			WithDataType(entity.FieldTypeVarChar).WithMaxLength(idMaxLen)).
		WithField(entity.NewField().WithName(fieldSessionID).
			WithDataType(entity.FieldTypeVarChar).WithMaxLength(sessionMaxLen)).
		WithField(entity.NewField().WithName(fieldKind).
			WithDataType(entity.FieldTypeVarChar).WithMaxLength(kindMaxLen)).
		WithField(entity.NewField().WithName(fieldText).
			WithDataType(entity.FieldTypeVarChar).WithMaxLength(textMaxLen)).
		WithField(entity.NewField().WithName(fieldVector).
			WithDataType(entity.FieldTypeFloatVector).WithDim(int64(dim))).
		WithField(entity.NewField().WithName(fieldCreatedAt).
			WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName(fieldMeta).
			WithDataType(entity.FieldTypeJSON))
}

// buildIndex 小维度用 IVF_FLAT（建索引快、内存省），大维度用 HNSW（高维下 IVF 要更多聚类才准）。
func buildIndex(dim int) (entity.Index, bool, error) {
	if dim <= 1024 {
		idx, err := entity.NewIndexIvfFlat(entity.L2, ivfNlist)
		return idx, false, err
	}
	idx, err := entity.NewIndexHNSW(entity.L2, hnswM, hnswEfConstruction)
	return idx, true, err
}

func schemaVectorDim(s *entity.Schema) (int, bool) {
	if s == nil {
		return 0, false
	}
	for _, f := range s.Fields {
		if f.Name != fieldVector {
			continue
		}
		if f.DataType != entity.FieldTypeFloatVector {
			return 0, false
		}
		var dim int
		if _, err := fmt.Sscanf(f.TypeParams[entity.TypeParamDim], "%d", &dim); err != nil || dim <= 0 {
			return 0, false
		}
		return dim, true
	}
	return 0, false
}

// Ingest 算向量后落库。id / created_at 由这里补全并回填给调用方。
func (s *milvusStore) Ingest(ctx context.Context, it Item) (Item, error) {
	if strings.TrimSpace(it.Text) == "" {
		return Item{}, errors.New("memory: text 不能为空")
	}
	// 无主记忆等于写进了「谁都能查」的空隙里。宁可报错。
	if it.TenantID == "" || it.UserID == "" {
		return Item{}, errors.New("memory: tenant_id 与 user_id 不能为空")
	}
	if it.Kind == "" {
		it.Kind = KindFact
	}

	vecs, err := s.emb.Embed(ctx, []string{it.Text})
	if err != nil {
		return Item{}, fmt.Errorf("memory: 生成向量: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != s.dim {
		return Item{}, fmt.Errorf("memory: embedder 返回维度异常（期望 1×%d）", s.dim)
	}

	it.Text = truncateBytes(it.Text, textMaxLen)
	if it.CreatedAt.IsZero() {
		it.CreatedAt = time.Now().UTC()
	}
	if it.ID == "" {
		it.ID = newID(it)
	}

	meta, err := encodeMeta(it.Meta)
	if err != nil {
		return Item{}, err
	}

	// partition key 集合不能指定分区名：服务端按 tenant_id 的哈希值路由，
	// 手工传分区名会被拒。租户的物理聚集由 schema 保证，不靠这里。
	if _, err := s.api.Insert(ctx, s.coll, "",
		entity.NewColumnVarChar(fieldID, []string{it.ID}),
		entity.NewColumnVarChar(fieldTenantID, []string{it.TenantID}),
		entity.NewColumnVarChar(fieldUserID, []string{it.UserID}),
		entity.NewColumnVarChar(fieldSessionID, []string{it.SessionID}),
		entity.NewColumnVarChar(fieldKind, []string{it.Kind}),
		entity.NewColumnVarChar(fieldText, []string{it.Text}),
		entity.NewColumnFloatVector(fieldVector, s.dim, [][]float32{vecs[0]}),
		entity.NewColumnInt64(fieldCreatedAt, []int64{it.CreatedAt.Unix()}),
		entity.NewColumnJSONBytes(fieldMeta, [][]byte{meta}),
	); err != nil {
		return Item{}, fmt.Errorf("memory: 写入 Milvus: %w", err)
	}
	return it, nil
}

// Search 检索。expr 永远包含 tenant_id + user_id，见 buildExpr。
func (s *milvusStore) Search(ctx context.Context, q Query) ([]Item, error) {
	expr, err := buildExpr(q.TenantID, q.UserID, q.Kind)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(q.Text) == "" {
		return nil, errors.New("memory: 检索文本不能为空")
	}
	k := q.K
	if k <= 0 {
		k = defaultTopK
	}
	if k > maxTopK {
		k = maxTopK
	}

	vecs, err := s.emb.Embed(ctx, []string{q.Text})
	if err != nil {
		return nil, fmt.Errorf("memory: 生成查询向量: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != s.dim {
		return nil, fmt.Errorf("memory: embedder 返回维度异常（期望 1×%d）", s.dim)
	}

	sp, err := s.searchParam()
	if err != nil {
		return nil, err
	}
	res, err := s.api.Search(ctx, s.coll, nil, expr, outputFields,
		[]entity.Vector{entity.FloatVector(vecs[0])}, fieldVector, entity.L2, k, sp,
		client.WithSearchQueryConsistencyLevel(searchQueryConsistency))
	if err != nil {
		return nil, fmt.Errorf("memory: 检索 Milvus: %w", err)
	}
	return toItems(res), nil
}

// Delete 先校验归属再删。
//
// 直接 DeleteByPks 是不行的：id 是 32 位十六进制，猜中的概率虽低，
// 但「猜不到」不是安全边界，且删错就是跨租户的数据破坏。
// 所以先用带 tenant+user 的 expr 查一次，查得到才删。
func (s *milvusStore) Delete(ctx context.Context, tenantID, userID, id string) error {
	expr, err := buildExpr(tenantID, userID, "")
	if err != nil {
		return err
	}
	idLit, err := exprLiteral(id)
	if err != nil {
		return err
	}
	expr += " and " + fieldID + " == " + idLit

	rs, err := s.api.Query(ctx, s.coll, nil, expr, []string{fieldID},
		client.WithSearchQueryConsistencyLevel(searchQueryConsistency))
	if err != nil {
		return fmt.Errorf("memory: 校验记忆归属: %w", err)
	}
	if rs.Len() == 0 || rs[0].Len() == 0 {
		// 与「不存在」同一个响应：不告诉调用方「这个 id 存在但不属于你」
		return fmt.Errorf("memory: 记忆 %s 不存在: %w", id, store.ErrNotFound)
	}
	got, err := rs[0].Get(0)
	if err != nil {
		return fmt.Errorf("memory: 读取记忆 id: %w", err)
	}
	pk, _ := got.(string)
	if pk == "" {
		pk = id
	}
	if err := s.api.DeleteByPks(ctx, s.coll, "", entity.NewColumnVarChar(fieldID, []string{pk})); err != nil {
		return fmt.Errorf("memory: 删除记忆: %w", err)
	}
	return nil
}

// Stats 只给个数，不返回内容——运营页和 admin-cli 都不该看到用户写了什么。
func (s *milvusStore) Stats(ctx context.Context) (Stats, error) {
	st := Stats{
		Enabled:    true,
		Backend:    "milvus",
		Collection: s.coll,
		Dim:        s.dim,
		Embedder:   s.emb.Name(),
	}
	rs, err := s.api.Query(ctx, s.coll, nil, "", []string{"count(*)"},
		client.WithSearchQueryConsistencyLevel(searchQueryConsistency))
	if err != nil {
		return st, fmt.Errorf("memory: 统计记忆条数: %w", err)
	}
	if rs.Len() > 0 && rs[0].Len() > 0 {
		if v, err := rs[0].Get(0); err == nil {
			st.Count = asInt64(v)
		}
	}
	return st, nil
}

func (s *milvusStore) Close() error {
	if s.api == nil {
		return nil
	}
	return s.api.Close()
}

func (s *milvusStore) searchParam() (entity.SearchParam, error) {
	if s.hnsw {
		return entity.NewIndexHNSWSearchParam(hnswEfSearch)
	}
	return entity.NewIndexIvfFlatSearchParam(ivfNprobe)
}

// ── 表达式与转换 ────────────────────────────────────────────

// buildExpr 构造过滤表达式。tenant / user 是必填，kind 可选。
//
// 这是整个模块的安全边界：值里只要有一个双引号能透传进来，
// 就能拼出 `user_id == "x" or user_id != ""` 读走别人的记忆。
// 所以不做「尽力转义」，而是**直接拒绝**含引号/反斜杠/控制字符的值——
// 合法身份标识（租户 id、用户 id、kind）本来就不该含这些字符。
func buildExpr(tenantID, userID, kind string) (string, error) {
	t, err := exprLiteral(tenantID)
	if err != nil {
		return "", err
	}
	u, err := exprLiteral(userID)
	if err != nil {
		return "", err
	}
	expr := fieldTenantID + " == " + t + " and " + fieldUserID + " == " + u
	if kind != "" {
		k, err := exprLiteral(kind)
		if err != nil {
			return "", err
		}
		expr += " and " + fieldKind + " == " + k
	}
	return expr, nil
}

func exprLiteral(v string) (string, error) {
	if v == "" {
		return "", errors.New("memory: 过滤条件不能为空")
	}
	if len(v) > maxExprLiteral {
		return "", fmt.Errorf("memory: 过滤条件过长（%d > %d）", len(v), maxExprLiteral)
	}
	for _, r := range v {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("memory: 过滤条件含非法字符 %q", r)
		}
	}
	return `"` + v + `"`, nil
}

// maxExprLiteral 与 schema 里 VarChar 的长度上限对齐。
const maxExprLiteral = idMaxLen

func toItems(res []client.SearchResult) []Item {
	if len(res) == 0 {
		return nil
	}
	r := res[0]
	if r.Err != nil || r.IDs == nil {
		return nil
	}
	n := r.IDs.Len()
	items := make([]Item, 0, n)
	for i := 0; i < n; i++ {
		it := Item{}
		if v, err := r.IDs.Get(i); err == nil {
			it.ID, _ = v.(string)
		}
		it.Text = columnString(r.Fields, fieldText, i)
		it.Kind = columnString(r.Fields, fieldKind, i)
		it.SessionID = columnString(r.Fields, fieldSessionID, i)
		it.UserID = columnString(r.Fields, fieldUserID, i)
		it.TenantID = columnString(r.Fields, fieldTenantID, i)
		if sec := columnInt64(r.Fields, fieldCreatedAt, i); sec > 0 {
			it.CreatedAt = time.Unix(sec, 0).UTC()
		}
		it.Meta = columnJSON(r.Fields, fieldMeta, i)
		if i < len(r.Scores) {
			// Milvus 的 L2 是距离：越小越像。直接返回距离会让调用方把「最差」当「最好」，
			// 所以统一换成 0~1 的相似度（距离 0 → 1）。
			it.Score = 1 / (1 + r.Scores[i])
		}
		items = append(items, it)
	}
	return items
}

func columnString(fields client.ResultSet, name string, i int) string {
	for _, c := range fields {
		if c.Name() != name || i >= c.Len() {
			continue
		}
		if v, err := c.Get(i); err == nil {
			switch s := v.(type) {
			case string:
				return s
			case []byte:
				return string(s)
			}
		}
	}
	return ""
}

func columnInt64(fields client.ResultSet, name string, i int) int64 {
	for _, c := range fields {
		if c.Name() != name || i >= c.Len() {
			continue
		}
		if v, err := c.Get(i); err == nil {
			return asInt64(v)
		}
	}
	return 0
}

func columnJSON(fields client.ResultSet, name string, i int) map[string]any {
	for _, c := range fields {
		if c.Name() != name || i >= c.Len() {
			continue
		}
		v, err := c.Get(i)
		if err != nil {
			return nil
		}
		var raw []byte
		switch x := v.(type) {
		case []byte:
			raw = x
		case string:
			raw = []byte(x)
		default:
			return nil
		}
		if len(raw) == 0 {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
			return nil
		}
		return m
	}
	return nil
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case string:
		var out int64
		if _, err := fmt.Sscanf(n, "%d", &out); err == nil {
			return out
		}
	}
	return 0
}

func encodeMeta(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("memory: 序列化 meta: %w", err)
	}
	return b, nil
}

// newID 取 (归属 + 内容 + 纳秒时间) 的 sha256 前 16 字节。
// 不用自增/时间戳做主键：id 会出现在 URL 里，带时间信息等于泄露写入时间。
func newID(it Item) string {
	h := sha256.New()
	for _, part := range []string{it.TenantID, it.UserID, it.SessionID, it.Kind, it.Text} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	_, _ = fmt.Fprintf(h, "%d", time.Now().UnixNano())
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// truncateBytes 按字节截断，并且不切断 UTF-8 字符。
// Milvus 的 VarChar 上限按字节算，直接按 rune 截会超；按字节硬切会产生非法 UTF-8。
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
