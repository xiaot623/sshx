package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func emptyEnvironment(string) string { return "" }

func TestLoadPolicyPrecedenceAndNoProxy(t *testing.T) {
	values := map[string]string{
		"SSHX_PROXY_URL": "socks5h://explicit.example:1080",
		"ALL_PROXY":      "http://all.example:8080",
		"HTTPS_PROXY":    "http://https.example:8080",
		"HTTP_PROXY":     "http://http.example:8080",
		"NO_PROXY":       ".internal.example,10.0.0.0/8,localhost:9000",
	}
	policy, err := LoadPolicy(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.selectGeneric("example.com:443"); got == nil || got.Host != "explicit.example:1080" {
		t.Fatalf("generic upstream = %v", got)
	}
	for _, address := range []string{"api.internal.example:443", "10.1.2.3:80", "localhost:9000"} {
		if !policy.bypass(address) {
			t.Fatalf("%s should bypass the upstream", address)
		}
	}
	if policy.bypass("localhost:9001") {
		t.Fatal("port-specific NO_PROXY rule matched another port")
	}
}

func TestPolicyEnvironmentSelection(t *testing.T) {
	values := map[string]string{
		"HTTP_PROXY":  "http://http.example:8080",
		"HTTPS_PROXY": "http://https.example:8080",
		"ALL_PROXY":   "socks5h://all.example:1080",
	}
	policy, err := LoadPolicy(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.selectHTTP("http", "target.example:80"); got == nil || got.Host != "http.example:8080" {
		t.Fatalf("HTTP upstream = %v", got)
	}
	if got := policy.selectGeneric("target.example:443"); got == nil || got.Host != "all.example:1080" {
		t.Fatalf("generic upstream = %v", got)
	}

	delete(values, "ALL_PROXY")
	policy, err = LoadPolicy(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.selectHTTP("http", "target.example:80"); got == nil || got.Host != "http.example:8080" {
		t.Fatalf("HTTP upstream without ALL_PROXY = %v", got)
	}
	if got := policy.selectGeneric("target.example:443"); got == nil || got.Host != "https.example:8080" {
		t.Fatalf("generic upstream without ALL_PROXY = %v", got)
	}

	policy, err = LoadPolicy(func(key string) string {
		if key == "http_proxy" {
			return "http://lowercase.example:8080"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.selectHTTP("http", "target.example:80"); got == nil || got.Host != "lowercase.example:8080" {
		t.Fatalf("lowercase HTTP upstream = %v", got)
	}
}

func TestLoadPolicyRejectsUnsupportedExplicitProxy(t *testing.T) {
	_, err := LoadPolicy(func(key string) string {
		if key == "SSHX_PROXY_URL" {
			return "ftp://proxy.example"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported proxy scheme") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "proxy.example") {
		t.Fatalf("error leaked proxy URL: %v", err)
	}
}

func TestLoadPolicyRedactsMalformedCredentials(t *testing.T) {
	_, err := LoadPolicy(func(key string) string {
		if key == "SSHX_PROXY_URL" {
			return "http://user:super-secret%zz@proxy.example"
		}
		return ""
	})
	if err == nil {
		t.Fatal("malformed proxy URL was accepted")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaked proxy credentials: %v", err)
	}
}

func TestHTTPNoProxyUsesDefaultPort(t *testing.T) {
	policy, err := LoadPolicy(func(key string) string {
		switch key {
		case "HTTP_PROXY":
			return "http://proxy.example:8080"
		case "NO_PROXY":
			return "direct.example:80"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.selectHTTP("http", "direct.example"); got != nil {
		t.Fatalf("HTTP default port did not match NO_PROXY: %v", got)
	}
	if policy.bypass(addressWithDefaultPort("https", "direct.example")) {
		t.Fatal("HTTPS default port unexpectedly matched the HTTP-only NO_PROXY rule")
	}
}

func TestHTTPProxyRequest(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/through-proxy" {
			t.Fatalf("path = %q", req.URL.Path)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()

	server := startTestServer(t)
	proxyURL := authenticatedURL(t, "http", server)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	response, err := client.Get(target.URL + "/through-proxy")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}
}

func TestHTTPProxyRequestChainsHTTPUpstream(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "chained")
	}))
	defer target.Close()
	upstream := startTestServer(t)
	upstreamURL := authenticatedURL(t, "http", upstream)
	server := startTestServerWithEnvironment(t, func(key string) string {
		if key == "SSHX_PROXY_URL" {
			return upstreamURL.String()
		}
		return ""
	})
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(authenticatedURL(t, "http", server))}}
	response, err := client.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "chained" {
		t.Fatalf("body = %q", body)
	}
}

func TestHTTPConnectAndSOCKS5ShareListener(t *testing.T) {
	echoAddress := startEchoServer(t)
	server := startTestServer(t)

	t.Run("http connect", func(t *testing.T) {
		conn, err := net.Dial("tcp", server.Address())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		token := base64.StdEncoding.EncodeToString([]byte(server.Username() + ":" + server.Password()))
		_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", echoAddress, echoAddress, token)
		reader := bufio.NewReader(conn)
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %s", response.Status)
		}
		assertEcho(t, conn, reader)
	})

	t.Run("socks5", func(t *testing.T) {
		conn, err := net.Dial("tcp", server.Address())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = conn.Write([]byte{5, 1, 2})
		reply := make([]byte, 2)
		if _, err := io.ReadFull(conn, reply); err != nil || reply[1] != 2 {
			t.Fatalf("method reply = %v, %v", reply, err)
		}
		user, password := []byte(server.Username()), []byte(server.Password())
		auth := append([]byte{1, byte(len(user))}, user...)
		auth = append(auth, byte(len(password)))
		auth = append(auth, password...)
		_, _ = conn.Write(auth)
		if _, err := io.ReadFull(conn, reply); err != nil || reply[1] != 0 {
			t.Fatalf("auth reply = %v, %v", reply, err)
		}
		host, portText, err := net.SplitHostPort(echoAddress)
		if err != nil {
			t.Fatal(err)
		}
		port, _ := net.LookupPort("tcp", portText)
		ip := net.ParseIP(host).To4()
		request := append([]byte{5, 1, 0, 1}, ip...)
		portBytes := make([]byte, 2)
		binary.BigEndian.PutUint16(portBytes, uint16(port))
		request = append(request, portBytes...)
		_, _ = conn.Write(request)
		connectReply := make([]byte, 10)
		if _, err := io.ReadFull(conn, connectReply); err != nil || connectReply[1] != 0 {
			t.Fatalf("connect reply = %v, %v", connectReply, err)
		}
		assertEcho(t, conn, conn)
	})
}

func TestProxyRequiresAuthentication(t *testing.T) {
	server := startTestServer(t)
	conn, err := net.Dial("tcp", server.Address())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET http://example.test/ HTTP/1.1\r\nHost: example.test\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("status = %s", response.Status)
	}
}

func TestGenericDialChainsConfiguredUpstream(t *testing.T) {
	echoAddress := startEchoServer(t)
	upstream := startTestServer(t)
	for _, scheme := range []string{"http", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			upstreamURL := authenticatedURL(t, scheme, upstream)
			policy, err := LoadPolicy(func(key string) string {
				if key == "SSHX_PROXY_URL" {
					return upstreamURL.String()
				}
				return ""
			})
			if err != nil {
				t.Fatal(err)
			}
			conn, err := policy.dialContext(context.Background(), "tcp", echoAddress)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			assertEcho(t, conn, conn)
		})
	}
}

