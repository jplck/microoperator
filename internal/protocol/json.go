package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// DecodeJSON bounds nesting and rejects ambiguous duplicate/case-variant keys
// before decoding into explicit types. Parser errors intentionally do not echo
// submitted values, which could contain an accidentally pasted credential.
func DecodeJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errors.New("JSON must be valid UTF-8")
	}
	scanner := json.NewDecoder(bytes.NewReader(data))
	scanner.UseNumber()
	if err := ScanJSONValue(scanner, 0); err != nil {
		return errors.New("JSON contains invalid, duplicate, or incorrectly cased fields")
	}
	if _, err := scanner.Token(); !errors.Is(err, io.EOF) {
		return errors.New("JSON must contain exactly one value")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("JSON contains an unknown field or an invalid field type")
	}
	return nil
}

func ScanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON nesting limit exceeded")
	}
	value, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := value.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || seen[key] || key != strings.ToLower(key) {
				return errors.New("invalid object key")
			}
			seen[key] = true
			if err := ScanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := ScanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected delimiter")
	}
	_, err = decoder.Token()
	return err
}
