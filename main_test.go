package main

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

func TestPeerTimersRetryConnectionQuickly(t *testing.T) {
	s := server.NewBgpServer()
	go s.Serve()
	defer s.Stop()
	if err := s.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{Asn: 65000, RouterId: "192.0.2.1", ListenPort: -1},
	}); err != nil {
		t.Fatal(err)
	}
	err := s.AddPeer(context.Background(), &api.AddPeerRequest{Peer: &api.Peer{
		Conf:   &api.PeerConf{NeighborAddress: "192.0.2.2", PeerAsn: 65000},
		Timers: newPeerTimers(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var got *api.Timers
	err = s.ListPeer(context.Background(), &api.ListPeerRequest{}, func(peer *api.Peer) { got = peer.Timers })
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.ConnectRetry != 10 {
		t.Errorf("ConnectRetry = %d, want 10", got.Config.ConnectRetry)
	}
	if got.Config.HoldTime != 90 {
		t.Errorf("HoldTime = %d, want 90 (GoBGP default)", got.Config.HoldTime)
	}
}

// lsNode is an IS-IS level-2 BGP-LS node path with a node name.
func lsNode(asn uint32, systemID, name string) *apiutil.Path {
	lndTLV := bgp.NewLsTLVNodeDescriptor(&bgp.LsNodeDescriptor{Asn: asn, IGPRouterID: systemID}, bgp.LS_TLV_LOCAL_NODE_DESC)
	nlri := &bgp.LsAddrPrefix{
		Type:   bgp.LS_NLRI_TYPE_NODE,
		Length: 4,
		NLRI: &bgp.LsNodeNLRI{
			LsNLRI: bgp.LsNLRI{
				NLRIType:   bgp.LS_NLRI_TYPE_NODE,
				Length:     uint16(lndTLV.Len() + 8 + 1),
				ProtocolID: bgp.LS_PROTOCOL_ISIS_L2,
			},
			LocalNodeDesc: &lndTLV,
		},
	}
	nh, _ := bgp.NewPathAttributeNextHop(netip.MustParseAddr("0.0.0.0"))
	ls := &bgp.PathAttributeLs{
		PathAttribute: bgp.PathAttribute{Type: bgp.BGP_ATTR_TYPE_LS, Flags: bgp.BGP_ATTR_FLAG_OPTIONAL},
		TLVs:          []bgp.LsTLVInterface{bgp.NewLsTLVNodeName(&name)},
	}
	return &apiutil.Path{Family: bgp.RF_LS, Nlri: nlri, Attrs: []bgp.PathAttributeInterface{bgp.NewPathAttributeOrigin(0), nh, ls}}
}

func TestKnownPathsWithdrawnWhenSessionDrops(t *testing.T) {
	r1 := netip.MustParseAddr("192.0.2.11")
	a := lsNode(65010, "\x00\x00\x00\x00\x00\x01", "a")
	b := lsNode(65010, "\x00\x00\x00\x00\x00\x02", "b")
	a.PeerAddress, b.PeerAddress = r1, r1
	k := newKnownPaths()
	k.update([]*apiutil.Path{a, b})
	aw := *a
	aw.Withdrawal = true
	k.update([]*apiutil.Path{&aw})

	got := k.withdrawAll(r1.String())
	if len(got) != 1 || !got[0].Withdrawal || got[0].Nlri.String() != b.Nlri.String() {
		t.Fatalf("withdrawAll = %v, want one withdrawal of %s", got, b.Nlri)
	}
	if again := k.withdrawAll(r1.String()); len(again) != 0 {
		t.Errorf("second withdrawAll = %v, want none", again)
	}
}

// TestRouterLinkStateNotReadvertised runs bgplswatcher's speaker with two
// routers in different ASes (both eBGP), r1 on 127.0.0.1 and r2 on ::1. r1
// announces a node, changes it and withdraws it; r2 must receive no UPDATE at
// all. Without blockReadvertisement r2 receives all three.
func TestRouterLinkStateNotReadvertised(t *testing.T) {
	ctx := context.Background()
	port := func(network, host string) uint32 {
		l, err := net.Listen(network, net.JoinHostPort(host, "0"))
		if err != nil {
			t.Skipf("cannot listen on %s: %v", host, err)
		}
		defer l.Close()
		_, p, _ := net.SplitHostPort(l.Addr().String())
		n, _ := strconv.Atoi(p)
		return uint32(n)
	}
	lsFamily := []*api.AfiSafi{{Config: &api.AfiSafiConfig{
		Family: &api.Family{Afi: api.Family_AFI_LS, Safi: api.Family_SAFI_LS}, Enabled: true,
	}}}
	start := func(asn uint32, routerID string, listen string, port int32) *server.BgpServer {
		s := server.NewBgpServer()
		go s.Serve()
		t.Cleanup(s.Stop)
		g := &api.Global{Asn: asn, RouterId: routerID, ListenPort: port}
		if listen != "" {
			g.ListenAddresses = []string{listen}
		}
		if err := s.StartBgp(ctx, &api.StartBgpRequest{Global: g}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	router := func(asn uint32, routerID, addr string, port uint32) *server.BgpServer {
		s := start(asn, routerID, addr, int32(port))
		if err := s.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
			Conf: &api.PeerConf{NeighborAddress: addr, PeerAsn: 65000}, AfiSafis: lsFamily,
			Transport: &api.Transport{PassiveMode: true},
		}}); err != nil {
			t.Fatal(err)
		}
		return s
	}

	p1, p2 := port("tcp4", "127.0.0.1"), port("tcp6", "::1")
	r1 := router(65010, "192.0.2.11", "127.0.0.1", p1)
	r2 := router(65020, "192.0.2.12", "::1", p2)
	speaker := start(65000, "192.0.2.1", "", -1)
	if err := blockReadvertisement(ctx, speaker); err != nil {
		t.Fatal(err)
	}
	for _, n := range []struct {
		addr string
		asn  uint32
		port uint32
	}{{"127.0.0.1", 65010, p1}, {"::1", 65020, p2}} {
		if err := speaker.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
			Conf:         &api.PeerConf{NeighborAddress: n.addr, PeerAsn: n.asn},
			Timers:       &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1}},
			AfiSafis:     lsFamily,
			Transport:    &api.Transport{RemotePort: n.port},
			EbgpMultihop: &api.EbgpMultihop{Enabled: true, MultihopTtl: 255},
		}}); err != nil {
			t.Fatal(err)
		}
	}

	updatesAtR2 := func() (established bool, updates uint64) {
		_ = r2.ListPeer(ctx, &api.ListPeerRequest{}, func(p *api.Peer) {
			established = p.State.SessionState == api.PeerState_SESSION_STATE_ESTABLISHED
			updates = p.State.GetMessages().GetReceived().GetUpdate()
		})
		return
	}
	speakerHas := func(n int) bool {
		got := 0
		_ = speaker.ListPath(apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_ADJ_IN, Name: "127.0.0.1", Family: bgp.RF_LS, EnableFiltered: true},
			func(_ bgp.NLRI, paths []*apiutil.Path) { got += len(paths) })
		return got == n
	}
	eventually := func(what string, cond func() bool) {
		for deadline := time.Now().Add(30 * time.Second); !cond(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	eventually("r2's session", func() bool { up, _ := updatesAtR2(); return up })
	_, before := updatesAtR2()

	node := lsNode(65010, "\x00\x00\x00\x00\x00\x01", "r1")
	if _, err := r1.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{node}}); err != nil {
		t.Fatal(err)
	}
	eventually("r1's node at the speaker", func() bool { return speakerHas(1) })
	if _, err := r1.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{lsNode(65010, "\x00\x00\x00\x00\x00\x01", "r1-renamed")}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	ls := bgp.RF_LS
	if err := r1.DeletePath(apiutil.DeletePathRequest{DeleteAll: true, DeleteFamily: &ls}); err != nil {
		t.Fatal(err)
	}
	eventually("r1's withdrawal at the speaker", func() bool { return speakerHas(0) })
	time.Sleep(2 * time.Second)

	if _, after := updatesAtR2(); after != before {
		t.Errorf("r2 received %d UPDATE message(s) carrying r1's link-state, want 0", after-before)
	}
}
