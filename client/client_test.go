package client

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type TestMessage struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type MockWSServer struct {
	server   *httptest.Server
	upgrader websocket.Upgrader
	clients  map[*websocket.Conn]bool
	mu       sync.RWMutex
	messages []TestMessage
	closed   bool
}

func NewMockWSServer() *MockWSServer {
	mock := &MockWSServer{
		clients: make(map[*websocket.Conn]bool),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}

	mock.server = httptest.NewServer(http.HandlerFunc(mock.handleWebSocket))
	return mock
}

func (m *MockWSServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	m.mu.Lock()
	m.clients[conn] = true
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.clients, conn)
		m.mu.Unlock()
		conn.Close()
	}()

	for !m.closed {
		var msg TestMessage
		err := conn.ReadJSON(&msg)
		if err != nil {
			break
		}
		m.mu.Lock()
		m.messages = append(m.messages, msg)
		m.mu.Unlock()

		if err := conn.WriteJSON(msg); err != nil {
			break
		}
	}
}

func (m *MockWSServer) BroadcastMessage(msg TestMessage) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for conn := range m.clients {
		conn.WriteJSON(msg)
	}
}

func (m *MockWSServer) GetReceivedMessages() []TestMessage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]TestMessage(nil), m.messages...)
}

func (m *MockWSServer) Close() {
	m.closed = true
	m.mu.Lock()
	for conn := range m.clients {
		conn.Close()
	}
	m.mu.Unlock()
	m.server.Close()
}

