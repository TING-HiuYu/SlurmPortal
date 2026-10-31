package resourcestatus

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// slurmNumber reads a Slurm JSON integer, plain or {set, infinite, number}; unset and infinite are unknown.
func slurmNumber(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, false
	}
	if raw[0] == '{' {
		var w struct {
			Set      bool            `json:"set"`
			Infinite bool            `json:"infinite"`
			Number   json.RawMessage `json:"number"`
		}
		if json.Unmarshal(raw, &w) != nil || !w.Set || w.Infinite {
			return 0, false
		}
		raw = bytes.TrimSpace(w.Number)
	}
	if len(raw) == 0 || raw[0] == '"' {
		return 0, false
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	return i, err == nil
}
