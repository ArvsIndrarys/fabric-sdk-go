/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package client

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyperledger/fabric-sdk-go/pkg/context/api/fab"
	"github.com/hyperledger/fabric-sdk-go/pkg/fab/events/client/dispatcher"
	eventservice "github.com/hyperledger/fabric-sdk-go/pkg/fab/events/service"
	"github.com/hyperledger/fabric-sdk-go/pkg/logging"
	"github.com/hyperledger/fabric-sdk-go/pkg/options"
	"github.com/pkg/errors"
)

var logger = logging.NewLogger("fabric_sdk_go")

// ConnectionState is the state of the client connection
type ConnectionState int32

const (
	// Disconnected indicates that the client is disconnected from the server
	Disconnected ConnectionState = iota
	// Connecting indicates that the client is in the process of establishing a connection
	Connecting
	// Connected indicates that the client is connected to the server
	Connected
)

// Client connects to an event server and receives events, such as block, filtered block,
// chaincode, and transaction status events. Client also monitors the connection to the
// event server and attempts to reconnect if the connection is closed.
type Client struct {
	eventservice.Service
	params
	sync.RWMutex
	connEvent         chan *fab.ConnectionEvent
	connectionState   int32
	stopped           int32
	registerOnce      sync.Once
	permitBlockEvents bool
	afterConnect      handler
	beforeReconnect   handler
}

type handler func() error

// New returns a new event client
func New(permitBlockEvents bool, dispatcher eventservice.Dispatcher, opts ...options.Opt) *Client {
	params := defaultParams()
	options.Apply(params, opts)

	return &Client{
		Service:           *eventservice.New(dispatcher, opts...),
		params:            *params,
		connEvent:         make(chan *fab.ConnectionEvent),
		connectionState:   int32(Disconnected),
		permitBlockEvents: permitBlockEvents,
	}
}

// SetAfterConnectHandler registers a handler that is called
// after the client connects to the event server. This allows for
// custom code to be executed for a particular
// event client implementation.
func (c *Client) SetAfterConnectHandler(h handler) {
	c.Lock()
	defer c.Unlock()
	c.afterConnect = h
}

func (c *Client) afterConnectHandler() handler {
	c.RLock()
	defer c.RUnlock()
	return c.afterConnect
}

// SetBeforeReconnectHandler registers a handler that will be called
// before retrying to reconnect to the event server. This allows for
// custom code to be executed for a particular event client implementation.
func (c *Client) SetBeforeReconnectHandler(h handler) {
	c.Lock()
	defer c.Unlock()
	c.beforeReconnect = h
}

func (c *Client) beforeReconnectHandler() handler {
	c.RLock()
	defer c.RUnlock()
	return c.beforeReconnect
}

// Connect connects to the peer and registers for events on a particular channel.
func (c *Client) Connect() error {
	if c.maxConnAttempts == 1 {
		return c.connect()
	}
	return c.connectWithRetry(c.maxConnAttempts, c.timeBetweenConnAttempts)
}

// Close closes the connection to the event server and deallocates all resources.
// Once this function is invoked the client may no longer be used.
func (c *Client) Close() {
	logger.Debugf("Attempting to close event client...")

	if !c.setStoppped() {
		// Already stopped
		logger.Debugf("Client already stopped")
		return
	}

	logger.Debugf("Stopping client...")

	if c.connEventCh != nil {
		close(c.connEventCh)
	}

	logger.Debugf("Sending disconnect request...")

	errch := make(chan error)
	c.Submit(dispatcher.NewDisconnectEvent(errch))
	err := <-errch

	if err != nil {
		logger.Warnf("Received error from disconnect request: %s", err)
	} else {
		logger.Debugf("Received success from disconnect request")
	}

	logger.Debugf("Stopping dispatcher...")

	c.Stop()

	c.mustSetConnectionState(Disconnected)

	logger.Debugf("... event client is stopped")
}

func (c *Client) connect() error {
	if c.Stopped() {
		return errors.New("event client is closed")
	}

	if !c.setConnectionState(Disconnected, Connecting) {
		return errors.Errorf("unable to connect event client since client is [%s]. Expecting client to be in state [%s]", c.ConnectionState(), Disconnected)
	}

	logger.Debugf("Submitting connection request...")

	errch := make(chan error)
	c.Submit(dispatcher.NewConnectEvent(errch))

	err := <-errch

	if err != nil {
		c.mustSetConnectionState(Disconnected)
		logger.Debugf("... got error in connection response: %s", err)
		return err
	}

	c.registerOnce.Do(func() {
		logger.Debugf("Submitting connection event registration...")
		_, eventch, err := c.RegisterConnectionEvent()
		if err != nil {
			logger.Errorf("Error registering for connection events: %s", err)
			c.Close()
		}
		c.connEvent = eventch
		go c.monitorConnection()
	})

	handler := c.afterConnectHandler()
	if handler != nil {
		if err := handler(); err != nil {
			logger.Warnf("Error invoking afterConnect handler: %s. Disconnecting...", err)

			c.Submit(dispatcher.NewDisconnectEvent(errch))

			select {
			case disconnErr := <-errch:
				if disconnErr != nil {
					logger.Warnf("Received error from disconnect request: %s", disconnErr)
				} else {
					logger.Debugf("Received success from disconnect request")
				}
			case <-time.After(c.respTimeout):
				logger.Warnf("Timed out waiting for disconnect response")
			}

			c.setConnectionState(Connecting, Disconnected)

			return errors.WithMessage(err, "error invoking afterConnect handler")
		}
	}

	c.setConnectionState(Connecting, Connected)

	logger.Debugf("Submitting connected event")
	c.Submit(dispatcher.NewConnectedEvent())

	return err
}

