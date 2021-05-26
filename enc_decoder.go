package rrpc

import (
	"encoding/gob"
	"encoding/json"
	"io"
)

type Encoder interface {
	Encode(interface{}) error
}

type Decoder interface {
	Decode(interface{}) error
}

type EncDecoder interface {
	NewEncoder(w io.Writer) Encoder
	NewDecoder(r io.Reader) Decoder
}

type gobEncDecoder byte

var GobEncDecoder EncDecoder = gobEncDecoder(101)

func (gc gobEncDecoder) NewEncoder(w io.Writer) Encoder {
	return gob.NewEncoder(w)
}
func (gc gobEncDecoder) NewDecoder(r io.Reader) Decoder {
	return gob.NewDecoder(r)
}

type jsonEncDecoder byte

var JsonEncDecoder EncDecoder = jsonEncDecoder(102)

func (jc jsonEncDecoder) NewEncoder(w io.Writer) Encoder {
	return json.NewEncoder(w)
}
func (jc jsonEncDecoder) NewDecoder(r io.Reader) Decoder {
	return json.NewDecoder(r)
}
