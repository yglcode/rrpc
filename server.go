// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
	Package rrpc is a fork of standard library net/rpc, which has been frozen.
        It provides access to the exported methods of an object across a
	network or other I/O connection.  A server registers an object, making it visible
	as a service with the name of the type of the object.  After registration, exported
	methods of the object will be accessible remotely.  A server may register multiple
	objects (services) of different types but it is an error to register multiple
	objects of the same type.

	Only methods that satisfy these criteria will be made available for remote access;
	other methods will be ignored:

		- the method's type is exported.
		- the method is exported.
		- the method has two arguments, both exported (or builtin) types.
		- the method's second argument is a pointer.
		- the method has return type error.

	In effect, the method must look schematically like

		func (t *T) MethodName(argType T1, replyType *T2) error

	where T1 and T2 can be marshaled by encoding/gob.
	These requirements apply even if a different codec is used.
	(In the future, these requirements may soften for custom codecs.)

	The method's first argument represents the arguments provided by the caller; the
	second argument represents the result parameters to be returned to the caller.
	The method's return value, if non-nil, is passed back as a string that the client
	sees as if created by errors.New.  If an error is returned, the reply parameter
	will not be sent back to the client.

	The server may handle requests on a single connection by calling ServeConn.  More
	typically it will create a network listener and call Accept or, for an HTTP
	listener, HandleHTTP and http.Serve.

	A client wishing to use the service establishes a connection and then invokes
	NewClient on the connection.  The convenience function Dial (DialHTTP) performs
	both steps for a raw network connection (an HTTP connection).  The resulting
	Client object has two methods, Call and Go, that specify the service and method to
	call, a pointer containing the arguments, and a pointer to receive the result
	parameters.

	The Call method waits for the remote call to complete while the Go method
	launches the call asynchronously and signals completion using the Call
	structure's Done channel.

	Unless an explicit codec is set up, package encoding/gob is used to
	transport the data.

	Here is a simple example.  A server wishes to export an object of type Arith:

		package server

		import "errors"

		type Args struct {
			A, B int
		}

		type Quotient struct {
			Quo, Rem int
		}

		type Arith int

		func (t *Arith) Multiply(args *Args, reply *int) error {
			*reply = args.A * args.B
			return nil
		}

		func (t *Arith) Divide(args *Args, quo *Quotient) error {
			if args.B == 0 {
				return errors.New("divide by zero")
			}
			quo.Quo = args.A / args.B
			quo.Rem = args.A % args.B
			return nil
		}

	The server calls (for HTTP service):

		arith := new(Arith)
		rrpc.Register(arith)
		rrpc.HandleHTTP()
		l, e := net.Listen("tcp", ":1234")
		if e != nil {
			log.Fatal("listen error:", e)
		}
		go http.Serve(l, nil)

	At this point, clients can see a service "Arith" with methods "Arith.Multiply" and
	"Arith.Divide".  To invoke one, a client first dials the server:

		client, err := rrpc.DialHTTP("tcp", serverAddress + ":1234")
		if err != nil {
			log.Fatal("dialing:", err)
		}

	Then it can make a remote call:

		// Synchronous call
		args := &server.Args{7,8}
		var reply int
		err = client.Call("Arith.Multiply", args, &reply)
		if err != nil {
			log.Fatal("arith error:", err)
		}
		fmt.Printf("Arith: %d*%d=%d", args.A, args.B, reply)

	or

		// Asynchronous call
		quotient := new(Quotient)
		divCall := client.Go("Arith.Divide", args, quotient, nil)
		replyCall := <-divCall.Done	// will be equal to divCall
		// check errors, print, etc.

	A server implementation will often provide a simple, type-safe wrapper for the
	client.

	rrpc allows you use different encoder/decoder for default connection setup process 
        (Dial, DialHTTP, Accept, HandleHTTP) by setting NewDefaultCodec function.
        The default is Gob encoder/decoder, a JSON based encoder/decoder is provided.

        rrpc also supports bidirectional rpc over the same connection,
        where there are servers active at both ends of connection and serving clients at other end.
*/
package rrpc

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
)

const (
	// Defaults used by HandleHTTP
	DefaultRPCPath   = "/_goRPC_"
	DefaultDebugPath = "/debug/rpc"
)