func (c *Client) connectWithRetry(maxAttempts uint, timeBetweenAttempts time.Duration) error {
	if c.Stopped() {
		return errors.New("event client is closed")
	}
	if timeBetweenAttempts < time.Second {
		timeBetweenAttempts = time.Second
	}

	var attempts uint
	for {
		attempts++
		logger.Debugf("Attempt #%d to connect...", attempts)
		if err := c.connect(); err != nil {
			logger.Warnf("... connection attempt failed: %s", err)
			if maxAttempts > 0 && attempts >= maxAttempts {
				logger.Warnf("maximum connect attempts exceeded")
				return errors.New("maximum connect attempts exceeded")
			}
			time.Sleep(timeBetweenAttempts)
		} else {
			logger.Debugf("... connect succeeded.")
			return nil
		}
	}
}

// RegisterBlockEvent registers for block events. If the client is not authorized to receive
// block events then an error is returned.
func (c *Client) RegisterBlockEvent(filter ...fab.BlockFilter) (fab.Registration, <-chan *fab.BlockEvent, error) {
	if !c.permitBlockEvents {
		return nil, nil, errors.New("block events are not permitted")
	}
	return c.Service.RegisterBlockEvent(filter...)
}

// RegisterConnectionEvent registers a connection event. The returned
// ConnectionEvent channel will be called whenever the client clients or disconnects
// from the event server
func (c *Client) RegisterConnectionEvent() (fab.Registration, chan *fab.ConnectionEvent, error) {
	if c.Stopped() {
		return nil, nil, errors.New("event client is closed")
	}

	eventch := make(chan *fab.ConnectionEvent, c.eventConsumerBufferSize)
	errch := make(chan error)
	regch := make(chan fab.Registration)
	c.Submit(dispatcher.NewRegisterConnectionEvent(eventch, regch, errch))

	select {
	case reg := <-regch:
		return reg, eventch, nil
	case err := <-errch:
		return nil, nil, err
	}
}

// Stopped returns true if the client has been stopped (disconnected)
// and is no longer usable.
func (c *Client) Stopped() bool {
	return atomic.LoadInt32(&c.stopped) == 1
}

func (c *Client) setStoppped() bool {
	return atomic.CompareAndSwapInt32(&c.stopped, 0, 1)
}

// ConnectionState returns the connection state
func (c *Client) ConnectionState() ConnectionState {
	return ConnectionState(atomic.LoadInt32(&c.connectionState))
}

// setConnectionState sets the connection state only if the given currentState
// matches the actual state. True is returned if the connection state was successfully set.
func (c *Client) setConnectionState(currentState, newState ConnectionState) bool {
	return atomic.CompareAndSwapInt32(&c.connectionState, int32(currentState), int32(newState))
}

func (c *Client) mustSetConnectionState(newState ConnectionState) {
	atomic.StoreInt32(&c.connectionState, int32(newState))
}

func (c *Client) monitorConnection() {
	logger.Debugf("Monitoring connection")
	for {
		event, ok := <-c.connEvent
		if !ok {
			logger.Debugln("Connection has closed.")
			break
		}

		if c.Stopped() {
			logger.Debugln("Event client has been stopped.")
			break
		}

		if c.connEventCh != nil {
			logger.Debugln("Sending connection event to subscriber.")
			c.connEventCh <- event
		}

		if event.Connected {
			logger.Debugf("Event client has connected")
		} else if c.reconn {
			logger.Warnf("Event client has disconnected. Details: %s", event.Err)
			if c.setConnectionState(Connected, Disconnected) {
				logger.Warnf("Attempting to reconnect...")
				go c.reconnect()
			} else if c.setConnectionState(Connecting, Disconnected) {
				logger.Warnf("Reconnect already in progress. Setting state to disconnected")
			}
		} else {
			logger.Debugf("Event client has disconnected. Terminating: %s", event.Err)
			go c.Close()
			break
		}
	}
	logger.Debugf("Exiting connection monitor")
}

func (c *Client) reconnect() {
	logger.Debugf("Waiting %s before attempting to reconnect event client...", c.reconnInitialDelay)
	time.Sleep(c.reconnInitialDelay)

	logger.Debugf("Attempting to reconnect event client...")

	handler := c.beforeReconnectHandler()
	if handler != nil {
		if err := handler(); err != nil {
			logger.Errorf("Error invoking beforeReconnect handler: %s", err)
			return
		}
	}

	if err := c.connectWithRetry(c.maxReconnAttempts, c.timeBetweenConnAttempts); err != nil {
		logger.Warnf("Could not reconnect event client: %s. Closing.", err)
		c.Close()
	}
}

func (s ConnectionState) String() string {
	switch s {
	case Disconnected:
		return "Disconnected"
	case Connected:
		return "Connected"
	case Connecting:
		return "Connecting"
	default:
		return "undefined"
	}
}
