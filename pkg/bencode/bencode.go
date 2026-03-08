// Package bencode implements encoding and decoding of bencoded data.
//
// BEP 3: The BitTorrent Protocol Specification - Bencoding
// https://www.bittorrent.org/beps/bep_0003.html
//
// Bencode is the encoding used by the BitTorrent protocol for .torrent files,
// tracker responses, and DHT messages.
//
// Bencode supports four data types:
//   - Byte strings: <length>:<contents> (e.g., "4:spam")
//   - Integers: i<number>e (e.g., "i42e")
//   - Lists: l<contents>e (e.g., "l4:spam4:eggse")
//   - Dictionaries: d<contents>e with sorted keys (e.g., "d3:cow3:mooe")
package bencode

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// RawMessage is a raw encoded bencode value.
// It can be used to delay bencode decoding or to encode pre-computed bencode.
type RawMessage []byte

// Common errors
var (
	ErrInvalidBencode = errors.New("invalid bencode format")
	ErrInvalidType    = errors.New("cannot encode/decode type")
)

// Cached reflect.Type to avoid allocation on every decodeValue call.
var rawMessageType = reflect.TypeOf((*RawMessage)(nil)).Elem()

// =============================================================================
// Decoder
// =============================================================================

// Decoder reads and decodes bencoded data from an input stream.
type Decoder struct {
	r   *bufio.Reader
	n   int  // bytes parsed
	raw bool // when true, accumulate raw bytes
	buf []byte
}

// NewDecoder returns a new decoder that reads from r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReader(r)}
}

// BytesParsed returns the number of bytes that have been parsed.
// This is useful for finding where bencode data ends in a payload.
func (d *Decoder) BytesParsed() int {
	return d.n
}

// Decode reads the next bencoded value and stores it in the value pointed to by v.
func (d *Decoder) Decode(v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return errors.New("bencode: Decode requires a non-nil pointer")
	}
	return d.decodeValue(rv.Elem())
}

func (d *Decoder) readByte() (byte, error) {
	b, err := d.r.ReadByte()
	if err == nil {
		d.n++
		if d.raw {
			d.buf = append(d.buf, b)
		}
	}
	return b, err
}

func (d *Decoder) readBytes(delim byte) ([]byte, error) {
	line, err := d.r.ReadBytes(delim)
	d.n += len(line)
	if d.raw {
		d.buf = append(d.buf, line...)
	}
	return line, err
}

func (d *Decoder) readFull(p []byte) (int, error) {
	n, err := io.ReadFull(d.r, p)
	d.n += n
	if d.raw {
		d.buf = append(d.buf, p[:n]...)
	}
	return n, err
}

func (d *Decoder) peek() (byte, error) {
	b, err := d.r.Peek(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (d *Decoder) decodeValue(v reflect.Value) error {
	// Handle RawMessage specially
	if v.Type() == rawMessageType {
		return d.decodeRaw(v)
	}

	// Handle pointers
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		return d.decodeValue(v.Elem())
	}

	b, err := d.peek()
	if err != nil {
		return err
	}

	switch {
	case b == 'i':
		return d.decodeInt(v)
	case b >= '0' && b <= '9':
		return d.decodeString(v)
	case b == 'l':
		return d.decodeList(v)
	case b == 'd':
		return d.decodeDict(v)
	default:
		return fmt.Errorf("bencode: invalid token %q", b)
	}
}

func (d *Decoder) decodeRaw(v reflect.Value) error {
	d.buf = d.buf[:0]
	d.raw = true
	defer func() { d.raw = false }()

	// Decode into throwaway interface to consume the value
	var x any
	if err := d.decodeValue(reflect.ValueOf(&x).Elem()); err != nil {
		return err
	}
	v.SetBytes(append([]byte(nil), d.buf...))
	return nil
}

func (d *Decoder) decodeInt(v reflect.Value) error {
	// Read 'i'
	if _, err := d.readByte(); err != nil {
		return err
	}

	// Read until 'e'
	line, err := d.readBytes('e')
	if err != nil {
		return err
	}

	numStr := string(line[:len(line)-1])

	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(numStr, 10, 64)
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(numStr, 10, 64)
		if err != nil {
			return err
		}
		v.SetUint(n)
	case reflect.Bool:
		n, err := strconv.ParseInt(numStr, 10, 64)
		if err != nil {
			return err
		}
		v.SetBool(n != 0)
	case reflect.Interface:
		n, err := strconv.ParseInt(numStr, 10, 64)
		if err != nil {
			return err
		}
		v.Set(reflect.ValueOf(n))
	default:
		return fmt.Errorf("bencode: cannot decode int into %s", v.Type())
	}
	return nil
}

