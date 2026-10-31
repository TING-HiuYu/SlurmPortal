//go:build !no_mailpool

package all

import (
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/mailpool"
)

func init() { plugin.Register(mailpool.Plugin{}) }
