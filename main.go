package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"
	"github.com/dustin/go-humanize"
	"github.com/hnakamur/myps/internal/align"
)

const (
	// CLK_TCK is a constant on Linux for all architectures except alpha and ia64.
	// See e.g.
	// https://git.musl-libc.org/cgit/musl/tree/src/conf/sysconf.c#n30
	// https://github.com/containerd/cgroups/pull/12
	// https://lore.kernel.org/lkml/agtlq6$iht$1@penguin.transmeta.com/
	//
	// copied from https://github.com/tklauser/go-sysconf/blob/v0.3.15/sysconf_linux.go#L18-L25
	_SYSTEM_CLK_TCK = 100
)

var cli CLI

type CLI struct {
	Service   []string `short:"s" help:"systemd service name"`
	Headers   bool     `default:"true" negatable:"" help:"show headers"`
	FormatSep string   `default:";" help:"separator character(s) for --format" env:"MYPS_FORMAT_SEP"`
	Format    string   `short:"o" default:"PID,R,{{.PID}};PPID,R,{{.PPID}};VSZ,R,{{.VSZ|iBytes}};RSS,R,{{.RSS|iBytes}};START,L,{{.START|format \"2006-01-02 15:04\"}};UPTIME,R,{{.UPTIME|duration}};COMMAND,L,{{.COMMAND}}" env:"MYPS_FORMAT" help:"title, align, and template for columns"`
	Version   bool     `help:"show version and exit"`
}

const (
	colTitlePID     = "PID"
	colTitlePPID    = "PPID"
	colTitleVSZ     = "VSZ"
	colTitleRSS     = "RSS"
	colTitleStart   = "START"
	colTitleUptime  = "UPTIME"
	colTitleCommand = "COMMAND"
)

const (
	alignLeft       = "left"
	alignLeftShort  = "L"
	alignRight      = "right"
	alignRightShort = "R"
)

func (c *CLI) Run(ctx context.Context) error {
	if c.Version {
		fmt.Println(version())
		return nil
	}

	log.Printf("headers=%v", c.Headers)

	columns, err := parseColumns(strings.Split(c.Format, c.FormatSep))
	if err != nil {
		return err
	}

	pids, err := getPidsOfServices(c.Service)
	if err != nil {
		return err
	}
	records, err := readProcPidStatMulti(pids)
	if err != nil {
		return err
	}

	rows, err := convertProcessRawRecordsToTableRows(columns, records)
	if err != nil {
		return err
	}

	header := convertColumnsToHeader(columns)
	headerAndRows := make([][]string, 0, 1+len(rows))
	headerAndRows = append(append(headerAndRows, header), rows...)

	alignments := convertColumnsToAlign(columns)
	alignedRows, err := align.AlignColumns(headerAndRows, alignments)
	if err != nil {
		return err
	}

	for _, row := range alignedRows {
		fmt.Println(strings.Join(row, "  "))
	}
	return nil
}

type Column struct {
	Title    string
	Align    align.Align
	Template *template.Template
}

func parseColumns(input []string) ([]Column, error) {
	columns := make([]Column, len(input))
	for i, columnText := range input {
		var err error
		columns[i], err = parseColumn(columnText)
		if err != nil {
			return nil, err
		}
	}
	return columns, nil
}

func parseColumn(input string) (Column, error) {
	terms := strings.SplitN(input, ",", 3)
	if len(terms) != 3 {
		return Column{}, fmt.Errorf("column must be in form TITLE,ALIGN,TEMPLATE: %s", input)
	}

	var column Column

	switch terms[0] {
	case colTitlePID, colTitlePPID, colTitleVSZ, colTitleRSS, colTitleStart,
		colTitleUptime, colTitleCommand:

		column.Title = terms[0]
	default:
		return Column{}, fmt.Errorf("invalid column title: %s", terms[0])
	}

	switch terms[1] {
	case alignLeft, alignLeftShort:
		column.Align = align.Left
	case alignRight, alignRightShort:
		column.Align = align.Right
	default:
		return Column{},
			fmt.Errorf("align must be one of %s, %s, %s, or %s: %s in %s",
				alignLeft, alignLeftShort, alignRight, alignRightShort,
				terms[1], input)
	}

	tmpl, err := template.New("").Funcs(templateFuncMap).Parse(terms[2])
	if err != nil {
		return Column{},
			fmt.Errorf("cannot parse template: %s, err=%s", terms[2], err)
	}
	column.Template = tmpl
	return column, nil
}

func convertColumnsToHeader(columns []Column) []string {
	row := make([]string, len(columns))
	for i, column := range columns {
		row[i] = column.Title
	}
	return row
}

func convertColumnsToAlign(columns []Column) []align.Align {
	config := make([]align.Align, len(columns))
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
		return fmt.Sprintf("%dm%dd%s", month, day, rest)
	}

	year := d / Year
	rest := d % Year
	month := rest / Month
	rest = rest % Month
	day := rest / Day
	rest = rest % Day
	return fmt.Sprintf("%dy%dm%dd%s", year, month, day, rest)
}

func convertProcessRawRecordsToTableRows(columns []Column, records []ProcessRawRecord) ([][]string, error) {
	pageSize, err := getPageSize()
	if err != nil {
		return nil, err
	}

	bootTime, err := getBootTime()
	if err != nil {
		return nil, err
	}

	now := time.Now()

	rows := make([][]string, len(records))
	for i, record := range records {
		vsizeInBytes, err := record.VSize.InBytes()
		if err != nil {
			return nil, err
		}

		rssPageCount, err := record.RSS.InPages()
		if err != nil {
			return nil, err
		}
		rssInBytes := rssPageCount * uint64(pageSize)

		start, err := record.StartTime.AsTime(bootTime)
		if err != nil {
			return nil, err
		}

		uptime := now.Sub(start).Truncate(time.Second)

		data := map[string]any{
			"PID":     record.Pid,
			"PPID":    record.PPid,
			"VSZ":     vsizeInBytes,
			"RSS":     rssInBytes,
			"START":   start,
			"UPTIME":  uptime,
			"COMMAND": record.Command,
		}

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

var ErrNoSuchService = errors.New("no such service")
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
				return nil, ErrNoSuchService
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
	StartTime StartTime
	VSize     VSize
	RSS       RSS
	Command   Cmdline
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

type StartTime struct {
	raw []byte
}

func (t StartTime) String() string {
	return string(t.raw)
}

func (t StartTime) AsTime(bootTime time.Time) (time.Time, error) {
	startTimeInClocks, err := strconv.ParseInt(t.String(), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return bootTime.Add(time.Duration(startTimeInClocks/_SYSTEM_CLK_TCK) * time.Second), nil
}

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
	const startTimeIdx = 22
	const vsizeIdx = 23
	const rssIdx = 24
	i := 1
	record := ProcessRawRecord{Pid: pid}
	for word := range bytes.SplitSeq(content, []byte{' '}) {
		switch i {
		case ppidIdx:
			record.PPid = PPid{raw: word}
		case startTimeIdx:
			record.StartTime = StartTime{raw: word}
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

func main() {
	ctx := kong.Parse(&cli)
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
