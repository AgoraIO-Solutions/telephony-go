package test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	telephony "github.com/AgoraIO-Solutions/telephony-go"

	"github.com/gorilla/websocket"
)

// TestDial verifies placing an outbound call:
//  1. Client sends {"action":"outbound","to":"...","from":"...",...}
//  2. Server responds with {"response":"outbound","data":{"success":true,"callid":"..."}}
//  3. Client parses the response into DialResult with correct Success and CallID
//  4. Call is tracked locally and accessible via GetActiveCalls()
func TestDial(t *testing.T) {
	server := startMockServer(t, func(conn *websocket.Conn) {
		doHandshake(conn, "s1", "c1")

		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var cmd map[string]interface{}
		json.Unmarshal(msg, &cmd)

		if cmd["action"] != "outbound" {
			t.Errorf("expected outbound action, got %v", cmd["action"])
		}
		if cmd["to"] != "+15551234567" {
			t.Errorf("expected to +15551234567, got %v", cmd["to"])
		}
		if cmd["from"] != "+15559876543" {
			t.Errorf("expected from +15559876543, got %v", cmd["from"])
		}
		if cmd["channel"] != "test-ch" {
			t.Errorf("expected channel test-ch, got %v", cmd["channel"])
		}
		if cmd["uid"] != "100" {
			t.Errorf("expected uid 100, got %v", cmd["uid"])
		}
		if cmd["region"] != "NA" {
			t.Errorf("expected region NA, got %v", cmd["region"])
		}

		resp := map[string]interface{}{
			"response":    "outbound",
			"status_code": 200,
			"data": map[string]interface{}{
				"success": true,
				"callid":  "call-abc-123",
			},
		}
		echoRequestID(cmd, resp)
		conn.WriteJSON(resp)

		time.Sleep(time.Second)
	})
	defer server.Close()

	client := telephony.NewClient(wsURLFromServer(server), "test-token", "c1", "test-appid")
	client.SetHandler(newMockHandler(false))

	ctx := context.Background()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result, err := client.Dial(dialCtx, telephony.DialParams{
		To:      "+15551234567",
		From:    "+15559876543",
		Channel: "test-ch",
		UID:     "100",
		Token:   "tok",
		Region:  "NA",
		Timeout: "30",
	})

	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if !result.Success {
		t.Error("expected success=true")
	}
	if result.CallID != "call-abc-123" {
		t.Errorf("expected callid call-abc-123, got %s", result.CallID)
	}

	// Verify call is tracked locally
	calls := client.GetActiveCalls()
	found := false
	for _, c := range calls {
		if c.CallID == "call-abc-123" {
			found = true
			if c.Direction != "outbound" {
				t.Errorf("expected direction outbound, got %s", c.Direction)
			}
			if c.To != "+15551234567" {
				t.Errorf("expected To +15551234567, got %s", c.To)
			}
		}
	}
	if !found {
		t.Error("call-abc-123 not found in GetActiveCalls()")
	}
}

// TestDialWithSipDomain verifies that the sip_domain parameter is correctly
// included in the outbound command when provided.
func TestDialWithSipDomain(t *testing.T) {
	var receivedDomain string

	server := startMockServer(t, func(conn *websocket.Conn) {
		doHandshake(conn, "s1", "c1")

		_, msg, _ := conn.ReadMessage()
		var cmd map[string]interface{}
		json.Unmarshal(msg, &cmd)
		receivedDomain, _ = cmd["sip_domain"].(string)

		resp := map[string]interface{}{
			"response":    "outbound",
			"status_code": 200,
			"data":        map[string]interface{}{"success": true, "callid": "call-d1"},
		}
		echoRequestID(cmd, resp)
		conn.WriteJSON(resp)
		time.Sleep(time.Second)
	})
	defer server.Close()

	client := telephony.NewClient(wsURLFromServer(server), "tok", "c1", "app")
	client.SetHandler(newMockHandler(false))

	ctx := context.Background()
	client.Connect(ctx)
	defer client.Close()

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client.Dial(dialCtx, telephony.DialParams{
		To:        "+1555",
		From:      "+1556",
		Channel:   "ch-d",
		UID:       "1",
		Token:     "t",
		Region:    "EU",
		Timeout:   "10",
		SipDomain: "gw.eu.example.com",
	})

	if receivedDomain != "gw.eu.example.com" {
		t.Errorf("expected sip_domain gw.eu.example.com, got %s", receivedDomain)
	}
}

