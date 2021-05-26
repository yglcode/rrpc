package rrpc

import (
	"bufio"
	"fmt"
	"io"
	"log"
)

//Kind is the kind of messages sent/recv thru codec
type Kind byte

const (
	Request Kind = iota
	Response
	Error
	NumKind
)

var kindNames = []string{"req", "resp", "error", "undefined"}

func (k Kind) String() string {
	return kindNames[k]
}

//Header is the header of messages sent/recv
type Header struct {
	Kind          Kind
	ServiceMethod string
	Seq           uint64
	Info          interface{}
	next          *Header
}

func (h Header) String() string {
	return fmt.Sprintf("%s: %s: %d", h.Kind, h.ServiceMethod, h.Seq)
}

// Codec is the transport thru which messages are sent/recv
type Codec interface {
	WriteHeaderBody(header *Header, body interface{}) error
	Close() error
	ReadHeader(header *Header) error
	ReadBody(body interface{}) error
}

type codec struct {
	rwc    io.ReadWriteCloser
	dec    Decoder
	enc    Encoder
	encBuf *bufio.Writer
	closed bool //???do we need this???
}

func NewCodec(rwc io.ReadWriteCloser, encdec EncDecoder) Codec {
	buf := bufio.NewWriter(rwc)
	c := &codec{
		rwc:    rwc,
		dec:    encdec.NewDecoder(rwc),
		enc:    encdec.NewEncoder(buf),
		encBuf: buf,
	}
	return c
}

func (c *codec) WriteHeaderBody(header *Header, body interface{}) (err error) {
	if err = c.enc.Encode(header); err != nil {
		//should we shutdown in case error? only for server side send
		if /*header.Kind!=Request &&*/ c.encBuf.Flush() == nil {
			// Gob couldn't encode the header. Should not happen, so if it does,
			// shut down the connection to signal that the connection is broken.
			if debugLog {
				log.Println("rpc: gob error encoding header:", err)
			}
			c.Close()
		}
		return
	}
	if err = c.enc.Encode(body); err != nil {
		//should we shutdown in case error? only for server side send
		if /*header.Kind!=Request &&*/ c.encBuf.Flush() == nil {
			// Was a gob problem encoding the body but the header has been written.
			// Shut down the connection to signal that the connection is broken.
			if debugLog {
				log.Println("rpc: gob error encoding body:", err)
			}
			c.Close()
		}
		return
	}
	return c.encBuf.Flush()
}

func (c *codec) ReadHeader(header *Header) error {
	return c.dec.Decode(header)
}

func (c *codec) ReadBody(body interface{}) error {
	return c.dec.Decode(body)
}

func (c *codec) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	c.encBuf.Flush()
	return c.rwc.Close()
}
