package server

import (
	"compress/gzip"
	"encoding/json"
	"github.com/gorilla/websocket"
	v2 "github.com/komari-monitor/komari-agent/protocol/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func preservePanelConfig(t *testing.T) {
	t.Helper()
	previous := *flags
	t.Cleanup(func() { *flags = previous })
	flags.Token = "local-test-token"
	flags.PreferIPVersion = ""
	flags.IgnoreUnsafeCert = false
	flags.DisableCompression = false
}

func decodeTestRequest(t *testing.T, r *http.Request) map[string]interface{} {
	t.Helper()
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return nil
		}
		defer reader.Close()
		body = reader
	}
	var result map[string]interface{}
	if err := json.NewDecoder(body).Decode(&result); err != nil {
		t.Error(err)
	}
	return result
}

func TestV2HTTPContract(t *testing.T) {
	for _, method := range []string{v2.MethodAgentBasicInfo, v2.MethodAgentTaskResult, v2.MethodAgentPingResult} {
		for _, response := range []struct {
			name, body string
			status     int
			fail       bool
		}{
			{"ok", `{"jsonrpc":"2.0","result":{}}`, 200, false},
			{"missing", `{"jsonrpc":"2.0","error":{"code":-32601,"message":"method not found"}}`, 400, true},
			{"denied", `{"jsonrpc":"2.0","error":{"code":-32001,"message":"denied"}}`, 200, true},
			{"old", `<html>old panel</html>`, 200, true},
			{"404", "", 404, true},
		} {
			t.Run(method+response.name, func(t *testing.T) {
				preservePanelConfig(t)
				var calls atomic.Int32
				panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != "/api/clients/v2/rpc" || r.URL.Query().Get("token") != "local-test-token" {
						t.Errorf("unexpected endpoint: %s", r.URL.Path)
					}
					body := decodeTestRequest(t, r)
					if body["jsonrpc"] != "2.0" || body["method"] != method {
						t.Errorf("unexpected request: %#v", body)
					}
					w.WriteHeader(response.status)
					_, _ = io.WriteString(w, response.body)
				}))
				defer panel.Close()
				flags.Endpoint = panel.URL
				var err error
				switch method {
				case v2.MethodAgentBasicInfo:
					err = tryUploadData(map[string]interface{}{"cpu_name": "fixture", "cpu_physical_cores": 2})
				case v2.MethodAgentTaskResult:
					err = sendTaskResult("fixture", "safe fixture", 0, time.Now())
				case v2.MethodAgentPingResult:
					err = sendPingResult(nil, 7, "tcp", 12, time.Now())
				}
				if (err != nil) != response.fail {
					t.Fatalf("error=%v, want failure %v", err, response.fail)
				}
				if calls.Load() != 1 {
					t.Fatalf("unexpected retry/downgrade: %d requests", calls.Load())
				}
			})
		}
	}
}

func TestV2WebSocketAndDisabledRemoteControl(t *testing.T) {
	preservePanelConfig(t)
	flags.DisableWebSsh = true
	result := make(chan map[string]interface{}, 1)
	messages := make(chan map[string]interface{}, 2)
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/clients/v2/rpc" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			result <- decodeTestRequest(t, r)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","result":{}}`)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for i := 0; i < 2; i++ {
			var body map[string]interface{}
			if err := conn.ReadJSON(&body); err != nil {
				t.Error(err)
				return
			}
			messages <- body
		}
		_ = conn.WriteJSON(v2.Request{JSONRPC: v2.Version, Method: v2.MethodAgentExec, Params: map[string]interface{}{"task_id": "disabled-task", "command": "MUST_NOT_EXECUTE"}})
		_, _, _ = conn.ReadMessage()
	}))
	defer panel.Close()
	flags.Endpoint = panel.URL
	conn, err := connectWebSocket(buildWebSocketEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, v2.BuildReportPayload([]byte(`{"cpu":42}`))); err != nil {
		t.Fatal(err)
	}
	if err := sendPingResult(conn, 7, "tcp", 12, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{v2.MethodAgentReport, v2.MethodAgentPingResult} {
		select {
		case body := <-messages:
			if body["jsonrpc"] != "2.0" || body["method"] != method {
				t.Fatalf("wrong envelope: %#v", body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("websocket report timeout")
		}
	}
	done := make(chan struct{})
	go handleWebSocketMessages(conn, done, nil)
	select {
	case body := <-result:
		params := body["params"].(map[string]interface{})
		if body["method"] != v2.MethodAgentTaskResult || params["task_id"] != "disabled-task" || params["result"] != "Remote control is disabled." || params["exit_code"] != float64(-1) {
			t.Fatalf("remote control guard failed: %#v", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disabled command timeout")
	}
	conn.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not stop")
	}
}

func TestHTTPReportingSurvivesWebSocketFailure(t *testing.T) {
	preservePanelConfig(t)
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/clients/v2/rpc" {
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","result":{}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer panel.Close()
	flags.Endpoint = panel.URL
	conn, err := connectWebSocket(buildWebSocketEndpoint())
	if conn != nil || err == nil {
		t.Fatal("expected failed websocket")
	}
	if _, err := postV2Request(v2.BuildReportRequest("fixture", []byte(`{}`), nil)); err != nil {
		t.Fatal(err)
	}
}
