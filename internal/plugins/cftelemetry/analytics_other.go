//go:build !(js && wasm)

package cftelemetry

import "github.com/TING-HiuYu/SlurmPortal/internal/telemetry"

func writeDataPoint(telemetry.FinishedSpan) {}
