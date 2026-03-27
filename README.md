# Agora Telephony Go SDK

Go SDK for Agora SIP telephony. Place and receive phone calls, send DTMF tones, bridge calls to Agora channels, and transfer calls -- all via a WebSocket connection to the Agora SIP Call Manager.

## Features

- Outbound calls (Dial, Hangup)
- Inbound call handling (Subscribe, Accept, Reject)
- DTMF send and receive
- Call bridge and unbridge (connect/disconnect Agora RTC channels)
- Call transfer
- Automatic reconnection with exponential backoff
- Thread-safe concurrent call tracking

## Installation

```bash
go get github.com/AgoraIO-Solutions/telephony-go
```

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    telephony "github.com/AgoraIO-Solutions/telephony-go"
)

func main() {
    client := telephony.NewClient(
        "wss://your-cm-host/v1/ws/events",
        "your-auth-token",
        "my-client-1",
        "your-app-id",
    )
    client.SetHandler(&MyHandler{})

    ctx := context.Background()
    if err := client.Connect(ctx); err != nil {
        log.Fatal(err)
    }
    defer client.Close()

    result, err := client.Dial(ctx, telephony.DialParams{
        To:      "+15559876543",
        From:    "+1555***4567",
        Channel: fmt.Sprintf("call_%d", time.Now().UnixMilli()),
        UID:     "100",
        Token:   "your-app-id",
        Region:  "AREA_CODE_NA",
        Timeout: "60",
        Sip:     "your-lb-host:5081;transport=tls",
    })
    if err != nil {
        log.Fatal(err)
    }
    log.Printf("Call placed: callid=%s", result.CallID)
}
```

Implement the `telephony.EventHandler` interface to receive call events:

```go
type MyHandler struct{}

func (h *MyHandler) OnConnected(sessionID string)            { log.Println("Connected:", sessionID) }
func (h *MyHandler) OnCallIncoming(call *telephony.Call) bool { return true } // return true to claim
func (h *MyHandler) OnCallRinging(call *telephony.Call)       { log.Println("Ringing:", call.CallID) }
func (h *MyHandler) OnCallAnswered(call *telephony.Call)      { log.Println("Answered:", call.CallID) }
func (h *MyHandler) OnBridgeStart(call *telephony.Call)       { log.Println("Bridge:", call.CallID) }
func (h *MyHandler) OnBridgeEnd(call *telephony.Call)         { log.Println("Unbridge:", call.CallID) }
func (h *MyHandler) OnCallHangup(call *telephony.Call)        { log.Println("Hangup:", call.CallID) }
func (h *MyHandler) OnError(err error)                        { log.Println("Error:", err) }
```

Optionally implement `telephony.DTMFHandler` for DTMF events:

```go
func (h *MyHandler) OnDTMFReceived(call *telephony.Call, digits string) {
    log.Printf("DTMF on %s: %s", call.CallID, digits)
}
```

## Examples

See the [examples/](examples/) directory for complete working programs:

| Example | Description |
|---------|-------------|
| [connect](examples/connect/) | Connect to CM, print session info, disconnect |
| [outbound](examples/outbound/) | Place outbound call, send DTMF, hang up |
| [inbound](examples/inbound/) | Subscribe to DID, auto-accept incoming calls |

## Tests

See the [test/](test/) directory for end-to-end tests against a live Call Manager and gateway.

See [test/README.md](test/README.md) for setup and run instructions.

## Environment Variables

### Examples

| Variable | Required | Description |
|----------|----------|-------------|
| `CM_HOST` | yes | Call Manager hostname (e.g. `sip.example.agora.io`) |
| `AUTH_TOKEN` | yes | Auth token for the CM |
| `APPID` | yes | Agora App ID |
| `TO_NUMBER` | outbound | Destination phone number (e.g. `+15559876543`) |
| `FROM_NUMBER` | outbound | Caller ID number (e.g. `+1555***4567`) |
| `REGION` | no | Gateway region (default: `AREA_CODE_NA`) |
| `SIP` | outbound | SIP address for load balancer |
| `DID` | inbound | DID to subscribe to for inbound calls |

### Tests

| Variable | Required | Description |
|----------|----------|-------------|
| `BC_AUTH_TOKEN` | yes | Auth token for the CM |
| `BC_APPID` | yes | Primary Agora App ID |
| `BC_DOMAIN` | yes | CM domain (e.g. `sip.dev.cm.01.agora.io`) |
| `BC_LB_DOMAIN` | yes | Load balancer domain (e.g. `sip.dev.lb.01.agora.io`) |
| `BC_APPID2` | no | Secondary App ID (for multi-appid tests) |
| `BC_INBOUND_DID` | no | DID for inbound loopback tests (default: `18005551234`) |
| `BC_TRANSPORT` | no | SIP transport: `udp`, `tcp`, or `tls` (default: `tls`) |
| `BC_STATUS_PAGE_TOKEN` | no | Status page auth token (for video test log verification) |

## API Reference

### Client

- `NewClient(wsURL, authToken, clientID, appID) *Client` -- create a new client
- `SetHandler(h EventHandler)` -- set the event handler
- `SetSubscribeNumbers(numbers []string)` -- set DID subscriptions (call before Connect)
- `Connect(ctx) error` -- connect and register with the CM
- `Subscribe(ctx, numbers []string) error` -- update subscriptions on a live connection
- `Dial(ctx, DialParams) (*DialResult, error)` -- place an outbound call
- `Accept(ctx, callid string, AcceptParams) error` -- accept an inbound call
- `Reject(ctx, callid, reason string) error` -- reject an inbound call
- `Bridge(ctx, callid string, BridgeParams) error` -- bridge call to Agora channel
- `Unbridge(ctx, callid string) error` -- remove Agora channel bridge
- `Transfer(ctx, callid, destination, leg string) error` -- transfer a call
- `SendDTMF(ctx, callid, digits string) error` -- send DTMF tones
- `Hangup(ctx, callid string) error` -- hang up a call
- `GetActiveCalls() []*Call` -- list active calls
- `IsConnected() bool` -- check connection status
- `Close() error` -- disconnect