func (d *Decoder) decodeString(v reflect.Value) error {
	// Read length until ':'
	line, err := d.readBytes(':')
	if err != nil {
		return err
	}

	length, err := strconv.Atoi(string(line[:len(line)-1]))
	if err != nil {
		return err
	}
	if length < 0 {
		return errors.New("bencode: negative string length")
	}

	// Read string bytes
	buf := make([]byte, length)
	if _, err := d.readFull(buf); err != nil {
		return err
	}

	switch v.Kind() {
	case reflect.String:
		v.SetString(string(buf))
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			v.SetBytes(buf)
		} else {
			return fmt.Errorf("bencode: cannot decode string into %s", v.Type())
		}
	case reflect.Interface:
		v.Set(reflect.ValueOf(string(buf)))
	default:
		return fmt.Errorf("bencode: cannot decode string into %s", v.Type())
	}
	return nil
}

func (d *Decoder) decodeList(v reflect.Value) error {
	// Read 'l'
	if _, err := d.readByte(); err != nil {
		return err
	}

	// Handle interface{}
	if v.Kind() == reflect.Interface {
		var list []any
		for {
			b, err := d.peek()
			if err != nil {
				return err
			}
			if b == 'e' {
				d.readByte()
				v.Set(reflect.ValueOf(list))
				return nil
			}
			var item any
			if err := d.decodeValue(reflect.ValueOf(&item).Elem()); err != nil {
				return err
			}
			list = append(list, item)
		}
	}

	if v.Kind() != reflect.Slice && v.Kind() != reflect.Array {
		return fmt.Errorf("bencode: cannot decode list into %s", v.Type())
	}

	i := 0
	for {
		b, err := d.peek()
		if err != nil {
			return err
		}
		if b == 'e' {
			d.readByte()
			return nil
		}

		// Grow slice if needed
		if v.Kind() == reflect.Slice {
			if i >= v.Cap() {
				newcap := v.Cap() * 2
				if newcap < 4 {
					newcap = 4
				}
				newv := reflect.MakeSlice(v.Type(), v.Len(), newcap)
				reflect.Copy(newv, v)
				v.Set(newv)
			}
			if i >= v.Len() {
				v.SetLen(i + 1)
			}
		}

		if err := d.decodeValue(v.Index(i)); err != nil {
			return err
		}
		i++
	}
}

func (d *Decoder) decodeDict(v reflect.Value) error {
	// Read 'd'
	if _, err := d.readByte(); err != nil {
		return err
	}

	// Handle interface{}
	if v.Kind() == reflect.Interface {
		dict := make(map[string]any)
		for {
			b, err := d.peek()
			if err != nil {
				return err
			}
			if b == 'e' {
				d.readByte()
				v.Set(reflect.ValueOf(dict))
				return nil
			}
			var key string
			if err := d.decodeString(reflect.ValueOf(&key).Elem()); err != nil {
				return err
			}
			var val any
			if err := d.decodeValue(reflect.ValueOf(&val).Elem()); err != nil {
				return err
			}
			dict[key] = val
		}
	}

	switch v.Kind() {
	case reflect.Map:
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		return d.decodeDictToMap(v)
	case reflect.Struct:
		return d.decodeDictToStruct(v)
	default:
		return fmt.Errorf("bencode: cannot decode dict into %s", v.Type())
	}
}

