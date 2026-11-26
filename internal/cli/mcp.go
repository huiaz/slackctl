package cli

import (
	"errors"
	"os"
	"os/signal"

	"github.com/cluas/slackctl/internal/auth"
	"github.com/cluas/slackctl/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

func newMCPCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:           "mcp --workspace https://example.slack.com",
		Short:         "通过标准输入输出提供固定工作区的四个只读 MCP 工具",
		Long:          "通过官方 Go MCP SDK 提供只读 stdio 服务。仅使用精确工作区配置的标准令牌，启动时核验实际身份。仅支持普通非 Enterprise Grid 工作区，Grid 中的单工作区安装也暂不支持。不支持浏览器和组织级认证；无事件唤醒。",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		// 不让继承的调试和附件开关影响 MCP 协议或认证。
		PersistentPreRun: func(*cobra.Command, []string) {},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()
			client, err := auth.ResolveReadOnlyClient(ctx, workspace)
			if err != nil {
				return err
			}
			if err = mcpserver.NewServer(client, version).Run(ctx, &mcp.StdioTransport{MaxLineLength: 64 * 1024}); err != nil && ctx.Err() == nil {
				return errors.New("MCP 标准输入输出会话结束异常。")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "完整工作区 HTTPS 地址（必填，禁止模糊选择）")
	_ = cmd.MarkFlagRequired("workspace")
	return cmd
}
