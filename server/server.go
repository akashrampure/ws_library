package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	ClientID string
	wsConn   *websocket.Conn
	mu       sync.Mutex
}

type WsConfig struct {
	Addr           string
	Path           string
	AllowedOrigins []string

	HandshakeTimeout time.Duration
	PongWait         time.Duration
	WriteTimeout     time.Duration

	MaxReadMessageSize int
	ReadBufferSize     int
	WriteBufferSize    int
	EnableCompression  bool
}

func NewWsConfig(addr, path string, allowedOrigins []string) *WsConfig {
	return &WsConfig{
		Addr:           addr,
		Path:           path,
		AllowedOrigins: allowedOrigins,

		HandshakeTimeout:   10 * time.Second,
		PongWait:           60 * time.Second,
		WriteTimeout:       60 * time.Second,
		MaxReadMessageSize: 10 * 1024 * 1024,
		ReadBufferSize:     256 * 1024,
		WriteBufferSize:    256 * 1024,
		EnableCompression:  false,
	}
}

type WsCallback struct {
	Started      func()
	Stopped      func()
	OnConnect    func(clientID string)
	OnDisconnect func(clientID string, err error)
	OnMessage    func(clientID string, msg []byte)
	OnError      func(err error)
}

type Server struct {
	config     *WsConfig
	upgrader   websocket.Upgrader
	clients    sync.Map
	callbacks  *WsCallback
	logger     *log.Logger
	httpServer *http.Server
	ctx        context.Context
	cancel     context.CancelFunc

	startOnce sync.Once
	stopOnce  sync.Once
}

func NewServer(config *WsConfig, callback *WsCallback, logger *log.Logger) *Server {
	if callback == nil {
		callback = &WsCallback{}
	}
	if logger == nil {
		logger = log.New(os.Stdout, "[ws-server] ", log.LstdFlags|log.Llongfile)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		config:    config,
		logger:    logger,
		callbacks: callback,
		ctx:       ctx,
		cancel:    cancel,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if len(config.AllowedOrigins) == 1 && config.AllowedOrigins[0] == "*" {
					return true
				}
				return slices.Contains(config.AllowedOrigins, origin)
			},
			HandshakeTimeout:  config.HandshakeTimeout,
			ReadBufferSize:    config.ReadBufferSize,
			WriteBufferSize:   config.WriteBufferSize,
			EnableCompression: config.EnableCompression,
		},
	}
}

func (s *Server) OnMessage(handler func(clientID string, msg []byte)) {
	s.callbacks.OnMessage = handler
}

func (s *Server) OnStarted(handler func()) {
	s.callbacks.Started = handler
}

func (s *Server) OnStopped(handler func()) {
	s.callbacks.Stopped = handler
}

func (s *Server) OnConnect(handler func(clientID string)) {
	s.callbacks.OnConnect = handler
}

func (s *Server) OnDisconnect(handler func(clientID string, err error)) {
	s.callbacks.OnDisconnect = handler
}

func (s *Server) OnError(handler func(err error)) {
	s.callbacks.OnError = handler
}

