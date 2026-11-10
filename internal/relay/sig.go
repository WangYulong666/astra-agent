package relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// 设备注册票据（ticket）的实现细节：
//
//	sig = hex(HMAC-SHA256(token, device + "|" + ts))
//
// ts 是 unix 秒。票据的有效窗口由 slogSkew 控制——窗口存在的意义不是「保密」
// （token 本身是共享密钥，没有它算不出 sig），而是让**抓到的 URL 不能长期复用**：
// 隧道 URL 会出现在 nginx access log、浏览器历史、代理日志里。
const slogSkew = 120 * time.Second

var errTicketExpired = errors.New("relay: 票据已过期")

// deviceIDPattern 限制设备 id 字符集：它会进 URL 路径、日志和 map key，
// 放开会产生路径歧义（"/"、".."）和日志注入（换行）。
var deviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// signTicket 计算设备注册票据。
func signTicket(token, device, ts string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(device + "|" + ts))
	return hex.EncodeToString(mac.Sum(nil))
}

// verifyTicket 校验票据。now 显式传入，便于测试时间偏差。
func verifyTicket(token, device, ts, sig string, now time.Time) error {
	if !deviceIDPattern.MatchString(device) {
		return ErrUnauthorized
	}
	if ts == "" || sig == "" {
		return ErrUnauthorized
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrUnauthorized
	}
	skew := now.Sub(time.Unix(sec, 0))
	if skew > slogSkew || skew < -slogSkew {
		return errTicketExpired
	}
	want := signTicket(token, device, ts)
	// 定长比较，避免按字节提前返回泄漏前缀。
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return ErrUnauthorized
	}
	return nil
}

// ticketPath 拼出设备端要连的隧道路径（含查询串）。
func ticketPath(device, token string, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	v := url.Values{}
	v.Set("device", device)
	v.Set("ts", ts)
	if token != "" {
		v.Set("sig", signTicket(token, device, ts))
	}
	return "/relay/tunnel?" + v.Encode()
}
