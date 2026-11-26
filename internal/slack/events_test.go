package slack

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestStrictRTMIdentityAndEndpoint(t *testing.T) {
	for _, body := range []string{
		`{"ok":true,"url":"wss://wss-primary.slack.com/link","team":{"id":"TOTHER1"},"self":{"id":"UEXAMPLE1"}}`,
		`{"ok":true,"url":"wss://wss-primary.slack.com/link","self":{"id":"UEXAMPLE1"}}`,
		`{"ok":true,"url":"wss://wss-primary.slack.com/link","team":{"id":"TEXAMPLE1"},"self":{"id":"UOTHER1"}}`,
		`{"ok":true,"url":"wss://evil.example/link","team":{"id":"TEXAMPLE1"},"self":{"id":"UEXAMPLE1"}}`,
		`{"ok":false,"error":"not_allowed_token_type","secret":"隐藏"}`,
	} {
		calls := 0
		c := strictFixture(t, func(r *http.Request) *http.Response {
			calls++
			if r.URL.Path != "/api/rtm.connect" {
				t.Fatal("握手调用了其他端点")
			}
			return jsonReply(body)
		})
		if _, err := c.ConnectEvents(t.Context()); err == nil || strings.Contains(err.Error(), "隐藏") || calls != 1 {
			t.Fatalf("握手未失败关闭: %v", err)
		}
	}
	for _, u := range []string{"ws://wss-primary.slack.com/x", "wss://wss-primary.slack.com.evil.example/x", "wss://user@wss-primary.slack.com/x", "wss://wss-primary.slack.com:443/x", "wss://wss-primary.slack.com/x#token", "wss://127.0.0.1/x"} {
		if ValidEventURL(u) {
			t.Fatal("接受不可信连接地址")
		}
	}
}

func TestRTMTransientFailureRemainsRetryable(t *testing.T) {
	c := strictFixture(t, func(*http.Request) *http.Response { r := jsonReply("合成暂时失败"); r.StatusCode = 503; return r })
	_, err := c.ConnectEvents(t.Context())
	if err == nil || errors.Is(err, ErrEventUnsupported) || errors.Is(err, ErrEventIdentity) {
		t.Fatal("临时连接错误被当成永久不支持")
	}
}
