package auth_test

// 编译期断言：*auth.Service 满足 runner.SandboxKeyIssuer 窄接口。
//
// 为什么放在 auth_test 外部测试包而不是 package auth：runner 传递依赖了 auth——
// runner(manager.go) → obs(log.go) → auth。若把这条断言放进 package auth 的
// 内部测试文件（与生产代码同一个编译单元），import runner 会形成环：
//
//	auth[含内部测试] → runner → obs → auth   ❌ import cycle
//
// 外部测试包是独立的编译单元：auth_test → auth 与 auth_test → runner → obs → auth
// 两条边都汇到 auth，但没有任何包 import auth_test，因此不成环。这也是 Go 里
// 「测试想 import 依赖了被测包的包」的标准解法。
import (
	"testing"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/runner"
)

var _ runner.SandboxKeyIssuer = (*auth.Service)(nil)

// 断言在编译期完成；留一个空测试用例，让 -run / 测试报告里能看到这条检查。
func TestServiceSatisfiesRunnerSandboxKeyIssuer(t *testing.T) {}
