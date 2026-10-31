//go:build !no_cftelemetry

package all

import (
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/cftelemetry"
)

func init() { plugin.RegisterTelemetry(cftelemetry.Plugin{}) }
