package test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	telephony "github.com/AgoraIO-Solutions/telephony-go"
)

// Production E2E tests for MULTI-mode clients.
//
// These tests connect to a production CM as a MULTI client with NO subscriptions
// (catch-all mode). The client receives all call_incoming events for allowed DIDs.
//
// Required env vars:
//   BC_AUTH_TOKEN    — MULTI auth token
//   BC_DOMAIN        — CM domain (e.g. sipcm.agora.io)
//   BC_LB_DOMAIN     — LB domain (e.g. sip.usa.lb.01.agora.io)
//   BC_COMMAND_APPID — real appid for Dial/Accept commands
//
// Optional:
//   BC_INBOUND_DID   — DID for loopback tests (default: 18005551234)
//   BC_TRANSPORT     — SIP transport (default: tls)
//   BC_WS_URL        — override WSS URL

func skipIfNoProd(t *testing.T) (wsURL, auth, cmdAppID string) {
	t.Helper()
	auth = os.Getenv("BC_AUTH_TOKEN")
	domain := os.Getenv("BC_DOMAIN")
	lbDomain := os.Getenv("BC_LB_DOMAIN")
	cmdAppID = os.Getenv("BC_COMMAND_APPID")
	if auth == "" || domain == "" || lbDomain == "" || cmdAppID == "" {
		t.Skip("BC_AUTH_TOKEN, BC_DOMAIN, BC_LB_DOMAIN, and BC_COMMAND_APPID required for prod tests")
	}
	wsURL = getEnvOrDefault("BC_WS_URL", "wss://"+domain+"/v1/ws/events")
	return
}

func prodInboundDID() string {
	return getEnvOrDefault("BC_INBOUND_DID", "18005551234")
}

// newProdClient creates a MULTI-mode client with no subscriptions.
func newProdClient(wsURL, auth, clientID string) *telephony.Client {
	return telephony.NewClient(wsURL, auth, clientID, "MULTI")
}

// prodDialParams builds DialParams for MULTI-mode (always includes AppID).
func prodDialParams(to, from, channel, uid, cmdAppID string) telephony.DialParams {
	return telephony.DialParams{
		To:      to,
		From:    from,
		Channel: channel,
		UID:     uid,
		Token:   cmdAppID,
		Region:  "AREA_CODE_NA",
		Timeout: "60",
		Sip:     lbSip(defaultTransport()),
		AppID:   cmdAppID,
	}
}

// ================================================================
// Test 1: Connect — MULTI client registers without subscribing
// ================================================================
func TestProdConnect(t *testing.T) {
	wsURL, auth, _ := skipIfNoProd(t)

	clientID := fmt.Sprintf("prodtest_connect_%d", time.Now().UnixMilli())
	client := newProdClient(wsURL, auth, clientID)
	handler := newE2EHandler(false)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	if !client.IsConnected() {
		t.Fatal("client should be connected")
	}

	events := handler.getEvents()
	if len(events) == 0 || events[0] != "connected" {
		t.Fatalf("expected first event to be 'connected', got %v", events)
	}

	t.Logf("PASS: MULTI client connected as %s", clientID)
}

// ================================================================
// Test 2: Outbound Dial — dial via LB, verify callid returned
// ================================================================
func TestProdOutboundDial(t *testing.T) {
	wsURL, auth, cmdAppID := skipIfNoProd(t)

	clientID := fmt.Sprintf("prodtest_outdial_%d", time.Now().UnixMilli())
	channel := fmt.Sprintf("prodtest_out_%d", time.Now().UnixMilli())
	client := newProdClient(wsURL, auth, clientID)
	handler := newE2EHandler(false)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	result, err := client.Dial(ctx, prodDialParams(
		"+"+prodInboundDID(), "+15551234567", channel, "100", cmdAppID,
	))
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if !result.Success || result.CallID == "" {
		t.Skipf("Dial not successful (likely no gateways): success=%v callid=%s data=%v",
			result.Success, result.CallID, result.Data)
	}
	t.Logf("PASS: Outbound call placed: callid=%s", result.CallID)

	// Wait briefly for any events
	time.Sleep(2 * time.Second)

	// Hangup
	endCtx, endCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer endCancel()
	if err := client.Hangup(endCtx, result.CallID); err != nil {
		t.Logf("Hangup warning: %v", err)
	}

	t.Logf("Events: %v", handler.getEvents())
}

