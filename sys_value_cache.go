package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SysValueCache struct {
	GetBootTime     func() (time.Time, error)
	GetSystemUptime func() (time.Duration, error)
	GetPageSize     func() (int, error)
}

func NewSysValueCache() *SysValueCache {
	return &SysValueCache{
		GetBootTime:     sync.OnceValues(readBootTime),
		GetSystemUptime: sync.OnceValues(readSystemUptime),
		GetPageSize:     sync.OnceValues(getPageSize),
	}
}

func readBootTime() (time.Time, error) {
	const filename = "/proc/stat"
	// btime 769041601
	//        boot time, in seconds since the Epoch, 1970-01-01
	//        00:00:00 +0000 (UTC).
	// https://man7.org/linux/man-pages/man5/proc_stat.5.html
	content, err := os.ReadFile(filename)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot read %s: %s", filename, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	const btimePrefix = "btime "
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, btimePrefix) {
			btime, err := strconv.ParseInt(line[len(btimePrefix):], 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("convert btime to int %s: %s", line, err)
			}
			return time.Unix(btime, 0), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return time.Time{}, err
	}
	return time.Time{}, fmt.Errorf("btime not found in %s", filename)
}

func readSystemUptime() (time.Duration, error) {
	const filename = "/proc/uptime"
	// This file contains two numbers (values in seconds): the
	// uptime of the system (including time spent in suspend) and
	// the amount of time spent in the idle process.
	// https://man7.org/linux/man-pages/man5/proc_uptime.5.html
	content, err := os.ReadFile(filename)
	if err != nil {
		return 0, fmt.Errorf("cannot read %s: %s", filename, err)
	}
	uptimeSecsBytes, _, found := bytes.Cut(content, []byte{' '})
	if !found {
		return 0, fmt.Errorf("unexpected formatted content in %s: content=%s",
			filename, string(content))
	}
	uptimeSecs, err := strconv.ParseFloat(string(uptimeSecsBytes), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid uptime value in %s: content=%s",
			filename, string(content))
	}
	return time.Duration(uptimeSecs * float64(time.Second)), nil
}

func getPageSize() (int, error) {
	cmd := exec.Command("getconf", "PAGESIZE")
	outputBytes, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(bytes.TrimSuffix(outputBytes, []byte{'\n'})))
}
