package rrpc

import (
	"log"
	"net"
	"reflect"
	"testing"
)

func TestBiDirOverTcp(t *testing.T) {
	// set default codec to Gob
	NewDefaultCodec = NewGobCodec

	// Serve Arith at this end of connection
	mathServer := NewServer()
	defer mathServer.Close()
	mathServer.Register(new(Arith))

	//this end will be connection server
	var l net.Listener
	l, connServerAddr := listenTCP()
	defer l.Close()
	log.Println("NewBiDirServer test RPC server listening on", connServerAddr)

	// use done chan to wait for goroutine exit
	done := make(chan struct{})

	go func() {
		// Serve Arith at this end of connection
		// and call Builtins at other end
		builtinCli, err := mathServer.AcceptBiDirection(l)
		if err != nil {
			t.Fatal("bidir.AcceptOne", err)
		}
		defer builtinCli.Close()
		// Test builtin calls
		testBuiltin(t, builtinCli)
		// mark goroutine is done
		close(done)
	}()

	// Serve Builtins at this end of connection
	builtinServer := NewServer()
	defer builtinServer.Close()
	builtinServer.Register(BuiltinTypes{})

	// this end is connection client
	// connect bidirection rpc and get client to call Arith
	mathClient, err := builtinServer.DialBiDirection("tcp", connServerAddr)
	if err != nil {
		t.Fatal("bidir.dialing", err)
	}
	defer mathClient.Close()

	testArith(t, mathClient)

	//wait for goroutine to exit
	<-done
}

func TestBiDirOverPipe(t *testing.T) {
	// set up in-memory pipe
	conn1, conn2 := net.Pipe()
	defer conn1.Close()

	codec1 := NewJsonCodec(conn1)
	codec2 := NewJsonCodec(conn2)

	// Serve Arith at one end of connection
	mathServer := NewServer()
	defer mathServer.Close()
	mathServer.Register(new(Arith))

	// receive client to call Builtin
	builtinCli := mathServer.ConnectBiDirectionCodec(codec1)
	defer builtinCli.Close()

	// Serve Builtins at other end
	builtinServer := NewServer()
	defer builtinServer.Close()
	builtinServer.Register(BuiltinTypes{})

	// receive client to call math/Arith
	mathClient := builtinServer.ConnectBiDirectionCodec(codec2)
	defer mathClient.Close()

	// use done chan to wait for goroutine exit
	done := make(chan struct{})

	go func() {
		// Serve Arith at this end of connection
		// and call Builtins at other end
		// Test builtin calls
		testBuiltin(t, builtinCli)
		// mark goroutine is done
		close(done)
	}()

	testArith(t, mathClient)

	//wait for goroutine to exit
	<-done
}

func testArith(t *testing.T, client *Client) {
	// Test Arith calls
	args := &Args{7, 8}
	reply := new(Reply)
	err := client.Call("Arith.Add", args, reply)
	if err != nil {
		t.Errorf("Add: expected no error but got string %q", err.Error())
	}
	if reply.C != args.A+args.B {
		t.Errorf("Add: expected %d got %d", reply.C, args.A+args.B)
	}

	// Out of order.
	args = &Args{7, 8}
	mulReply := new(Reply)
	mulCall := client.Go("Arith.Mul", args, mulReply, nil)
	addReply := new(Reply)
	addCall := client.Go("Arith.Add", args, addReply, nil)

	addCall = <-addCall.Done
	if addCall.Error != nil {
		t.Errorf("Add: expected no error but got string %q", addCall.Error.Error())
	}
	if addReply.C != args.A+args.B {
		t.Errorf("Add: expected %d got %d", addReply.C, args.A+args.B)
	}

	mulCall = <-mulCall.Done
	if mulCall.Error != nil {
		t.Errorf("Mul: expected no error but got string %q", mulCall.Error.Error())
	}
	if mulReply.C != args.A*args.B {
		t.Errorf("Mul: expected %d got %d", mulReply.C, args.A*args.B)
	}
}

func testBuiltin(t *testing.T, client *Client) {
	// Map
	args := &Args{7, 8}
	replyMap := map[int]int{}
	err := client.Call("BuiltinTypes.Map", args, &replyMap)
	if err != nil {
		t.Errorf("Map: expected no error but got string %q", err.Error())
	}
	if replyMap[args.A] != args.B {
		t.Errorf("Map: expected %d got %d", args.B, replyMap[args.A])
	}

	// Slice
	args = &Args{7, 8}
	replySlice := []int{}
	err = client.Call("BuiltinTypes.Slice", args, &replySlice)
	if err != nil {
		t.Errorf("Slice: expected no error but got string %q", err.Error())
	}
	if e := []int{args.A, args.B}; !reflect.DeepEqual(replySlice, e) {
		t.Errorf("Slice: expected %v got %v", e, replySlice)
	}

	// Array
	args = &Args{7, 8}
	replyArray := [2]int{}
	err = client.Call("BuiltinTypes.Array", args, &replyArray)
	if err != nil {
		t.Errorf("Array: expected no error but got string %q", err.Error())
	}
	if e := [2]int{args.A, args.B}; !reflect.DeepEqual(replyArray, e) {
		t.Errorf("Array: expected %v got %v", e, replyArray)
	}
}
