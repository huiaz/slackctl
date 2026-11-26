package auth

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cluas/slackctl/internal/slack"
)

type strictAuthTransport func(*http.Request) (*http.Response, error)

func (f strictAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestReadOnlyStoredSelection(t *testing.T) {
	cleanup := setupTestCredentials(t)
	defer cleanup()
	t.Setenv("SLACK_TOKEN", "xoxp-environment-synthetic")
	t.Setenv("SLACK_WORKSPACE_URL", "https://other.slack.com")
	creds := &Credentials{Default: "https://other.slack.com", Workspaces: []Workspace{
		{WorkspaceURL: "https://other.slack.com", WorkspaceName: "example", Auth: slack.Auth{Type: slack.AuthStandard, Token: "xoxp-other"}},
		{WorkspaceURL: "https://example.slack.com", Auth: slack.Auth{Type: slack.AuthStandard, Token: "xoxp-" + "fixture-stored"}},
	}}
	if err := SaveCredentials(creds); err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	http.DefaultTransport = strictAuthTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer xoxp-"+"fixture-stored" {
			t.Fatal("未使用精确配置")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"url":"https://example.slack.com/","team_id":"TEXAMPLE1","user_id":"UEXAMPLE1"}`)), Header: make(http.Header)}, nil
	})
	if _, err := ResolveReadOnlyClient(t.Context(), "https://EXAMPLE.slack.com/"); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"example", "https://missing.slack.com", "http://example.slack.com", "https://example.slack.com/path", "https://org.enterprise.slack.com", "https://example.slack.com:443", "https://example.slack.com?x=1"} {
		if _, err := ResolveReadOnlyClient(t.Context(), url); err == nil {
			t.Fatalf("接受无效配置: %s", url)
		}
	}
	if calls != 1 {
		t.Fatal("无效配置触发网络")
	}
	creds.Workspaces = append(creds.Workspaces, creds.Workspaces[1])
	if err := SaveCredentials(creds); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveReadOnlyClient(t.Context(), "https://example.slack.com"); err == nil {
		t.Fatal("接受重复配置")
	}
	// 原 CLI 仍保留环境变量优先行为。
	client, ws, err := ResolveClient("example")
	if err != nil || client.WorkspaceURL() != "https://other.slack.com" || ws.Auth.Token != "xoxp-environment-synthetic" {
		t.Fatal("改变原 CLI 认证行为")
	}
}

func TestCredentialsFixtureIsolation(t *testing.T) {
	outer := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "home"} {
		t.Setenv(key, outer)
	}
	t.Run("隔离全部平台目录", func(t *testing.T) {
		cleanup := setupTestCredentials(t)
		defer cleanup()
		fixture := os.Getenv("HOME")
		if fixture == outer || fixture == "" {
			t.Fatal("没有新建隔离目录")
		}
		for _, key := range []string{"HOME", "USERPROFILE", "home"} {
			if os.Getenv(key) != fixture {
				t.Fatalf("目录变量未隔离: %s", key)
			}
		}
		actual, err := os.UserHomeDir()
		if err != nil || actual != fixture {
			t.Fatal("平台主目录未指向合成目录")
		}
		if credentialsFile() != filepath.Join(fixture, ".config", "agent-slack", "credentials.json") {
			t.Fatal("凭证路径离开合成目录")
		}
	})
	for _, key := range []string{"HOME", "USERPROFILE", "home"} {
		if os.Getenv(key) != outer {
			t.Fatalf("目录变量未恢复: %s", key)
		}
	}
}

func TestReadOnlyCanonicalStoredSelection(t *testing.T) {
	cases := []struct {
		name    string
		urls    []string
		wantErr bool
	}{
		{"仅大写主机", []string{"https://EXAMPLE.slack.com"}, false},
		{"canonical 重复", []string{"https://EXAMPLE.slack.com", "https://example.slack.com/"}, true},
		{"无效候选", []string{"http://example.slack.com", "https://example.slack.com/path", "https://example.slack.com?x=1", "https://example.slack.com:443", "https://user@example.slack.com", "https://org.enterprise.slack.com"}, true},
		{"有效与无效候选", []string{"https://example.slack.com/path", "https://EXAMPLE.slack.com"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleanup := setupTestCredentials(t)
			defer cleanup()
			creds := &Credentials{Version: 1}
			for _, url := range tc.urls {
				creds.Workspaces = append(creds.Workspaces, Workspace{WorkspaceURL: url, Auth: slack.Auth{Type: slack.AuthStandard, Token: "xoxp-synthetic"}})
			}
			if err := SaveCredentials(creds); err != nil {
				t.Fatal(err)
			}
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			calls := 0
			http.DefaultTransport = strictAuthTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != "https://slack.com/api/auth.test" {
					t.Fatal("选择配置时查询了非认证端点")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"url":"https://example.slack.com/","team_id":"TEXAMPLE1","user_id":"UEXAMPLE1"}`))}, nil
			})
			client, err := ResolveReadOnlyClient(t.Context(), "https://example.slack.com")
			if tc.wantErr {
				if err == nil || client != nil || calls != 0 {
					t.Fatal("无效或重复配置没有在请求前拒绝")
				}
			} else if err != nil || client == nil || calls != 1 {
				t.Fatalf("canonical 配置未选中: %v", err)
			}
		})
	}
}
