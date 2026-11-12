package core

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("merhaba"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// getOver: verilen bağlantı üzerinden TLS el sıkışması + HTTP isteği yapar.
func getOver(t *testing.T, c net.Conn, sni string) string {
	t.Helper()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true, ServerName: sni})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("el sıkışma başarısız: %v", err)
	}
	tc.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"))
	b, _ := io.ReadAll(tc)
	return string(b)
}

func TestFindSNI(t *testing.T) {
	c, s := net.Pipe()
	go func() { tls.Client(c, &tls.Config{ServerName: "discord.com"}).Handshake() }()
	rec, isTLS, err := readFirstRecord(s)
	if err != nil || !isTLS {
		t.Fatalf("kayıt okunamadı: %v %v", err, isTLS)
	}
	st, l, ok := findSNI(rec[5:])
	if !ok || string(rec[5+st:5+st+l]) != "discord.com" {
		t.Fatalf("SNI bulunamadı (ok=%v)", ok)
	}
	c.Close()
	s.Close()
}

// helloConn: ilk yazmayı (ClientHello) seçilen yöntemle parçalayarak gönderir.
type helloConn struct {
	net.Conn
	st    int
	first bool
}

func (h *helloConn) Write(b []byte) (int, error) {
	if !h.first {
		h.first = true
		if err := sendHello(h.Conn, b, h.st); err != nil {
			return 0, err
		}
		return len(b), nil
	}
	return h.Conn.Write(b)
}

// Her yöntem gerçek bir TLS sunucusu tarafından kabul edilmeli.
func TestEveryStrategyHandshakes(t *testing.T) {
	srv := newBackend(t)
	for st := 0; st < numStrategies; st++ {
		t.Run(strategyName(st), func(t *testing.T) {
			c, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			out := getOver(t, &helloConn{Conn: c, st: st}, "discord.com")
			if !strings.Contains(out, "merhaba") {
				t.Fatalf("beklenmeyen yanıt: %q", out)
			}
		})
	}
}

// fakeDPI: ilk TCP parçası tam 1 bayt değilse bağlantıyı koparan sahte DPI.
func fakeDPI(t *testing.T, backend string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				c.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, err := c.Read(buf)
				if err != nil || n != 1 {
					return // "engelle": veri göndermeden kapat
				}
				r, err := net.Dial("tcp", backend)
				if err != nil {
					return
				}
				defer r.Close()
				r.Write(buf[:n])
				c.SetReadDeadline(time.Time{})
				go io.Copy(r, c)
				io.Copy(c, r)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// Otomatik mod, ilk yöntemler engellenince çalışanı bulup hatırlamalı.
func TestAutoFallbackFindsWorkingStrategy(t *testing.T) {
	srv := newBackend(t)
	dpi := fakeDPI(t, srv.Listener.Addr().String())
	resetLearned()
	SetMode("auto")

	appEnd, flowEnd := net.Pipe()
	go func() {
		defer flowEnd.Close()
		tlsFlow(flowEnd, dpi)
	}()
	out := getOver(t, appEnd, "blocked.example")
	appEnd.Close()
	if !strings.Contains(out, "merhaba") {
		t.Fatalf("beklenmeyen yanıt: %q", out)
	}
	if got := int(lastGood.Load()); got != sFirst {
		t.Fatalf("öğrenilen yöntem %s, beklenen %s", strategyName(got), strategyName(sFirst))
	}
	host, _, _ := net.SplitHostPort(dpi)
	if v, ok := learned.Load(host); !ok || v.(int) != sFirst {
		t.Fatalf("hedef için yöntem kaydedilmedi")
	}
	// İkinci bağlantı doğrudan öğrenilen yöntemle başlamalı.
	if o := order(host); o[0] != sFirst {
		t.Fatalf("sıra yanlış: %v", o)
	}
	// Ağ değişince hafıza sıfırlanmalı.
	NetworkChanged()
	if o := order(host); o[0] != sSNI {
		t.Fatalf("sıfırlama sonrası sıra yanlış: %v", o)
	}
}

// Sabit modda yedek yöntem denenmemeli.
func TestFixedModeNoFallback(t *testing.T) {
	srv := newBackend(t)
	dpi := fakeDPI(t, srv.Listener.Addr().String())
	SetMode("s0")
	defer SetMode("auto")
	appEnd, flowEnd := net.Pipe()
	go func() {
		defer flowEnd.Close()
		tlsFlow(flowEnd, dpi)
	}()
	appEnd.SetDeadline(time.Now().Add(10 * time.Second))
	err := tls.Client(appEnd, &tls.Config{InsecureSkipVerify: true, ServerName: "x"}).Handshake()
	if err == nil {
		t.Fatal("sabit modda engelli yöntemle el sıkışma başarılı olmamalıydı")
	}
}

func TestDoHAndDNSLoop(t *testing.T) {
	var got int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/dns-message" {
			http.Error(w, "kötü istek", 400)
			return
		}
		b, _ := io.ReadAll(r.Body)
		got++
		b[2] |= 0x80      // yanıt bayrağı
		b[0], b[1] = 0, 0 // sunucu kimliği sıfırlasa bile biz geri yazmalıyız
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(b)
	}))
	defer srv.Close()

	oldS, oldT := dohServers, dohClient.Transport.(*http.Transport).TLSClientConfig
	dohServers = []string{srv.URL}
	dohClient.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	defer func() {
		dohServers = oldS
		dohClient.Transport.(*http.Transport).TLSClientConfig = oldT
	}()

	q := []byte{0xAB, 0xCD, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c', 0, 0, 1, 0, 1}
	resp := dohQuery(q)
	if resp == nil || resp[0] != 0xAB || resp[1] != 0xCD || resp[2]&0x80 == 0 {
		t.Fatalf("DoH yanıtı hatalı: %v", resp)
	}

	// UDP DNS döngüsü: bir "uygulama" soru gönderir, DoH yanıtı geri gelir.
	app, flow := net.Pipe()
	go func() { defer flow.Close(); dnsLoop(flow, "127.0.0.1:9") }()
	app.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := app.Write(q); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, err := app.Read(buf)
	if err != nil || n != len(q) || buf[0] != 0xAB || buf[2]&0x80 == 0 {
		t.Fatalf("DNS döngüsü yanıtı hatalı: n=%d err=%v", n, err)
	}
	app.Close()
}

func TestHTTPFlowSplitsHost(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	chunks := make(chan int, 4)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		total := 0
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		for !strings.Contains(string(buf[:total]), "\r\n\r\n") {
			n, err := c.Read(buf[total:])
			if n > 0 {
				chunks <- n
			}
			total += n
			if err != nil {
				break
			}
		}
		c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"))
	}()
	app, flow := net.Pipe()
	go func() { defer flow.Close(); httpFlow(flow, ln.Addr().String()) }()
	app.SetDeadline(time.Now().Add(5 * time.Second))
	app.Write([]byte("GET / HTTP/1.1\r\nHost: blocked.example\r\n\r\n"))
	b, _ := io.ReadAll(app)
	if !strings.Contains(string(b), "ok") {
		t.Fatalf("yanıt yok: %q", b)
	}
	if len(chunks) < 2 {
		t.Fatalf("Host başlığı bölünmedi (parça sayısı=%d)", len(chunks))
	}
}
