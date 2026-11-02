module github.com/alexschlessinger/pollytool

go 1.27.0

require (
	github.com/alecthomas/chroma/v2 v2.27.0
	github.com/denormal/go-gitignore v0.0.0-20180930084346-ae8ad1d07817
	github.com/dop251/goja v0.0.0-20261102140139-622d10077b6c
	github.com/gdamore/tcell/v3 v3.6.0
	github.com/google/jsonschema-go v0.4.3
	github.com/lmittmann/tint v1.2.1
	github.com/mattn/go-runewidth v0.0.31
	github.com/mattn/go-sixel v0.0.14
	github.com/metaspartan/gotui/v5 v5.0.3
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/rivo/uniseg v0.4.7
	github.com/urfave/cli/v3 v3.14.0
	github.com/yuin/goldmark v1.8.7
	go.yaml.in/yaml/v3 v3.0.6
	golang.org/x/image v0.47.0
	golang.org/x/sync v0.24.0
	golang.org/x/sys v0.49.0
	golang.org/x/term v0.47.0
	modernc.org/sqlite v1.61.1
	mvdan.cc/sh/v3 v3.14.1
)

require (
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/danwakefield/fnmatch v0.0.0-20160403171240-cbb64ac3d964 // indirect
	github.com/dlclark/regexp2/v2 v2.12.0 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/gdamore/encoding v1.0.1 // indirect
	github.com/go-sourcemap/sourcemap v2.1.4+incompatible // indirect
	github.com/google/pprof v0.0.0-20261026192309-dbd0ed30aa88 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/soniakeys/quant v1.0.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	modernc.org/libc v1.80.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

// Preserve dirty-block rendering and deterministic headless terminal captures.
replace github.com/gdamore/tcell/v3 => github.com/alexschlessinger/tcell/v3 v3.5.1-0.20260921182018-0765ea4d951d

// Use native terminal mocks for gotui's offscreen entry points.
replace github.com/metaspartan/gotui/v5 => github.com/alexschlessinger/gotui/v5 v5.0.4-0.20260921181657-8935b7206b5a
