package session

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/perorin0418/naisen/internal/proto"
)

// ネットワークの内線を探す仕組み（UDP）。
//
// 内線は、TCP と同じアドレス・ポート（BasePort+番号）で UDP の問い合わせも待つ。
// 探す側は、ブロードキャストアドレスと 127.0.0.1 の、番号 1〜49 のポートに問い合わせを送り、返事を集める。
// 127.0.0.1 だけで待つ内線はこのマシンの中からしか見えず、0.0.0.0 などで待つ内線は LAN からも見える。

type discoverReq struct {
	Naisen  string `json:"naisen"` // "?"
	Version int    `json:"version"`
}

// ServeDiscovery は UDP の問い合わせに info() の情報で答える。pc が閉じられると戻る。
func ServeDiscovery(pc net.PacketConn, info func() proto.Hello) {
	buf := make([]byte, 2048)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		var req discoverReq
		if json.Unmarshal(buf[:n], &req) != nil || req.Naisen != "?" {
			continue
		}
		b, err := json.Marshal(info())
		if err != nil {
			continue
		}
		_, _ = pc.WriteTo(b, from)
	}
}

// broadcastTargets はこのマシンのネットワークのブロードキャストアドレスと 127.0.0.1 を返す。
func broadcastTargets() []net.IP {
	targets := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv4bcast}
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ip, mask := ipn.IP.To4(), net.IP(ipn.Mask).To4()
			if mask == nil {
				continue
			}
			b := make(net.IP, 4)
			for i := range 4 {
				b[i] = ip[i] | ^mask[i]
			}
			targets = append(targets, b)
		}
	}
	return targets
}

// Discover はネットワークの内線を UDP で探す。wait の間に届いた返事を返す（Addr に返事の送り元）。
func Discover(ctx context.Context, wait time.Duration) []*proto.Hello {
	return discover(ctx, wait, broadcastTargets(), proto.MinNumber, proto.MaxNumber)
}

func discover(ctx context.Context, wait time.Duration, targets []net.IP, lo, hi int) []*proto.Hello {
	pc, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil
	}
	defer pc.Close()
	req, _ := json.Marshal(discoverReq{Naisen: "?", Version: proto.Version})
	for _, ip := range targets {
		for n := lo; n <= hi; n++ {
			_, _ = pc.WriteTo(req, &net.UDPAddr{IP: ip, Port: proto.BasePort + n})
		}
	}
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = pc.SetReadDeadline(deadline)
	var mu sync.Mutex
	seen := map[string]bool{}
	var found []*proto.Hello
	buf := make([]byte, 4096)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			break
		}
		var h proto.Hello
		if json.Unmarshal(buf[:n], &h) != nil || h.Number == 0 {
			continue
		}
		ua, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		key := ua.IP.String() + "#" + strconv.Itoa(h.Number)
		mu.Lock()
		if !seen[key] {
			seen[key] = true
			h.Addr = ua.IP.String()
			found = append(found, &h)
		}
		mu.Unlock()
	}
	return found
}
