package netclient

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testHostname = "looking.test"

func TestTLSProfilesNegotiateHTTP1AndHTTP2(t *testing.T) {
	for _, tc := range []struct {
		name string
		h2   bool
	}{
		{name: "http1"},
		{name: "http2", h2: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, roots := newTLSServer(t, tc.h2, func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, r.Proto)
			})
			defer server.Close()

			client, err := New(Options{RootCAs: roots})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Get(server.URL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if got, want := resp.ProtoMajor, map[bool]int{true: 2, false: 1}[tc.h2]; got != want {
				t.Fatalf("protocol major = %d, want %d (%s)", got, want, resp.Proto)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(body), map[bool]string{true: "HTTP/2.0", false: "HTTP/1.1"}[tc.h2]; got != want {
				t.Fatalf("body = %q, want %q", got, want)
			}
		})
	}
}

func TestTLSRejectsUntrustedCertificate(t *testing.T) {
	server, _ := newTLSServer(t, false, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	defer server.Close()

	client, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(server.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("GET succeeded with an untrusted certificate")
	}
}

func TestClientTimeoutIncludesResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for {
			if _, err := io.WriteString(w, "x"); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(25 * time.Millisecond)
		}
	}))
	defer server.Close()

	client, err := New(Options{Timeout: 180 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("GET headers: %v", err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if err == nil {
		t.Fatal("body read succeeded past the client timeout")
	}
}

func TestContextCancellationClosesHTTP1Body(t *testing.T) {
	bodyStarted := make(chan struct{})
	serverCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(bodyStarted)
		<-r.Context().Done()
		close(serverCanceled)
	}))
	defer server.Close()

	client, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET headers: %v", err)
	}
	<-bodyStarted

	readDone := make(chan error, 1)
	go func() {
		_, readErr := io.Copy(io.Discard, resp.Body)
		readDone <- readErr
	}()
	cancel()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("body read completed without observing cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("body read remained blocked after request cancellation")
	}
	_ = resp.Body.Close()
	select {
	case <-serverCanceled:
	case <-time.After(time.Second):
		t.Fatal("server connection remained open after cancellation")
	}
}

func TestEarlyBodyCloseDoesNotDrainHTTP1Response(t *testing.T) {
	serverCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "x")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(serverCanceled)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := New(Options{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		_ = resp.Body.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("early body close attempted to drain the remaining response")
	}
	select {
	case <-serverCanceled:
	case <-time.After(time.Second):
		t.Fatal("early body close left the server connection open")
	}
}

func TestHTTPConnectUsesConnectIPAndPreservesOriginalTLSName(t *testing.T) {
	observedSNI := make(chan string, 1)
	observedHost := make(chan string, 1)
	server, roots := newTLSServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		observedSNI <- r.TLS.ServerName
		observedHost <- r.Host
		_, _ = io.WriteString(w, "ok")
	})
	defer server.Close()
	upstream := listenAddress(t, server.URL)

	proxy, connectTarget := newHTTPConnectProxy(t, upstream)
	defer proxy.Close()
	port := serverPort(t, server.URL)
	target := "https://" + net.JoinHostPort(testHostname, port) + "/"
	proxyURL := "http://" + proxy.Addr().String()
	client, err := New(Options{
		ProxyURL:    proxyURL,
		ConnectHost: testHostname,
		ConnectIP:   "127.0.0.1",
		RootCAs:     roots,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("GET through CONNECT: %v", err)
	}
	defer resp.Body.Close()
	if got := <-connectTarget; got != net.JoinHostPort("127.0.0.1", port) {
		t.Fatalf("CONNECT target = %q", got)
	}
	if got := <-observedSNI; got != testHostname {
		t.Fatalf("TLS SNI = %q, want %q", got, testHostname)
	}
	if got := <-observedHost; got != net.JoinHostPort(testHostname, port) {
		t.Fatalf("HTTP Host = %q", got)
	}
}

