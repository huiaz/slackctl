package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/cluas/slackctl/internal/mcpevents"
	"github.com/cluas/slackctl/internal/slack"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Options struct {
	EnableWrites bool
	EnableEvents bool
	Events       *mcpevents.Stream
}

const approvalNotice = "默认 dry_run 仅预览；显式 dry_run:false 才执行。工具参数不能代替宿主的用户审批，宿主必须按其审批规则确认目标、内容和动作。写入结果不明时不自动重试。"

func registerOptional(server *mcp.Server, client *slack.ReadOnlyClient, opt Options) {
	if opt.EnableWrites {
		for _, def := range []toolDefinition{
			{"send_message", "向明确频道 ID 发送消息。", []string{"channel", "text", "blocks", "dry_run"}, []string{"channel", "text"}},
			{"reply_thread", "按同域链接或频道和根 ts 回复，永不广播到主时间线。", []string{"link", "channel", "ts", "text", "blocks", "dry_run"}, []string{"text"}},
			{"add_reaction", "按同域链接或频道和精确 ts 添加表情；已存在视为成功。", []string{"link", "channel", "ts", "name", "dry_run"}, []string{"name"}},
		} {
			s := schema(def)
			props := s["properties"].(map[string]any)
			for _, key := range def.fields {
				switch key {
				case "text":
					props[key] = map[string]any{"type": "string", "minLength": 1, "maxLength": slack.ActionMaxTextBytes, "description": "Slack mrkdwn 文本，最多 12000 字节；也作为 Blocks 的回退正文"}
				case "blocks":
					props[key] = map[string]any{"type": "array", "minItems": 1, "maxItems": 50, "items": map[string]any{"type": "object"}, "description": "原生 Slack Block Kit，JSON 总计最多 16000 字节"}
				case "name":
					props[key] = map[string]any{"type": "string", "pattern": "^[a-zA-Z0-9_+-]{1,100}$", "description": "不含冒号的 emoji 名称"}
				case "dry_run":
					props[key] = map[string]any{"type": "boolean", "default": true, "description": approvalNotice}
				}
			}
			server.AddTool(&mcp.Tool{Name: def.name, Description: def.description + approvalNotice, InputSchema: s, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				a, err := parseAction(def, req.Params.Arguments)
				if err != nil {
					return failure(err), nil
				}
				ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				r, err := client.Act(ctx, a)
				if err != nil {
					return failure(err), nil
				}
				return optionalResult(map[string]any{"result": r, "notice": approvalNotice}), nil
			})
		}
	}
	if opt.EnableEvents {
		server.AddTool(&mcp.Tool{Name: "list_events", Description: "读取已经实时接收的内存事件。cursor 为空读取最早保留事件；使用返回 cursor 续读，过期时确认丢失后用返回 cursor 重置。读取本身不触发唤醒或 Slack 写入。", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"cursor": map[string]any{"type": "string", "maxLength": 64}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "default": 20}}, "additionalProperties": false}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var a struct {
				Cursor string `json:"cursor"`
				Limit  int    `json:"limit"`
			}
			a.Limit = 20
			if !validOptionalFields(req.Params.Arguments, []string{"cursor", "limit"}, nil) || json.Unmarshal(req.Params.Arguments, &a) != nil {
				return failure(errArguments), nil
			}
			r, err := opt.Events.List(a.Cursor, a.Limit)
			if err != nil {
				return failure(err), nil
			}
			return optionalResult(r), nil
		})
	}
}

func validOptionalFields(raw json.RawMessage, allowed, required []string) bool {
	if len(raw) > 48*1024 {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return false
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		token, err := dec.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return false
		}
		if _, exists := fields[key]; exists {
			return false
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return false
		}
		fields[key] = value
	}
	if end, err := dec.Token(); err != nil || end != json.Delim('}') {
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		return false
	}
	set := map[string]bool{}
	for _, key := range allowed {
		set[key] = true
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	for key, value := range fields {
		if !set[key] || string(value) == "null" {
			return false
		}
	}
	return true
}

func parseAction(def toolDefinition, raw json.RawMessage) (slack.Action, error) {
	var a slack.Action
	if !validOptionalFields(raw, def.fields, def.required) || json.Unmarshal(raw, &a) != nil {
		return a, errArguments
	}
	a.Kind = def.name
	return a, nil
}

func optionalResult(data any) *mcp.CallToolResult {
	b, err := json.Marshal(data)
	if err != nil {
		return failure(errors.New("工具结果编码失败。"))
	}
	r := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
	encoded, err := json.Marshal(r)
	if err != nil || len(encoded) > maxResultBytes {
		return failure(errors.New("工具结果超限，请减少读取条数。"))
	}
	return r
}
