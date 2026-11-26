package slack

import (
	"net/http"
	"strings"
	"testing"
)

func TestActionPreviewAndExactPayload(t *testing.T) {
	calls := 0
	c := strictFixture(t, func(r *http.Request) *http.Response {
		calls++
		_ = r.ParseForm()
		if r.URL.Path != "/api/chat.postMessage" || r.Form.Get("channel") != "CEXAMPLE1" || r.Form.Get("thread_ts") != "1700000000.000001" || r.Form.Get("reply_broadcast") != "false" || r.Form.Get("text") != "*合成正文*" {
			t.Errorf("写入目标或富文本语义错误: %v", r.Form)
		}
		if !strings.Contains(r.Form.Get("blocks"), "rich_text") {
			t.Fatal("原生富文本 Blocks 没有传递")
		}
		return jsonReply(`{"ok":true,"channel":"CEXAMPLE1","ts":"1700000002.000001","secret":"不输出"}`)
	})
	a := Action{Kind: "reply_thread", Link: "https://example.slack.com/archives/CEXAMPLE1/p1700000001000002?thread_ts=1700000000.000001", Text: "*合成正文*"}
	a.Blocks = []byte(`[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"合成正文"}]}]}]`)
	r, err := c.Act(t.Context(), a)
	if err != nil || !r.DryRun || r.ThreadTS != "1700000000.000001" || calls != 0 {
		t.Fatalf("默认预览失败: %+v %v", r, err)
	}
	a.DryRun = new(bool)
	r, err = c.Act(t.Context(), a)
	if err != nil || r.DryRun || r.TS != "1700000002.000001" || calls != 1 {
		t.Fatalf("显式执行失败: %+v %v", r, err)
	}
}

func TestActionRejectsUnsafeTargets(t *testing.T) {
	c := strictFixture(t, func(*http.Request) *http.Response { t.Fatal("无效动作不应联网"); return nil })
	for _, a := range []Action{
		{Kind: "send_message", Channel: "#general", Text: "合成"},
		{Kind: "send_message", Channel: "CEXAMPLE1", TS: "1700000000.000001", Text: "合成"},
		{Kind: "reply_thread", Link: "https://other.slack.com/archives/CEXAMPLE1/p1700000000000001", Text: "合成"},
		{Kind: "add_reaction", Channel: "CEXAMPLE1", TS: "1700000000.000001", Name: ":wave:"},
		{Kind: "send_message", Channel: "CEXAMPLE1", Text: strings.Repeat("x", 12001)},
		{Kind: "chat.delete", Channel: "CEXAMPLE1", Text: "合成"},
	} {
		if _, err := c.Act(t.Context(), a); err == nil {
			t.Fatalf("接受无效动作: %s", a.Kind)
		}
	}
}

func TestReactionExactAndAlreadyReacted(t *testing.T) {
	c := strictFixture(t, func(r *http.Request) *http.Response {
		_ = r.ParseForm()
		if r.URL.Path != "/api/reactions.add" || r.Form.Get("timestamp") != "1700000001.000002" || r.Form.Get("name") != "wave" {
			t.Fatal("reaction 没有使用精确回复消息")
		}
		return jsonReply(`{"ok":false,"error":"already_reacted"}`)
	})
	r, err := c.Act(t.Context(), Action{Kind: "add_reaction", Link: "https://example.slack.com/archives/CEXAMPLE1/p1700000001000002?thread_ts=1700000000.000001", Name: "wave", DryRun: new(bool)})
	if err != nil || !r.AlreadyReacted {
		t.Fatalf("重复表情未按幂等成功处理: %+v %v", r, err)
	}
}

func TestActionUnknownOutcomeNeverRetries(t *testing.T) {
	calls := 0
	c := strictFixture(t, func(*http.Request) *http.Response {
		calls++
		r := jsonReply("包含秘密的错误")
		r.StatusCode = 500
		return r
	})
	_, err := c.Act(t.Context(), Action{Kind: "send_message", Channel: "CEXAMPLE1", Text: "合成", DryRun: new(bool)})
	if err == nil || calls != 1 || !strings.Contains(err.Error(), "不明") || strings.Contains(err.Error(), "秘密") {
		t.Fatalf("错误泄漏或自动重试: %v", err)
	}
}

func TestFixedAPIRejectsUnlistedMethods(t *testing.T) {
	c := strictFixture(t, func(*http.Request) *http.Response { t.Fatal("调用未列入允许列表的方法"); return nil })
	var out any
	if err := c.fixedAPI(t.Context(), "chat.delete", nil, &out); err == nil {
		t.Fatal("接受任意 Slack 方法")
	}
}
