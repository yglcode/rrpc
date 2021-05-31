package rrpc

import (
	"bufio"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"log"
)

//Kind is the kind of messages sent/recv thru codec
type Kind byte

const (
	Request Kind = iota
	RequestWithContext
	Response
	Cancel
	Error
	NumKind
)

var kindNames = []string{"Request", "RequestWithContext", "Response", "Cancel", "Error", "Undefined"}

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

type encoder interface {
	Encode(interface{}) error
}

type decoder interface {
	Decode(interface{}) error
}

type codec struct {
	rwc    io.ReadWriteCloser
	dec    decoder
	enc    encoder
	encBuf *bufio.Writer
	closed bool
}

// CodecMaker is a func to turn an io conn into Codec with
// specific marshaling schemes, such Gob, JSON, protobuf,...
type CodecMaker func(rwc io.ReadWriteCloser) Codec

// NewGobCodec returns a Codec to marshal data using Gob
func NewGobCodec(rwc io.ReadWriteCloser) Codec {
	buf := bufio.NewWriter(rwc)
	c := &codec{
		rwc:    rwc,
		dec:    gob.NewDecoder(rwc),
		enc:    gob.NewEncoder(buf),
		encBuf: buf,
	}
	return c
}

// NewJsonCodec returns a Codec to marshal data using JSON
func NewJsonCodec(rwc io.ReadWriteCloser) Codec {
	buf := bufio.NewWriter(rwc)
	c := &codec{
		rwc:    rwc,
		dec:    newJsonDecoder(rwc),
		enc:    newJsonEncoder(buf),
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

func newJsonEncoder(w io.Writer) encoder {
	return json.NewEncoder(w)
}

func newJsonDecoder(r io.Reader) decoder {
	dec := &discardJsonDecoder{json.NewDecoder(r)}
	dec.DisallowUnknownFields()
	return dec
}

// Following convention of gob decoder, if passed in v==nil,
// discard next value decoded
type discardJsonDecoder struct {
	*json.Decoder
}

func (djd *discardJsonDecoder) Decode(v interface{}) error {
	var discard interface{}
	if v == nil {
		v = &discard
	}
	return djd.Decoder.Decode(v)
}
