// Package localwake 仅通过固定本机 Orca IPC 传递结构化引用，不转交正文。
package localwake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cluas/slackctl/internal/mcpevents"
)

const DefaultWindow = 180 * time.Second
const queueSize = 256

var botPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
var channelPattern = regexp.MustCompile(`^[CGD][A-Z0-9]{1,63}$`)
var timestampPattern = regexp.MustCompile(`^[0-9]{10}\.[0-9]{6}$`)
var userPattern = regexp.MustCompile(`^[UW][A-Z0-9]+$`)

type Config struct {
	Path     string
	Bot      string
	Channels []string
	Mentions bool
	DMs      bool
	Window   time.Duration
}

type Reference struct {
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	Type     string `json:"type"`
}

type Payload struct {
	Version   int         `json:"version"`
	Kind      string      `json:"kind"`
	Count     int         `json:"count"`
	Events    []Reference `json:"events"`
	Truncated bool        `json:"truncated"`
}

type batch struct {
	until time.Time
	refs  []Reference
	count int
}
type Wake struct {
	config   Config
	channels map[string]bool
	input    chan mcpevents.Event
	pending  map[string]*batch
	seen     map[string]bool
	keys     []string
	dropped  atomic.Uint64
	run      func(context.Context, string, []string, []string) error
	sleep    func(context.Context, time.Duration) bool
}

func New(c Config) (*Wake, error) {
	if !filepath.IsAbs(c.Path) || filepath.Clean(c.Path) != c.Path || !botPattern.MatchString(c.Bot) {
		return nil, errors.New("本机唤醒需要可信的绝对 CLI 路径和固定 bot 名称。")
	}
	info, err := os.Stat(c.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil, errors.New("本机 Orca CLI 路径不是可执行文件。")
	}
	if len(c.Channels) > 128 {
		return nil, errors.New("唤醒频道白名单超限。")
	}
	channels := map[string]bool{}
	for _, ch := range c.Channels {
		if !channelPattern.MatchString(ch) {
			return nil, errors.New("唤醒白名单必须为精确频道 ID。")
		}
		channels[ch] = true
	}
	if len(channels) == 0 && !c.DMs {
		return nil, errors.New("唤醒必须配置频道白名单或显式启用人的私信。")
	}
	if c.Window == 0 {
		c.Window = DefaultWindow
	}
	if c.Window < time.Second || c.Window > time.Hour {
		return nil, errors.New("唤醒合并窗口必须在一秒至一小时之间。")
	}
	return &Wake{config: c, channels: channels, input: make(chan mcpevents.Event, queueSize), pending: map[string]*batch{}, seen: map[string]bool{}, run: execLocal, sleep: wait}, nil
}

// Offer 不阻塞事件读取；满队列计数由后台定时记录，不输出事件内容。
func (w *Wake) Offer(e mcpevents.Event) {
	select {
	case w.input <- e:
	default:
		w.dropped.Add(1)
	}
}

func reference(e mcpevents.Event) Reference {
	root := e.ThreadTS
	if root == "" {
		root = e.TS
	}
	return Reference{Channel: e.Channel, TS: e.TS, ThreadTS: root, Type: e.Type}
}
func payload(refs []Reference, count int) Payload {
	return Payload{Version: 1, Kind: "slack_event_references", Count: count, Events: refs, Truncated: count > len(refs)}
}

