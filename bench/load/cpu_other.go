//go:build !windows

package main

import "time"

// cpuTime: CPU sampling is only implemented for Windows (the benchmark host).
func cpuTime(pid int) time.Duration { return 0 }