func (s *Server) Start() error {
	var startErr error

	s.startOnce.Do(func() {
		mux := http.NewServeMux()
		mux.HandleFunc(s.config.Path, s.handleWS)

		s.httpServer = &http.Server{
			Addr:    s.config.Addr,
			Handler: mux,
		}

		s.logger.Printf("WebSocket server running at ws://localhost%s%s", s.config.Addr, s.config.Path)

		if s.callbacks.Started != nil {
			s.callbacks.Started()
		}

		startErr = s.httpServer.ListenAndServe()
		if startErr != nil {
			s.logger.Printf("HTTP server failed: %v", startErr)
		}
	})

	return startErr
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {

	clientID := r.Header.Get("Client-Id")

	if clientID == "" {
		http.Error(w, "missing client ID", http.StatusBadRequest)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Println("WebSocket upgrade failed:", err)
		if s.callbacks.OnError != nil {
			s.callbacks.OnError(err)
		}
		http.Error(w, "WebSocket upgrade failed", http.StatusBadRequest)
		return
	}

	s.clients.Store(clientID, &Client{
		ClientID: clientID,
		wsConn:   conn,
		mu:       sync.Mutex{},
	})

	if s.callbacks.OnConnect != nil {
		s.callbacks.OnConnect(clientID)
	}

	go s.listen(clientID, conn)
}

func (s *Server) listen(clientID string, conn *websocket.Conn) {
	value, ok := s.clients.Load(clientID)
	if !ok {
		return
	}

	client, ok := value.(*Client)
	if !ok || client == nil {
		return
	}

	defer func() {
		s.closeConnection(clientID, conn, "client disconnected")
		if s.callbacks.OnDisconnect != nil {
			s.callbacks.OnDisconnect(clientID, fmt.Errorf("client terminated connection"))
		}
	}()

	conn.SetReadLimit(int64(s.config.MaxReadMessageSize))
	conn.SetReadDeadline(time.Now().Add(s.config.PongWait))

	conn.SetPingHandler(func(appData string) error {
		conn.SetWriteDeadline(time.Now().Add(s.config.WriteTimeout))
		err := conn.WriteMessage(websocket.PongMessage, []byte(appData))
		if err != nil {
			return err
		}
		conn.SetWriteDeadline(time.Time{})
		conn.SetReadDeadline(time.Now().Add(s.config.PongWait))
		return nil
	})

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err) {
				s.logger.Printf("Unexpected error from client %s: %v", client.ClientID, err)
			} else {
				s.logger.Printf("Client %s closed connection: %v", client.ClientID, err)
			}
			break
		}

		if s.callbacks.OnMessage != nil {
			s.callbacks.OnMessage(client.ClientID, msg)
		}
	}
}

func (s *Server) Broadcast(msg interface{}) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 100)

	s.clients.Range(func(key, value any) bool {
		client, ok := value.(*Client)
		if !ok || client == nil {
			return true
		}

		sem <- struct{}{}
		wg.Add(1)

		go func(c *Client) {
			defer func() {
				<-sem
				wg.Done()
			}()

			c.mu.Lock()
			defer c.mu.Unlock()
			c.wsConn.SetWriteDeadline(time.Now().Add(s.config.WriteTimeout))
			if err := c.wsConn.WriteJSON(msg); err != nil {
				s.logger.Printf("Broadcast error to client %s: %v", c.ClientID, err)
				s.closeConnection(c.ClientID, c.wsConn, "client disconnected due to error")
				if s.callbacks.OnDisconnect != nil {
					s.callbacks.OnDisconnect(c.ClientID, fmt.Errorf("client disconnected due to error: %v", err))
				}
			} else {
				c.wsConn.SetWriteDeadline(time.Time{})
			}
		}(client)
		return true
	})
	wg.Wait()
}

func (s *Server) Send(clientID string, msg interface{}) error {
	value, ok := s.clients.Load(clientID)
	if !ok {
		return fmt.Errorf("client not found: %s", clientID)
	}

	client, ok := value.(*Client)
	if !ok || client == nil {
		return fmt.Errorf("client cast failed or is nil: %s", clientID)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	client.wsConn.SetWriteDeadline(time.Now().Add(s.config.WriteTimeout))
	if err := client.wsConn.WriteJSON(msg); err != nil {
		s.logger.Printf("Send error to client %s: %v", clientID, err)
		s.closeConnection(clientID, client.wsConn, "client disconnected due to error")
		return fmt.Errorf("write to client %s failed: %w", clientID, err)
	}
	client.wsConn.SetWriteDeadline(time.Time{})
	return nil
}

func (s *Server) closeConnection(clientID string, conn *websocket.Conn, reason string) {
	closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason)
	conn.SetWriteDeadline(time.Now().Add(s.config.WriteTimeout))
	_ = conn.WriteMessage(websocket.CloseMessage, closeMsg)

	time.Sleep(100 * time.Millisecond)

	_ = conn.Close()

	s.clients.Delete(clientID)
}

func (s *Server) Shutdown() {
	s.stopOnce.Do(func() {
		s.cancel()

		if s.callbacks.Stopped != nil {
			s.callbacks.Stopped()
		}

		if s.httpServer != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.httpServer.Shutdown(ctx); err != nil {
				s.logger.Printf("HTTP server shutdown failed: %v", err)
			}
		}

		s.clients.Range(func(key, value any) bool {
			conn := value.(*Client)
			s.closeConnection(conn.ClientID, conn.wsConn, "server shutting down")
			return true
		})
	})
}
