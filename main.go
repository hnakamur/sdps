package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"os"
	"os/exec"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"
	"github.com/dustin/go-humanize"
)

const cliName = `sdps`
const description = `"` + cliName + `" is an alternative "ps" command specifically designed for processes within systemd services.

Its name is an abbreviation of "systemd ps".

It's not a full replacement for "ps", but rather focuses on a core subset of functionality to serve two use cases:

* Displaying Human-Readable Process Information.

  View process details in a format easy for humans to read.

  # ` + cliName + ` -s nginx,trafficserver

  # ` + cliName + ` -s nginx,trafficserver -c pid,vsz,rss,start,command -f start=humanRelTime

* Outputting Single Values for Monitoring/Scripting:

  Extract a single process value, ideal for integration with monitoring software or for use in scripts.

  # ` + cliName + ` -s nginx -l 'nginx: worker' -c uptime -f uptime=seconds -g min --no-header

  # ` + cliName + ` -s nginx -l 'nginx: master' -c pid --no-header

"` + cliName + `" works solely on Linux systems running systemd.
`

var cliVars = kong.Vars{
	"column_default": `pid,ppid,pcpu,vsz,rss,start,uptime,command`,
	"column_help": `Columns to display in the output. Available columns: ` +
		`"pid", "ppid", "pcpu", "vsz", "rss", "start", "uptime", and "command".`,
	"format_default": `vsz=iBytes;rss=iBytes;start=format "2006-01-02 15:04";uptime=duration`,
	"format_help": `Specify formatting functions for column values. Uses Go's text/template syntax after "|". ` +
		`Available functions: "iBytes" for "vsz" and "rss", "format" or "humanRelTime" for "start", ` +
		`"duration" or "seconds" for "uptime". ` +
		`For "duration" units: "y" = 365.25 days, "M" = 30.4375 days, "d" = 24 hours. ` +
		`For "format" layout details, see https://pkg.go.dev/time@latest#Layout.`,
	"align_help":         `Override default column alignments. L (Left) or R (right).`,
	"default_align_help": `Set the default alignment for all columns. L (Left) or R (right).`,
	"agg_help": `Aggregate a single column value from processes. Currently, only ` +
		`"--column=uptime --agg=min" is supported.`,
}

var cli CLI

type CLI struct {
	Service []string `group:"process" short:"s" required:"" xor:"entry" help:"Specify systemd service name(s)."`
	Filter  string   `group:"process" short:"l" help:"Filter processes by their command line."`

	Column       []string          `group:"output" short:"c" default:"${column_default}" env:"SDPS_COLUMN" help:"${column_help}"`
	Format       map[string]string `group:"output" short:"f" default:"${format_default}" env:"SDPS_FORMAT" help:"${format_help}"`
	DefaultAlign string            `group:"output" short:"d" default:"R" env:"SDPS_DEFAULT_ALIGN" help:"${default_align_help}"`
	Align        map[string]string `group:"output" short:"a" default:"command=L" env:"SDPS_ALIGN" help:"${align_help}"`
	Agg          string            `group:"output" short:"g" help:"${agg_help}"`
	Header       bool              `group:"output" default:"true" negatable:"" help:"Control whether to show the header row."`
	Version      bool              `required:"" xor:"entry" help:"Show version and exit."`
}

const (
	alignLeft  = "L"
	alignRight = "R"
)

const (
	aggMin = "min"
)

const (
	fieldPID     = "pid"
	fieldPPID    = "ppid"
	fieldPCPU    = "pcpu"
	fieldVSZ     = "vsz"
	fieldRSS     = "rss"
	fieldStart   = "start"
	fieldUptime  = "uptime"
	fieldCommand = "command"
)

var fieldTitles = map[string]string{
	fieldPID:     "PID",
	fieldPPID:    "PPID",
	fieldPCPU:    "%CPU",
	fieldVSZ:     "VSZ",
	fieldRSS:     "RSS",
	fieldStart:   "START",
	fieldUptime:  "UPTIME",
	fieldCommand: "COMMAND",
}

