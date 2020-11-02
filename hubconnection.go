package signalr

import (
	"bytes"
	"context"
	"github.com/rotisserie/eris"
	"sync"
	"time"
)

// hubConnection is used by HubContext, Server and ClientConnection to realize the external API.
// hubConnection uses a transport connection (of type Connection) and a HubProtocol to send and receive SignalR messages.
type hubConnection interface {
	Start()
	IsConnected() bool
	ConnectionID() string
	Receive() (interface{}, error)
	SendInvocation(id string, target string, args []interface{}) error
	SendStreamInvocation(id string, target string, args []interface{}, streamIds []string) error
	StreamItem(id string, item interface{}) error
	Completion(id string, result interface{}, error string) error
	Close(error string, allowReconnect bool) error
	Ping() error
	LastWriteStamp() time.Time
	Items() *sync.Map
	Context() context.Context
	Abort()
}

func newHubConnection(connection Connection, protocol HubProtocol, maximumReceiveMessageSize uint, info StructuredLogger) hubConnection {
	ctx, cancelFunc := context.WithCancel(connection.Context())
	c := &defaultHubConnection{
		ctx:                       ctx,
		cancelFunc:                cancelFunc,
		protocol:                  protocol,
		mx:                        sync.Mutex{},
		connection:                connection,
		maximumReceiveMessageSize: maximumReceiveMessageSize,
		items:                     &sync.Map{},
		abortChans:                make([]chan error, 0),
		info:                      info,
	}
	// Listen on abort
	go func() {
		<-c.ctx.Done()
		c.mx.Lock()
		if c.connected {
			for _, ch := range c.abortChans {
				go func(ch chan error, err error) {
					ch <- err
				}(ch, eris.Wrap(c.connection.Context().Err(), "connection canceled"))
			}
			c.abortChans = []chan error{}
			c.connected = false
		}
		c.mx.Unlock()
	}()
	return c
}

type defaultHubConnection struct {
	ctx                       context.Context
	cancelFunc                context.CancelFunc
	protocol                  HubProtocol
	mx                        sync.Mutex
	connected                 bool
	abortChans                []chan error
	connection                Connection
	maximumReceiveMessageSize uint
	items                     *sync.Map
	lastWriteStamp            time.Time
	info                      StructuredLogger
}

func (c *defaultHubConnection) Items() *sync.Map {
	return c.items
}

func (c *defaultHubConnection) Start() {
	defer c.mx.Unlock()
	c.mx.Lock()
	c.connected = true
}

func (c *defaultHubConnection) IsConnected() bool {
	defer c.mx.Unlock()
	c.mx.Lock()
	return c.connected
}

func (c *defaultHubConnection) Close(errorText string, allowReconnect bool) error {
	var closeMessage = closeMessage{
		Type:           7,
		Error:          errorText,
		AllowReconnect: allowReconnect,
	}
	return c.protocol.WriteMessage(closeMessage, c.connection)
}

func (c *defaultHubConnection) ConnectionID() string {
	return c.connection.ConnectionID()
}

func (c *defaultHubConnection) Context() context.Context {
	return c.ctx
}

func (c *defaultHubConnection) Abort() {
	c.cancelFunc()
}

type receiveResult struct {
	message interface{}
	err     error
}

func (c *defaultHubConnection) Receive() (interface{}, error) {
	if c.ctx.Err() != nil {
		return nil, eris.Wrap(c.ctx.Err(), "hubConnection canceled")
	}
	recvResCh := make(chan receiveResult, 1)
	go func(chan receiveResult) {
		var buf bytes.Buffer
		var data = make([]byte, c.maximumReceiveMessageSize)
		var n int
		for {
			if message, complete, parseErr := c.protocol.ReadMessage(&buf); !complete {
				// Partial message, need more data
				// ReadMessage read data out of the buf, so its gone there: refill
				buf.Write(data[:n])
				readResCh := make(chan receiveResult, 1)
				go func(chan receiveResult) {
					var readErr error
					n, readErr = c.connection.Read(data)
					if readErr == nil {
						buf.Write(data[:n])
					}
					readResCh <- receiveResult{n, readErr}
					close(readResCh)
				}(readResCh)
				select {
				case readRes := <-readResCh:
					if readRes.err != nil {
						c.Abort()
						recvResCh <- readRes
						close(recvResCh)
						return
					}
					n = readRes.message.(int)
				case <-c.ctx.Done():
					recvResCh <- receiveResult{err: eris.Wrap(c.ctx.Err(), "hubConnection canceled")}
					close(recvResCh)
					return
				}
			} else {
				recvResCh <- receiveResult{message, parseErr}
				close(recvResCh)
				return
			}
		}
	}(recvResCh)
	select {
	case recvRes := <-recvResCh:
		return recvRes.message, recvRes.err
	case <-c.ctx.Done():
		return nil, eris.Wrap(c.ctx.Err(), "hubConnection canceled")
	}
}

func (c *defaultHubConnection) SendInvocation(id string, target string, args []interface{}) error {
	var invocationMessage = invocationMessage{
		Type:         1,
		InvocationID: id,
		Target:       target,
		Arguments:    args,
	}
	return c.writeMessage(invocationMessage)
}

func (c *defaultHubConnection) SendStreamInvocation(id string, target string, args []interface{}, streamIds []string) error {
	var invocationMessage = invocationMessage{
		Type:         4,
		InvocationID: id,
		Target:       target,
		Arguments:    args,
		StreamIds:    streamIds,
	}
	return c.writeMessage(invocationMessage)
}

func (c *defaultHubConnection) StreamItem(id string, item interface{}) error {
	var streamItemMessage = streamItemMessage{
		Type:         2,
		InvocationID: id,
		Item:         item,
	}
	return c.writeMessage(streamItemMessage)
}

func (c *defaultHubConnection) Completion(id string, result interface{}, error string) error {
	var completionMessage = completionMessage{
		Type:         3,
		InvocationID: id,
		Result:       result,
		Error:        error,
	}
	return c.writeMessage(completionMessage)
}

func (c *defaultHubConnection) Ping() error {
	var pingMessage = hubMessage{
		Type: 6,
	}
	return c.writeMessage(pingMessage)
}

func (c *defaultHubConnection) LastWriteStamp() time.Time {
	return c.lastWriteStamp
}

func (c *defaultHubConnection) writeMessage(message interface{}) error {
	c.mx.Lock()
	c.lastWriteStamp = time.Now()
	c.mx.Unlock()
	err := func() error {
		if !c.IsConnected() {
			return eris.Wrap(c.ctx.Err(), "hubConnection canceled")
		}
		e := make(chan error, 1)
		go func() { e <- c.protocol.WriteMessage(message, c.connection) }()
		select {
		case <-c.ctx.Done():
			// Wait for WriteMessage to return
			<-e
			return eris.Wrap(c.ctx.Err(), "hubConnection canceled")
		case err := <-e:
			if err != nil {
				c.Abort()
			}
			return err
		}
	}()
	if err != nil {
		_ = c.info.Log(evt, msgSend, "message", fmtMsg(message), "error", err)
	}
	return err
}
