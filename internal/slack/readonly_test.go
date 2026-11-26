package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type readOnlyTransport func(*http.Request) (*http.Response, error)

func (f readOnlyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonReply(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

const testIdentity = `{"ok":true,"url":"https://example.slack.com/","team_id":"TEXAMPLE1","user_id":"UEXAMPLE1","is_enterprise_install":false}`

func strictFixture(t *testing.T, handler func(*http.Request) *http.Response) *ReadOnlyClient {
	t.Helper()
	c, err := NewReadOnlyClient(t.Context(), Auth{Type: AuthStandard, Token: "xoxp-synthetic"}, "https://example.slack.com", readOnlyTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "slack.com" || r.URL.Scheme != "https" || r.Method != "POST" {
			t.Fatal("请求离开固定 API")
		}
		if r.Header.Get("Authorization") != "Bearer xoxp-synthetic" {
			t.Fatal("认证身份改变")
		}
		if r.URL.Path == "/api/auth.test" {
			return jsonReply(testIdentity), nil
		}
		return handler(r), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestReadOnlyIdentity(t *testing.T) {
	for _, body := range []string{
		strings.ReplaceAll(testIdentity, "example.slack.com", "other.slack.com"),
		`{"ok":true,"url":"https://example.slack.com/","user_id":"UEXAMPLE1"}`,
		strings.ReplaceAll(testIdentity, `false`, `true`),
		`{"ok":false,"error":"enterprise_is_restricted","token":"secret"}`,
	} {
		calls := 0
		_, err := NewReadOnlyClient(t.Context(), Auth{Type: AuthStandard, Token: "xoxp-synthetic"}, "https://example.slack.com", readOnlyTransport(func(r *http.Request) (*http.Response, error) { calls++; return jsonReply(body), nil }))
		if err == nil || calls != 1 || strings.Contains(err.Error(), "secret") {
			t.Fatalf("未拒绝身份或发生回退: %v 次数 %d", err, calls)
		}
	}
	for _, a := range []Auth{{Type: AuthBrowser, XoxcToken: "synthetic"}, {Type: AuthStandard, Token: "xoxp-synthetic", EnterpriseURL: "https://org.enterprise.slack.com"}, {Type: AuthStandard, Token: "xoxe-synthetic"}} {
		_, err := NewReadOnlyClient(t.Context(), a, "https://example.slack.com", readOnlyTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("不支持的认证发起请求")
			return nil, nil
		}))
		if err == nil {
			t.Fatal("接受不支持认证")
		}
	}
}
func TestReadOnlyExactAndBlocks(t *testing.T) {
	body := `{"ok":true,"messages":[{"ts":"1700000000.000001","text":"合成正文","blocks":[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"link","url":"https://example.org/top"}]}]}],"attachments":[{"fields":[{"title":"字段","value":"链接"}],"blocks":[{"type":"actions","elements":[{"type":"button","url":"https://example.org/action"}]}]}],"files":[{"id":"FEXAMPLE1","title":"假文件","url_private":"https://example.org/file","token":"secret"}],"token":"secret"}]}`
	c := strictFixture(t, func(r *http.Request) *http.Response {
		_ = r.ParseForm()
		if r.Form.Get("latest") != "1700000000.000001" || r.Form.Get("oldest") != "1700000000.000001" {
			t.Fatal("未限定精确时间戳")
		}
		return jsonReply(body)
	})
	msg, err := c.ReadMessage(t.Context(), "CEXAMPLE1", "1700000000.000001", "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(msg)
	for _, s := range []string{"rich_text", "https://example.org/top", "https://example.org/action", "fields", "假文件", "permalink"} {
		if !strings.Contains(string(b), s) {
			t.Fatalf("丢失 %s: %s", s, b)
		}
	}
	if strings.Contains(string(b), "secret") {
		t.Fatal("未裁剪非消息字段")
	}
	c = strictFixture(t, func(*http.Request) *http.Response {
		return jsonReply(strings.ReplaceAll(body, "1700000000.000001", "1699999999.000001"))
	})
	if _, err = c.ReadMessage(t.Context(), "CEXAMPLE1", "1700000000.000001", ""); err == nil {
		t.Fatal("返回邻近消息")
	}
}
func TestReadOnlyThreadCursorAndReply(t *testing.T) {
	calls := 0
	c := strictFixture(t, func(r *http.Request) *http.Response {
		calls++
		_ = r.ParseForm()
		if r.URL.Path != "/api/conversations.replies" || r.Form.Get("ts") != "1700000000.000001" {
			t.Fatal("线程根错误")
		}
		if calls%2 == 1 {
			return jsonReply(`{"ok":true,"messages":[{"ts":"1700000000.000001"}],"has_more":true,"response_metadata":{"next_cursor":"next"}}`)
		}
		if r.Form.Get("cursor") != "next" {
			t.Fatal("未传递原版游标")
		}
		return jsonReply(`{"ok":true,"messages":[{"ts":"1700000001.000002","thread_ts":"1700000000.000001","text":"回复"}]}`)
	})
	msg, err := c.ReadMessage(t.Context(), "CEXAMPLE1", "1700000001.000002", "1700000000.000001")
	if err != nil || msg.TS != "1700000001.000002" || !strings.Contains(msg.Permalink, "thread_ts=1700000000.000001") {
		t.Fatalf("回复不精确: %v %v", msg, err)
	}
	page, err := c.ReadThread(t.Context(), "CEXAMPLE1", "1700000000.000001", 2)
	if err != nil || len(page.Messages) != 2 {
		t.Fatalf("分页失败: %v", err)
	}
}
func TestReadOnlyFailures(t *testing.T) {
	for _, body := range []string{`{"ok":false,"error":"missing_scope","secret":"认证"}`, `{"ok":false,"error":"enterprise_is_restricted"}`, strings.Repeat("x", 1024*1024+1), `{"ok":true} garbage`} {
		calls := 0
		c := strictFixture(t, func(*http.Request) *http.Response { calls++; return jsonReply(body) })
		_, err := c.History(context.Background(), "CEXAMPLE1", 1)
		if err == nil || calls != 1 || strings.Contains(err.Error(), "认证\"") {
			t.Fatalf("错误边界失败: %v", err)
		}
	}
	c := strictFixture(t, func(*http.Request) *http.Response {
		r := jsonReply("secret")
		r.StatusCode = 302
		r.Header.Set("Location", "https://other.slack.com/api/conversations.history")
		return r
	})
	if _, err := c.History(t.Context(), "CEXAMPLE1", 1); err == nil {
		t.Fatal("跟随重定向")
	}
}

func TestReadOnlyTruncationMetadata(t *testing.T) {
	calls := 0
	c := strictFixture(t, func(r *http.Request) *http.Response {
		calls++
		if calls == 1 {
			return jsonReply(`{"ok":true,"messages":[{"ts":"1700000000.000001"}],"response_metadata":{"next_cursor":"next"}}`)
		}
		return jsonReply(`{"ok":true,"messages":[{"ts":"1700000001.000001"},{"ts":"1700000002.000001"}]}`)
	})
	page, err := c.History(t.Context(), "CEXAMPLE1", 2)
	if err != nil || len(page.Messages) != 2 || !page.HasMore {
		t.Fatalf("截断未明确标记: %+v %v", page, err)
	}
}

func TestReadOnlySafeTransportErrorsAndCancellation(t *testing.T) {
	calls := 0
	transport := readOnlyTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == "/api/auth.test" {
			return jsonReply(testIdentity), nil
		}
		<-r.Context().Done()
		return nil, fmt.Errorf("原始错误包含 Authorization 和 synthetic-secret: %w", r.Context().Err())
	})
	c, err := NewReadOnlyClient(t.Context(), Auth{Type: AuthStandard, Token: "xoxb-synthetic"}, "https://example.slack.com", transport)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.History(ctx, "CEXAMPLE1", 1)
	if err == nil || strings.Contains(err.Error(), "synthetic-secret") || calls != 2 {
		t.Fatalf("原始错误泄露或回退: %v", err)
	}
}

func TestReadOnlyRejectsMalformedTargetsAndThread(t *testing.T) {
	c := strictFixture(t, func(*http.Request) *http.Response {
		return jsonReply(`{"ok":true,"messages":[{"ts":"1700000009.000001","thread_ts":"1700000009.000001"}]}`)
	})
	for _, link := range []string{
		"https://example.slack.com:443/archives/CEXAMPLE1/p1700000000000001",
		"https://example.slack.com/archives/CEXAMPLE1/p1700000000000001?cid=COTHER01",
		"https://user@example.slack.com/archives/CEXAMPLE1/p1700000000000001",
		"https://example.slack.com/archives/CEXAMPLE1/p1700000000000001/trailing",
		"https://example.slack.com/archives/CEXAMPLE1/p1700000000000001#fragment",
		"http://example.slack.com/archives/CEXAMPLE1/p1700000000000001",
		"https://example.slack.com/archives/CEXAMPLE1/p1700000000000001?thread_ts=",
	} {
		if _, err := c.ParseTarget(link, "", "", ""); err == nil {
			t.Fatalf("接受无效链接: %s", link)
		}
	}
	if _, err := c.ReadThread(t.Context(), "CEXAMPLE1", "1700000000.000001", 20); err == nil {
		t.Fatal("接受错误线程根")
	}
	if _, err := c.ReadMessage(t.Context(), "CEXAMPLE1", "1700000001.000001", "1700000000.000001"); err == nil {
		t.Fatal("错误线程命中回复")
	}
}

func TestReadOnlyHTTPDenialsAndCursorLoop(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		calls := 0
		c := strictFixture(t, func(*http.Request) *http.Response {
			calls++
			r := jsonReply("synthetic-secret")
			r.StatusCode = status
			return r
		})
		if _, err := c.History(t.Context(), "CEXAMPLE1", 20); err == nil || strings.Contains(err.Error(), "synthetic-secret") || calls != 1 {
			t.Fatalf("HTTP 错误不安全: %v", err)
		}
	}
	calls := 0
	c := strictFixture(t, func(*http.Request) *http.Response {
		calls++
		return jsonReply(`{"ok":true,"messages":[],"response_metadata":{"next_cursor":"repeat"}}`)
	})
	if _, err := c.History(t.Context(), "CEXAMPLE1", 20); err == nil || calls != 2 {
		t.Fatalf("循环游标未终止: %v %d", err, calls)
	}
}

