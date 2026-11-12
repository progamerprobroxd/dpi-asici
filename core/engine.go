package core

import (
	"fmt"
	"net"
	"strconv"
	"syscall"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const nicID = tcpip.NICID(1)

type engine struct {
	s  *stack.Stack
	fd int
}

func newEngine(fd, mtu int) (*engine, error) {
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			ipv6.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
			icmp.NewProtocol4,
			icmp.NewProtocol6,
		},
	})

	ep, err := fdbased.New(&fdbased.Options{
		FDs:            []int{fd},
		MTU:            uint32(mtu),
		EthernetHeader: false, // Android TUN: ham IP paketleri
	})
	if err != nil {
		return nil, fmt.Errorf("fdbased: %w", err)
	}
	// NIC devre dışı oluşturulur: Android VPN'i açar açmaz kuyrukta paket olabilir;
	// işleyiciler hazır olmadan okumaya başlanmamalı.
	if terr := s.CreateNICWithOptions(nicID, ep, stack.NICOptions{Disabled: true}); terr != nil {
		return nil, fmt.Errorf("NIC oluşturulamadı: %v", terr)
	}
	// Başka adrese giden her paketi kabul et (şeffaf proxy).
	if terr := s.SetPromiscuousMode(nicID, true); terr != nil {
		return nil, fmt.Errorf("promiscuous: %v", terr)
	}
	if terr := s.SetSpoofing(nicID, true); terr != nil {
		return nil, fmt.Errorf("spoofing: %v", terr)
	}
	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})

	sack := tcpip.TCPSACKEnabled(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)

	tcpFwd := tcp.NewForwarder(s, 0, 1024, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		var wq waiter.Queue
		tep, terr := r.CreateEndpoint(&wq)
		if terr != nil {
			r.Complete(true)
			return
		}
		r.Complete(false)
		conn := gonet.NewTCPConn(&wq, tep)
		dst := net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort)))
		go handleTCP(conn, dst, id.LocalPort)
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)

	udpFwd := udp.NewForwarder(s, func(r *udp.ForwarderRequest) bool {
		id := r.ID()
		// QUIC (UDP 443) bilerek düşürülür: uygulamalar TCP'ye döner ve
		// parçalama yöntemi onlara da uygulanır.
		if id.LocalPort == 443 {
			return true
		}
		var wq waiter.Queue
		uep, terr := r.CreateEndpoint(&wq)
		if terr != nil {
			return false
		}
		conn := gonet.NewUDPConn(&wq, uep)
		dst := net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort)))
		go handleUDP(conn, dst, id.LocalPort)
		return true
	})
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	// Her şey hazır: paket işlemeye başla.
	if terr := s.EnableNIC(nicID); terr != nil {
		return nil, fmt.Errorf("NIC etkinleştirilemedi: %v", terr)
	}
	return &engine{s: s, fd: fd}, nil
}

func (e *engine) close() {
	e.s.Close()
	e.s.Wait()
	syscall.Close(e.fd)
}
