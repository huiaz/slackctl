package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "合成测试"}, DNSNames: []string{"slack.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
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
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "slack.com:443" {
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
			if request.Method != "POST" || request.Host != "slack.com" || request.Header.Get("Authorization") != "Bearer xoxp-synthetic" {
				t.Error("请求身份或方法改变")
			}
			_, _ = io.Copy(io.Discard, request.Body)
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
	mode := "真实二进制"
	if useHelper {
		mode = "相同 CLI 入口的测试子进程（旧系统证书兼容）"
	}
	t.Logf("%s initialize、tools/list、四次 tools/call 与错身份启动检查均通过；所有 HTTPS 请求仅到本地合成代理", mode)
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
