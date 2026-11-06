package schedule

// 5 段 cron：分 时 日 月 周。
//
// 为什么自己写：标准库没有，引 cron 库要改 go.mod（冻结）。
// 而且只需要「下一个匹配时刻」这一件事，几十行位运算就够，
// 不需要 robustness 到支持 @every / 秒级 / 6 段那些扩展语法。
//
// 语义上对齐 Vixie cron（Linux 上的 crond）：
//   - 日与周是 **OR**：两个字段都写死时，命中任一即可
//   - 有一边是 `*` 时退化成只看另一边（`*` 恒真，与 AND 等价）
//   - 周字段 0 和 7 都是周日
//   - 不支持 JAN/MON 这类名字、不支持 "@daily"、不支持 6 段（秒）

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrInvalid 表示调用方给的东西不合法（cron 表达式、时区、必填字段）。
// api 层据此回 400 而不是 500。
var ErrInvalid = errors.New("schedule: 参数不合法")

// ErrNoNext 表示表达式合法但永远不匹配（例如 2 月 30 日）。
var ErrNoNext = errors.New("schedule: 未来 5 年内没有匹配时间")

// defaultTimezone 是没配时区时的默认值。
const defaultTimezone = "Asia/Shanghai"

// cronHorizon 是 Next 往前找的上限。5 年足够跨过一个闰年周期
// （`0 0 29 2 *` 从 3 月往后找最多等 4 年），再远就认为是写错了。
const cronHorizonYears = 5

// cronSpec 是解析后的表达式。每个字段压成位图，匹配就是一次位测试。
//
// 用位图而不是 slice：字段取值范围都 ≤ 60，一个 uint64 装得下，
// 匹配 O(1)，也省得为每次判断分配。
type cronSpec struct {
	minute uint64 // 0-59
	hour   uint64 // 0-23
	dom    uint64 // 1-31
	month  uint64 // 1-12
	dow    uint64 // 0-6（7 已归一成 0）

	// domStar / dowStar 记录「该字段是不是以 * 开头」。
	// 这是 OR 语义的开关：两边都不是 * 才 OR，否则只看写死的那边。
	// 注意 `*/2` 也算 star——Vixie cron 的 flags 就是这么打的。
	domStar bool
	dowStar bool

	loc *time.Location
}

// parseCron 解析表达式并绑定时区。tz 为空用 Asia/Shanghai。
func parseCron(expr, tz string) (*cronSpec, error) {
	loc, err := loadLocation(tz)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("%w：cron 需要 5 段（分 时 日 月 周），实际 %d 段 %q", ErrInvalid, len(fields), expr)
	}
	c := &cronSpec{loc: loc}
	if c.minute, _, err = parseField(fields[0], "分", 0, 59, false); err != nil {
		return nil, err
	}
	if c.hour, _, err = parseField(fields[1], "时", 0, 23, false); err != nil {
		return nil, err
	}
	if c.dom, c.domStar, err = parseField(fields[2], "日", 1, 31, false); err != nil {
		return nil, err
	}
	if c.month, _, err = parseField(fields[3], "月", 1, 12, false); err != nil {
		return nil, err
	}
	if c.dow, c.dowStar, err = parseField(fields[4], "周", 0, 7, true); err != nil {
		return nil, err
	}
	return c, nil
}

// loadLocation 解析时区名。空 → 默认时区。
//
// 默认时区加载失败（精简镜像没装 tzdata）时退回固定 +08:00 而不是报错：
// 那是环境问题，不该让整个调度能力起不来。用户显式写的时区名不认识则明确报错。
func loadLocation(tz string) (*time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		tz = defaultTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err == nil {
		return loc, nil
	}
	if tz == defaultTimezone {
		return time.FixedZone("CST", 8*3600), nil
	}
	return nil, fmt.Errorf("%w：未知时区 %q", ErrInvalid, tz)
}

// parseField 解析一个字段，返回位图与「是否以 * 开头」。
//
// 支持：`*`、`*/n`、`a`、`a-b`、`a-b/n`、`a/n`（a 到上界每 n 个）、以及用逗号串起来。
// wrap7 只对周字段为真：值 7 归一成 0（周日）。
func parseField(field, name string, min, max int, wrap7 bool) (uint64, bool, error) {
	var mask uint64
	star := strings.HasPrefix(field, "*")
	for _, item := range strings.Split(field, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return 0, false, fmt.Errorf("%w：%s 段 %q 里有空项（多余的逗号）", ErrInvalid, name, field)
		}
		lo, hi, step := min, max, 1
		switch {
		case item == "*":
			// 全段
		default:
			body := item
			hasStep := false
			if i := strings.IndexByte(body, '/'); i >= 0 {
				if strings.IndexByte(body[i+1:], '/') >= 0 {
					return 0, false, fmt.Errorf("%w：%s 段 %q 步长写了两遍", ErrInvalid, name, item)
				}
				n, err := parseStep(body[i+1:], name, item)
				if err != nil {
					return 0, false, err
				}
				step, hasStep = n, true
				body = body[:i]
			}
			switch {
			case body == "*":
				// `*/n`：上界保持 max
			default:
				if i := strings.IndexByte(body, '-'); i >= 0 {
					var err error
					if lo, err = parseNum(body[:i], name, item); err != nil {
						return 0, false, err
					}
					if hi, err = parseNum(body[i+1:], name, item); err != nil {
						return 0, false, err
					}
				} else {
					var err error
					if lo, err = parseNum(body, name, item); err != nil {
						return 0, false, err
					}
					// `a/n` 约定是「从 a 到上界，每 n 个」；`a` 就是单个值。
					hi = lo
					if hasStep {
						hi = max
					}
				}
			}
		}
		if lo < min || hi > max {
			return 0, false, fmt.Errorf("%w：%s 段 %q 超出范围 %d-%d", ErrInvalid, name, item, min, max)
		}
		if lo > hi {
			return 0, false, fmt.Errorf("%w：%s 段 %q 的区间起点大于终点", ErrInvalid, name, item)
		}
		for v := lo; v <= hi; v += step {
			n := v
			if wrap7 && n == 7 {
				n = 0
			}
			mask |= 1 << uint(n)
		}
	}
	return mask, star, nil
}

