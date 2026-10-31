//go:build !no_cfaccess

package all

import (
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/cfaccess"
)

func init() { plugin.Register(cfaccess.Plugin{}) }
