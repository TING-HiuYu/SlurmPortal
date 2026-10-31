package jobnotification

var variables = func() []Variable {
	pairs := [][2]string{
		{"user.nickname", "用户昵称"}, {"user.username", "用户名"}, {"user.mail", "用户邮箱"},
		{"job.job_id", "作业编号"}, {"job.sluid", "执行标识"}, {"job.name", "作业名称"},
		{"job.duration", "最后一次执行的有效运行时长"}, {"job.array", "数组信息"},
		{"job.heterogeneous", "异构组件信息"},
		{"job.requested_tres", "申请的 TRES 原始字符串；billing 是资源权重，非费用或实际用量"},
		{"job.allocated_tres", "分配的 TRES 原始字符串，非实际用量"},
		{"job.stdout_expanded", "Slurm 展开后的标准输出路径"}, {"job.standard_output", "标准输出原始路径模板，可含 %j"},
		{"job.batch_host", "批处理主机"}, {"job.command", "作业命令或脚本路径"},
		{"job.working_directory", "作业工作目录"}, {"job.submit_line", "提交命令行"},
		{"job.nodes", "节点列表表达式"}, {"job.partition", "分区"}, {"job.account", "Slurm 账户"}, {"job.qos", "服务质量 QOS"},
		{"job.stderr_expanded", "Slurm 展开后的标准错误路径；明确空串保留"},
		{"job.standard_error", "标准错误原始路径模板；空串不代表没有错误"},
		{"job.stdin_expanded", "Slurm 展开后的标准输入路径"}, {"job.standard_input", "标准输入原始路径模板"},
		{"job.cpus_per_tres", "每个 TRES 的 CPU 配额原始字符串"},
		{"job.memory_per_tres", "每个 TRES 的内存配额原始字符串，非 GPU 显存用量"},
		{"job.tres_per_node", "每个节点的 TRES 原始字符串"},
		{"job.user_name", "Slurm 作业用户名；收件人仍按 UID 查询"}, {"job.group_name", "Slurm 作业用户组"},
		{"job.cpus", "作业 CPU 数"}, {"job.node_count", "节点数"}, {"job.tasks", "任务数"},
		{"job.cpus_per_task", "每个任务的 CPU 数"}, {"job.memory_per_node_mib", "每个节点的申请内存（MiB）"},
		{"job.memory_per_cpu_mib", "每个 CPU 的申请内存（MiB）"}, {"job.priority", "作业优先级"},
		{"job.restart_count", "重启次数"}, {"job.group_id", "Slurm 用户组 GID"},
		{"job.submit_time", "提交时间（Unix 秒）"}, {"job.start_time", "开始时间（Unix 秒）"}, {"job.end_time", "结束时间（Unix 秒）"},
		{"job.time_limit", "运行时限（UNLIMITED 或分钟）"},
		{"job.submitted_at", "提交时间（RFC3339 UTC）"}, {"job.started_at", "开始时间（RFC3339 UTC）"},
		{"job.ended_at", "结束时间（RFC3339 UTC）"},
		{"job.submitted_at_local", "提交时间（配置的 IANA 时区）"}, {"job.started_at_local", "开始时间（配置的 IANA 时区）"},
		{"job.ended_at_local", "结束时间（配置的 IANA 时区）"},
		{"job.metadata_complete", "摘要复制时未因类型或大小丢失字段；不表示上游提供了所有字段"},
		{"job.metadata_omitted", "因类型或大小省略的 job 字段名列表；小报文预算下可省略此列表"},
		{"cluster.id", "可信集群标识"},
		{"cluster.display_name", "集群名称"}, {"result.state", "终态"}, {"result.exit_code", "退出码"},
		{"duration.active_seconds", "有效运行秒数，扣除暂停"}, {"observed_at", "通知观察时间"},
		{"usage.gpu.mean_utilization_percent", "GPU 时间加权平均利用率（百分比）；无有效采样时为空"},
		{"usage.gpu.mean_memory_utilization_percent", "显存容量平均占用率（百分比）；无有效采样时为空"},
		{"usage.gpu.complete", "GPU 统计是否完整；false 时结合缺失原因和覆盖时长判断"},
		{"usage.gpu.allocated_gpu_seconds", "分配的 GPU·秒"},
		{"usage.gpu.observed_gpu_seconds", "有效采样覆盖的 GPU·秒"},
		{"usage.gpu.missing_reasons", "GPU 统计缺失或不完整原因"},
		{"observed_at_local", "通知观察时间（配置的 IANA 时区）"},
		{"constants.support_email", "管理员自定义常量示例"},
	}
	out := make([]Variable, len(pairs))
	for i, p := range pairs {
		out[i] = Variable{Name: p[0], Description: p[1]}
	}
	return out
}()

const previewCluster = "example-cluster"

const previewEventJSON = `{"version":1,"event_id":"preview/s0000000000001/ended","cluster_id":"example-cluster",
"job":{"job_id":12345,"sluid":"s0000000000001","uid":10000,"name":"示例作业","array":null,"heterogeneous":null,
"requested_tres":"cpu=30,mem=62G,node=1,billing=30,gres/gpu=1","allocated_tres":"cpu=30,mem=62G,node=1,billing=30,gres/gpu=1",
"stdout_expanded":"/home/example-user/completion/logs/example-sleep61.12345.out",
"standard_output":"/home/example-user/completion/logs/example-sleep61.%j.out",
"batch_host":"node01","command":"/home/example-user/completion/runs/example-sleep61.sbatch",
"working_directory":"/home/example-user",
"submit_line":"sbatch /home/example-user/completion/runs/example-sleep61.sbatch",
"nodes":"node01","partition":"gpu","account":"example-user","qos":"normal",
"stderr_expanded":"","standard_error":"","stdin_expanded":"/dev/null","standard_input":"/dev/null",
"cpus_per_tres":"gres/gpu:30","memory_per_tres":"gres/gpu:63488","tres_per_node":"gres/gpu:1",
"user_name":"example-user","group_name":"sshusers","cpus":30,"node_count":1,"tasks":1,"cpus_per_task":1,
"memory_per_node_mib":63488,"priority":1,"restart_count":0,"group_id":10000,
"submit_time":1792886400,"start_time":1792886400,"end_time":1792890001,"time_limit":"UNLIMITED",
"submitted_at":"2026-10-25T00:00:00Z","started_at":"2026-10-25T00:00:00Z","ended_at":"2026-10-25T01:00:01Z",
"metadata_complete":true,"metadata_omitted":[]},
"result":{"state":"COMPLETED","exit_code":{"status":0,"signal":0}},
"duration":{"active_seconds":3601,"scope":"last_attempt","excludes_suspend":true},"raw":{"controller":{},"accounting":null},
"usage":{"gpu":{"allocated_gpu_seconds":3601,"observed_gpu_seconds":3601,"utilization_percent_seconds":275476.5,
"memory_utilization_percent_seconds":148361.2,"mean_utilization_percent":76.5,"mean_memory_utilization_percent":41.2,
"complete":true,"missing_reasons":[]}},
"observed_at":"2026-10-25T04:00:00Z"}`
