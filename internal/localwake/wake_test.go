package localwake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cluas/slackctl/internal/mcpevents"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{Path: path, Bot: "example-bot", Channels: []string{"CEXAMPLE1"}, Mentions: true, DMs: true}
}

func TestSimulatedExecutableReceivesFixedHomePayload(t *testing.T) {
	dir := t.TempDir()
	source := `package main
import("encoding/json";"os";"path/filepath")
func main(){
 p,_:=os.Executable();a:=os.Args[1:];b,err:=os.ReadFile(a[len(a)-1]);if err!=nil{os.Exit(2)}
 data,_:=json.Marshal(map[string]any{"args":a,"body":json.RawMessage(b),"environment":os.Getenv("ORCA_ENVIRONMENT"),"proxy":os.Getenv("HTTPS_PROXY"),"pairing":os.Getenv("ORCA_PAIRING_CODE")})
 if os.WriteFile(filepath.Join(filepath.Dir(p),"receipt.json"),data,0600)!=nil{os.Exit(3)}
}
`
	input := filepath.Join(dir, "receiver.go")
	binary := filepath.Join(dir, "receiver")
	if err := os.WriteFile(input, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", binary, input)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("模拟接收端构建失败: %v %s", err, out)
	}
	cfg := testConfig(t)
	cfg.Path = binary
	w, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCA_PAIRING_CODE", "synthetic-secret")
	t.Setenv("HTTPS_PROXY", "https://example.invalid")
	e := mcpevents.Event{Type: "message", Channel: "CEXAMPLE1", TS: "1700000000.000001"}
	if err = w.deliver(t.Context(), payload([]Reference{reference(e)}, 1)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Args        []string `json:"args"`
		Body        Payload  `json:"body"`
		Environment string   `json:"environment"`
		Proxy       string   `json:"proxy"`
		Pairing     string   `json:"pairing"`
	}
	if json.Unmarshal(data, &got) != nil || got.Environment != "local" || got.Proxy != "" || got.Pairing != "" || got.Body.Count != 1 || slices.Contains(got.Args, "--chat") || !slices.Contains(got.Args, "bot:example-bot") {
		t.Fatalf("模拟接收端断言失败: %s", data)
	}
}

func TestLocalFixedHomeRoutingAndMinimalPayload(t *testing.T) {
	w, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	var file string
	w.run = func(ctx context.Context, path string, args, env []string) error {
		if path != w.config.Path || !slices.Equal(args[:len(args)-1], []string{"orchestration", "send", "--environment", "local", "--to", "bot:example-bot", "--from", "bot:example-bot", "--type", "handoff", "--subject", "Slack 事件通知", "--body-file"}) {
			t.Fatalf("固定 Home Chat 参数错误: %v", args)
		}
		if strings.Contains(strings.Join(env, " "), "secret") || strings.Contains(strings.Join(args, " "), "--chat") {
			t.Fatal("泄漏环境或指定了其他聊天")
		}
		file = args[len(args)-1]
		stat, err := os.Stat(file)
		if err != nil || stat.Mode().Perm() != 0600 {
			t.Fatal("临时文件权限错误")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"隐藏正文", "UOTHER1", "https://", "xoxp-"} {
			if strings.Contains(string(data), bad) {
				t.Fatal("传递了禁止的内容")
			}
		}
		var p Payload
		if json.Unmarshal(data, &p) != nil || p.Count != 1 || p.Events[0].Channel != "CEXAMPLE1" {
			t.Fatal("结构化引用错误")
		}
		return nil
	}
	e := mcpevents.Event{Type: "message", Channel: "CEXAMPLE1", TS: "1700000000.000001", User: "UOTHER1", Text: "隐藏正文 https://example.org xoxp-synthetic", IsMention: true}
	if err = w.deliver(t.Context(), payload([]Reference{reference(e)}, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("临时载荷未删除")
	}
}

func TestWakeFiltersAggregationDedupAndBoundedRetry(t *testing.T) {
	w, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	e := mcpevents.Event{Type: "message", Channel: "CEXAMPLE1", TS: "1700000000.000001", User: "UOTHER1"}
	if p := w.collect(e, now); p != nil {
		t.Fatal("告警不应立即唤醒")
	}
	w.collect(e, now)
	e.TS = "1700000001.000001"
	w.collect(e, now)
	if got := w.flush(now.Add(179 * time.Second)); len(got) != 0 {
		t.Fatal("合并窗口过早结束")
	}
	got := w.flush(now.Add(180 * time.Second))
	if len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("合并或去重错误: %+v", got)
	}
	for _, bad := range []mcpevents.Event{
		{Type: "message", Channel: "COTHER01", TS: e.TS},
		{Type: "message", Channel: "DEXAMPLE1", TS: e.TS, User: "UOTHER1", IsDM: true, BotID: "BEXAMPLE1"},
		{Type: "message", Channel: "DEXAMPLE1", TS: e.TS, User: "UOTHER1", IsDM: true, AppID: "AEXAMPLE1"},
		{Type: "message", Channel: "DEXAMPLE1", TS: e.TS, User: "UEXAMPLE1", IsDM: true, FromSelf: true},
		{Type: "message", Channel: "DEXAMPLE1", TS: e.TS, IsDM: true},
	} {
		if p := w.collect(bad, now); p != nil {
			t.Fatal("过滤失败")
		}
	}
	e.Channel = "DEXAMPLE1"
	e.IsDM = true
	if p := w.collect(e, now); p == nil {
		t.Fatal("人的私信没有立即唤醒")
	}
	calls := 0
	w.run = func(context.Context, string, []string, []string) error { calls++; return errors.New("隐藏输出") }
	w.sleep = func(context.Context, time.Duration) bool { return true }
	if err := w.deliver(t.Context(), got[0]); err == nil || strings.Contains(err.Error(), "隐藏") || calls != 3 {
		t.Fatal("重试或脱敏错误")
	}
}

func TestWakeRejectsUntrustedConfiguration(t *testing.T) {
	for _, c := range []Config{{Path: "orca", Bot: "example"}, {Path: "https://example.org/orca", Bot: "example"}, {Path: "/usr/bin/true", Bot: "example --chat x"}, {Path: "/usr/bin/true", Bot: "example", Channels: []string{"*"}}} {
		if _, err := New(c); err == nil {
			t.Fatal("接受无效本机配置")
		}
	}
}