// Precompute the reflect type for error. Can't use error directly
// because Typeof takes an empty interface value. This is annoying.
var typeOfError = reflect.TypeOf((*error)(nil)).Elem()

type MethodKind byte
const (
	Method MethodKind = iota
	MethodWithContext
	NumMethodKind
)

var mKindNames = []string{"Method","MethodWithContext","Undefined"}

func (k MethodKind) String() string {
	return mKindNames[k]
}

type methodType struct {
	sync.Mutex // protects counters
	methodKind MethodKind
	method     reflect.Method
	ArgType    reflect.Type
	ReplyType  reflect.Type
	numCalls   uint
}

type service struct {
	name   string                 // name of service
	rcvr   reflect.Value          // receiver of methods for the service
	typ    reflect.Type           // type of the receiver
	method map[string]*methodType // registered methods
}

// Server represents an RPC Server.
type Server struct {
	serviceMap  sync.Map   // map[string]*service
	headerLock  sync.Mutex // protects freeHeaders
	freeHeaders *Header
	doneCh      chan struct{}
	connLock    sync.Mutex
	closing     bool
	actCodecs   map[*connDriver]struct{}
}

// NewServer returns a new Server.
func NewServer() *Server {
	return &Server{
		doneCh:    make(chan struct{}),
		actCodecs: make(map[*connDriver]struct{}),
	}
}

// DefaultServer is the default instance of *Server.
var DefaultServer = NewServer()

// NewDefaultCodec is used to create codec used in default connection set up:
// Dial, DialHTTP, Accept, HandleHTTP,...
var NewDefaultCodec CodecMaker = NewGobCodec

// Close shutdown server
func (s *Server) Close() {
	s.connLock.Lock()
	s.closing = true
	num := len(s.actCodecs)
	for cd, _ := range s.actCodecs {
		cd.Close()
	}
	s.connLock.Unlock()
	for i := 0; i < num; i++ {
		<-s.doneCh
	}
}

func (s *Server) connShutdown(cd *connDriver) {
	// Wait for all active call to finish before closing codec.
	cd.wg.Wait()
	cd.Close()
	s.connLock.Lock()
	closing := s.closing
	delete(s.actCodecs, cd)
	s.connLock.Unlock()
	if closing {
		//synchronize server closing
		s.doneCh <- struct{}{}
	}
}

// Is this type exported or a builtin?
func isExportedOrBuiltinType(t reflect.Type) bool {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	// PkgPath will be non-empty even for an exported type,
	// so we need to check the type name as well.
	return token.IsExported(t.Name()) || t.PkgPath() == ""
}

// Register publishes in the server the set of methods of the
// receiver value that satisfy the following conditions:
//	- exported method of exported type
//	- two arguments, both of exported type
//	- the second argument is a pointer
//	- one return value, of type error
// It returns an error if the receiver is not an exported type or has
// no suitable methods. It also logs the error using package log.
// The client accesses each method using a string of the form "Type.Method",
// where Type is the receiver's concrete type.
func (server *Server) Register(rcvr interface{}) error {
	return server.register(rcvr, "", false)
}

// RegisterName is like Register but uses the provided name for the type
// instead of the receiver's concrete type.
func (server *Server) RegisterName(name string, rcvr interface{}) error {
	return server.register(rcvr, name, true)
}

func (server *Server) register(rcvr interface{}, name string, useName bool) error {
	s := new(service)
	s.typ = reflect.TypeOf(rcvr)
	s.rcvr = reflect.ValueOf(rcvr)
	sname := reflect.Indirect(s.rcvr).Type().Name()
	if useName {
		sname = name
	}
	if sname == "" {
		s := "rpc.Register: no service name for type " + s.typ.String()
		log.Print(s)
		return errors.New(s)
	}
	if !token.IsExported(sname) && !useName {
		s := "rpc.Register: type " + sname + " is not exported"
		log.Print(s)
		return errors.New(s)
	}
	s.name = sname

	// Install the methods
	s.method = suitableMethods(s.typ, true)

	if len(s.method) == 0 {
		str := ""

		// To help the user, see if a pointer receiver would work.
		method := suitableMethods(reflect.PtrTo(s.typ), false)
		if len(method) != 0 {
			str = "rpc.Register: type " + sname + " has no exported methods of suitable type (hint: pass a pointer to value of that type)"
		} else {
			str = "rpc.Register: type " + sname + " has no exported methods of suitable type"
		}
		log.Print(str)
		return errors.New(str)
	}

	if _, dup := server.serviceMap.LoadOrStore(sname, s); dup {
		return errors.New("rpc: service already defined: " + sname)
	}
	return nil
}

