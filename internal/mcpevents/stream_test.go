package mcpevents

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQueueCursorDedupAndProjection(t *testing.T) {
	s := New(nil)
	var disabled *Stream
	if r, err := disabled.List("", 20); err != nil || r.Status != "disabled" {
		t.Fatal("未启用状态错误")
	}
	r, err := s.List("", 20)
	if err != nil || len(r.Events) != 0 || r.Status != "connecting" {
		t.Fatal("空结果状态错误")
	}
	cursor := r.Cursor
	data := []byte(`{"type":"message","team":"TEXAMPLE1","channel":"CEXAMPLE1","ts":"1700000000.000001","user":"UOTHER1","text":"合成正文","blocks":[{"type":"section","text":{"type":"plain_text","text":"字段"}}],"token":"隐藏"}`)
	if err := s.accept(data, "TEXAMPLE1", "UEXAMPLE1"); err != nil {
		t.Fatal(err)
	}
	_ = s.accept(data, "TEXAMPLE1", "UEXAMPLE1")
	r, err = s.List(cursor, 20)
	if err != nil || len(r.Events) != 1 || !strings.Contains(r.Events[0].Text, "合成") {
		t.Fatalf("去重或投影失败: %+v %v", r, err)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "隐藏") || !strings.Contains(string(b), "字段") {
		t.Fatal("协议字段泄漏或丢失 Blocks")
	}
	for i := range QueueCapacity + 1 {
		e := Event{Type: "message", Channel: "CEXAMPLE1", TS: "1700000000.000001", Text: strings.Repeat("x", i+1)}
		s.push(e)
	}
	r, err = s.List(cursor, 20)
	if err != nil || !r.CursorExpired || len(r.Events) != 0 {
		t.Fatal("过期游标未显式报告")
	}
	if _, err = s.List("无效游标", 20); err == nil {
		t.Fatal("接受伪造游标")
	}
	s.setStatus("disconnected")
	r, _ = s.List("", 1)
	if r.Status != "disconnected" || len(r.Events) != 1 || !r.HasMore {
		t.Fatal("断连缓存状态错误")
	}
}

func TestEventTeamAndProtocolBoundary(t *testing.T) {
	s := New(nil)
	for _, data := range []string{`{"type":"message","team":"TOTHER1"}`, `{"type":"hello","team_id":"TOTHER1"}`, `{"type":"message","team":null}`} {
		if s.accept([]byte(data), "TEXAMPLE1", "UEXAMPLE1") == nil {
			t.Fatal("没有拒绝不匹配团队")
		}
	}
	for _, data := range []string{`{"type":"presence_change","user":"UOTHER1","token":"隐藏"}`, `{"type":"reconnect_url","url":"wss://wss-primary.slack.com/秘密"}`} {
		if err := s.accept([]byte(data), "TEXAMPLE1", "UEXAMPLE1"); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := s.List("", 20)
	if len(r.Events) != 0 {
		t.Fatal("输出了无关原始协议")
	}
}

func TestRejectsMalformedEventReferencesAndBoundsPayload(t *testing.T) {
	s := New(nil)
	for _, fields := range []string{
		`"ts":"1700000000..00001"`,
		`"ts":"1700000000.000001","thread_ts":"隐藏"`,
		`"ts":"1700000000.000001","user":"隐藏"`,
	} {
		_ = s.accept([]byte(`{"type":"message","channel":"CEXAMPLE1",`+fields+`}`), "TEXAMPLE1", "UEXAMPLE1")
	}
	r, _ := s.List("", 20)
	if len(r.Events) != 0 {
		t.Fatal("接受畸形业务引用")
	}
	data := []byte(`{"type":"message","channel":"CEXAMPLE1","ts":"1700000000.000001","text":"` + strings.Repeat("文", 3000) + `","blocks":[{"type":"section","text":"` + strings.Repeat("x", 20000) + `"}]}`)
	if err := s.accept(data, "TEXAMPLE1", "UEXAMPLE1"); err != nil {
		t.Fatal(err)
	}
	r, _ = s.List("", 20)
	if len(r.Events) != 1 || !r.Events[0].Truncated || len(r.Events[0].Blocks) != 0 || len([]rune(r.Events[0].Text)) != 2000 {
		t.Fatal("载荷上限不正确")
	}
}
