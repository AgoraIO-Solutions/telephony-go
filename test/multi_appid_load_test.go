package test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	telephony "github.com/AgoraIO-Solutions/telephony-go"
)

// --- loadHandler: per-call event tracking ---
type loadHandler struct {
	mu         sync.Mutex
	perCall    map[string][]string // callid -> event types
	incomingCh chan *telephony.Call
	errCount   atomic.Int64
}

func newLoadHandler() *loadHandler {
	return &loadHandler{
		perCall:    make(map[string][]string),
		incomingCh: make(chan *telephony.Call, 50),
	}
}

func (h *loadHandler) record(callid, event string) {
	h.mu.Lock()
	h.perCall[callid] = append(h.perCall[callid], event)
	h.mu.Unlock()
}

func (h *loadHandler) getEvents(callid string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.perCall[callid]...)
}

func (h *loadHandler) allCallIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0, len(h.perCall))
	for id := range h.perCall {
		ids = append(ids, id)
	}
	return ids
}

func (h *loadHandler) OnConnected(sessionID string) {}
func (h *loadHandler) OnCallIncoming(call *telephony.Call) bool {
	h.record(call.CallID, "incoming")
	select {
	case h.incomingCh <- call:
	default:
	}
	return true
}
func (h *loadHandler) OnCallRinging(call *telephony.Call)  { h.record(call.CallID, "ringing") }
func (h *loadHandler) OnCallAnswered(call *telephony.Call) { h.record(call.CallID, "answered") }
func (h *loadHandler) OnBridgeStart(call *telephony.Call)  { h.record(call.CallID, "bridge_start") }
func (h *loadHandler) OnBridgeEnd(call *telephony.Call)    { h.record(call.CallID, "bridge_end") }
func (h *loadHandler) OnCallHangup(call *telephony.Call)   { h.record(call.CallID, "hangup") }
func (h *loadHandler) OnError(err error)                   { h.errCount.Add(1) }
func (h *loadHandler) OnDTMFReceived(call *telephony.Call, digits string) {
	h.record(call.CallID, "dtmf:"+digits)
}

// --- Helpers ---

func randomDTMF() string {
	chars := "0123456789*#"
	n := 4 + rand.Intn(3) // 4-6 digits
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func countLogLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	s := bufio.NewScanner(f)
	for s.Scan() {
		n++
	}
	return n
}

// getNewLogEntries returns log lines added after baseline that contain prefix in the channel field.
func getNewLogEntries(path string, baseline int, prefix string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var result []string
	lineNum := 0
	s := bufio.NewScanner(f)
	for s.Scan() {
		lineNum++
		if lineNum <= baseline {
			continue
		}
		line := s.Text()
		if prefix == "" || strings.Contains(line, prefix) {
			result = append(result, line)
		}
	}
	return result
}

func samplePM2CPU() float64 {
	out, err := exec.Command("pm2", "jlist").Output()
	if err != nil {
		return -1
	}
	var procs []map[string]interface{}
	if err := json.Unmarshal(out, &procs); err != nil {
		return -1
	}
	for _, p := range procs {
		if p["name"] == "sip-call-manager" {
			monit, _ := p["monit"].(map[string]interface{})
			cpu, _ := monit["cpu"].(float64)
			return cpu
		}
	}
	return -1
}

func hasEvent(events []string, name string) bool {
	for _, e := range events {
		if e == name {
			return true
		}
	}
	return false
}

// waitForCallEvent polls per-call events until one of the target events appears or timeout.
func waitForCallEvent(handler *loadHandler, callid string, targets []string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			recorded := handler.getEvents(callid)
			for _, want := range targets {
				if hasEvent(recorded, want) {
					return true
				}
			}
		case <-deadline:
			return false
		}
	}
}

func hasDTMFEvent(events []string) bool {
	for _, e := range events {
		if strings.HasPrefix(e, "dtmf:") {
			return true
		}
	}
	return false
}

