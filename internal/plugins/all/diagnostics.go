//go:build !no_diagnostics

package all

import (
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/diagnostics"
)

func init() { plugin.Register(diagnostics.Plugin{}) }
