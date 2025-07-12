# sdps

## Usage

```
Usage: sdps --service=SERVICE,... [flags]

"sdps" is an alternative "ps" command specifically designed for processes within systemd services.

Its name is an abbreviation of "systemd ps".

It's not a full replacement for "ps", but rather focuses on a core subset of functionality to serve
two use cases:

* Displaying Human-Readable Process Information.

    View process details in a format easy for humans to read.

    # sdps -s nginx,trafficserver

    # sdps -s nginx,trafficserver -c pid,vsz,rss,start,command -f start=humanRelTime

* Outputting Single Values for Monitoring/Scripting:

    Extract a single process value, ideal for integration with monitoring software or for use in scripts.

    # sdps -s nginx -l 'nginx: worker' -c uptime -f uptime=seconds -g min --no-header

    # sdps -s nginx -l 'nginx: master' -c pid --no-header

Flags:
  -h, --help       Show context-sensitive help.
      --version    Show version and exit.

process
  -s, --service=SERVICE,...    Specify systemd service name(s).
  -l, --filter=STRING          Filter processes by their command line.

output
  -c, --column=pid,ppid,vsz,rss,start,uptime,command,...
                             Columns to display in the output. Available columns: "pid", "ppid",
                             "vsz", "rss", "start", "uptime", and "command" ($MYPS_COLUMN).
  -f, --format=vsz=iBytes;rss=iBytes;start=format "2006-01-02 15:04";uptime=duration
                             Specify formatting functions for column values. Uses Go's text/template
                             syntax after "|". Available functions: "iBytes" for "vsz" and "rss",
                             "format" or "humanRelTime" for "start", "duration" or "seconds" for
                             "uptime". Note for units in the output of "duration": "y" is 365.25
                             days, "M" is 30.4375 days, "d" is 24 hours. For "format" layout
                             details, see https://pkg.go.dev/time@latest#Layout ($MYPS_FORMAT).
  -d, --default-align="R"    Set the default alignment for all columns ($MYPS_DEFAULT_ALIGN).
  -a, --align=command=L      Override default column alignments ($MYPS_ALIGN).
  -g, --agg=STRING           Aggregate a single field value from processes. Currently, only
                             "--field=uptime --agg=min" is supported.
      --[no-]header          Control whether to show the header row.
```

## For Developer: How to build sdps as "not a dynamic executable"

Install [Go](https://go.dev/) and run the following command:

```
go build -trimpath -tags osusergo
```
