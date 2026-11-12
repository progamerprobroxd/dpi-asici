// Package core, Android VpnService'in verdiği TUN arayüzünü okuyup trafiği
// DPI'ya takılmayacak şekilde (TLS ClientHello parçalama + DoH DNS) internete
// çıkaran küçük bir kullanıcı alanı ağ yığınıdır.
//
// gomobile ile Android'e bağlanır: Java tarafında go.core.Core sınıfı olur.
package core

import (
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

var (
	mu  sync.Mutex
	eng *engine

	statActive    atomic.Int64
	statTotal     atomic.Int64
	statUp        atomic.Int64
	statDown      atomic.Int64
	statFallbacks atomic.Int64
	statDNS       atomic.Int64
	running       atomic.Bool
)

// Start, TUN dosya tanımlayıcısını (fd) alır ve çalışmaya başlar.
// mode: "auto" (önerilen) ya da "s0".."s5" (sabit yöntem).
func Start(fd int, mtu int, mode string) error {
	mu.Lock()
	defer mu.Unlock()
	if eng != nil {
		return errors.New("zaten çalışıyor")
	}
	if mtu < 576 || mtu > 9000 {
		mtu = 1500
	}

	// Isınmayı azaltmak için: en fazla 2 iş parçacığı ve küçük bir bellek hedefi.
	runtime.GOMAXPROCS(2)
	debug.SetMemoryLimit(96 << 20)

	SetMode(mode)
	resetStats()

	// Java tarafındaki ParcelFileDescriptor kendi kopyasını kapatır; biz kendi kopyamızı kullanırız.
	nfd, err := syscall.Dup(fd)
	if err != nil {
		return fmt.Errorf("fd kopyalanamadı: %w", err)
	}
	syscall.CloseOnExec(nfd)
	if err := syscall.SetNonblock(nfd, true); err != nil {
		syscall.Close(nfd)
		return fmt.Errorf("fd ayarlanamadı: %w", err)
	}

	e, err := newEngine(nfd, mtu)
	if err != nil {
		syscall.Close(nfd)
		return err
	}
	eng = e
	running.Store(true)
	return nil
}

// Stop, çalışan motoru durdurur.
func Stop() {
	mu.Lock()
	e := eng
	eng = nil
	mu.Unlock()
	running.Store(false)
	if e != nil {
		e.close()
	}
}

// NetworkChanged, Wi-Fi ↔ mobil veri geçişinde öğrenilmiş yöntemleri sıfırlar.
func NetworkChanged() {
	resetLearned()
}

// Stats, arayüz için küçük bir JSON döndürür.
func Stats() string {
	var b strings.Builder
	b.WriteString("{")
	b.WriteString(`"running":` + strconv.FormatBool(running.Load()))
	b.WriteString(`,"active":` + strconv.FormatInt(statActive.Load(), 10))
	b.WriteString(`,"total":` + strconv.FormatInt(statTotal.Load(), 10))
	b.WriteString(`,"up":` + strconv.FormatInt(statUp.Load(), 10))
	b.WriteString(`,"down":` + strconv.FormatInt(statDown.Load(), 10))
	b.WriteString(`,"fallbacks":` + strconv.FormatInt(statFallbacks.Load(), 10))
	b.WriteString(`,"dns":` + strconv.FormatInt(statDNS.Load(), 10))
	b.WriteString(`,"strategy":` + strconv.Quote(strategyName(int(lastGood.Load()))))
	b.WriteString(`,"mode":` + strconv.Quote(modeString()))
	b.WriteString("}")
	return b.String()
}

func resetStats() {
	statActive.Store(0)
	statTotal.Store(0)
	statUp.Store(0)
	statDown.Store(0)
	statFallbacks.Store(0)
	statDNS.Store(0)
	resetLearned()
}