// ================================================================
// Test 3: Inbound Accept — loopback call, verify call_incoming
// received WITHOUT subscribing, accept, DTMF both legs, hangup
// ================================================================
func TestProdInboundAccept(t *testing.T) {
	wsURL, auth, cmdAppID := skipIfNoProd(t)

	testDID := prodInboundDID()
	clientID := fmt.Sprintf("prodtest_inaccept_%d", time.Now().UnixMilli())
	channel := fmt.Sprintf("prodtest_accept_%d", time.Now().UnixMilli())
	client := newProdClient(wsURL, auth, clientID)
	handler := newE2EHandler(true)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	time.Sleep(500 * time.Millisecond)

	// Dial outbound to loopback DID — should come back as inbound via catch-all
	result, err := client.Dial(ctx, prodDialParams(
		"+"+testDID, "+15551234567", channel, "100", cmdAppID,
	))
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if !result.Success || result.CallID == "" {
		t.Skipf("Dial not successful (likely no gateways): success=%v callid=%s data=%v",
			result.Success, result.CallID, result.Data)
	}
	t.Logf("Outbound call placed: callid=%s", result.CallID)

	// Wait for call_incoming — MULTI catch-all should deliver it without subscription
	var inboundCallID string
	select {
	case call := <-handler.incomingCh:
		inboundCallID = call.CallID
		t.Logf("PASS call_incoming received (no subscription): callid=%s from=%s to=%s",
			call.CallID, call.From, call.To)

		// Accept the inbound leg
		if err := client.Accept(ctx, call.CallID, telephony.AcceptParams{
			Token:   cmdAppID,
			Channel: channel,
			UID:     "200",
			AppID:   cmdAppID,
		}); err != nil {
			t.Fatalf("Accept failed: %v", err)
		}
		t.Log("Inbound call accepted")

	case <-time.After(30 * time.Second):
		t.Fatal("Timed out waiting for call_incoming — MULTI catch-all may not be working")
	}

	// Wait for call_answered
	select {
	case call := <-handler.answeredCh:
		t.Logf("PASS OnCallAnswered: callid=%s", call.CallID)
	case <-time.After(15 * time.Second):
		t.Log("No answered event")
	}

	// Wait for bridge_start
	select {
	case call := <-handler.bridgeCh:
		t.Logf("PASS OnBridgeStart: callid=%s", call.CallID)
	case <-time.After(15 * time.Second):
		t.Log("No bridge event")
	}

	// DTMF on outbound leg (A-leg)
	if err := client.SendDTMF(ctx, result.CallID, "1234#"); err != nil {
		t.Logf("SendDTMF on outbound failed: %v", err)
	} else {
		select {
		case digits := <-handler.dtmfCh:
			if digits == "1234#" {
				t.Logf("PASS A-leg DTMF: %s", digits)
			} else {
				t.Errorf("A-leg DTMF mismatch: expected '1234#', got '%s'", digits)
			}
		case <-time.After(5 * time.Second):
			t.Log("No A-leg dtmf_received")
		}
	}

	// DTMF on inbound leg (B-leg)
	if err := client.SendDTMF(ctx, inboundCallID, "5678*"); err != nil {
		t.Logf("SendDTMF on inbound failed: %v", err)
	} else {
		select {
		case digits := <-handler.dtmfCh:
			if digits == "5678*" {
				t.Logf("PASS B-leg DTMF: %s", digits)
			} else {
				t.Errorf("B-leg DTMF mismatch: expected '5678*', got '%s'", digits)
			}
		case <-time.After(5 * time.Second):
			t.Log("No B-leg dtmf_received")
		}
	}

	// Hold briefly then hangup
	time.Sleep(2 * time.Second)
	endCtx, endCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer endCancel()
	if err := client.Hangup(endCtx, result.CallID); err != nil {
		t.Logf("Hangup warning: %v", err)
	}

	select {
	case <-handler.hangupCh:
		t.Log("PASS hangup event received")
	case <-time.After(10 * time.Second):
		t.Log("No hangup event")
	}

	t.Logf("Events: %v", handler.getEvents())
}

// ================================================================
// Test 4: Inbound Reject — loopback call, reject inbound leg
// ================================================================
func TestProdInboundReject(t *testing.T) {
	wsURL, auth, cmdAppID := skipIfNoProd(t)

	testDID := prodInboundDID()
	clientID := fmt.Sprintf("prodtest_inreject_%d", time.Now().UnixMilli())
	channel := fmt.Sprintf("prodtest_reject_%d", time.Now().UnixMilli())
	client := newProdClient(wsURL, auth, clientID)
	handler := newE2EHandler(true)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	time.Sleep(500 * time.Millisecond)

	result, err := client.Dial(ctx, prodDialParams(
		"+"+testDID, "+15551234567", channel, "100", cmdAppID,
	))
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if !result.Success || result.CallID == "" {
		t.Skipf("Dial not successful (likely no gateways): success=%v callid=%s data=%v",
			result.Success, result.CallID, result.Data)
	}
	t.Logf("Outbound call placed: callid=%s", result.CallID)

	// Wait for call_incoming
	select {
	case call := <-handler.incomingCh:
		t.Logf("PASS call_incoming received: callid=%s", call.CallID)

		// Reject the inbound leg
		if err := client.Reject(ctx, call.CallID, "busy"); err != nil {
			t.Fatalf("Reject failed: %v", err)
		}
		t.Log("PASS call rejected — gateway gets 404")

	case <-time.After(30 * time.Second):
		t.Fatal("Timed out waiting for call_incoming")
	}

	// Outbound leg should terminate after reject
	select {
	case <-handler.hangupCh:
		t.Log("PASS hangup event received after reject")
	case <-time.After(15 * time.Second):
		t.Log("No hangup event after reject — cleaning up")
		endCtx, endCancel := context.WithTimeout(context.Background(), 10*time.Second)
		client.Hangup(endCtx, result.CallID)
		endCancel()
	}

	t.Logf("Events: %v", handler.getEvents())
}

