package main

import (
	"context"
	"fmt"
	stdlog "log"
	"net"
	"os"
	"sync"

	toml "github.com/BurntSushi/toml"
	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/log"
	"github.com/osrg/gobgp/v4/pkg/server"
	"github.com/sirupsen/logrus"
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
	Config NeighborConfig `toml:"config"`
}

type NeighborConfig struct {
	NeighborAddress            string `toml:"neighbor-address"`
	PeerAS                     uint32 `toml:"peer-as"`
	TopolographWatcherEndpoint string `toml:"topolograph-watcher-endpoint"`
	PassiveMode                bool   `toml:"passive-mode"`
	LocalAddress               string `toml:"local-address"`
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
	logger := logrus.New()
	logger.SetLevel(logrus.InfoLevel)

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
	serverOpts = append(serverOpts, server.LoggerOption(&myLogger{logger: logger}))
	serverOpts = append(serverOpts, server.GrpcOption(grpcServerOpts))
	serverOpts = append(serverOpts, server.GrpcListenAddress("0.0.0.0:50051"))
	logger.Infof("GoBGP gRPC server listening on 0.0.0.0:50051 (gobgp CLI access enabled)")

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

		logger.Infof("Connecting to Topolograph watcher at %s...", endpoint)
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
		logger.Infof("Connected to Topolograph watcher at %s", endpoint)

		return client, nil
	}

	// Create client for global endpoint
	globalClient, err := getClientForEndpoint(globalEndpoint)
	if err != nil {
		stdlog.Fatal(err)
	}

	// Watch for BGP events and forward to Topolograph watcher
	// BatchSize=1 processes paths individually to avoid large message batches
	if err := s.WatchEvent(context.Background(), &api.WatchEventRequest{
		Table: &api.WatchEventRequest_Table{
			Filters: []*api.WatchEventRequest_Table_Filter{
				{Type: api.WatchEventRequest_Table_Filter_TYPE_BEST, Init: false}, // Only new changes, not initial dump
			},
		},
		BatchSize: 1, // Process paths one at a time to avoid huge batches
	}, func(r *api.WatchEventResponse) {
		if t := r.GetTable(); t != nil {
			if len(t.Paths) > 100 {
				logger.Warnf("WARNING: Received unusually large batch of %d paths!", len(t.Paths))
			}

			// Forward all BGP-LS paths to Topolograph watcher
			forwarded := 0
			for _, path := range t.Paths {
				if path.Family != nil && path.Family.Afi == api.Family_AFI_LS && path.Family.Safi == api.Family_SAFI_LS {
					// Determine which endpoint to use based on neighbor IP
					var targetClient api.GoBgpServiceClient = globalClient
					endpoint := globalEndpoint // Default to global endpoint
					neighborIP := path.NeighborIp

					if neighborIP != "" {
						// Try to find neighbor-specific endpoint
						clientsMutex.RLock()
						neighborEndpoint, found := neighborToEndpoint[neighborIP]
						clientsMutex.RUnlock()

						if found {
							endpoint = neighborEndpoint // Update endpoint for logging
							// Get or create client for this endpoint
							client, err := getClientForEndpoint(endpoint)
							if err != nil {
								logger.Errorf("Failed to get client for endpoint %s: %v", endpoint, err)
								// Fall back to global client
								targetClient = globalClient
								endpoint = globalEndpoint // Reset to global for logging
							} else {
								targetClient = client
							}
						}
					}

					// Create a copy of the path for forwarding (to avoid modifying the original)
					forwardPath := &api.Path{
						Nlri:       path.Nlri,
						Pattrs:     path.Pattrs,
						Age:        path.Age,
						IsWithdraw: path.IsWithdraw,
						Family:     path.Family,
					}

					_, err := targetClient.AddPath(context.Background(), &api.AddPathRequest{
						TableType: api.TableType_TABLE_TYPE_GLOBAL,
						Path:      forwardPath,
					})
					if err != nil {
						logger.Errorf("Failed to forward BGP-LS path to Topolograph watcher (endpoint: %s): %v", endpoint, err)
					} else {
						logger.Infof("Successfully forwarded BGP-LS path to Topolograph watcher (endpoint: %s)", endpoint)
						forwarded++
					}
				}
			}
			if forwarded > 0 {
				logger.Infof("Forwarded %d BGP-LS paths to Topolograph watcher", forwarded)
			}
		}
	}); err != nil {
		stdlog.Fatal(err)
	}

	logger.Infof("WatchEvent configured to forward BGP-LS routes (global endpoint: %s)", globalEndpoint)

	// Add BGP neighbors from config
	if len(config.Neighbors) > 0 {
		for _, neighborEntry := range config.Neighbors {
			neighborConfig := neighborEntry.Config
			if neighborConfig.NeighborAddress == "" {
				logger.Infof("Skipping neighbor with empty neighbor-address")
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
				logger.Infof("Mapped neighbor %s to endpoint %s", neighborConfig.NeighborAddress, neighborEndpoint)

				// Pre-create connection for this endpoint if different from global
				if neighborEndpoint != globalEndpoint {
					_, err := getClientForEndpoint(neighborEndpoint)
					if err != nil {
						stdlog.Fatalf("Failed to create connection for neighbor %s endpoint %s: %v", neighborConfig.NeighborAddress, neighborEndpoint, err)
					}
				}
			}

			logger.Infof("Adding BGP peer: %s (ASN: %d)", neighborConfig.NeighborAddress, neighborConfig.PeerAS)
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
			}

			if err := s.AddPeer(context.Background(), &api.AddPeerRequest{
				Peer: peer,
			}); err != nil {
				stdlog.Fatalf("Failed to add peer %s: %v", neighborConfig.NeighborAddress, err)
			}
			logger.Infof("BGP peer %s added successfully (will forward to endpoint: %s)", neighborConfig.NeighborAddress, neighborEndpoint)
		}
	} else {
		logger.Infof("No neighbors configured - running in local mode. Routes can be added manually via gobgp CLI")
	}

	// Wait forever
	select {}
}

type myLogger struct {
	logger *logrus.Logger
}

func (l *myLogger) Panic(msg string, fields log.Fields) {
	l.logger.WithFields(logrus.Fields(fields)).Panic(msg)
}

func (l *myLogger) Fatal(msg string, fields log.Fields) {
	l.logger.WithFields(logrus.Fields(fields)).Fatal(msg)
}

func (l *myLogger) Error(msg string, fields log.Fields) {
	l.logger.WithFields(logrus.Fields(fields)).Error(msg)
}

func (l *myLogger) Warn(msg string, fields log.Fields) {
	l.logger.WithFields(logrus.Fields(fields)).Warn(msg)
}

func (l *myLogger) Info(msg string, fields log.Fields) {
	l.logger.WithFields(logrus.Fields(fields)).Info(msg)
}

func (l *myLogger) Debug(msg string, fields log.Fields) {
	l.logger.WithFields(logrus.Fields(fields)).Debug(msg)
}

func (l *myLogger) SetLevel(level log.LogLevel) {
	l.logger.SetLevel(logrus.Level(int(level)))
}

func (l *myLogger) GetLevel() log.LogLevel {
	return log.LogLevel(l.logger.GetLevel())
}
