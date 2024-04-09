//go:build linux
// +build linux

package eth

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lysShub/sockit/conn"
	iconn "github.com/lysShub/sockit/conn/internal"
	itcp "github.com/lysShub/sockit/conn/tcp/internal"
	"github.com/lysShub/sockit/errorx"
	"github.com/lysShub/sockit/helper"
	"github.com/lysShub/sockit/helper/bpf"
	"github.com/lysShub/sockit/helper/ipstack"
	"github.com/lysShub/sockit/packet"
	"github.com/lysShub/sockit/route"
	"github.com/lysShub/sockit/socket/eth"
	"github.com/lysShub/sockit/test"
	"github.com/lysShub/sockit/test/debug"
	"github.com/mdlayher/arp"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

type Listener struct {
	addr netip.AddrPort
	cfg  *conn.Config

	tcp *net.TCPListener

	raw *net.IPConn

	conns   map[itcp.ID]struct{}
	connsMu sync.RWMutex

	closeErr atomic.Pointer[error]
}

var _ conn.Listener = (*Listener)(nil)

func Listen(laddr netip.AddrPort, opts ...conn.Option) (*Listener, error) {
	var l = &Listener{
		cfg:   conn.Options(opts...),
		conns: make(map[itcp.ID]struct{}, 16),
	}

	var err error
	l.tcp, l.addr, err = iconn.ListenLocal(laddr, l.cfg.UsedPort)
	if err != nil {
		return nil, l.close(err)
	}
	if err = iconn.SetTSOByAddr(l.addr.Addr(), l.cfg.TSO); err != nil {
		return nil, l.close(err)
	}

	l.raw, err = net.ListenIP(
		"ip:tcp",
		&net.IPAddr{IP: l.addr.Addr().AsSlice(), Zone: laddr.Addr().Zone()},
	)
	if err != nil {
		return nil, l.close(err)
	}

	raw, err := l.raw.SyscallConn()
	if err != nil {
		return nil, l.close(err)
	}

	if err = bpf.SetRawBPF(
		raw,
		bpf.FilterDstPortAndSynFlag(l.addr.Port()),
	); err != nil {
		return nil, l.close(err)
	}

	return l, nil
}

func (l *Listener) close(cause error) error {
	if l.closeErr.CompareAndSwap(nil, &net.ErrClosed) {
		if l.tcp != nil {
			if err := l.tcp.Close(); err != nil {
				cause = err
			}
		}
		if l.raw != nil {
			if err := l.raw.Close(); err != nil {
				cause = err
			}
		}

		if cause != nil {
			l.closeErr.Store(&cause)
		}
		return cause
	}
	return *l.closeErr.Load()
}

func (l *Listener) Addr() netip.AddrPort {
	return l.addr
}

// todo: not support private proto that not start with tcp SYN flag
func (l *Listener) Accept() (conn.RawConn, error) {
	var min, max = itcp.SizeRange(l.addr.Addr().Is4())

	var ip = make([]byte, max)
	for {
		n, err := l.raw.Read(ip[:max])
		if err != nil {
			return nil, l.close(err)
		} else if n < min {
			return nil, fmt.Errorf("recved invalid ip packet, bytes %d", n)
		}

		var id = itcp.ID{Local: l.addr}
		switch header.IPVersion(ip) {
		case 4:
			iphdr := header.IPv4(ip[:n])
			tcphdr := header.TCP(iphdr.Payload())
			id.Remote = netip.AddrPortFrom(netip.AddrFrom4(iphdr.SourceAddress().As4()), tcphdr.SourcePort())
			id.ISN = tcphdr.SequenceNumber()
		case 6:
			iphdr := header.IPv6(ip[:n])
			tcphdr := header.TCP(iphdr.Payload())
			id.Remote = netip.AddrPortFrom(netip.AddrFrom4(iphdr.SourceAddress().As4()), tcphdr.SourcePort())
			id.ISN = tcphdr.SequenceNumber()
		default:
			continue
		}

		l.connsMu.RLock()
		_, has := l.conns[id]
		l.connsMu.RLock()

		if !has {
			l.connsMu.Lock()
			l.conns[id] = struct{}{}
			l.connsMu.Unlock()

			c := newConnect(
				id, l.deleteConn, l.cfg.CtxPeriod,
			)
			if err := c.init(l.cfg); err != nil {
				return nil, errorx.WrapTemp(c.close(err))
			}
			return c, nil
		}
	}
}

func (l *Listener) deleteConn(id itcp.ID) error {
	if l == nil {
		return nil
	}
	time.AfterFunc(time.Minute, func() {
		l.connsMu.Lock()
		defer l.connsMu.Unlock()

		delete(l.conns, id)
	})
	return nil
}

func (l *Listener) Close() error {
	return l.close(nil)
}

type Conn struct {
	itcp.ID

	// todo: set buff 0
	tcp *net.TCPListener

	raw     *eth.Conn
	ipstack *ipstack.IPStack
	gateway net.HardwareAddr

	ctxPeriod time.Duration
	closeFn   itcp.CloseCallback

	closeErr atomic.Pointer[error]
}

var _ conn.RawConn = (*Conn)(nil)