func (c *CLI) Run(ctx context.Context) error {
	if c.Version {
		fmt.Println(version())
		return nil
	}

	columns, err := buildColumns(c.Column, c.Format, c.Align, c.DefaultAlign)
	if err != nil {
		return err
	}

	if c.Agg != "" {
		if len(columns) != 1 || columns[0].Field != fieldUptime {
			return errors.New("flag --agg is supported only for --field=UPTIME")
		}
		if c.Agg != aggMin {
			return errors.New("only supported value for flag --agg is \"min\"")
		}
	}

	pids, err := getPidsOfServices(c.Service)
	if err != nil {
		return err
	}
	records, err := readProcPidStatMulti(pids)
	if err != nil {
		return err
	}

	if c.Filter != "" {
		records = filterProcessRawRecordsWithCmdline(records, c.Filter)
	}

	rows, err := convertProcessRawRecordsToTableRows(columns, records, c.Agg)
	if err != nil {
		return err
	}

	var unalignedRows [][]string
	if c.Header {
		header := convertColumnsToHeader(columns)
		unalignedRows = make([][]string, 0, 1+len(rows))
		unalignedRows = append(append(unalignedRows, header), rows...)
	} else {
		unalignedRows = rows
	}

	var alignedRows [][]string
	if len(unalignedRows) <= 1 {
		alignedRows = unalignedRows
	} else {
		alignments := convertColumnsToAlign(columns)
		alignedRows, err = AlignColumns(unalignedRows, alignments)
		if err != nil {
			return err
		}
	}

	for _, row := range alignedRows {
		fmt.Println(strings.Join(row, "  "))
	}
	return nil
}

