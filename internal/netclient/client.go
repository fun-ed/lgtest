package netclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
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

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// Options configures the network behavior of a looking-glass HTTP client.
type Options struct {
	ProxyURL    string
	Family      string
	Profile     string
	ConnectHost string
	ConnectIP   string
	Timeout     time.Duration
	RootCAs     *x509.CertPool
}

// New creates a client that uses a fresh connection for every request.
func New(opts Options) (*http.Client, error) {
	family := opts.Family
	if family == "" {
		family = "auto"
	}
	if family != "auto" && family != "4" && family != "6" {
		return nil, errors.New("netclient: family must be auto, 4, or 6")
	}
	profile := opts.Profile
	if profile == "" {
		profile = "firefox"
	}
	if profile != "firefox" && profile != "go" {
		return nil, errors.New("netclient: profile must be firefox or go")
	}
	if opts.Timeout < 0 {
		return nil, errors.New("netclient: timeout cannot be negative")
	}
	if (opts.ConnectHost == "") != (opts.ConnectIP == "") {
		return nil, errors.New("netclient: connect host and IP must be provided together")
	}
	if opts.ConnectHost != "" {
		if !validConnectHost(opts.ConnectHost) {
			return nil, errors.New("netclient: invalid connect host")
		}
		if _, err := parseIP(opts.ConnectIP); err != nil {
			return nil, errors.New("netclient: connect IP must be a literal IP address")
		}
	}

	proxy, err := parseProxyURL(opts.ProxyURL)
	if err != nil {
		return nil, err
	}
	transport := &roundTripper{
		family:      family,
		profile:     profile,
		connectHost: opts.ConnectHost,
		connectIP:   opts.ConnectIP,
		rootCAs:     opts.RootCAs,
		proxy:       proxy,
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       opts.Timeout,
		CheckRedirect: checkRedirect,
	}, nil
}

type proxyConfig struct {
	scheme string
	host   string
	port   string
}

type roundTripper struct {
	family      string
	profile     string
	connectHost string
	connectIP   string
	rootCAs     *x509.CertPool
	proxy       *proxyConfig
}

func parseProxyURL(raw string) (*proxyConfig, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("netclient: invalid proxy URL or proxy credentials are not supported")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "socks5", "socks5h":
	default:
		return nil, errors.New("netclient: proxy scheme must be http, socks5, or socks5h")
	}
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, "\r\n") {
		return nil, errors.New("netclient: invalid proxy URL")
	}
	port, explicit, valid := authorityPort(u.Host)
	if !valid {
		return nil, errors.New("netclient: invalid proxy URL")
	}
	if !explicit {
		switch scheme {
		case "http":
			port = "80"
		default:
			port = "1080"
		}
	}
	return &proxyConfig{scheme: scheme, host: host, port: port}, nil
}

func authorityPort(authority string) (port string, explicit, valid bool) {
	if strings.HasPrefix(authority, "[") {
		closeBracket := strings.LastIndexByte(authority, ']')
		if closeBracket < 0 {
			return "", false, false
		}
		rest := authority[closeBracket+1:]
		if rest == "" {
			return "", false, true
		}
		if !strings.HasPrefix(rest, ":") {
			return "", false, false
		}
		port, explicit = rest[1:], true
	} else if strings.Count(authority, ":") > 1 {
		return "", false, false
	} else if i := strings.LastIndexByte(authority, ':'); i >= 0 {
		port, explicit = authority[i+1:], true
	}
	if !explicit {
		return "", false, true
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", true, false
	}
	return strconv.Itoa(n), true, true
}

func validConnectHost(host string) bool {
	if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "\r\n/@\\[]") {
		return false
	}
	return true
}

func parseIP(value string) (net.IP, error) {
	ip := net.ParseIP(value)
	if ip == nil {
		return nil, errors.New("invalid IP")
	}
	return ip, nil
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("netclient: stopped after 10 redirects")
	}
	if len(via) != 0 && strings.EqualFold(via[len(via)-1].URL.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
		return errors.New("netclient: HTTPS downgrade redirects are not allowed")
	}
	if req.URL.User != nil || req.Header.Get("Authorization") != "" || req.Header.Get("Proxy-Authorization") != "" {
		return errors.New("netclient: redirects containing credentials are not allowed")
	}
	return nil
}

