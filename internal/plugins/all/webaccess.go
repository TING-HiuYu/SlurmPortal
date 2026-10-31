//go:build !no_webaccess

package all

import (
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/webaccess"
)

func init() { plugin.Register(webaccess.Plugin{}) }
