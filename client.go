// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

package rrpc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// ServerError represents an error that has been returned from
// the remote side of the RPC connection.
type ServerError string

func (e ServerError) Error() string {
	return string(e)
}

var ErrShutdown = errors.New("connection is shut down")

// Call represents an active RPC.
type Call struct {
	ServiceMethod   string          // The name of the service and method to call.
	Args            interface{}     // The argument to the function (*struct).
	Reply           interface{}     // The reply from the function (*struct).
	Error           error           // After completion, the error status.
	Done            chan *Call      // Receives *Call when Go is complete.
	ctx             context.Context //If not nil, RPC will be run with context Ctx
	ctxCancelerStop chan struct{}   //Context cancel watcher exit signal
}

// Client represents an RPC Client.
// There may be multiple outstanding Calls associated
// with a single Client, and a Client may be used by
// multiple goroutines simultaneously.
type Client struct {
	conndrv *connDriver

	reqMutex sync.Mutex // protects following
	header   Header

	mutex    sync.Mutex // protects following
	seq      uint64
	pending  map[uint64]*Call
	closing  bool // user has called Close
	shutdown bool // server has told us to stop
}

func newClient(cdrv *connDriver) *Client {
	return &Client{
		conndrv: cdrv,
		pending: make(map[uint64]*Call),
	}
}

func (client *Client) send(call *Call) {
	isCancel := call.Reply == nil && call.ServiceMethod == "cancel"

	client.reqMutex.Lock()
	defer client.reqMutex.Unlock()

	// Register this call.
	client.mutex.Lock()
	if client.shutdown || client.closing {
		client.mutex.Unlock()
		call.Error = ErrShutdown
		call.done()
		if debugLog {
			log.Println("drop call when client close")
		}
		return
	}
	seq := client.seq
	client.seq++
	if !isCancel {
		//Cancel is oneway
		client.pending[seq] = call
	}
	client.mutex.Unlock()

	// Encode and send the request.
	client.header.Seq = seq
	client.header.Kind = Request
	if isCancel {
		client.header.Kind = Cancel
		//send call seq number to be canceled in header.Info
		client.header.Info = call.Args
		call.Args = emptyBody
	} else if call.ctx != nil {
		client.header.Kind = RequestWithContext
		deadline, hasDeadline := call.ctx.Deadline()
		if !hasDeadline {
			deadline = time.Time{} //set it to zero val
		}
		//pass deadline info thru Header
		client.header.Info = deadline.Format(time.RFC3339)
	}
	client.header.ServiceMethod = call.ServiceMethod
	if debugLog {
		log.Printf("%p cli send req: %s\n", client, &client.header)
	}
	client.conndrv.wlock.Lock()
	err := client.conndrv.codec.WriteHeaderBody(&client.header, call.Args)
	client.conndrv.wlock.Unlock()
	if err != nil {
		if debugLog {
			log.Printf("fail to send req: %s, err: %s\n", &client.header, err)
		}
		client.mutex.Lock()
		//call = client.pending[seq]
		delete(client.pending, seq)
		client.mutex.Unlock()
		//if call != nil {
		call.Error = err
		call.done()
		//}
	} else if isCancel {
		//since no one will handle cancel msg failure
		//just return right away
		call.done()
	}
	//for method-call with context, monitor its cancel signal
	if call.ctx != nil && call.ctx.Done() != nil {
		go func() {
			//wait for cancel signal
			//it could comes from local cancel or remote completion
			select {
			case <-call.ctx.Done():
				//local cancelation, send cancel msg
				client.Call("cancel", seq, nil)
			case <-call.ctxCancelerStop:
				//completion, just exit
				if debugLog {
					log.Println("job complete, canceler exit")
				}
			}
		}()
	}
}

var emptyBody = struct{}{}

// handleResponse handles one response/error forwarded by connDriver
func (client *Client) handleResponse(codec Codec, header *Header) (err error) {
	seq := header.Seq
	client.mutex.Lock()
	call := client.pending[seq]
	delete(client.pending, seq)
	client.mutex.Unlock()

	if debugLog {
		log.Printf("%p cli recv: %s\n", client, header)
	}

	switch {
	case call == nil:
		// We've got no pending call. That usually means that
		// WriteRequest partially failed, and call was already
		// removed; response is a server telling us about an
		// error reading request body. We should still attempt
		// to read error body, but there's no one to give it to.
		err = codec.ReadBody(nil)
		if err != nil {
			err = errors.New("reading error body: " + err.Error())
		}
		if debugLog {
			log.Printf("%p: no call for: %s\n", client, header)
		}
	case header.Kind == Error:
		// We've got an error response. Give this to the request;
		// any subsequent requests will get the ReadResponseBody
		// error if there is one.
		call.Error = ServerError(header.Info.(string))
		err = codec.ReadBody(nil)
		if err != nil {
			err = errors.New("reading error body: " + err.Error())
		}
		if call.ctxCancelerStop != nil {
			//let context cancel watcher exit
			close(call.ctxCancelerStop)
		}
		call.done()
	default:
		err = codec.ReadBody(call.Reply)
		if err != nil {
			call.Error = errors.New("reading body " + err.Error())
		}
		if call.ctxCancelerStop != nil {
			//let context cancel watcher exit
			close(call.ctxCancelerStop)
		}
		call.done()
	}
	if debugLog && err != nil {
		log.Printf("%p cli handleResp err: %v\n", client, err)
	}
	return
}

func (call *Call) done() {
	select {
	case call.Done <- call:
		// ok
	default:
		// We don't want to block here. It is the caller's responsibility to make
		// sure the channel has enough buffer space. See comment in Go().
		if debugLog {
			log.Println("rpc: discarding Call reply due to insufficient Done chan capacity")
		}
	}
}