// suitableMethods returns suitable Rpc methods of typ, it will report
// error using log if reportErr is true.
func suitableMethods(typ reflect.Type, reportErr bool) map[string]*methodType {
	methods := make(map[string]*methodType)
	for m := 0; m < typ.NumMethod(); m++ {
		method := typ.Method(m)
		mtype := method.Type
		mname := method.Name
		// Method must be exported.
		if method.PkgPath != "" {
			continue
		}
		// Two kinds of methods:
		// normal method needs three ins: receiver, *args, *reply.
		// method with context has four ins: receiver, context, *args, *reply
		var mKind MethodKind = Method
		switch {
		case mtype.NumIn()==3:
			mKind=Method
		case mtype.NumIn()==4:
			mKind=MethodWithContext
		default:
			if reportErr {
				log.Printf("rpc.Register: method %q has %d input parameters; needs exactly three or four\n", mname, mtype.NumIn())
			}
			continue
		}
		nextArg := 1
		if mKind == MethodWithContext {
			arg1Type := mtype.In(nextArg)
			if arg1Type.Name()!="Context" || arg1Type.PkgPath()!="context" {
				if reportErr {
					log.Printf("rpc.Register: first argument type of method with context %q is not context.Context: %q\n", mname, arg1Type)
				}
				continue
			}
			nextArg++
		}
		// Request arg need not be a pointer.
		argType := mtype.In(nextArg)
		if !isExportedOrBuiltinType(argType) {
			if reportErr {
				log.Printf("rpc.Register: argument type of method %q is not exported: %q\n", mname, argType)
			}
			continue
		}
		// Reply arg must be a pointer.
		nextArg++
		replyType := mtype.In(nextArg)
		if replyType.Kind() != reflect.Ptr {
			if reportErr {
				log.Printf("rpc.Register: reply type of method %q is not a pointer: %q\n", mname, replyType)
			}
			continue
		}
		// Reply type must be exported.
		if !isExportedOrBuiltinType(replyType) {
			if reportErr {
				log.Printf("rpc.Register: reply type of method %q is not exported: %q\n", mname, replyType)
			}
			continue
		}
		// Method needs one out.
		if mtype.NumOut() != 1 {
			if reportErr {
				log.Printf("rpc.Register: method %q has %d output parameters; needs exactly one\n", mname, mtype.NumOut())
			}
			continue
		}
		// The return type of the method must be error.
		if returnType := mtype.Out(0); returnType != typeOfError {
			if reportErr {
				log.Printf("rpc.Register: return type of method %q is %q, must be error\n", mname, returnType)
			}
			continue
		}
		methods[mname] = &methodType{methodKind: mKind, method: method, ArgType: argType, ReplyType: replyType}
	}
	return methods
}

// A value sent as a placeholder for the server's response value when the server
// receives an invalid request. It is never decoded by the client since the Response
// contains an error when it is used.
var invalidRequest = struct{}{}

func (server *Server) sendResponse(conndrv *connDriver, req *Header, reply interface{}, errmsg string) {
	resp := server.getHeader()
	// Encode the response header
	resp.ServiceMethod = req.ServiceMethod
	resp.Seq = req.Seq
	resp.Kind = Response
	if errmsg != "" {
		resp.Kind = Error
		resp.Info = errmsg
		reply = invalidRequest
	}
	conndrv.wlock.Lock()
	err := conndrv.codec.WriteHeaderBody(resp, reply)
	conndrv.wlock.Unlock()
	if debugLog {
		if err != nil {
			log.Println("rpc: fail writing response:", err)
		} else {
			log.Printf("%p srv send: %s\n", server, resp)
		}
	}
	server.freeHeader(resp)
}

