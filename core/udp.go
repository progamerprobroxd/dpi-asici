package core

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// DoH sunucuları IP ile çağrılır: ne sistem DNS'i ne de SNI kullanılır.
var dohServers = []string{
	"https://1.1.1.1/dns-query",
	"https://8.8.8.8/dns-query",
	"https://9.9.9.9/dns-query",
}

var (
	dohFirst  atomic.Int32
	dohClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     60 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
		},
	}
)

// dohQuery: ham DNS sorgusunu DoH ile çözer (RFC 8484). Başarısızsa nil döner.
func dohQuery(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	n := len(dohServers)
	start := int(dohFirst.Load())
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		req, err := http.NewRequest("POST", dohServers[idx], bytes.NewReader(q))
		if err != nil {
			return nil
		}
		req.Header.Set("Content-Type", "application/dns-message")
		req.Header.Set("Accept", "application/dns-message")
		resp, err := dohClient.Do(req)
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 65535))
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || len(b) < 12 {
			continue
		}
		b[0], b[1] = q[0], q[1] // sorgu kimliğini koru
		dohFirst.Store(int32(idx))
		return b
	}
	return nil
}

// plainDNS: DoH çalışmazsa sorguyu uygulamanın istediği sunucuya düz iletir.
func plainDNS(q []byte, dst string) []byte {
	c, err := net.DialTimeout("udp", dst, 3*time.Second)
	if err != nil {
		return nil
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(q); err != nil {
		return nil
	}
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}

func handleUDP(c net.Conn, dst string, port uint16) {
	defer c.Close()
	if port == 53 {
		dnsLoop(c, dst)
		return
	}
	relayUDP(c, dst)
}

func dnsLoop(c net.Conn, dst string) {
	buf := make([]byte, 4096)
	for {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			statDNS.Add(1)
			resp := dohQuery(q)
			if resp == nil {
				resp = plainDNS(q, dst)
			}
			if resp != nil {
				c.Write(resp)
			}
		}()
	}
}

func relayUDP(c net.Conn, dst string) {
	r, err := net.DialTimeout("udp", dst, 5*time.Second)
	if err != nil {
		return
	}
	defer r.Close()
	done := make(chan struct{}, 2)
	go copyPackets(r, c, &statUp, done)
	go copyPackets(c, r, &statDown, done)
	<-done
}

func copyPackets(dst, src net.Conn, ctr *atomic.Int64, done chan<- struct{}) {
	buf := make([]byte, 2048)
	for {
		src.SetReadDeadline(time.Now().Add(2 * time.Minute))
		n, err := src.Read(buf)
		if err != nil {
			break
		}
		ctr.Add(int64(n))
		if _, err := dst.Write(buf[:n]); err != nil {
			break
		}
	}
	done <- struct{}{}
}