func filterProcessRawRecordsWithCmdline(records []ProcessRawRecord, filter string) []ProcessRawRecord {
	var filtered []ProcessRawRecord
	for _, record := range records {
		if strings.Contains(record.Command.String(), filter) {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

type Column struct {
	Field    string
	Align    Align
	Template *template.Template
}

func buildColumns(fields []string, funcCalls, alignments map[string]string, defaultAlign string) ([]Column, error) {
	columns := make([]Column, len(fields))
	for i, field := range fields {
		switch field {
		case fieldPID, fieldPPID, fieldPCPU, fieldVSZ, fieldRSS, fieldStart,
			fieldUptime, fieldCommand:

			columns[i].Field = field
		default:
			return nil, fmt.Errorf("invalid field: %s, must be one of %s", field,
				strings.Join([]string{fieldPID, fieldPPID, fieldVSZ, fieldRSS, fieldStart,
					fieldUptime, "or " + fieldCommand}, ", "))
		}

		a, ok := alignments[field]
		if !ok {
			a = defaultAlign
		}
		switch a {
		case alignLeft:
			columns[i].Align = AlignLeft
		case alignRight:
			columns[i].Align = AlignRight
		default:
			return nil, fmt.Errorf("invalid align: %s, must be %s or %s", a, alignLeft, alignRight)
		}

		var tmplText string
		if funcCall, ok := funcCalls[field]; ok {
			tmplText = fmt.Sprintf("{{.%s|%s}}", field, funcCall)
		} else {
			tmplText = fmt.Sprintf("{{.%s}}", field)
		}
		tmpl, err := template.New("").Funcs(templateFuncMap).Parse(tmplText)
		if err != nil {
			return nil,
				fmt.Errorf("cannot parse template: %s, err=%s", tmplText, err)
		}
		columns[i].Template = tmpl
	}
	return columns, nil
}

func convertColumnsToHeader(columns []Column) []string {
	row := make([]string, len(columns))
	for i, column := range columns {
		row[i] = fieldTitles[column.Field]
	}
	return row
}

func convertColumnsToAlign(columns []Column) []Align {
	config := make([]Align, len(columns))
	for i, column := range columns {
		config[i] = column.Align
	}
	return config
}

var templateFuncMap = template.FuncMap{
	"iBytes":       iBytes,
	"format":       formatTime,
	"humanRelTime": humanize.Time,
	"seconds":      seconds,
	"duration":     formatDuration,
}

func iBytes(b uint64) string {
	return humanize.IBytes(b)
}

func formatTime(layout string, t time.Time) string {
	return t.Format(layout)
}

func seconds(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Second), 10)
}

const (
	Year  = time.Duration(365.25 * float64(Day))
	Month = Year / 12
	Day   = 24 * time.Hour
)

func formatDuration(d time.Duration) string {
	if d < 0 {
		return "-" + formatDuration(-d)
	}

	if d < Day {
		return d.String()
	}

	if d < Month {
		day := d / Day
		rest := d % Day
		return fmt.Sprintf("%dd%s", day, rest)
	}

	if d < Year {
		month := d / Month
		rest := d % Month
		day := rest / Day
		rest = rest % Day
		return fmt.Sprintf("%dM%dd%s", month, day, rest)
	}

	year := d / Year
	rest := d % Year
	month := rest / Month
	rest = rest % Month
	day := rest / Day
	rest = rest % Day
	return fmt.Sprintf("%dy%dM%dd%s", year, month, day, rest)
}

func convertProcessRawRecordsToTableRows(columns []Column, records []ProcessRawRecord, agg string) ([][]string, error) {
	hasPID := false
	hasPPID := false
	hasPCPU := false
	hasVSZ := false
	hasRSS := false
	hasStart := false
	hasUptime := false
	hasCommand := false
	for _, column := range columns {
		switch column.Field {
		case fieldPID:
			hasPID = true
		case fieldPPID:
			hasPPID = true
		case fieldPCPU:
			hasPCPU = true
		case fieldVSZ:
			hasVSZ = true
		case fieldRSS:
			hasRSS = true
		case fieldStart:
			hasStart = true
		case fieldUptime:
			hasUptime = true
		case fieldCommand:
			hasCommand = true
		}
	}

	var err error
	var pageSize int
	if hasRSS {
		pageSize, err = getPageSize()
		if err != nil {
			return nil, err
		}
	}

	var bootTime time.Time
	if hasStart || hasUptime || hasPCPU {
		bootTime, err = getBootTime()
		if err != nil {
			return nil, err
		}
	}

	var sysUptime time.Duration
	if hasUptime || hasPCPU {
		sysUptime, err = getSystemUptime()
		if err != nil {
			return nil, err
		}
	}

	dataList := make([]map[string]any, len(records))
	for i, record := range records {
		data := make(map[string]any)

		if hasPID {
			data[fieldPID] = record.Pid
		}
		if hasPPID {
			data[fieldPPID] = record.PPid
		}
		if hasVSZ {
			vsizeInBytes, err := record.VSize.InBytes()
			if err != nil {
				return nil, err
			}
			data[fieldVSZ] = vsizeInBytes
		}
		if hasRSS {
			rssPageCount, err := record.RSS.InPages()
			if err != nil {
				return nil, err
			}
			rssInBytes := rssPageCount * uint64(pageSize)
			data[fieldRSS] = rssInBytes
		}
		if hasStart || hasUptime || hasPCPU {
			startDur, err := record.StartTime.AsDuration()
			if err != nil {
				return nil, err
			}

			if hasStart {
				data[fieldStart] = bootTime.Add(startDur)
			}
			if hasUptime || hasPCPU {
				procUptime := sysUptime - startDur
				if hasUptime {
					data[fieldUptime] = procUptime.Truncate(time.Second)
				}
				if hasPCPU {
					pcpu, err := record.percentCPU(procUptime)
					if err != nil {
						return nil, err
					}
					data[fieldPCPU] = fmt.Sprintf("%.1f", pcpu)
				}
			}
		}
		if hasCommand {
			data[fieldCommand] = record.Command
		}

		dataList[i] = data
	}

	if agg == aggMin {
		if len(dataList) > 1 {
			data := dataList[0]
			uptime := data[fieldUptime].(time.Duration)
			for i := range dataList {
				if dataList[i][fieldUptime].(time.Duration) < uptime {
					data = dataList[i]
					uptime = dataList[i][fieldUptime].(time.Duration)
				}
			}
			dataList = []map[string]any{data}
		} else if len(dataList) == 0 {
			dataList = []map[string]any{
				{
					fieldUptime: time.Duration(0),
				},
			}
		}
	}

	rows := make([][]string, len(dataList))
	for i, data := range dataList {
		rows[i] = make([]string, len(columns))
		for j, col := range columns {
			var err error
			rows[i][j], err = renderTemplate(col.Template, data)
			if err != nil {
				return nil, err
			}
		}
	}
	return rows, nil
}

func renderTemplate(tmpl *template.Template, data any) (string, error) {
	var sb strings.Builder
	if err := tmpl.Execute(&sb, data); err != nil {
		return "", err
	}
	return sb.String(), nil
}

var ErrNotStarted = errors.New("not started")

func getPidsOfServices(services []string) ([]int, error) {
	var pids []int
	for _, service := range services {
		servicePids, err := getPidsOfService(service)
		if err != nil && !errors.Is(err, ErrNotStarted) {
			return nil, err
		}
		pids = append(pids, servicePids...)
	}
	return pids, nil
}

func getPidsOfService(service string) ([]int, error) {
	if err := validateServiceName(service); err != nil {
		return nil, err
	}
	filename := fmt.Sprintf("/sys/fs/cgroup/system.slice/%s.service/cgroup.procs", service)
	content, err := os.ReadFile(filename)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			exists, err2 := checkServiceExists(service)
			if err2 != nil {
				return nil, err2
			}
			if !exists {
				return nil, fmt.Errorf("no such service: %s", service)
			}
			return nil, ErrNotStarted
		}
		return nil, fmt.Errorf("cannot get pids from %s: %w", filename, err)
	}

	var pids []int
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		pid, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("cannot convert pid to int, line=%s, err=%s", line, err)
		}
		pids = append(pids, pid)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return pids, nil
}

