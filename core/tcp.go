package core

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var dialer = net.Dialer{Timeout: 8 * time.Second, KeepAlive: 45 * time.Second}

const idleTimeout = 5 * time.Minute

// mapDst yalnızca testlerde hedefi değiştirmek için kullanılır.
var mapDst = func(s string) string { return s }

var bufPool = sync.Pool{New: func() any { b := make([]byte, 16*1024); return &b }}

// handleTCP: TUN'dan gelen her TCP bağlantısı için bir kez çalışır.
// dst, uygulamanın gerçekten bağlanmak istediği "IP:port" adresidir.
func handleTCP(app net.Conn, dst string, port uint16) {
	statActive.Add(1)
	statTotal.Add(1)
	defer statActive.Add(-1)
	defer app.Close()

	switch port {
	case 443, 8443:
		tlsFlow(app, dst)
	case 80:
		httpFlow(app, dst)
	default:
		plainFlow(app, dst, nil)
	}
}

func plainFlow(app net.Conn, dst string, first []byte) {
	r, err := dialer.Dial("tcp", mapDst(dst))
	if err != nil {
		return
	}
	defer r.Close()
	if len(first) > 0 {
		if _, err := r.Write(first); err != nil {
			return
		}
		statUp.Add(int64(len(first)))
	}
	pipe(app, r)
}

// readFirstRecord: uygulamanın gönderdiği ilk TLS kaydını (başlık+gövde) okur.
// TLS değilse okunan baytları ve isTLS=false döndürür.
func readFirstRecord(c net.Conn) (data []byte, isTLS bool, err error) {
	hdr := make([]byte, 5)
	n, err := io.ReadFull(c, hdr)
	if err != nil {
		if n > 0 {
			return hdr[:n], false, nil
		}
		return nil, false, err
	}
	if hdr[0] != 0x16 || hdr[1] != 0x03 {
		return hdr, false, nil
	}
	l := int(binary.BigEndian.Uint16(hdr[3:5]))
	if l == 0 || l > 16384 {
		return hdr, false, nil
	}
	rec := make([]byte, 5+l)
	copy(rec, hdr)
	if _, err := io.ReadFull(c, rec[5:]); err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// tlsFlow: ClientHello'yu parçalı gönderir; sunucudan yanıt gelmezse
// (DPI bağlantıyı kesti/sildi) sıradaki yöntemle yeniden dener.
func tlsFlow(app net.Conn, dst string) {
	// Tarayıcıların boşta açtığı bağlantılar için bol zaman tanı.
	app.SetReadDeadline(time.Now().Add(90 * time.Second))
	first, isTLS, err := readFirstRecord(app)
	app.SetReadDeadline(time.Time{})
	if err != nil || len(first) == 0 {
		return
	}
	if !isTLS {
		plainFlow(app, dst, first)
		return
	}

	host, _, _ := net.SplitHostPort(dst)
	ord := order(host)
	for i, st := range ord {
		last := i == len(ord)-1
		r, err := dialer.Dial("tcp", mapDst(dst))
		if err != nil {
			return // bağlantı kurulamadı: yöntemle ilgisi yok
		}
		if err := sendHello(r, first, st); err != nil {
			r.Close()
			statFallbacks.Add(1)
			continue
		}
		wait := 3500 * time.Millisecond
		if last {
			wait = 12 * time.Second
		}
		r.SetReadDeadline(time.Now().Add(wait))
		bp := bufPool.Get().(*[]byte)
		n, rerr := r.Read(*bp)
		r.SetReadDeadline(time.Time{})
		if n > 0 {
			learn(host, st)
			statUp.Add(int64(len(first)))
			statDown.Add(int64(n))
			_, werr := app.Write((*bp)[:n])
			bufPool.Put(bp)
			if werr == nil && rerr == nil {
				pipe(app, r)
			}
			r.Close()
			return
		}
		bufPool.Put(bp)
		r.Close()
		if len(ord) > 1 {
			statFallbacks.Add(1)
		}
	}
}

// httpFlow: düz HTTP isteğinin Host başlığını iki parçaya bölerek gönderir.
func httpFlow(app net.Conn, dst string) {
	app.SetReadDeadline(time.Now().Add(90 * time.Second))
	buf := make([]byte, 8192)
	n, err := app.Read(buf)
	app.SetReadDeadline(time.Time{})
	if n == 0 || (err != nil && err != io.EOF) {
		return
	}
	first := buf[:n]
	r, derr := dialer.Dial("tcp", mapDst(dst))
	if derr != nil {
		return
	}
	defer r.Close()
	if tc, ok := r.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	cut := 0
	if fixedMode.Load() != sPlain {
		if i := bytes.Index(first, []byte("\r\nHost:")); i > 0 {
			cut = i + 7
		}
	}
	if cut > 0 && cut < len(first) {
		if _, err := r.Write(first[:cut]); err != nil {
			return
		}
		gap()
		if _, err := r.Write(first[cut:]); err != nil {
			return
		}
	} else if _, err := r.Write(first); err != nil {
		return
	}
	statUp.Add(int64(n))
	pipe(app, r)
}

// --- veri aktarımı ---

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// pipe: iki yönlü kopyalama; bir yön bitince diğerine kısa bir süre tanır.
func pipe(app, remote net.Conn) {
	done := make(chan struct{}, 2)
	go func() { copyIdle(remote, app, &statUp); closeWrite(remote); done <- struct{}{} }()
	go func() { copyIdle(app, remote, &statDown); closeWrite(app); done <- struct{}{} }()
	<-done
	dl := time.Now().Add(30 * time.Second)
	app.SetDeadline(dl)
	remote.SetDeadline(dl)
	<-done
}

// copyIdle: boşta kalan bağlantıları kapatarak kaynak (pil/CPU) harcamaz.
func copyIdle(dst, src net.Conn, ctr *atomic.Int64) {
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for {
		src.SetReadDeadline(time.Now().Add(idleTimeout))
		n, err := src.Read(buf)
		if n > 0 {
			ctr.Add(int64(n))
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
