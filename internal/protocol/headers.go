package protocol

import "fmt"

// Well-known header names. Their table position is their wire identifier, so
// the order here must never change.
const (
	HeaderContentType     = "content-type"
	HeaderContentLength   = "content-length"
	HeaderServer          = "server"
	HeaderDate            = "date"
	HeaderConnection      = "connection"
	HeaderContentEncoding = "content-encoding"
	HeaderCacheControl    = "cache-control"
	HeaderLastModified    = "last-modified"
)

var tableNames = [...]string{
	1: HeaderContentType,
	2: HeaderContentLength,
	3: HeaderServer,
	4: HeaderDate,
	5: HeaderConnection,
	6: HeaderContentEncoding,
	7: HeaderCacheControl,
	8: HeaderLastModified,
}

var tableIDs = func() map[string]uint8 {
	m := make(map[string]uint8, len(tableNames))
	for id, name := range tableNames {
		if name != "" {
			m[name] = uint8(id)
		}
	}
	return m
}()

// Header values are raw bytes: nothing on the wire promises they are text.
type Header struct {
	Name  string
	Value []byte
}

func NewHeader(name, value string) Header {
	return Header{Name: name, Value: []byte(value)}
}

// Get returns the first value for name, since repeated headers keep their order.
func Get(headers []Header, name string) ([]byte, bool) {
	for _, h := range headers {
		if h.Name == name {
			return h.Value, true
		}
	}
	return nil, false
}

func validCustomName(name string) bool {
	if len(name) == 0 || len(name) > MaxCustomNameLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

func appendHeaders(dst []byte, headers []Header) ([]byte, error) {
	if len(headers) > MaxHeaders {
		return nil, fmt.Errorf("%d headers exceed the limit of %d", len(headers), MaxHeaders)
	}
	dst = append(dst, byte(len(headers)))
	for _, h := range headers {
		if len(h.Value) > maxHeaderValueLen {
			return nil, fmt.Errorf("value of header %q is %d bytes, over the limit", h.Name, len(h.Value))
		}
		if id, ok := tableIDs[h.Name]; ok {
			dst = append(dst, id)
		} else {
			if !validCustomName(h.Name) {
				return nil, fmt.Errorf("invalid header name %q", h.Name)
			}
			dst = append(dst, 0)
			dst = appendU16(dst, uint16(len(h.Name)))
			dst = append(dst, h.Name...)
		}
		dst = appendU16(dst, uint16(len(h.Value)))
		dst = append(dst, h.Value...)
	}
	return dst, nil
}

// readHeaders parses the count byte and the headers behind it. Headers whose
// identifier we do not know are still consumed, so a newer peer's additions
// cannot knock us out of step, but they are not returned.
func readHeaders(c *cursor) ([]Header, error) {
	count, err := c.u8()
	if err != nil {
		return nil, err
	}
	// Each header needs at least three bytes, so a count that cannot possibly
	// fit is rejected before allocating for it.
	if int(count)*3 > c.remaining() {
		return nil, malformed("%d headers announced but only %d bytes remain", count, c.remaining())
	}
	headers := make([]Header, 0, count)
	for i := 0; i < int(count); i++ {
		id, err := c.u8()
		if err != nil {
			return nil, err
		}

		var name string
		known := true
		switch {
		case id == 0:
			nameLen, err := c.u16()
			if err != nil {
				return nil, err
			}
			raw, err := c.take(int(nameLen))
			if err != nil {
				return nil, err
			}
			name = string(raw)
			if !validCustomName(name) {
				return nil, malformed("custom header name of %d bytes is not allowed", nameLen)
			}
			if _, clash := tableIDs[name]; clash {
				return nil, malformed("header %q must use its table identifier", name)
			}
		case int(id) < len(tableNames):
			name = tableNames[id]
		default:
			known = false
		}

		valueLen, err := c.u16()
		if err != nil {
			return nil, err
		}
		value, err := c.take(int(valueLen))
		if err != nil {
			return nil, err
		}
		if known {
			headers = append(headers, Header{Name: name, Value: value})
		}
	}
	return headers, nil
}
