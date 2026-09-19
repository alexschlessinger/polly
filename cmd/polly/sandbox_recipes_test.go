package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

type buildRecipe struct {
	Name     string             `json:"name"`
	Version  int                `json:"version"`
	Versions string             `json:"versions"`
	Prepare  sandboxPreparation `json:"prepare"`
}

func readBuildRecipes(t *testing.T) []buildRecipe {
	t.Helper()
	paths, err := filepath.Glob("../../skills/builtin/sandbox-setup/recipes/*.json")
	if err != nil || len(paths) != 13 {
		t.Fatalf("recipe inventory: %d %v", len(paths), err)
	}
	var recipes []buildRecipe
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var r buildRecipe
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		recipes = append(recipes, r)
	}
	return recipes
}

func recipeArgs(t *testing.T, r buildRecipe) tools.Args {
	t.Helper()
	data, err := json.Marshal(r.Prepare)
	if err != nil {
		t.Fatal(err)
	}
	var args tools.Args
	if err := json.Unmarshal(data, &args); err != nil {
		t.Fatal(err)
	}
	return args
}

func TestSandboxRecipeDeclarations(t *testing.T) {
	for _, r := range readBuildRecipes(t) {
		t.Run(r.Name, func(t *testing.T) {
			if r.Version != 1 || r.Versions == "" {
				t.Fatal("missing recipe version contract")
			}
			_, state := sandboxTryState(t)
			defer state.sandboxProfile.Close()
			startSandboxInit(state)
			for range 2 {
				if out, code := callSandboxTool(t, context.Background(), state, sandboxPrepareTool, recipeArgs(t, r)); code != "" {
					t.Fatalf("preparation: %s %s", code, out)
				}
			}
		})
	}
}

type nativeRecipeFixture struct {
	tool, version, setup, bootstrap, build, test string
	files                                        map[string]string
}

