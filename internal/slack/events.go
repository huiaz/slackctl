package slack

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

var ErrEventIdentity = errors.New("实时事件身份或工作区无法核实，已停止接收。")
var ErrEventUnsupported = errors.New("当前标准凭证无法建立已核验的 RTM 连接；请检查 Slack 的令牌类型、权限与 RTM 支持范围。")

// Identity 只返回已经 auth.test 核验的团队及身份，不暴露认证材料。
func (c *ReadOnlyClient) Identity() (team, user string) { return c.teamID, c.userID }

// ValidEventURL 只信任明确列出的 Slack WSS 主机，禁止端口、用户信息及片段。
func ValidEventURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || u.Scheme != "wss" || u.User != nil || u.Fragment != "" || u.Port() != "" {
		return false
	}
	switch u.Host {
	case "wss-primary.slack.com", "wss-backup.slack.com", "wss-mobile.slack.com":
		return true
	}
	return false
}

// ConnectEvents 每次重新从固定 API 获取连接地址并重新核验团队与身份。
// 不使用旧客户端的浏览器、组织回退、任意端点或 reconnect_url。
func (c *ReadOnlyClient) ConnectEvents(ctx context.Context) (*websocket.Conn, error) {
	var info struct {
		URL  string `json:"url"`
		Team struct {
			ID string `json:"id"`
		} `json:"team"`
		Self struct {
			ID string `json:"id"`
		} `json:"self"`
	}
	if err := c.fixedAPI(ctx, "rtm.connect", nil, &info); err != nil {
		if errors.Is(err, errReadOnlyAuth) || errors.Is(err, ErrEventUnsupported) {
			return nil, ErrEventUnsupported
		}
		return nil, errors.New("实时连接请求暂时失败，地址与认证已隐藏。")
	}
	if info.Team.ID != c.teamID || info.Self.ID != c.userID || c.teamID == "" || c.userID == "" || !ValidEventURL(info.URL) {
		return nil, ErrEventIdentity
	}
	client := *c.http
	client.Timeout = 0
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.token)
	h.Set("Origin", "https://app.slack.com")
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, info.URL, &websocket.DialOptions{HTTPClient: &client, HTTPHeader: h})
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, errors.New("实时连接建立失败，未跟随重定向；地址及认证已隐藏。")
	}
	conn.SetReadLimit(64 * 1024)
	return conn, nil
}
