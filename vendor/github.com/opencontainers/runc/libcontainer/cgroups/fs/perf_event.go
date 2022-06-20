// +build linux

package fs

import (
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/configs"
)

type PerfEventGroup struct {
}

func (s *PerfEventGroup) Name() string {
	return "perf_event"
}

// 创建"perf_event" subsystem cgroup子系统路径
// 将d.pid（不为-1时）加入到"perf_event" subsystem cgroup下的"cgroup.procs"文件
func (s *PerfEventGroup) Apply(d *cgroupData) error {
	// we just want to join this group even though we don't set anything
	if _, err := d.join("perf_event"); err != nil && !cgroups.IsNotFound(err) {
		return err
	}
	return nil
}

func (s *PerfEventGroup) Set(path string, cgroup *configs.Cgroup) error {
	return nil
}

func (s *PerfEventGroup) Remove(d *cgroupData) error {
	return removePath(d.path("perf_event"))
}

func (s *PerfEventGroup) GetStats(path string, stats *cgroups.Stats) error {
	return nil
}