func (t *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("netclient: request URL is required")
	}
	if req.URL.User != nil {
		return nil, errors.New("netclient: URL credentials are not supported")
	}
	scheme := strings.ToLower(req.URL.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, errors.New("netclient: only HTTP and HTTPS URLs are supported")
	}
	host, port, err := requestHostPort(req.URL)
	if err != nil {
		return nil, fmt.Errorf("netclient: invalid request authority: %w", err)
	}
	ctx := req.Context()
	conn, err := t.dialRoute(ctx, host, port)
	if err != nil {
		return nil, err
	}
	cleanup := &connectionCleanup{conn: conn}
	stopCancel := context.AfterFunc(ctx, func() { _ = cleanup.Close() })
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			stopCancel()
			_ = cleanup.Close()
			return nil, fmt.Errorf("netclient set connection deadline: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		stopCancel()
		_ = cleanup.Close()
		return nil, err
	}

	var tlsState *tls.ConnectionState
	if scheme == "https" {
		secureConn, state, err := t.handshake(ctx, conn, host)
		if err != nil {
			stopCancel()
			_ = cleanup.Close()
			return nil, err
		}
		tlsState = &state
		if state.NegotiatedProtocol == "h2" {
			h2Conn, err := (&http2.Transport{}).NewClientConn(secureConn)
			if err != nil {
				stopCancel()
				_ = cleanup.Close()
				return nil, fmt.Errorf("netclient initialize HTTP/2: %w", err)
			}
			cleanup.setHTTP2(h2Conn)
			resp, err := h2Conn.RoundTrip(req)
			if err != nil {
				stopCancel()
				_ = cleanup.Close()
				return nil, fmt.Errorf("netclient HTTP/2 request: %w", err)
			}
			if resp.Request == nil {
				resp.Request = req
			}
			resp.TLS = tlsState
			resp.Body = &managedBody{body: resp.Body, cleanup: cleanup, stopCancel: stopCancel}
			return resp, nil
		}
		if state.NegotiatedProtocol != "" && state.NegotiatedProtocol != "http/1.1" {
			stopCancel()
			_ = cleanup.Close()
			return nil, fmt.Errorf("netclient TLS negotiated unsupported protocol %q", state.NegotiatedProtocol)
		}
		conn = secureConn
	}

	resp, err := roundTripHTTP1(conn, req)
	if err != nil {
		stopCancel()
		_ = cleanup.Close()
		return nil, fmt.Errorf("netclient HTTP/1.1 request: %w", err)
	}
	if resp.Request == nil {
		resp.Request = req
	}
	resp.TLS = tlsState
	resp.Body = &managedBody{body: resp.Body, cleanup: cleanup, stopCancel: stopCancel}
	return resp, nil
}

func requestHostPort(u *url.URL) (host, port string, err error) {
	host = u.Hostname()
	if host == "" || strings.ContainsAny(host, "\r\n") {
		return "", "", errors.New("missing hostname")
	}
	port, explicit, valid := authorityPort(u.Host)
	if !valid {
		return "", "", errors.New("invalid port")
	}
	if !explicit {
		if strings.EqualFold(u.Scheme, "http") {
			port = "80"
		} else {
			port = "443"
		}
	}
	return host, port, nil
}

func (t *roundTripper) dialRoute(ctx context.Context, host, port string) (net.Conn, error) {
	if t.proxy == nil {
		address, err := t.destinationAddress(ctx, host, port)
		if err != nil {
			return nil, err
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, fmt.Errorf("netclient dial target: %w", err)
		}
		return conn, nil
	}

	proxyAddress := net.JoinHostPort(t.proxy.host, t.proxy.port)
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("netclient dial proxy: %w", err)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("netclient set proxy deadline: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	switch t.proxy.scheme {
	case "http":
		targetHost := host
		if t.pinMatches(host) || t.family != "auto" {
			address, err := t.destinationAddress(ctx, host, port)
			if err != nil {
				_ = conn.Close()
				return nil, err
			}
			targetHost, _, err = net.SplitHostPort(address)
			if err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("netclient prepare CONNECT target: %w", err)
			}
		}
		tunneled, err := httpConnect(ctx, conn, targetHost, port)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		return tunneled, nil
	case "socks5", "socks5h":
		destination, remoteDNS, err := t.socksDestination(ctx, host)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		if err := socks5Connect(conn, destination, port, remoteDNS); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("netclient SOCKS5 negotiation: %w", err)
		}
		return conn, nil
	default:
		_ = conn.Close()
		return nil, errors.New("netclient: unsupported proxy scheme")
	}
}

