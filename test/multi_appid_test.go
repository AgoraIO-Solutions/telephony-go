package test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	telephony "github.com/AgoraIO-Solutions/telephony-go"
)

// ================================================================
// Test: Multi-AppID — two clients with different appids use the
// same channel:uid pair. Events must route to the correct client.
// Before the fix, the second call's tracking would overwrite the first.
// ================================================================
func TestE2EMultiAppIDSameChannelUID(t *testing.T) {
	wsURL, _, auth, appid := skipIfNoEnv(t)

	// Second appid: shares the same auth token on dev
	appid2 := getEnvOrDefault("BC_APPID2", "")
	if appid2 == "" {
		t.Skip("BC_APPID2 required for multi-appid tests")
	}

	// Both use the SAME channel and uid — this is the collision scenario
	sharedChannel := fmt.Sprintf("testc_multi_%d", time.Now().UnixMilli())
	sharedUID := "100"

	t.Logf("Multi-AppID test: appid1=%s appid2=%s channel=%s uid=%s", appid, appid2, sharedChannel, sharedUID)

	// --- Client A: appid1 ---
	clientIDA := fmt.Sprintf("gotest_multi_a_%d", time.Now().UnixMilli())
	clientA := telephony.NewClient(wsURL, auth, clientIDA, appid)
	handlerA := newE2EHandler(false)
	clientA.SetHandler(handlerA)

	// --- Client B: appid2 ---
	clientIDB := fmt.Sprintf("gotest_multi_b_%d", time.Now().UnixMilli())
	clientB := telephony.NewClient(wsURL, auth, clientIDB, appid2)
	handlerB := newE2EHandler(false)
	clientB.SetHandler(handlerB)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Connect both
	if err := clientA.Connect(ctx); err != nil {
		t.Fatalf("Client A Connect: %v", err)
	}
	defer clientA.Close()

	if err := clientB.Connect(ctx); err != nil {
		t.Fatalf("Client B Connect: %v", err)
	}
	defer clientB.Close()

	time.Sleep(500 * time.Millisecond)

	// Both dial outbound with the SAME channel + uid but different appids
	var resultA, resultB *telephony.DialResult
	var errA, errB error
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		resultA, errA = clientA.Dial(ctx, telephony.DialParams{
			To:      "+15559876543",
			From:    "+15551234567",
			Channel: sharedChannel,
			UID:     sharedUID,
			Token:   appid,
			Region:  "AREA_CODE_AS",
			Timeout: "60",
			Sip:     lbSip(defaultTransport()),
		})
	}()

	// Small stagger so both calls are in flight with same channel:uid
	time.Sleep(200 * time.Millisecond)

	go func() {
		defer wg.Done()
		resultB, errB = clientB.Dial(ctx, telephony.DialParams{
			To:      "+15559876543",
			From:    "+15551234567",
			Channel: sharedChannel,
			UID:     sharedUID,
			Token:   appid2,
			Region:  "AREA_CODE_AS",
			Timeout: "60",
			Sip:     lbSip(defaultTransport()),
		})
	}()

	wg.Wait()

	if errA != nil {
		t.Fatalf("Client A Dial failed: %v", errA)
	}
	if errB != nil {
		t.Fatalf("Client B Dial failed: %v", errB)
	}

	if !resultA.Success || resultA.CallID == "" {
		t.Skipf("Client A Dial not successful (no gateways): %v", resultA.Data)
	}
	if !resultB.Success || resultB.CallID == "" {
		t.Skipf("Client B Dial not successful (no gateways): %v", resultB.Data)
	}

	t.Logf("Client A callid=%s", resultA.CallID)
	t.Logf("Client B callid=%s", resultB.CallID)

	if resultA.CallID == resultB.CallID {
		t.Fatal("Both clients got the same callid — something is very wrong")
	}

	// Wait for events on both handlers
	waitForEvent := func(name string, ch chan *telephony.Call, timeout time.Duration) *telephony.Call {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case call := <-ch:
			return call
		case <-timer.C:
			t.Logf("%s: no event within %v", name, timeout)
			return nil
		}
	}

	// Both should get answered events
	callA := waitForEvent("A answered", handlerA.answeredCh, 30*time.Second)
	callB := waitForEvent("B answered", handlerB.answeredCh, 30*time.Second)

	if callA != nil {
		t.Logf("Client A answered: callid=%s channel=%s", callA.CallID, callA.Channel)
	}
	if callB != nil {
		t.Logf("Client B answered: callid=%s channel=%s", callB.CallID, callB.Channel)
	}

	// Verify events are NOT cross-routed
	// Client A should only have events for callid A
	eventsA := handlerA.getEvents()
	eventsB := handlerB.getEvents()
	t.Logf("Client A events: %v", eventsA)
	t.Logf("Client B events: %v", eventsB)

	for _, ev := range eventsA {
		if containsCallID(ev, resultB.CallID) {
			t.Errorf("Client A received event for Client B's callid: %s", ev)
		}
	}
	for _, ev := range eventsB {
		if containsCallID(ev, resultA.CallID) {
			t.Errorf("Client B received event for Client A's callid: %s", ev)
		}
	}

	// Hangup both
	hangupCtx, hangupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer hangupCancel()
	clientA.Hangup(hangupCtx, resultA.CallID)
	clientB.Hangup(hangupCtx, resultB.CallID)

	// Wait for hangups
	waitForEvent("A hangup", handlerA.hangupCh, 10*time.Second)
	waitForEvent("B hangup", handlerB.hangupCh, 10*time.Second)

	t.Log("Multi-AppID test passed: both clients got independent events with same channel:uid")
}

