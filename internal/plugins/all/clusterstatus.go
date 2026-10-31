//go:build !no_clusterstatus

package all

import (
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/clusterstatus"
)

func init() { plugin.Register(clusterstatus.Plugin{}) }
