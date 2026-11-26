package auth

import (
	"context"
	"errors"

	"github.com/cluas/slackctl/internal/slack"
)

// ResolveReadOnlyClient 只读取精确匹配的已存储配置，不读取环境认证或浏览器。
func ResolveReadOnlyClient(ctx context.Context, workspace string) (*slack.ReadOnlyClient, error) {
	canonical, err := slack.CanonicalReadOnlyWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	creds, err := LoadCredentials()
	if err != nil {
		return nil, errors.New("无法读取已保存的 Slack 配置。")
	}
	var selected *Workspace
	for _, ws := range creds.Workspaces {
		candidate, err := slack.CanonicalReadOnlyWorkspace(ws.WorkspaceURL)
		if err != nil || candidate != canonical {
			continue
		}
		if selected != nil {
			return nil, errors.New("目标工作区存在重复配置。")
		}
		selected = &ws
	}
	if selected == nil {
		return nil, errors.New("未找到与完整工作区地址精确匹配的配置。")
	}
	return slack.NewReadOnlyClient(ctx, selected.Auth, canonical, nil)
}
