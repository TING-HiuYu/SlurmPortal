//go:build !no_resourcestatus

package all

import (
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/resourcestatus"
)

func init() { plugin.Register(resourcestatus.Plugin{}) }
