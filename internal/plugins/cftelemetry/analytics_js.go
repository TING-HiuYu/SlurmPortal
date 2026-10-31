//go:build js && wasm

package cftelemetry

import (
	"syscall/js"

	"github.com/TING-HiuYu/SlurmPortal/internal/telemetry"
)

// writeDataPoint forwards one request summary to globalThis.goV2Analytics, which the edge binds to an
// Analytics Engine dataset when configured.
func writeDataPoint(s telemetry.FinishedSpan) {
	fn := js.Global().Get("goV2Analytics")
	if fn.Type() != js.TypeFunction {
		return
	}
	route, status := "", 0.0
	for _, a := range s.Attrs {
		switch a.Key {
		case "http.route":
			route, _ = a.Value.(string)
		case "http.response.status_code":
			if v, ok := a.Value.(int); ok {
				status = float64(v)
			}
		}
	}
	errFlag := 0.0
	if s.Err != "" {
		errFlag = 1
	}
	fn.Invoke(map[string]any{
		"indexes": []any{route},
		"blobs":   []any{route, s.Name},
		"doubles": []any{float64(s.End.Sub(s.Start).Microseconds()) / 1000, status, errFlag},
	})
}