func validateServiceName(service string) error {
	if strings.ContainsRune(service, '/') || service == ".." {
		return errors.New("invalid service name")
	}
	return nil
}

func checkServiceExists(service string) (bool, error) {
	cmd := exec.Command("systemctl",
		"show", "--value", "--property=LoadError", service)
	outputBytes, err := cmd.Output()
	if err != nil {
		return false, err
	}
	const noSuchUnit = "org.freedesktop.systemd1.NoSuchUnit "
	return !strings.HasPrefix(string(outputBytes), noSuchUnit), nil
}

type ProcessRawRecord struct {
	Pid       int
	PPid      PPid
	UTime     ClockTicks
	STime     ClockTicks
	StartTime ClockTicks
	VSize     VSize
	RSS       RSS
	Command   Cmdline
}

func (r *ProcessRawRecord) percentCPU(procUptime time.Duration) (float64, error) {
	uTimeTicks, err := r.UTime.AsTicks()
	if err != nil {
		return 0, fmt.Errorf("failed to convert utime to integer: %s", err)
	}
	sTimeTicks, err := r.STime.AsTicks()
	if err != nil {
		return 0, fmt.Errorf("failed to convert stime to integer: %s", err)
	}
	uptimeTicks := procUptime / (time.Second / _SYSTEM_CLK_TCK)
	return float64(uTimeTicks+sTimeTicks) / float64(uptimeTicks) * 100, nil
}

func readProcPidStatMulti(pids []int) ([]ProcessRawRecord, error) {
	var wg sync.WaitGroup
	wg.Add(len(pids))
	records := make([]ProcessRawRecord, len(pids))
	errors := make([]error, len(pids))
	for i, pid := range pids {
		func() {
			defer wg.Done()
			records[i], errors[i] = readProcPidStatAndCommand(pid)
		}()
	}
	wg.Wait()
	return records, joinErrors(errors...)
}

func joinErrors(errs ...error) error {
	n := 0
	var lastErr error
	for _, err := range errs {
		if err != nil {
			lastErr = err
			n++
		}
	}
	switch n {
	case 0:
		return nil
	case 1:
		return lastErr
	default:
		return errors.Join(errs...)
	}
}

func readProcPidStatAndCommand(pid int) (ProcessRawRecord, error) {
	record, err := readProcPidStat(pid)
	var err2 error
	record.Command, err2 = readProdPidCmdline(pid)
	return record, joinErrors(err, err2)
}

type PPid struct {
	raw []byte
}

func (p PPid) String() string {
	return string(p.raw)
}

type ClockTicks struct {
	raw []byte
}

func (t ClockTicks) AsTicks() (uint64, error) {
	return strconv.ParseUint(string(t.raw), 10, 64)
}