func nativeRecipeFixtures() map[string]nativeRecipeFixture {
	js := map[string]string{
		"package.json":     `{"name":"sandbox-fixture","private":true,"version":"1.0.0","scripts":{"build":"node --check index.js","test":"node --test test.js"},"dependencies":{"fixture-dep":"file:./dep","is-number":"7.0.0"}}`,
		"dep/package.json": `{"name":"fixture-dep","version":"1.0.0","main":"index.js"}`,
		"dep/index.js":     "module.exports = (a,b) => a+b;\n",
		"index.js":         "module.exports = require('fixture-dep');\n",
		"test.js":          "const test=require('node:test'); const assert=require('node:assert/strict'); const add=require('./index'); const isNumber=require('is-number'); test('dependency',()=>assert.equal(isNumber(4),true)); test('sum',()=>assert.equal(add(2,2),4)); test('zero',()=>assert.equal(add(0,0),0));\n",
	}
	py := map[string]string{"test_sum.py": "import unittest, idna\nclass Sum(unittest.TestCase):\n def test_sum(self): self.assertEqual(2+2,4)\n def test_dependency(self): self.assertEqual(idna.encode('example.test'), b'example.test')\n", "requirements.txt": "idna==3.10\n", "pyproject.toml": "[project]\nname='sandbox-fixture'\nversion='1.0.0'\nrequires-python='>=3.10'\ndependencies=['idna==3.10']\n"}
	java := map[string]string{"src/main/java/demo/Sum.java": "package demo; public class Sum { public static int add(int a,int b) { return a+b; } }", "src/test/java/demo/SumTest.java": "package demo; import org.junit.Test; import static org.junit.Assert.*; public class SumTest { @Test public void sum() { assertEquals(4,Sum.add(2,2)); } @Test public void zero() { assertEquals(0,Sum.add(0,0)); } }"}
	maven := map[string]string{"pom.xml": `<project><modelVersion>4.0.0</modelVersion><groupId>demo</groupId><artifactId>fixture</artifactId><version>1</version><properties><maven.compiler.source>17</maven.compiler.source><maven.compiler.target>17</maven.compiler.target></properties><dependencies><dependency><groupId>junit</groupId><artifactId>junit</artifactId><version>4.13.2</version><scope>test</scope></dependency></dependencies></project>`}
	gradle := map[string]string{"settings.gradle": "rootProject.name = 'fixture'\n", "build.gradle": "plugins { id 'java' }\nrepositories { mavenCentral() }\ndependencies { testImplementation 'junit:junit:4.13.2' }\n"}
	for path, data := range java {
		maven[path] = data
		gradle[path] = data
	}
	return map[string]nativeRecipeFixture{
		"go":           {tool: "go", version: "go version", setup: "GOTOOLCHAIN=local go mod download all", bootstrap: "GOTOOLCHAIN=local go mod download", build: "GOTOOLCHAIN=local go build ./...", test: "GOTOOLCHAIN=local go test -v -count=1 ./...", files: map[string]string{"go.mod": "module example.test/fixture\n\ngo 1.21\n\nrequire github.com/google/uuid v1.6.0\n", "sum.go": "package fixture\nimport \"github.com/google/uuid\"\nfunc ID() string { return uuid.Nil.String() }\nfunc Add(a,b int) int { return a+b }\n", "sum_test.go": "package fixture\nimport \"testing\"\nfunc TestSum(t *testing.T) { if Add(2,2)!=4 || ID()!=\"00000000-0000-0000-0000-000000000000\" { t.Fatal(\"sum or dependency\") } }\nfunc TestZero(t *testing.T) { if Add(0,0)!=0 { t.Fatal(\"zero\") } }\n"}},
		"npm":          {tool: "npm", version: "node --version && npm --version", setup: "npm install --package-lock-only", bootstrap: "npm ci", build: "npm run build", test: "npm test", files: js},
		"pnpm":         {tool: "pnpm", version: "pnpm --version", setup: `pnpm install --lockfile-only --store-dir="$PNPM_STORE_PATH"`, bootstrap: `pnpm install --frozen-lockfile --store-dir="$PNPM_STORE_PATH"`, build: "pnpm run build", test: "pnpm test", files: js},
		"yarn-classic": {tool: "yarn", version: "yarn --version", setup: "yarn install", bootstrap: "yarn install --frozen-lockfile", build: "yarn build", test: "yarn test", files: js},
		"yarn-modern":  {tool: "yarn", version: "yarn --version", setup: "yarn install", bootstrap: "yarn install --immutable", build: "yarn build", test: "yarn test", files: js},
		"uv":           {tool: "uv", version: "uv --version && python3 --version", setup: "uv lock --no-python-downloads", bootstrap: "uv sync --frozen --no-python-downloads", build: "uv run --frozen --no-python-downloads python -m compileall -q test_sum.py", test: "uv run --frozen --no-python-downloads python -m unittest -v", files: py},
		"pip":          {tool: "python3", version: "python3 --version && python3 -m ensurepip --version", bootstrap: "python3 -m venv .venv && .venv/bin/python -m pip install -r requirements.txt", build: ".venv/bin/python -m compileall -q test_sum.py", test: ".venv/bin/python -m unittest -v", files: py},
		"cargo":        {tool: "cargo", version: "cargo --version && rustc --version", setup: "cargo generate-lockfile", bootstrap: "cargo fetch --locked", build: "cargo build --locked", test: "cargo test --locked", files: map[string]string{"Cargo.toml": "[package]\nname='fixture'\nversion='0.1.0'\nedition='2021'\n[dependencies]\nitoa='=1.0.15'\n", "src/lib.rs": "pub fn add(a:i32,b:i32)->i32 { a+b }\n#[cfg(test)] mod tests { use super::*; #[test] fn sum(){assert_eq!(add(2,2),4); assert_eq!(itoa::Buffer::new().format(4),\"4\");} #[test] fn zero(){assert_eq!(add(0,0),0);} }\n"}},
		"gradle":       {tool: "gradle", version: "gradle --version", bootstrap: "gradle --no-daemon -Porg.gradle.java.installations.auto-download=false classes testClasses", build: "gradle --no-daemon -Porg.gradle.java.installations.auto-download=false classes", test: "gradle --no-daemon -Porg.gradle.java.installations.auto-download=false test --rerun-tasks", files: gradle},
		"maven":        {tool: "mvn", version: "mvn --version", bootstrap: `mvn -B -Dmaven.repo.local="$MAVEN_REPO" dependency:go-offline`, build: `mvn -B -Dmaven.repo.local="$MAVEN_REPO" compile`, test: `mvn -B -Dmaven.repo.local="$MAVEN_REPO" verify`, files: maven},
		"dotnet":       {tool: "dotnet", version: "dotnet --version", setup: "dotnet new xunit --no-restore --name Fixture --output .", bootstrap: "dotnet restore", build: "dotnet build --no-restore", test: "dotnet test --no-restore --logger 'console;verbosity=normal'", files: map[string]string{}},
		"zig":          {tool: "zig", version: "zig version", bootstrap: "zig test test.zig", build: "zig build-obj test.zig", test: "zig test test.zig", files: map[string]string{"test.zig": "const std=@import(\"std\"); test \"sum\" { try std.testing.expectEqual(@as(i32,4),2+2); } test \"zero\" { try std.testing.expectEqual(@as(i32,0),0+0); }\n"}},
		"c-cpp":        {tool: "cmake", version: "cmake --version && cc --version", bootstrap: "cmake -S . -B build", build: "cmake --build build", test: "ctest --test-dir build --output-on-failure", files: map[string]string{"CMakeLists.txt": "cmake_minimum_required(VERSION 3.20)\nproject(fixture C)\nenable_testing()\nadd_executable(sum sum.c)\nadd_test(NAME sum COMMAND sum)\n", "sum.c": "int main(void) { return (2+2==4) ? 0 : 1; }\n"}},
	}
}

