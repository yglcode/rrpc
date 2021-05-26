package rrpc

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
)

/*
*  Conveniences types and functions for accepting remote client connections
* and serving RPC protocols. For more advanaced use cases (such as ), we should
* use other packages (such as "net", "io") to set up a connection (a ReadWriteCloser)
* with remote client first, then use Server.ServeConn(conn, EncDecoder) or
* Server.ServeCodec(codec) to serve RPC protocol directly.
 */

const (
	// Defaults used by HandleHTTP
	DefaultRPCPath   = "/_goRPC_"
	DefaultDebugPath = "/debug/rpc"
)

// Acceptor accepts and handle each RPC connection from clients
type Acceptor func(net.Conn)

// NewAcceptor returns a new Acceptor to accept net.Conn to run RPC protocol.
func NewAcceptor(f func(net.Conn)) Acceptor {
	return Acceptor(f)
}

// DefaultAcceptor uses DefaultServer and gob encoder/decoder.
var DefaultAcceptor = Acceptor(func(conn net.Conn) {
	DefaultServer.ServeConn(conn, GobEncDecoder)
})

// Close shutdown rpc default server
func Close() {
	DefaultServer.Close()
}

// Accept accepts connections on the listener and serves requests
// for each incoming connection. Accept blocks until the listener
// returns a non-nil error. The caller typically invokes Accept in a
// go statement.
func (acpt Acceptor) Accept(lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			log.Print("rpc.Serve: accept:", err.Error())
			return
		}
		go acpt(conn)
	}
}

// Accept accepts connections on the listener and serves requests
// to DefaultServer for each incoming connection.
// Accept blocks; the caller typically invokes it in a go statement.
func Accept(lis net.Listener) { DefaultAcceptor.Accept(lis) }

// Can connect to RPC service using HTTP CONNECT to rpcPath.
var connected = "200 Connected to Go RPC"

// ServeHTTP implements a http.Handler that hijacks http conn to run RPC protocol.
func (acpt Acceptor) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if conn, err := HijackHTTPConn(w, req); err == nil {
		acpt(conn)
	}
}

// HijackHTTPConn hijacks a http conn to run RPC protocol,
// paired with client side func DialHTTPPathForConn()
func HijackHTTPConn(w http.ResponseWriter, req *http.Request) (net.Conn, error) {
	if req.Method != "CONNECT" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		io.WriteString(w, "405 must CONNECT\n")
		return nil, errors.New("405 must CONNECT")
	}
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		log.Print("rpc hijacking ", req.RemoteAddr, ": ", err.Error())
		return nil, err
	}
	io.WriteString(conn, "HTTP/1.0 "+connected+"\n\n")
	return conn, nil
}

// HandleHTTP registers an HTTP handler for RPC messages on rpcPath,
// and a debugging handler on debugPath.
// It is still necessary to invoke http.Serve(), typically in a go statement.
func (acpt Acceptor) HandleHTTP(rpcPath string) {
	http.Handle(rpcPath, acpt)
}

// HTTPDebugServer registers a HTTP debugging handler for server on debugPath
func HTTPDebugServer(debugPath string, server *Server) {
	http.Handle(debugPath, debugHTTP{server})
}

// HandleHTTP registers an HTTP handler for RPC messages to DefaultServer
// on DefaultRPCPath and a debugging handler on DefaultDebugPath.
// It is still necessary to invoke http.Serve(), typically in a go statement.
func HandleHTTP() {
	DefaultAcceptor.HandleHTTP(DefaultRPCPath)
	HTTPDebugServer(DefaultDebugPath, DefaultServer)
}
