// 手动 e2e 探针：连真实控制面的远程桌面 WS，收真帧、发真输入。
//
//	go run ./tools/wscheck.go http://127.0.0.1:7788 <token> <sessionID>
//
// 为什么要它：单元测试只能用假 capture，而「帧到底是不是真画面」只有连真
// 服务、真抓屏才能证明。本机实测一次收到 24 帧 / 1.19MB，首帧 48KB JPEG。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/coder/websocket"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Println("用法: wscheck <server> <token> <sessionID>")
		os.Exit(2)
	}
	server, token, sid := os.Args[1], os.Args[2], os.Args[3]
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	// 1. 换一次性票据（WS 不能带 header，服务端复用 SSE 票据机制）
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server+"/v1/auth/sse-ticket", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		fmt.Println("票据请求失败:", err)
		os.Exit(1)
	}
	var tk struct {
		Ticket string `json:"ticket"`
	}
	if err := decode(resp, &tk); err != nil || tk.Ticket == "" {
		fmt.Printf("票据响应异常 (status=%d err=%v)\n", resp.StatusCode, err)
		os.Exit(1)
	}
	fmt.Println("① 票据 OK，长度", len(tk.Ticket))

	// 2. 连 WS
	wsURL := fmt.Sprintf("%s/v1/remote/sessions/%s/stream?ticket=%s",
		replaceScheme(server), sid, tk.Ticket)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		fmt.Println("WS 连接失败:", err)
		os.Exit(1)
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	conn.SetReadLimit(8 << 20)
	fmt.Println("② WS 已连上")

	// 3. 收帧：hello 是 text，画面是 binary
	frames, bytes, hello := 0, 0, ""
	deadline := time.Now().Add(8 * time.Second)
	sentInput := false
	for time.Now().Before(deadline) {
		typ, data, rerr := conn.Read(ctx)
		if rerr != nil {
			fmt.Println("读结束:", rerr)
			break
		}
		switch typ {
		case websocket.MessageText:
			s := string(data)
			if hello == "" {
				hello = s
			}
			fmt.Println("   text:", trunc(s, 160))
		case websocket.MessageBinary:
			frames++
			bytes += len(data)
			if frames == 1 {
				_ = os.WriteFile("/tmp/rd-frame1.jpg", data, 0o644)
				fmt.Printf("   第 1 帧: %d 字节，JPEG 头 %x\n", len(data), data[:3])
			}
			if frames == 3 {
				_ = os.WriteFile("/tmp/rd-frame3.jpg", data, 0o644)
			}
			if frames >= 3 && !sentInput {
				sentInput = true
				msg := `{"type":"input","op":"click","x":0.5,"y":0.5}`
				_ = conn.Write(ctx, websocket.MessageText, []byte(msg))
				fmt.Println("   已发送点击事件，等注入结果")
			}
		}
	}
	fmt.Printf("③ 收到 %d 帧 / 共 %d 字节\n", frames, bytes)
	if frames < 2 {
		fmt.Println("✗ 帧数不足，画面链路有问题")
		os.Exit(1)
	}
	fmt.Println("✓ 画面链路通")
}

func replaceScheme(s string) string {
	if len(s) > 5 && s[:5] == "https" {
		return "wss" + s[5:]
	}
	if len(s) > 4 && s[:4] == "http" {
		return "ws" + s[4:]
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func decode(resp *http.Response, v any) error {
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}