func (t ClockTicks) AsDuration() (time.Duration, error) {
	ticks, err := strconv.ParseUint(string(t.raw), 10, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(ticks) * (time.Second / _SYSTEM_CLK_TCK), nil
}

func (t ClockTicks) String() string {
	return string(t.raw)
}

const (
	// CLK_TCK is the number of clock ticks per second.
	//
	// CLK_TCK is a constant on Linux for all architectures except alpha and ia64.
	// See e.g.
	// https://git.musl-libc.org/cgit/musl/tree/src/conf/sysconf.c#n30
	// https://github.com/containerd/cgroups/pull/12
	// https://lore.kernel.org/lkml/agtlq6$iht$1@penguin.transmeta.com/
	//
	// copied from https://github.com/tklauser/go-sysconf/blob/v0.3.15/sysconf_linux.go#L18-L25
	_SYSTEM_CLK_TCK = 100
)

type VSize struct {
	raw []byte
}

func (s VSize) String() string {
	return string(s.raw)
}

func (s VSize) InBytes() (uint64, error) {
	return strconv.ParseUint(s.String(), 10, 64)
}

type RSS struct {
	raw []byte
}

func (r RSS) String() string {
	return string(r.raw)
}

func (r RSS) InPages() (uint64, error) {
	return strconv.ParseUint(r.String(), 10, 64)
}

func readProcPidStat(pid int) (ProcessRawRecord, error) {
	//  (1) pid  %d
	//         The process ID.
	//
	//  ...(snip)...
	//
	//  (4) ppid  %d
	//         The PID of the parent of this process.
	//
	//  ...(snip)...
	//
	//  (14) utime  %lu
	//         Amount of time that this process has been scheduled
	//         in user mode, measured in clock ticks (divide by
	//         sysconf(_SC_CLK_TCK)).  This includes guest time,
	//         guest_time (time spent running a virtual CPU, see
	//         below), so that applications that are not aware of
	//         the guest time field do not lose that time from
	//         their calculations.
	//
	//  (15) stime  %lu
	//         Amount of time that this process has been scheduled
	//         in kernel mode, measured in clock ticks (divide by
	//         sysconf(_SC_CLK_TCK)).
	//
	//  ...(snip)...
	//
	//  (22) starttime  %llu
	//         The time the process started after system boot.
	//         Before Linux 2.6, this value was expressed in
	//         jiffies.  Since Linux 2.6, the value is expressed in
	//         clock ticks (divide by sysconf(_SC_CLK_TCK)).
	//
	//  (23) vsize  %lu
	//         Virtual memory size in bytes.
	//
	//  (24) rss  %ld
	//         Resident Set Size: number of pages the process has
	//         in real memory.  This is just the pages which count
	//         toward text, data, or stack space.  This does not
	//         include pages which have not been demand-loaded in,
	//         or which are swapped out.  This value is inaccurate;
	//         see /proc/pid/statm below.
	//
	// https://man7.org/linux/man-pages/man5/proc_pid_stat.5.html
	filename := fmt.Sprintf("/proc/%d/stat", pid)
	content, err := os.ReadFile(filename)
	if err != nil {
		return ProcessRawRecord{}, fmt.Errorf("cannot read %s: %s", filename, err)
	}
	const ppidIdx = 4
	const utimeIdx = 14
	const stimeIdx = 15
	const startTimeIdx = 22
	const vsizeIdx = 23
	const rssIdx = 24
	i := 1
	record := ProcessRawRecord{Pid: pid}
	for word := range bytes.SplitSeq(content, []byte{' '}) {
		switch i {
		case ppidIdx:
			record.PPid = PPid{raw: word}
		case utimeIdx:
			record.UTime = ClockTicks{raw: word}
		case stimeIdx:
			record.STime = ClockTicks{raw: word}
		case startTimeIdx:
			record.StartTime = ClockTicks{raw: word}
		case vsizeIdx:
			record.VSize = VSize{raw: word}
		case rssIdx:
			record.RSS = RSS{raw: word}
			return record, nil
		}
		i++
	}
	return ProcessRawRecord{}, errors.New("cannot find starttime")
}

type Cmdline struct {
	raw []byte
}

func (c Cmdline) String() string {
	cmd := bytes.TrimRight(c.raw, "\x00")
	return string(bytes.ReplaceAll(cmd, []byte{'\x00'}, []byte{' '}))
}

func readProdPidCmdline(pid int) (Cmdline, error) {
	filename := fmt.Sprintf("/proc/%d/cmdline", pid)
	content, err := os.ReadFile(filename)
	if err != nil {
		return Cmdline{}, fmt.Errorf("cannot read %s: %s", filename, err)
	}
	return Cmdline{raw: content}, nil
}

func getPageSize() (int, error) {
	cmd := exec.Command("getconf", "PAGESIZE")
	outputBytes, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(bytes.TrimSuffix(outputBytes, []byte{'\n'})))
}

func getBootTime() (time.Time, error) {
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

func getSystemUptime() (time.Duration, error) {
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

func main() {
	ctx := kong.Parse(&cli,
		kong.Name(cliName),
		kong.Description(description),
		kong.UsageOnError(),
		cliVars)
	// kong.BindTo is needed to bind a context.Context value.
	// See https://github.com/alecthomas/kong/issues/48
	ctx.BindTo(context.Background(), (*context.Context)(nil))
	err := ctx.Run()
	ctx.FatalIfErrorf(err)
}

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return "(devel)"
}