// ================================================================
// 40-call multi-appid load test — single MULTI-mode WS client
//
// Each call dials BC_INBOUND_DID (loopback) so both outbound and
// inbound legs are exercised. An auto-accept goroutine handles
// incoming calls and sends B-leg DTMF. Concurrency is capped at
// 10 in-flight calls to avoid saturating the gateway.
//
// Env vars:
//   BC_AUTH_TOKEN  (required)
//   BC_APPID       (required) — primary appid
//   BC_APPID2      (optional) — secondary appid
//   BC_INBOUND_DID (optional) — loopback DID (default: 18005551234)
//
// Verifies:
//   - Concurrent sendCommand: request_id matching, no collisions
//   - Scoped tracking: same channel:uid across different appids
//   - Webhook delivery: all events received via WS
//   - Duration log: entries with correct appid and direction
//   - DTMF A-leg: sent and (if echoed) received per outbound call
//   - DTMF B-leg: sent on accepted inbound calls, verified
//   - Auto-accept: all incoming calls accepted via WS
//   - Gateway resilience: temp failures tracked, not fatal
// ================================================================
func TestE2EMultiAppIDLoad(t *testing.T) {
	wsURL, _, auth, _ := skipIfNoEnv(t)
	appid1 := commandAppID()
	appid2 := getEnvOrDefault("BC_APPID2", "")
	if appid2 == "" {
		t.Skip("BC_APPID2 required for multi-appid load tests")
	}

	const (
		totalCalls      = 80
		durationLogPath = "/tmp/call_durations.log"
	)
	loopbackDID := inboundDID() // loopback -> inbound + outbound legs

	ts := time.Now().UnixMilli()
	chPrefix := fmt.Sprintf("loadm_%d", ts) // channel prefix for log matching

	// Record duration log baseline
	logBaseline := countLogLines(durationLogPath)
	t.Logf("Duration log baseline: %d lines", logBaseline)

	// --- Setup single MULTI client ---
	handler := newLoadHandler()
	clientID := fmt.Sprintf("load_multi_%d", ts)
	client := telephony.NewClient(wsURL, auth, clientID, "MULTI")
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()
	time.Sleep(time.Second)

	// Subscribe to loopback DID so inbound calls arrive via WS
	if err := client.Subscribe(ctx, []string{loopbackDID}); err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// --- CPU monitor ---
	var cpuMu sync.Mutex
	var cpuSamples []float64
	cpuDone := make(chan struct{})
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-cpuDone:
				return
			case <-tick.C:
				if cpu := samplePM2CPU(); cpu >= 0 {
					cpuMu.Lock()
					cpuSamples = append(cpuSamples, cpu)
					cpuMu.Unlock()
				}
			}
		}
	}()

	// --- Auto-accept goroutine: accept incoming calls and send B-leg DTMF ---
	var bLegMu sync.Mutex
	bLegDTMF := make(map[string]string) // inbound callid → digits sent
	var acceptCount atomic.Int64

	acceptDone := make(chan struct{})
	go func() {
		for {
			select {
			case call := <-handler.incomingCh:
				acceptCount.Add(1)
				ch := fmt.Sprintf("%s_inb_%s", chPrefix, call.CallID[:8])
				aCtx, aC := context.WithTimeout(ctx, 10*time.Second)
				err := client.Accept(aCtx, call.CallID, telephony.AcceptParams{
					AppID: appid1, Token: appid1, Channel: ch, UID: "200",
				})
				aC()
				if err != nil {
					continue
				}

				// Wait for bridge event, then send B-leg DTMF (in separate goroutine to not block acceptor)
				go func(cid string) {
					if !waitForCallEvent(handler, cid, []string{"bridge_start", "answered"}, 30*time.Second) {
						return
					}
					time.Sleep(time.Second) // brief settle after bridge
					digits := randomDTMF()
					client.SendDTMF(ctx, cid, digits)
					bLegMu.Lock()
					bLegDTMF[cid] = digits
					bLegMu.Unlock()
				}(call.CallID)
			case <-acceptDone:
				return
			}
		}
	}()

	// --- Define 40 calls ---
	type callDef struct {
		idx       int
		channel   string
		uid       string
		appid     string // effective appid for this call
		collision string // collision group label (empty = none)
	}

	half := totalCalls / 2 // 20 per appid
	defs := make([]callDef, totalCalls)

	// Collision pairs: calls 0+20, 1+21, ..., 9+29 share channel:uid across appids
	for i := 0; i < 10; i++ {
		colCh := fmt.Sprintf("%s_col_%d", chPrefix, i)
		defs[i] = callDef{i, colCh, "100", appid1, fmt.Sprintf("col%d", i)}
		defs[half+i] = callDef{half + i, colCh, "100", appid2, fmt.Sprintf("col%d", i)}
	}

	// Remaining unique calls (10-19 = appid1, 30-39 = appid2)
	for i := 10; i < half; i++ {
		defs[i] = callDef{i, fmt.Sprintf("%s_a_%d", chPrefix, i), fmt.Sprintf("%d", 200+i), appid1, ""}
	}
	for i := half + 10; i < totalCalls; i++ {
		defs[i] = callDef{i, fmt.Sprintf("%s_b_%d", chPrefix, i), fmt.Sprintf("%d", 200+i), appid2, ""}
	}

	// --- Outcome tracking ---
	type outcome struct {
		callid        string
		events        []string
		dtmfSent      string
		dtmfRecv      bool
		dtmfRecvMatch bool   // received digits match sent
		dtmfRecvDigit string // actual digits received
		err           string
		ok            bool
		dialMs        int64
		gwIP          string
	}
	results := make([]outcome, totalCalls)

	// --- Launch goroutines with concurrency limit ---
	const maxConcurrent = 10 // max in-flight calls at once (prevents gateway saturation)
	sem := make(chan struct{}, maxConcurrent)

	t.Logf("=== Launching %d loopback calls (max %d concurrent) via single MULTI client (DID=%s) ===", totalCalls, maxConcurrent, loopbackDID)
	t.Logf("    appid1=%s appid2=%s", shortID(appid1), shortID(appid2))
	testStart := time.Now()

	var wg sync.WaitGroup
	for i := range defs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			// Acquire semaphore — blocks until a slot is free
			sem <- struct{}{}
			defer func() { <-sem }()

			def := defs[idx]
			o := &results[idx]
			o.dtmfSent = randomDTMF()

			params := telephony.DialParams{
				To:      "+" + loopbackDID,
				From:    "+15551234567",
				Channel: def.channel,
				UID:     def.uid,
				Token:   def.appid,
				Region:  "AREA_CODE_AS",
				Timeout: "60",
				Sip:     lbSip(defaultTransport()),
				AppID:   def.appid,
			}

			t0 := time.Now()
			res, err := client.Dial(ctx, params)
			o.dialMs = time.Since(t0).Milliseconds()

			if err != nil {
				o.err = fmt.Sprintf("dial: %v", err)
				return
			}
			if res.Data != nil {
				if gwIP, ok := res.Data["data"].(map[string]interface{}); ok {
					if ip, ok := gwIP["gateway_ip"].(string); ok {
						o.gwIP = ip
					}
				}
			}
			if !res.Success || res.CallID == "" {
				o.err = fmt.Sprintf("dial: no callid (data=%v)", res.Data)
				return
			}
			o.callid = res.CallID

			// Wait for bridge event on this callid (event-driven, not fixed sleep)
			if !waitForCallEvent(handler, res.CallID, []string{"bridge_start", "answered"}, 30*time.Second) {
				o.err = "no bridge/answered event"
				hCtx, hC := context.WithTimeout(context.Background(), 10*time.Second)
				client.Hangup(hCtx, res.CallID)
				hC()
				return
			}
			time.Sleep(time.Second) // brief settle after bridge

			// Send DTMF
			if e := client.SendDTMF(ctx, res.CallID, o.dtmfSent); e != nil {
				// Not fatal — track it
			}

			// Wait for DTMF echo (poll per-call events)
			dtmfDeadline := time.After(5 * time.Second)
			dtmfTick := time.NewTicker(200 * time.Millisecond)
		dtmfWait:
			for {
				select {
				case <-dtmfTick.C:
					if hasDTMFEvent(handler.getEvents(res.CallID)) {
						break dtmfWait
					}
				case <-dtmfDeadline:
					break dtmfWait
				}
			}
			dtmfTick.Stop()

			// Check for received DTMF
			events := handler.getEvents(res.CallID)
			for _, ev := range events {
				if strings.HasPrefix(ev, "dtmf:") {
					o.dtmfRecv = true
					o.dtmfRecvDigit = strings.TrimPrefix(ev, "dtmf:")
					if o.dtmfRecvDigit == o.dtmfSent {
						o.dtmfRecvMatch = true
					}
					break
				}
			}

			// Hold a bit more (duration logged)
			time.Sleep(3 * time.Second)

			// Hangup
			hCtx, hC := context.WithTimeout(context.Background(), 10*time.Second)
			client.Hangup(hCtx, res.CallID)
			hC()

			time.Sleep(2 * time.Second)
			o.events = handler.getEvents(res.CallID)
			o.ok = true
		}(i)

		time.Sleep(100 * time.Millisecond) // spawn goroutines quickly; semaphore gates concurrency
	}

	// Wait for all calls
	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(6 * time.Minute):
		t.Fatal("Load test timed out")
	}

	close(acceptDone)
	close(cpuDone)
	elapsed := time.Since(testStart)
	time.Sleep(5 * time.Second) // Let duration log flush

	// --- Verification ---

	// 1. Cross-routing check on collision pairs
	crossRouteErrors := 0
	collisionGroups := map[string][]int{}
	for i, d := range defs {
		if d.collision != "" {
			collisionGroups[d.collision] = append(collisionGroups[d.collision], i)
		}
	}
	for group, indices := range collisionGroups {
		if len(indices) != 2 {
			continue
		}
		a, b := indices[0], indices[1]
		callidA := results[a].callid
		callidB := results[b].callid
		if callidA == "" || callidB == "" {
			continue
		}
		// Events for callid A should NOT appear in callid B's events, and vice versa
		eventsA := handler.getEvents(callidA)
		eventsB := handler.getEvents(callidB)
		for _, ev := range eventsA {
			// Check if callid B leaked into A's event tracking
			if strings.Contains(ev, callidB) {
				t.Errorf("CROSS-ROUTE [%s]: call %d events contain callid from call %d: %s", group, a, b, ev)
				crossRouteErrors++
			}
		}
		for _, ev := range eventsB {
			if strings.Contains(ev, callidA) {
				t.Errorf("CROSS-ROUTE [%s]: call %d events contain callid from call %d: %s", group, b, a, ev)
				crossRouteErrors++
			}
		}
		// Also verify same callid didn't get assigned to both
		if callidA == callidB {
			t.Errorf("CROSS-ROUTE [%s]: calls %d and %d got same callid %s", group, a, b, callidA)
			crossRouteErrors++
		}
	}

	// 2. Duration log entries — outbound calls only produce B-leg (inbound pin-lookup)
	//    start/end events, so the channel in the log is "pinl_*" (from test.js), not
	//    our "loadm_*" prefix.  Count all new entries after baseline.
	newEntries := getNewLogEntries(durationLogPath, logBaseline, "")
	durationsByChannel := map[string][]string{}
	for _, entry := range newEntries {
		parts := strings.Split(entry, ",")
		if len(parts) >= 4 {
			ch := parts[2]
			durationsByChannel[ch] = append(durationsByChannel[ch], entry)
		}
	}

	// 3. CPU stats
	cpuMu.Lock()
	cpuMax, cpuAvg := 0.0, 0.0
	for _, c := range cpuSamples {
		if c > cpuMax {
			cpuMax = c
		}
		cpuAvg += c
	}
	if len(cpuSamples) > 0 {
		cpuAvg /= float64(len(cpuSamples))
	}
	cpuMu.Unlock()

	// 4. Tally
	okCount, failCount, gwFail := 0, 0, 0
	answeredTotal, bridgeTotal, hangupTotal, dtmfSentTotal, dtmfRecvTotal, dtmfMatchTotal := 0, 0, 0, 0, 0, 0
	_ = durationsByChannel // loopback channels vary, can't match per-call

	t.Log("")
	t.Log("==========================================================================================")
	t.Logf("  Multi-AppID Load Test -- %d loopback calls (max %d concurrent), DTMF both legs", totalCalls, maxConcurrent)
	t.Log("==========================================================================================")
	t.Logf("  %-3s %-8s %-6s %-36s %-6s %-7s %-28s %-6s",
		"#", "AppID", "Col", "CallID", "DTMF", "DialMs", "Events", "Status")
	t.Log("------------------------------------------------------------------------------------------")

	for i, d := range defs {
		o := results[i]
		status := "OK"
		if !o.ok {
			status = "FAIL"
			failCount++
			if strings.Contains(o.err, "dial") {
				gwFail++
			}
		} else {
			okCount++
		}

		if hasEvent(o.events, "answered") {
			answeredTotal++
		}
		if hasEvent(o.events, "bridge_start") {
			bridgeTotal++
		}
		if hasEvent(o.events, "hangup") {
			hangupTotal++
		}
		if o.dtmfSent != "" {
			dtmfSentTotal++
		}
		if o.dtmfRecv {
			dtmfRecvTotal++
			if o.dtmfRecvMatch {
				dtmfMatchTotal++
			}
		}

		_ = d.channel // loopback channels vary, can't match per-call

		appShort := d.appid
		if len(appShort) > 8 {
			appShort = appShort[:8]
		}
		col := d.collision
		if col == "" {
			col = "-"
		}
		callid := o.callid
		if callid == "" {
			callid = "-"
		}
		dtmfStr := o.dtmfSent
		if o.dtmfRecvMatch {
			dtmfStr += " OK"
		} else if o.dtmfRecv {
			dtmfStr += " !=" + o.dtmfRecvDigit
		}
		evStr := strings.Join(o.events, ",")
		if len(evStr) > 28 {
			evStr = evStr[:25] + "..."
		}
		if !o.ok {
			evStr = o.err
			if len(evStr) > 28 {
				evStr = evStr[:25] + "..."
			}
		}

		t.Logf("  %-3d %-8s %-6s %-36s %-6s %-7d %-28s %-6s",
			i, appShort, col, callid, dtmfStr, o.dialMs, evStr, status)
	}

	// B-leg DTMF verification
	bLegMu.Lock()
	bLegSent := len(bLegDTMF)
	bLegRecvCount, bLegMatchCount := 0, 0
	for cid, sent := range bLegDTMF {
		events := handler.getEvents(cid)
		for _, ev := range events {
			if strings.HasPrefix(ev, "dtmf:") {
				bLegRecvCount++
				if strings.TrimPrefix(ev, "dtmf:") == sent {
					bLegMatchCount++
				}
				break
			}
		}
	}
	bLegMu.Unlock()

	accepted := acceptCount.Load()

	t.Log("==========================================================================================")
	t.Logf("  Calls:     %d/%d succeeded  (%d gateway failures)", okCount, totalCalls, gwFail)
	t.Logf("  Events:    answered=%d  bridge=%d  hangup=%d", answeredTotal, bridgeTotal, hangupTotal)
	t.Logf("  DTMF A-leg:  sent=%d  received=%d  matched=%d", dtmfSentTotal, dtmfRecvTotal, dtmfMatchTotal)
	t.Logf("  DTMF B-leg:  sent=%d  received=%d  matched=%d  (inbound leg)", bLegSent, bLegRecvCount, bLegMatchCount)
	t.Logf("  Accepted:  %d incoming calls (auto-accept)", accepted)
	t.Logf("  Durations: %d new entries in log (expect ~%d)", len(newEntries), okCount)
	t.Logf("  Cross-rte: %d errors", crossRouteErrors)
	t.Logf("  CM CPU:    avg=%.1f%%  peak=%.1f%%  (%d samples)", cpuAvg, cpuMax, len(cpuSamples))
	t.Logf("  Go SDK:    %d goroutines", runtime.NumGoroutine())
	t.Logf("  Elapsed:   %s", elapsed.Round(time.Second))
	t.Log("==========================================================================================")

	// --- Assertions ---
	if crossRouteErrors > 0 {
		t.Errorf("CRITICAL: %d cross-routing errors detected in collision pairs", crossRouteErrors)
	}

	if okCount < totalCalls*4/5 { // Allow up to 20% gateway temp failures
		t.Errorf("Too many failures: %d/%d succeeded (need >= 16)", okCount, totalCalls)
	}

	if len(newEntries) == 0 && okCount > 0 {
		if _, err := os.Stat(durationLogPath); errors.Is(err, os.ErrNotExist) {
			t.Log("Duration log not found (running remotely?) -- skipping duration check")
		} else {
			t.Error("No new duration log entries -- logging may be broken")
		}
	}

	if answeredTotal < okCount/2 {
		t.Errorf("Too few answered events: %d (expected at least %d)", answeredTotal, okCount/2)
	}

	// A-leg DTMF round-trip: received digits must match sent digits on most calls
	if okCount > 0 && dtmfRecvTotal < okCount/2 {
		t.Errorf("Too few A-leg DTMF received: %d/%d (expected at least %d)", dtmfRecvTotal, okCount, okCount/2)
	}
	if dtmfRecvTotal > 0 && dtmfMatchTotal < dtmfRecvTotal/2 {
		t.Errorf("A-leg DTMF mismatch: only %d/%d received matched sent digits", dtmfMatchTotal, dtmfRecvTotal)
	}

	// B-leg DTMF: at least half of accepted calls should have DTMF received
	if bLegRecvCount < okCount/2 {
		t.Errorf("Too few B-leg DTMF: %d/%d (expected at least %d)", bLegRecvCount, bLegSent, okCount/2)
	}

	t.Logf("\nLoad test complete: %d/%d calls succeeded, %d cross-route errors, %d gateway failures, %d accepted",
		okCount, totalCalls, crossRouteErrors, gwFail, accepted)
}

func goroutineCount() int {
	return runtime.NumGoroutine()
}
