package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const dialTimeout = 10 * time.Second

type Environment func(string) string

type Policy struct {
	explicit *url.URL
	http     *url.URL
	https    *url.URL
	all      *url.URL
	noProxy  []string

	mu         sync.Mutex
	transports map[string]*http.Transport
}

type Options struct {
	Getenv Environment
}

type Server struct {
	listener net.Listener
	policy   *Policy
	user     string
	password string

	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	mu        sync.Mutex
	active    map[net.Conn]struct{}
	closed    bool
	wg        sync.WaitGroup
}

func LoadPolicy(getenv Environment) (*Policy, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	parse := func(value string) (*url.URL, error) {
		if strings.TrimSpace(value) == "" {
			return nil, nil
		}
		u, err := url.Parse(value)
		if err != nil {
			// url.Parse errors can include the original URL. Do not let malformed
			// credentials escape through errors that callers may log.
			return nil, errors.New("invalid proxy URL")
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
		if u.Hostname() == "" {
			return nil, errors.New("proxy URL requires a host")
		}
		return u, nil
	}
	first := func(names ...string) string {
		for _, name := range names {
			if value := strings.TrimSpace(getenv(name)); value != "" {
				return value
			}
		}
		return ""
	}
	explicit, err := parse(first("SSHX_PROXY_URL"))
	if err != nil {
		return nil, fmt.Errorf("SSHX_PROXY_URL: %w", err)
	}
	httpProxy, err := parse(first("HTTP_PROXY", "http_proxy"))
	if err != nil {
		return nil, fmt.Errorf("HTTP_PROXY: %w", err)
	}
	httpsProxy, err := parse(first("HTTPS_PROXY", "https_proxy"))
	if err != nil {
		return nil, fmt.Errorf("HTTPS_PROXY: %w", err)
	}
	allProxy, err := parse(first("ALL_PROXY", "all_proxy"))
	if err != nil {
		return nil, fmt.Errorf("ALL_PROXY: %w", err)
	}
	noProxy := strings.Split(first("NO_PROXY", "no_proxy"), ",")
	return &Policy{
		explicit:   explicit,
		http:       httpProxy,
		https:      httpsProxy,
		all:        allProxy,
		noProxy:    noProxy,
		transports: map[string]*http.Transport{},
	}, nil
}

func Start(parent context.Context, opts Options) (*Server, error) {
	policy, err := LoadPolicy(opts.Getenv)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	passwordBytes := make([]byte, 24)
	if _, err := rand.Read(passwordBytes); err != nil {
		_ = listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	s := &Server{
		listener: listener,
		policy:   policy,
		user:     "sshx",
		password: hex.EncodeToString(passwordBytes),
		ctx:      ctx,
		cancel:   cancel,
		active:   map[net.Conn]struct{}{},
	}
	s.wg.Add(1)
	go s.acceptLoop()
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	return s, nil
}

func (s *Server) Address() string { return s.listener.Addr().String() }

func (s *Server) Username() string { return s.user }

func (s *Server) Password() string { return s.password }

func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.cancel()
		s.mu.Lock()
		s.closed = true
		for conn := range s.active {
			_ = conn.Close()
		}
		s.mu.Unlock()
		err = s.listener.Close()
		s.policy.closeIdleConnections()
	})
	s.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.active[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			defer func() {
				s.mu.Lock()
				delete(s.active, conn)
				s.mu.Unlock()
			}()
			_ = s.serveConn(conn)
		}()
	}
}

func (s *Server) serveConn(conn net.Conn) error {
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	if err != nil {
		return err
	}
	if first[0] == 5 {
		return s.serveSOCKS5(conn, reader)
	}
	return s.serveHTTP(conn, reader)
}

