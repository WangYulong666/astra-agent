package api

import "strings"

// RouteSpec 是契约里一条路由的机器可读描述，供能力注册表消费。
//
// 为什么不直接把 openapi.yaml 丢给 agent：那是给人看的文档，缺「是否启用 /
// 需要哪个环境变量 / 对应 CLI 命令」，而且沙箱里拿不到。这里只抽必要字段。
type RouteSpec struct {
	Method      string
	Path        string
	OperationID string
	Summary     string
	Tag         string
}

// SpecRoutes 从内嵌的 openapi.yaml 解析全部路由。走的是 openapiDocument
// 同一条解析路径，解析器坏了会跟着暴露，而不是另起一套捷径。
func SpecRoutes() ([]RouteSpec, error) {
	doc, err := parseYAMLSubset(openapiYAML)
	if err != nil {
		return nil, err
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, nil
	}
	paths, _ := root["paths"].(map[string]any)
	out := make([]RouteSpec, 0, len(paths))
	for p, raw := range paths {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			op, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			id, _ := op["operationId"].(string)
			summary, _ := op["summary"].(string)
			tags, _ := op["tags"].([]any)
			tag := ""
			if len(tags) > 0 {
				tag, _ = tags[0].(string)
			}
			out = append(out, RouteSpec{
				Method: strings.ToUpper(method), Path: p,
				OperationID: id, Summary: summary, Tag: tag,
			})
		}
	}
	return out, nil
}