func Connect(laddr, raddr netip.AddrPort, opts ...conn.Option) (*Conn, error) {
	cfg := conn.Options(opts...)
	var c = newConnect(
		itcp.ID{Local: laddr, Remote: raddr, ISN: 0},
		nil, cfg.CtxPeriod,
	)

	var err error
	c.tcp, c.Local, err = iconn.ListenLocal(laddr, cfg.UsedPort)
	if err != nil {
		return nil, c.close(err)
	}
	if err := iconn.SetTSOByAddr(c.Local.Addr(), cfg.TSO); err != nil {
		return nil, c.close(err)
	}

	if err := c.init(cfg); err != nil {
		return nil, c.close(err)
	}
	return c, nil
}

func newConnect(id itcp.ID, closeCall itcp.CloseCallback, ctxPeriod time.Duration) *Conn {
	return &Conn{
		ID:        id,
		closeFn:   closeCall,
		ctxPeriod: ctxPeriod,
	}
}

func (c *Conn) init(cfg *conn.Config) (err error) {
	entry, err := route.GetBestInterface(c.Remote.Addr())
	if err != nil {
		return err
	}

	// set gateway mac address
	var ifi *net.Interface
	if !entry.Next.IsValid() {
		// is on loopback

		return errors.New("not support loopback connect")
		lo, err := helper.LoopbackInterface()
		if err != nil {
			return err
		}
		ifi, err = net.InterfaceByName(lo)
		if err != nil {
			return errors.WithStack(err)
		}
		c.gateway = net.HardwareAddr(make([]byte, 6))
	} else {
		if debug.Debug() {
			require.Equal(test.T(), c.Local.Addr(), entry.Addr)
		}
		ifi, err = net.InterfaceByIndex(int(entry.Interface))
		if err != nil {
			return errors.WithStack(err)
		}

		// get gatway hardware address
		if client, err := arp.Dial(c.raw.Interface()); err != nil {
			return errors.WithStack(err)
		} else {
			defer client.Close()
			if err = client.SetDeadline(time.Now().Add(time.Second * 3)); err != nil {
				return errors.WithStack(err)
			}

			c.gateway, err = client.Resolve(entry.Next)
			if err != nil {
				return errors.WithStack(err)
			}
		}
	}

	// create eth conn and set bpf filter
	c.raw, err = eth.Listen("eth:ip4", ifi)
	if err != nil {
		return err
	}
	if err := bpf.SetRawBPF(
		c.raw.SyscallConn(),
		bpf.FilterEndpoint(header.TCPProtocolNumber, c.Remote, c.Local),
	); err != nil {
		return err
	}

	if c.ipstack, err = ipstack.New(
		c.Local.Addr(), c.Remote.Addr(),
		header.TCPProtocolNumber, cfg.IPStack.Unmarshal(),
	); err != nil {
		return err
	}
	return nil
}

func (c *Conn) close(cause error) error {
	if c.closeErr.CompareAndSwap(nil, &net.ErrClosed) {
		if c.tcp != nil {
			if err := c.tcp.Close(); err != nil {
				cause = err
			}
		}
		if c.raw != nil {
			if err := c.raw.Close(); err != nil {
				cause = err
			}
		}
		if c.closeFn != nil {
			if err := c.closeFn(c.ID); err != nil {
				cause = err
			}
		}

		if cause != nil {
			c.closeErr.Store(&cause)
		}
		return cause
	}
	return *c.closeErr.Load()
}

func (c *Conn) Read(ctx context.Context, pkt *packet.Packet) (err error) {
	b := pkt.Bytes()

	var n int
	for {
		err = c.raw.SetReadDeadline(time.Now().Add(c.ctxPeriod))
		if err != nil {
			return err
		}

		n, _, err = c.raw.Recvfrom(b[:cap(b)], 0)
		if err == nil {
			break
		} else if errors.Is(err, os.ErrDeadlineExceeded) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				continue
			}
		} else {
			return err
		}
	}
	pkt.SetData(n)

	if debug.Debug() {
		test.ValidIP(test.T(), pkt.Bytes())
	}

	hdr, err := iconn.ValidComplete(pkt.Bytes())
	if err != nil {
		return err
	}
	pkt.SetHead(pkt.Head() + int(hdr))
	return nil
}

func (c *Conn) Write(ctx context.Context, pkt *packet.Packet) (err error) {
	c.ipstack.AttachOutbound(pkt)
	if debug.Debug() {
		test.ValidIP(test.T(), pkt.Bytes())
	}

	err = c.raw.Sendto(pkt.Bytes(), 0, c.gateway)
	return err
}

func (c *Conn) Inject(ctx context.Context, p *packet.Packet) (err error) {
	return errors.New("todo: not support, need test")

	// c.ipstack.AttachInbound(p)
	// if debug.Debug() {
	// 	test.ValidIP(test.T(), p.Data())
	// }
	// // p.Attach(c.outEthdr[:])
	// _, err = c.raw.Write(p.Data())
	// return err
}

func (c *Conn) Close() (err error) {
	return c.close(nil)
}

func (c *Conn) LocalAddr() netip.AddrPort  { return c.Local }
func (c *Conn) RemoteAddr() netip.AddrPort { return c.Remote }
func (c *Conn) Raw() *eth.Conn             { return c.raw }