func parseNum(s, name, item string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		return 0, fmt.Errorf("%w：%s 段 %q 不是合法数字", ErrInvalid, name, item)
	}
	return n, nil
}

func parseStep(s, name, item string) (int, error) {
	n, err := parseNum(s, name, item)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("%w：%s 段 %q 的步长必须大于 0", ErrInvalid, name, item)
	}
	return n, nil
}

// Next 返回 t 之后（严格大于，且按分钟对齐）的第一个匹配时刻。
//
// 跳着走而不是逐分钟试：月份/日期/小时不对就整段跳过去，
// 最坏情况（2-29）也只有几千次循环，逐分钟要上百万次。
func (c *cronSpec) Next(t time.Time) (time.Time, error) {
	limit := t.AddDate(cronHorizonYears, 0, 0)
	cur := t.In(c.loc).Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 200000; i++ {
		if cur.After(limit) {
			return time.Time{}, fmt.Errorf("%w：%s", ErrNoNext, c.String())
		}
		y, mo, d := cur.Date()
		var next time.Time
		switch {
		case !bitSet(c.month, int(mo)):
			next = time.Date(y, mo+1, 1, 0, 0, 0, 0, c.loc)
		case !c.dayMatches(cur):
			next = time.Date(y, mo, d+1, 0, 0, 0, 0, c.loc)
		case !bitSet(c.hour, cur.Hour()):
			if h, ok := nextBit(c.hour, cur.Hour()+1); ok {
				next = time.Date(y, mo, d, h, 0, 0, 0, c.loc)
			} else {
				next = time.Date(y, mo, d+1, 0, 0, 0, 0, c.loc)
			}
		case !bitSet(c.minute, cur.Minute()):
			if m, ok := nextBit(c.minute, cur.Minute()+1); ok {
				next = time.Date(y, mo, d, cur.Hour(), m, 0, 0, c.loc)
			} else if h, ok := nextBit(c.hour, cur.Hour()+1); ok {
				next = time.Date(y, mo, d, h, 0, 0, 0, c.loc)
			} else {
				next = time.Date(y, mo, d+1, 0, 0, 0, 0, c.loc)
			}
		default:
			return cur, nil
		}
		// 兜底：DST 春季跳变时，那个本地时刻根本不存在，time.Date 会把它
		// **往前**归一（02:00 → 01:00），于是 next 不前进，循环原地打转。
		// 按绝对时间 +1 分钟，保证每轮严格前进——代价是跳变空洞里要逐分钟走
		// （最多一小时），换来的是绝不空转。
		if !next.After(cur) {
			next = cur.Add(time.Minute)
		}
		cur = next
	}
	return time.Time{}, fmt.Errorf("%w：%s", ErrNoNext, c.String())
}

// dayMatches 是 OR 语义的核心。
// 两个字段都写死（都不是 *）时命中任一即可——这是标准 cron 的行为，
// 写成 AND 会让 `0 0 1 * 1` 变成「1 号且是周一」，一年只跑一两次。
func (c *cronSpec) dayMatches(t time.Time) bool {
	dom := bitSet(c.dom, t.Day())
	dow := bitSet(c.dow, int(t.Weekday()))
	if c.domStar || c.dowStar {
		return dom && dow
	}
	return dom || dow
}

// String 只用于错误信息，不保证能还原成原表达式。
func (c *cronSpec) String() string {
	return fmt.Sprintf("分=%#x 时=%#x 日=%#x 月=%#x 周=%#x 时区=%s",
		c.minute, c.hour, c.dom, c.month, c.dow, c.loc)
}

func bitSet(mask uint64, v int) bool {
	if v < 0 || v > 63 {
		return false
	}
	return mask&(1<<uint(v)) != 0
}

// nextBit 找 >= from 的第一个置位，没有返回 false。
func nextBit(mask uint64, from int) (int, bool) {
	for i := from; i < 64; i++ {
		if mask&(1<<uint(i)) != 0 {
			return i, true
		}
	}
	return 0, false
}
