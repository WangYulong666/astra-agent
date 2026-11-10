// astra-relay 是公网中继：让手机在任意网络下连上家里电脑的控制面。
//
// 设备端主动出站连上来，手机连 /t/{device}/...，relay 只做字节转发。
// 用法见 docs/relay.md。
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/WangYulong666/astra-agent/internal/relay"
)

func main() {
	lg := log.New(os.Stderr, "", log.LstdFlags)
	err := run(os.Args[1:], os.Getenv, lg, os.Stdout)
	switch {
	case errors.Is(err, flag.ErrHelp):
		usage(os.Stdout)
		return
	case err != nil:
		lg.Printf("astra-relay: %v", err)
		lg.Printf("用 -h 查看参数说明")
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `astra-relay：公网中继（让手机在外网连上家里电脑）

用法：
  astra-relay -addr :9443 -token <共享密钥> [-tls-cert c.pem -tls-key k.pem] [-public-url URL]

参数：
  -addr string          监听地址，默认 :9443
  -public-url string    对外根地址，仅用于日志/横幅，例 https://relay.example.com
  -token string         设备注册令牌（也可用环境变量 ASTRA_RELAY_TOKEN）
  -tls-cert string      TLS 证书，必须与 -tls-key 同时给出
  -tls-key string       TLS 私钥
  -max-streams int      单设备并发流上限，默认 128
  -idle-timeout dur     隧道空闲断开阈值，默认 90s

安全提示：-token 为空即「调试模式」，只接受 127.0.0.1 连入，且严禁放在反代之后。
`)
}

// options 是解析后的运行参数（纯数据，便于单测）。
type options struct {
	addr        string
	publicURL   string
	token       string
	tlsCert     string
	tlsKey      string
	maxStreams  int
	idleTimeout time.Duration
}

func (o options) tlsEnabled() bool { return o.tlsCert != "" || o.tlsKey != "" }
func (o options) debug() bool      { return o.token == "" }

// parseOptions 只做「参数 → 配置」的纯转换与校验，不碰网络、不读证书文件。
func parseOptions(args []string, getenv func(string) string) (options, error) {
	fs := flag.NewFlagSet("astra-relay", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var o options
	fs.StringVar(&o.addr, "addr", ":9443", "监听地址，例 :9443 或 127.0.0.1:9443")
	fs.StringVar(&o.publicURL, "public-url", "", "对外访问的根地址（只用于日志），例 https://relay.example.com")
	fs.StringVar(&o.token, "token", "", "设备注册令牌（也可用 ASTRA_RELAY_TOKEN）")
	fs.StringVar(&o.tlsCert, "tls-cert", "", "TLS 证书文件；与 -tls-key 同时给出才启用 TLS")
	fs.StringVar(&o.tlsKey, "tls-key", "", "TLS 私钥文件")
	fs.IntVar(&o.maxStreams, "max-streams", 128, "单设备并发流上限")
	fs.DurationVar(&o.idleTimeout, "idle-timeout", 90*time.Second, "隧道空闲断开阈值")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("不认识的参数: %s", strings.Join(fs.Args(), " "))
	}

	if strings.TrimSpace(o.token) == "" && getenv != nil {
		// 环境变量优先度低于 flag：只有没显式给 -token 时才回退。
		o.token = strings.TrimSpace(getenv("ASTRA_RELAY_TOKEN"))
	}

	switch {
	case strings.TrimSpace(o.addr) == "":
		return options{}, errors.New("-addr 不能为空")
	case (o.tlsCert == "") != (o.tlsKey == ""):
		return options{}, errors.New("-tls-cert 与 -tls-key 必须成对给出")
	case o.maxStreams <= 0:
		return options{}, errors.New("-max-streams 必须大于 0")
	case o.idleTimeout < 10*time.Second:
		return options{}, errors.New("-idle-timeout 不能小于 10s（太小会把正常长连接也掐掉）")
	}
	return o, nil
}

func run(args []string, getenv func(string) string, lg *log.Logger, out io.Writer) error {
	o, err := parseOptions(args, getenv)
	if err != nil {
		return err
	}

	cfg := relay.ServerConfig{
		Addr:        o.addr,
		Token:       o.token,
		PublicURL:   o.publicURL,
		Logger:      lg,
		MaxStreams:  o.maxStreams,
		IdleTimeout: o.idleTimeout,
	}
	if o.tlsEnabled() {
		cert, err := tls.LoadX509KeyPair(o.tlsCert, o.tlsKey)
		if err != nil {
			return fmt.Errorf("加载证书失败: %w", err)
		}
		cfg.TLS = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	srv, err := relay.OpenServer(cfg)
	if err != nil {
		return err
	}

	banner(out, o, srv.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	lg.Printf("收到退出信号，正在优雅关闭…")
	return srv.Close()
}

// banner 把「现在到底是个什么形态」一次说清：运维第一眼要看的就是这几行。
func banner(out io.Writer, o options, realAddr string) {
	scheme, ws := "http", "ws"
	if o.tlsEnabled() {
		scheme, ws = "https", "wss"
	}
	fmt.Fprintf(out, "\n  astra-relay\n")
	fmt.Fprintf(out, "  监听地址   %s（实际 %s）\n", o.addr, realAddr)
	fmt.Fprintf(out, "  TLS        %v（%s）\n", o.tlsEnabled(), scheme)
	fmt.Fprintf(out, "  调试模式   %v\n", o.debug())
	fmt.Fprintf(out, "  公网 URL   %s\n", orDash(o.publicURL))
	fmt.Fprintf(out, "  并发流上限 %d / 设备，空闲超时 %s\n", o.maxStreams, o.idleTimeout)
	if o.publicURL != "" {
		host := strings.TrimPrefix(strings.TrimPrefix(o.publicURL, "https://"), "http://")
		host = strings.TrimSuffix(host, "/")
		fmt.Fprintf(out, "  手机访问   %s/t/<device>/...\n", strings.TrimSuffix(o.publicURL, "/"))
		// 设备端该用 ws 还是 wss，取决于手机/设备看到的那个入口，而不是 relay 自己
		// 有没有证书（反代终结 TLS 时 relay 是明文，但对外仍然是 wss）。
		if strings.HasPrefix(o.publicURL, "https://") {
			ws = "wss"
		} else if strings.HasPrefix(o.publicURL, "http://") {
			ws = "ws"
		}
		fmt.Fprintf(out, "  设备配置   ASTRA_RELAY_URL=%s://%s\n", ws, host)
	}
	fmt.Fprintln(out)

	if o.debug() {
		fmt.Fprintln(out, "  ############################################################")
		fmt.Fprintln(out, "  #  ⚠  危险：未设置 -token（ASTRA_RELAY_TOKEN 也为空）")
		fmt.Fprintln(out, "  #  此模式只接受 127.0.0.1 连入。若前面挂了反代，而 relay")
		fmt.Fprintln(out, "  #  看到的来源是回环，那么任何人只要能把请求送到这个端口，")
		fmt.Fprintln(out, "  #  就能注册任意设备 id 并接管 /t/<device>/ 的全部流量。")
		fmt.Fprintln(out, "  #  生产环境必须设置 -token。")
		fmt.Fprintln(out, "  ############################################################")
		fmt.Fprintln(out)
	}
	if !o.tlsEnabled() && !o.debug() {
		fmt.Fprintln(out, "  提示：未启用 TLS，仅应在「反代已终结 TLS、本机明文回源」时使用。")
		fmt.Fprintln(out)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
