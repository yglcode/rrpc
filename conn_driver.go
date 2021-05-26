package rrpc

import (
	"errors"
	"io"
	"log"
	"sync"
)

//the communication driver for client/server on one end of connection
type connDriver struct {
	server *Server
	client *Client
	//codec write lock
	wlock *sync.Mutex
	codec Codec
	//wait group for outstanding calls to server
	wg *sync.WaitGroup
}

func newConnDriver(codec Codec) *connDriver {
	return &connDriver{
		codec: codec,
		wlock: new(sync.Mutex),
		wg:    new(sync.WaitGroup),
	}
}

func (cd *connDriver) Close() error {
	cd.wlock.Lock()
	defer cd.wlock.Unlock()
	return cd.codec.Close()
}

func (cd *connDriver) Loop() {
	//if debugLog {
	//log.Println("Loop() start, server=",cd.server,", client=",cd.client)
	//}
	if cd.server != nil {
		//register active codec/connDriver with server
		cd.server.connLock.Lock()
		if cd.server.closing {
			cd.server.connLock.Unlock()
			return
		}
		cd.server.actCodecs[cd] = struct{}{}
		cd.server.connLock.Unlock()
	}
	var err error
	var header *Header
	if cd.server != nil {
		header = cd.server.getHeader()
	} else {
		//for pure client, reuse a single header
		header = &Header{}
	}
	for {
		err = cd.codec.ReadHeader(header)
		if err != nil {
			//failed to decode Header, exit
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if debugLog {
					log.Println("rpc:", err)
				}
				break
			}
			err = errors.New("rpc: cannot decode header: " + err.Error())
			if debugLog {
				log.Println("rpc:", err)
			}
			break
		}
		if header.Kind == Request {
			// Forward requests to server
			if cd.server != nil {
				err = cd.server.handleRequest(cd, header)
				if err != nil && debugLog {
					log.Println("rpc:", err)
				}
				//keep working incase of handling error
				//if err!=nil {
				//	break
				//}
				//since header is freed inside server.handleRequest
				//allocate a new header
				header = cd.server.getHeader()
			} else {
				if debugLog {
					log.Println("rpc: receive requests, but there is no server")
				}

			}
		} else if header.Kind == Response || header.Kind == Error {
			// Forwars reponses and errors to client
			if cd.client != nil {
				err = cd.client.handleResponse(cd.codec, header)
				if err != nil && debugLog {
					log.Println("rpc:", err)
				}
				if err != nil && cd.server == nil {
					//pure client break out on 1st error
					if debugLog {
						log.Println("rpc: >>>client exit, ", err)
					}
					break
				}
				*header = Header{} //reset header
			} else {
				if debugLog {
					log.Println("rpc: receive responses, but there is no client")
				}

			}
		} else {
			if debugLog {
				log.Printf("rpc: invalid header.Kind: %v\n", header.Kind)
			}
		}
	}
	if cd.server != nil {
		cd.server.freeHeader(header)
		//wait for all outstanding calls
		cd.server.connShutdown(cd)
	}
	if cd.client != nil {
		//notify remaining clients
		cd.client.connShutdown(err)
	}
	if debugLog {
		log.Println("*** Loop() Exit, server=", cd.server != nil, ", client=", cd.client != nil)
	}
}
