# BGP-LS Watcher v1.0.3

## Changes

- Rebuilt against GoBGP v4.6.0 (was v4.0.0). Picks up upstream BGP-LS and gRPC fixes from the v4.1–v4.6 line.

## Docker

```bash
docker pull vadims06/bgplswatcher:v1.0.3
```

Integration tests (IS-IS P2P and transit, BGP-LS and GRE modes) pass against this build.
