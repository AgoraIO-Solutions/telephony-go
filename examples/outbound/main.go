// Example: place an outbound call, wait for answer, send DTMF, and hang up.
//
// Environment variables:
//
//	CM_HOST     - Call Manager host (e.g. sip.dev.cm.01.agora.io)
//	AUTH_TOKEN  - Auth token for the CM
//	APPID       - Agora App ID
//	TO_NUMBER   - Destination phone number (e.g. +15559876543)
//	FROM_NUMBER - Caller ID number (e.g. +15551234567)
//	REGION      - Gateway region (default: AREA_CODE_NA)
//	SIP         - SIP address for LB (e.g. lb.example.com:5081;transport=tls)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	telephony "github.com/AgoraIO-Solutions/telephony-go"
)

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func logEvent(event string, data interface{}) {
	entry := map[string]interface{}{
		"time":  time.Now().Format(time.RFC3339),
		"event": event,
		"data":  data,
	}
	b, _ := json.Marshal(entry)
	fmt.Println(string(b))
}

type handler struct {
	answeredCh chan *telephony.Call
	hangupCh   chan *telephony.Call
	dtmfCh     chan string
}

func (h *handler) OnConnected(sessionID string) {
	logEvent("connected", map[string]string{"session_id": sessionID})
}

func (h *handler) OnCallIncoming(call *telephony.Call) bool { return false }
func (h *handler) OnCallRinging(call *telephony.Call) {
	logEvent("ringing", call)
}

func (h *handler) OnCallAnswered(call *telephony.Call) {
	logEvent("answered", call)
	select {
	case h.answeredCh <- call:
	default:
	}
}

func (h *handler) OnBridgeStart(call *telephony.Call) {
	logEvent("bridge_start", call)
}

func (h *handler) OnBridgeEnd(call *telephony.Call) {
	logEvent("bridge_end", call)
}

func (h *handler) OnCallHangup(call *telephony.Call) {
	logEvent("hangup", call)
	select {
	case h.hangupCh <- call:
	default:
	}
}

func (h *handler) OnError(err error) {
	logEvent("error", map[string]string{"error": err.Error()})
}

func (h *handler) OnDTMFReceived(call *telephony.Call, digits string) {
	logEvent("dtmf_received", map[string]string{"callid": call.CallID, "digits": digits})
	select {
	case h.dtmfCh <- digits:
	default:
	}
}

func main() {
	host := requireEnv("CM_HOST")
	authToken := requireEnv("AUTH_TOKEN")
	appID := requireEnv("APPID")
	toNumber := requireEnv("TO_NUMBER")
	fromNumber := requireEnv("FROM_NUMBER")
	region := envOrDefault("REGION", "AREA_CODE_NA")
	sipAddr := requireEnv("SIP")

	wsURL := "wss://" + host + "/v1/ws/events"
	clientID := fmt.Sprintf("example_outbound_%d", time.Now().UnixMilli())
	channel := fmt.Sprintf("example_out_%d", time.Now().UnixMilli())

	h := &handler{
		answeredCh: make(chan *telephony.Call, 1),
		hangupCh:   make(chan *telephony.Call, 1),
		dtmfCh:     make(chan string, 1),
	}

	client := telephony.NewClient(wsURL, authToken, clientID, appID)
	client.SetHandler(h)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	log.Printf("Connecting to %s ...", wsURL)
	if err := client.Connect(ctx); err != nil {
		log.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	log.Printf("Dialing %s from %s ...", toNumber, fromNumber)
	result, err := client.Dial(ctx, telephony.DialParams{
		To:      toNumber,
		From:    fromNumber,
		Channel: channel,
		UID:     "100",
		Token:   appID,
		Region:  region,
		Timeout: "60",
		Sip:     sipAddr,
	})
	if err != nil {
		log.Fatalf("Dial failed: %v", err)
	}
	if !result.Success || result.CallID == "" {
		log.Fatalf("Dial not successful: %+v", result.Data)
	}
	log.Printf("Call placed: callid=%s", result.CallID)

	// Wait for answered
	select {
	case <-h.answeredCh:
		log.Println("Call answered")
	case <-time.After(30 * time.Second):
		log.Println("No answered event within 30s")
	}

	// Send DTMF
	log.Println("Sending DTMF: 1234#")
	if err := client.SendDTMF(ctx, result.CallID, "1234#"); err != nil {
		log.Printf("SendDTMF failed: %v", err)
	} else {
		select {
		case digits := <-h.dtmfCh:
			log.Printf("DTMF echo received: %s", digits)
		case <-time.After(5 * time.Second):
			log.Println("No DTMF echo (gateway may not echo)")
		}
	}

	// Wait 3 seconds then hang up
	time.Sleep(3 * time.Second)

	log.Println("Hanging up...")
	hangupCtx, hangupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer hangupCancel()
	if err := client.Hangup(hangupCtx, result.CallID); err != nil {
		log.Printf("Hangup error: %v", err)
	}

	select {
	case <-h.hangupCh:
		log.Println("Hangup confirmed")
	case <-time.After(10 * time.Second):
		log.Println("No hangup event")
	}
}
