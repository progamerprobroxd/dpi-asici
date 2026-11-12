//go:build linux && !android

package core

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

func cpuTime() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// Isınma regresyonu: boşta iken neredeyse hiç CPU harcanmamalı,
// veri akarken de makul bir maliyet olmalı.
func TestIdleAndThroughputCPU(t *testing.T) {
	tun := setupTUN(t, "dpitest1", "10.201.0.1", "255.255.255.0")
	defer tun.Close()

	const size = 150 << 20 // 150 MB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64<<10)
		for sent := 0; sent < size; sent += len(buf) {
			w.Write(buf)
		}
	}))
	defer srv.Close()

	const fake = "10.201.0.50:80"
	old := mapDst
	mapDst = func(s string) string {
		if s == fake {
			return srv.Listener.Addr().String()
		}
		return s
	}
	defer func() { mapDst = old }()

	if err := Start(int(tun.Fd()), 1500, "auto"); err != nil {
		t.Fatal(err)
	}
	defer Stop()

	// 1) Boşta CPU
	time.Sleep(500 * time.Millisecond)
	c0 := cpuTime()
	time.Sleep(4 * time.Second)
	idle := cpuTime() - c0
	t.Logf("4 sn boşta CPU: %v", idle)
	if idle > 200*time.Millisecond {
		t.Errorf("boşta CPU çok yüksek: %v", idle)
	}

	// 2) Aktarım
	c, err := net.DialTimeout("tcp", fake, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.0\r\nHost: x\r\n\r\n"))
	c1, t1 := cpuTime(), time.Now()
	n, _ := io.Copy(io.Discard, c)
	el, cpu := time.Since(t1), cpuTime()-c1
	t.Logf("aktarım: %.0f MB, %.1f sn, %.0f MB/sn, CPU %.2f sn (hem sunucu hem istemci hem çekirdek dahil)",
		float64(n)/1e6, el.Seconds(), float64(n)/1e6/el.Seconds(), cpu.Seconds())
	if n < size/2 {
		t.Errorf("veri eksik geldi: %d", n)
	}
}
