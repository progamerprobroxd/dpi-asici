//go:build linux && !android

package core

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// Gerçek bir TUN aygıtı kurar (Android'deki VpnService'in yaptığının aynısı).
func setupTUN(t *testing.T, name, ip, mask string) *os.File {
	t.Helper()
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("TUN açılamadı: %v", err)
	}
	var ifr struct {
		Name  [16]byte
		Flags uint16
		_     [22]byte
	}
	copy(ifr.Name[:], name)
	ifr.Flags = 0x0001 | 0x1000 // IFF_TUN | IFF_NO_PI
	if err := ioctl(int(f.Fd()), 0x400454ca, unsafe.Pointer(&ifr)); err != nil {
		f.Close()
		t.Skipf("TUNSETIFF başarısız (izin yok?): %v", err)
	}

	s, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		f.Close()
		t.Skipf("soket: %v", err)
	}
	defer syscall.Close(s)
	setAddr := func(req uintptr, addr string) error {
		var r struct {
			Name   [16]byte
			Family uint16
			Port   uint16
			Addr   [4]byte
			_      [16]byte
		}
		copy(r.Name[:], name)
		r.Family = syscall.AF_INET
		copy(r.Addr[:], net.ParseIP(addr).To4())
		return ioctl(s, req, unsafe.Pointer(&r))
	}
	if err := setAddr(0x8916, ip); err != nil { // SIOCSIFADDR
		f.Close()
		t.Skipf("adres atanamadı: %v", err)
	}
	if err := setAddr(0x891c, mask); err != nil { // SIOCSIFNETMASK
		f.Close()
		t.Skipf("maske atanamadı: %v", err)
	}
	var fl struct {
		Name  [16]byte
		Flags int16
		_     [22]byte
	}
	copy(fl.Name[:], name)
	if err := ioctl(s, 0x8913, unsafe.Pointer(&fl)); err != nil { // SIOCGIFFLAGS
		f.Close()
		t.Skipf("bayraklar okunamadı: %v", err)
	}
	fl.Flags |= 0x1 | 0x40                                        // IFF_UP | IFF_RUNNING
	if err := ioctl(s, 0x8914, unsafe.Pointer(&fl)); err != nil { // SIOCSIFFLAGS
		f.Close()
		t.Skipf("arayüz açılamadı: %v", err)
	}
	return f
}

// Uçtan uca: çekirdek TCP/IP yığını → TUN → gVisor → parçalama → "internet".
func TestEndToEndOverTUN(t *testing.T) {
	tun := setupTUN(t, "dpitest0", "10.200.0.1", "255.255.255.0")
	defer tun.Close()

	backend := newBackend(t)
	dpi := fakeDPI(t, backend.Listener.Addr().String())

	const fakeWeb = "10.200.0.77:443" // TUN alt ağında, gerçekte var olmayan bir "site"
	target := backend.Listener.Addr().String()
	oldMap := mapDst
	mapDst = func(s string) string {
		if s == fakeWeb {
			return target
		}
		return s
	}
	defer func() { mapDst = oldMap }()

	// DNS (UDP/53) için DoH'u yerel sahte sunucuya yönlendir.
	doh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		b[2] |= 0x80
		w.Write(b)
	}))
	defer doh.Close()
	oldS, oldT := dohServers, dohClient.Transport.(*http.Transport).TLSClientConfig
	dohServers = []string{doh.URL}
	dohClient.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	defer func() {
		dohServers = oldS
		dohClient.Transport.(*http.Transport).TLSClientConfig = oldT
	}()

	resetLearned()
	if err := Start(int(tun.Fd()), 1500, "auto"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer Stop()

	dial := func(addr string) net.Conn {
		c, err := net.DialTimeout("tcp", addr, 8*time.Second)
		if err != nil {
			t.Fatalf("TUN üzerinden bağlanılamadı: %v", err)
		}
		return c
	}

	t.Run("TLS", func(t *testing.T) {
		c := dial(fakeWeb)
		defer c.Close()
		if out := getOver(t, c, "discord.com"); !strings.Contains(out, "merhaba") {
			t.Fatalf("beklenmeyen yanıt: %q", out)
		}
	})

	t.Run("TLS-DPI-yedek-yontem", func(t *testing.T) {
		mapDst = func(s string) string {
			if s == fakeWeb {
				return dpi
			}
			return s
		}
		resetLearned()
		c := dial(fakeWeb)
		defer c.Close()
		if out := getOver(t, c, "roblox.com"); !strings.Contains(out, "merhaba") {
			t.Fatalf("beklenmeyen yanıt: %q", out)
		}
		if got := int(lastGood.Load()); got != sFirst {
			t.Fatalf("öğrenilen yöntem %s", strategyName(got))
		}
	})

	t.Run("DNS", func(t *testing.T) {
		c, err := net.Dial("udp", "10.200.0.88:53")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c', 0, 0, 1, 0, 1}
		c.SetDeadline(time.Now().Add(6 * time.Second))
		c.Write(q)
		buf := make([]byte, 512)
		n, err := c.Read(buf)
		if err != nil || n != len(q) || buf[0] != 0x12 || buf[2]&0x80 == 0 {
			t.Fatalf("DNS yanıtı hatalı: n=%d err=%v", n, err)
		}
	})

	if s := Stats(); !strings.Contains(s, `"running":true`) {
		t.Fatalf("istatistik hatalı: %s", s)
	}
	t.Logf("istatistik: %s", Stats())
}
