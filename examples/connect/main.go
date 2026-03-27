// Example: connect to Agora SIP Call Manager, print session info, and disconnect.
//
// Environment variables:
//
//	CM_HOST    - Call Manager host (e.g. sip.dev.cm.01.agora.io)
//	AUTH_TOKEN - Auth token for the CM
//	APPID      - Agora App ID
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

type handler struct{}

func (h *handler) OnConnected(sessionID string) {
	logEvent("connected", map[string]string{"session_id": sessionID})
}

func (h *handler) OnCallIncoming(call *telephony.Call) bool { return false }
func (h *handler) OnCallRinging(call *telephony.Call)       {}
func (h *handler) OnCallAnswered(call *telephony.Call)      {}
func (h *handler) OnBridgeStart(call *telephony.Call)       {}
func (h *handler) OnBridgeEnd(call *telephony.Call)         {}
func (h *handler) OnCallHangup(call *telephony.Call)        {}
func (h *handler) OnError(err error) {
	logEvent("error", map[string]string{"error": err.Error()})
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

func main() {
	host := requireEnv("CM_HOST")
	authToken := requireEnv("AUTH_TOKEN")
	appID := requireEnv("APPID")

	wsURL := "wss://" + host + "/v1/ws/events"
	clientID := fmt.Sprintf("example_connect_%d", time.Now().UnixMilli())

	client := telephony.NewClient(wsURL, authToken, clientID, appID)
	client.SetHandler(&handler{})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	log.Printf("Connecting to %s ...", wsURL)
	if err := client.Connect(ctx); err != nil {
		log.Fatalf("Connect failed: %v", err)
	}

	log.Println("Connected")
	log.Printf("IsConnected: %v", client.IsConnected())

	if err := client.Close(); err != nil {
		log.Fatalf("Close failed: %v", err)
	}
	log.Println("Disconnected")
}
