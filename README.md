# BGP-LS Watcher

Forwards BGP-LS messages from GoBGP to Topolograph watcher via gRPC.

## Configuration

Configure via `config.toml`:

```toml
[global.config]
  as = 64512
  router-id = "192.168.255.1"
  topolograph-watcher-endpoint = "localhost:50052"

[[neighbors]]
  [neighbors.config]
    neighbor-address = "10.0.255.1"
    peer-as = 65001
    topolograph-watcher-endpoint = "localhost:50052"
    passive-mode = false
```

Several `[[neighbors]]` can share one bgplswatcher, each with its own
`topolograph-watcher-endpoint`. bgplswatcher never advertises anything to a
router, not even one router's withdrawals to another: received paths stay out
of the global RIB and are forwarded from each neighbor's adj-in. So
`gobgp global rib -a ls` is empty; use `gobgp neighbor <ip> adj-in -a ls`.

## Usage

```bash
# Build
go build -o bgplswatcher

# Run (reads config.toml)
./bgplswatcher
```

## Docker

```bash
docker-compose up -d
```

#### Developing
To rebuild the image this repository should be placed in gobgp/cmd folder


# Online Resources. Contacts
* Telegram group: [https://t.me/topolograph](https://t.me/topolograph)
* Main site: https://topolograph.com
* Docker version of site: https://github.com/Vadims06/topolograph-docker
* MCP: https://github.com/Vadims06/topolograph-mcp-server
* OSPFWatcher: https://github.com/Vadims06/ospfwatcher
* ISISWatcher: https://github.com/Vadims06/isiswatcher