func (m *methodType) NumCalls() (n uint) {
	m.Lock()
	n = m.numCalls
	m.Unlock()
	return n
}

func (s *service) call(server *Server, conndrv *connDriver, mtype *methodType, req *Header, argv, replyv reflect.Value) {
	defer conndrv.wg.Done()
	mtype.Lock()
	mtype.numCalls++
	mtype.Unlock()
	function := mtype.method.Func
	var returnValues []reflect.Value
	// Invoke the method, providing a new value for the reply.
	if mtype.methodKind==MethodWithContext {
		ctxVal := reflect.ValueOf(req.Info)
		returnValues = function.Call([]reflect.Value{s.rcvr, ctxVal, argv, replyv})
	} else /*if mtype.methodKind==Method*/ {
		returnValues = function.Call([]reflect.Value{s.rcvr, argv, replyv})
	}
	// The return value for the method is an error.
	errInter := returnValues[0].Interface()
	errmsg := ""
	if errInter != nil {
		errmsg = errInter.(error).Error()
	}
	server.sendResponse(conndrv, req, replyv.Interface(), errmsg)
	if mtype.methodKind==MethodWithContext {
		conndrv.RemoveCall(req.Seq)
	}
	server.freeHeader(req)
}

// ServeConn runs the server on a single connection.
// ServeConn blocks, serving the connection until the client hangs up.
// The caller typically invokes ServeConn in a go statement.
// ServeConn uses the gob wire format (see package gob) on the
// connection. To use an alternate codec, use ServeCodec.
// See NewClient's comment for information about concurrent access.
func (server *Server) ServeConn(conn io.ReadWriteCloser) {
	server.ServeCodec(NewDefaultCodec(conn))
}

// ServeCodec is like ServeConn but uses the specified codec to
// decode requests and encode responses.
func (server *Server) ServeCodec(codec Codec) {
	conndrv := newConnDriver(codec)
	conndrv.server = server
	conndrv.Loop()
}

// handleRequest handles one request forwarded from connDriver
func (server *Server) handleRequest(conndrv *connDriver, req *Header) error {
	if debugLog {
		log.Printf("%p srv recv: %s\n", server, req)
	}

	service, mtype, err := server.decodeRequestHeader(req)
	if err != nil {
		// discard body
		if debugLog {
			log.Println("srv: discard Body")
		}
		conndrv.codec.ReadBody(nil)
		server.sendResponse(conndrv, req, invalidRequest, err.Error())
		server.freeHeader(req)
		return err
	}

	argv, replyv, err := server.readRequestBody(conndrv.codec, service, mtype, req)

	if err != nil {
		server.sendResponse(conndrv, req, invalidRequest, err.Error())
		server.freeHeader(req)
		return err
	}

	if mtype.methodKind == MethodWithContext {
		cancel,err := setupCallContext(req)
		if err!=nil {
			if debugLog {
				log.Println(err.Error())
			}
			return err
		}
		conndrv.AddCall(req.Seq,cancel)
	}

	conndrv.wg.Add(1)
	go service.call(server, conndrv, mtype, req, argv, replyv)
	return nil
}

func setupCallContext(req *Header) (cancel context.CancelFunc, err error) {
	deadline, err := time.Parse(time.RFC3339, req.Info.(string))
	if err!=nil {
		err = fmt.Errorf("failed to parse deadline time: %v, %v", err, req.Info)
		return
	}
	var ctx context.Context
	if deadline.IsZero() {
		ctx, cancel = context.WithCancel(context.Background())
	} else {
		ctx, cancel = context.WithDeadline(context.Background(), deadline)
	}
	req.Info = ctx  //pack ctx for use inside call
	return
}

