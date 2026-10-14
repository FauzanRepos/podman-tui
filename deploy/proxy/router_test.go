package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman-tui/deploy"
)

func freePort(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return uint16(port)
}
func testProxyApplication(t *testing.T) (deploy.Application, *Router) {
	t.Helper()
	binary, err := exec.LookPath("nginx")
	if err != nil {
		t.Skip("Nginx binary unavailable")
	}
	root := t.TempDir()
	managed := filepath.Join(root, "managed")
	os.Mkdir(managed, 0700)
	conf := filepath.Join(root, "nginx.conf")
	port := freePort(t)
	text := fmt.Sprintf("pid %s;\nerror_log %s;\nevents {}\nhttp { access_log off; include %s; }\n", quote(filepath.Join(root, "nginx.pid")), quote(filepath.Join(root, "nginx.error")), quote(filepath.Join(managed, "*.conf")))
	if err = os.WriteFile(conf, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "-c", conf)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("isolated nginx start: %v %s", err, output)
	}
	t.Cleanup(func() { exec.Command(binary, "-s", "quit", "-c", conf).Run() })
	a := deploy.Application{ID: strings.Repeat("a", 32), Name: "web", Image: "docker.io/library/busybox:1.37", Profile: "production", Profiles: map[string]deploy.RuntimeConfig{"production": {Ports: []deploy.Port{{Container: 8080}}}}, Domains: []string{"web.example.invalid"}, Proxy: &deploy.ProxyConfig{ManagedDir: managed, ConfigFile: conf, Binary: binary, BackendHost: "127.0.0.1", ContainerPort: 8080, HTTPPort: port}, Readiness: deploy.Readiness{Mode: "http", Path: "/", TimeoutSeconds: 3}}
	return a, New(true)
}
func requestBody(t *testing.T, a deploy.Application, expected string) string {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for {
		request, _ := http.NewRequest(http.MethodGet, BackendAddress("127.0.0.1", a.Proxy.HTTPPort), nil)
		request.Host = a.Domains[0]
		response, err := client.Do(request)
		if err == nil {
			data, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr == nil && response.StatusCode == 200 && string(data) == expected {
				return string(data)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated nginx did not serve the expected route", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNginxSwitchUndoConflictAndInvalidConfig(t *testing.T) {
	a, router := testProxyApplication(t)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "known-good") }))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "replacement") }))
	defer second.Close()
	if _, err := router.Switch(context.Background(), a, deploy.Candidate{Endpoint: first.URL}); err != nil {
		t.Fatal(err)
	}
	if got := requestBody(t, a, "known-good"); got != "known-good" {
		t.Fatal("wrong initial route", got)
	}
	undo, err := router.Switch(context.Background(), a, deploy.Candidate{Endpoint: second.URL})
	if err != nil {
		t.Fatal(err)
	}
	if got := requestBody(t, a, "replacement"); got != "replacement" {
		t.Fatal("candidate route not active", got)
	}
	if err = undo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := requestBody(t, a, "known-good"); got != "known-good" {
		t.Fatal("route rollback failed", got)
	}
	conflict := a
	conflict.ID = strings.Repeat("b", 32)
	conflict.Name = "other"
	if _, err = router.Switch(context.Background(), conflict, deploy.Candidate{Endpoint: second.URL}); err == nil {
		t.Fatal("domain conflict accepted")
	}
	// A missing certificate pair is a generated-config validation failure, not a live routing cutover.
	invalid := a
	cfg := *a.Proxy
	cfg.Certificate = filepath.Join(t.TempDir(), "missing.pem")
	cfg.PrivateKey = filepath.Join(t.TempDir(), "missing.key")
	invalid.Proxy = &cfg
	if _, err = router.Switch(context.Background(), invalid, deploy.Candidate{Endpoint: second.URL}); err == nil {
		t.Fatal("invalid nginx config activated")
	}
	if got := requestBody(t, a, "known-good"); got != "known-good" {
		t.Fatal("invalid config disturbed known-good route", got)
	}
	external := filepath.Join(a.Proxy.ManagedDir, "external.conf")
	content := []byte("# external operator configuration\n")
	os.WriteFile(external, content, 0600)
	if err = router.Remove(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if actual, _ := os.ReadFile(external); string(actual) != string(content) {
		t.Fatal("external nginx file changed")
	}
}

func TestExternalDomainPatternsAndEndpointInjection(t *testing.T) {
	for _, pattern := range []string{"*.example.invalid", ".example.invalid", "web.*"} {
		if conflicts("server_name "+pattern+";", "/not/this.conf", []string{"web.example.invalid"}) == nil {
			t.Fatal("external domain overlap missed", pattern)
		}
	}
	a := deploy.Application{ID: strings.Repeat("a", 32), Name: "web", Image: "busybox", Profile: "production", Profiles: map[string]deploy.RuntimeConfig{"production": {Ports: []deploy.Port{{Container: 80}}}}, Domains: []string{"web.example.invalid"}, Proxy: &deploy.ProxyConfig{ManagedDir: "/tmp/managed", ConfigFile: "/tmp/nginx.conf", BackendHost: "127.0.0.1", ContainerPort: 80}}
	for _, endpoint := range []string{"http://127.0.0.1:80; include /etc/passwd", "http://other:80", "https://127.0.0.1:80", "http://127.0.0.1:0", "http://127.0.0.1:80/?x=y"} {
		if _, err := config(a, deploy.Candidate{Endpoint: endpoint}); err == nil {
			t.Fatal("unsafe endpoint accepted", endpoint)
		}
	}
}

func TestWebSocketUpgradePreservesBidirectionalFrames(t *testing.T) {
	a, router := testProxyApplication(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Upgrade") == "" {
			io.WriteString(w, "ready")
			return
		}
		if request.Header.Get("Upgrade") != "websocket" || request.Header.Get("Connection") != "upgrade" {
			http.Error(w, "upgrade headers lost", http.StatusBadRequest)
			return
		}
		connection, stream, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		connection.SetDeadline(time.Now().Add(2 * time.Second))
		// RFC 6455 handshake vector and one masked client text frame.
		io.WriteString(stream, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
		stream.Flush()
		var frame [11]byte
		if _, err = io.ReadFull(stream, frame[:]); err != nil {
			return
		}
		if frame[0] != 0x81 || frame[1] != 0x85 {
			return
		}
		var message [5]byte
		for i := range message {
			message[i] = frame[6+i] ^ frame[2+i%4]
		}
		if string(message[:]) != "Hello" {
			return
		}
		stream.Write(append([]byte{0x81, 5}, message[:]...))
		stream.Flush()
	}))
	defer backend.Close()
	if _, err := router.Switch(context.Background(), a, deploy.Candidate{Endpoint: backend.URL}); err != nil {
		t.Fatal(err)
	}
	requestBody(t, a, "ready")
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", a.Proxy.HTTPPort), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(connection, "GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", a.Domains[0])
	stream := bufio.NewReader(connection)
	response, err := http.ReadResponse(stream, &http.Request{Method: http.MethodGet})
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatal("WebSocket upgrade rejected", err)
	}
	if response.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatal("handshake header lost")
	}
	connection.Write([]byte{0x81, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58})
	var echoed [7]byte
	if _, err = io.ReadFull(stream, echoed[:]); err != nil || string(echoed[2:]) != "Hello" || echoed[0] != 0x81 || echoed[1] != 5 {
		t.Fatal("WebSocket frame did not traverse managed proxy", err)
	}
}
