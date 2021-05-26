package rrpc

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
)

/*
* Convenience types and functions to connect to RPC server and create a
* RPC client. For more advanaced use cases (such as ), we should
* use other packages (such as "net", "io") to set up a connection
* (a ReadWriteCloser) to server first, then use NewClient(conn, EncDecoder)
* or NewClientWithCodec(codec) to create RPC clients.
 */

// Dialer is RPC clients factory: dials RPC server for connections and create RPC clients
type Dialer func(net.Conn) *Client

// DefaultDialer uses go encode/decoder
var DefaultDialer = Dialer(func(conn net.Conn) *Client {
	return NewClient(conn, GobEncDecoder)
})

// DialHTTP connects to an HTTP RPC server at the specified network address
// listening on the default HTTP RPC path.
func (d Dialer) DialHTTP(network, address string) (*Client, error) {
	return d.DialHTTPPath(network, address, DefaultRPCPath)
}

// DialHTTPPath connects to an HTTP RPC server
// at the specified network address and path, and return a RPC client
func (d Dialer) DialHTTPPath(network, address, path string) (*Client, error) {
	conn, err := DialHTTPPathForConn(network, address, path)
	if err != nil {
		return nil, err
	}
	return d(conn), nil
}

// Dial connects to an RPC server at the specified network address.
func (d Dialer) Dial(network, address string) (*Client, error) {
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}
	return d(conn), nil
}

// DialHTTP connects to an HTTP RPC server at the specified network address
// listening on the default HTTP RPC path.
func DialHTTP(network, address string) (*Client, error) {
	return DefaultDialer.DialHTTP(network, address)
}

// DialHTTPPath connects to an HTTP RPC server
// at the specified network address and path, and return a RPC client
func DialHTTPPath(network, address, path string) (*Client, error) {
	return DefaultDialer.DialHTTPPath(network, address, path)
}

// Dial connects to an RPC server at the specified network address.
func Dial(network, address string) (*Client, error) {
	return DefaultDialer.Dial(network, address)
}

// DialHTTPPathForConn connects to an HTTP RPC server
// at the specified network address and path, and return a conn to run RPC protocol
// paired with server side func HijackHTTPConn()
func DialHTTPPathForConn(network, address, path string) (net.Conn, error) {
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}
	io.WriteString(conn, "CONNECT "+path+" HTTP/1.0\n\n")

	// Require successful HTTP response
	// before switching to RPC protocol.
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err == nil && resp.Status == connected {
		return conn, nil
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