// TestDialWithAudioScenario verifies that sdk_options and audio_scenario
// parameters are included in the outbound command when provided.
func TestDialWithAudioScenario(t *testing.T) {
	var receivedScenario, receivedSDKOpts string

	server := startMockServer(t, func(conn *websocket.Conn) {
		doHandshake(conn, "s1", "c1")

		_, msg, _ := conn.ReadMessage()
		var cmd map[string]interface{}
		json.Unmarshal(msg, &cmd)
		receivedScenario, _ = cmd["audio_scenario"].(string)
		receivedSDKOpts, _ = cmd["sdk_options"].(string)

		resp := map[string]interface{}{
			"response":    "outbound",
			"status_code": 200,
			"data":        map[string]interface{}{"success": true, "callid": "call-as1"},
		}
		echoRequestID(cmd, resp)
		conn.WriteJSON(resp)
		time.Sleep(time.Second)
	})
	defer server.Close()

	client := telephony.NewClient(wsURLFromServer(server), "tok", "c1", "app")
	client.SetHandler(newMockHandler(false))

	ctx := context.Background()
	client.Connect(ctx)
	defer client.Close()

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client.Dial(dialCtx, telephony.DialParams{
		To:            "+1555",
		From:          "+1556",
		Channel:       "ch-as",
		UID:           "1",
		Token:         "t",
		Region:        "NA",
		Timeout:       "10",
		AudioScenario: "10",
		SDKOptions:    `{"audioProfile":1}`,
	})

	if receivedScenario != "10" {
		t.Errorf("expected audio_scenario 10, got %s", receivedScenario)
	}
	if receivedSDKOpts != `{"audioProfile":1}` {
		t.Errorf("expected sdk_options {\"audioProfile\":1}, got %s", receivedSDKOpts)
	}
}

// TestDialWithoutAudioScenario verifies that audio_scenario and sdk_options
// are NOT included when empty (omitempty behavior).
func TestDialWithoutAudioScenario(t *testing.T) {
	var hasScenario, hasSDKOpts bool

	server := startMockServer(t, func(conn *websocket.Conn) {
		doHandshake(conn, "s1", "c1")

		_, msg, _ := conn.ReadMessage()
		var cmd map[string]interface{}
		json.Unmarshal(msg, &cmd)
		_, hasScenario = cmd["audio_scenario"]
		_, hasSDKOpts = cmd["sdk_options"]

		resp := map[string]interface{}{
			"response":    "outbound",
			"status_code": 200,
			"data":        map[string]interface{}{"success": true, "callid": "call-as2"},
		}
		echoRequestID(cmd, resp)
		conn.WriteJSON(resp)
		time.Sleep(time.Second)
	})
	defer server.Close()

	client := telephony.NewClient(wsURLFromServer(server), "tok", "c1", "app")
	client.SetHandler(newMockHandler(false))

	ctx := context.Background()
	client.Connect(ctx)
	defer client.Close()

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client.Dial(dialCtx, telephony.DialParams{
		To: "+1555", From: "+1556", Channel: "ch", UID: "1", Token: "t", Region: "NA", Timeout: "10",
	})

	if hasScenario {
		t.Error("audio_scenario should not be present when empty")
	}
	if hasSDKOpts {
		t.Error("sdk_options should not be present when empty")
	}
}

// TestDialFailure verifies that the client correctly reports a failed outbound
// call (success=false in response data).
func TestDialFailure(t *testing.T) {
	server := startMockServer(t, func(conn *websocket.Conn) {
		doHandshake(conn, "s1", "c1")
		_, msg, _ := conn.ReadMessage()
		var received map[string]interface{}
		json.Unmarshal(msg, &received)

		resp := map[string]interface{}{
			"response":    "outbound",
			"status_code": 503,
			"data": map[string]interface{}{
				"success": false,
				"error":   "No resource currently available",
			},
		}
		echoRequestID(received, resp)
		conn.WriteJSON(resp)
		time.Sleep(time.Second)
	})
	defer server.Close()

	client := telephony.NewClient(wsURLFromServer(server), "tok", "c1", "app")
	client.SetHandler(newMockHandler(false))

	ctx := context.Background()
	client.Connect(ctx)
	defer client.Close()

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result, err := client.Dial(dialCtx, telephony.DialParams{
		To: "+1555", From: "+1556", Channel: "ch", UID: "1", Token: "t", Region: "NA", Timeout: "10",
	})

	if err != nil {
		t.Fatalf("Dial returned error (should still return result): %v", err)
	}
	if result.Success {
		t.Error("expected success=false for failed dial")
	}
}

// TestDialTimeout verifies that the client returns a context error when the
// server doesn't respond in time.
func TestDialTimeout(t *testing.T) {
	server := startMockServer(t, func(conn *websocket.Conn) {
		doHandshake(conn, "s1", "c1")
		conn.ReadMessage() // read outbound — never reply
		time.Sleep(10 * time.Second)
	})
	defer server.Close()

	client := telephony.NewClient(wsURLFromServer(server), "tok", "c1", "app")
	client.SetHandler(newMockHandler(false))

	ctx := context.Background()
	client.Connect(ctx)
	defer client.Close()

	dialCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	_, err := client.Dial(dialCtx, telephony.DialParams{
		To: "+1555", From: "+1556", Channel: "ch", UID: "1", Token: "t", Region: "NA", Timeout: "10",
	})

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}
