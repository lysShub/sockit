//go:build linux
// +build linux

package conn

import (
	"net"
	"net/netip"
	"sync"
	"unsafe"

	"github.com/lysShub/sockit/helper"
	"github.com/lysShub/sockit/route"
	"github.com/pkg/errors"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// ListenLocal occupy local port and avoid system stack reply RST
func ListenLocal(laddr netip.AddrPort, usedPort bool) (*net.TCPListener, netip.AddrPort, error) {
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: laddr.Addr().AsSlice(), Port: int(laddr.Port())})
	if err != nil {
		if usedPort {
			if errors.Is(err, unix.EADDRINUSE) {
				return nil, laddr, nil
			}
		}
		return nil, netip.AddrPort{}, err
	} else if usedPort {
		return nil, netip.AddrPort{}, errors.WithStack(ErrNotUsedPort(laddr.Port()))
	}

	raw, err := l.SyscallConn()
	if err != nil {
		l.Close()
		return nil, netip.AddrPort{}, err
	}
	var e error
	err = raw.Control(func(fd uintptr) {
		if rawIns, e1 := bpf.Assemble([]bpf.Instruction{
			bpf.RetConstant{Val: 0},
		}); e1 != nil {
			e = e1
			return
		} else {
			prog := &unix.SockFprog{
				Len:    uint16(len(rawIns)),
				Filter: (*unix.SockFilter)(unsafe.Pointer(&rawIns[0])),
			}

			if e = unix.SetsockoptSockFprog(
				int(fd), unix.SOL_SOCKET,
				unix.SO_ATTACH_FILTER, prog,
			); e != nil {
				return
			}
		}
	})
	if e != nil {
		l.Close()
		return nil, netip.AddrPort{}, e
	} else if err != nil {
		l.Close()
		return nil, netip.AddrPort{}, err
	}

	addr := netip.MustParseAddrPort(l.Addr().String())
	return l, netip.AddrPortFrom(laddr.Addr(), addr.Port()), nil
}

var tsoCache = struct {
	sync.RWMutex
	cache map[netip.Addr]bool
}{
	cache: map[netip.Addr]bool{},
}

func SetTSO(local, remote netip.Addr, tso bool) error {
	// get route table is expensive call, optimize for server case
	if !remote.IsPrivate() {
		tsoCache.RLock()
		old, has := tsoCache.cache[local]
		tsoCache.RUnlock()

		if has && old == tso {
			return nil
		} else {
			defer func() {
				tsoCache.Lock()
				tsoCache.cache[local] = tso
				tsoCache.Unlock()
			}()
		}
	}

	table, err := route.GetTable()
	if err != nil {
		return err
	}

	var ifIdx uint32
	if remote.IsPrivate() {
		for _, e := range table {
			if e.Addr.IsLoopback() {
				ifIdx = e.Interface
				break
			}
		}
	} else {
		for _, e := range table {
			if e.Addr == local {
				ifIdx = e.Interface
				break
			}
		}
	}
	if ifIdx == 0 {
		return errors.Errorf("invalid local address %s", local.String())
	}

	name, err := helper.IoctlGifname(int(ifIdx))
	if err != nil {
		return err
	}
	return helper.IoctlTSO(name, tso)
}