// NewClient returns a new Client to handle requests to the
// set of services at the other end of the connection.
// It adds a buffer to the write side of the connection so
// the header and payload are sent as a unit.
//
// The read and write halves of the connection are serialized independently,
// so no interlocking is required. However each half may be accessed
// concurrently so the implementation of conn should protect against
// concurrent reads or concurrent writes.
func NewClient(conn io.ReadWriteCloser) *Client {
	return NewClientWithCodec(NewDefaultCodec(conn))
}

// NewClientWithCodec is like NewClient but uses the specified
// codec to encode requests and decode responses.
func NewClientWithCodec(codec Codec) *Client {
	conndrv := newConnDriver(codec)
	conndrv.client = newClient(conndrv)
	go conndrv.Loop()
	return conndrv.client
}

// Close calls the underlying codec's Close method. If the connection is already
// shutting down, ErrShutdown is returned.
func (client *Client) Close() error {
	client.mutex.Lock()
	if client.closing {
		client.mutex.Unlock()
		return ErrShutdown
	}
	client.closing = true
	client.mutex.Unlock()
	return client.conndrv.Close()
}

func (client *Client) connShutdown(err error) {
	// Terminate pending calls.
	client.reqMutex.Lock()
	client.mutex.Lock()
	client.shutdown = true
	closing := client.closing
	if err == io.EOF {
		if closing {
			err = ErrShutdown
		} else {
			err = io.ErrUnexpectedEOF
		}
	}
	for _, call := range client.pending {
		call.Error = err
		if call.ctxCancelerStop != nil {
			//let context cancel watcher exit
			close(call.ctxCancelerStop)
		}
		call.done()
	}
	client.mutex.Unlock()
	client.reqMutex.Unlock()
	if debugLog && err != io.EOF && !closing {
		log.Println("*** rpc: client shutdown, protocol error:", err)
	}
}

// Go invokes the function asynchronously. It returns the Call structure representing
// the invocation. The done channel will signal when the call is complete by returning
// the same Call object. If done is nil, Go will allocate a new channel.
// If non-nil, done must be buffered or Go will deliberately crash.
func (client *Client) Go(serviceMethod string, args interface{}, reply interface{}, done chan *Call) *Call {
	call := new(Call)
	call.ServiceMethod = serviceMethod
	call.Args = args
	call.Reply = reply
	if done == nil {
		done = make(chan *Call, 10) // buffered.
	} else {
		// If caller passes done != nil, it must arrange that
		// done has enough buffer for the number of simultaneous
		// RPCs that will be using that channel. If the channel
		// is totally unbuffered, it's best not to run at all.
		if cap(done) == 0 {
			log.Panic("rpc: done channel is unbuffered")
		}
	}
	call.Done = done
	client.send(call)
	return call
}

// Call invokes the named function, waits for it to complete, and returns its error status.
func (client *Client) Call(serviceMethod string, args interface{}, reply interface{}) error {
	call := <-client.Go(serviceMethod, args, reply, make(chan *Call, 1)).Done
	return call.Error
}

// GoWithContext invokes the function asynchronously. It returns the Call structure representing
// the invocation. The done channel will signal when the call is complete by returning
// the same Call object. If done is nil, Go will allocate a new channel.
// If non-nil, done must be buffered or Go will deliberately crash.
// A context.Context object is passed to support timeout and cancelation.
func (client *Client) GoWithContext(serviceMethod string, ctx context.Context, args interface{}, reply interface{}, done chan *Call) *Call {
	call := new(Call)
	call.ServiceMethod = serviceMethod
	call.ctx = ctx
	call.ctxCancelerStop = make(chan struct{})
	call.Args = args
	call.Reply = reply
	if done == nil {
		done = make(chan *Call, 10) // buffered.
	} else {
		// If caller passes done != nil, it must arrange that
		// done has enough buffer for the number of simultaneous
		// RPCs that will be using that channel. If the channel
		// is totally unbuffered, it's best not to run at all.
		if cap(done) == 0 {
			log.Panic("rpc: done channel is unbuffered")
		}
	}
	call.Done = done
	client.send(call)
	return call
}

// CallWithContext invokes the named function, waits for it to complete, and returns its error status.
// A context.Context object is passed to support timeout and cancelation.
func (client *Client) CallWithContext(serviceMethod string, ctx context.Context, args interface{}, reply interface{}) error {
	call := <-client.GoWithContext(serviceMethod, ctx, args, reply, make(chan *Call, 1)).Done
	return call.Error
}

// DialHTTP connects to an HTTP RPC server at the specified network address
// listening on the default HTTP RPC path.
func DialHTTP(network, address string) (*Client, error) {
	return DialHTTPPath(network, address, DefaultRPCPath)
}

// DialHTTPPath connects to an HTTP RPC server
// at the specified network address and path.
func DialHTTPPath(network, address, path string) (*Client, error) {
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}
	io.WriteString(conn, "CONNECT "+path+" HTTP/1.0\n\n")

	// Require successful HTTP response
	// before switching to RPC protocol.
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err == nil && resp.Status == connected {
		return NewClient(conn), nil
	}
	if err == nil {
		err = errors.New("unexpected HTTP response: " + resp.Status)
	}
	conn.Close()
	return nil, &net.OpError{
		Op:   "dial-http",
		Net:  network + " " + address,
		Addr: nil,
		Err:  err,
	}
}

// Dial connects to an RPC server at the specified network address.
func Dial(network, address string) (*Client, error) {
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}
	return NewClient(conn), nil
}
