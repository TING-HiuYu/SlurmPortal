//go:build !no_currentstatus

package all

import (
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/currentstatus"
)

func init() { plugin.Register(currentstatus.Plugin{}) }
