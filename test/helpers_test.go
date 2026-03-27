package test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	telephony "github.com/AgoraIO-Solutions/telephony-go"

	"github.com/gorilla/websocket"
)

// --- Mock WebSocket server ---

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// startMockServer creates an httptest server with a WS endpoint at /v1/ws/events.
// The provided handler function is called for each WS connection.
func startMockServer(t *testing.T, handler func(conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ws/events", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade error: %v", err)
			return
		}
		defer conn.Close()
		handler(conn)
	})
	return httptest.NewServer(mux)
}

// wsURLFromServer converts an httptest server URL to ws:// + /v1/ws/events.
func wsURLFromServer(server *httptest.Server) string {
	return "ws" + server.URL[4:] + "/v1/ws/events" // trim "http", add "ws"
}

// doHandshake performs the connect+register server-side handshake.
func doHandshake(conn *websocket.Conn, sessionID, clientID string) {
	conn.WriteJSON(map[string]interface{}{
		"status":     "connected",
		"session_id": sessionID,
	})
	conn.ReadMessage() // read register message
	conn.WriteJSON(map[string]interface{}{
		"status":    "registered",
		"client_id": clientID,
	})
}

// echoRequestID copies request_id from a received message into a response message.
// Mock servers must call this before sending responses so the SDK can match them.
func echoRequestID(received map[string]interface{}, response map[string]interface{}) {
	if reqID, ok := received["request_id"].(string); ok && reqID != "" {
		response["request_id"] = reqID
	}
}

// --- Mock EventHandler ---

// mockHandler records events for test assertions.
type mockHandler struct {
	mu             sync.Mutex
	connectedCalls int
	sessionID      string
	events         []string
	incomingCalls  []*telephony.Call
	claimIncoming  bool
	dtmfDigits     []string
}

func newMockHandler(claimIncoming bool) *mockHandler {
	return &mockHandler{claimIncoming: claimIncoming}
}

func (h *mockHandler) OnConnected(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connectedCalls++
	h.sessionID = sessionID
}

func (h *mockHandler) OnCallIncoming(call *telephony.Call) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "incoming:"+call.CallID)
	h.incomingCalls = append(h.incomingCalls, call)
	return h.claimIncoming
}

func (h *mockHandler) OnCallRinging(call *telephony.Call) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "ringing:"+call.CallID)
}

func (h *mockHandler) OnCallAnswered(call *telephony.Call) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "answered:"+call.CallID)
}

func (h *mockHandler) OnBridgeStart(call *telephony.Call) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "bridge_start:"+call.CallID)
}

func (h *mockHandler) OnBridgeEnd(call *telephony.Call) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "bridge_end:"+call.CallID)
}

func (h *mockHandler) OnCallHangup(call *telephony.Call) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "hangup:"+call.CallID)
}

func (h *mockHandler) OnError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "error:"+err.Error())
}

func (h *mockHandler) OnDTMFReceived(call *telephony.Call, digits string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "dtmf:"+digits)
	h.dtmfDigits = append(h.dtmfDigits, digits)
}

func (h *mockHandler) getDTMFDigits() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]string, len(h.dtmfDigits))
	copy(cp, h.dtmfDigits)
	return cp
}

func (h *mockHandler) getEvents() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]string, len(h.events))
	copy(cp, h.events)
	return cp
}

func (h *mockHandler) getSessionID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessionID
}

func (h *mockHandler) getConnectedCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connectedCalls
}
