package mcpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cluas/slackctl/internal/mcpevents"
	"github.com/cluas/slackctl/internal/slack"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSDKOptionalActionsAndEvents(t *testing.T) {
	for _, writes := range []bool{false, true} {
		for _, events := range []bool{false, true} {
			t.Run(strings.Join([]string{string(rune('0' + boolInt(writes))), string(rune('0' + boolInt(events)))}, ""), func(t *testing.T) {
				var calls atomic.Int32
				client, err := slack.NewReadOnlyClient(t.Context(), slack.Auth{Type: slack.AuthStandard, Token: "xoxp-synthetic"}, "https://example.slack.com", fixtureTransport(func(r *http.Request) (*http.Response, error) {
					body := `{"ok":true,"url":"https://example.slack.com/","team_id":"TEXAMPLE1","user_id":"UEXAMPLE1"}`
					if r.URL.Path != "/api/auth.test" {
						calls.Add(1)
						_ = r.ParseForm()
						if r.URL.Path != "/api/chat.postMessage" || r.Form.Get("text") != "*合成*" {
							t.Error("不正确写入")
						}
						body = `{"ok":true,"channel":"CEXAMPLE1","ts":"1700000000.000001"}`
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				var stream *mcpevents.Stream
				if events {
					stream = mcpevents.New(client)
				}
				st, ct := mcp.NewInMemoryTransports()
				ss, err := NewServer(client, "test", Options{EnableWrites: writes, EnableEvents: events, Events: stream}).Connect(t.Context(), st, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer ss.Close()
				cs, err := mcp.NewClient(&mcp.Implementation{Name: "合成客户端", Version: "1"}, nil).Connect(t.Context(), ct, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer cs.Close()
				listed, err := cs.ListTools(t.Context(), nil)
				if err != nil || len(listed.Tools) != 4+3*boolInt(writes)+boolInt(events) {
					t.Fatalf("开关耦合或列表错误: %v", err)
				}
				if !writes {
					return
				}
				for _, args := range []map[string]any{{"channel": "CEXAMPLE1", "text": "*合成*"}, {"channel": "CEXAMPLE1", "text": "*合成*", "dry_run": true}} {
					r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: args})
					if err != nil || r.IsError || calls.Load() != 0 {
						t.Fatal("预览发送了消息")
					}
				}
				for _, args := range []map[string]any{{"channel": "CEXAMPLE1", "text": "*合成*", "dry_run": nil}, {"channel": "CEXAMPLE1", "text": "*合成*", "dry_run": "false"}, {"channel": "CEXAMPLE1", "text": "*合成*", "method": "chat.delete"}} {
					r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: args})
					if err != nil || !r.IsError || calls.Load() != 0 {
						t.Fatal("接受异常参数")
					}
				}
				r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"channel": "CEXAMPLE1", "text": "*合成*", "dry_run": false}})
				if err != nil || r.IsError || calls.Load() != 1 {
					t.Fatal("实际写入失败")
				}
				if events {
					r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_events", Arguments: map[string]any{}})
					b, _ := json.Marshal(r)
					if err != nil || r.IsError || !strings.Contains(string(b), "connecting") {
						t.Fatal("事件空读取错误")
					}
				}
			})
		}
	}
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestActionRejectsDuplicateExecutionSwitch(t *testing.T) {
	def := toolDefinition{name: "send_message", fields: []string{"channel", "text", "dry_run"}, required: []string{"channel", "text"}}
	if _, err := parseAction(def, json.RawMessage(`{"channel":"CEXAMPLE1","text":"合成","dry_run":true,"dry_run":false}`)); err == nil {
		t.Fatal("重复执行开关产生歧义")
	}
}
