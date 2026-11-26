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