func TestReadOnlySearchPreservesReplyAndBlocks(t *testing.T) {
	c := strictFixture(t, func(r *http.Request) *http.Response {
		if r.URL.Path != "/api/search.messages" {
			t.Fatal("搜索调用错误端点")
		}
		return jsonReply(`{"ok":true,"messages":{"total":1,"matches":[{"channel":{"id":"CEXAMPLE1"},"ts":"1700000001.000002","text":"搜索回复","permalink":"https://example.slack.com/archives/CEXAMPLE1/p1700000001000002?thread_ts=1700000000.000001&cid=CEXAMPLE1","blocks":[{"type":"section","fields":[{"type":"mrkdwn","text":"假字段"}]}],"attachments":[{"blocks":[{"type":"actions","elements":[{"type":"button","url":"https://example.org/action"}]}]}]}]}}`)
	})
	result, err := c.Search(t.Context(), "合成查询", 1)
	if err != nil {
		t.Fatal(err)
	}
	msg := result.Messages[0]
	if msg.ThreadTS != "1700000000.000001" || !strings.Contains(msg.Permalink, "thread_ts=1700000000.000001") {
		t.Fatal("搜索回复链接丢失线程根")
	}
	data, _ := json.Marshal(result)
	for _, want := range []string{"假字段", "https://example.org/action", "detail_notice"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("搜索丢失 %s", want)
		}
	}
}