func (d *Decoder) decodeDictToMap(v reflect.Value) error {
	elemType := v.Type().Elem()
	for {
		b, err := d.peek()
		if err != nil {
			return err
		}
		if b == 'e' {
			d.readByte()
			return nil
		}

		var key string
		if err := d.decodeString(reflect.ValueOf(&key).Elem()); err != nil {
			return err
		}

		elem := reflect.New(elemType).Elem()
		if err := d.decodeValue(elem); err != nil {
			return err
		}
		v.SetMapIndex(reflect.ValueOf(key), elem)
	}
}

func (d *Decoder) decodeDictToStruct(v reflect.Value) error {
	fields := getStructFields(v.Type())
	for {
		b, err := d.peek()
		if err != nil {
			return err
		}
		if b == 'e' {
			d.readByte()
			return nil
		}

		var key string
		if err := d.decodeString(reflect.ValueOf(&key).Elem()); err != nil {
			return err
		}

		if field, ok := fields[key]; ok {
			fv := v.FieldByIndex(field.Index)
			if err := d.decodeValue(fv); err != nil {
				return err
			}
		} else {
			// Skip unknown field
			var skip any
			if err := d.decodeValue(reflect.ValueOf(&skip).Elem()); err != nil {
				return err
			}
		}
	}
}

// =============================================================================
// Encoder
// =============================================================================

// Encoder writes bencoded data to an output stream.
type Encoder struct {
	w io.Writer
}

// NewEncoder returns a new encoder that writes to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// Encode writes the bencoded representation of v to the stream.
func (e *Encoder) Encode(v any) error {
	return e.encodeValue(reflect.ValueOf(v))
}

func (e *Encoder) encodeValue(v reflect.Value) error {
	// Handle pointers and interfaces
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}

	// Handle RawMessage
	if rm, ok := v.Interface().(RawMessage); ok {
		_, err := e.w.Write(rm)
		return err
	}

	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		_, err := fmt.Fprintf(e.w, "i%de", v.Int())
		return err

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		_, err := fmt.Fprintf(e.w, "i%de", v.Uint())
		return err

	case reflect.Bool:
		n := 0
		if v.Bool() {
			n = 1
		}
		_, err := fmt.Fprintf(e.w, "i%de", n)
		return err

	case reflect.String:
		s := v.String()
		_, err := fmt.Fprintf(e.w, "%d:%s", len(s), s)
		return err

	case reflect.Slice, reflect.Array:
		// Handle []byte as string
		if v.Type().Elem().Kind() == reflect.Uint8 {
			b := v.Bytes()
			_, err := fmt.Fprintf(e.w, "%d:", len(b))
			if err != nil {
				return err
			}
			_, err = e.w.Write(b)
			return err
		}
		return e.encodeList(v)

	case reflect.Map:
		return e.encodeMap(v)

	case reflect.Struct:
		return e.encodeStruct(v)

	default:
		return fmt.Errorf("bencode: cannot encode %s", v.Type())
	}
}

func (e *Encoder) encodeList(v reflect.Value) error {
	if _, err := e.w.Write([]byte{'l'}); err != nil {
		return err
	}
	for i := 0; i < v.Len(); i++ {
		if err := e.encodeValue(v.Index(i)); err != nil {
			return err
		}
	}
	_, err := e.w.Write([]byte{'e'})
	return err
}

func (e *Encoder) encodeMap(v reflect.Value) error {
	if _, err := e.w.Write([]byte{'d'}); err != nil {
		return err
	}

	// Sort keys
	keys := v.MapKeys()
	sortedKeys := make([]string, len(keys))
	for i, k := range keys {
		sortedKeys[i] = k.String()
	}
	sort.Strings(sortedKeys)

	for _, key := range sortedKeys {
		val := v.MapIndex(reflect.ValueOf(key))
		if isNilValue(val) {
			continue
		}
		// Write key
		if _, err := fmt.Fprintf(e.w, "%d:%s", len(key), key); err != nil {
			return err
		}
		// Write value
		if err := e.encodeValue(val); err != nil {
			return err
		}
	}

	_, err := e.w.Write([]byte{'e'})
	return err
}