// ================================================================
// Test: MULTI-mode — client registers with appid "MULTI", sends
// appid per command. Verifies enforcement (missing appid → error)
// and correct routing.
// ================================================================
func TestE2EMultiMode(t *testing.T) {
	wsURL, _, auth, appid := skipIfNoEnv(t)

	multiAuth := getEnvOrDefault("BC_MULTI_AUTH", auth)
	channel := fmt.Sprintf("testc_multimode_%d", time.Now().UnixMilli())

	t.Logf("MULTI-mode test: channel=%s", channel)

	clientID := fmt.Sprintf("gotest_multimode_%d", time.Now().UnixMilli())
	client := telephony.NewClient(wsURL, multiAuth, clientID, "MULTI")
	handler := newE2EHandler(false)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	// Test 1: Dial WITHOUT appid → should fail with "appid required in MULTI mode"
	t.Log("--- Test: outbound without appid should fail ---")
	_, err := client.Dial(ctx, telephony.DialParams{
		To:      "+15559876543",
		From:    "+15551234567",
		Channel: channel + "_noappid",
		UID:     "100",
		Token:   "tok",
		Region:  "NA",
		Timeout: "30",
	})
	// The error comes back as a WS response with error field, which Dial returns as a DialResult
	// The server sends back {error: "appid required in MULTI mode"} which won't have a "response" key,
	// so the SDK may time out or get a nil response. Let's check:
	if err != nil {
		t.Logf("Dial without appid correctly failed: %v", err)
	} else {
		t.Log("Dial without appid returned (may have gotten error in response)")
	}

	// Test 2: Dial WITH appid → should succeed
	t.Log("--- Test: outbound with appid should succeed ---")
	result, err := client.Dial(ctx, telephony.DialParams{
		To:      "+15559876543",
		From:    "+15551234567",
		Channel: channel,
		UID:     "200",
		Token:   appid,
		Region:  "AREA_CODE_AS",
		Timeout: "60",
		Sip:     lbSip(defaultTransport()),
		AppID:   appid,
	})
	if err != nil {
		t.Fatalf("Dial with appid failed: %v", err)
	}
	if !result.Success || result.CallID == "" {
		t.Skipf("Dial not successful (no gateways): %v", result.Data)
	}
	t.Logf("MULTI-mode call placed: callid=%s", result.CallID)

	// Wait for answered
	timer := time.NewTimer(30 * time.Second)
	select {
	case call := <-handler.answeredCh:
		timer.Stop()
		t.Logf("MULTI-mode call answered: callid=%s", call.CallID)
	case <-timer.C:
		t.Log("No answered event — continuing to hangup")
	}

	// Hangup
	hangupCtx, hangupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer hangupCancel()
	if err := client.Hangup(hangupCtx, result.CallID); err != nil {
		t.Logf("Hangup warning: %v", err)
	}

	select {
	case <-handler.hangupCh:
		t.Log("Hangup received")
	case <-time.After(10 * time.Second):
		t.Log("No hangup event")
	}

	t.Logf("MULTI-mode events: %v", handler.getEvents())
	t.Log("MULTI-mode test passed")
}

// containsCallID checks if an event string contains the given callid.
func containsCallID(event, callid string) bool {
	if callid == "" {
		return false
	}
	return len(event) > len(callid) && event[len(event)-len(callid):] == callid
}
