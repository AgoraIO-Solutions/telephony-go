# Telephony Go SDK -- End-to-End Tests

These tests run against a live Agora SIP Call Manager and gateway. They exercise the full call lifecycle: connect, dial, accept, DTMF, bridge, transfer, and hangup.

## Prerequisites

- Go 1.21 or later
- Network access to the Agora SIP Call Manager (CM) and load balancer (LB)
- Valid auth token and app ID for the target CM environment

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `BC_AUTH_TOKEN` | yes | Auth token for the CM (e.g. `Basic <your-auth-token>`) |
| `BC_APPID` | yes | Primary Agora App ID |
| `BC_DOMAIN` | yes | CM domain (e.g. `sip.dev.cm.01.agora.io`) |
| `BC_LB_DOMAIN` | yes | Load balancer domain (e.g. `sip.dev.lb.01.agora.io`) |
| `BC_APPID2` | no | Secondary App ID (for multi-appid tests) |
| `BC_INBOUND_DID` | no | DID for inbound loopback tests (default: `18005551234`) |
| `BC_TRANSPORT` | no | SIP transport: `udp`, `tcp`, or `tls` (default: `tls`) |
| `BC_WS_URL` | no | Override WebSocket URL (default: `wss://{BC_DOMAIN}/v1/ws/events`) |
| `BC_CM_URL` | no | Override CM HTTP URL (default: `https://{BC_DOMAIN}`) |
| `BC_COMMAND_APPID` | no | App ID for Dial/Accept commands in MULTI mode |
| `BC_MULTI_AUTH` | no | Auth token for MULTI-mode tests (default: same as `BC_AUTH_TOKEN`) |
| `BC_STATUS_PAGE_TOKEN` | no | Status page auth token (for video test log verification) |

## Running Tests

### Clone and set up

```bash
git clone https://github.com/AgoraIO-Solutions/telephony-go.git
cd telephony-go/test
```

### Set environment variables

```bash
export BC_AUTH_TOKEN="Basic <your-auth-token>"
export BC_APPID="<your-app-id>"
export BC_DOMAIN="<your-cm-domain>"
export BC_LB_DOMAIN="<your-lb-domain>"
```

### Run all e2e tests

```bash
go test -v -count=1 -run TestE2E -timeout 300s ./...
```

### Run a specific test

```bash
go test -v -count=1 -run TestE2EOutboundDTMF -timeout 120s ./...
```

### Run multi-appid tests (requires BC_APPID2)

```bash
BC_APPID2="<your-second-app-id>" go test -v -count=1 -run TestE2EMultiAppID -timeout 300s ./...
```

### Run transport soak test

```bash
go test -v -count=1 -run TestE2ETransportSoak -timeout 600s ./...
```

### Run unit tests only (mock server, no credentials needed)

The `helpers_test.go` file contains a mock WebSocket server and handler used by the unit tests in the main SDK repository. These are included here for reference but the unit tests themselves (connect, dial, accept, etc.) are in the SDK's own test suite.

## Test Files

| File | Description |
|------|-------------|
| `e2e_test.go` | Core e2e tests: connect, subscribe, outbound DTMF, inbound accept/reject, subscription filtering, full round-trip, video call, parallel load, event lifecycle, bridge/unbridge, transfer |
| `multi_appid_test.go` | Multi-AppID isolation: two clients with different appids use same channel:uid; MULTI-mode client sends appid per command |
| `multi_appid_load_test.go` | 80-call load test: single MULTI-mode client, concurrent calls across two appids, collision pairs, auto-accept inbound, DTMF both legs, duration log verification |
| `transport_soak_test.go` | Transport soak: 30 calls cycling UDP/TCP/TLS via load balancer, auto-accept, DTMF both legs, per-transport success tracking |
| `helpers_test.go` | Shared test infrastructure: mock WebSocket server, mock EventHandler, handshake helpers |

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| `BC_AUTH_TOKEN, BC_APPID, BC_DOMAIN, and BC_LB_DOMAIN required` | Missing required env vars | Set all four required env vars |
| `ws dial failed: dial tcp: lookup ... no such host` | DNS resolution failed for CM domain | Check `BC_DOMAIN` is correct and reachable |
| `registration failed: unauthorized` | Invalid auth token | Check `BC_AUTH_TOKEN` matches `authorization_{appid}` in CM config |
| `Dial not successful (likely no gateways)` | No gateways available for the region | Check gateway availability on CM status page |
| `Timed out waiting for call_incoming` | Gateway not routing inbound calls back | Verify `BC_INBOUND_DID` has no pinlookup configured (falls through to WS subscription) |
| `BC_APPID2 required for multi-appid tests` | Missing secondary appid | Set `BC_APPID2` or skip multi-appid tests |
| `BC_LB_DOMAIN env var is required` (panic) | Missing load balancer domain | Set `BC_LB_DOMAIN` to the LB hostname |
| `Too many failures` in load/soak tests | Gateway saturation or network issues | Reduce concurrency or check gateway health |
| Tests hang for a long time | Network timeout to CM or gateway | Check network connectivity, increase `-timeout` flag |
