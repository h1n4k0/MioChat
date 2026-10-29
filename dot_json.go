package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Dedicated flat owner wire: reject unknown and duplicate fields instead of
// silently accepting an attempted account/callback/target override.
func (in *guiDotRequest) UnmarshalJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return errDotUnavailable
	}
	seen := map[string]bool{}
	for d.More() {
		token, e = d.Token()
		if e != nil {
			return e
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return errDotUnavailable
		}
		seen[key] = true
		var raw json.RawMessage
		if e = d.Decode(&raw); e != nil {
			return e
		}
	}
	if _, e = d.Token(); e != nil {
		return e
	}
	if _, e = d.Token(); !errors.Is(e, io.EOF) {
		return errDotUnavailable
	}
	type plain guiDotRequest
	var out plain
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e = d.Decode(&out); e != nil {
		return e
	}
	*in = guiDotRequest(out)
	return nil
}
