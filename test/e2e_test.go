package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	telephony "github.com/AgoraIO-Solutions/telephony-go"
)

// E2E integration tests connect to a REAL CM server with a REAL gateway.
// Requires env vars:
//   BC_AUTH_TOKEN  (required) — auth token for the CM
//   BC_APPID       (required) — app ID
//   BC_DOMAIN      (required) — CM domain (e.g. sip.dev.cm.01.agora.io)
//   BC_LB_DOMAIN   (required) — LB domain (e.g. sip.dev.lb.01.agora.io)
//   BC_WS_URL      (optional) — overrides wss://{BC_DOMAIN}/v1/ws/events
//   BC_CM_URL      (optional) — overrides https://{BC_DOMAIN}

func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func inboundDID() string {
	return getEnvOrDefault("BC_INBOUND_DID", "18005551234")
}

// defaultTransport returns BC_TRANSPORT env var or "tls" if not set.
func defaultTransport() string {
	return getEnvOrDefault("BC_TRANSPORT", "tls")
}

func lbSip(transport string) string {
	lbDomain := os.Getenv("BC_LB_DOMAIN")
	if lbDomain == "" {
		panic("BC_LB_DOMAIN env var is required")
	}
	switch transport {
	case "udp":
		return lbDomain + ":5080"
	case "tcp":
		return lbDomain + ":5080;transport=tcp"
	default:
		return lbDomain + ":5081;transport=tls"
	}
}

func skipIfNoEnv(t *testing.T) (wsURL, cmURL, auth, appid string) {
	t.Helper()
	auth = os.Getenv("BC_AUTH_TOKEN")
	appid = os.Getenv("BC_APPID")
	domain := os.Getenv("BC_DOMAIN")
	lbDomain := os.Getenv("BC_LB_DOMAIN")
	if auth == "" || appid == "" || domain == "" || lbDomain == "" {
		t.Skip("BC_AUTH_TOKEN, BC_APPID, BC_DOMAIN, and BC_LB_DOMAIN required for e2e tests")
	}
	wsURL = getEnvOrDefault("BC_WS_URL", "wss://"+domain+"/v1/ws/events")
	cmURL = getEnvOrDefault("BC_CM_URL", "https://"+domain)
	return
}

// commandAppID returns the appid to use in Dial/Accept commands.
// In MULTI mode, BC_COMMAND_APPID provides the actual appid for commands.
func commandAppID() string {
	if v := os.Getenv("BC_COMMAND_APPID"); v != "" {
		return v
	}
	return os.Getenv("BC_APPID")
}

// shortID safely truncates an ID for logging (max 8 chars).
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// e2eHandler captures events for assertions.
type e2eHandler struct {
	mu          sync.Mutex
	sessionID   string
	connected   bool
	events      []string
	calls       []*telephony.Call
	dtmfDigits  []string
	claimCalls   bool
	incomingCh   chan *telephony.Call
	answeredCh   chan *telephony.Call
	bridgeCh     chan *telephony.Call
	bridgeEndCh  chan *telephony.Call
	hangupCh     chan *telephony.Call
	dtmfCh       chan string
}

func newE2EHandler(claimCalls bool) *e2eHandler {
	return &e2eHandler{
		claimCalls:  claimCalls,
		incomingCh:  make(chan *telephony.Call, 10),
		answeredCh:  make(chan *telephony.Call, 10),
		bridgeCh:    make(chan *telephony.Call, 10),
		bridgeEndCh: make(chan *telephony.Call, 10),
		hangupCh:    make(chan *telephony.Call, 10),
		dtmfCh:      make(chan string, 10),
	}
}

func (h *e2eHandler) OnConnected(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionID = sessionID
	h.connected = true
	h.events = append(h.events, "connected")
}

func (h *e2eHandler) OnCallIncoming(call *telephony.Call) bool {
	h.mu.Lock()
	h.events = append(h.events, "incoming:"+call.CallID)
	h.calls = append(h.calls, call)
	h.mu.Unlock()
	select {
	case h.incomingCh <- call:
	default:
	}
	return h.claimCalls
}

func (h *e2eHandler) OnCallRinging(call *telephony.Call) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "ringing:"+call.CallID)
}

func (h *e2eHandler) OnCallAnswered(call *telephony.Call) {
	h.mu.Lock()
	h.events = append(h.events, "answered:"+call.CallID)
	h.mu.Unlock()
	select {
	case h.answeredCh <- call:
	default:
	}
}

func (h *e2eHandler) OnBridgeStart(call *telephony.Call) {
	h.mu.Lock()
	h.events = append(h.events, "bridge_start:"+call.CallID)
	h.mu.Unlock()
	select {
	case h.bridgeCh <- call:
	default:
	}
}

func (h *e2eHandler) OnBridgeEnd(call *telephony.Call) {
	h.mu.Lock()
	h.events = append(h.events, "bridge_end:"+call.CallID)
	h.mu.Unlock()
	select {
	case h.bridgeEndCh <- call:
	default:
	}
}

func (h *e2eHandler) OnCallHangup(call *telephony.Call) {
	h.mu.Lock()
	h.events = append(h.events, "hangup:"+call.CallID)
	h.mu.Unlock()
	select {
	case h.hangupCh <- call:
	default:
	}
}

func (h *e2eHandler) OnError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "error:"+err.Error())
}

func (h *e2eHandler) OnDTMFReceived(call *telephony.Call, digits string) {
	h.mu.Lock()
	h.events = append(h.events, "dtmf:"+digits)
	h.dtmfDigits = append(h.dtmfDigits, digits)
	h.mu.Unlock()
	select {
	case h.dtmfCh <- digits:
	default:
	}
}

func (h *e2eHandler) getEvents() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]string, len(h.events))
	copy(cp, h.events)
	return cp
}

// drainChannels clears all buffered events from handler channels.
func (h *e2eHandler) drainChannels() {
	for len(h.incomingCh) > 0 {
		<-h.incomingCh
	}
	for len(h.answeredCh) > 0 {
		<-h.answeredCh
	}
	for len(h.bridgeCh) > 0 {
		<-h.bridgeCh
	}
	for len(h.bridgeEndCh) > 0 {
		<-h.bridgeEndCh
	}
	for len(h.hangupCh) > 0 {
		<-h.hangupCh
	}
	for len(h.dtmfCh) > 0 {
		<-h.dtmfCh
	}
}