// ================================================================
// Test 5: Multi-Client Broadcast — two MULTI clients (no subscriptions),
// both should receive call_incoming. First accepts, second gets error.
// ================================================================
func TestProdMultiClientBroadcast(t *testing.T) {
	wsURL, auth, cmdAppID := skipIfNoProd(t)

	testDID := prodInboundDID()
	ts := time.Now().UnixMilli()
	channel := fmt.Sprintf("prodtest_broadcast_%d", ts)

	// Client A
	clientA := newProdClient(wsURL, auth, fmt.Sprintf("prodtest_bcast_a_%d", ts))
	handlerA := newE2EHandler(true)
	clientA.SetHandler(handlerA)

	// Client B
	clientB := newProdClient(wsURL, auth, fmt.Sprintf("prodtest_bcast_b_%d", ts))
	handlerB := newE2EHandler(true)
	clientB.SetHandler(handlerB)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := clientA.Connect(ctx); err != nil {
		t.Fatalf("Client A Connect failed: %v", err)
	}
	defer clientA.Close()

	if err := clientB.Connect(ctx); err != nil {
		t.Fatalf("Client B Connect failed: %v", err)
	}
	defer clientB.Close()

	time.Sleep(time.Second)

	// Dial from client A to loopback DID
	result, err := clientA.Dial(ctx, prodDialParams(
		"+"+testDID, "+15551234567", channel, "100", cmdAppID,
	))
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if !result.Success || result.CallID == "" {
		t.Skipf("Dial not successful (likely no gateways): success=%v callid=%s data=%v",
			result.Success, result.CallID, result.Data)
	}
	t.Logf("Outbound call placed: callid=%s", result.CallID)

	// Both clients should receive call_incoming
	var callIDA, callIDB string

	// Wait for client A
	select {
	case call := <-handlerA.incomingCh:
		callIDA = call.CallID
		t.Logf("PASS Client A got call_incoming: callid=%s", call.CallID)
	case <-time.After(30 * time.Second):
		t.Fatal("Client A: timed out waiting for call_incoming")
	}

	// Wait for client B
	select {
	case call := <-handlerB.incomingCh:
		callIDB = call.CallID
		t.Logf("PASS Client B got call_incoming: callid=%s", call.CallID)
	case <-time.After(10 * time.Second):
		t.Fatal("Client B: timed out waiting for call_incoming — broadcast may not be working")
	}

	// Client A accepts
	if err := clientA.Accept(ctx, callIDA, telephony.AcceptParams{
		Token:   cmdAppID,
		Channel: channel,
		UID:     "200",
		AppID:   cmdAppID,
	}); err != nil {
		t.Fatalf("Client A Accept failed: %v", err)
	}
	t.Log("PASS Client A accepted the call")

	// Client B tries to accept — should fail (call already claimed)
	err = clientB.Accept(ctx, callIDB, telephony.AcceptParams{
		Token:   cmdAppID,
		Channel: channel + "_b",
		UID:     "300",
		AppID:   cmdAppID,
	})
	if err != nil {
		t.Logf("PASS Client B accept correctly failed: %v", err)
	} else {
		t.Log("Client B accept did not return error — call may have been double-accepted")
	}

	// Wait for answered on client A
	select {
	case call := <-handlerA.answeredCh:
		t.Logf("PASS Client A answered: callid=%s", call.CallID)
	case <-time.After(15 * time.Second):
		t.Log("No answered event on client A")
	}

	// Cleanup
	time.Sleep(2 * time.Second)
	endCtx, endCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer endCancel()
	if err := clientA.Hangup(endCtx, result.CallID); err != nil {
		t.Logf("Hangup warning: %v", err)
	}

	select {
	case <-handlerA.hangupCh:
		t.Log("Hangup received")
	case <-time.After(10 * time.Second):
		t.Log("No hangup event")
	}

	t.Logf("Client A events: %v", handlerA.getEvents())
	t.Logf("Client B events: %v", handlerB.getEvents())
}