// Opt-in native integration: cold, warm, reopened session and fresh checkout.
// Missing toolchains are reported as skips, never as coverage or successful init.
func TestSandboxNativeRecipes(t *testing.T) {
	selection := os.Getenv("POLLYTOOL_SANDBOX_RECIPE_TESTS")
	if selection == "" {
		t.Skip("set POLLYTOOL_SANDBOX_RECIPE_TESTS=1 (or comma-separated recipe names)")
	}
	fixtures := nativeRecipeFixtures()
	for _, recipe := range readBuildRecipes(t) {
		t.Run(recipe.Name, func(t *testing.T) {
			if selection != "1" && !strings.Contains(","+selection+",", ","+recipe.Name+",") {
				t.Skip("not selected")
			}
			f := fixtures[recipe.Name]
			if _, err := exec.LookPath(f.tool); err != nil {
				t.Skipf("prerequisite not on host PATH: %s", f.tool)
			}
			home, err := os.MkdirTemp(os.Getenv("HOME"), "recipe-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(home)
			t.Setenv("HOME", home)
			t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
			t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("GIT_CONFIG_COUNT", "0")
			t.Setenv("GIT_CONFIG_PARAMETERS", "")
			checkout := filepath.Join(home, "project")
			// Git creates fixture checkouts on the host; every project command below
			// runs through the same sandboxed bash tool used by an ordinary session.
			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-c", "user.name=polly", "-c", "user.email=polly@example.com", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			git("init", "-q", "--template=", checkout)
			for path, data := range f.files {
				name := filepath.Join(checkout, path)
				writeFile(t, name, data)
				// Match ordinary Git checkout modes before generating lockfiles.
				// Yarn's file-package archives include directory/file permissions.
				if err := os.Chmod(name, 0o644); err != nil {
					t.Fatal(err)
				}
				for dir := filepath.Dir(name); dir != checkout; dir = filepath.Dir(dir) {
					if err := os.Chmod(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
			t.Chdir(checkout)
			open := func() *conversationState {
				cfg, err := sandbox.ParsePreset("workspace+net+git")
				if err != nil {
					t.Fatal(err)
				}
				profile := openSandboxProfile(&Config{})
				layer, ok := profile.apply(cfg, sandbox.New)
				if profile.applyErr != nil {
					t.Fatal(profile.applyErr)
				}
				opts := []tools.RegistryOption{tools.WithSandboxFactory(sandbox.New, cfg)}
				if ok {
					opts = append(opts, tools.WithSandboxLayer(sandboxProfileLayer, layer))
				}
				registry := tools.NewToolRegistry(nil, opts...)
				if _, err := registry.LoadToolAuto("bash"); err != nil {
					t.Fatal(err)
				}
				state := &conversationState{toolRegistry: registry, sandboxProfile: profile}
				t.Cleanup(func() { registry.Close(); profile.Close() })
				return state
			}
			state := open()
			startSandboxInit(state)
			if out, code := callSandboxTool(t, context.Background(), state, sandboxPrepareTool, recipeArgs(t, recipe)); code != "" {
				t.Fatalf("prepare: %s %s", code, out)
			}
			for _, link := range recipe.Prepare.Links {
				if link.Directory {
					continue
				}
				path, err := state.sandboxProfile.ws.storageRoots().Resolve(state.sandboxProfile.profile.Storage, link.Target)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := state.toolRegistry.LoadToolAuto("write_file"); err != nil {
					t.Fatal(err)
				}
				writer, _ := state.toolRegistry.Get("write_file")
				if _, err := writer.Execute(context.Background(), tools.Args{"path": path, "content": ""}); err != nil {
					t.Fatal(err)
				}
			}
			run := func(command string) string {
				if command == "" {
					return ""
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				bash, _ := state.toolRegistry.Get("bash")
				out, err := bash.Execute(ctx, tools.Args{"command": command})
				if err != nil {
					t.Fatalf("%s\n%s\n%v", command, out, err)
				}
				return out
			}
			version := run(f.version)
			t.Logf("tool versions: %s", version)
			if recipe.Name == "yarn-classic" && !strings.HasPrefix(strings.TrimSpace(version), "1.") {
				t.Skip("Yarn classic not installed")
			}
			if recipe.Name == "yarn-modern" && strings.HasPrefix(strings.TrimSpace(version), "1.") {
				t.Skip("modern Yarn not installed")
			}
			run(f.setup)
			// Commit only fixture sources and generated lockfiles/templates. A fresh
			// linked worktree must restore its own project-local dependencies.
			for path := range f.files {
				git("add", "--", path)
			}
			for _, name := range []string{"go.sum", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "uv.lock", "Cargo.lock", "Fixture.csproj", "UnitTest1.cs"} {
				if _, err := os.Stat(filepath.Join(checkout, name)); err == nil {
					git("add", "--", name)
				} else if !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			git("commit", "-qm", "fixture")
			for _, phase := range []string{"cold", "warm", "reopened", "new-worktree", "after-reset"} {
				if phase == "reopened" {
					state.toolRegistry.Close()
					state.sandboxProfile.Close()
					state = open()
				}
				if phase == "new-worktree" {
					state.toolRegistry.Close()
					state.sandboxProfile.Close()
					other := filepath.Join(home, "worktree")
					git("worktree", "add", "-q", "--detach", other, "HEAD")
					t.Chdir(other)
					state = open()
				}
				if phase == "after-reset" {
					p := state.sandboxProfile
					if err := p.ensureLease(); err != nil {
						t.Fatal(err)
					}
					if err := p.lease.Exclusive(func() error { return p.ws.storageRoots().Clean(context.Background(), p.profile.Storage, true) }); err != nil {
						t.Fatal(err)
					}
				}
				run(f.bootstrap)
				run(f.build)
				t.Logf("%s tests: %s", phase, run(f.test))
			}
		})
	}
}
