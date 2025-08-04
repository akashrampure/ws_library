package client

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type ClientConfig struct {
	Scheme  string
	Host    string
	Port    string
	Path    string
	Headers http.Header

	MaxReadMessageSize int

	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	HandshakeTimeout time.Duration

	MaxRetries    int
	RetryInterval time.Duration
}

func NewClientConfig(scheme, host, port, path, clientId string, retryInterval, maxRetries int) *ClientConfig {
	return &ClientConfig{
		Scheme: scheme,
		Host:   host,
		Port:   port,
		Path:   path,
		Headers: http.Header{
			"Client-Id": {clientId},
		},

		MaxReadMessageSize: 10 * 1024 * 1024,

		ReadTimeout:      60 * time.Second,
		WriteTimeout:     10 * time.Second,
		HandshakeTimeout: 10 * time.Second,

		MaxRetries:    maxRetries,
		RetryInterval: time.Duration(retryInterval) * time.Second,
	}
}

type ClientCallbacks struct {
	Started      func()
	Stopped      func()
	OnConnect    func()
	OnDisconnect func(err error)
	OnMessage    func(msg []byte)
	OnError      func(err error)
}

type Client struct {
	config    *ClientConfig
	callbacks *ClientCallbacks

	conn      *websocket.Conn
	mu        sync.RWMutex
	writeMu   sync.Mutex
	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup

	ctx    context.Context
	cancel context.CancelFunc
	logger *log.Logger

	retryCount int
}

func NewClient(config *ClientConfig, callback *ClientCallbacks, logger *log.Logger) *Client {
	if callback == nil {
		callback = &ClientCallbacks{}
	}
	if logger == nil {
		logger = log.New(os.Stdout, "[ws-client] ", log.LstdFlags|log.Llongfile)
	}
	ctx, cancel := context.WithCancel(context.Background())

	return &Client{
		config:    config,
		callbacks: callback,
		ctx:       ctx,
		cancel:    cancel,
		logger:    logger,
	}
}

func (c *Client) OnStarted(handler func()) {
	c.callbacks.Started = handler
}

func (c *Client) OnStopped(handler func()) {
	c.callbacks.Stopped = handler
}

func (c *Client) OnConnect(handler func()) {
	c.callbacks.OnConnect = handler
}

func (c *Client) OnDisconnect(handler func(err error)) {
	c.callbacks.OnDisconnect = handler
}

func (c *Client) OnMessage(handler func(msg []byte)) {
	c.callbacks.OnMessage = handler
}

func (c *Client) OnError(handler func(err error)) {
	c.callbacks.OnError = handler
}

func (c *Client) Start() {
	c.startOnce.Do(func() {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.run()
		}()
	})
}

func (c *Client) Stop() {
	c.stopOnce.Do(func() {
		c.cancel()
		c.closeConn()
		c.wg.Wait()
		if c.callbacks.Stopped != nil {
			c.callbacks.Stopped()
		}
	})
}

func (c *Client) Send(msg interface{}) error {
	conn := c.getConn()
	if conn == nil {
		return errors.New("websocket client: not connected")
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	conn.SetWriteDeadline(time.Now().Add(c.config.WriteTimeout))
	if err := conn.WriteJSON(msg); err != nil {
		return err
	}

	conn.SetWriteDeadline(time.Time{})
	return nil
}

func (c *Client) run() {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			err := c.subscribe()
			if err != nil {
				c.logger.Printf("Connection failed (attempt %d/%d): %v", c.retryCount+1, c.config.MaxRetries, err)

				if c.callbacks.OnError != nil {
					c.callbacks.OnError(err)
				}

				c.retryCount++

				if c.retryCount >= c.config.MaxRetries {
					c.logger.Printf("Max retries (%d) exceeded. Stopping client.", c.config.MaxRetries)
					if c.callbacks.OnError != nil {
						c.callbacks.OnError(fmt.Errorf("max retries exceeded: %d", c.config.MaxRetries))
					}
					return
				}

				waitTime := time.Duration(c.retryCount) * c.config.RetryInterval
				c.logger.Printf("Retrying in %v... (attempt %d)", waitTime, c.retryCount)

				select {
				case <-c.ctx.Done():
					return
				case <-time.After(waitTime):
					continue
				}
			} else {
				c.retryCount = 0

				pingCtx, pingCancel := context.WithCancel(c.ctx)
				go c.ping(pingCtx)

				c.read()

				pingCancel()
			}

			c.closeConn()
		}
	}
}

func (c *Client) subscribe() error {
	url := fmt.Sprintf("%s://%s:%s%s", c.config.Scheme, c.config.Host, c.config.Port, c.config.Path)
	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = c.config.HandshakeTimeout
	conn, _, err := dialer.Dial(url, c.config.Headers)
	if err != nil {
		return err
	}

	if c.callbacks.Started != nil {
		c.callbacks.Started()
	}

	c.setConn(conn)

	conn.SetReadLimit(int64(c.config.MaxReadMessageSize))
	conn.SetReadDeadline(time.Now().Add(c.config.ReadTimeout))

	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(c.config.ReadTimeout))
		return nil
	})

	if c.callbacks.OnConnect != nil {
		c.callbacks.OnConnect()
	}

	return nil
}

func (c *Client) ping(ctx context.Context) {
	ticker := time.NewTicker(c.config.ReadTimeout / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			conn := c.getConn()
			if conn != nil {
				c.writeMu.Lock()
				err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(c.config.WriteTimeout))
				c.writeMu.Unlock()
				if err != nil {
					c.logger.Printf("Ping error: %v", err)
					if c.callbacks.OnError != nil {
						c.callbacks.OnError(err)
					}
					return
				}
			}
		}
	}
}

func (c *Client) read() {
	conn := c.getConn()
	if conn == nil {
		return
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			_, msg, err := conn.ReadMessage()
			if err != nil {
				if c.ctx.Err() == nil && c.callbacks.OnDisconnect != nil {
					c.callbacks.OnDisconnect(err)
				}
				return
			}
			if c.callbacks.OnMessage != nil {
				c.callbacks.OnMessage(msg)
			}
		}
	}
}

func (c *Client) setConn(conn *websocket.Conn) {
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
}

func (c *Client) getConn() *websocket.Conn {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.conn
}

func (c *Client) closeConn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutting down normally"))
		_ = c.conn.Close()
		c.conn = nil
	}
}
