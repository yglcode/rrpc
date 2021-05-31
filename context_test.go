package rrpc

import (
	"context"
	"log"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	ctxOnce sync.Once
	srvAddr string
)

type ArithCtx int

func (t *ArithCtx) Add(args Args, reply *Reply) (err error) {
	reply.C = args.A + args.B
	return nil
}

func (t *ArithCtx) AddWithContext(ctx context.Context, args Args, reply *Reply) (err error) {
	reply.C = args.A + args.B
	select {
	case <-ctx.Done():
		err = ctx.Err()
	default:
	}
	return
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
	return
}

func startCtxServer() {
	//setup server
	ctxServer := NewServer()
	ctxServer.Register(new(ArithCtx))

	var l net.Listener
	l, srvAddr = listenTCP()
	log.Println("NewContextServer test Timeout and Cancelation listening on", srvAddr)

	//change default codec to JSON
	//NewDefaultCodec = NewJsonCodec

	go ctxServer.Accept(l)
}

// TestTimeoutAndCancel test RPC timeout and cancelation
func TestTimeoutAndCancel(t *testing.T) {
	ctxOnce.Do(startCtxServer)

	//set up client
	client, err := Dial("tcp", srvAddr)
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
	ctx, cancel = context.WithTimeout(context.Background(), time.Duration(100*time.Millisecond))
	defer cancel()
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
		t.Logf("Add: got error %v", err.Error())
	}
}

func BenchmarkCallWithContext(b *testing.B) {
	ctxOnce.Do(startCtxServer)

	client, err := Dial("tcp", srvAddr)
	if err != nil {
		b.Fatal("dialing", err)
	}
	defer client.Close()

	// Synchronous calls
	args := &Args{7, 8}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		reply := new(Reply)
		for pb.Next() {
			err := client.CallWithContext("ArithCtx.AddWithContext", ctx, args, reply)
			if err != nil {
				b.Fatalf("rpc error: ArithCtx.AddWithContext: expected no error but got string %q", err.Error())
			}
			if reply.C != args.A+args.B {
				b.Fatalf("rpc error: ArithCtx.AddWithContext: expected %d got %d", args.A+args.B, reply.C)
			}
		}
	})
}

func BenchmarkGoWithContext(b *testing.B) {
	ctxOnce.Do(startCtxServer)

	client, err := Dial("tcp", srvAddr)
	if err != nil {
		b.Fatal("dialing", err)
	}
	defer client.Close()

	const MaxConcurrentCalls = 100
	args := &Args{7, 8}
	procs := 4 * runtime.GOMAXPROCS(-1)
	send := int32(b.N)
	recv := int32(b.N)
	var wg sync.WaitGroup
	wg.Add(procs)
	gate := make(chan bool, MaxConcurrentCalls)
	res := make(chan *Call, MaxConcurrentCalls)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.ResetTimer()

	for p := 0; p < procs; p++ {
		go func() {
			for atomic.AddInt32(&send, -1) >= 0 {
				gate <- true
				reply := new(Reply)
				client.GoWithContext("ArithCtx.AddWithContext", ctx, args, reply, res)
			}
		}()
		go func() {
			for call := range res {
				A := call.Args.(*Args).A
				B := call.Args.(*Args).B
				C := call.Reply.(*Reply).C
				if A+B != C {
					b.Errorf("incorrect reply: ArithCtx.AddWithContext: expected %d got %d", A+B, C)
					return
				}
				<-gate
				if atomic.AddInt32(&recv, -1) == 0 {
					close(res)
				}
			}
			wg.Done()
		}()
	}
	wg.Wait()
}