func TestReadOnlyNonEnterpriseIdentity(t *testing.T) {
	cases := []struct {
		name, token, body string
		wantErr           bool
	}{
		{"普通用户无安装标志", "xoxp-synthetic", `{"ok":true,"url":"https://example.slack.com/","team_id":"TEXAMPLE1","user_id":"UEXAMPLE1"}`, false},
		{"普通机器人无安装标志", "xoxb-synthetic", `{"ok":true,"url":"https://example.slack.com/","team_id":"TEXAMPLE1","user_id":"UEXAMPLE1","bot_id":"BEXAMPLE1"}`, false},
		{"Grid 无安装标志", "xoxp-synthetic", `{"ok":true,"url":"https://example.slack.com/","team_id":"TEXAMPLE1","user_id":"UEXAMPLE1","enterprise_id":"EEXAMPLE1"}`, true},
		{"Grid 空安装标志", "xoxp-synthetic", `{"ok":true,"url":"https://example.slack.com/","team_id":"TEXAMPLE1","user_id":"UEXAMPLE1","enterprise_id":"EEXAMPLE1","is_enterprise_install":null}`, true},
		{"Grid 单工作区安装", "xoxb-synthetic", `{"ok":true,"url":"https://example.slack.com/","team_id":"TEXAMPLE1","user_id":"UEXAMPLE1","enterprise_id":"EEXAMPLE1","is_enterprise_install":false}`, true},
		{"组织没有团队", "xoxp-synthetic", `{"ok":true,"url":"https://example.slack.com/","user_id":"UEXAMPLE1","enterprise_id":"EEXAMPLE1"}`, true},
		{"空企业字段不作无归属证明", "xoxp-synthetic", `{"ok":true,"url":"https://example.slack.com/","team_id":"TEXAMPLE1","user_id":"UEXAMPLE1","enterprise_id":null}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client, err := NewReadOnlyClient(t.Context(), Auth{Type: AuthStandard, Token: tc.token}, "https://example.slack.com", readOnlyTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/api/auth.test" {
					t.Fatal("认证阶段查询了消息")
				}
				return jsonReply(tc.body), nil
			}))
			if calls != 1 {
				t.Fatal("身份核验发生重试")
			}
			if tc.wantErr {
				if err == nil || client != nil {
					t.Fatal("未拒绝企业或不可证实的身份")
				}
			} else if err != nil || client == nil {
				t.Fatalf("普通身份被拒绝: %v", err)
			}
		})
	}
}

func TestReadOnlyReplyStopsOnMatchingPage(t *testing.T) {
	calls := 0
	c := strictFixture(t, func(r *http.Request) *http.Response {
		calls++
		if calls > 1 {
			t.Fatal("目标已在第一页命中，不得查询后页")
		}
		_ = r.ParseForm()
		if r.URL.Path != "/api/conversations.replies" || r.Form.Get("ts") != "1700000000.000001" {
			t.Fatal("线程请求参数错误")
		}
		return jsonReply(`{"ok":true,"messages":[{"ts":"1700000000.000001"},{"ts":"1700000001.000002","thread_ts":"1700000000.000001","text":"精确回复"}],"response_metadata":{"next_cursor":"forbidden-next"}}`)
	})
	msg, err := c.ReadMessage(t.Context(), "CEXAMPLE1", "1700000001.000002", "1700000000.000001")
	if err != nil || msg == nil || msg.TS != "1700000001.000002" || calls != 1 {
		t.Fatalf("首次命中未返回: %v", err)
	}
}

func TestReadOnlyReplyChecksMatchingPageBoundaries(t *testing.T) {
	for _, field := range []string{`"channel_id":"COTHER01"`, `"thread_ts":"1700000099.000001"`} {
		c := strictFixture(t, func(*http.Request) *http.Response {
			return jsonReply(`{"ok":true,"messages":[{"ts":"1700000000.000001"},{"ts":"1700000001.000002",` + field + `}]}`)
		})
		if _, err := c.ReadMessage(t.Context(), "CEXAMPLE1", "1700000001.000002", "1700000000.000001"); err == nil {
			t.Fatal("目标消息绕过频道或线程检查")
		}
	}
}

func TestReadOnlyReplyRetainsWindowAndPageLimit(t *testing.T) {
	t.Run("目标超出前五十条", func(t *testing.T) {
		messages := []map[string]string{{"ts": "1700000000.000001"}}
		for i := 1; i <= 50; i++ {
			messages = append(messages, map[string]string{"ts": fmt.Sprintf("1700000001.%06d", i), "thread_ts": "1700000000.000001"})
		}
		data, err := json.Marshal(map[string]any{"ok": true, "messages": messages, "response_metadata": map[string]string{"next_cursor": "forbidden-next"}})
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		c := strictFixture(t, func(*http.Request) *http.Response {
			calls++
			if calls > 1 {
				t.Fatal("超过条数窗口还继续分页")
			}
			return jsonReply(string(data))
		})
		if _, err = c.ReadMessage(t.Context(), "CEXAMPLE1", "1700000001.000050", "1700000000.000001"); err == nil {
			t.Fatal("错误接受窗口外回复")
		}
	})
	t.Run("最多十页", func(t *testing.T) {
		calls := 0
		c := strictFixture(t, func(*http.Request) *http.Response {
			calls++
			if calls > 10 {
				t.Fatal("超过页数上限")
			}
			ts := "1700000000.000001"
			if calls > 1 {
				ts = fmt.Sprintf("1700000001.%06d", calls)
			}
			return jsonReply(fmt.Sprintf(`{"ok":true,"messages":[{"ts":%q,"thread_ts":"1700000000.000001"}],"response_metadata":{"next_cursor":%q}}`, ts, fmt.Sprint(calls)))
		})
		if _, err := c.ReadMessage(t.Context(), "CEXAMPLE1", "1700000002.000001", "1700000000.000001"); err == nil || calls != 10 {
			t.Fatal("页数上限未保留")
		}
	})
}

func TestReadOnlyMessageAuthorAndSearchChannel(t *testing.T) {
	message := `{"ts":"1700000000.000001","username":"合成机器人","text":"合成正文","blocks":[{"type":"section","fields":[{"type":"mrkdwn","text":"字段"}]}],"attachments":[{"blocks":[{"type":"actions","elements":[{"type":"button","url":"https://example.org/action"}]}]}]}`
	c := strictFixture(t, func(r *http.Request) *http.Response {
		switch r.URL.Path {
		case "/api/conversations.history":
			return jsonReply(`{"ok":true,"messages":[` + message + `]}`)
		case "/api/search.messages":
			match := strings.TrimSuffix(message, "}") + `,"channel":{"id":"CEXAMPLE1","name":"example-channel"}}`
			return jsonReply(`{"ok":true,"messages":{"total":1,"matches":[` + match + `]}}`)
		default:
			t.Fatal("不应查询用户资料")
		}
		return nil
	})
	msg, err := c.ReadMessage(t.Context(), "CEXAMPLE1", "1700000000.000001", "")
	if err != nil {
		t.Fatal(err)
	}
	search, err := c.Search(t.Context(), "合成查询", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []any{msg, search.Messages[0]} {
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["user_name"]) != `"合成机器人"` {
			t.Fatal("丢失仅由 username 提供的作者")
		}
		if _, ok := fields["username"]; ok {
			t.Fatal("没有映射为统一的 user_name 字段")
		}
		for _, want := range []string{"fields", "https://example.org/action"} {
			if !strings.Contains(string(data), want) {
				t.Fatal("新增元数据破坏了 Blocks")
			}
		}
	}
	data, _ := json.Marshal(search.Messages[0])
	if !strings.Contains(string(data), `"channel_name":"example-channel"`) {
		t.Fatal("搜索丢失频道名称")
	}
}

func TestReadOnlySearchRequiresMetadata(t *testing.T) {
	cases := []struct {
		name, body string
		wantErr    bool
	}{
		{"有效空结果", `{"ok":true,"messages":{"total":0,"matches":[]}}`, false},
		{"缺少消息对象", `{"ok":true}`, true},
		{"空消息对象", `{"ok":true,"messages":null}`, true},
		{"缺少全部元数据", `{"ok":true,"messages":{}}`, true},
		{"缺少总数", `{"ok":true,"messages":{"matches":[]}}`, true},
		{"空总数", `{"ok":true,"messages":{"total":null,"matches":[]}}`, true},
		{"缺少匹配列表", `{"ok":true,"messages":{"total":0}}`, true},
		{"空匹配列表值", `{"ok":true,"messages":{"total":0,"matches":null}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := strictFixture(t, func(*http.Request) *http.Response { return jsonReply(tc.body) })
			result, err := c.Search(t.Context(), "合成查询", 20)
			if tc.wantErr {
				if err != errReadOnlyResponse || result != nil {
					t.Fatalf("畸形响应被当作空结果: %v", err)
				}
			} else if err != nil || result == nil || result.Total != 0 || result.Messages == nil || len(result.Messages) != 0 {
				t.Fatalf("合法空结果被拒绝: %v", err)
			}
		})
	}
}