func (s *Server) serveHTTP(client net.Conn, reader *bufio.Reader) error {
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return err
		}
		if !s.authorizeHTTP(req) {
			_ = writeHTTPError(client, http.StatusProxyAuthRequired, "proxy authentication required", true)
			return nil
		}
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Proxy-Connection")
		if req.Method == http.MethodConnect {
			return s.serveConnect(client, reader, req)
		}
		if req.URL.Scheme == "" {
			req.URL.Scheme = "http"
		}
		if req.URL.Host == "" {
			req.URL.Host = req.Host
		}
		req.RequestURI = ""
		req = req.WithContext(s.ctx)
		transport := s.policy.transportForHTTP(req.URL.Scheme, req.URL.Host)
		resp, err := transport.RoundTrip(req)
		if err != nil {
			_ = writeHTTPError(client, http.StatusBadGateway, err.Error(), false)
			return nil
		}
		err = resp.Write(client)
		_ = resp.Body.Close()
		if err != nil || req.Close || resp.Close {
			return err
		}
	}
}

func (s *Server) authorizeHTTP(req *http.Request) bool {
	value := req.Header.Get("Proxy-Authorization")
	scheme, payload, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	return err == nil && credentialsEqual(decoded, []byte(s.user+":"+s.password))
}

func writeHTTPError(w io.Writer, status int, message string, authenticate bool) error {
	body := message + "\n"
	headers := ""
	if authenticate {
		headers = "Proxy-Authenticate: Basic realm=\"sshx\"\r\n"
	}
	_, err := fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n%sContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), headers, len(body), body)
	return err
}

func (s *Server) serveConnect(client net.Conn, bufferedClient *bufio.Reader, req *http.Request) error {
	address := req.Host
	if _, _, err := net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(address, "443")
	}
	upstream, err := s.policy.dialContext(s.ctx, "tcp", address)
	if err != nil {
		_ = writeHTTPError(client, http.StatusBadGateway, err.Error(), false)
		return nil
	}
	defer upstream.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return err
	}
	return relay(s.ctx, client, bufferedClient, upstream)
}

func (s *Server) serveSOCKS5(client net.Conn, reader *bufio.Reader) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return err
	}
	if header[0] != 5 {
		return errors.New("unsupported SOCKS version")
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return err
	}
	hasUserPass := false
	for _, method := range methods {
		hasUserPass = hasUserPass || method == 2
	}
	if !hasUserPass {
		_, _ = client.Write([]byte{5, 0xff})
		return nil
	}
	if _, err := client.Write([]byte{5, 2}); err != nil {
		return err
	}
	if err := s.readSOCKSAuth(reader); err != nil {
		_, _ = client.Write([]byte{1, 1})
		return nil
	}
	if _, err := client.Write([]byte{1, 0}); err != nil {
		return err
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(reader, request); err != nil {
		return err
	}
	if request[0] != 5 || request[1] != 1 {
		_, _ = client.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return nil
	}
	host, err := readSOCKSHost(reader, request[3])
	if err != nil {
		_, _ = client.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0})
		return nil
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBytes); err != nil {
		return err
	}
	address := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBytes))))
	upstream, err := s.policy.dialContext(s.ctx, "tcp", address)
	if err != nil {
		_, _ = client.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return nil
	}
	defer upstream.Close()
	if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	return relay(s.ctx, client, reader, upstream)
}

func (s *Server) readSOCKSAuth(reader *bufio.Reader) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return err
	}
	if header[0] != 1 {
		return errors.New("unsupported SOCKS authentication version")
	}
	user := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, user); err != nil {
		return err
	}
	length, err := reader.ReadByte()
	if err != nil {
		return err
	}
	password := make([]byte, int(length))
	if _, err := io.ReadFull(reader, password); err != nil {
		return err
	}
	if !credentialsEqual(user, []byte(s.user)) || !credentialsEqual(password, []byte(s.password)) {
		return errors.New("invalid proxy credentials")
	}
	return nil
}

func credentialsEqual(left, right []byte) bool {
	return subtle.ConstantTimeCompare(left, right) == 1
}

