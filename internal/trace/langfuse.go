// openLangfuse 由 Langfuse 实现补全（走 Ingestion HTTP API，不引 SDK）。
package trace

import "fmt"

func openLangfuse(cfg Config) (Tracer, error) {
	return nil, fmt.Errorf("trace: Langfuse 实现未接入（ENDPOINT=%s）", cfg.Endpoint)
}
