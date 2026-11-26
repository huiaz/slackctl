package cli

import (
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cluas/slackctl/internal/auth"
	"github.com/cluas/slackctl/internal/localwake"
	"github.com/cluas/slackctl/internal/mcpevents"
	"github.com/cluas/slackctl/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

func newMCPCmd() *cobra.Command {
	var workspace string
	var writes, events, wakeEnabled bool
	var wakeConfig localwake.Config
	cmd := &cobra.Command{
		Use:           "mcp --workspace https://example.slack.com",
		Short:         "通过标准输入输出提供固定工作区的四个只读 MCP 工具",
		Long:          "通过官方 Go MCP SDK 提供 stdio 服务，默认仅四个只读工具。写入、实时事件与本机唤醒均需显式启用。仅使用精确工作区配置的标准令牌，启动时核验实际身份。仅支持普通非 Enterprise Grid 工作区，Grid 中的单工作区安装也暂不支持。不支持浏览器和组织级认证。",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		// 不让继承的调试和附件开关影响 MCP 协议或认证。
		PersistentPreRun: func(*cobra.Command, []string) {},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMCP(cmd, workspace, writes, events, wakeEnabled, wakeConfig, false)
		},
	}
	cmd.PersistentFlags().StringVar(&workspace, "workspace", "", "完整工作区 HTTPS 地址（必填，禁止模糊选择）")
	_ = cmd.MarkPersistentFlagRequired("workspace")
	cmd.Flags().BoolVar(&writes, "enable-writes", false, "显式注册三个写工具；默认仅预览，参数不代替宿主审批")
	cmd.Flags().BoolVar(&events, "enable-events", false, "显式实时接收事件并注册 list_events，与写入开关独立")
	cmd.PersistentFlags().BoolVar(&wakeEnabled, "enable-local-wake", false, "显式启用固定本机 Orca IPC 唤醒，需要实时事件入口")
	cmd.PersistentFlags().StringVar(&wakeConfig.Path, "orca-path", "", "部署者信任的本机 Orca CLI 绝对路径")
	cmd.PersistentFlags().StringVar(&wakeConfig.Bot, "orca-bot", "", "固定本机 bot 名称，同一 bot 的 Home Chat")
	cmd.PersistentFlags().StringSliceVar(&wakeConfig.Channels, "wake-channels", nil, "精确频道 ID 白名单，不接受通配符")
	cmd.PersistentFlags().BoolVar(&wakeConfig.Mentions, "wake-mentions", false, "白名单频道中的直接提及立即唤醒")
	cmd.PersistentFlags().BoolVar(&wakeConfig.DMs, "wake-dms", false, "人的直接私信立即唤醒，跳过自身和 bot/app")
	cmd.PersistentFlags().DurationVar(&wakeConfig.Window, "wake-window", 180*time.Second, "白名单告警合并窗口，范围一秒至一小时")
	cmd.AddCommand(&cobra.Command{Use: "listen", Short: "独立常驻接收严格实时事件，可选本机唤醒，需要外部进程管理", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return runMCP(cmd, workspace, false, true, wakeEnabled, wakeConfig, true)
	}})
	return cmd
}

func runMCP(cmd *cobra.Command, workspace string, writes, events, wakeEnabled bool, wakeConfig localwake.Config, standalone bool) error {
	if wakeEnabled && !events {
		return errors.New("本机唤醒需要 --enable-events 或 mcp listen 常驻入口。")
	}
	var wake *localwake.Wake
	if wakeEnabled {
		var err error
		wake, err = localwake.New(wakeConfig)
		if err != nil {
			return err
		}
	}
	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	client, err := auth.ResolveReadOnlyClient(ctx, workspace)
	if err != nil {
		return err
	}
	var stream *mcpevents.Stream
	eventErr := make(chan error, 1)
	if events {
		stream = mcpevents.New(client)
		stream.SetStatusSink(func(status string) {
			labels := map[string]string{"connected": "已连接", "disconnected": "已断连，等待重连；断线期间事件不会重放", "failed": "失败并停止", "stopped": "已停止"}
			if label, ok := labels[status]; ok {
				cmd.PrintErrln("实时事件状态：" + label + "。")
			}
		})
		if wake != nil {
			stream.SetSink(wake.Offer)
			workers.Go(func() { wake.Run(ctx, cmd.ErrOrStderr()) })
		}
		if err = stream.Start(ctx); err != nil {
			return err
		}
		if standalone {
			return stream.Wait()
		}
		// 断连可在 list_events 中读取；身份失配等终止错误会取消会话。
		workers.Go(func() {
			if err := stream.Wait(); err != nil {
				eventErr <- err
				cmd.PrintErrln("实时事件接收已停止：身份、权限或协议核验失败。")
				cancel()
			}
		})
	}
	if err = mcpserver.NewServer(client, version, mcpserver.Options{EnableWrites: writes, EnableEvents: events, Events: stream}).Run(ctx, &mcp.StdioTransport{MaxLineLength: 64 * 1024}); err != nil && ctx.Err() == nil {
		return errors.New("MCP 标准输入输出会话结束异常。")
	}
	select {
	case err := <-eventErr:
		return err
	default:
	}
	return nil
}