func (m *MockWSServer) GetHostAndPort() (string, string) {
	u, _ := url.Parse(m.server.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	return host, port
}

func TestClient_BasicConnection(t *testing.T) {
	server := NewMockWSServer()
	defer server.Close()

	host, port := server.GetHostAndPort()
	config := &ClientConfig{
		Scheme:             "ws",
		Host:               host,
		Port:               port,
		Path:               "/",
		Headers:            http.Header{"Client-Id": {"test-client"}},
		MaxReadMessageSize: 1024 * 1024,
		ReadTimeout:        30 * time.Second,
		WriteTimeout:       10 * time.Second,
		HandshakeTimeout:   10 * time.Second,
		MaxRetries:         3,
		RetryInterval:      1 * time.Second,
	}

	var connected, started, stopped bool
	var errorCount int
	var mu sync.Mutex

	callbacks := &ClientCallbacks{
		Started: func() {
			mu.Lock()
			started = true
			mu.Unlock()
		},
		Stopped: func() {
			mu.Lock()
			stopped = true
			mu.Unlock()
		},
		OnConnect: func() {
			mu.Lock()
			connected = true
			mu.Unlock()
		},
		OnDisconnect: func(err error) {
			mu.Lock()
			errorCount++
			mu.Unlock()
		},
	}

	client := NewClient(config, callbacks, nil)
	client.Start()

	for i := 0; i < 50; i++ {
		mu.Lock()
		isConnected := connected && started
		mu.Unlock()
		if isConnected {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	isConnected := connected
	isStarted := started
	errors := errorCount
	mu.Unlock()

	if !isConnected {
		t.Fatal("Client failed to connect")
	}

	if !isStarted {
		t.Fatal("Client failed to start properly")
	}

	client.Stop()

	for i := 0; i < 50; i++ {
		mu.Lock()
		isStopped := stopped
		mu.Unlock()
		if isStopped {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	isStopped := stopped
	mu.Unlock()

	if !isStopped {
		t.Fatal("Client failed to call stopped callback properly")
	}

	if errors > 0 {
		t.Fatalf("Client had %d errors", errors)
	}
}

func TestClient_MessageSendAndReceive(t *testing.T) {
	server := NewMockWSServer()
	defer server.Close()

	host, port := server.GetHostAndPort()
	config := &ClientConfig{
		Scheme:             "ws",
		Host:               host,
		Port:               port,
		Path:               "/",
		Headers:            http.Header{"Client-Id": {"test-client"}},
		MaxReadMessageSize: 1024 * 1024,
		ReadTimeout:        30 * time.Second,
		WriteTimeout:       10 * time.Second,
		HandshakeTimeout:   10 * time.Second,
		MaxRetries:         3,
		RetryInterval:      1 * time.Second,
	}

	var receivedMessages []TestMessage
	var mu sync.Mutex
	var connected bool

	callbacks := &ClientCallbacks{
		OnConnect: func() {
			mu.Lock()
			connected = true
			mu.Unlock()
		},
		OnMessage: func(msg []byte) {
			var testMsg TestMessage
			if err := json.Unmarshal(msg, &testMsg); err == nil {
				mu.Lock()
				receivedMessages = append(receivedMessages, testMsg)
				mu.Unlock()
			}
		},
	}

	client := NewClient(config, callbacks, nil)
	client.Start()

	for i := 0; i < 50; i++ {
		mu.Lock()
		isConnected := connected
		mu.Unlock()
		if isConnected {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	testMsg := TestMessage{Type: "test", Data: "hello world"}
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

	if messages[0].Type != testMsg.Type || messages[0].Data != testMsg.Data {
		t.Fatalf("Message mismatch. Expected %+v, got %+v", testMsg, messages[0])
	}

	client.Stop()
}

func TestClient_Reconnection(t *testing.T) {
	server := NewMockWSServer()
	host, port := server.GetHostAndPort()

	config := &ClientConfig{
		Scheme:             "ws",
		Host:               host,
		Port:               port,
		Path:               "/",
		Headers:            http.Header{"Client-Id": {"test-client"}},
		MaxReadMessageSize: 1024 * 1024,
		ReadTimeout:        2 * time.Second,
		WriteTimeout:       1 * time.Second,
		HandshakeTimeout:   1 * time.Second,
		MaxRetries:         5,
		RetryInterval:      500 * time.Millisecond,
	}

	var connectCount, disconnectCount int
	var mu sync.Mutex
	var firstConnected bool

	callbacks := &ClientCallbacks{
		OnConnect: func() {
			mu.Lock()
			connectCount++
			firstConnected = true
			mu.Unlock()
		},
		OnDisconnect: func(err error) {
			mu.Lock()
			disconnectCount++
			mu.Unlock()
		},
		OnError: func(err error) {
			t.Logf("Error occurred: %v", err)
		},
	}

	client := NewClient(config, callbacks, log.New(os.Stdout, "[test] ", log.LstdFlags))
	client.Start()

	for i := 0; i < 50; i++ {
		mu.Lock()
		connected := firstConnected
		mu.Unlock()
		if connected {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	server.Close()

	time.Sleep(3 * time.Second)

	mu.Lock()
	connects := connectCount
	disconnects := disconnectCount
	mu.Unlock()

	t.Logf("Connections: %d, Disconnections: %d", connects, disconnects)

	if connects < 1 {
		t.Fatalf("Expected at least 1 connection, got %d", connects)
	}

	if disconnects < 1 {
		t.Fatalf("Expected at least 1 disconnection, got %d", disconnects)
	}

	client.Stop()
}

func TestClient_MaxRetriesExceeded(t *testing.T) {
	config := &ClientConfig{
		Scheme:             "ws",
		Host:               "127.0.0.1",
		Port:               "99999",
		Path:               "/",
		Headers:            http.Header{"Client-Id": {"test-client"}},
		MaxReadMessageSize: 1024 * 1024,
		ReadTimeout:        30 * time.Second,
		WriteTimeout:       10 * time.Second,
		HandshakeTimeout:   1 * time.Second,
		MaxRetries:         2,
		RetryInterval:      100 * time.Millisecond,
	}

	var errorCount int
	var mu sync.Mutex
	var lastError error

	callbacks := &ClientCallbacks{
		OnError: func(err error) {
			mu.Lock()
			errorCount++
			lastError = err
			mu.Unlock()
		},
	}

	client := NewClient(config, callbacks, nil)
	client.Start()

	time.Sleep(2 * time.Second)

	mu.Lock()
	errors := errorCount
	finalError := lastError
	mu.Unlock()

	if errors < 2 {
		t.Fatalf("Expected at least 2 errors, got %d", errors)
	}

	if finalError == nil || !strings.Contains(finalError.Error(), "max retries exceeded") {
		t.Fatalf("Expected max retries error, got: %v", finalError)
	}

	client.Stop()
}

func TestClient_SendWithoutConnection(t *testing.T) {
	config := &ClientConfig{
		Scheme:             "ws",
		Host:               "127.0.0.1",
		Port:               "99999",
		Path:               "/",
		Headers:            http.Header{"Client-Id": {"test-client"}},
		MaxReadMessageSize: 1024 * 1024,
		ReadTimeout:        30 * time.Second,
		WriteTimeout:       10 * time.Second,
		HandshakeTimeout:   1 * time.Second,
		MaxRetries:         1,
		RetryInterval:      100 * time.Millisecond,
	}

	client := NewClient(config, nil, nil)

	err := client.Send(TestMessage{Type: "test", Data: "data"})
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("Expected 'not connected' error, got: %v", err)
	}
}

func TestClient_ConcurrentMessages(t *testing.T) {
	server := NewMockWSServer()
	defer server.Close()

	host, port := server.GetHostAndPort()
	config := &ClientConfig{
		Scheme:             "ws",
		Host:               host,
		Port:               port,
		Path:               "/",
		Headers:            http.Header{"Client-Id": {"test-client"}},
		MaxReadMessageSize: 1024 * 1024,
		ReadTimeout:        30 * time.Second,
		WriteTimeout:       10 * time.Second,
		HandshakeTimeout:   10 * time.Second,
		MaxRetries:         3,
		RetryInterval:      1 * time.Second,
	}

	var receivedMessages []TestMessage
	var mu sync.Mutex
	var connected bool

	callbacks := &ClientCallbacks{
		OnConnect: func() {
			mu.Lock()
			connected = true
			mu.Unlock()
		},
		OnMessage: func(msg []byte) {
			var testMsg TestMessage
			if err := json.Unmarshal(msg, &testMsg); err == nil {
				mu.Lock()
				receivedMessages = append(receivedMessages, testMsg)
				mu.Unlock()
			}
		},
	}

	client := NewClient(config, callbacks, nil)
	client.Start()

	for i := 0; i < 50; i++ {
		mu.Lock()
		isConnected := connected
		mu.Unlock()
		if isConnected {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	const numMessages = 10
	var wg sync.WaitGroup

	for i := 0; i < numMessages; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			msg := TestMessage{Type: "test", Data: fmt.Sprintf("message-%d", id)}
			if err := client.Send(msg); err != nil {
				t.Errorf("Failed to send message %d: %v", id, err)
			}
		}(i)
	}

	wg.Wait()

	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	messages := receivedMessages
	mu.Unlock()

	if len(messages) != numMessages {
		t.Fatalf("Expected %d messages, got %d", numMessages, len(messages))
	}

	client.Stop()
}

func TestClient_CallbackChaining(t *testing.T) {
	server := NewMockWSServer()
	defer server.Close()

	host, port := server.GetHostAndPort()
	config := &ClientConfig{
		Scheme:             "ws",
		Host:               host,
		Port:               port,
		Path:               "/",
		Headers:            http.Header{"Client-Id": {"test-client"}},
		MaxReadMessageSize: 1024 * 1024,
		ReadTimeout:        30 * time.Second,
		WriteTimeout:       10 * time.Second,
		HandshakeTimeout:   10 * time.Second,
		MaxRetries:         3,
		RetryInterval:      1 * time.Second,
	}

	var started, stopped, connected bool
	var mu sync.Mutex

	client := NewClient(config, nil, nil)

	client.OnStarted(func() {
		mu.Lock()
		started = true
		mu.Unlock()
	})

	client.OnStopped(func() {
		mu.Lock()
		stopped = true
		mu.Unlock()
	})

	client.OnConnect(func() {
		mu.Lock()
		connected = true
		mu.Unlock()
	})

	client.Start()

	for i := 0; i < 50; i++ {
		mu.Lock()
		isStarted := started
		isConnected := connected
		mu.Unlock()
		if isStarted && isConnected {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	client.Stop()
	for i := 0; i < 50; i++ {
		mu.Lock()
		isStopped := stopped
		mu.Unlock()
		if isStopped {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	wasStarted := started
	wasStopped := stopped
	wasConnected := connected
	mu.Unlock()

	if !wasStarted {
		t.Error("Started callback was not called")
	}

	if !wasStopped {
		t.Error("Stopped callback was not called")
	}

	if !wasConnected {
		t.Error("Connect callback was not called")
	}
}

func TestClient_ConnectionTimeout(t *testing.T) {
	config := &ClientConfig{
		Scheme:             "ws",
		Host:               "10.255.255.1",
		Port:               "8080",
		Path:               "/",
		Headers:            http.Header{"Client-Id": {"test-client"}},
		MaxReadMessageSize: 1024 * 1024,
		ReadTimeout:        30 * time.Second,
		WriteTimeout:       10 * time.Second,
		HandshakeTimeout:   1 * time.Second,
		MaxRetries:         2,
		RetryInterval:      100 * time.Millisecond,
	}

	var errorCount int
	var mu sync.Mutex

	callbacks := &ClientCallbacks{
		OnError: func(err error) {
			mu.Lock()
			errorCount++
			mu.Unlock()
		},
	}

	client := NewClient(config, callbacks, nil)
	client.Start()

	time.Sleep(3 * time.Second)

	mu.Lock()
	errors := errorCount
	mu.Unlock()

	if errors < 2 {
		t.Fatalf("Expected at least 2 timeout errors, got %d", errors)
	}

	client.Stop()
}

func BenchmarkClient_MessageThroughput(b *testing.B) {
	server := NewMockWSServer()
	defer server.Close()

	host, port := server.GetHostAndPort()
	config := &ClientConfig{
		Scheme:             "ws",
		Host:               host,
		Port:               port,
		Path:               "/",
		Headers:            http.Header{"Client-Id": {"bench-client"}},
		MaxReadMessageSize: 1024 * 1024,
		ReadTimeout:        30 * time.Second,
		WriteTimeout:       10 * time.Second,
		HandshakeTimeout:   10 * time.Second,
		MaxRetries:         3,
		RetryInterval:      1 * time.Second,
	}

	var connected bool
	var mu sync.Mutex

	callbacks := &ClientCallbacks{
		OnConnect: func() {
			mu.Lock()
			connected = true
			mu.Unlock()
		},
	}

	client := NewClient(config, callbacks, log.New(os.Stdout, "", 0))
	client.Start()

	for i := 0; i < 100; i++ {
		mu.Lock()
		isConnected := connected
		mu.Unlock()
		if isConnected {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		msg := TestMessage{Type: "bench", Data: "benchmark message"}
		for pb.Next() {
			client.Send(msg)
		}
	})

	client.Stop()
}
