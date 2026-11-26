package cli

import (
	"strings"
	"testing"
)

func TestMCPCommandRegistered(t *testing.T) {
	cmd, _, err := NewRootCmd().Find([]string{"mcp"})
	if err != nil || cmd.Name() != "mcp" {
		t.Fatal("缺少原生 MCP 命令")
	}
}

func TestMCPHelpDocumentsNonEnterpriseScope(t *testing.T) {
	cmd, _, err := NewRootCmd().Find([]string{"mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd.Long, "Enterprise Grid") || !strings.Contains(cmd.Long, "单工作区安装") {
		t.Fatal("帮助未说明 Grid 支持边界")
	}
}

func TestMCPExplicitCapabilities(t *testing.T) {
	cmd := newMCPCmd()
	for _, name := range []string{"enable-writes", "enable-events"} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil || flag.DefValue != "false" {
			t.Errorf("缺少默认关闭的独立开关 %s", name)
		}
	}
	listen, _, err := cmd.Find([]string{"listen"})
	if err != nil || listen == cmd || listen.Name() != "listen" {
		t.Fatal("缺少独立常驻入口")
	}
}
