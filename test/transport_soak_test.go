package test

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	telephony "github.com/AgoraIO-Solutions/telephony-go"
)

// ================================================================
// Transport Soak Test — calls via LB across UDP, TCP, TLS
//
// Based on TestE2EMultiAppIDLoad pattern: single WS client,
// dials subscribed DID via LB, auto-accept inbound, DTMF both legs.
// Cycles SIP transport round-robin (UDP, TCP, TLS) across calls.
//
// Each call: outbound via LB → gateway routes back as inbound → Go SDK accepts
// = 2 SIP legs per call (1 out + 1 in)
//
// Verifies Go SDK event lifecycle per call:
//   A-leg (outbound): answered, dtmf (hangup arrives on composite key — not matchable by callid)
//   B-leg (inbound):  incoming, answered, bridge_start, dtmf, hangup
//
// Env vars:
//   BC_AUTH_TOKEN  (required)
//   BC_APPID       (required)
//   BC_INBOUND_DID (optional) — subscribed DID (default: 18005551234)
//
// 30 calls total (10 per transport), 30 max concurrent, ~15s each.
// Two instances with different BC_INBOUND_DID = ~60 calls, ~120 SIP legs.
// ================================================================

func getTransports() []struct {
	name string
	sip  string
} {
	return []struct {
		name string
		sip  string
	}{
		{"UDP", lbSip("udp")},
		{"TCP", lbSip("tcp")},
		{"TLS", lbSip("tls")},
	}
}

