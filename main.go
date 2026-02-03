package main

import (
	"context"
	"fmt"
	stdlog "log"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	toml "github.com/BurntSushi/toml"
	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Config struct {
	Global    GlobalConfig    `toml:"global"`
	Neighbors []NeighborEntry `toml:"neighbors"`
}

type GlobalConfig struct {
	Config GlobalConfigData `toml:"config"`
}

type GlobalConfigData struct {
	AS                         uint32 `toml:"as"`
	RouterID                   string `toml:"router-id"`
	TopolographWatcherEndpoint string `toml:"topolograph-watcher-endpoint"`
}

type NeighborEntry struct {
	Config       NeighborConfig     `toml:"config"`
	Transport    TransportConfig    `toml:"transport"`
	EbgpMultihop EbgpMultihopConfig `toml:"ebgp-multihop"`
}

type NeighborConfig struct {
	NeighborAddress            string `toml:"neighbor-address"`
	PeerAS                     uint32 `toml:"peer-as"`
	TopolographWatcherEndpoint string `toml:"topolograph-watcher-endpoint"`
	PassiveMode                bool   `toml:"passive-mode"`
	LocalAddress               string `toml:"local-address"`
}

type TransportConfig struct {
	Config TransportConfigData `toml:"config"`
}

type TransportConfigData struct {
	LocalAddress string `toml:"local-address"`
}

type EbgpMultihopConfig struct {
	Config EbgpMultihopConfigData `toml:"config"`
}

type EbgpMultihopConfigData struct {
	Enabled     bool   `toml:"enabled"`
	MultihopTtl uint32 `toml:"multihop-ttl"`
}

func main() {
	// Load config from toml file
	config := &Config{}

	// Try to load config.toml
	if _, err := os.Stat("config.toml"); err != nil {
		stdlog.Fatalf("config.toml not found: %v", err)
	}

	data, err := os.ReadFile("config.toml")
	if err != nil {
		stdlog.Fatalf("Error reading config file: %v", err)
	}
	if err := toml.Unmarshal(data, config); err != nil {
		stdlog.Fatalf("Error parsing config file: %v", err)
	}

	// Validate global config
	if config.Global.Config.AS == 0 {
		stdlog.Fatal("global.config.as is required")
	}
	if config.Global.Config.RouterID == "" {
		stdlog.Fatal("global.config.router-id is required")
	}
	if config.Global.Config.TopolographWatcherEndpoint == "" {
		stdlog.Fatal("global.config.topolograph-watcher-endpoint is required")
	}

	// Setup logging - use InfoLevel to avoid verbose byte dumps
	lvl := &slog.LevelVar{}
	lvl.Set(slog.LevelInfo)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))

	// Increase max message size to handle large BGP-LS messages (up to 1GB)
	// Note: Messages can grow large when GoBGP includes accumulated RIB state or processes
	// complex topologies. This is a safety limit to handle edge cases.
	maxMsgSize := 1 << 30 // 1GB to handle accumulated state from GoBGP processing
	grpcServerOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(maxMsgSize),
		grpc.MaxSendMsgSize(maxMsgSize),
	}

	// Create GoBGP server - always enable gRPC listener on port 50051 for gobgp CLI access
	var serverOpts []server.ServerOption
	serverOpts = append(serverOpts, server.LoggerOption(logger, lvl))
	serverOpts = append(serverOpts, server.GrpcOption(grpcServerOpts))
	serverOpts = append(serverOpts, server.GrpcListenAddress("0.0.0.0:50051"))
	logger.Info("GoBGP gRPC server listening on 0.0.0.0:50051 (gobgp CLI access enabled)")

	s := server.NewBgpServer(serverOpts...)
	go s.Serve()

	// Check if any neighbor is in passive mode - if so, we need to listen on BGP port
	needsListener := false
	for _, neighborEntry := range config.Neighbors {
		if neighborEntry.Config.PassiveMode {
			needsListener = true
			break
		}
	}

	listenPort := int32(-1)
	if needsListener {
		listenPort = 179 // Standard BGP port
	}

	// Start BGP server
	if err := s.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        config.Global.Config.AS,
			RouterId:   config.Global.Config.RouterID,
			ListenPort: listenPort,
		},
	}); err != nil {
		stdlog.Fatal(err)
	}

	// Get topolograph watcher endpoint from global config
	globalEndpoint := config.Global.Config.TopolographWatcherEndpoint

	// Safety check: ensure we're not connecting to our own server
	if globalEndpoint == "localhost:50051" || globalEndpoint == "127.0.0.1:50051" || globalEndpoint == ":50051" {
		stdlog.Fatalf("ERROR: Topolograph watcher endpoint (%s) cannot be the same as GoBGP server port (50051). This would create an infinite loop! Use port 50052 instead.", globalEndpoint)
	}

	// Build neighbor-to-endpoint mapping and connection pool
	neighborToEndpoint := make(map[string]string)               // neighbor IP -> endpoint
	endpointToClient := make(map[string]api.GoBgpServiceClient) // endpoint -> client
	var clientsMutex sync.RWMutex

	// Helper function to get or create client for an endpoint
	getClientForEndpoint := func(endpoint string) (api.GoBgpServiceClient, error) {
		clientsMutex.RLock()
		client, exists := endpointToClient[endpoint]
		clientsMutex.RUnlock()

		if exists {
			return client, nil
		}

		// Safety check for new endpoint
		if endpoint == "localhost:50051" || endpoint == "127.0.0.1:50051" || endpoint == ":50051" {
			return nil, fmt.Errorf("ERROR: Topolograph watcher endpoint (%s) cannot be the same as GoBGP server port (50051). This would create an infinite loop! Use port 50052 instead", endpoint)
		}

		logger.Info("Connecting to Topolograph watcher", slog.String("endpoint", endpoint))
		conn, err := grpc.NewClient(endpoint,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(maxMsgSize),
				grpc.MaxCallSendMsgSize(maxMsgSize),
			))
		if err != nil {
			return nil, fmt.Errorf("failed to create gRPC connection to Topolograph watcher: %w", err)
		}

		client = api.NewGoBgpServiceClient(conn)
		clientsMutex.Lock()
		endpointToClient[endpoint] = client
		clientsMutex.Unlock()
		logger.Info("Connected to Topolograph watcher", slog.String("endpoint", endpoint))

		return client, nil
	}

	// Create client for global endpoint
	globalClient, err := getClientForEndpoint(globalEndpoint)
	if err != nil {
		stdlog.Fatal(err)
	}

	// Note: Peer events are automatically available via WatchEvent stream.
	// Python watcher should subscribe to WatchEvent from bgplswatcher (port 50051) to receive peer events.
	// No special forwarding needed - events flow through the existing WatchEvent server.

	// Watch for BGP events and forward to Topolograph watcher
	if err := s.WatchEvent(context.Background(), server.WatchEventMessageCallbacks{
		OnBestPath: func(paths []*apiutil.Path, timestamp time.Time) {
			if len(paths) > 100 {
				logger.Warn("Received unusually large batch of paths", slog.Int("count", len(paths)))
			}

			// Forward all BGP-LS paths to Topolograph watcher
			forwarded := 0
			for _, path := range paths {
				if path.Family.Afi() == uint16(api.Family_AFI_LS) && path.Family.Safi() == uint8(api.Family_SAFI_LS) {
					// Determine which endpoint to use based on neighbor IP
					var targetClient api.GoBgpServiceClient = globalClient
					endpoint := globalEndpoint // Default to global endpoint
					neighborIP := path.PeerAddress.String()

					if neighborIP != "" && neighborIP != "<nil>" {
						// Try to find neighbor-specific endpoint
						clientsMutex.RLock()
						neighborEndpoint, found := neighborToEndpoint[neighborIP]
						clientsMutex.RUnlock()

						if found {
							endpoint = neighborEndpoint // Update endpoint for logging
							// Get or create client for this endpoint
							client, err := getClientForEndpoint(endpoint)
							if err != nil {
								logger.Error("Failed to get client for endpoint", slog.String("endpoint", endpoint), slog.Any("error", err))
								// Fall back to global client
								targetClient = globalClient
								endpoint = globalEndpoint // Reset to global for logging
							} else {
								targetClient = client
							}
						}
					}

					// Convert apiutil.Path to api.Path for forwarding
					forwardPath, err := apiutil.NewPath(path.Family, path.Nlri, path.Withdrawal, path.Attrs, time.Unix(path.Age, 0))
					if err != nil {
						logger.Error("Failed to convert path", slog.Any("error", err))
						continue
					}

					_, err = targetClient.AddPath(context.Background(), &api.AddPathRequest{
						TableType: api.TableType_TABLE_TYPE_GLOBAL,
						Path:      forwardPath,
					})
					if err != nil {
						logger.Error("Failed to forward BGP-LS path to Topolograph watcher", slog.String("endpoint", endpoint), slog.Any("error", err))
					} else {
						forwarded++
					}
				}
			}
			if forwarded > 0 {
				logger.Info("Forwarded BGP-LS paths to Topolograph watcher", slog.Int("count", forwarded))
			}
		},
		OnPeerUpdate: func(peer *apiutil.WatchEventMessage_PeerEvent, timestamp time.Time) {
			// Peer events are automatically streamed via WatchEvent to any subscribed clients.
			// Python watcher should subscribe to WatchEvent from bgplswatcher (port 50051) to receive peer events.
			// Log peer establishment for debugging.
			if peer != nil && peer.Type == apiutil.PEER_EVENT_STATE {
				if peer.Peer.State.SessionState == bgp.BGP_FSM_ESTABLISHED {
					peerIP := peer.Peer.State.NeighborAddress.String()
					tcpPort := peer.Peer.Transport.RemotePort
					logger.Info("BGP peer session established",
						slog.String("peer_ip", peerIP),
						slog.Int("tcp_port", int(tcpPort)),
						slog.String("note", "Peer events available via WatchEvent stream"))
				}
			}
		},
	}, server.WatchBestPath(false), server.WatchPeer()); err != nil {
		stdlog.Fatal(err)
	}

	logger.Info("WatchEvent configured to forward BGP-LS routes", slog.String("global_endpoint", globalEndpoint))

	// Add BGP neighbors from config
	if len(config.Neighbors) > 0 {
		for _, neighborEntry := range config.Neighbors {
			neighborConfig := neighborEntry.Config
			if neighborConfig.NeighborAddress == "" {
				logger.Info("Skipping neighbor with empty neighbor-address")
				continue
			}

			// Use neighbor-specific endpoint if provided, otherwise use global
			neighborEndpoint := neighborConfig.TopolographWatcherEndpoint
			if neighborEndpoint == "" {
				neighborEndpoint = globalEndpoint
			}

			// Map neighbor address to endpoint
			neighborIP := net.ParseIP(neighborConfig.NeighborAddress)
			if neighborIP != nil {
				clientsMutex.Lock()
				neighborToEndpoint[neighborIP.String()] = neighborEndpoint
				clientsMutex.Unlock()
				logger.Info("Mapped neighbor to endpoint", slog.String("neighbor", neighborConfig.NeighborAddress), slog.String("endpoint", neighborEndpoint))

				// Pre-create connection for this endpoint if different from global
				if neighborEndpoint != globalEndpoint {
					_, err := getClientForEndpoint(neighborEndpoint)
					if err != nil {
						stdlog.Fatalf("Failed to create connection for neighbor %s endpoint %s: %v", neighborConfig.NeighborAddress, neighborEndpoint, err)
					}
				}
			}

			logger.Info("Adding BGP peer", slog.String("neighbor", neighborConfig.NeighborAddress), slog.Int("asn", int(neighborConfig.PeerAS)))
			peer := &api.Peer{
				Conf: &api.PeerConf{
					NeighborAddress: neighborConfig.NeighborAddress,
					PeerAsn:         neighborConfig.PeerAS,
				},
				ApplyPolicy: &api.ApplyPolicy{
					ImportPolicy: &api.PolicyAssignment{
						DefaultAction: api.RouteAction_ROUTE_ACTION_ACCEPT,
					},
					ExportPolicy: &api.PolicyAssignment{
						DefaultAction: api.RouteAction_ROUTE_ACTION_REJECT,
					},
				},
				AfiSafis: []*api.AfiSafi{
					{
						Config: &api.AfiSafiConfig{
							Family: &api.Family{
								Afi:  api.Family_AFI_LS,
								Safi: api.Family_SAFI_LS,
							},
							Enabled: true,
						},
					},
				},
			}

			peer.Transport = &api.Transport{}
			if neighborConfig.PassiveMode {
				peer.Transport.PassiveMode = true
			}
			if neighborConfig.LocalAddress != "" {
				peer.Transport.LocalAddress = neighborConfig.LocalAddress
			} else if neighborEntry.Transport.Config.LocalAddress != "" {
				peer.Transport.LocalAddress = neighborEntry.Transport.Config.LocalAddress
			}

			// Set eBGP multihop configuration if enabled
			if neighborEntry.EbgpMultihop.Config.Enabled {
				peer.EbgpMultihop = &api.EbgpMultihop{
					Enabled:     true,
					MultihopTtl: neighborEntry.EbgpMultihop.Config.MultihopTtl,
				}
				logger.Info("eBGP multihop configured",
					slog.String("neighbor", neighborConfig.NeighborAddress),
					slog.Bool("enabled", true),
					slog.Int("multihop-ttl", int(neighborEntry.EbgpMultihop.Config.MultihopTtl)))
			}

			if err := s.AddPeer(context.Background(), &api.AddPeerRequest{
				Peer: peer,
			}); err != nil {
				stdlog.Fatalf("Failed to add peer %s: %v", neighborConfig.NeighborAddress, err)
			}
			logger.Info("BGP peer added successfully", slog.String("peer", neighborConfig.NeighborAddress), slog.String("endpoint", neighborEndpoint))
		}
	} else {
		logger.Info("No neighbors configured - running in local mode. Routes can be added manually via gobgp CLI")
	}

	// Wait forever
	select {}
}
