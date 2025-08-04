package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type TestMessage struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type TestClient struct {
	conn     *websocket.Conn
	clientID string
	messages [][]byte
	mu       sync.Mutex
	closed   bool
}

func NewTestClient(serverURL, clientID string) (*TestClient, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}
	u.Scheme = "ws"

	headers := http.Header{
		"Client-Id": {clientID},
	}

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), headers)
	if err != nil {
		return nil, err
	}

	client := &TestClient{
		conn:     conn,
		clientID: clientID,
		messages: make([][]byte, 0),
	}

	go client.readMessages()

	return client, nil
}

func (tc *TestClient) readMessages() {
	defer func() {
		tc.mu.Lock()
		tc.closed = true
		tc.mu.Unlock()
	}()

	for {
		_, message, err := tc.conn.ReadMessage()
		if err != nil {
			return
		}
		tc.mu.Lock()
		tc.messages = append(tc.messages, message)
		tc.mu.Unlock()
	}
}

func (tc *TestClient) Send(msg interface{}) error {
	return tc.conn.WriteJSON(msg)
}

func (tc *TestClient) GetMessages() [][]byte {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return append([][]byte(nil), tc.messages...)
}

func (tc *TestClient) Close() error {
	return tc.conn.Close()
}

func (tc *TestClient) IsClosed() bool {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.closed
}

func TestServer_BasicConnection(t *testing.T) {
	config := NewWsConfig(":0", "/ws", []string{"*"})

	var connected, stopped bool
	var connectedClientID string
	var mu sync.Mutex

	callbacks := &WsCallback{

		Stopped: func() {
			mu.Lock()
			stopped = true
			mu.Unlock()
		},
		OnConnect: func(clientID string) {
			mu.Lock()
			connected = true
			connectedClientID = clientID
			mu.Unlock()
		},
	}

	server := NewServer(config, callbacks, nil)

	go func() {
		if err := server.Start(); err != nil && err != http.ErrServerClosed {
			t.Errorf("Server failed to start: %v", err)
		}
	}()

	time.Sleep(100 * time.Millisecond)

	testServer := httptest.NewServer(http.HandlerFunc(server.handleWS))
	defer testServer.Close()

	client, err := NewTestClient(testServer.URL+"/ws", "test-client-1")
	if err != nil {
		t.Fatalf("Failed to connect client: %v", err)
	}
	defer client.Close()

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	isConnected := connected
	clientID := connectedClientID
	mu.Unlock()

	if !isConnected {
		t.Fatal("Server did not report client connection")
	}

	if clientID != "test-client-1" {
		t.Fatalf("Expected client ID 'test-client-1', got '%s'", clientID)
	}

	server.Shutdown()

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	isStopped := stopped
	mu.Unlock()

	if !isStopped {
		t.Fatal("Server did not call stopped callback")
	}
}

func TestServer_MessageHandling(t *testing.T) {
	config := NewWsConfig(":0", "/ws", []string{"*"})

	var receivedMessages []struct {
		ClientID string
		Message  []byte
	}
	var mu sync.Mutex

	callbacks := &WsCallback{
		OnMessage: func(clientID string, msg []byte) {
			mu.Lock()
			receivedMessages = append(receivedMessages, struct {
				ClientID string
				Message  []byte
			}{clientID, msg})
			mu.Unlock()
		},
	}

	server := NewServer(config, callbacks, nil)

	testServer := httptest.NewServer(http.HandlerFunc(server.handleWS))
	defer testServer.Close()

	client, err := NewTestClient(testServer.URL+"/ws", "test-client-1")
	if err != nil {
		t.Fatalf("Failed to connect client: %v", err)
	}
	defer client.Close()

	time.Sleep(100 * time.Millisecond)

	testMsg := TestMessage{Type: "test", Data: "hello server"}
	if err := client.Send(testMsg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	messages := receivedMessages
	mu.Unlock()

	if len(messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(messages))
	}

	if messages[0].ClientID != "test-client-1" {
		t.Fatalf("Expected client ID 'test-client-1', got '%s'", messages[0].ClientID)
	}

	var receivedMsg TestMessage
	if err := json.Unmarshal(messages[0].Message, &receivedMsg); err != nil {
		t.Fatalf("Failed to unmarshal message: %v", err)
	}

	if receivedMsg.Type != testMsg.Type || receivedMsg.Data != testMsg.Data {
		t.Fatalf("Message mismatch. Expected %+v, got %+v", testMsg, receivedMsg)
	}
}

