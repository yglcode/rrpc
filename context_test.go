package rrpc

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"
)

type ArithCtx int

func (t *ArithCtx) Add(args Args, reply *Reply) (err error) {
	reply.C = args.A + args.B
	return nil
}

func (t *ArithCtx) AddWithContextFor5Sec(ctx context.Context, args Args, reply *Reply) (err error) {
	reply.C = args.A + args.B
	timer := time.NewTimer(time.Second)
	count := 0
LOOP:
	for count < 5 {
		select {
		case <-ctx.Done():
			err = ctx.Err()
			break LOOP
		case <-timer.C:
			count++
			timer.Reset(time.Second)
		}
	}
	timer.Stop()
	fmt.Println("done")
	return
}

// TestTimeoutAndCancel test RPC timeout and cancelation
func TestTimeoutAndCancel(t *testing.T) {
	//setup server
	jsonServer = NewServer()
	jsonServer.Register(new(ArithCtx))

	l, addr := listenTCP()
	log.Println("NewJsonServer test Timeout and Cancelation listening on", addr)

	//change default codec to JSON
	NewDefaultCodec = NewJsonCodec

	go jsonServer.Accept(l)

	//set up client
	client, err := Dial("tcp", addr)
	if err != nil {
		t.Fatal("dialing", err)
	}
	defer client.Close()

	// test cancelation
	args := &Args{7, 8}
	reply := new(Reply)
	ctx, cancel := context.WithCancel(context.Background())
	addCall := client.GoWithContext("ArithCtx.AddWithContextFor5Sec", ctx, args, reply, nil)
	cancel()
	addCall = <-addCall.Done
	if addCall.Error == nil {
		t.Errorf("Add: expected cancelation error but got nil %v", addCall.Error)
	} else {
		t.Logf("Add: got cancelation %q", addCall.Error.Error())
	}

	// test timeout
	args = &Args{7, 8}
	reply = new(Reply)
	ctx, _ = context.WithTimeout(context.Background(), time.Duration(100*time.Millisecond))
	err = client.CallWithContext("ArithCtx.AddWithContextFor5Sec", ctx, args, reply)
	if err == nil {
		t.Errorf("Add: expected timeout error but got nil %v", err)
	} else {
		t.Logf("Add: got timeout %q", err.Error())
	}

	// test failure if invoke non-context method with context
	args = &Args{7, 8}
	reply = new(Reply)
	ctx = context.Background()
	err = client.CallWithContext("ArithCtx.Add", ctx, args, reply)
	if err == nil {
		t.Errorf("Add: expected error but got nil %v", err)
	} else {
		t.Logf("Add: got error %q", err.Error())
	}
}
