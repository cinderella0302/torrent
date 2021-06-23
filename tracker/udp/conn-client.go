package udp

import (
	"context"
	"log"
	"net"

	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/missinggo/v2"
)

type NewConnClientOpts struct {
	Network string
	Host    string
	Ipv6    *bool
}

type ConnClient struct {
	cl      Client
	conn    net.Conn
	d       Dispatcher
	readErr error
	ipv6    bool
}

func (cc *ConnClient) reader() {
	for {
		b := make([]byte, 0x800)
		n, err := cc.conn.Read(b)
		if err != nil {
			// TODO: Do bad things to the dispatcher, and incoming calls to the client if we have a
			// read error.
			cc.readErr = err
			break
		}
		err = cc.d.Dispatch(b[:n])
		if err != nil {
			log.Printf("dispatching packet received on %v: %v", cc.conn, err)
		}
	}
}

func ipv6(opt *bool, network string, conn net.Conn) bool {
	if opt != nil {
		return *opt
	}
	switch network {
	case "udp4":
		return false
	case "udp6":
		return true
	}
	rip := missinggo.AddrIP(conn.RemoteAddr())
	return rip.To16() != nil && rip.To4() == nil
}

func NewConnClient(opts NewConnClientOpts) (cc ConnClient, err error) {
	cc.conn, err = net.Dial(opts.Network, opts.Host)
	if err != nil {
		return
	}
	cc.ipv6 = ipv6(opts.Ipv6, opts.Network, cc.conn)
	go cc.reader()
	cc.cl = Client{
		Dispatcher: &cc.d,
		Writer:     cc.conn,
	}
	return
}

func (c *ConnClient) Close() error {
	return c.conn.Close()
}

func (c *ConnClient) Announce(
	ctx context.Context, req AnnounceRequest, opts Options,
) (
	h AnnounceResponseHeader, nas AnnounceResponsePeers, err error,
) {
	nas = func() AnnounceResponsePeers {
		if c.ipv6 {
			return &krpc.CompactIPv6NodeAddrs{}
		} else {
			return &krpc.CompactIPv4NodeAddrs{}
		}
	}()
	h, err = c.cl.Announce(ctx, req, nas, opts)
	return
}
