package rrpc

/*
* BiDirection RPC: there are both client and server at each end of same connection.
* The server at each end expose its set of methods and services, while clients at
* the other end can call these services.
 */

import (
	"log"
	"net"
)

// BiDirectionSession is one end of bidirectional rpc over a conn/encdecoder,
// have both client and server
type BiDirectionSession struct {
	server *Server
	encdec EncDecoder
}

// NewBiDirectionSession creates a new bidirectional rpc session
func NewBiDirectionSession(s *Server, c EncDecoder) *BiDirectionSession {
	if s == nil {
		s = DefaultServer
	}
	if c == nil {
		c = GobEncDecoder
	}
	return &BiDirectionSession{s, c}
}

// DefaultBiDirectionSession uses DefaultServer and GobEncDecoder
var DefaultBiDirectionSession = &BiDirectionSession{DefaultServer, GobEncDecoder}

// Dial connects to remote server and setup bidirection rpc, return client to
// invoke remote services and serve calls from remote client with bidir.server
func (bidir *BiDirectionSession) Dial(network, address string) (*Client, error) {
	conn, err := net.Dial(network, address)
	if err != nil {
		log.Print("rpc.BiDirection: dial:", err.Error())
		return nil, err
	}
	return bidir.Connect(conn), nil
}

// Accept accept remote connection and setup bidirection rpc, return client to
// invoke remote services and serve calls from remote client with bidir.server
func (bidir *BiDirectionSession) AcceptOne(lis net.Listener) (*Client, error) {
	conn, err := lis.Accept()
	if err != nil {
		log.Print("rpc.BiDirection: accept:", err.Error())
		return nil, err
	}
	return bidir.Connect(conn), nil
}

// Connect connects bidir.server and remote connection to setup bidirectional rpc,
// return client for invoking remote services
func (bidir *BiDirectionSession) Connect(conn net.Conn) *Client {
	codec := NewCodec(conn, bidir.encdec)
	return ConnBiDirectionCodec(bidir.server, codec)
}

// Connect connects bidir.server and remote connection to setup bidirectional rpc
// return client for invoking remote services
func ConnBiDirectionCodec(srv *Server, codec Codec) *Client {
	conndrv := newConnDriver(codec)
	conndrv.server = srv
	conndrv.client = newClient(conndrv)
	go conndrv.Loop()
	return conndrv.client
}

// Dial connects to remote server and setup bidirection rpc, return client to
// invoke remote services and serve calls from remote client with bidir.server
func DialBiDirection(network, address string) (*Client, error) {
	return DefaultBiDirectionSession.Dial(network, address)
}

// Accept accept remote connection and setup bidirection rpc, return client to
// invoke remote services and serve calls from remote client with bidir.server
func AcceptBiDirection(lis net.Listener) (*Client, error) {
	return DefaultBiDirectionSession.AcceptOne(lis)
}