func TestConfiguredUpstreamFailureDoesNotFallBackToDirect(t *testing.T) {
	echoAddress := startEchoServer(t)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstreamAddress := closed.Addr().String()
	_ = closed.Close()
	policy, err := LoadPolicy(func(key string) string {
		if key == "SSHX_PROXY_URL" {
			return "http://" + upstreamAddress
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := policy.dialContext(context.Background(), "tcp", echoAddress); err == nil {
		_ = conn.Close()
		t.Fatal("dial unexpectedly fell back to the direct target")
	}
}

func startTestServer(t *testing.T) *Server {
	t.Helper()
	return startTestServerWithEnvironment(t, emptyEnvironment)
}

func startTestServerWithEnvironment(t *testing.T, getenv Environment) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	server, err := Start(ctx, Options{Getenv: getenv})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = server.Close()
	})
	return server
}

func authenticatedURL(t *testing.T, scheme string, server *Server) *url.URL {
	t.Helper()
	value, err := url.Parse(scheme + "://" + url.UserPassword(server.Username(), server.Password()).String() + "@" + server.Address())
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func startEchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

func assertEcho(t *testing.T, writer io.Writer, reader io.Reader) {
	t.Helper()
	payload := []byte("proxy-echo")
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	type result struct {
		value []byte
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value := make([]byte, len(payload))
		_, err := io.ReadFull(reader, value)
		done <- result{value: value, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil || string(got.value) != string(payload) {
			t.Fatalf("echo = %q, %v", got.value, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for echo")
	}
}
