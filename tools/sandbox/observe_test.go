package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const testDenialTag = "polly-00112233445566778899aabbccddeeff"

// Report messages as the macOS kernel writes them: the report line, then the
// tag the profile's deny rule carries.
func TestParseSeatbeltReport(t *testing.T) {
	skipIfWindows(t)
	tagged := func(line string) string { return line + "\n" + testDenialTag }
	for _, tt := range []struct {
		name    string
		message string
		want    Denial
		ok      bool
	}{
		{"read", tagged("Sandbox: cat(41462) deny(1) file-read-data /Users/a/.zshrc"),
			Denial{Access: AccessRead, Path: "/Users/a/.zshrc", Operation: "file-read-data", Process: "cat", Count: 1}, true},
		{"metadata", tagged("Sandbox: ls(41491) deny(1) file-read-metadata /Users/a/notes"),
			Denial{Access: AccessRead, Path: "/Users/a/notes", Operation: "file-read-metadata", Process: "ls", Count: 1}, true},
		{"write", tagged("Sandbox: mkdir(7) deny(1) file-write-create /Users/a/.cache"),
			Denial{Access: AccessWrite, Path: "/Users/a/.cache", Operation: "file-write-create", Process: "mkdir", Count: 1}, true},
		{"one duplicate", tagged("1 duplicate report for Sandbox: ls(41491) deny(1) file-read-metadata /Users/a"),
			Denial{Access: AccessRead, Path: "/Users/a", Operation: "file-read-metadata", Process: "ls", Count: 1}, true},
		{"duplicates", tagged("12 duplicate reports for Sandbox: go(9) deny(1) file-write-data /Users/a/Library/Caches/go-build/x"),
			Denial{Access: AccessWrite, Path: "/Users/a/Library/Caches/go-build/x", Operation: "file-write-data", Process: "go", Count: 12}, true},
		{"spaces and parentheses", tagged("Sandbox: Helper (Renderer)(123) deny(1) file-read-data /Users/a/My Files/x (1).txt"),
			Denial{Access: AccessRead, Path: "/Users/a/My Files/x (1).txt", Operation: "file-read-data", Process: "Helper (Renderer)", Count: 1}, true},
		{"remote", tagged("Sandbox: nc(46725) deny(1) network-outbound remote:*:443"),
			Denial{Access: AccessNetwork, Path: "remote:*:443", Operation: "network-outbound", Process: "nc", Count: 1}, true},
		{"socket", tagged("Sandbox: nc(46726) deny(1) network-outbound /private/var/run/mDNSResponder"),
			Denial{Access: AccessNetwork, Path: "/private/var/run/mDNSResponder", Operation: "network-outbound", Process: "nc", Count: 1}, true},
		{"another sandbox's tag", "Sandbox: cat(1) deny(1) file-read-data /x\npolly-ffffffffffffffffffffffffffffffff", Denial{}, false},
		{"untagged", "Sandbox: cat(1) deny(1) file-read-data /x", Denial{}, false},
		{"tag inside the target", "Sandbox: cat(1) deny(1) file-read-data /x " + testDenialTag, Denial{}, false},
		{"another sender", tagged("System Policy: touch(1) deny(1) file-write-create /x"), Denial{}, false},
		{"not a file or network access", tagged("Sandbox: x(1) deny(1) mach-lookup com.apple.foo"), Denial{}, false},
		{"relative path", tagged("Sandbox: x(1) deny(1) file-read-data x/y"), Denial{}, false},
		{"control character", tagged("Sandbox: x(1) deny(1) file-read-data /a\tb"), Denial{}, false},
		{"a target over the bound", tagged("Sandbox: x(1) deny(1) file-read-data /" + strings.Repeat("a", maxDenialTarget)), Denial{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseSeatbeltReport(tt.message, testDenialTag)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("parseSeatbeltReport = %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestSeatbeltNoise(t *testing.T) {
	skipIfWindows(t)
	home := "/Users/a"
	for _, tt := range []struct {
		d    Denial
		want bool
	}{
		{Denial{Access: AccessWrite, Path: "/dev/dtracehelper"}, true},
		{Denial{Access: AccessWrite, Path: "/dev/tty"}, true},
		{Denial{Access: AccessRead, Path: "/Users/a/.CFUserTextEncoding"}, true},
		{Denial{Access: AccessWrite, Path: "/Users/a/.CFUserTextEncoding"}, false},
		{Denial{Access: AccessRead, Path: "/Users/a/Library/Preferences/com.apple.security.plist"}, true},
		{Denial{Access: AccessRead, Path: "/Users/a/Library/Preferences/.GlobalPreferences.plist"}, true},
		{Denial{Access: AccessRead, Path: "/Users/a/Library/Preferences/org.example.tool.plist"}, false},
		{Denial{Access: AccessWrite, Path: "/Users/a/Library/Preferences/com.apple.security.plist"}, false},
		// The home directory itself is never grantable, but a denial of it
		// explains a failure, such as mkdir -p stopping there.
		{Denial{Access: AccessRead, Path: "/Users/a"}, false},
		{Denial{Access: AccessRead, Path: "/Users/a/.npmrc"}, false},
	} {
		if got := seatbeltNoise(tt.d, home); got != tt.want {
			t.Errorf("seatbeltNoise(%+v) = %v, want %v", tt.d, got, tt.want)
		}
	}
}

func TestDenialSetMergesAndBounds(t *testing.T) {
	var set denialSet
	set.add(Denial{Access: AccessRead, Path: "/a", Operation: "file-read-metadata", Count: 1})
	set.add(Denial{Access: AccessWrite, Path: "/a", Operation: "file-write-create", Count: 1})
	set.add(Denial{Access: AccessRead, Path: "/a", Operation: "file-read-data", Count: 2})
	want := []Denial{
		{Access: AccessRead, Path: "/a", Operation: "file-read-metadata", Count: 3},
		{Access: AccessWrite, Path: "/a", Operation: "file-write-create", Count: 1},
	}
	if !reflect.DeepEqual(set.denials, want) || set.truncated {
		t.Fatalf("merged = %+v (truncated %v), want %+v", set.denials, set.truncated, want)
	}
	for i := range maxDenials {
		set.add(Denial{Access: AccessRead, Path: "/b/" + strconv.Itoa(i), Count: 1})
	}
	if len(set.denials) != maxDenials || !set.truncated {
		t.Fatalf("kept %d denials, truncated %v; want %d and truncated", len(set.denials), set.truncated, maxDenials)
	}
	set.add(Denial{Access: AccessRead, Path: "/a", Count: 1})
	if set.denials[0].Count != 4 {
		t.Fatalf("a known denial past the bound still counts: %+v", set.denials[0])
	}
}

// homeReport renders what linuxTrialScript writes: the listing before the
// command, then the directories and the other entries after it.
func homeReport(before, dirs, files []string) []byte {
	var records []string
	records = append(records, before...)
	records = append(records, homeReportDirs)
	records = append(records, dirs...)
	records = append(records, homeReportFiles)
	records = append(records, files...)
	return []byte(strings.Join(records, "\x00") + "\x00")
}

func TestParseHomeReport(t *testing.T) {
	skipIfWindows(t)
	home := "/home/u"
	at := func(rel string) string { return filepath.Join(home, rel) }
	// The mountpoints bwrap made for a grant are there before the command.
	before := []string{home, at("src"), at("src/project")}
	dirs := []string{home, at("src"), at("src/project"),
		at(".cache"), at(".cache/tool"), at(".cache/tool/objects"),
		at(".config"), at(".config/app"),
		at("src/scratch"), at("src/scratch/a"), at("src/scratch/b")}
	files := []string{at(".cache/tool/objects/1"), at(".cache/tool/objects/2"),
		at(".config/app/settings.json"), at(".lesshst"), "/etc/outside", at("bad\nname")}

	denials, complete := parseHomeReport(homeReport(before, dirs, files), home)
	want := []Denial{
		{Access: AccessWrite, Path: at(".cache/tool/objects"), Operation: "write", Count: 5, Discarded: true, Directory: true},
		{Access: AccessWrite, Path: at(".config/app"), Operation: "write", Count: 3, Discarded: true, Directory: true},
		{Access: AccessWrite, Path: at(".lesshst"), Operation: "write", Count: 1, Discarded: true},
		// A new directory holding more than one new directory is the root.
		{Access: AccessWrite, Path: at("src/scratch"), Operation: "write", Count: 3, Discarded: true, Directory: true},
	}
	if !complete || !reflect.DeepEqual(denials, want) {
		t.Fatalf("parseHomeReport = %+v, %v\nwant %+v", denials, complete, want)
	}

	// A command that never finished leaves only the first listing.
	if denials, complete := parseHomeReport([]byte(strings.Join(before, "\x00")+"\x00"), home); complete || len(denials) != 0 {
		t.Fatalf("an unfinished report = %+v, %v; want nothing and incomplete", denials, complete)
	}
	// Nothing new is nothing.
	if denials, complete := parseHomeReport(homeReport(before, before, nil), home); !complete || len(denials) != 0 {
		t.Fatalf("an unchanged home = %+v, %v", denials, complete)
	}
}

func TestClassifyDenials(t *testing.T) {
	skipIfWindows(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	work := filepath.Join(home, "src", "project")
	shared := filepath.Join(home, "src", "shared")
	for _, dir := range []string{work, shared, filepath.Join(home, ".ssh")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := PrepareConfig(Config{WritablePaths: []string{work}, ReadPaths: []string{shared}})
	if err != nil {
		t.Fatal(err)
	}
	denials := []Denial{
		{Access: AccessRead, Path: filepath.Join(home, "notes", "todo.txt")},
		{Access: AccessRead, Path: filepath.Join(home, ".ssh", "id_ed25519")},
		{Access: AccessWrite, Path: filepath.Join(home, ".ssh", "config")},
		{Access: AccessWrite, Path: filepath.Join(home, ".cache")},
		{Access: AccessWrite, Path: filepath.Join(shared, "out")},
		{Access: AccessWrite, Path: filepath.Join(work, "build")},
		{Access: AccessRead, Path: filepath.Join(shared, "lib.go")},
		{Access: AccessNetwork, Path: "remote:*:443"},
	}
	if err := ClassifyDenials(cfg, denials); err != nil {
		t.Fatal(err)
	}
	want := []DenialCause{CausePrivate, CauseMasked, CauseMasked, CauseNotWritable, CauseNotWritable, CauseUnexplained, CauseUnexplained, CauseNetwork}
	for i, d := range denials {
		if d.Cause != want[i] {
			t.Errorf("%s %s: cause %q, want %q", d.Access, d.Path, d.Cause, want[i])
		}
	}
}

func TestConfigMergeCarriesTheDenialTag(t *testing.T) {
	tagged := Config{denialTag: testDenialTag}
	if got := tagged.Merge(Config{ReadPaths: []string{"/x"}}); got.denialTag != testDenialTag {
		t.Fatalf("merging over a tagged base lost the tag: %q", got.denialTag)
	}
	if got := (Config{}).Merge(tagged); got.denialTag != testDenialTag {
		t.Fatalf("a tagged overlay did not tag the merge: %q", got.denialTag)
	}
	prepared, err := PrepareConfig(tagged)
	if err != nil || prepared.denialTag != testDenialTag {
		t.Fatalf("PrepareConfig = %q, %v; want the tag kept", prepared.denialTag, err)
	}
}