func (w *Wake) collect(e mcpevents.Event, now time.Time) *Payload {
	if e.Type != "message" || e.FromSelf || !channelPattern.MatchString(e.Channel) || !timestampPattern.MatchString(e.TS) || (e.ThreadTS != "" && !timestampPattern.MatchString(e.ThreadTS)) {
		return nil
	}
	switch e.Subtype {
	case "", "bot_message", "file_share", "me_message", "thread_broadcast":
	default:
		return nil
	}
	humanDM := w.config.DMs && e.IsDM && strings.HasPrefix(e.Channel, "D") && userPattern.MatchString(e.User) && e.BotID == "" && e.AppID == "" && e.Subtype != "bot_message"
	if !w.channels[e.Channel] && !humanDM {
		return nil
	}
	if e.IsDM && !humanDM {
		return nil
	}
	key := e.Channel + ":" + e.TS
	if w.seen[key] {
		return nil
	}
	w.seen[key] = true
	w.keys = append(w.keys, key)
	if len(w.keys) > 4096 {
		delete(w.seen, w.keys[0])
		w.keys = w.keys[1:]
	}
	r := reference(e)
	if humanDM || (w.config.Mentions && e.IsMention) {
		p := payload([]Reference{r}, 1)
		return &p
	}
	// 按频道合并，引用数量与待处理频道数均有上限。
	b := w.pending[e.Channel]
	if b == nil {
		if len(w.pending) >= 128 {
			w.dropped.Add(1)
			return nil
		}
		b = &batch{until: now.Add(w.config.Window)}
		w.pending[e.Channel] = b
	}
	b.count++
	if len(b.refs) < 16 {
		b.refs = append(b.refs, r)
	}
	return nil
}

func (w *Wake) flush(now time.Time) []Payload {
	var result []Payload
	for channel, b := range w.pending {
		if now.Before(b.until) {
			continue
		}
		result = append(result, payload(b.refs, b.count))
		delete(w.pending, channel)
	}
	return result
}

// Run 的发送在单独工作协程进行；慢 IPC 不阻塞过滤、聚合或 MCP。
func (w *Wake) Run(ctx context.Context, log io.Writer) {
	jobs := make(chan Payload, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case p, ok := <-jobs:
				if !ok {
					return
				}
				if err := w.deliver(ctx, p); err != nil {
					fmt.Fprintln(log, "本机唤醒失败：已达到超时或重试上限，远端输出已隐藏。")
				}
			}
		}
	}()
	defer func() { close(jobs); <-done }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	send := func(p Payload) {
		select {
		case jobs <- p:
		default:
			w.dropped.Add(1)
		}
	}
	for {
		select {
		case <-ctx.Done():
			if len(w.pending) > 0 || len(w.input) > 0 || len(jobs) > 0 {
				fmt.Fprintln(log, "本机唤醒停止，仍有未投递的内存事件；不会自动持久化或重放。")
			}
			return
		case e := <-w.input:
			if p := w.collect(e, time.Now()); p != nil {
				send(*p)
			}
		case now := <-ticker.C:
			for _, p := range w.flush(now) {
				send(p)
			}
			if n := w.dropped.Swap(0); n > 0 {
				fmt.Fprintf(log, "本机唤醒队列超限，丢弃计数：%d。\n", n)
			}
		}
	}
}

func (w *Wake) deliver(ctx context.Context, p Payload) error {
	data, err := json.Marshal(p)
	if err != nil || len(data) > 16*1024 {
		return errors.New("本机唤醒载荷超限。")
	}
	f, err := os.CreateTemp("", "slackctl-wake-*.json")
	if err != nil {
		return errors.New("本机唤醒临时文件创建失败。")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return errors.New("本机唤醒临时文件写入失败。")
	}
	if err = f.Close(); err != nil {
		return errors.New("本机唤醒临时文件关闭失败。")
	}
	args := []string{"orchestration", "send", "--environment", "local", "--to", "bot:" + w.config.Bot, "--from", "bot:" + w.config.Bot, "--type", "handoff", "--subject", "Slack 事件通知", "--body-file", name}
	// 不继承远端 Orca 环境、代理、认证或注入选项；Home Chat 路由省略 --chat。
	env := []string{"PATH=/usr/bin:/bin", "ORCA_ENVIRONMENT=local"}
	for _, key := range []string{"HOME", "USERPROFILE", "TMPDIR", "SYSTEMROOT"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	for attempt := range 3 {
		if ctx.Err() != nil {
			break
		}
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = w.run(callCtx, w.config.Path, args, env)
		cancel()
		if err == nil {
			return nil
		}
		if attempt < 2 && !w.sleep(ctx, time.Duration(1<<attempt)*time.Second) {
			break
		}
	}
	return errors.New("本机唤醒投递失败；最多三次尝试，结果可能不明，接收端应按引用去重。")
}

func execLocal(ctx context.Context, path string, args, env []string) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	return cmd.Run()
}
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