// ServeRequest is like ServeCodec but synchronously serves a single request.
// It does not close the codec upon completion.
func (server *Server) ServeRequest(codec Codec) error {
	//create a temp connDriver for one time use
	//possible race if same codec/connection used in diff connDriver
	tmpConnDrv := newConnDriver(codec)
	tmpConnDrv.server=server
	req, err := server.readRequestHeader(codec)
	if err != nil {
		server.freeHeader(req)
		return err
	}

	service, mtype, err := server.decodeRequestHeader(req)
	if err != nil {
		// discard body
		if debugLog {
			log.Println("srv: discard Body")
		}
		codec.ReadBody(nil)
		server.sendResponse(tmpConnDrv, req, invalidRequest, err.Error())
		server.freeHeader(req)
		return err
	}

	argv, replyv, err := server.readRequestBody(codec, service, mtype, req)

	if err != nil {
		server.sendResponse(tmpConnDrv, req, invalidRequest, err.Error())
		server.freeHeader(req)
		return err
	}
	
	if mtype.methodKind == MethodWithContext {
		cancel,err := setupCallContext(req)
		if err!=nil {
			if debugLog {
				log.Println(err.Error())
			}
			return err
		}
		tmpConnDrv.AddCall(req.Seq,cancel)
	}

	tmpConnDrv.wg.Add(1) //just for keep call() api consistent
	service.call(server, tmpConnDrv, mtype, req, argv, replyv)
	return nil
}

func (server *Server) readRequestBody(codec Codec, service *service, mtype *methodType, req *Header) (argv, replyv reflect.Value, err error) {
	// Decode the argument value.
	argIsValue := false // if true, need to indirect before calling.
	if mtype.ArgType.Kind() == reflect.Ptr {
		argv = reflect.New(mtype.ArgType.Elem())
	} else {
		argv = reflect.New(mtype.ArgType)
		argIsValue = true
	}
	// argv guaranteed to be a pointer now.
	if err = codec.ReadBody(argv.Interface()); err != nil {
		return
	}
	if argIsValue {
		argv = argv.Elem()
	}

	replyv = reflect.New(mtype.ReplyType.Elem())

	switch mtype.ReplyType.Elem().Kind() {
	case reflect.Map:
		replyv.Elem().Set(reflect.MakeMap(mtype.ReplyType.Elem()))
	case reflect.Slice:
		replyv.Elem().Set(reflect.MakeSlice(mtype.ReplyType.Elem(), 0, 0))
	}
	return
}

func (server *Server) readRequestHeader(codec Codec) (req *Header, err error) {
	// Grab the request header.
	req = server.getHeader()
	err = codec.ReadHeader(req)
	if err != nil {
		req = nil
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return
		}
		err = errors.New("rpc: server cannot decode header: " + err.Error())
		return
	}

	// We read the header successfully. If we see an error now,
	// we can still recover and move on to the next request.
	return
}

func (server *Server) decodeRequestHeader(req *Header) (svc *service, mtype *methodType, err error) {
	dot := strings.LastIndex(req.ServiceMethod, ".")
	if dot < 0 {
		err = errors.New("rpc: service/method request ill-formed: " + req.ServiceMethod)
		return
	}
	serviceName := req.ServiceMethod[:dot]
	methodName := req.ServiceMethod[dot+1:]

	// Look up the request.
	svci, ok := server.serviceMap.Load(serviceName)
	if !ok {
		err = errors.New("rpc: can't find service " + req.ServiceMethod)
		return
	}
	svc = svci.(*service)
	mtype = svc.method[methodName]
	if mtype == nil {
		err = errors.New("rpc: can't find method " + req.ServiceMethod)
	}
	return
}

func (server *Server) getHeader() *Header {
	server.headerLock.Lock()
	hdr := server.freeHeaders
	if hdr == nil {
		hdr = new(Header)
	} else {
		server.freeHeaders = hdr.next
		*hdr = Header{}
	}
	server.headerLock.Unlock()
	return hdr
}

func (server *Server) freeHeader(hdr *Header) {
	server.headerLock.Lock()
	hdr.next = server.freeHeaders
	server.freeHeaders = hdr
	server.headerLock.Unlock()
}

// Register publishes the receiver's methods in the DefaultServer.
func Register(rcvr interface{}) error { return DefaultServer.Register(rcvr) }

// RegisterName is like Register but uses the provided name for the type
// instead of the receiver's concrete type.
func RegisterName(name string, rcvr interface{}) error {
	return DefaultServer.RegisterName(name, rcvr)
}