func readSOCKSHost(reader *bufio.Reader, addressType byte) (string, error) {
	switch addressType {
	case 1:
		value := make([]byte, net.IPv4len)
		_, err := io.ReadFull(reader, value)
		return net.IP(value).String(), err
	case 4:
		value := make([]byte, net.IPv6len)
		_, err := io.ReadFull(reader, value)
		return net.IP(value).String(), err
	case 3:
		length, err := reader.ReadByte()
		if err != nil {
			return "", err
		}
		value := make([]byte, int(length))
		_, err = io.ReadFull(reader, value)
		return string(value), err
	default:
		return "", errors.New("unsupported SOCKS address type")
	}
}

func (p *Policy) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	upstream := p.selectGeneric(address)
	if upstream == nil {
		return (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, network, address)
	}
	switch strings.ToLower(upstream.Scheme) {
	case "http", "https":
		return dialHTTPProxy(ctx, upstream, address)
	case "socks5", "socks5h":
		return dialSOCKSProxy(ctx, upstream, address)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", upstream.Scheme)
	}
}

func (p *Policy) selectGeneric(address string) *url.URL {
	if p.bypass(address) {
		return nil
	}
	if p.explicit != nil {
		return p.explicit
	}
	if p.all != nil {
		return p.all
	}
	if p.https != nil {
		return p.https
	}
	return p.http
}

func (p *Policy) selectHTTP(scheme, address string) *url.URL {
	address = addressWithDefaultPort(scheme, address)
	if p.bypass(address) {
		return nil
	}
	if p.explicit != nil {
		return p.explicit
	}
	if strings.EqualFold(scheme, "https") && p.https != nil {
		return p.https
	}
	if strings.EqualFold(scheme, "http") && p.http != nil {
		return p.http
	}
	return p.all
}

func addressWithDefaultPort(scheme, address string) string {
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}
	port := "80"
	if strings.EqualFold(scheme, "https") {
		port = "443"
	}
	return net.JoinHostPort(strings.Trim(address, "[]"), port)
}

