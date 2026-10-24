package bus

import "syscall"

func changedNS(stat *syscall.Stat_t) int64 { return stat.Ctim.Nano() }
