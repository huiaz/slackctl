package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const ActionMaxTextBytes = 12000
const ActionMaxBlocksBytes = 16000

var emojiPattern = regexp.MustCompile(`^[a-zA-Z0-9_+-]{1,100}$`)
var errAlreadyReacted = errors.New("已存在相同表情。")

// Action 的零值 DryRun 表示预览；只有显式 false 才执行。
type Action struct {
	Kind    string          `json:"-"`
	Channel string          `json:"channel,omitempty"`
	TS      string          `json:"ts,omitempty"`
	Link    string          `json:"link,omitempty"`
	Text    string          `json:"text,omitempty"`
	Blocks  json.RawMessage `json:"blocks,omitempty"`
	Name    string          `json:"name,omitempty"`
	DryRun  *bool           `json:"dry_run,omitempty"`
}

type ActionResult struct {
	Action         string          `json:"action"`
	Workspace      string          `json:"workspace"`
	Channel        string          `json:"channel"`
	TS             string          `json:"ts,omitempty"`
	ThreadTS       string          `json:"thread_ts,omitempty"`
	Text           string          `json:"text,omitempty"`
	Blocks         json.RawMessage `json:"blocks,omitempty"`
	Name           string          `json:"name,omitempty"`
	DryRun         bool            `json:"dry_run"`
	AlreadyReacted bool            `json:"already_reacted,omitzero"`
}

// Act 复用固定工作区的身份与消息目标校验，仅支持三个已知动作。
func (c *ReadOnlyClient) Act(ctx context.Context, a Action) (*ActionResult, error) {
	r := &ActionResult{Action: a.Kind, Workspace: c.workspace, Channel: a.Channel, Text: a.Text, Blocks: a.Blocks, Name: a.Name, DryRun: a.DryRun == nil || *a.DryRun}
	method := "chat.postMessage"
	switch a.Kind {
	case "send_message":
		if !channelPattern.MatchString(a.Channel) || a.TS != "" || a.Link != "" {
			return nil, errReadOnlyArgs
		}
	case "reply_thread", "add_reaction":
		ref, err := c.ParseTarget(a.Link, a.Channel, a.TS, "")
		if err != nil {
			return nil, err
		}
		r.Channel, r.TS = ref.ChannelID, ref.Timestamp
		if a.Kind == "reply_thread" {
			r.ThreadTS = ref.ThreadTS
			if r.ThreadTS == "" {
				r.ThreadTS = ref.Timestamp
			}
			r.TS = ""
		} else {
			method = "reactions.add"
		}
	default:
		return nil, errReadOnlyArgs
	}
	p := url.Values{"channel": {r.Channel}}
	if method == "reactions.add" {
		if !emojiPattern.MatchString(a.Name) || a.Text != "" || len(a.Blocks) != 0 {
			return nil, errReadOnlyArgs
		}
		p.Set("timestamp", r.TS)
		p.Set("name", a.Name)
	} else {
		if a.Name != "" || strings.TrimSpace(a.Text) == "" || len(a.Text) > ActionMaxTextBytes || !utf8.ValidString(a.Text) || strings.ContainsRune(a.Text, 0) {
			return nil, errReadOnlyArgs
		}
		if len(a.Blocks) > 0 {
			var blocks []map[string]any
			if len(a.Blocks) > ActionMaxBlocksBytes || json.Unmarshal(a.Blocks, &blocks) != nil || len(blocks) == 0 || len(blocks) > 50 {
				return nil, errReadOnlyArgs
			}
			for _, b := range blocks {
				if typ, ok := b["type"].(string); !ok || typ == "" {
					return nil, errReadOnlyArgs
				}
			}
			p.Set("blocks", string(a.Blocks))
		}
		p.Set("text", a.Text)
		p.Set("reply_broadcast", "false")
		if r.ThreadTS != "" {
			p.Set("thread_ts", r.ThreadTS)
		}
	}
	if r.DryRun {
		return r, nil
	}
	var response struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	err := c.fixedAPI(ctx, method, p, &response)
	if errors.Is(err, errAlreadyReacted) {
		r.AlreadyReacted = true
		return r, nil
	}
	if err != nil {
		return nil, errors.New("Slack 写入结果不明或被拒绝；不会自动重试，请先读取目标核实。")
	}
	if method == "chat.postMessage" {
		if response.Channel != r.Channel || !timestampPattern.MatchString(response.TS) {
			return nil, errors.New("Slack 写入结果不明；响应目标无法核实，请先读取目标，勿自动重试。")
		}
		r.TS = response.TS
	}
	return r, nil
}
