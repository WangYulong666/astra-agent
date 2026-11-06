package schedule

import "fmt"

// Open 装配调度器与持久化。dataDir 是 schedule.json 所在目录。
func Open(cfg Config, runner Runner, dataDir string) (Store, Scheduler, error) {
	return nil, nil, fmt.Errorf("schedule: 实现未接入（data=%s）", dataDir)
}
