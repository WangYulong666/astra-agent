package schedule

import (
	"errors"
	"testing"
	"time"
)

// at 在指定时区解析「2006-01-02 15:04:05」，测试里写时间更直观。
func at(t *testing.T, tz, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("加载时区 %s: %v", tz, err)
	}
	tm, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc)
	if err != nil {
		t.Fatalf("解析时间 %s: %v", s, err)
	}
	return tm
}

func mustCron(t *testing.T, expr, tz string) *cronSpec {
	t.Helper()
	c, err := parseCron(expr, tz)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", expr, err)
	}
	return c
}

func TestCronNextEveryFiveMinutes(t *testing.T) {
	c := mustCron(t, "*/5 * * * *", "")
	got, err := c.Next(at(t, "Asia/Shanghai", "2026-01-01 10:02:30"))
	if err != nil {
		t.Fatal(err)
	}
	want := at(t, "Asia/Shanghai", "2026-01-01 10:05:00")
	if !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestCronNextDailyNine(t *testing.T) {
	c := mustCron(t, "0 9 * * *", "")
	// 当天还没到 9 点
	if got, want := mustNext(t, c, at(t, "Asia/Shanghai", "2026-01-01 08:00:00")),
		at(t, "Asia/Shanghai", "2026-01-01 09:00:00"); !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
	// 已经过了：顺延到明天（Next 是严格大于，09:00:00 这个瞬间本身不算）
	if got, want := mustNext(t, c, at(t, "Asia/Shanghai", "2026-01-01 09:00:00")),
		at(t, "Asia/Shanghai", "2026-01-02 09:00:00"); !got.Equal(want) {
		t.Fatalf("整点边界 got %s want %s", got, want)
	}
	if got, want := mustNext(t, c, at(t, "Asia/Shanghai", "2026-01-01 23:59:59")),
		at(t, "Asia/Shanghai", "2026-01-02 09:00:00"); !got.Equal(want) {
		t.Fatalf("跨天 got %s want %s", got, want)
	}
}

func TestCronNextMonthlyFirst(t *testing.T) {
	c := mustCron(t, "0 0 1 * *", "")
	got := mustNext(t, c, at(t, "Asia/Shanghai", "2026-01-15 12:00:00"))
	want := at(t, "Asia/Shanghai", "2026-02-01 00:00:00")
	if !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
	// 12 月 1 号之后应该是次年 1 月 1 号（跨年）
	got = mustNext(t, c, at(t, "Asia/Shanghai", "2026-12-05 00:00:00"))
	want = at(t, "Asia/Shanghai", "2027-01-01 00:00:00")
	if !got.Equal(want) {
		t.Fatalf("跨年 got %s want %s", got, want)
	}
}

func TestCronLeapDay(t *testing.T) {
	c := mustCron(t, "0 0 29 2 *", "")
	// 2026 不是闰年，下一次 2-29 是 2028
	got := mustNext(t, c, at(t, "Asia/Shanghai", "2026-03-01 00:00:00"))
	want := at(t, "Asia/Shanghai", "2028-02-29 00:00:00")
	if !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
	// 闰年当天再往后找，是下一个闰年（严格大于，不是同一天）
	got = mustNext(t, c, at(t, "Asia/Shanghai", "2028-02-29 00:00:00"))
	want = at(t, "Asia/Shanghai", "2032-02-29 00:00:00")
	if !got.Equal(want) {
		t.Fatalf("闰年边界 got %s want %s", got, want)
	}
}

func TestCronDayOfWeekSundayZeroAndSeven(t *testing.T) {
	zero := mustCron(t, "0 0 * * 0", "")
	seven := mustCron(t, "0 0 * * 7", "")
	from := at(t, "Asia/Shanghai", "2026-01-09 12:00:00") // 周五
	a := mustNext(t, zero, from)
	b := mustNext(t, seven, from)
	if !a.Equal(b) {
		t.Fatalf("0 与 7 应都是周日：%s vs %s", a, b)
	}
	if a.Weekday() != time.Sunday {
		t.Fatalf("期望周日，实际 %s", a.Weekday())
	}
	if want := at(t, "Asia/Shanghai", "2026-01-11 00:00:00"); !a.Equal(want) {
		t.Fatalf("got %s want %s", a, want)
	}
}

// 日与周是 OR：只要命中任一。写成 AND 的话这条从 1 月 7 日往后要找
// 「既是 1 号又是周一」，得等到 2026-06-01，一年跑不了几次。
func TestCronDayOfMonthOrDayOfWeek(t *testing.T) {
	c := mustCron(t, "0 0 1 * 1", "")
	from := at(t, "Asia/Shanghai", "2026-01-07 12:00:00") // 周三
	got := mustNext(t, c, from)
	want := at(t, "Asia/Shanghai", "2026-01-12 00:00:00") // 下周一，不是 1 号
	if !got.Equal(want) {
		t.Fatalf("OR 语义失败：got %s want %s", got, want)
	}
	if got.Weekday() != time.Monday || got.Day() != 12 {
		t.Fatalf("got %s，期望周一", got)
	}
	// 反过来：1 号不是周一也要命中
	from = at(t, "Asia/Shanghai", "2026-01-31 12:00:00") // 周六
	got = mustNext(t, c, from)
	want = at(t, "Asia/Shanghai", "2026-02-01 00:00:00") // 周日，但是 1 号
	if !got.Equal(want) {
		t.Fatalf("OR 语义失败（1 号分支）：got %s want %s", got, want)
	}
	if got.Weekday() == time.Monday {
		t.Fatalf("这条应该靠日字段命中，结果是周一 %s，说明测试没覆盖到", got)
	}
}

// 一边写 * 时是「只看另一边」，不该被 OR 影响成「天天跑」。
func TestCronStarFieldDoesNotWiden(t *testing.T) {
	c := mustCron(t, "0 0 * * 1", "")
	got := mustNext(t, c, at(t, "Asia/Shanghai", "2026-01-07 12:00:00"))
	if want := at(t, "Asia/Shanghai", "2026-01-12 00:00:00"); !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestCronInvisibleRangeAndStep(t *testing.T) {
	c := mustCron(t, "10-20/5 * * * *", "")
	cases := []struct{ from, want string }{
		{"2026-01-01 10:01:00", "2026-01-01 10:10:00"},
		{"2026-01-01 10:10:00", "2026-01-01 10:15:00"},
		{"2026-01-01 10:20:00", "2026-01-01 11:10:00"}, // 区间用完，跳到下一小时
	}
	for _, tc := range cases {
		got := mustNext(t, c, at(t, "Asia/Shanghai", tc.from))
		want := at(t, "Asia/Shanghai", tc.want)
		if !got.Equal(want) {
			t.Fatalf("from %s: got %s want %s", tc.from, got, want)
		}
	}
}

func TestCronList(t *testing.T) {
	c := mustCron(t, "0,30 * * * *", "")
	if got, want := mustNext(t, c, at(t, "Asia/Shanghai", "2026-01-01 10:05:00")),
		at(t, "Asia/Shanghai", "2026-01-01 10:30:00"); !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
	if got, want := mustNext(t, c, at(t, "Asia/Shanghai", "2026-01-01 10:30:00")),
		at(t, "Asia/Shanghai", "2026-01-01 11:00:00"); !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestCronYearEndBoundary(t *testing.T) {
	c := mustCron(t, "59 23 31 12 *", "")
	got := mustNext(t, c, at(t, "Asia/Shanghai", "2026-06-01 00:00:00"))
	want := at(t, "Asia/Shanghai", "2026-12-31 23:59:00")
	if !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestCronTimezoneIsHonored(t *testing.T) {
	c := mustCron(t, "0 9 * * *", "America/New_York")
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := mustNext(t, c, from)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 1, 1, 9, 0, 0, 0, ny)
	if !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
	if h := got.In(ny).Hour(); h != 9 {
		t.Fatalf("本地小时应为 9，实际 %d", h)
	}
}

func TestCronDefaultTimezone(t *testing.T) {
	c := mustCron(t, "0 9 * * *", "")
	if name := c.loc.String(); name != "Asia/Shanghai" {
		t.Fatalf("默认时区应为 Asia/Shanghai，实际 %s", name)
	}
	got := mustNext(t, c, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if got.Hour() != 9 {
		t.Fatalf("应按默认时区的 9 点命中，实际 %s", got)
	}
}

func TestCronInvalidExpressions(t *testing.T) {
	cases := []string{
		"",
		"* * * *",       // 段数不够
		"* * * * * *",   // 段数太多
		"*/0 * * * *",   // 步长 0
		"*/x * * * *",   // 步长不是数字
		"60 * * * *",    // 分钟越界
		"* 24 * * *",    // 小时越界
		"* * 0 * *",     // 日从 1 开始
		"* * 32 * *",    // 日越界
		"* * * 13 *",    // 月越界
		"* * * * 8",     // 周越界
		"5-3 * * * *",   // 区间反了
		"1,,2 * * * *",  // 空项
		"1- * * * *",    // 少了一头
		"-1 * * * *",    // 负数
		"JAN * * * *",   // 不支持名字
		"*/2/3 * * * *", // 两个步长
		"@daily",        // 不支持宏
	}
	for _, expr := range cases {
		if _, err := parseCron(expr, ""); err == nil {
			t.Errorf("%q 应该报错，实际通过了", expr)
		} else if !errors.Is(err, ErrInvalid) {
			t.Errorf("%q 的错误应可被 ErrInvalid 匹配，实际 %v", expr, err)
		}
	}
}

func TestCronInvalidTimezone(t *testing.T) {
	_, err := parseCron("0 9 * * *", "Mars/Olympus")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("未知时区应报 ErrInvalid，实际 %v", err)
	}
}

func TestCronImpossibleDate(t *testing.T) {
	// 2 月没有 30 日：合法表达式但永远不匹配
	c := mustCron(t, "0 0 30 2 *", "")
	_, err := c.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if !errors.Is(err, ErrNoNext) {
		t.Fatalf("应返回 ErrNoNext，实际 %v", err)
	}
}

// 夏令时春季跳变：2026-03-08 02:00–03:00 在纽约不存在。
//
// 这个用例是回归测试：曾经 time.Date(2026,3,8,2,0,...) 被归一成 01:00，
// next 不前进，循环原地打转到上限后报「5 年内没有匹配时间」。
// 现在的行为是跳过当天（那个时刻压根不存在），顺延到第二天 02:30。
func TestCronAcrossDSTSpringForward(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	c := mustCron(t, "30 2 * * *", "America/New_York")
	from := time.Date(2026, 3, 7, 12, 0, 0, 0, ny)
	got := mustNext(t, c, from)
	want := time.Date(2026, 3, 9, 2, 30, 0, 0, ny)
	if !got.Equal(want) {
		t.Fatalf("跳变当天应被跳过：got %s want %s", got.In(ny), want)
	}
	// 之后必须继续正常推进（不能卡在跳变点附近）
	cur := got
	for i := 0; i < 3; i++ {
		nxt := mustNext(t, c, cur)
		if !nxt.After(cur) {
			t.Fatalf("跳变后没有前进：%s → %s", cur.In(ny), nxt.In(ny))
		}
		if nxt.In(ny).Hour() != 2 || nxt.In(ny).Minute() != 30 {
			t.Fatalf("跳变后命中时刻不对：%s", nxt.In(ny))
		}
		cur = nxt
	}
}

// 秋季回拨：01:00–02:00 出现两次。我们只在**第一次**触发（不重复跑），
// 与 crond（会跑两遍）不同，这是刻意的：重复跑一次日报比漏跑更烦人。
func TestCronAcrossDSTFallBack(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	c := mustCron(t, "30 1 * * *", "America/New_York")
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, ny)
	first := mustNext(t, c, from)
	if _, off := first.In(ny).Zone(); off != -4*3600 {
		t.Fatalf("应命中第一次 01:30（EDT, -4h），实际 %s", first.In(ny))
	}
	second := mustNext(t, c, first)
	if !second.Equal(time.Date(2026, 11, 2, 1, 30, 0, 0, ny)) {
		t.Fatalf("第二次应是次日 01:30，实际 %s", second.In(ny))
	}
}

func TestCronNextIsStrictlyIncreasing(t *testing.T) {
	c := mustCron(t, "*/7 * * * *", "")
	cur := at(t, "Asia/Shanghai", "2026-01-01 00:00:00")
	for i := 0; i < 200; i++ {
		next := mustNext(t, c, cur)
		if !next.After(cur) {
			t.Fatalf("第 %d 次没有前进：%s → %s", i, cur, next)
		}
		if next.Minute()%7 != 0 {
			t.Fatalf("分钟不是 7 的倍数：%s", next)
		}
		cur = next
	}
}

func mustNext(t *testing.T, c *cronSpec, from time.Time) time.Time {
	t.Helper()
	got, err := c.Next(from)
	if err != nil {
		t.Fatalf("Next(%s) 失败: %v", from, err)
	}
	return got
}
