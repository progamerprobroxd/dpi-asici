package core

import (
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Parçalama yöntemleri. "auto" modda sırayla denenir; çalışan yöntem hatırlanır.
const (
	sSNI    = iota // ClientHello'yu SNI'nin ortasından 2 TCP parçasına böl
	sTLSTCP        // 2 TLS kaydı + ilk kaydı ayrıca TCP'de böl
	sFirst         // ilk bayt tek başına, kalanı sonra
	sMulti         // 2 bayt | SNI'ye kadar | kalan
	sTLSRec        // yalnızca 2 TLS kaydı
	sPlain         // parçalama yok
	numStrategies
)

var strategyNames = [numStrategies]string{
	"SNI bölme", "TLS+TCP", "İlk bayt", "3 parça", "TLS kayıt", "Düz",
}

func strategyName(i int) string {
	if i < 0 || i >= numStrategies {
		return "?"
	}
	return strategyNames[i]
}

// --- mod ve öğrenme durumu ---

var (
	fixedMode atomic.Int32 // -1: otomatik, 0..5: sabit yöntem
	lastGood  atomic.Int32 // en son başarılı olan yöntem
	learned   sync.Map     // hedef IP -> yöntem
)

func init() { fixedMode.Store(-1) }

// SetMode: "auto" ya da "s0".."s5".
func SetMode(m string) {
	if len(m) == 2 && m[0] == 's' && m[1] >= '0' && m[1] < byte('0'+numStrategies) {
		fixedMode.Store(int32(m[1] - '0'))
		return
	}
	fixedMode.Store(-1)
}

func modeString() string {
	if v := fixedMode.Load(); v >= 0 {
		return "s" + string(rune('0'+v))
	}
	return "auto"
}

func resetLearned() {
	learned.Range(func(k, _ any) bool { learned.Delete(k); return true })
	lastGood.Store(0)
}

// order: bu hedef için denenecek yöntem sırası.
func order(host string) []int {
	if v := fixedMode.Load(); v >= 0 {
		return []int{int(v)}
	}
	first := int(lastGood.Load())
	if v, ok := learned.Load(host); ok {
		first = v.(int)
	}
	out := make([]int, 0, numStrategies)
	out = append(out, first)
	for i := 0; i < numStrategies; i++ {
		if i != first {
			out = append(out, i)
		}
	}
	return out
}

func learn(host string, st int) {
	learned.Store(host, st)
	lastGood.Store(int32(st))
}

// --- ClientHello ayrıştırma ve gönderme ---

// findSNI: ClientHello gövdesinde (kayıt başlığı hariç) sunucu adının
// başlangıcını ve uzunluğunu döndürür.
func findSNI(b []byte) (start, length int, ok bool) {
	if len(b) < 43 || b[0] != 0x01 {
		return
	}
	p := 38
	p += 1 + int(b[p]) // session id
	if p+2 > len(b) {
		return
	}
	p += 2 + int(binary.BigEndian.Uint16(b[p:])) // cipher suites
	if p+1 > len(b) {
		return
	}
	p += 1 + int(b[p]) // compression methods
	if p+2 > len(b) {
		return
	}
	end := p + 2 + int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	if end > len(b) {
		end = len(b)
	}
	for p+4 <= end {
		t := binary.BigEndian.Uint16(b[p:])
		l := int(binary.BigEndian.Uint16(b[p+2:]))
		p += 4
		if t == 0 { // server_name
			if l < 5 || p+5 > len(b) {
				return
			}
			nl := int(binary.BigEndian.Uint16(b[p+3:]))
			s := p + 5
			if s+nl > len(b) {
				return
			}
			return s, nl, true
		}
		p += l
	}
	return
}

func tlsRecord(hdr, body []byte) []byte {
	out := make([]byte, 5+len(body))
	out[0], out[1], out[2] = 0x16, hdr[1], hdr[2]
	binary.BigEndian.PutUint16(out[3:5], uint16(len(body)))
	copy(out[5:], body)
	return out
}

func gap() { time.Sleep(time.Millisecond) }

// sendHello: istemcinin ilk TLS kaydını (rec = başlık+gövde) seçilen yöntemle gönderir.
func sendHello(r net.Conn, rec []byte, st int) error {
	if tc, ok := r.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	write := func(parts ...[]byte) error {
		for i, p := range parts {
			if len(p) == 0 {
				continue
			}
			if i > 0 {
				gap()
			}
			if _, err := r.Write(p); err != nil {
				return err
			}
		}
		return nil
	}
	if len(rec) < 7 || st == sPlain {
		return write(rec)
	}
	hdr, body := rec[:5], rec[5:]
	cut := len(body) / 2
	if s, l, ok := findSNI(body); ok {
		cut = s + l/2
	}
	if cut < 1 {
		cut = 1
	}
	if cut >= len(body) {
		cut = len(body) - 1
	}
	c := 5 + cut // kayıt başlığı dahil kesme noktası

	switch st {
	case sSNI:
		return write(rec[:c], rec[c:])
	case sTLSTCP:
		a, b := tlsRecord(hdr, body[:cut]), tlsRecord(hdr, body[cut:])
		return write(a[:2], a[2:], b)
	case sFirst:
		return write(rec[:1], rec[1:])
	case sMulti:
		return write(rec[:2], rec[2:c], rec[c:])
	case sTLSRec:
		return write(tlsRecord(hdr, body[:cut]), tlsRecord(hdr, body[cut:]))
	}
	return write(rec)
}
