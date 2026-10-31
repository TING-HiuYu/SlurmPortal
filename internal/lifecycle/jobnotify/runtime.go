package jobnotify

// activeRuntimeSeconds follows Slurm 26.05 src/scontrol/info_job.c RunTime.
// It accounts only for this execution, excluding suspended time. Identity,
// allocation, terminal-state and threshold checks belong to the caller.
func activeRuntimeSeconds(start, end, suspend, pre int64) (int64, bool) {
	if start < 0 || end < start || end-start > (1<<53)-1 || suspend < 0 || pre < 0 {
		return 0, false
	}
	if start == 0 {
		return 0, true
	}
	if suspend != 0 {
		if suspend < start || suspend > end || pre > suspend-start {
			return 0, false
		}
		return end - suspend + pre, true
	}
	return end - start, true
}