func TestServer_SendToSpecificClient(t *testing.T) {
	config := NewWsConfig(":0", "/ws", []string{"*"})
	server := NewServer(config, nil, nil)

	testServer := httptest.NewServer(http.HandlerFunc(server.handleWS))
	defer testServer.Close()

	client1, err := NewTestClient(testServer.URL+"/ws", "client-1")
	if err != nil {
		t.Fatalf("Failed to connect client1: %v", err)
	}
	defer client1.Close()

	client2, err := NewTestClient(testServer.URL+"/ws", "client-2")
	if err != nil {
		t.Fatalf("Failed to connect client2: %v", err)
	}
	defer client2.Close()

	time.Sleep(100 * time.Millisecond)

	testMsg := TestMessage{Type: "private", Data: "hello client-1"}
	if err := server.Send("client-1", testMsg); err != nil {
		t.Fatalf("Failed to send message to client-1: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	client1Messages := client1.GetMessages()
	if len(client1Messages) != 1 {
		t.Fatalf("Client1 expected 1 message, got %d", len(client1Messages))
	}

	var receivedMsg TestMessage
	if err := json.Unmarshal(client1Messages[0], &receivedMsg); err != nil {
		t.Fatalf("Failed to unmarshal message: %v", err)
	}

	if receivedMsg.Type != testMsg.Type || receivedMsg.Data != testMsg.Data {
		t.Fatalf("Message mismatch. Expected %+v, got %+v", testMsg, receivedMsg)
	}

	client2Messages := client2.GetMessages()
	if len(client2Messages) != 0 {
		t.Fatalf("Client2 expected 0 messages, got %d", len(client2Messages))
	}
}

func TestServer_Broadcast(t *testing.T) {
	config := NewWsConfig(":0", "/ws", []string{"*"})
	server := NewServer(config, nil, nil)

	testServer := httptest.NewServer(http.HandlerFunc(server.handleWS))
	defer testServer.Close()

	client1, err := NewTestClient(testServer.URL+"/ws", "client-1")
	if err != nil {
		t.Fatalf("Failed to connect client1: %v", err)
	}
	defer client1.Close()

	client2, err := NewTestClient(testServer.URL+"/ws", "client-2")
	if err != nil {
		t.Fatalf("Failed to connect client2: %v", err)
	}
	defer client2.Close()

	client3, err := NewTestClient(testServer.URL+"/ws", "client-3")
	if err != nil {
		t.Fatalf("Failed to connect client3: %v", err)
	}
	defer client3.Close()

	time.Sleep(100 * time.Millisecond)

	testMsg := TestMessage{Type: "broadcast", Data: "hello everyone"}
	server.Broadcast(testMsg)

	time.Sleep(200 * time.Millisecond)

	clients := []*TestClient{client1, client2, client3}
	for i, client := range clients {
		messages := client.GetMessages()
		if len(messages) != 1 {
			t.Fatalf("Client %d expected 1 message, got %d", i+1, len(messages))
		}

		var receivedMsg TestMessage
		if err := json.Unmarshal(messages[0], &receivedMsg); err != nil {
			t.Fatalf("Client %d: Failed to unmarshal message: %v", i+1, err)
		}

		if receivedMsg.Type != testMsg.Type || receivedMsg.Data != testMsg.Data {
			t.Fatalf("Client %d: Message mismatch. Expected %+v, got %+v", i+1, testMsg, receivedMsg)
		}
	}
}

func TestServer_ClientDisconnection(t *testing.T) {
	config := NewWsConfig(":0", "/ws", []string{"*"})

	var disconnectedClients []string
	var mu sync.Mutex

	callbacks := &WsCallback{
		OnDisconnect: func(clientID string, err error) {
			mu.Lock()
			disconnectedClients = append(disconnectedClients, clientID)
			mu.Unlock()
		},
	}

	server := NewServer(config, callbacks, nil)

	testServer := httptest.NewServer(http.HandlerFunc(server.handleWS))
	defer testServer.Close()

	client, err := NewTestClient(testServer.URL+"/ws", "disconnect-test")
	if err != nil {
		t.Fatalf("Failed to connect client: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	client.Close()

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	disconnected := disconnectedClients
	mu.Unlock()

	if len(disconnected) != 1 {
		t.Fatalf("Expected 1 disconnection, got %d", len(disconnected))
	}

	if disconnected[0] != "disconnect-test" {
		t.Fatalf("Expected client ID 'disconnect-test', got '%s'", disconnected[0])
	}
}

func TestServer_MissingClientID(t *testing.T) {
	config := NewWsConfig(":0", "/ws", []string{"*"})
	server := NewServer(config, nil, nil)

	testServer := httptest.NewServer(http.HandlerFunc(server.handleWS))
	defer testServer.Close()

	u, _ := url.Parse(testServer.URL + "/ws")
	u.Scheme = "ws"

	_, resp, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err == nil {
		t.Fatal("Expected connection to fail without Client-Id header")
	}

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected status %d, got %d", http.StatusBadRequest, resp.StatusCode)
	}
}

func TestServer_OriginValidation(t *testing.T) {
	config := NewWsConfig(":0", "/ws", []string{"https://example.com"})
	server := NewServer(config, nil, nil)

	testServer := httptest.NewServer(http.HandlerFunc(server.handleWS))
	defer testServer.Close()

	u, _ := url.Parse(testServer.URL + "/ws")
	u.Scheme = "ws"

	headers := http.Header{
		"Client-Id": {"test-client"},
		"Origin":    {"https://malicious.com"},
	}

	_, resp, err := websocket.DefaultDialer.Dial(u.String(), headers)
	if err == nil {
		t.Fatal("Expected connection to fail with invalid origin")
	}

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Expected status %d, got %d", http.StatusForbidden, resp.StatusCode)
	}

	validHeaders := http.Header{
		"Client-Id": {"test-client"},
		"Origin":    {"https://example.com"},
	}

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), validHeaders)
	if err != nil {
		t.Fatalf("Connection should succeed with valid origin: %v", err)
	}
	defer conn.Close()
}

