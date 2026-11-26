package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cluas/slackctl/internal/slack"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func connectFixture(t *testing.T, body string) *mcp.ClientSession {
	t.Helper()
	client, err := slack.NewReadOnlyClient(t.Context(), slack.Auth{Type: slack.AuthStandard, Token: "xoxp-synthetic"}, "https://example.slack.com", fixtureTransport(func(r *http.Request) (*http.Response, error) {
		out := body
		switch r.URL.Path {
		case "/api/auth.test":
			out = `{"ok":true,"url":"https://example.slack.com/","team_id":"TEXAMPLE1","user_id":"UEXAMPLE1"}`
		case "/api/search.messages":
			out = `{"ok":true,"messages":{"total":1,"matches":[{"ts":"1700000000.000001","text":"假搜索","channel":{"id":"CEXAMPLE1"}}]}}`
		case "/api/conversations.replies":
			_ = r.ParseForm()
			if r.Form.Get("ts") != "1700000000.000001" {
				t.Error("回复链接未选取线程根")
			}
			out = `{"ok":true,"messages":[{"ts":"1700000000.000001","text":"根"},{"ts":"1700000001.000002","thread_ts":"1700000000.000001","text":"回复"}]}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(out))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	st, ct := mcp.NewInMemoryTransports()
	ss, err := NewServer(client, "test").Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "synthetic-test", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
func TestSDKFourReadOnlyTools(t *testing.T) {
	cs := connectFixture(t, `{"ok":true,"messages":[{"ts":"1700000000.000001","text":"假正文","blocks":[{"type":"section","fields":[{"type":"mrkdwn","text":"假字段"}]}]}]}`)
	result, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatal("缺只读注解")
		}
		b, _ := json.Marshal(tool.InputSchema)
		if strings.Contains(string(b), `"workspace"`) || strings.Contains(string(b), `"url"`) {
			t.Fatal("暴露通用入口")
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"channel_history", "read_message", "read_thread", "search_messages"}) {
		t.Fatal(names)
	}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"search_messages", map[string]any{"query": "假数据"}, "detail_notice"},
		{"read_message", map[string]any{"channel": "CEXAMPLE1", "ts": "1700000000.000001"}, "假字段"},
		{"channel_history", map[string]any{"channel": "CEXAMPLE1", "limit": 1}, "假正文"},
		{"read_thread", map[string]any{"link": "https://example.slack.com/archives/CEXAMPLE1/p1700000001000002?thread_ts=1700000000.000001&cid=CEXAMPLE1"}, "回复"},
		{"read_message", map[string]any{"link": "https://example.slack.com/archives/CEXAMPLE1/p1700000001000002?thread_ts=1700000000.000001"}, "回复"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(got)
			var payload map[string]json.RawMessage
			if err := json.Unmarshal([]byte(got.Content[0].(*mcp.TextContent).Text), &payload); err != nil {
				t.Fatal(err)
			}
			if _, ok := payload["data"]; ok {
				t.Fatal("工具契约不应增加 data 包装层")
			}
			if tc.name == "channel_history" && string(payload["channel"]) != `"CEXAMPLE1"` {
				t.Fatal("缺少频道元数据")
			}
			if got.IsError || !strings.Contains(string(b), tc.want) || !strings.Contains(string(b), "不可信") {
				t.Fatalf("结果异常: %s", b)
			}
		})
	}
}
func TestSDKRejectArgumentsAndSize(t *testing.T) {
	cs := connectFixture(t, `{"ok":true,"messages":[{"ts":"1700000000.000001","text":"`+strings.Repeat("x", 300*1024)+`"}]}`)
	for _, args := range []map[string]any{
		{"link": "https://other.slack.com/archives/CEXAMPLE1/p1700000000000001"},
		{"link": "https://example.slack.com.evil.org/archives/CEXAMPLE1/p1700000000000001"},
		{"channel": "#general", "ts": "1700000000.000001"},
		{"channel": "CEXAMPLE1", "ts": "1700000000.000001", "workspace": "https://other.slack.com"},
		{"channel": "CEXAMPLE1", "ts": "1700000000.000001", "url": "https://example.org"},
		{"channel": "CEXAMPLE1", "ts": "1700000000.000001", "method": "POST"},
		{"channel": "CEXAMPLE1", "ts": "1700000000.000001\n"},
		{"link": "https://example.slack.com/archives/CEXAMPLE1/p1700000000000001", "channel": "CEXAMPLE1"},
		{"link": "https://example.slack.com/archives/CEXAMPLE1/p1700000000000001?thread_ts=1700000000.000001&thread_ts=1700000000.000002"},
		{"channel": "CEXAMPLE1", "ts": nil},
	} {
		got, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "read_message", Arguments: args})
		if err != nil || !got.IsError {
			t.Fatalf("接受无效参数: %v %v", got, err)
		}
	}
	for _, limit := range []any{0, 51, -1, 1.5, nil} {
		got, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "channel_history", Arguments: map[string]any{"channel": "CEXAMPLE1", "limit": limit}})
		if err != nil || !got.IsError {
			t.Fatalf("接受无效条数: %v %v", got, err)
		}
	}
	got, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "channel_history", Arguments: map[string]any{"channel": "CEXAMPLE1"}})
	if err != nil || !got.IsError || strings.Contains(got.Content[0].(*mcp.TextContent).Text, strings.Repeat("x", 100)) {
		t.Fatal("未限制工具结果")
	}
}
