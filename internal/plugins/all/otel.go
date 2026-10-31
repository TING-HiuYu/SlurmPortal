//go:build !no_otel

package all

import (
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/otel"
)

func init() { plugin.RegisterTelemetry(otel.Plugin{}) }