func (e *Encoder) encodeStruct(v reflect.Value) error {
	if _, err := e.w.Write([]byte{'d'}); err != nil {
		return err
	}

	// Build sorted key-value pairs
	type kv struct {
		key string
		val reflect.Value
	}
	var pairs []kv

	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		fv := v.Field(i)

		// Skip unexported fields
		if !field.IsExported() {
			continue
		}

		// Parse tag
		tag := field.Tag.Get("bencode")
		if tag == "-" {
			continue
		}

		name, opts := parseTag(tag)
		if name == "" {
			name = field.Name
		}

		// Handle omitempty
		if opts.contains("omitempty") && isEmptyValue(fv) {
			continue
		}

		// Skip nil values
		if isNilValue(fv) {
			continue
		}

		pairs = append(pairs, kv{key: name, val: fv})
	}

	// Sort by key
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].key < pairs[j].key
	})

	// Encode pairs
	for _, p := range pairs {
		if _, err := fmt.Fprintf(e.w, "%d:%s", len(p.key), p.key); err != nil {
			return err
		}
		if err := e.encodeValue(p.val); err != nil {
			return err
		}
	}

	_, err := e.w.Write([]byte{'e'})
	return err
}

// =============================================================================
// Helper functions
// =============================================================================

// DecodeBytes decodes bencoded data from a byte slice.
func DecodeBytes(data []byte, v any) error {
	return NewDecoder(bytes.NewReader(data)).Decode(v)
}

// DecodeString decodes bencoded data from a string.
func DecodeString(data string, v any) error {
	return NewDecoder(strings.NewReader(data)).Decode(v)
}

// EncodeBytes encodes v as bencode and returns the bytes.
func EncodeBytes(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// EncodeString encodes v as bencode and returns a string.
func EncodeString(v any) (string, error) {
	b, err := EncodeBytes(v)
	return string(b), err
}

// =============================================================================
// Tag parsing
// =============================================================================

type tagOptions string

func parseTag(tag string) (string, tagOptions) {
	if idx := strings.Index(tag, ","); idx != -1 {
		return tag[:idx], tagOptions(tag[idx+1:])
	}
	return tag, ""
}

func (o tagOptions) contains(opt string) bool {
	s := string(o)
	for s != "" {
		var next string
		if i := strings.Index(s, ","); i >= 0 {
			s, next = s[:i], s[i+1:]
		}
		if s == opt {
			return true
		}
		s = next
	}
	return false
}

// =============================================================================
// Struct field cache
// =============================================================================

type structField struct {
	Index []int
	Name  string
}

// structFieldCache avoids re-reflecting struct types on every decode.
var structFieldCache sync.Map // map[reflect.Type]map[string]structField

func getStructFields(t reflect.Type) map[string]structField {
	if cached, ok := structFieldCache.Load(t); ok {
		return cached.(map[string]structField)
	}
	fields := make(map[string]structField)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue // skip unexported
		}

		tag := f.Tag.Get("bencode")
		if tag == "-" {
			continue
		}

		name, _ := parseTag(tag)
		if name == "" {
			name = f.Name
		}

		if isValidTag(name) {
			fields[name] = structField{Index: f.Index, Name: name}
		}
	}
	structFieldCache.Store(t, fields)
	return fields
}

func isValidTag(key string) bool {
	if key == "" {
		return false
	}
	for _, c := range key {
		if c != ' ' && c != '$' && c != '-' && c != '_' && c != '.' && !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			return false
		}
	}
	return true
}

func isEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Ptr:
		return v.IsNil()
	}
	return false
}

func isNilValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Interface, reflect.Ptr, reflect.Map, reflect.Slice:
		return v.IsNil()
	}
	return false
}

// Marshal encodes v as bencode and returns the bytes.
// This is an alias for EncodeBytes.
func Marshal(v any) ([]byte, error) {
	return EncodeBytes(v)
}

// Unmarshal decodes a bencoded byte slice into v.
// This is an alias for DecodeBytes.
func Unmarshal(data []byte, v any) error {
	return DecodeBytes(data, v)
}
