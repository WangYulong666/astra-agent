// astra-relay 是公网中继：让手机在任意网络下连上家里电脑的控制面。
//
// 设备端主动出站连上来，手机连 /t/{device}/...，relay 只做字节转发。
// 用法见 docs/remote-desktop.md 的「外网连接」一节。
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "astra-relay: 实现未接入（由 W1 填充）")
	os.Exit(1)
}