func (t *roundTripper) destinationAddress(ctx context.Context, host, port string) (string, error) {
	var target net.IPAddr
	if t.pinMatches(host) {
		ip, _ := parseIP(t.connectIP)
		target = net.IPAddr{IP: ip}
	} else if literal, err := parseIP(host); err == nil {
		target = net.IPAddr{IP: literal}
	} else if t.family == "auto" {
		return net.JoinHostPort(host, port), nil
	} else {
		resolved, err := resolveDestination(ctx, host, t.family)
		if err != nil {
			return "", fmt.Errorf("netclient resolve destination: %w", err)
		}
		target = resolved
	}
	if t.family != "auto" && !ipMatchesFamily(target.IP, t.family) {
		return "", errors.New("netclient destination IP does not match requested family")
	}
	return net.JoinHostPort(target.String(), port), nil
}

func (t *roundTripper) socksDestination(ctx context.Context, host string) (string, bool, error) {
	if t.pinMatches(host) {
		ip, _ := parseIP(t.connectIP)
		if t.family != "auto" && !ipMatchesFamily(ip, t.family) {
			return "", false, errors.New("netclient destination IP does not match requested family")
		}
		return ip.String(), false, nil
	}
	if literal, err := parseIP(host); err == nil {
		if t.family != "auto" && !ipMatchesFamily(literal, t.family) {
			return "", false, errors.New("netclient destination IP does not match requested family")
		}
		return literal.String(), false, nil
	}
	if t.proxy.scheme == "socks5h" && t.family == "auto" {
		return host, true, nil
	}
	resolved, err := resolveDestination(ctx, host, t.family)
	if err != nil {
		return "", false, fmt.Errorf("netclient resolve destination: %w", err)
	}
	return resolved.String(), false, nil
}

func (t *roundTripper) pinMatches(host string) bool {
	return t.connectHost != "" && normalizedHost(host) == normalizedHost(t.connectHost)
}

func normalizedHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

func resolveDestination(ctx context.Context, host, family string) (net.IPAddr, error) {
	if literal, err := parseIP(host); err == nil {
		if family != "auto" && !ipMatchesFamily(literal, family) {
			return net.IPAddr{}, errors.New("literal destination has the wrong address family")
		}
		return net.IPAddr{IP: literal}, nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return net.IPAddr{}, err
	}
	for _, ip := range ips {
		if family == "auto" || ipMatchesFamily(ip.IP, family) {
			return ip, nil
		}
	}
	return net.IPAddr{}, errors.New("no destination address matches requested family")
}

func ipMatchesFamily(ip net.IP, family string) bool {
	if family == "auto" {
		return true
	}
	is4 := ip.To4() != nil
	return family == "4" && is4 || family == "6" && !is4 && ip.To16() != nil
}