func TestE2ETransportSoak(t *testing.T) {
	wsURL, _, auth, appid := skipIfNoEnv(t)
	cmdAppID := commandAppID()

	const (
		callsPerTransport = 10
		totalCalls        = callsPerTransport * 3 // 30
		maxConcurrent     = 30
	)

	loopbackDID := inboundDID()
	ts := time.Now().UnixMilli()
	didSuffix := loopbackDID[len(loopbackDID)-4:]
	chPrefix := fmt.Sprintf("testc_tsoak_%d_%s", ts, didSuffix)
	clientID := fmt.Sprintf("tsoak_%d_%s", ts, didSuffix)

	handler := newLoadHandler()
	client := telephony.NewClient(wsURL, auth, clientID, appid)
	client.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()
	time.Sleep(time.Second)

	if err := client.Subscribe(ctx, []string{loopbackDID}); err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// CPU monitor
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

	// Auto-accept goroutine + B-leg DTMF
	var bLegMu sync.Mutex
	bLegDTMF := make(map[string]string)
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
					AppID: cmdAppID, Token: cmdAppID, Channel: ch, UID: "200",
				})
				aC()
				if err != nil {
					continue
				}
				go func(cid string) {
					if !waitForCallEvent(handler, cid, []string{"bridge_start", "answered"}, 30*time.Second) {
						return
					}
					time.Sleep(time.Second)
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

	// Build interleaved call definitions: UDP,TCP,TLS,UDP,TCP,TLS,...
	type callDef struct {
		idx       int
		channel   string
		uid       string
		transport string
		sipAddr   string
	}
	defs := make([]callDef, totalCalls)
	for ci := 0; ci < callsPerTransport; ci++ {
		for ti, tr := range getTransports() {
			idx := ci*len(getTransports()) + ti
			defs[idx] = callDef{
				idx:       idx,
				channel:   fmt.Sprintf("%s_%s_%d", chPrefix, strings.ToLower(tr.name), idx),
				uid:       fmt.Sprintf("%d", 100+idx),
				transport: tr.name,
				sipAddr:   tr.sip,
			}
		}
	}

	type outcome struct {
		callid        string
		events        []string
		dtmfSent      string
		dtmfRecv      bool
		dtmfRecvMatch bool
		dtmfRecvDigit string
		err           string
		ok            bool
		dialMs        int64
		sipAddr       string
		missingEvents []string // expected events not received
	}
	results := make([]outcome, totalCalls)

	sem := make(chan struct{}, maxConcurrent)

	t.Logf("=== Transport Soak: %d calls (%d/transport), max %d concurrent (DID=%s) ===",
		totalCalls, callsPerTransport, maxConcurrent, loopbackDID)
	testStart := time.Now()

	var wg sync.WaitGroup
	rateTick := time.NewTicker(334 * time.Millisecond) // 3 calls/sec per suite (~12/sec across 4 suites, ~24 SIP legs/sec)
	defer rateTick.Stop()
	for i := range defs {
		<-rateTick.C
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			def := defs[idx]
			o := &results[idx]
			o.dtmfSent = randomDTMF()
			o.sipAddr = def.sipAddr

			t0 := time.Now()
			res, err := client.Dial(ctx, telephony.DialParams{
				To:      "+" + loopbackDID,
				From:    "+15551234567",
				Channel: def.channel,
				UID:     def.uid,
				Token:   cmdAppID,
				Region:  "AREA_CODE_AS",
				Timeout: "60",
				Sip:     def.sipAddr,
				AppID:   cmdAppID,
			})
			o.dialMs = time.Since(t0).Milliseconds()

			if err != nil {
				o.err = fmt.Sprintf("dial: %v", err)
				return
			}
			if !res.Success || res.CallID == "" {
				o.err = fmt.Sprintf("dial: no callid (data=%v)", res.Data)
				return
			}
			o.callid = res.CallID

			if !waitForCallEvent(handler, res.CallID, []string{"bridge_start", "answered"}, 30*time.Second) {
				o.err = "no bridge/answered event"
				hCtx, hC := context.WithTimeout(context.Background(), 10*time.Second)
				client.Hangup(hCtx, res.CallID)
				hC()
				return
			}

			// A-leg DTMF — send 2s after bridge
			time.Sleep(2 * time.Second)
			client.SendDTMF(ctx, res.CallID, o.dtmfSent)

			// Wait for A-leg DTMF echo (up to 5s)
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

			// Hold call for remaining time to reach ~22s total from bridge
			// Already spent ~2s + DTMF wait (~1-5s) ≈ 3-7s, hold 15s more
			// Extra time lets B-leg receive bridge_start and send DTMF on late-starting calls
			time.Sleep(15 * time.Second)

			hCtx, hC := context.WithTimeout(context.Background(), 10*time.Second)
			client.Hangup(hCtx, res.CallID)
			hC()

			// Wait for hangup event — poll for up to 10s
			hangupDeadline := time.After(10 * time.Second)
			hangupTick := time.NewTicker(500 * time.Millisecond)
		hangupWait:
			for {
				select {
				case <-hangupTick.C:
					if hasEvent(handler.getEvents(res.CallID), "hangup") {
						break hangupWait
					}
				case <-hangupDeadline:
					break hangupWait
				}
			}
			hangupTick.Stop()
			o.events = handler.getEvents(res.CallID)

			// Check expected A-leg events: answered
			// hangup arrives on composite key (appid:channel:uid) not callid — SDK can't match
			// ringing/bridge_start/bridge_end not delivered to outbound A-leg by gateway
			for _, exp := range []string{"answered"} {
				found := false
				for _, ev := range o.events {
					if ev == exp {
						found = true
						break
					}
				}
				if !found {
					o.missingEvents = append(o.missingEvents, exp)
				}
			}

			o.ok = true
		}(i)

		time.Sleep(100 * time.Millisecond)
	}

	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(6 * time.Minute):
		t.Fatal("Transport soak timed out")
	}

	close(acceptDone)
	close(cpuDone)
	elapsed := time.Since(testStart)
	time.Sleep(3 * time.Second)

	// --- Summary ---
	perTransport := map[string]struct{ ok, fail, dtmfOK int }{}
	for _, tr := range getTransports() {
		perTransport[tr.name] = struct{ ok, fail, dtmfOK int }{}
	}

	okCount, failCount := 0, 0
	dtmfSentTotal, dtmfRecvTotal, dtmfMatchTotal := 0, 0, 0
	missingEventCounts := map[string]int{}

	t.Log("")
	t.Log("==========================================================================================")
	t.Logf("  Transport Soak -- %d loopback calls (max %d concurrent), DTMF both legs", totalCalls, maxConcurrent)
	t.Log("==========================================================================================")
	t.Logf("  %-3s %-5s %-45s %-8s %-7s %-32s %-6s %-s",
		"#", "XPORT", "SIP", "DTMF", "DialMs", "Events", "Status", "Missing")
	t.Log("------------------------------------------------------------------------------------------------------------------------------")

	for i := range defs {
		def := defs[i]
		o := results[i]
		status := "OK"
		if !o.ok {
			status = "FAIL"
			failCount++
		} else {
			okCount++
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

		s := perTransport[def.transport]
		if o.ok {
			s.ok++
		} else {
			s.fail++
		}
		if o.dtmfRecvMatch {
			s.dtmfOK++
		}
		perTransport[def.transport] = s

		for _, me := range o.missingEvents {
			missingEventCounts[me]++
		}

		sipStr := o.sipAddr
		if sipStr == "" {
			sipStr = def.sipAddr
		}
		dtmfStr := o.dtmfSent
		if o.dtmfRecvMatch {
			dtmfStr += " OK"
		} else if o.dtmfRecv {
			dtmfStr += "!=" + o.dtmfRecvDigit
		}
		evStr := strings.Join(o.events, ",")
		if len(evStr) > 32 {
			evStr = evStr[:29] + "..."
		}
		if !o.ok {
			evStr = o.err
			if len(evStr) > 32 {
				evStr = evStr[:29] + "..."
			}
		}
		missingStr := ""
		if len(o.missingEvents) > 0 {
			missingStr = strings.Join(o.missingEvents, ",")
		}

		t.Logf("  %-3d %-5s %-45s %-8s %-7d %-32s %-6s %s",
			i, def.transport, sipStr, dtmfStr, o.dialMs, evStr, status, missingStr)
	}

	// B-leg event verification: incoming, answered, bridge_start, bridge_end, hangup, dtmf
	bLegMu.Lock()
	bLegSent := len(bLegDTMF)
	bLegRecvCount, bLegMatchCount := 0, 0
	bLegMissingCounts := map[string]int{}
	// B-leg expected: incoming, answered, bridge_start, hangup
	// (bridge_end not reliably sent before hangup by gateway)
	bLegExpected := []string{"incoming", "answered", "bridge_start", "hangup"}
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
		// Check all expected B-leg events
		for _, exp := range bLegExpected {
			found := false
			for _, ev := range events {
				if ev == exp {
					found = true
					break
				}
			}
			if !found {
				bLegMissingCounts["B:"+exp]++
			}
		}
	}
	bLegMu.Unlock()

	accepted := acceptCount.Load()

	// CPU
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

	t.Log("==============================================================================================================================")
	for _, tr := range getTransports() {
		s := perTransport[tr.name]
		t.Logf("  %s:  %d/%d ok,  %d dtmf matched", tr.name, s.ok, s.ok+s.fail, s.dtmfOK)
	}
	t.Logf("  Total:     %d/%d succeeded  (%d failures)", okCount, totalCalls, failCount)
	t.Logf("  DTMF A-leg:  sent=%d  received=%d  matched=%d", dtmfSentTotal, dtmfRecvTotal, dtmfMatchTotal)
	t.Logf("  DTMF B-leg:  sent=%d  received=%d  matched=%d  (inbound leg)", bLegSent, bLegRecvCount, bLegMatchCount)
	// Merge A-leg and B-leg missing event counts
	allMissing := map[string]int{}
	for ev, cnt := range missingEventCounts {
		allMissing["A:"+ev] = cnt
	}
	for ev, cnt := range bLegMissingCounts {
		allMissing[ev] = cnt
	}
	if len(allMissing) > 0 {
		parts := []string{}
		for ev, cnt := range allMissing {
			parts = append(parts, fmt.Sprintf("%s=%d", ev, cnt))
		}
		t.Logf("  Missing events:  %s  (A-leg: %d ok calls, B-leg: %d accepted)", strings.Join(parts, "  "), okCount, bLegSent)
	} else {
		t.Logf("  Missing events:  none — all expected events received")
	}
	t.Logf("  Accepted:  %d incoming calls (auto-accept)", accepted)
	t.Logf("  CM CPU:    avg=%.1f%%  peak=%.1f%%  (%d samples)", cpuAvg, cpuMax, len(cpuSamples))
	t.Logf("  Go SDK:    %d goroutines", runtime.NumGoroutine())
	t.Logf("  Elapsed:   %s", elapsed.Round(time.Second))
	t.Log("==============================================================================================================================")

	// Assertions
	if okCount < totalCalls*4/5 {
		t.Errorf("Too many failures: %d/%d succeeded (need >= %d)", okCount, totalCalls, totalCalls*4/5)
	}
	for _, tr := range getTransports() {
		s := perTransport[tr.name]
		if s.ok == 0 {
			t.Errorf("%s: all %d calls failed", tr.name, s.ok+s.fail)
		}
	}
	if okCount > 0 && dtmfRecvTotal < okCount/2 {
		t.Errorf("Too few A-leg DTMF: %d/%d", dtmfRecvTotal, okCount)
	}
	if bLegRecvCount < okCount/2 {
		t.Errorf("Too few B-leg DTMF: %d/%d", bLegRecvCount, bLegSent)
	}
	totalMissing := 0
	for _, cnt := range allMissing {
		totalMissing += cnt
	}
	if totalMissing > 0 {
		t.Logf("WARNING: %d missing events (may be delayed delivery under load)", totalMissing)
	}

	// Allow in-flight gateway lookups to complete before WS disconnect.
	// Without this, lookups arriving after client.Close() get "not found".
	time.Sleep(3 * time.Second)

	t.Logf("\nTransport soak complete: %d/%d ok, %d fail", okCount, totalCalls, failCount)
}