func (p *Policy) transportForHTTP(scheme, address string) *http.Transport {
	upstream := p.selectHTTP(scheme, address)
	key := "direct"
	if upstream != nil {
		key = upstream.String()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if transport := p.transports[key]; transport != nil {
		return transport
	}
	transport := &http.Transport{
		Proxy:             http.ProxyURL(upstream),
		DialContext:       (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true,
	}
	p.transports[key] = transport
	return transport
}

func (p *Policy) closeIdleConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, transport := range p.transports {
		transport.CloseIdleConnections()
	}
}

func (p *Policy) bypass(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	for _, raw := range p.noProxy {
		rule := strings.TrimSpace(raw)
		if rule == "" {
			continue
		}
		if rule == "*" {
			return true
		}
		ruleHost, rulePort, splitErr := net.SplitHostPort(rule)
		if splitErr == nil {
			if rulePort != port {
				continue
			}
			rule = ruleHost
		}
		if _, network, cidrErr := net.ParseCIDR(rule); cidrErr == nil {
			if ip != nil && network.Contains(ip) {
				return true
			}
			continue
		}
		rule = strings.Trim(strings.ToLower(rule), "[]")
		if strings.HasPrefix(rule, "*.") {
			rule = strings.TrimPrefix(rule, "*")
		}
		lowerHost := strings.ToLower(host)
		if strings.HasPrefix(rule, ".") {
			if strings.HasSuffix(lowerHost, rule) || lowerHost == strings.TrimPrefix(rule, ".") {
				return true
			}
			continue
		}
		if lowerHost == rule || strings.HasSuffix(lowerHost, "."+rule) {
			return true
		}
	}
	return false
}

func dialHTTPProxy(ctx context.Context, proxyURL *url.URL, address string) (net.Conn, error) {
	proxyAddress := withDefaultPort(proxyURL, "80", "443")
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(proxyURL.Scheme, "https") {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: proxyURL.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tlsConn
	}
	headers := ""
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + password))
		headers = "Proxy-Authorization: Basic " + token + "\r\n"
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", address, address, headers); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = conn.Close()
		return nil, fmt.Errorf("upstream proxy returned %s", response.Status)
	}
	if reader.Buffered() > 0 {
		return &bufferedConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

func dialSOCKSProxy(ctx context.Context, proxyURL *url.URL, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", withDefaultPort(proxyURL, "1080", "1080"))
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) {
		_ = conn.Close()
		return nil, err
	}
	method := byte(0)
	if proxyURL.User != nil {
		method = 2
	}
	if _, err := conn.Write([]byte{5, 1, method}); err != nil {
		return fail(err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fail(err)
	}
	if reply[0] != 5 || reply[1] != method {
		return fail(errors.New("upstream SOCKS proxy rejected authentication method"))
	}
	if method == 2 {
		password, _ := proxyURL.User.Password()
		user := []byte(proxyURL.User.Username())
		pass := []byte(password)
		if len(user) > 255 || len(pass) > 255 {
			return fail(errors.New("upstream SOCKS credentials are too long"))
		}
		payload := append([]byte{1, byte(len(user))}, user...)
		payload = append(payload, byte(len(pass)))
		payload = append(payload, pass...)
		if _, err := conn.Write(payload); err != nil {
			return fail(err)
		}
		if _, err := io.ReadFull(conn, reply); err != nil {
			return fail(err)
		}
		if reply[1] != 0 {
			return fail(errors.New("upstream SOCKS authentication failed"))
		}
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fail(err)
	}
	if strings.EqualFold(proxyURL.Scheme, "socks5") && net.ParseIP(host) == nil {
		ips, resolveErr := net.DefaultResolver.LookupHost(ctx, host)
		if resolveErr != nil || len(ips) == 0 {
			if resolveErr == nil {
				resolveErr = errors.New("proxy target did not resolve")
			}
			return fail(resolveErr)
		}
		host = ips[0]
	}
	target, err := socksAddress(host, port)
	if err != nil {
		return fail(err)
	}
	request := append([]byte{5, 1, 0}, target...)
	if _, err := conn.Write(request); err != nil {
		return fail(err)
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fail(err)
	}
	if header[1] != 0 {
		return fail(fmt.Errorf("upstream SOCKS connect failed with code %d", header[1]))
	}
	if err := discardSOCKSAddress(conn, header[3]); err != nil {
		return fail(err)
	}
	return conn, nil
}

func socksAddress(host, port string) ([]byte, error) {
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, errors.New("invalid target port")
	}
	var out []byte
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			out = append([]byte{1}, ipv4...)
		} else {
			out = append([]byte{4}, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, errors.New("target hostname is too long")
		}
		out = append([]byte{3, byte(len(host))}, host...)
	}
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(portNumber))
	return append(out, portBytes...), nil
}

func discardSOCKSAddress(reader io.Reader, addressType byte) error {
	length := 0
	switch addressType {
	case 1:
		length = net.IPv4len
	case 4:
		length = net.IPv6len
	case 3:
		var value [1]byte
		if _, err := io.ReadFull(reader, value[:]); err != nil {
			return err
		}
		length = int(value[0])
	default:
		return errors.New("invalid upstream SOCKS response")
	}
	_, err := io.CopyN(io.Discard, reader, int64(length+2))
	return err
}

func withDefaultPort(proxyURL *url.URL, httpPort, httpsPort string) string {
	if proxyURL.Port() != "" {
		return proxyURL.Host
	}
	port := httpPort
	if strings.EqualFold(proxyURL.Scheme, "https") {
		port = httpsPort
	}
	return net.JoinHostPort(proxyURL.Hostname(), port)
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func relay(ctx context.Context, client net.Conn, clientReader io.Reader, upstream net.Conn) error {
	errCh := make(chan error, 2)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
			_ = upstream.Close()
		case <-done:
		}
	}()
	go func() {
		_, err := io.Copy(upstream, clientReader)
		if closer, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(client, upstream)
		if closer, ok := client.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		errCh <- err
	}()
	first := <-errCh
	second := <-errCh
	if first != nil && !errors.Is(first, net.ErrClosed) {
		return first
	}
	if second != nil && !errors.Is(second, net.ErrClosed) {
		return second
	}
	return nil
}
