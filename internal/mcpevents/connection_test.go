package mcpevents

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cluas/slackctl/internal/slack"
	"github.com/coder/websocket"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mockClient(t *testing.T, ws *httptest.Server) *slack.ReadOnlyClient {
	t.Helper()
	target, _ := url.Parse(ws.URL)
	c, err := slack.NewReadOnlyClient(t.Context(), slack.Auth{Type: slack.AuthStandard, Token: "xoxp-synthetic"}, "https://example.slack.com", transport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer xoxp-synthetic" {
			t.Error("事件认证被改变")
		}
		if r.URL.Host == "wss-primary.slack.com" {
			r = r.Clone(r.Context())
			u := *r.URL
			u.Scheme = target.Scheme
			u.Host = target.Host
			r.URL = &u
			return ws.Client().Transport.RoundTrip(r)
		}
		if r.URL.Host != "slack.com" {
			t.Fatal("模拟传输阻止未知主机")
		}
		body := `{"ok":true,"url":"https://example.slack.com/","team_id":"TEXAMPLE1","user_id":"UEXAMPLE1"}`
		if r.URL.Path == "/api/rtm.connect" {
			body = `{"ok":true,"url":"wss://wss-primary.slack.com/合成连接","team":{"id":"TEXAMPLE1"},"self":{"id":"UEXAMPLE1"}}`
		} else if r.URL.Path != "/api/auth.test" {
			t.Fatal("事件消费调用了其他 API")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRealtimeReconnectWithoutPollingAndBoundedHeartbeat(t *testing.T) {
	var connections, pings atomic.Int32
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		n := connections.Add(1)
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","team_id":"TEXAMPLE1"}`))
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"message","channel":"CEXAMPLE1","user":"UOTHER1","ts":"1700000000.000001","text":"合成"}`))
		if n == 1 {
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"goodbye"}`))
			return
		}
		for {
			_, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var p struct {
				ID int `json:"id"`
			}
			_ = json.Unmarshal(b, &p)
			pings.Add(1)
			data, _ := json.Marshal(map[string]any{"type": "pong", "reply_to": p.ID})
			_ = c.Write(r.Context(), websocket.MessageText, data)
		}
	}))
	defer ws.Close()
	s := New(mockClient(t, ws))
	s.pingInterval = 10 * time.Millisecond
	s.sleep = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
	var sinkCalls atomic.Int32
	s.SetSink(func(Event) { sinkCalls.Add(1) })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for pings.Load() < 3 {
		select {
		case <-deadline:
			t.Fatal("没有实时重连或心跳")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	if connections.Load() != 2 || sinkCalls.Load() != 1 || pings.Load() > 10 {
		t.Fatalf("去重、独立实时消费或心跳节流错误：连接 %d，事件 %d，心跳 %d", connections.Load(), sinkCalls.Load(), pings.Load())
	}
	p, _ := s.List("", 20)
	if len(p.Events) != 1 || p.Status != "stopped" {
		t.Fatal("关闭状态或缓存错误")
	}
}

func TestWebsocketRedirectAndForeignTeamFailClosed(t *testing.T) {
	for _, redirect := range []bool{true, false} {
		t.Run(map[bool]string{true: "重定向", false: "异工作区"}[redirect], func(t *testing.T) {
			ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if redirect {
					http.Redirect(w, r, "https://evil.example/隐藏", 302)
					return
				}
				c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
				if err != nil {
					return
				}
				defer c.CloseNow()
				_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","team":"TOTHER1"}`))
				_, _, _ = c.Read(r.Context())
			}))
			defer ws.Close()
			s := New(mockClient(t, ws))
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := s.Start(ctx)
			if err == nil {
				err = s.Wait()
			}
			if err == nil || strings.Contains(err.Error(), "evil.example") {
				t.Fatal("未安全失败")
			}
			p, _ := s.List("", 20)
			if len(p.Events) != 0 || p.Status != "failed" {
				t.Fatal("失败事件进入业务队列")
			}
		})
	}
}