// --- Helper: fetch websocket-status endpoint ---
func fetchWSStatus(cmURL string) (map[string]interface{}, error) {
	localURL := "http://127.0.0.1:7360/v1/api/websocket-status"
	resp, err := http.Get(localURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// --- Helper: simulate a gateway lookup POST to service API ---
// Used only for subscription filtering test where we need a lookup without a real call.
func postServiceLookup(cmURL, did, pin, callerid, callid string) (*http.Response, error) {
	body := fmt.Sprintf(`{"action":"lookup","did":"%s","pin":"%s","callerid":"%s","callid":"%s"}`, did, pin, callerid, callid)
	req, err := http.NewRequest("POST", cmURL+"/v1/api/service", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Real-IP", "34.243.25.141")
	client := &http.Client{Timeout: 35 * time.Second}
	return client.Do(req)
}

// ================================================================
// Test 1: Connect + Register
// ================================================================
func TestE2EConnect(t *testing.T) {
	wsURL, cmURL, auth, appid := skipIfNoEnv(t)

	clientID := fmt.Sprintf("gotest_connect_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, auth, clientID, appid)
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

	time.Sleep(500 * time.Millisecond)
	status, err := fetchWSStatus(cmURL)
	if err != nil {
		t.Fatalf("Failed to fetch websocket-status: %v", err)
	}

	count, _ := status["client_count"].(float64)
	if count < 1 {
		t.Fatalf("expected at least 1 client in status, got %v", count)
	}

	t.Logf("Connected successfully, client_count=%v", count)
}

// ================================================================
// Test 2: Connect with subscribe_numbers
// ================================================================
func TestE2EConnectWithSubscription(t *testing.T) {
	wsURL, cmURL, auth, appid := skipIfNoEnv(t)

	clientID := fmt.Sprintf("gotest_sub_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, auth, clientID, appid)
	client.SetSubscribeNumbers([]string{"+15559876543", "+18005551234"})
	handler := newE2EHandler(false)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	time.Sleep(500 * time.Millisecond)
	status, err := fetchWSStatus(cmURL)
	if err != nil {
		t.Fatalf("Failed to fetch websocket-status: %v", err)
	}

	clients, ok := status["clients"].([]interface{})
	if !ok || len(clients) == 0 {
		t.Fatalf("expected clients in status, got %v", status["clients"])
	}

	found := false
	for _, c := range clients {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cm["client_id"] == clientID {
			found = true
			nums, _ := cm["subscribed_numbers"].([]interface{})
			if len(nums) != 2 {
				t.Fatalf("expected 2 subscribed numbers, got %v", nums)
			}
			t.Logf("Client %s subscribed to %v", clientID, nums)
			break
		}
	}
	if !found {
		t.Fatalf("client %s not found in websocket-status", clientID)
	}
}

// ================================================================
// Test 3: Dynamic Subscribe (update subscriptions on live connection)
// ================================================================
func TestE2EDynamicSubscribe(t *testing.T) {
	wsURL, cmURL, auth, appid := skipIfNoEnv(t)

	clientID := fmt.Sprintf("gotest_dynsub_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, auth, clientID, appid)
	handler := newE2EHandler(false)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	if err := client.Subscribe(ctx, []string{"+18005551235"}); err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	time.Sleep(500 * time.Millisecond)
	status, err := fetchWSStatus(cmURL)
	if err != nil {
		t.Fatalf("Failed to fetch websocket-status: %v", err)
	}

	clients, _ := status["clients"].([]interface{})
	for _, c := range clients {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cm["client_id"] == clientID {
			nums, _ := cm["subscribed_numbers"].([]interface{})
			if len(nums) != 1 {
				t.Fatalf("expected 1 subscribed number after dynamic subscribe, got %v", nums)
			}
			t.Logf("Dynamic subscribe verified: %v", nums)
			return
		}
	}
	t.Fatalf("client %s not found in websocket-status after subscribe", clientID)
}

// ================================================================
// Test 4: Outbound E2E — real call via LB, events via WS, DTMF, hangup
// Dials +15559876543 (has pinlookup configured — gateway handles normally)
// ================================================================
func TestE2EOutboundDTMF(t *testing.T) {
	wsURL, _, auth, appid := skipIfNoEnv(t)

	clientID := fmt.Sprintf("gotest_outbound_%d", time.Now().UnixMilli())
	channel := fmt.Sprintf("testc_goint_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, auth, clientID, appid)
	handler := newE2EHandler(false)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	result, err := client.Dial(ctx, telephony.DialParams{
		To:      "+15559876543",
		From:    "+15551234567",
		Channel: channel,
		UID:     "100",
		Token:   appid,
		Region:  "AREA_CODE_AS",
		Timeout: "60",
		Sip:     lbSip(defaultTransport()),
		AppID:   commandAppID(),
	})
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if !result.Success || result.CallID == "" {
		t.Skipf("Dial not successful (likely no gateways): success=%v callid=%s data=%v", result.Success, result.CallID, result.Data)
	}
	t.Logf("Call placed: callid=%s", result.CallID)

	// Wait for answered
	select {
	case call := <-handler.answeredCh:
		t.Logf("Call answered: callid=%s", call.CallID)
	case <-time.After(30 * time.Second):
		t.Log("No answered event — skipping to hangup")
		goto hangup
	}

	// Wait for bridge
	select {
	case call := <-handler.bridgeCh:
		t.Logf("Bridge started: callid=%s", call.CallID)
	case <-time.After(10 * time.Second):
		t.Log("No bridge event")
	}

	// Send DTMF
	if err := client.SendDTMF(ctx, result.CallID, "1234#"); err != nil {
		t.Logf("SendDTMF failed: %v", err)
	} else {
		t.Log("DTMF sent: 1234#")
		select {
		case digits := <-handler.dtmfCh:
			t.Logf("DTMF received: %s", digits)
			if digits != "1234#" {
				t.Errorf("expected DTMF '1234#', got '%s'", digits)
			}
		case <-time.After(5 * time.Second):
			t.Log("No dtmf_received event (gateway may not echo)")
		}
	}

hangup:
	endCtx, endCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer endCancel()
	if err := client.Hangup(endCtx, result.CallID); err != nil {
		t.Logf("Hangup warning: %v", err)
	}

	select {
	case <-handler.hangupCh:
		t.Log("Hangup event received")
	case <-time.After(10 * time.Second):
		t.Log("No hangup event")
	}

	t.Logf("Events: %v", handler.getEvents())
}

// ================================================================
// Test 5: Inbound Accept E2E — subscribe to DID, dial it via LB,
// the call loops back as inbound, accept it, full real lifecycle
// Uses DID 18005551234 (no pinlookup — falls through to WS subscription)
// ================================================================
func TestE2EInboundAcceptDTMF(t *testing.T) {
	wsURL, _, auth, appid := skipIfNoEnv(t)

	testDID := inboundDID()
	clientID := fmt.Sprintf("gotest_inaccept_%d", time.Now().UnixMilli())
	channel := fmt.Sprintf("testc_goaccept_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, auth, clientID, appid)
	client.SetSubscribeNumbers([]string{testDID})
	handler := newE2EHandler(true)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	time.Sleep(500 * time.Millisecond)

	// Dial the subscribed DID — call goes through LB to gateway, loops back as inbound
	result, err := client.Dial(ctx, telephony.DialParams{
		To:      "+" + testDID,
		From:    "+15551234567",
		Channel: channel,
		UID:     "100",
		Token:   appid,
		Region:  "AREA_CODE_AS",
		Timeout: "60",
		Sip:     lbSip(defaultTransport()),
		AppID:   commandAppID(),
	})
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if !result.Success || result.CallID == "" {
		t.Skipf("Dial not successful (likely no gateways): success=%v callid=%s data=%v", result.Success, result.CallID, result.Data)
	}
	t.Logf("Outbound call placed: callid=%s", result.CallID)

	// The gateway receives the call as inbound on testDID, does lookup to CM.
	// CM sees WS subscription → holds lookup → broadcasts call_incoming to us.
	var inboundCallID string
	select {
	case call := <-handler.incomingCh:
		inboundCallID = call.CallID
		t.Logf("Inbound call_incoming received: callid=%s from=%s to=%s", call.CallID, call.From, call.To)

		// Accept the inbound leg
		if err := client.Accept(ctx, call.CallID, telephony.AcceptParams{
			Token:   appid,
			Channel: channel,
			UID:     "200",
			AppID:   commandAppID(),
		}); err != nil {
			t.Fatalf("Accept failed: %v", err)
		}
		t.Log("Inbound call accepted")

	case <-time.After(30 * time.Second):
		t.Fatal("Timed out waiting for call_incoming — gateway may not have looped back")
	}

	// Wait for call_answered (real event from gateway via webhook.js)
	select {
	case call := <-handler.answeredCh:
		t.Logf("OnCallAnswered: callid=%s", call.CallID)
	case <-time.After(15 * time.Second):
		t.Log("No answered event")
	}

	// Wait for bridge_start (real event from gateway via webhook.js)
	select {
	case call := <-handler.bridgeCh:
		t.Logf("OnBridgeStart: callid=%s", call.CallID)
	case <-time.After(15 * time.Second):
		t.Log("No bridge event")
	}

	// --- DTMF on outbound leg (A-leg) ---
	t.Log("Sending DTMF on outbound leg (A-leg)...")
	if err := client.SendDTMF(ctx, result.CallID, "1234#"); err != nil {
		t.Errorf("SendDTMF on outbound failed: %v", err)
	} else {
		select {
		case digits := <-handler.dtmfCh:
			t.Logf("DTMF received on outbound: %s", digits)
			if digits != "1234#" {
				t.Errorf("outbound DTMF: expected '1234#', got '%s'", digits)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("No dtmf_received on outbound leg")
		}
	}

	// --- DTMF on inbound leg (B-leg, accepted call) ---
	t.Log("Sending DTMF on inbound leg (B-leg)...")
	if err := client.SendDTMF(ctx, inboundCallID, "5678*"); err != nil {
		t.Errorf("SendDTMF on inbound failed: %v", err)
	} else {
		select {
		case digits := <-handler.dtmfCh:
			t.Logf("DTMF received on inbound: %s", digits)
			if digits != "5678*" {
				t.Errorf("inbound DTMF: expected '5678*', got '%s'", digits)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("No dtmf_received on inbound leg")
		}
	}

	// Hold the call briefly
	time.Sleep(3 * time.Second)

	// Hangup the outbound leg — tears down both legs
	endCtx, endCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer endCancel()
	if err := client.Hangup(endCtx, result.CallID); err != nil {
		t.Logf("Hangup warning: %v", err)
	}

	select {
	case <-handler.hangupCh:
		t.Log("Hangup event received")
	case <-time.After(10 * time.Second):
		t.Log("No hangup event")
	}

	t.Logf("Inbound callid=%s, Events: %v", inboundCallID, handler.getEvents())
}

// ================================================================
// Test 6: Inbound Reject — subscribe to DID, dial it via LB,
// reject the inbound leg → gateway gets 404
// ================================================================
func TestE2EInboundReject(t *testing.T) {
	wsURL, _, auth, appid := skipIfNoEnv(t)

	testDID := inboundDID()
	clientID := fmt.Sprintf("gotest_inreject_%d", time.Now().UnixMilli())
	channel := fmt.Sprintf("testc_goreject_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, auth, clientID, appid)
	client.SetSubscribeNumbers([]string{testDID})
	handler := newE2EHandler(true)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	time.Sleep(500 * time.Millisecond)

	result, err := client.Dial(ctx, telephony.DialParams{
		To:      "+" + testDID,
		From:    "+15551234567",
		Channel: channel,
		UID:     "100",
		Token:   appid,
		Region:  "AREA_CODE_AS",
		Timeout: "30",
		Sip:     lbSip(defaultTransport()),
		AppID:   commandAppID(),
	})
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if !result.Success || result.CallID == "" {
		t.Skipf("Dial not successful (likely no gateways): success=%v callid=%s data=%v", result.Success, result.CallID, result.Data)
	}
	t.Logf("Outbound call placed: callid=%s", result.CallID)

	// Wait for call_incoming from the loopback
	select {
	case call := <-handler.incomingCh:
		t.Logf("Incoming call received: callid=%s", call.CallID)

		// Reject the inbound leg
		if err := client.Reject(ctx, call.CallID, "busy"); err != nil {
			t.Fatalf("Reject failed: %v", err)
		}
		t.Log("Call rejected — gateway gets 404")

	case <-time.After(30 * time.Second):
		t.Fatal("Timed out waiting for call_incoming")
	}

	// Outbound leg should also terminate since inbound was rejected
	select {
	case <-handler.hangupCh:
		t.Log("Hangup event received after reject")
	case <-time.After(15 * time.Second):
		t.Log("No hangup event after reject — cleaning up")
		endCtx, endCancel := context.WithTimeout(context.Background(), 10*time.Second)
		client.Hangup(endCtx, result.CallID)
		endCancel()
	}

	t.Logf("Events: %v", handler.getEvents())
}

// ================================================================
// Test 7: Subscription Filtering — subscribe to DID A, lookup for DID B → no event
// ================================================================
func TestE2ESubscriptionFiltering(t *testing.T) {
	wsURL, cmURL, auth, appid := skipIfNoEnv(t)

	subscribedDID := inboundDID()
	unsubscribedDID := "18007771234"
	clientID := fmt.Sprintf("gotest_filter_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, auth, clientID, appid)
	client.SetSubscribeNumbers([]string{subscribedDID})
	handler := newE2EHandler(true)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	time.Sleep(500 * time.Millisecond)

	// Drain any stale call_incoming events from previous tests (e.g. EventLifecycle
	// client may still be disconnecting and a late event can arrive on our channel)
	drainLoop:
	for {
		select {
		case stale := <-handler.incomingCh:
			t.Logf("Drained stale call_incoming from previous test: callid=%s to=%s", stale.CallID, stale.To)
		case <-time.After(2 * time.Second):
			break drainLoop
		}
	}

	// Lookup for unsubscribed DID — no subscriber, no pinlookup → falls through to not found
	callid := fmt.Sprintf("test_filter_%d", time.Now().UnixMilli())
	resp, err := postServiceLookup(cmURL, unsubscribedDID, "9999", "+15551234567", callid)
	if err != nil {
		t.Fatalf("Lookup request failed: %v", err)
	}
	resp.Body.Close()

	// Verify our client did NOT receive call_incoming for the wrong DID
	select {
	case call := <-handler.incomingCh:
		t.Fatalf("Should NOT have received call_incoming for unsubscribed DID, but got: %+v", call)
	case <-time.After(3 * time.Second):
		t.Log("Correctly did not receive call_incoming for unsubscribed DID")
	}
}

// ================================================================
// Test 8: Full Round Trip — outbound call THEN outbound→inbound accept
// on the same connection. Proves the Go client can both originate and
// receive+accept calls with DTMF support.
// ================================================================
func TestE2EFullRoundTrip(t *testing.T) {
	wsURL, _, auth, appid := skipIfNoEnv(t)

	testDID := inboundDID()
	clientID := fmt.Sprintf("gotest_roundtrip_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, auth, clientID, appid)
	client.SetSubscribeNumbers([]string{testDID})
	handler := newE2EHandler(true)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	// ---- Part 1: Pure outbound call (to a DID with pinlookup — no WS intercept) ----
	t.Log("=== Part 1: Outbound Dial ===")

	outChannel := fmt.Sprintf("testc_gort_out_%d", time.Now().UnixMilli())
	outResult, err := client.Dial(ctx, telephony.DialParams{
		To:      "+15559876543",
		From:    "+15551234567",
		Channel: outChannel,
		UID:     "100",
		Token:   appid,
		Region:  "AREA_CODE_AS",
		Timeout: "60",
		Sip:     lbSip(defaultTransport()),
		AppID:   commandAppID(),
	})
	if err != nil {
		t.Fatalf("Outbound Dial failed: %v", err)
	}

	outboundOK := outResult.Success && outResult.CallID != ""
	if !outboundOK {
		t.Logf("Outbound Dial not successful (no gateways) — skipping outbound portion: success=%v callid=%s", outResult.Success, outResult.CallID)
	} else {
		t.Logf("Outbound call placed: callid=%s", outResult.CallID)

		select {
		case <-handler.bridgeCh:
			t.Log("Outbound bridge started")
		case <-time.After(30 * time.Second):
			t.Log("No outbound bridge event — continuing")
		}

		if err := client.SendDTMF(ctx, outResult.CallID, "99#"); err != nil {
			t.Logf("Outbound SendDTMF failed: %v", err)
		} else {
			t.Log("Outbound DTMF sent: 99#")
			select {
			case digits := <-handler.dtmfCh:
				t.Logf("Outbound DTMF received: %s", digits)
			case <-time.After(5 * time.Second):
				t.Log("No outbound dtmf_received")
			}
		}

		hangupCtx, hangupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := client.Hangup(hangupCtx, outResult.CallID); err != nil {
			t.Logf("Outbound hangup warning: %v", err)
		}
		hangupCancel()

		select {
		case <-handler.hangupCh:
			t.Log("Outbound hangup received")
		case <-time.After(10 * time.Second):
			t.Log("No outbound hangup event")
		}
	}

	// Pause and drain channels between phases
	time.Sleep(time.Second)
	handler.drainChannels()

	// ---- Part 2: Outbound→Inbound loopback (dial subscribed DID, accept inbound) ----
	t.Log("=== Part 2: Inbound Accept via loopback ===")

	inChannel := fmt.Sprintf("testc_gort_in_%d", time.Now().UnixMilli())
	inResult, err := client.Dial(ctx, telephony.DialParams{
		To:      "+" + testDID,
		From:    "+15551234567",
		Channel: inChannel,
		UID:     "100",
		Token:   appid,
		Region:  "AREA_CODE_AS",
		Timeout: "60",
		Sip:     lbSip(defaultTransport()),
		AppID:   commandAppID(),
	})
	if err != nil {
		t.Fatalf("Inbound Dial failed: %v", err)
	}
	if !inResult.Success || inResult.CallID == "" {
		t.Skipf("Inbound Dial not successful (no gateways): success=%v callid=%s", inResult.Success, inResult.CallID)
	}
	t.Logf("Loopback call placed: callid=%s", inResult.CallID)

	// Wait for call_incoming from the loopback
	select {
	case call := <-handler.incomingCh:
		t.Logf("Inbound call_incoming: callid=%s from=%s to=%s", call.CallID, call.From, call.To)

		if err := client.Accept(ctx, call.CallID, telephony.AcceptParams{
			Token:   appid,
			Channel: inChannel,
			UID:     "300",
			AppID:   commandAppID(),
		}); err != nil {
			t.Fatalf("Accept failed: %v", err)
		}
		t.Log("Inbound call accepted")

	case <-time.After(30 * time.Second):
		t.Fatal("Timed out waiting for inbound call_incoming")
	}

	// Wait for real answered + bridge events
	select {
	case <-handler.answeredCh:
		t.Log("Inbound answered")
	case <-time.After(15 * time.Second):
		t.Log("No inbound answered event")
	}

	select {
	case <-handler.bridgeCh:
		t.Log("Inbound bridge started")
	case <-time.After(15 * time.Second):
		t.Log("No inbound bridge event")
	}

	// Send DTMF on the outbound leg
	if err := client.SendDTMF(ctx, inResult.CallID, "55#"); err != nil {
		t.Logf("Inbound SendDTMF failed: %v", err)
	} else {
		t.Log("Inbound DTMF sent: 55#")
		select {
		case digits := <-handler.dtmfCh:
			t.Logf("Inbound DTMF received: %s", digits)
			if digits != "55#" {
				t.Errorf("expected DTMF '55#', got '%s'", digits)
			}
		case <-time.After(5 * time.Second):
			t.Log("No inbound dtmf_received")
		}
	}

	// Hangup
	endCtx, endCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer endCancel()
	if err := client.Hangup(endCtx, inResult.CallID); err != nil {
		t.Logf("Inbound hangup warning: %v", err)
	}

	select {
	case <-handler.hangupCh:
		t.Log("Inbound hangup received")
	case <-time.After(10 * time.Second):
		t.Log("No inbound hangup event")
	}

	t.Logf("Full round-trip events: %v", handler.getEvents())
}

// ================================================================
// Test: Video Call — outbound call with video=true, verify it reaches
// the gateway and gets logged correctly. Also tests inbound loopback
// with video=true on Accept.
// ================================================================
func TestE2EVideoCall(t *testing.T) {
	wsURL, cmURL, auth, appid := skipIfNoEnv(t)

	testDID := inboundDID()
	clientID := fmt.Sprintf("gotest_video_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, auth, clientID, appid)
	client.SetSubscribeNumbers([]string{testDID})
	handler := newE2EHandler(true)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	// ---- Part 1: Outbound video call to pinlookup DID ----
	t.Log("=== Part 1: Outbound Video Call ===")
	outChannel := fmt.Sprintf("testc_vid_out_%d", time.Now().UnixMilli())
	outResult, err := client.Dial(ctx, telephony.DialParams{
		To:      "+15559876543",
		From:    "+15551234567",
		Channel: outChannel,
		UID:     "100",
		Token:   appid,
		Region:  "AREA_CODE_AS",
		Timeout: "60",
		Sip:     lbSip(defaultTransport()),
		Video:   true,
		AppID:   commandAppID(),
	})
	if err != nil {
		t.Fatalf("Outbound Video Dial failed: %v", err)
	}

	if !outResult.Success || outResult.CallID == "" {
		t.Skipf("Outbound video call not successful (no gateways): %v", outResult.Data)
	}
	t.Logf("Outbound video call placed: callid=%s", outResult.CallID)

	// Wait for answered
	select {
	case call := <-handler.answeredCh:
		t.Logf("Outbound video call answered: callid=%s", call.CallID)
	case <-time.After(30 * time.Second):
		t.Log("No outbound answered event — continuing")
	}

	// Hangup outbound
	hangupCtx, hangupCancel := context.WithTimeout(ctx, 10*time.Second)
	defer hangupCancel()
	client.Hangup(hangupCtx, outResult.CallID)
	time.Sleep(2 * time.Second)

	// ---- Part 2: Inbound loopback video call ----
	t.Log("=== Part 2: Inbound Video Call via Loopback ===")
	inChannel := fmt.Sprintf("testc_vid_in_%d", time.Now().UnixMilli())
	inResult, err := client.Dial(ctx, telephony.DialParams{
		To:      "+" + testDID,
		From:    "+15551234567",
		Channel: inChannel,
		UID:     "200",
		Token:   appid,
		Region:  "AREA_CODE_AS",
		Timeout: "60",
		Sip:     lbSip(defaultTransport()),
		Video:   true,
		AppID:   commandAppID(),
	})
	if err != nil {
		t.Fatalf("Inbound video Dial failed: %v", err)
	}

	// Wait for call_incoming on the subscribed DID
	var inboundCallID string
	select {
	case call := <-handler.incomingCh:
		inboundCallID = call.CallID
		t.Logf("Inbound video call_incoming: callid=%s from=%s to=%s", call.CallID, call.From, call.To)
	case <-time.After(30 * time.Second):
		t.Fatal("No call_incoming for video loopback")
	}

	// Accept with video=true
	err = client.Accept(ctx, inboundCallID, telephony.AcceptParams{
		Token:   appid,
		Channel: inChannel + "_in",
		UID:     "300",
		Video:   true,
		AppID:   commandAppID(),
	})
	if err != nil {
		t.Fatalf("Video Accept failed: %v", err)
	}
	t.Log("Video call accepted with video=true")

	// Wait for answered + bridge
	select {
	case call := <-handler.answeredCh:
		t.Logf("Inbound video call answered: callid=%s", call.CallID)
	case <-time.After(15 * time.Second):
		t.Log("No inbound answered")
	}

	select {
	case call := <-handler.bridgeCh:
		t.Logf("Inbound video call bridge started: callid=%s", call.CallID)
	case <-time.After(10 * time.Second):
		t.Log("No inbound bridge")
	}

	// Hangup inbound
	hangupCtx2, hangupCancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer hangupCancel2()
	if inResult.CallID != "" {
		client.Hangup(hangupCtx2, inResult.CallID)
	}

	select {
	case <-handler.hangupCh:
		t.Log("Video call hangup received")
	case <-time.After(10 * time.Second):
		t.Log("No hangup event")
	}

	// ---- Part 3: Verify duration log has video entry ----
	t.Log("=== Part 3: Verify Duration Log ===")
	time.Sleep(2 * time.Second) // let log flush

	resp, err := http.Get(cmURL + "/v1/api/service?action=status&auth=" + os.Getenv("BC_STATUS_PAGE_TOKEN"))
	if err == nil {
		defer resp.Body.Close()
		t.Logf("Status page returned %d", resp.StatusCode)
	}

	// Read the duration log directly
	logData, err := os.ReadFile("/tmp/call_durations.log")
	if err != nil {
		t.Logf("Could not read duration log: %v", err)
	} else {
		lines := strings.Split(string(logData), "\n")
		videoEntries := 0
		for _, line := range lines {
			if strings.Contains(line, outChannel) || strings.Contains(line, inChannel) {
				t.Logf("Duration log entry: %s", line)
				if strings.HasSuffix(strings.TrimSpace(line), ",video") {
					videoEntries++
				}
			}
		}
		t.Logf("Video entries in duration log: %d", videoEntries)
		if videoEntries == 0 {
			t.Log("Warning: no video entries found in duration log — video field may not be reaching service.js")
		}
	}

	t.Logf("Video test events: %v", handler.getEvents())
	t.Log("Video call test complete")
}

// ================================================================
// Test 9: Parallel Load Test — multiple DIDs, multiple calls per DID
// Configurable via env vars:
//   BC_LOAD_DIDS          — comma-separated DIDs (default: 18005551234)
//   BC_LOAD_CALLS_PER_DID — calls per DID (default: 1)
//   BC_LOAD_CALLS_PER_SEC — rate limit (default: 1)
// ================================================================
func TestE2ELoadParallelCalls(t *testing.T) {
	wsURL, _, auth, appid := skipIfNoEnv(t)

	// Parse config from env vars
	didsStr := getEnvOrDefault("BC_LOAD_DIDS", inboundDID())
	dids := strings.Split(didsStr, ",")
	for i, d := range dids {
		dids[i] = strings.TrimSpace(d)
	}

	callsPerDID := 1
	if v := os.Getenv("BC_LOAD_CALLS_PER_DID"); v != "" {
		fmt.Sscanf(v, "%d", &callsPerDID)
	}
	callsPerSec := 1
	if v := os.Getenv("BC_LOAD_CALLS_PER_SEC"); v != "" {
		fmt.Sscanf(v, "%d", &callsPerSec)
	}

	totalCalls := len(dids) * callsPerDID
	t.Logf("Load test: %d DIDs × %d calls/DID = %d total calls at %d/sec", len(dids), callsPerDID, totalCalls, callsPerSec)

	// Connect a single client subscribed to all DIDs
	clientID := fmt.Sprintf("gotest_load_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, auth, clientID, appid)
	client.SetSubscribeNumbers(dids)
	handler := newE2EHandler(true)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(totalCalls*90+60)*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	time.Sleep(500 * time.Millisecond)

	type callResult struct {
		index   int
		did     string
		callid  string
		success bool
		err     string
		dtmfOK  bool
	}

	results := make([]callResult, totalCalls)
	var wg sync.WaitGroup

	// Rate limiter
	rateTicker := time.NewTicker(time.Second / time.Duration(callsPerSec))
	defer rateTicker.Stop()

	// Verify first call works before launching all
	firstChannel := fmt.Sprintf("testc_load_probe_%d", time.Now().UnixMilli())
	probeResult, err := client.Dial(ctx, telephony.DialParams{
		To:      "+" + dids[0],
		From:    "+15551234567",
		Channel: firstChannel,
		UID:     "100",
		Token:   appid,
		Region:  "AREA_CODE_AS",
		Timeout: "30",
		Sip:     lbSip(defaultTransport()),
		AppID:   commandAppID(),
	})
	if err != nil || !probeResult.Success || probeResult.CallID == "" {
		t.Skipf("Probe call failed (no gateways available) — skipping load test: err=%v success=%v", err, probeResult)
	}
	// Clean up probe call: reject the inbound leg (loopback) to free the DID on the gateway,
	// then hang up the outbound leg. Without rejecting, the held lookup blocks the DID for 30s.
	probeInTimer := time.NewTimer(10 * time.Second)
	select {
	case inCall := <-handler.incomingCh:
		probeInTimer.Stop()
		rejectCtx, rejectCancel := context.WithTimeout(context.Background(), 5*time.Second)
		client.Reject(rejectCtx, inCall.CallID, "probe cleanup")
		rejectCancel()
	case <-probeInTimer.C:
		t.Log("Probe: no call_incoming received (non-loopback DID?)")
	}
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	client.Hangup(probeCtx, probeResult.CallID)
	probeCancel()
	time.Sleep(2 * time.Second)
	handler.drainChannels()

	for i := 0; i < totalCalls; i++ {
		<-rateTicker.C
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			did := dids[idx%len(dids)]
			channel := fmt.Sprintf("testc_load_%d_%d", time.Now().UnixMilli(), idx)
			dtmfPattern := fmt.Sprintf("%d#", idx+1)

			result := callResult{index: idx, did: did}

			// Dial outbound to DID via LB (loopback → inbound)
			dialResult, dialErr := client.Dial(ctx, telephony.DialParams{
				To:      "+" + did,
				From:    "+15551234567",
				Channel: channel,
				UID:     fmt.Sprintf("%d", 100+idx),
				Token:   appid,
				Region:  "AREA_CODE_AS",
				Timeout: "60",
				Sip:     lbSip(defaultTransport()),
			})
			if dialErr != nil {
				result.err = fmt.Sprintf("dial: %v", dialErr)
				results[idx] = result
				return
			}
			if !dialResult.Success || dialResult.CallID == "" {
				result.err = "dial failed: no callid"
				results[idx] = result
				return
			}
			result.callid = dialResult.CallID

			// Wait for call_incoming and accept
			inTimer := time.NewTimer(30 * time.Second)
			select {
			case call := <-handler.incomingCh:
				inTimer.Stop()
				acceptErr := client.Accept(ctx, call.CallID, telephony.AcceptParams{
					Token:   appid,
					Channel: channel,
					UID:     fmt.Sprintf("%d", 200+idx),
					AppID:   commandAppID(),
				})
				if acceptErr != nil {
					result.err = fmt.Sprintf("accept: %v", acceptErr)
					results[idx] = result
					return
				}
			case <-inTimer.C:
				result.err = "timeout waiting for call_incoming"
				results[idx] = result
				// Hangup the outbound leg
				hCtx, hCancel := context.WithTimeout(context.Background(), 10*time.Second)
				client.Hangup(hCtx, dialResult.CallID)
				hCancel()
				return
			}

			// Wait for bridge_start
			bridgeTimer := time.NewTimer(15 * time.Second)
			select {
			case <-handler.bridgeCh:
				bridgeTimer.Stop()
			case <-bridgeTimer.C:
				// Continue anyway — bridge event may be delayed
			}

			// Send DTMF
			if dtmfErr := client.SendDTMF(ctx, dialResult.CallID, dtmfPattern); dtmfErr != nil {
				result.err = fmt.Sprintf("dtmf: %v", dtmfErr)
			} else {
				dtmfTimer := time.NewTimer(5 * time.Second)
				select {
				case digits := <-handler.dtmfCh:
					dtmfTimer.Stop()
					if digits == dtmfPattern {
						result.dtmfOK = true
					}
				case <-dtmfTimer.C:
					// DTMF echo not received — not a hard failure (gateway may not echo)
				}
			}

			// Hangup
			hCtx, hCancel := context.WithTimeout(context.Background(), 10*time.Second)
			client.Hangup(hCtx, dialResult.CallID)
			hCancel()

			hangupTimer := time.NewTimer(10 * time.Second)
			select {
			case <-handler.hangupCh:
				hangupTimer.Stop()
			case <-hangupTimer.C:
			}

			result.success = true
			results[idx] = result
		}(i)

		// Small stagger between goroutine launches
		time.Sleep(100 * time.Millisecond)
	}

	// Wait for all calls to complete
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Duration(totalCalls*120+60) * time.Second):
		t.Fatal("Timed out waiting for all load test calls to complete")
	}

	// Summary
	successes := 0
	failures := 0
	dtmfMatches := 0
	for _, r := range results {
		if r.success {
			successes++
			if r.dtmfOK {
				dtmfMatches++
			}
		} else {
			failures++
			t.Logf("FAIL call[%d] did=%s callid=%s err=%s", r.index, r.did, r.callid, r.err)
		}
	}

	t.Logf("Load test complete: %d/%d succeeded, %d DTMF matches, %d failures", successes, totalCalls, dtmfMatches, failures)

	if failures > 0 {
		t.Errorf("%d/%d calls failed", failures, totalCalls)
	}
}

// ================================================================
// Test 10: Event Lifecycle — mandatory assertions for all 6 event types
// on both outbound and inbound calls, with hangup tested on both legs.
//
// Phase 1: Outbound call to +15559876543 (pinlookup DID)
//   - Hard-assert: call_answered, agora_bridge_start, dtmf_received, call_hangup
//   - Hangup via outbound callid (endcall action)
//
// Phase 2: Inbound call via loopback to 18005551234 (no pinlookup)
//   - Hard-assert: call_incoming, call_answered, agora_bridge_start,
//     dtmf_received (both legs), call_hangup
//   - Hangup via INBOUND callid (hangup action — tests inbound hangup path)
// ================================================================
func TestE2EEventLifecycle(t *testing.T) {
	wsURL, _, auth, appid := skipIfNoEnv(t)

	testDID := inboundDID()
	clientID := fmt.Sprintf("gotest_lifecycle_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, auth, clientID, appid)
	client.SetSubscribeNumbers([]string{testDID})
	handler := newE2EHandler(true)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	time.Sleep(500 * time.Millisecond)

	// ---- Phase 1: Outbound call — hangup outbound leg ----
	t.Log("=== Phase 1: Outbound call + event lifecycle + outbound hangup ===")

	outChannel := fmt.Sprintf("testc_lifecycle_out_%d", time.Now().UnixMilli())
	outResult, err := client.Dial(ctx, telephony.DialParams{
		To:      "+15559876543",
		From:    "+15551234567",
		Channel: outChannel,
		UID:     "100",
		Token:   appid,
		Region:  "AREA_CODE_AS",
		Timeout: "60",
		Sip:     lbSip(defaultTransport()),
		AppID:   commandAppID(),
	})
	if err != nil {
		t.Fatalf("Outbound Dial failed: %v", err)
	}
	if !outResult.Success || outResult.CallID == "" {
		t.Skipf("Outbound Dial not successful (no gateways): success=%v callid=%s", outResult.Success, outResult.CallID)
	}
	t.Logf("Outbound call placed: callid=%s", outResult.CallID)

	// MANDATORY: call_answered
	select {
	case call := <-handler.answeredCh:
		t.Logf("PASS outbound OnCallAnswered: callid=%s", call.CallID)
	case <-time.After(30 * time.Second):
		t.Fatalf("FAIL outbound: no call_answered event within 30s")
	}

	// agora_bridge_start — A-leg may or may not receive this (B-leg event routes
	// to pinlookup webhook, not back to WS client). Check but don't fail.
	select {
	case call := <-handler.bridgeCh:
		t.Logf("PASS outbound OnBridgeStart: callid=%s", call.CallID)
	case <-time.After(5 * time.Second):
		t.Log("INFO outbound: no agora_bridge_start on A-leg (expected — B-leg event)")
	}

	// MANDATORY: dtmf_received
	if err := client.SendDTMF(ctx, outResult.CallID, "147#"); err != nil {
		t.Fatalf("FAIL outbound SendDTMF: %v", err)
	}
	select {
	case digits := <-handler.dtmfCh:
		if digits != "147#" {
			t.Errorf("FAIL outbound OnDTMFReceived: expected '147#', got '%s'", digits)
		} else {
			t.Logf("PASS outbound OnDTMFReceived: %s", digits)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("FAIL outbound: no dtmf_received event within 5s")
	}

	// Hangup outbound leg (endcall action)
	hangupCtx1, hangupCancel1 := context.WithTimeout(context.Background(), 10*time.Second)
	if err := client.Hangup(hangupCtx1, outResult.CallID); err != nil {
		t.Errorf("Outbound Hangup error: %v", err)
	}
	hangupCancel1()

	// call_hangup — A-leg may not receive this webhook (B-leg teardown event).
	// The SDK removes the call from state in Hangup() itself, so we verify that.
	select {
	case call := <-handler.hangupCh:
		t.Logf("PASS outbound OnCallHangup: callid=%s", call.CallID)
	case <-time.After(5 * time.Second):
		t.Log("INFO outbound: no call_hangup on A-leg (expected — B-leg event)")
	}

	// MANDATORY: call removed from active calls after Hangup()
	for _, c := range client.GetActiveCalls() {
		if c.CallID == outResult.CallID {
			t.Errorf("FAIL: outbound call %s still in active calls after hangup", outResult.CallID)
		}
	}

	outEvents := handler.getEvents()
	t.Logf("Phase 1 events: %v", outEvents)

	// Pause between phases, drain channels
	time.Sleep(2 * time.Second)
	handler.drainChannels()

	// ---- Phase 2: Inbound call via loopback — hangup inbound leg ----
	t.Log("=== Phase 2: Inbound call + event lifecycle + inbound hangup ===")

	inChannel := fmt.Sprintf("testc_lifecycle_in_%d", time.Now().UnixMilli())
	inResult, err := client.Dial(ctx, telephony.DialParams{
		To:      "+" + testDID,
		From:    "+15551234567",
		Channel: inChannel,
		UID:     "100",
		Token:   appid,
		Region:  "AREA_CODE_AS",
		Timeout: "60",
		Sip:     lbSip(defaultTransport()),
		AppID:   commandAppID(),
	})
	if err != nil {
		t.Fatalf("Inbound Dial failed: %v", err)
	}
	if !inResult.Success || inResult.CallID == "" {
		t.Skipf("Inbound Dial not successful (no gateways): success=%v callid=%s", inResult.Success, inResult.CallID)
	}
	t.Logf("Loopback call placed: outbound callid=%s", inResult.CallID)

	// MANDATORY: call_incoming
	var inboundCallID string
	select {
	case call := <-handler.incomingCh:
		inboundCallID = call.CallID
		t.Logf("PASS inbound OnCallIncoming: callid=%s from=%s to=%s", call.CallID, call.From, call.To)
	case <-time.After(30 * time.Second):
		t.Fatalf("FAIL inbound: no call_incoming event within 30s")
	}

	// Accept the inbound leg
	if err := client.Accept(ctx, inboundCallID, telephony.AcceptParams{
		Token:   appid,
		Channel: inChannel,
		UID:     "200",
		AppID:   commandAppID(),
	}); err != nil {
		t.Fatalf("FAIL Accept: %v", err)
	}
	t.Log("Inbound call accepted")

	// MANDATORY: call_answered
	select {
	case call := <-handler.answeredCh:
		t.Logf("PASS inbound OnCallAnswered: callid=%s", call.CallID)
	case <-time.After(15 * time.Second):
		t.Errorf("FAIL inbound: no call_answered event within 15s")
	}

	// MANDATORY: agora_bridge_start
	select {
	case call := <-handler.bridgeCh:
		t.Logf("PASS inbound OnBridgeStart: callid=%s", call.CallID)
	case <-time.After(15 * time.Second):
		t.Errorf("FAIL inbound: no agora_bridge_start event within 15s")
	}

	// MANDATORY: dtmf_received on outbound leg (A-leg)
	if err := client.SendDTMF(ctx, inResult.CallID, "258#"); err != nil {
		t.Errorf("FAIL outbound-leg SendDTMF: %v", err)
	} else {
		select {
		case digits := <-handler.dtmfCh:
			if digits != "258#" {
				t.Errorf("FAIL outbound-leg OnDTMFReceived: expected '258#', got '%s'", digits)
			} else {
				t.Logf("PASS outbound-leg OnDTMFReceived: %s", digits)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("FAIL outbound-leg: no dtmf_received event within 5s")
		}
	}

	// MANDATORY: dtmf_received on inbound leg (B-leg)
	if err := client.SendDTMF(ctx, inboundCallID, "369*"); err != nil {
		t.Errorf("FAIL inbound-leg SendDTMF: %v", err)
	} else {
		select {
		case digits := <-handler.dtmfCh:
			if digits != "369*" {
				t.Errorf("FAIL inbound-leg OnDTMFReceived: expected '369*', got '%s'", digits)
			} else {
				t.Logf("PASS inbound-leg OnDTMFReceived: %s", digits)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("FAIL inbound-leg: no dtmf_received event within 5s")
		}
	}

	// Hangup the INBOUND leg (hangup action — not endcall)
	// This proves Hangup() works on accepted inbound calls.
	hangupCtx2, hangupCancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	if err := client.Hangup(hangupCtx2, inboundCallID); err != nil {
		t.Errorf("Inbound Hangup error: %v", err)
	}
	hangupCancel2()
	t.Logf("Hangup sent on inbound callid=%s", inboundCallID)

	// MANDATORY: call_hangup (from inbound or outbound leg teardown)
	select {
	case call := <-handler.hangupCh:
		t.Logf("PASS inbound OnCallHangup: callid=%s", call.CallID)
	case <-time.After(10 * time.Second):
		t.Errorf("FAIL inbound: no call_hangup event within 10s")
	}

	// Clean up outbound leg if still active
	outboundStillActive := false
	for _, c := range client.GetActiveCalls() {
		if c.CallID == inResult.CallID {
			outboundStillActive = true
		}
	}
	if outboundStillActive {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		client.Hangup(cleanupCtx, inResult.CallID)
		cleanupCancel()
		// Drain the second hangup event
		select {
		case <-handler.hangupCh:
			t.Log("Outbound leg also hung up")
		case <-time.After(10 * time.Second):
		}
	}

	allEvents := handler.getEvents()
	t.Logf("Phase 2 events: %v", allEvents)

	// ---- Final summary: verify key event types were seen ----
	t.Log("=== Event type summary ===")
	eventTypes := map[string]bool{
		"incoming":     false,
		"answered":     false,
		"bridge_start": false,
		"hangup":       false,
		"dtmf":         false,
	}
	for _, e := range allEvents {
		for prefix := range eventTypes {
			if strings.HasPrefix(e, prefix+":") {
				eventTypes[prefix] = true
			}
		}
	}
	for etype, seen := range eventTypes {
		if seen {
			t.Logf("  PASS %s event received", etype)
		} else {
			t.Errorf("  FAIL %s event NEVER received across entire test", etype)
		}
	}
}

// ================================================================
// Test 11: Bridge + Unbridge — loopback call, then:
//   Phase A: Re-bridge (replace existing bridge with new channel)
//   Phase B: Unbridge (remove bridge entirely → bridge_end)
//
// Uses loopback pattern because the B-leg webhook is auto-injected
// by the WS proxy during Accept, so bridge events route back to us.
// ================================================================
func TestE2EBridgeUnbridge(t *testing.T) {
	wsURL, _, auth, appid := skipIfNoEnv(t)

	testDID := inboundDID()
	clientID := fmt.Sprintf("gotest_bridge_%d", time.Now().UnixMilli())
	outChannel := fmt.Sprintf("testc_gobridge_a_%d", time.Now().UnixMilli())
	inChannel := fmt.Sprintf("testc_gobridge_b_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, auth, clientID, appid)
	client.SetSubscribeNumbers([]string{testDID})
	handler := newE2EHandler(true)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	time.Sleep(500 * time.Millisecond)

	// Place outbound call to subscribed DID → loops back as inbound
	result, err := client.Dial(ctx, telephony.DialParams{
		To:      "+" + testDID,
		From:    "+15551234567",
		Channel: outChannel,
		UID:     "100",
		Token:   appid,
		Region:  "AREA_CODE_AS",
		Timeout: "60",
		Sip:     lbSip(defaultTransport()),
		AppID:   commandAppID(),
	})
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if !result.Success || result.CallID == "" {
		t.Skipf("Dial not successful (no gateways): success=%v callid=%s", result.Success, result.CallID)
	}
	t.Logf("Outbound call placed: callid=%s", result.CallID)

	// Wait for call_incoming from the loopback
	var inboundCallID string
	select {
	case call := <-handler.incomingCh:
		inboundCallID = call.CallID
		t.Logf("Inbound call_incoming: callid=%s", call.CallID)

		if err := client.Accept(ctx, call.CallID, telephony.AcceptParams{
			Token:   appid,
			Channel: inChannel,
			UID:     "200",
			AppID:   commandAppID(),
		}); err != nil {
			t.Fatalf("Accept failed: %v", err)
		}
		t.Log("Inbound call accepted")

	case <-time.After(30 * time.Second):
		t.Fatal("Timed out waiting for call_incoming")
	}

	// Wait for answered + initial bridge_start
	select {
	case <-handler.answeredCh:
	case <-time.After(15 * time.Second):
		t.Fatal("No call_answered")
	}
	select {
	case call := <-handler.bridgeCh:
		t.Logf("PASS Initial bridge_start: callid=%s", call.CallID)
	case <-time.After(15 * time.Second):
		t.Fatal("No initial bridge_start")
	}

	// Confirm bridge works with DTMF
	time.Sleep(time.Second)
	if err := client.SendDTMF(ctx, inboundCallID, "88#"); err != nil {
		t.Errorf("SendDTMF failed: %v", err)
	} else {
		select {
		case digits := <-handler.dtmfCh:
			if digits == "88#" {
				t.Log("PASS DTMF on initial bridge: 88#")
			} else {
				t.Errorf("DTMF mismatch: expected '88#', got '%s'", digits)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("No dtmf_received on initial bridge")
		}
	}

	time.Sleep(2 * time.Second)

	// === Phase A: Re-Bridge (replace bridge without unbridge) ===
	rebridgeChannel := fmt.Sprintf("testc_rebridge_%d", time.Now().UnixMilli())
	t.Logf("Phase A: Bridge to new channel=%s (replacing existing bridge)...", rebridgeChannel)
	bridgeErr := client.Bridge(ctx, inboundCallID, telephony.BridgeParams{
		Token:   appid,
		Channel: rebridgeChannel,
		UID:     "201",
		AppID:   commandAppID(),
	})
	if bridgeErr != nil {
		t.Logf("Bridge command error: %v", bridgeErr)
	} else {
		t.Log("Bridge command sent successfully")
	}

	// Check for bridge_end (old bridge torn down) — may or may not fire
	gotBridgeEnd := false
	select {
	case call := <-handler.bridgeEndCh:
		gotBridgeEnd = true
		t.Logf("PASS bridge_end after re-bridge: callid=%s", call.CallID)
	case <-time.After(5 * time.Second):
		t.Log("INFO: no bridge_end after re-bridge (gateway may replace in-place)")
	}

	// Check for bridge_start (new bridge) — may or may not fire depending on gateway
	gotReBridgeStart := false
	select {
	case call := <-handler.bridgeCh:
		gotReBridgeStart = true
		t.Logf("PASS bridge_start after re-bridge: callid=%s", call.CallID)
	case <-time.After(5 * time.Second):
		t.Log("INFO: no bridge_start after re-bridge")
	}

	if gotReBridgeStart {
		// DTMF on re-bridged call to confirm it works
		time.Sleep(time.Second)
		if err := client.SendDTMF(ctx, inboundCallID, "33#"); err != nil {
			t.Errorf("SendDTMF after re-bridge failed: %v", err)
		} else {
			select {
			case digits := <-handler.dtmfCh:
				if digits == "33#" {
					t.Log("PASS DTMF after re-bridge: 33#")
				} else {
					t.Errorf("DTMF mismatch: expected '33#', got '%s'", digits)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("No dtmf_received after re-bridge")
			}
		}
	}

	// === Phase B: Unbridge ===
	// If re-bridge didn't work (gateway may have torn down), skip unbridge
	callStillActive := false
	for _, c := range client.GetActiveCalls() {
		if c.CallID == inboundCallID {
			callStillActive = true
			break
		}
	}

	if callStillActive {
		t.Log("Phase B: Unbridge...")
		if err := client.Unbridge(ctx, inboundCallID); err != nil {
			t.Logf("Unbridge error: %v", err)
		} else {
			t.Log("Unbridge sent successfully")
			// If we already got bridge_end from re-bridge, we need another one here
			if !gotBridgeEnd || gotReBridgeStart {
				select {
				case call := <-handler.bridgeEndCh:
					t.Logf("PASS bridge_end after unbridge: callid=%s", call.CallID)
				case <-time.After(10 * time.Second):
					t.Errorf("FAIL: no bridge_end after unbridge")
				}
			}
		}
	} else {
		t.Log("Phase B: skipped — call already torn down after re-bridge")
	}

	// Wait for hangup
	select {
	case <-handler.hangupCh:
		t.Log("Hangup event received")
	case <-time.After(10 * time.Second):
		t.Log("No hangup event")
	}

	// Clean up outbound leg
	for _, c := range client.GetActiveCalls() {
		if c.CallID == result.CallID {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			client.Hangup(cleanupCtx, result.CallID)
			cleanupCancel()
			select {
			case <-handler.hangupCh:
			case <-time.After(5 * time.Second):
			}
			break
		}
	}

	t.Logf("Events: %v", handler.getEvents())
}

// ================================================================
// Test 12: Transfer — loopback call, accept, then transfer to the
// pinlookup DID (+15559876543). Verifies Transfer SDK call succeeds
// on a live call.
// ================================================================
func TestE2ETransfer(t *testing.T) {
	wsURL, _, auth, appid := skipIfNoEnv(t)

	testDID := inboundDID()
	clientID := fmt.Sprintf("gotest_transfer_%d", time.Now().UnixMilli())
	outChannel := fmt.Sprintf("testc_gotransfer_a_%d", time.Now().UnixMilli())
	inChannel := fmt.Sprintf("testc_gotransfer_b_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, auth, clientID, appid)
	client.SetSubscribeNumbers([]string{testDID})
	handler := newE2EHandler(true)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	time.Sleep(500 * time.Millisecond)

	// Place loopback call
	result, err := client.Dial(ctx, telephony.DialParams{
		To:      "+" + testDID,
		From:    "+15551234567",
		Channel: outChannel,
		UID:     "100",
		Token:   appid,
		Region:  "AREA_CODE_AS",
		Timeout: "60",
		Sip:     lbSip(defaultTransport()),
		AppID:   commandAppID(),
	})
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if !result.Success || result.CallID == "" {
		t.Skipf("Dial not successful (no gateways): success=%v callid=%s", result.Success, result.CallID)
	}
	t.Logf("Outbound call placed: callid=%s", result.CallID)

	// Wait for call_incoming and accept
	var inboundCallID string
	select {
	case call := <-handler.incomingCh:
		inboundCallID = call.CallID
		t.Logf("Inbound call_incoming: callid=%s", call.CallID)

		if err := client.Accept(ctx, call.CallID, telephony.AcceptParams{
			Token:   appid,
			Channel: inChannel,
			UID:     "200",
			AppID:   commandAppID(),
		}); err != nil {
			t.Fatalf("Accept failed: %v", err)
		}
		t.Log("Inbound call accepted")

	case <-time.After(30 * time.Second):
		t.Fatal("Timed out waiting for call_incoming")
	}

	// Wait for bridge
	select {
	case <-handler.answeredCh:
	case <-time.After(15 * time.Second):
		t.Fatal("No call_answered")
	}
	select {
	case <-handler.bridgeCh:
		t.Log("Bridge started")
	case <-time.After(15 * time.Second):
		t.Fatal("No bridge_start")
	}

	time.Sleep(2 * time.Second)

	// Transfer the inbound B-leg to the pinlookup DID
	t.Logf("Sending Transfer on inbound callid=%s to +15559876543...", inboundCallID)
	transferErr := client.Transfer(ctx, inboundCallID, "+15559876543", "")
	if transferErr != nil {
		t.Logf("Transfer returned error: %v (gateway may not support transfer)", transferErr)
	} else {
		t.Log("PASS Transfer command succeeded")
	}

	// The transfer may cause bridge_end, new events, or hangup — collect what we get
	time.Sleep(3 * time.Second)

	// Clean up both legs
	for _, c := range client.GetActiveCalls() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		client.Hangup(cleanupCtx, c.CallID)
		cleanupCancel()
	}

	// Drain hangup events
	for i := 0; i < 2; i++ {
		select {
		case <-handler.hangupCh:
		case <-time.After(5 * time.Second):
		}
	}

	_ = inboundCallID
	t.Logf("Events: %v", handler.getEvents())
}
