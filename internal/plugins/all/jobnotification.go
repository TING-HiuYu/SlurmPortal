//go:build !no_jobnotification

package all

import (
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/jobnotification"
)

func init() { plugin.Register(jobnotification.Plugin{}) }
