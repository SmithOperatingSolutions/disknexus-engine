// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// A minimal XML property-list reader — enough for `diskutil -plist`:
// dict, array, string, integer, real, true, false, date (as string).

type plistDict map[string]any

func (d plistDict) str(key string) string {
	s, _ := d[key].(string)
	return s
}

func (d plistDict) boolean(key string) bool {
	b, _ := d[key].(bool)
	return b
}

func (d plistDict) integer(key string) int64 {
	switch v := d[key].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

func (d plistDict) dicts(key string) []plistDict {
	arr, _ := d[key].([]any)
	out := make([]plistDict, 0, len(arr))
	for _, x := range arr {
		if m, ok := x.(plistDict); ok {
			out = append(out, m)
		}
	}
	return out
}

func parsePlist(data []byte) (plistDict, error) {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("plist: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if se.Name.Local == "plist" {
			continue
		}
		v, err := plistValue(dec, se)
		if err != nil {
			return nil, err
		}
		d, ok := v.(plistDict)
		if !ok {
			return nil, fmt.Errorf("plist: top-level value is %T, want dict", v)
		}
		return d, nil
	}
}

func plistValue(dec *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict":
		d := plistDict{}
		key := ""
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, fmt.Errorf("plist dict: %w", err)
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "key" {
					var k string
					if err := dec.DecodeElement(&k, &t); err != nil {
						return nil, err
					}
					key = k
					continue
				}
				v, err := plistValue(dec, t)
				if err != nil {
					return nil, err
				}
				d[key] = v
			case xml.EndElement:
				return d, nil
			}
		}
	case "array":
		var arr []any
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, fmt.Errorf("plist array: %w", err)
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := plistValue(dec, t)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			case xml.EndElement:
				return arr, nil
			}
		}
	case "string", "date", "data":
		var s string
		if err := dec.DecodeElement(&s, &se); err != nil {
			return nil, err
		}
		return s, nil
	case "integer":
		var s string
		if err := dec.DecodeElement(&s, &se); err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("plist integer %q", s)
		}
		return n, nil
	case "real":
		var s string
		if err := dec.DecodeElement(&s, &se); err != nil {
			return nil, err
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("plist real %q", s)
		}
		return f, nil
	case "true", "false":
		if err := dec.Skip(); err != nil && err != io.EOF {
			return nil, err
		}
		return se.Name.Local == "true", nil
	}
	return nil, fmt.Errorf("plist: unsupported element <%s>", se.Name.Local)
}
