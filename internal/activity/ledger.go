package activity

import "fmt"

// openImpl 是装配桩，由并行任务替换成真实现。
func openImpl(_ RunReader, _ Sources) (Ledger, error) {
	return nil, fmt.Errorf("activity: 实现未接入")
}
