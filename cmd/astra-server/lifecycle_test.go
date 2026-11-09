package main

import (
	"context"
	"encoding/json"
	"github.com/WangYulong666/astra-agent/internal/app"
	"io"
	"log"
	"net/http"
	"testing"
	"time"
)

func TestExplicitZeroDisablesLifecycleInRunningServer(t *testing.T) {
	// 验证 CLI 到真实 app 装配及 HTTP 状态，不只检查辅助函数的返回值。
	srv, err := app.New(app.Config{Addr: "127.0.0.1:0", Provider: "local", Agent: "shell", Policy: "ask", DataDir: t.TempDir(), StoreKind: "file", Logger: log.New(io.Discard, "", 0), Sweep: lifecycleDuration(0), Idle: lifecycleDuration(0), MaxAge: lifecycleDuration(0)})
	if err != nil {
		t.Fatal(err)
	}
	url, err := srv.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = srv.Close()
	})
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get("http://" + url + "/v1/admin/lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var view struct {
		Policy struct {
			Enabled bool `json:"enabled"`
			Sweep   int  `json:"sweepIntervalSeconds"`
			Idle    int  `json:"idleTimeoutSeconds"`
			Age     int  `json:"maxAgeSeconds"`
		} `json:"policy"`
	}
	if err := json.NewDecoder(res.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || view.Policy.Enabled || view.Policy.Sweep > 0 || view.Policy.Idle > 0 || view.Policy.Age > 0 {
		t.Fatalf("explicit zero became enabled: HTTP %d %+v", res.StatusCode, view)
	}
}