// ServeConn runs the DefaultServer on a single connection.
// ServeConn blocks, serving the connection until the client hangs up.
// The caller typically invokes ServeConn in a go statement.
// ServeConn uses the gob wire format (see package gob) on the
// connection. To use an alternate codec, use ServeCodec.
// See NewClient's comment for information about concurrent access.
func ServeConn(conn io.ReadWriteCloser) {
	DefaultServer.ServeConn(conn)
}

// ServeCodec is like ServeConn but uses the specified codec to
// decode requests and encode responses.
func ServeCodec(codec Codec) {
	DefaultServer.ServeCodec(codec)
}

// ServeRequest is like ServeCodec but synchronously serves a single request.
// It does not close the codec upon completion.
func ServeRequest(codec Codec) error {
	return DefaultServer.ServeRequest(codec)
}

// ConnectBiDirectionCodec connects server and conn/codec to remote peer
// for bidirectional rpc. It will serve its registered obj/methods over conn,
// return client for invoking remote services.
func (server *Server) ConnectBiDirectionCodec(codec Codec) *Client {
	conndrv := newConnDriver(codec)
	conndrv.server = server
	conndrv.client = newClient(conndrv)
	go conndrv.Loop()
	return conndrv.client
}

// AcceptBiDirection accept remote connection and setup bidirection rpc.
// It return client to invoke remote services and serve calls from
// remote client.
func (server *Server) AcceptBiDirection(lis net.Listener) (*Client, error) {
	conn, err := lis.Accept()
	if err != nil {
		log.Print("rpc.BiDirection: accept:", err.Error())
		return nil, err
	}
	return server.ConnectBiDirectionCodec(NewDefaultCodec(conn)), nil
}

// DialBiDirection connects to remote server and setup bidirection rpc.
// It return client to invoke remote services and serve calls from
// remote client.
func (server *Server) DialBiDirection(network, address string) (*Client, error) {
	conn, err := net.Dial(network, address)
	if err != nil {
		log.Print("rpc.BiDirection: dial:", err.Error())
		return nil, err
	}
	return server.ConnectBiDirectionCodec(NewDefaultCodec(conn)), nil
}

// Accept accepts connections on the listener and serves requests
// for each incoming connection. Accept blocks until the listener
// returns a non-nil error. The caller typically invokes Accept in a
// go statement.
func (server *Server) Accept(lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			log.Print("rpc.Serve: accept:", err.Error())
			return
		}
		go server.ServeConn(conn)
	}
}

// Accept accepts connections on the listener and serves requests
// to DefaultServer for each incoming connection.
// Accept blocks; the caller typically invokes it in a go statement.
func Accept(lis net.Listener) { DefaultServer.Accept(lis) }

// Can connect to RPC service using HTTP CONNECT to rpcPath.
var connected = "200 Connected to Go RPC"

// ServeHTTP implements an http.Handler that answers RPC requests.
func (server *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if conn, err := hijackHTTPConn(w, req); err == nil {
		server.ServeConn(conn)
	}
}

// hijackHTTPConn hijacks a http conn to run RPC protocol,
// paired with client side func DialHTTPPathForConn()
func hijackHTTPConn(w http.ResponseWriter, req *http.Request) (net.Conn, error) {
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
func (server *Server) HandleHTTP(rpcPath, debugPath string) {
	http.Handle(rpcPath, server)
	http.Handle(debugPath, debugHTTP{server})
}

// HandleHTTP registers an HTTP handler for RPC messages to DefaultServer
// on DefaultRPCPath and a debugging handler on DefaultDebugPath.
// It is still necessary to invoke http.Serve(), typically in a go statement.
func HandleHTTP() {
	DefaultServer.HandleHTTP(DefaultRPCPath, DefaultDebugPath)
}

// DialBiDirection connects to remote server and setup bidirection rpc, return client to
// invoke remote services and serve calls from remote client with DefaultServer
func DialBiDirection(network, address string) (*Client, error) {
	return DefaultServer.DialBiDirection(network, address)
}

// AcceptBiDirection accept remote connection and setup bidirection rpc, return client to
// invoke remote services and serve calls from remote client with DefaultServer
func AcceptBiDirection(lis net.Listener) (*Client, error) {
	return DefaultServer.AcceptBiDirection(lis)
}
