// Example: subscribe to a DID, auto-accept incoming calls, wait for hangup or Ctrl+C.
//
// Environment variables:
//
//	CM_HOST    - Call Manager host (e.g. sip.dev.cm.01.agora.io)
//	AUTH_TOKEN - Auth token for the CM
//	APPID      - Agora App ID
//	DID        - Phone number (DID) to subscribe to (e.g. 18005551234)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
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
	client *telephony.Client
	appID  string
}

func (h *handler) OnConnected(sessionID string) {
	logEvent("connected", map[string]string{"session_id": sessionID})
}

func (h *handler) OnCallIncoming(call *telephony.Call) bool {
	logEvent("call_incoming", call)

	// Auto-accept the call
	channel := fmt.Sprintf("inbound_%s_%d", call.CallID[:8], time.Now().UnixMilli())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := h.client.Accept(ctx, call.CallID, telephony.AcceptParams{
		Token:   h.appID,
		Channel: channel,
		UID:     "200",
	})
	if err != nil {
		logEvent("accept_failed", map[string]string{"callid": call.CallID, "error": err.Error()})
	} else {
		logEvent("accepted", map[string]string{"callid": call.CallID, "channel": channel})
	}
	return true // claim the call
}

func (h *handler) OnCallRinging(call *telephony.Call) {
	logEvent("ringing", call)
}

func (h *handler) OnCallAnswered(call *telephony.Call) {
	logEvent("answered", call)
}

func (h *handler) OnBridgeStart(call *telephony.Call) {
	logEvent("bridge_start", call)
}

func (h *handler) OnBridgeEnd(call *telephony.Call) {
	logEvent("bridge_end", call)
}

func (h *handler) OnCallHangup(call *telephony.Call) {
	logEvent("hangup", call)
}

func (h *handler) OnError(err error) {
	logEvent("error", map[string]string{"error": err.Error()})
}

func (h *handler) OnDTMFReceived(call *telephony.Call, digits string) {
	logEvent("dtmf_received", map[string]string{"callid": call.CallID, "digits": digits})
}

func main() {
	host := requireEnv("CM_HOST")
	authToken := requireEnv("AUTH_TOKEN")
	appID := requireEnv("APPID")
	did := requireEnv("DID")

	wsURL := "wss://" + host + "/v1/ws/events"
	clientID := fmt.Sprintf("example_inbound_%d", time.Now().UnixMilli())

	client := telephony.NewClient(wsURL, authToken, clientID, appID)
	client.SetSubscribeNumbers([]string{did})

	h := &handler{client: client, appID: appID}
	client.SetHandler(h)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	log.Printf("Connecting to %s ...", wsURL)
	if err := client.Connect(ctx); err != nil {
		log.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	log.Printf("Subscribed to DID %s, waiting for calls... (Ctrl+C to quit)", did)

	// Wait for interrupt signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
}
