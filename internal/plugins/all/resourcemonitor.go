//go:build !no_resourcemonitor

package all

import (
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/resourcemonitor"
)

func init() { plugin.Register(resourcemonitor.Plugin{}) }
