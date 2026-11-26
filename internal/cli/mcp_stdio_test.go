package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	goversion "go/version"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPBinaryStdio 通过真实二进制、SDK 客户端和本地 TLS 代理验证协议。
// 子进程仅能取得合成配置；代理不建立任何上游连接。
func TestMCPBinaryStdio(t *testing.T) {
	dir := t.TempDir()
	binary := os.Getenv("SLACKCTL_MCP_TEST_BINARY")
	if binary == "" {
		binary = filepath.Join(dir, "slackctl")
		build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./cmd/slackctl")
		build.Dir = filepath.Join("..", "..")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("构建失败: %v %s", err, output)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "合成测试"}, DNSNames: []string{"slack.com", "wss-primary.slack.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(dir, "ca.pem")
	if err = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var reject atomic.Bool
	var writeRequests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || (r.Host != "slack.com:443" && r.Host != "wss-primary.slack.com:443") {
			t.Error("子进程请求了非固定端点")
			http.Error(w, "拒绝", 403)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12})
		if err = tlsConn.Handshake(); err != nil {
			t.Error(err)
			return
		}
		reader := bufio.NewReader(tlsConn)
		for {
			request, err := http.ReadRequest(reader)
			if err != nil {
				return
			}
			requests.Add(1)
			if request.Host == "wss-primary.slack.com" {
				if request.Method != "GET" || request.Header.Get("Authorization") != "Bearer xoxp-synthetic" || request.Header.Get("Upgrade") != "websocket" {
					t.Error("WS 身份或握手错误")
					return
				}
				hash := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				response := &http.Response{StatusCode: 101, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Upgrade": {"websocket"}, "Connection": {"Upgrade"}, "Sec-Websocket-Accept": {base64.StdEncoding.EncodeToString(hash[:])}}}
				if response.Write(tlsConn) != nil {
					return
				}
				writeSyntheticFrame(tlsConn, `{"type":"hello","team_id":"TEXAMPLE1"}`)
				writeSyntheticFrame(tlsConn, `{"type":"message","team":"TEXAMPLE1","channel":"CEXAMPLE1","ts":"1700000000.000001","user":"UOTHER1","text":"<@UEXAMPLE1> 合成正文","blocks":[{"type":"section","text":{"type":"plain_text","text":"合成字段"}}]}`)
				_, _ = io.Copy(io.Discard, tlsConn)
				return
			}
			if request.Method != "POST" || request.Host != "slack.com" || request.Header.Get("Authorization") != "Bearer xoxp-synthetic" {
				t.Error("请求身份或方法改变")
			}
			_ = request.ParseForm()
			_ = request.Body.Close()
			body := `{"ok":true,"messages":[{"ts":"1700000000.000001","text":"合成消息"}]}`
			switch request.URL.Path {
			case "/api/auth.test":
				body = `{"ok":true,"url":"https://example.slack.com/","team_id":"TEXAMPLE1","user_id":"UEXAMPLE1"}`
				if reject.Load() {
					body = `{"ok":true,"url":"https://other.slack.com/","team_id":"TOTHER01","user_id":"UOTHER01"}`
				}
			case "/api/search.messages":
				body = `{"ok":true,"messages":{"total":1,"matches":[{"channel":{"id":"CEXAMPLE1"},"ts":"1700000000.000001","text":"合成消息"}]}}`
			case "/api/conversations.history", "/api/conversations.replies":
			case "/api/chat.postMessage":
				writeRequests.Add(1)
				if request.Form.Get("channel") != "CEXAMPLE1" || request.Form.Get("text") != "*合成写入*" || request.Form.Get("reply_broadcast") != "false" {
					t.Error("实际消息 payload 错误")
				}
				if root := request.Form.Get("thread_ts"); root != "" && root != "1700000000.000001" {
					t.Error("实际回复未回到根线程")
				}
				body = `{"ok":true,"channel":"CEXAMPLE1","ts":"1700000002.000001"}`
			case "/api/reactions.add":
				writeRequests.Add(1)
				if request.Form.Get("timestamp") != "1700000001.000002" || request.Form.Get("name") != "wave" {
					t.Error("实际 reaction payload 错误")
				}
				body = `{"ok":false,"error":"already_reacted"}`
			case "/api/rtm.connect":
				body = `{"ok":true,"url":"wss://wss-primary.slack.com/synthetic","team":{"id":"TEXAMPLE1"},"self":{"id":"UEXAMPLE1"}}`
			default:
				t.Error("出现非只读方法")
			}
			response := &http.Response{StatusCode: 200, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
			if response.Write(tlsConn) != nil {
				return
			}
		}
	}))
	defer proxy.Close()
	fixtureDir := filepath.Join(dir, ".config", "agent-slack")
	if err = os.MkdirAll(fixtureDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := `{"version":1,"workspaces":[{"workspace_url":"https://example.slack.com","auth":{"auth_type":"standard","token":"xoxp-synthetic"}}]}`
	if err = os.WriteFile(filepath.Join(fixtureDir, "credentials.json"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	childEnv := []string{"HOME=" + dir, "USERPROFILE=" + dir, "home=" + dir, "HTTPS_PROXY=" + proxy.URL, "HTTP_PROXY=" + proxy.URL, "NO_PROXY=", "SSL_CERT_FILE=" + certFile, "SSL_CERT_DIR=" + dir, "SLACKCTL_DEBUG=1", "GODEBUG=x509sslcertoverrideplatform=1,x509usefallbackroots=1"}
	help := exec.CommandContext(t.Context(), binary, "mcp", "--help")
	help.Env = childEnv
	if output, err := help.CombinedOutput(); err != nil || !bytes.Contains(output, []byte("--workspace")) {
		t.Fatalf("帮助失败: %v %s", err, output)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	useHelper := (runtime.GOOS == "darwin" || runtime.GOOS == "windows") && goversion.Compare(runtime.Version(), "go1.27") < 0
	if useHelper {
		childEnv = append(childEnv, "SLACKCTL_MCP_HELPER=1")
	}
	newCommand := func(args ...string) *exec.Cmd {
		if useHelper {
			// 旧 Go 在这些系统忽略 SSL_CERT_FILE；测试子进程通过标准库注入合成根证书。
			helper, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			return exec.CommandContext(ctx, helper, append([]string{"-test.run=^TestMCPStdioHelper$", "--"}, args...)...)
		}
		return exec.CommandContext(ctx, binary, args...)
	}
	command := newCommand("mcp", "--workspace", "https://example.slack.com", "--verbose")
	command.Env = childEnv
	var stderr bytes.Buffer
	command.Stderr = &stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "synthetic-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("初始化失败: %v %s", err, stderr.Bytes())
	}
	defer session.Close()
	if session.InitializeResult().ServerInfo.Name != "slackctl" {
		t.Fatal("服务端身份错误")
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 4 {
		t.Fatalf("工具列表失败: %v", err)
	}
	for name, args := range map[string]map[string]any{
		"search_messages": {"query": "合成消息"},
		"read_message":    {"channel": "CEXAMPLE1", "ts": "1700000000.000001"},
		"read_thread":     {"channel": "CEXAMPLE1", "ts": "1700000000.000001"},
		"channel_history": {"channel": "CEXAMPLE1"},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(result)
		if result.IsError || !bytes.Contains(data, []byte("合成消息")) {
			t.Fatalf("工具失败 %s: %s", name, data)
		}
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("标准错误出现输出: %s", stderr.Bytes())
	}
	if requests.Load() != 5 {
		t.Fatalf("请求数异常: %d", requests.Load())
	}
	// 错误的真实身份必须在 initialize 之前失败，stdout 不能混入错误。
	reject.Store(true)
	mismatch := newCommand("mcp", "--workspace", "https://example.slack.com")
	mismatch.Env = childEnv
	var stdout, authErr bytes.Buffer
	mismatch.Stdout = &stdout
	mismatch.Stderr = &authErr
	if err = mismatch.Run(); err == nil || stdout.Len() != 0 || strings.Contains(authErr.String(), "xoxp-") {
		t.Fatal("错身份启动未安全失败")
	}
	if requests.Load() != 6 {
		t.Fatal("错身份发生重试或回退")
	}
	reject.Store(false)
	t.Run("显式写入和默认预览", func(t *testing.T) {
		cmd := newCommand("mcp", "--workspace", "https://example.slack.com", "--enable-writes")
		cmd.Env = childEnv
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		listed, err := cs.ListTools(ctx, nil)
		if err != nil || len(listed.Tools) != 7 {
			t.Fatal("写开关没有独立注册三个工具")
		}
		for name, args := range map[string]map[string]any{
			"send_message": {"channel": "CEXAMPLE1", "text": "*合成写入*"},
			"reply_thread": {"link": "https://example.slack.com/archives/CEXAMPLE1/p1700000001000002?thread_ts=1700000000.000001", "text": "*合成写入*"},
			"add_reaction": {"link": "https://example.slack.com/archives/CEXAMPLE1/p1700000001000002?thread_ts=1700000000.000001", "name": "wave"},
		} {
			before := writeRequests.Load()
			r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
			if err != nil || r.IsError || writeRequests.Load() != before {
				t.Fatal("默认预览发出写请求")
			}
			args["dry_run"] = false
			r, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
			if err != nil || r.IsError || writeRequests.Load() != before+1 {
				t.Fatal("显式写入失败")
			}
		}
		before := writeRequests.Load()
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "reply_thread", Arguments: map[string]any{"link": "https://other.slack.com/archives/CEXAMPLE1/p1700000000000001", "text": "合成", "dry_run": false}})
		if err != nil || !r.IsError || writeRequests.Load() != before {
			t.Fatal("跨域写入未拒绝")
		}
		_ = cs.Close()
		if stderr.Len() != 0 {
			t.Fatal("写入泄漏协议外输出")
		}
	})
	t.Run("实时事件独立读取", func(t *testing.T) {
		cmd := newCommand("mcp", "--workspace", "https://example.slack.com", "--enable-events")
		cmd.Env = childEnv
		cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		listed, err := cs.ListTools(ctx, nil)
		if err != nil || len(listed.Tools) != 5 {
			t.Fatal("事件开关注册了写工具")
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_events", Arguments: map[string]any{}})
			if err != nil || r.IsError {
				t.Fatal("事件读取失败")
			}
			data, _ := json.Marshal(r)
			if bytes.Contains(data, []byte("合成字段")) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("实时事件没有进入队列")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("常驻入口模拟唤醒", func(t *testing.T) {
		receiver := filepath.Join(dir, "synthetic-receiver")
		source := filepath.Join(dir, "receiver.go")
		code := `package main
import("os";"path/filepath";"encoding/json")
func main(){a:=os.Args[1:];if len(a)!=14||a[0]!="orchestration"||a[1]!="send"||a[3]!="local"||a[5]!="bot:example-bot"||a[7]!="bot:example-bot"||a[9]!="handoff"||a[11]!="Slack 事件通知"||a[12]!="--body-file"{os.Exit(2)};b,e:=os.ReadFile(a[13]);if e!=nil{os.Exit(3)};var v map[string]any;if json.Unmarshal(b,&v)!=nil{os.Exit(4)};p,_:=os.Executable();if os.WriteFile(filepath.Join(filepath.Dir(p),"wake-receipt.json"),b,0600)!=nil{os.Exit(5)}}`
		if err := os.WriteFile(source, []byte(code), 0600); err != nil {
			t.Fatal(err)
		}
		build := exec.CommandContext(t.Context(), "go", "build", "-o", receiver, source)
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("模拟端构建失败: %v %s", err, out)
		}
		cmd := newCommand("mcp", "listen", "--workspace", "https://example.slack.com", "--enable-local-wake", "--orca-path", receiver, "--orca-bot", "example-bot", "--wake-channels", "CEXAMPLE1", "--wake-mentions")
		cmd.Env = childEnv
		var out, logs bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &logs
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			data, err := os.ReadFile(filepath.Join(dir, "wake-receipt.json"))
			if err == nil {
				if bytes.Contains(data, []byte("合成正文")) || bytes.Contains(data, []byte("UOTHER1")) || !bytes.Contains(data, []byte("CEXAMPLE1")) {
					t.Fatal("唤醒载荷越界")
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("常驻事件未触发模拟本机唤醒")
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = cmd.Process.Signal(os.Interrupt)
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 || strings.Contains(logs.String(), "xoxp-") {
			t.Fatal("常驻模式输出泄漏")
		}
	})
	mode := "真实二进制"
	if useHelper {
		mode = "相同 CLI 入口的测试子进程（旧系统证书兼容）"
	}
	t.Logf("%s initialize、tools/list、四次 tools/call 与错身份启动检查均通过；所有 HTTPS 请求仅到本地合成代理", mode)
}

func writeSyntheticFrame(w io.Writer, text string) {
	b := []byte(text)
	header := []byte{0x81, byte(len(b))}
	if len(b) >= 126 {
		header = []byte{0x81, 126, 0, 0}
		binary.BigEndian.PutUint16(header[2:], uint16(len(b)))
	}
	_, _ = w.Write(append(header, b...))
}

func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv("SLACKCTL_MCP_HELPER") != "1" {
		return
	}
	cert, err := os.ReadFile(os.Getenv("SSL_CERT_FILE"))
	if err != nil {
		os.Exit(2)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cert) {
		os.Exit(2)
	}
	x509.SetFallbackRoots(roots)
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(2)
	}
	root := NewRootCmd()
	root.SetArgs(args[1:])
	if err := root.Execute(); err != nil {
		_, _ = io.WriteString(os.Stderr, err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}
