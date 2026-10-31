//go:build !no_oauth

package all

import (
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/oauth"
)

func init() { plugin.Register(oauth.Plugin{}) }
