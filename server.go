package rrpc

import (
	"errors"
	"go/token"
	"io"
	"log"
	"reflect"
	"strings"
	"sync"
)

// Precompute the reflect type for error. Can't use error directly
// because Typeof takes an empty interface value. This is annoying.
var typeOfError = reflect.TypeOf((*error)(nil)).Elem()

type methodType struct {
	sync.Mutex // protects counters
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
		// Method needs three ins: receiver, *args, *reply.
		if mtype.NumIn() != 3 {
			if reportErr {
				log.Printf("rpc.Register: method %q has %d input parameters; needs exactly three\n", mname, mtype.NumIn())
			}
			continue
		}
		// First arg need not be a pointer.
		argType := mtype.In(1)
		if !isExportedOrBuiltinType(argType) {
			if reportErr {
				log.Printf("rpc.Register: argument type of method %q is not exported: %q\n", mname, argType)
			}
			continue
		}
		// Second arg must be a pointer.
		replyType := mtype.In(2)
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
		methods[mname] = &methodType{method: method, ArgType: argType, ReplyType: replyType}
	}
	return methods
}

// A value sent as a placeholder for the server's response value when the server
// receives an invalid request. It is never decoded by the client since the Response
// contains an error when it is used.
var invalidRequest = struct{}{}

func (server *Server) sendResponse(sending *sync.Mutex, req *Header, reply interface{}, codec Codec, errmsg string) {
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
	sending.Lock()
	err := codec.WriteHeaderBody(resp, reply)
	sending.Unlock()
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

func (s *service) call(server *Server, sending *sync.Mutex, wg *sync.WaitGroup, mtype *methodType, req *Header, argv, replyv reflect.Value, codec Codec) {
	if wg != nil {
		defer wg.Done()
	}
	mtype.Lock()
	mtype.numCalls++
	mtype.Unlock()
	function := mtype.method.Func
	// Invoke the method, providing a new value for the reply.
	returnValues := function.Call([]reflect.Value{s.rcvr, argv, replyv})
	// The return value for the method is an error.
	errInter := returnValues[0].Interface()
	errmsg := ""
	if errInter != nil {
		errmsg = errInter.(error).Error()
	}
	server.sendResponse(sending, req, replyv.Interface(), codec, errmsg)
	server.freeHeader(req)
}

// ServeConn runs the server on a single connection.
// ServeConn blocks, serving the connection until the client hangs up.
// The caller typically invokes ServeConn in a go statement.
// ServeConn uses the gob wire format (see package gob) on the
// connection. To use an alternate codec, use ServeCodec.
// See NewClient's comment for information about concurrent access.
func (server *Server) ServeConn(conn io.ReadWriteCloser, encdec ...EncDecoder) {
	//by default use gob for encode/decode
	edc := GobEncDecoder
	if len(encdec) > 0 {
		edc = encdec[0]
	}
	server.ServeCodec(NewCodec(conn, edc))
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
	sending := conndrv.wlock
	wg := conndrv.wg
	codec := conndrv.codec

	if debugLog {
		log.Printf("%p srv recv: %s\n", server, req)
	}

	service, mtype, err := server.decodeRequestHeader(req)
	if err != nil {
		// discard body
		if debugLog {
			log.Println("srv: discard Body")
		}
		codec.ReadBody(nil)
		server.sendResponse(sending, req, invalidRequest, codec, err.Error())
		server.freeHeader(req)
		if debugLog && err != io.EOF {
			log.Println("rpc01:", err)
		}
		return err
	}

	argv, replyv, err := server.readRequestBody(codec, service, mtype, req)

	if err != nil {
		server.sendResponse(sending, req, invalidRequest, codec, err.Error())
		server.freeHeader(req)
		if debugLog && err != io.EOF {
			log.Println("rpc02:", err)
		}
		return err
	}

	wg.Add(1)
	go service.call(server, sending, wg, mtype, req, argv, replyv, codec)
	return nil
}

// ServeRequest is like ServeCodec but synchronously serves a single request.
// It does not close the codec upon completion.
func (server *Server) ServeRequest(codec Codec) error {
	sending := new(sync.Mutex)
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
		server.sendResponse(sending, req, invalidRequest, codec, err.Error())
		server.freeHeader(req)
		return err
	}

	argv, replyv, err := server.readRequestBody(codec, service, mtype, req)

	if err != nil {
		server.sendResponse(sending, req, invalidRequest, codec, err.Error())
		server.freeHeader(req)
		return err
	}
	service.call(server, sending, nil, mtype, req, argv, replyv, codec)
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
func ServeConn(conn io.ReadWriteCloser, encdec ...EncDecoder) {
	DefaultServer.ServeConn(conn, encdec...)
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