func TestServer_ConcurrentConnections(t *testing.T) {
	config := NewWsConfig(":0", "/ws", []string{"*"})
	server := NewServer(config, nil, nil)

	testServer := httptest.NewServer(http.HandlerFunc(server.handleWS))
	defer testServer.Close()

	const numClients = 10
	var wg sync.WaitGroup
	var clients []*TestClient
	var mu sync.Mutex

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			client, err := NewTestClient(testServer.URL+"/ws", fmt.Sprintf("client-%d", id))
			if err != nil {
				t.Errorf("Failed to connect client %d: %v", id, err)
				return
			}
			mu.Lock()
			clients = append(clients, client)
			mu.Unlock()
		}(i)
	}

	wg.Wait()

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	connectedClients := len(clients)
	mu.Unlock()

	if connectedClients != numClients {
		t.Fatalf("Expected %d clients, got %d", numClients, connectedClients)
	}

	testMsg := TestMessage{Type: "concurrent", Data: "stress test"}
	server.Broadcast(testMsg)

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	for i, client := range clients {
		messages := client.GetMessages()
		if len(messages) != 1 {
			t.Errorf("Client %d expected 1 message, got %d", i, len(messages))
		}
	}
	mu.Unlock()

	for _, client := range clients {
		client.Close()
	}
}

func TestServer_SendToNonExistentClient(t *testing.T) {
	config := NewWsConfig(":0", "/ws", []string{"*"})
	server := NewServer(config, nil, nil)

	err := server.Send("non-existent-client", TestMessage{Type: "test", Data: "test"})
	if err == nil {
		t.Fatal("Expected error when sending to non-existent client")
	}

	expectedError := "client not found: non-existent-client"
	if err.Error() != expectedError {
		t.Fatalf("Expected error '%s', got '%s'", expectedError, err.Error())
	}
}

func TestServer_CallbackChaining(t *testing.T) {
	config := NewWsConfig(":0", "/ws", []string{"*"})

	var stopped, connected, messageReceived bool
	var mu sync.Mutex

	server := NewServer(config, nil, nil)

	server.OnStopped(func() {
		mu.Lock()
		stopped = true
		mu.Unlock()
	})

	server.OnConnect(func(clientID string) {
		mu.Lock()
		connected = true
		mu.Unlock()
	})

	server.OnMessage(func(clientID string, msg []byte) {
		mu.Lock()
		messageReceived = true
		mu.Unlock()
	})

	testServer := httptest.NewServer(http.HandlerFunc(server.handleWS))
	defer testServer.Close()

	client, err := NewTestClient(testServer.URL+"/ws", "chain-test")
	if err != nil {
		t.Fatalf("Failed to connect client: %v", err)
	}
	defer client.Close()

	time.Sleep(100 * time.Millisecond)

	client.Send(TestMessage{Type: "test", Data: "callback test"})
	time.Sleep(100 * time.Millisecond)

	server.Shutdown()
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	allCallbacksCalled := connected && messageReceived && stopped
	mu.Unlock()

	if !allCallbacksCalled {
		t.Fatalf("Not all callbacks were called. Connected: %v, MessageReceived: %v, Stopped: %v",
			connected, messageReceived, stopped)
	}
}

func BenchmarkServer_MessageThroughput(b *testing.B) {
	config := NewWsConfig(":0", "/ws", []string{"*"})
	server := NewServer(config, nil, log.New(os.Stdout, "", 0))

	testServer := httptest.NewServer(http.HandlerFunc(server.handleWS))
	defer testServer.Close()

	client, err := NewTestClient(testServer.URL+"/ws", "bench-client")
	if err != nil {
		b.Fatalf("Failed to connect client: %v", err)
	}
	defer client.Close()

	time.Sleep(100 * time.Millisecond)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		msg := TestMessage{Type: "bench", Data: "benchmark message"}
		for pb.Next() {
			server.Send("bench-client", msg)
		}
	})
}
