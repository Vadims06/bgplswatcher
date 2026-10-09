package main

import (
	"context"
	"testing"

	api "github.com/osrg/gobgp/v4/api"
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