func httpConnect(ctx context.Context, conn net.Conn, host, port string) (net.Conn, error) {
	if strings.ContainsAny(host, "\r\n") {
		return nil, errors.New("netclient proxy CONNECT target is invalid")
	}
	authority := net.JoinHostPort(host, port)
	if _, err := io.WriteString(conn, "CONNECT "+authority+" HTTP/1.1\r\nHost: "+authority+"\r\n\r\n"); err != nil {
		return nil, fmt.Errorf("netclient proxy CONNECT write: %w", err)
	}
	reader := bufio.NewReader(conn)
	connectReq := &http.Request{Method: http.MethodConnect}
	resp, err := http.ReadResponse(reader, connectReq)
	if err != nil {
		return nil, fmt.Errorf("netclient proxy CONNECT response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("netclient proxy CONNECT failed with status %d", resp.StatusCode)
	}
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

func socks5Connect(conn net.Conn, destination, port string, remoteDNS bool) error {
	if len(destination) == 0 {
		return errors.New("empty destination")
	}
	if len(destination) > 255 && remoteDNS {
		return errors.New("destination hostname is too long")
	}
	if err := writeFull(conn, []byte{5, 1, 0}); err != nil {
		return fmt.Errorf("write greeting: %w", err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(conn, method); err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}
	if method[0] != 5 || method[1] != 0 {
		return errors.New("proxy does not support unauthenticated SOCKS5")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("invalid destination port")
	}
	request := []byte{5, 1, 0}
	if ip := net.ParseIP(destination); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			request = append(request, 1)
			request = append(request, ipv4...)
		} else {
			request = append(request, 4)
			request = append(request, ip.To16()...)
		}
	} else {
		if !remoteDNS {
			return errors.New("local SOCKS destination must be an IP address")
		}
		request = append(request, 3, byte(len(destination)))
		request = append(request, destination...)
	}
	request = append(request, byte(portNumber>>8), byte(portNumber))
	if err := writeFull(conn, request); err != nil {
		return fmt.Errorf("write connect request: %w", err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("read connect reply: %w", err)
	}
	if reply[0] != 5 || reply[2] != 0 {
		return errors.New("invalid SOCKS5 connect reply")
	}
	if reply[1] != 0 {
		return fmt.Errorf("SOCKS5 connect rejected with status %d", reply[1])
	}
	var addressLength int
	switch reply[3] {
	case 1:
		addressLength = net.IPv4len
	case 4:
		addressLength = net.IPv6len
	case 3:
		length := []byte{0}
		if _, err := io.ReadFull(conn, length); err != nil {
			return fmt.Errorf("read bound address: %w", err)
		}
		addressLength = int(length[0])
	default:
		return errors.New("invalid SOCKS5 bound address type")
	}
	bound := make([]byte, addressLength+2)
	if _, err := io.ReadFull(conn, bound); err != nil {
		return fmt.Errorf("read bound address: %w", err)
	}
	return nil
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (t *roundTripper) handshake(ctx context.Context, conn net.Conn, host string) (net.Conn, tls.ConnectionState, error) {
	protocols := []string{"h2", "http/1.1"}
	if t.profile == "go" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: host,
			RootCAs:    t.rootCAs,
			NextProtos: protocols,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, tls.ConnectionState{}, fmt.Errorf("netclient TLS handshake: %w", err)
		}
		return tlsConn, tlsConn.ConnectionState(), nil
	}
	utlsConn := utls.UClient(conn, &utls.Config{
		ServerName: host,
		RootCAs:    t.rootCAs,
		NextProtos: protocols,
	}, utls.HelloFirefox_120)
	if err := utlsConn.HandshakeContext(ctx); err != nil {
		return nil, tls.ConnectionState{}, fmt.Errorf("netclient TLS handshake: %w", err)
	}
	state := standardTLSState(utlsConn.ConnectionState())
	return &tlsStateConn{Conn: utlsConn, state: state}, state, nil
}

func standardTLSState(state utls.ConnectionState) tls.ConnectionState {
	return tls.ConnectionState{
		Version:                     state.Version,
		HandshakeComplete:           state.HandshakeComplete,
		DidResume:                   state.DidResume,
		CipherSuite:                 state.CipherSuite,
		NegotiatedProtocol:          state.NegotiatedProtocol,
		NegotiatedProtocolIsMutual:  state.NegotiatedProtocolIsMutual,
		ServerName:                  state.ServerName,
		PeerCertificates:            state.PeerCertificates,
		VerifiedChains:              state.VerifiedChains,
		SignedCertificateTimestamps: state.SignedCertificateTimestamps,
		OCSPResponse:                state.OCSPResponse,
		TLSUnique:                   state.TLSUnique,
		ECHAccepted:                 state.ECHAccepted,
	}
}

func roundTripHTTP1(conn net.Conn, req *http.Request) (*http.Response, error) {
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return resp, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

type tlsStateConn struct {
	net.Conn
	state tls.ConnectionState
}

func (c *tlsStateConn) ConnectionState() tls.ConnectionState {
	return c.state
}

type connectionCleanup struct {
	mu     sync.Mutex
	conn   net.Conn
	h2Conn *http2.ClientConn
	closed bool
}

func (c *connectionCleanup) setHTTP2(conn *http2.ClientConn) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = conn.Close()
		return
	}
	c.h2Conn = conn
	c.mu.Unlock()
}

func (c *connectionCleanup) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	h2Conn := c.h2Conn
	c.mu.Unlock()
	if h2Conn != nil {
		_ = h2Conn.Close()
	}
	return c.conn.Close()
}

type managedBody struct {
	body       io.ReadCloser
	cleanup    *connectionCleanup
	stopCancel func() bool
	once       sync.Once
}

func (b *managedBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil {
		_ = b.Close()
	}
	return n, err
}

func (b *managedBody) Close() error {
	var err error
	b.once.Do(func() {
		b.stopCancel()
		// Do not let HTTP/1.1 Body.Close drain the rest of a capped download.
		closeErr := b.cleanup.Close()
		err = b.body.Close()
		if err == nil {
			err = closeErr
		}
	})
	return err
}