func TestSOCKS5hSupportsRemoteHostnameAndForcedLiteralDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer server.Close()
	upstream := listenAddress(t, server.URL)
	port := serverPort(t, server.URL)

	t.Run("remote hostname", func(t *testing.T) {
		proxy, destination := newSOCKS5Proxy(t, upstream)
		defer proxy.Close()
		client, err := New(Options{ProxyURL: "socks5h://" + proxy.Addr().String()})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Get("http://" + net.JoinHostPort(testHostname, port) + "/")
		if err != nil {
			t.Fatalf("GET through SOCKS5h: %v", err)
		}
		defer resp.Body.Close()
		if got := <-destination; got != testHostname {
			t.Fatalf("SOCKS destination = %q, want remote hostname %q", got, testHostname)
		}
	})

	t.Run("forced family uses literal destination", func(t *testing.T) {
		proxy, destination := newSOCKS5Proxy(t, upstream)
		defer proxy.Close()
		client, err := New(Options{ProxyURL: "socks5h://" + proxy.Addr().String(), Family: "4"})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Get("http://localhost:" + port + "/")
		if err != nil {
			t.Fatalf("GET through forced-family SOCKS5h: %v", err)
		}
		defer resp.Body.Close()
		if got := <-destination; got != "127.0.0.1" {
			t.Fatalf("SOCKS destination = %q, want locally resolved IPv4 literal", got)
		}
	})
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{name: "profile", opts: Options{Profile: "chrome"}},
		{name: "family", opts: Options{Family: "5"}},
		{name: "proxy scheme", opts: Options{ProxyURL: "ftp://proxy.invalid"}},
		{name: "proxy credentials", opts: Options{ProxyURL: "http://user:secret@proxy.invalid"}},
		{name: "incomplete pin", opts: Options{ConnectIP: "127.0.0.1"}},
		{name: "invalid pin", opts: Options{ConnectHost: testHostname, ConnectIP: "not-an-ip"}},
		{name: "negative timeout", opts: Options{Timeout: -time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if client, err := New(tc.opts); err == nil {
				if client != nil {
					t.Fatal("New returned a client for invalid options")
				}
				t.Fatal("New accepted invalid options")
			}
		})
	}
}

func newTLSServer(t *testing.T, enableHTTP2 bool, handler http.HandlerFunc) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	cert, roots := testCertificate(t)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	server.EnableHTTP2 = enableHTTP2
	server.StartTLS()
	return server, roots
}

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: testHostname},
		DNSNames:              []string{testHostname},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pair.Leaf = root
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return pair, roots
}

func listenAddress(t *testing.T, serverURL string) string {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	return net.JoinHostPort("127.0.0.1", u.Port())
}

func serverPort(t *testing.T, serverURL string) string {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

func newHTTPConnectProxy(t *testing.T, upstream string) (net.Listener, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gotTarget := make(chan string, 1)
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				reader := bufio.NewReader(client)
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				fields := strings.Fields(line)
				if len(fields) != 3 || fields[0] != http.MethodConnect {
					return
				}
				for {
					line, err = reader.ReadString('\n')
					if err != nil || line == "\r\n" || line == "\n" {
						break
					}
				}
				gotTarget <- fields[1]
				upstreamConn, err := net.Dial("tcp", upstream)
				if err != nil {
					return
				}
				defer upstreamConn.Close()
				_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
				tunnel(client, reader, upstreamConn)
			}()
		}
	}()
	return listener, gotTarget
}

func newSOCKS5Proxy(t *testing.T, upstream string) (net.Listener, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gotDestination := make(chan string, 1)
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				greeting := make([]byte, 2)
				if _, err := io.ReadFull(client, greeting); err != nil {
					return
				}
				methods := make([]byte, int(greeting[1]))
				if _, err := io.ReadFull(client, methods); err != nil {
					return
				}
				if _, err := client.Write([]byte{5, 0}); err != nil {
					return
				}
				request := make([]byte, 4)
				if _, err := io.ReadFull(client, request); err != nil || request[0] != 5 || request[1] != 1 {
					return
				}
				var destination string
				switch request[3] {
				case 1:
					addr := make([]byte, net.IPv4len)
					if _, err := io.ReadFull(client, addr); err != nil {
						return
					}
					destination = net.IP(addr).String()
				case 3:
					length := []byte{0}
					if _, err := io.ReadFull(client, length); err != nil {
						return
					}
					name := make([]byte, int(length[0]))
					if _, err := io.ReadFull(client, name); err != nil {
						return
					}
					destination = string(name)
				case 4:
					addr := make([]byte, net.IPv6len)
					if _, err := io.ReadFull(client, addr); err != nil {
						return
					}
					destination = net.IP(addr).String()
				default:
					return
				}
				port := make([]byte, 2)
				if _, err := io.ReadFull(client, port); err != nil {
					return
				}
				gotDestination <- destination
				upstreamConn, err := net.Dial("tcp", upstream)
				if err != nil {
					_, _ = client.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer upstreamConn.Close()
				if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
					return
				}
				tunnel(client, client, upstreamConn)
			}()
		}
	}()
	return listener, gotDestination
}

func tunnel(client net.Conn, buffered io.Reader, upstream net.Conn) {
	go func() {
		_, _ = io.Copy(upstream, buffered)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	_, _ = io.Copy(client, upstream)
	if tcp, ok := client.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}
