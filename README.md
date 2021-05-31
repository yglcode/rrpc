## Reflect based RPC (net/rpc fork) ##

Golang original net/rpc package has been frozen for 4-5 years now [issue #16844](https://github.com/golang/go/issues/16844). Go net/rpc uses reflect to automate method signature discovery and data marshaling, so there is no need for IDL/protobuf and stub/skeleton code generation. People like it for its simplicity (API and implementation), and good performance for request-response style RPCs [cockroachdb benchmark](https://github.com/cockroachdb/rpc-bench). It is good for writing tools, such as out of process plugins and extensions.

This is a fork of the originial Go net/rpc code for some rework/enhancements:

* keep API the same as original as much as possible.

* rework the internals so that codec and message receiver are reused for both client and server.

* support simple bi-directional RPC thru the same connection.

    * There are both client and server at each end of a connection.

* support method cancelation and timeout thru context.Context: CallWithContext() and GoWithContext().

    * only deadline and cancelation signal are propagated to from client to server, no context values.




[godoc](https://pkg.go.dev/github.com/yglcode/rrpc).

