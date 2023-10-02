package nbtests

// This files has "integration tests": tests that execute notebooks using `nbconvert` which in turn executes
// GoNB as its kernel.
//
// It's a very convenient and easy way to run the tests: it conveniently compiles GoNB binary with --cover (to
// include coverage information) and installs it in a temporary Jupyter configuration location, and includes
// some trivial matching functionality to check for the required output strings, see examples below.
//
// The notebooks used for testing are all in `.../gonb/examples/tests` directory.

import (
	"flag"
	"fmt"
	"github.com/janpfeifer/gonb/common"
	"github.com/janpfeifer/gonb/internal/goexec"
	"github.com/janpfeifer/gonb/internal/kernel"
	"github.com/stretchr/testify/require"
	"k8s.io/klog/v2"
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"
)

var panicf = common.Panicf

var (
	flagLogExec       = flag.Bool("log_exec", false, "Log the execution of the notebook")
	flagPrintNotebook = flag.Bool("print_notebook", false, "Print tested notebooks, useful if debugging unexpected results.")
	flagExtraFlags    = flag.String("kernel_args", "--logtostderr",
		"extra arguments passed to `gonb --install` that eventually gets passed to the kernel. "+
			"Commonly for debugging one will want to set \"--logtostderr --vmodule=...\"")

	// gonbRunArgs is passed to `go run` when building the gonb kernel to be tested.
	gonbRunArgs []string
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustValue[T any](v T, err error) T {
	must(err)
	return v
}

func mustRemoveAll(dir string) {
	if dir == "" || dir == "/" {
		return
	}
	must(os.RemoveAll(dir))
}

var (
	rootDir, jupyterDir string
	jupyterExecPath     string
	tmpGocoverdir       string // Created if {REAL_}GOCOVERDIR is not set at start up.
)

// setup for integration tests:
//
//	. Build a gonb binary with --cover (and set GOCOVERDIR).
//	. Set up a temporary jupyter kernel configuration, so that `nbconvert` will use it.
func setup() {
	flag.Parse()
	rootDir = GoNBRootDir()
	if testing.Short() {
		fmt.Println("Test running with --short(), not setting up Jupyter.")
		return
	}

	// Overwrite GOCOVERDIR if $REAL_GOCOVERDIR is given, because
	// -test.gocoverdir value is not propagated.
	// See: https://groups.google.com/g/golang-nuts/c/tg0ZrfpRMSg
	if goCoverDir := os.Getenv("REAL_GOCOVERDIR"); goCoverDir != "" {
		must(os.Setenv("GOCOVERDIR", goCoverDir))
	} else if goCoverDir := os.Getenv("GOCOVERDIR"); goCoverDir != "" {
		// If running manually, and having set GOCOVERDIR, also set REAL_GOCOVERDIR
		// for consistency (both are set).
		must(os.Setenv("REAL_GOCOVERDIR", goCoverDir))
	} else {
		klog.Info(
			"Tests are configured to generate coverage information, but $REAL_GOCOVERDIR or $GOCOVERDIR " +
				"are not set -- see script `run_coverage.sh` for an example. So we are creating a temporary GOCOVERDIR " +
				" that will be deleted in the end.")
		var err error
		tmpGocoverdir, err = os.MkdirTemp("", "gonb_nbtests_gocoverdir_")
		if err != nil {
			panicf("Failed to create a temporary directory for GOCOVERDIR: %+v", err)
		}
		klog.Infof("{REAL_}GOCOVERDIR=%s", tmpGocoverdir)
		must(os.Setenv("GOCOVERDIR", tmpGocoverdir))
		must(os.Setenv("REAL_GOCOVERDIR", tmpGocoverdir))

	}

	// Find jupyter executable.
	var err error
	jupyterExecPath, err = exec.LookPath("jupyter")
	if err != nil {
		panicf(
			"Command `jupyter` is not in path. To run integration tests from `nbtests` "+
				"you need `jupyter` and `nbconvert` installed -- and if installed with Conda "+
				"you need remember to activate your conda environment -- see conda documentation. Error: %+v", err)
	}
	klog.Infof("jupyter: found in %q", jupyterExecPath)

	// Parse extraInstallArgs.
	extraInstallArgs := strings.Split(*flagExtraFlags, " ")

	// Compile and install gonb binary as a local jupyter kernel.
	jupyterDir = mustValue(InstallTmpGonbKernel(gonbRunArgs, extraInstallArgs))
	fmt.Printf("%s=%s\n", kernel.JupyterDataDirEnv, jupyterDir)
}

// TestMain is used to set-up / shutdown needed for these integration tests.
func TestMain(m *testing.M) {
	setup()
	if testing.Short() {
		fmt.Println("Test running with --short(), not setting up Jupyter.")
		return
	}

	// Run tests.
	code := m.Run()

	// Clean up.
	if !testing.Short() {
		mustRemoveAll(jupyterDir)
	}
	if tmpGocoverdir != "" {
		mustRemoveAll(tmpGocoverdir)
	}

	os.Exit(code)
}

// executeNotebook (in `examples/tests`) and returns a reader to the output of the execution.
// It executes using `nbconvert` set to `asciidoc` (text) output.
func executeNotebook(t *testing.T, notebook string) *os.File {

	// Execute notebook.
	notebookRelPath := path.Join("examples", "tests", notebook+".ipynb")
	args := []string{"-n=" + notebookRelPath, "-jupyter_dir=" + rootDir}
	if *flagLogExec {
		args = append(args, "-jupyter_log", "-console_log", "-vmodule=main=1")
	}
	nbexec := exec.Command(
		path.Join(jupyterDir, "nbexec"), args...)
	nbexec.Stderr = os.Stderr
	nbexec.Stdout = os.Stdout
	require.NoErrorf(t, nbexec.Run(), "Failed to execute notebook %q with %q",
		path.Join(rootDir, notebookRelPath), nbexec)

	// Convert notebook output.
	tmpOutput := mustValue(os.CreateTemp("", "gonb_nbtests_output"))
	nbconvertOutputName := tmpOutput.Name()
	must(tmpOutput.Close())
	must(os.Remove(nbconvertOutputName))
	nbconvertOutputPath := nbconvertOutputName + ".asciidoc" // nbconvert adds this suffix.
	nbconvert := exec.Command(
		jupyterExecPath, "nbconvert", "--to", "asciidoc",
		"--output", nbconvertOutputName,
		path.Join(rootDir, notebookRelPath))
	nbconvert.Stdout, nbconvert.Stderr = os.Stderr, os.Stdout
	klog.Infof("Executing: %q", nbconvert)
	err := nbconvert.Run()
	require.NoError(t, err)
	f, err := os.Open(nbconvertOutputPath)
	require.NoErrorf(t, err, "Failed to open the output of %q", nbconvert)
	return f
}

func clearNotebook(t *testing.T, notebook string) {
	// Execute notebook.
	notebookRelPath := path.Join("examples", "tests", notebook+".ipynb")
	nbexec := exec.Command(
		path.Join(jupyterDir, "nbexec"), "-n="+notebookRelPath,
		"-jupyter_dir="+rootDir, "-clear")
	nbexec.Stderr = os.Stderr
	nbexec.Stdout = os.Stdout
	require.NoErrorf(t, nbexec.Run(), "Failed to clear notebook %q with %q",
		path.Join(rootDir, notebookRelPath), nbexec)
}

func TestHello(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration (nbconvert) test for short tests.")
		return
	}
	f := executeNotebook(t, "hello")
	err := Check(f,
		Match(OutputLine(2),
			Separator,
			"Hello World!",
			Separator),
		*flagPrintNotebook)

	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(f.Name()))
	clearNotebook(t, "hello")
}

func TestFunctions(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration (nbconvert) test for short tests.")
		return
	}
	notebook := "functions"
	f := executeNotebook(t, notebook)
	err := Check(f,
		Match(
			OutputLine(3),
			Separator,
			"incr: x=2, y=4.14",
			Separator,
		), *flagPrintNotebook)

	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(f.Name()))
	clearNotebook(t, notebook)
}

func TestInit(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration (nbconvert) test for short tests.")
		return
	}
	notebook := "init"
	f := executeNotebook(t, notebook)
	err := Check(f,
		Sequence(
			Match(
				OutputLine(2),
				Separator,
				"init_a",
				Separator,
			),
			Match(
				OutputLine(3),
				Separator,
				"init_a",
				"init_b",
				Separator,
			),
			Match(
				OutputLine(4),
				Separator,
				"init: v0",
				"init_a",
				"init_b",
				Separator,
			),
			Match(
				OutputLine(5),
				Separator,
				"init: v1",
				"init_a",
				"init_b",
				Separator,
			),
			Match(
				OutputLine(6),
				Separator,
				"removed func init_a",
				"removed func init_b",
				Separator),
			Match(
				OutputLine(7),
				Separator,
				"init: v1",
				"Done",
				Separator,
			),
		),
		*flagPrintNotebook)

	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(f.Name()))
	clearNotebook(t, notebook)
}

// TestGoWork tests support for `go.work` and `%goworkfix` as well as management
// of tracked directories.
func TestGoWork(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration (nbconvert) test for short tests.")
		return
	}
	f := executeNotebook(t, "gowork")
	err := Check(f,
		Sequence(
			Match(
				OutputLine(5),
				Separator,
				`Added replace rule for module "a.com/a/pkg" to local directory`,
				Separator,
			),
			Match(
				OutputLine(6),
				Separator,
				"module gonb_",
				"",
				"go ",
				"",
				"replace a.com/a/pkg => TMP_PKG",
				Separator,
			),
			Match(
				OutputLine(7),
				Separator,
				"List of files/directories being tracked",
				"",
				"/tmp/gonb_tests_gowork_",
				Separator,
			),
			Match(
				OutputLine(9),
				Separator,
				`Untracked "/tmp/gonb_tests_gowork_..."`,
				"",
				"No files or directory being tracked yet",
				Separator,
			),
		), *flagPrintNotebook)

	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(f.Name()))
}

// TestGoFlags tests `%goflags` special command support.
func TestGoFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration (nbconvert) test for short tests.")
		return
	}
	f := executeNotebook(t, "goflags")
	err := Check(f,
		Sequence(
			// Check `%goflags` is correctly keeping/erasing state.
			Match(
				OutputLine(1),
				Separator,
				"%goflags=[\"-cover\"]",
				Separator,
			),
			Match(
				OutputLine(2),
				Separator,
				"%goflags=[\"-cover\"]",
				Separator,
			),
			Match(
				OutputLine(3),
				Separator,
				"%goflags=[]",
				Separator,
			),

			// Check that `-cover` actually had an effect: this it tied to the how go coverage works, and will break
			// the the Go tools change -- probably ok, if it doesn't happen to often.
			// If it does change, just manually run the notebook, see what is the updated output, and if correct,
			// copy over here.
			Match(
				OutputLine(7),
				Separator,
				"A\t\t100.0%",
				"B\t\t0.0%",
			),

			// Check full reset.
			Match(
				OutputLine(8),
				Separator,
				"State reset: all memorized declarations discarded",
				Separator,
			),

			// Check manual running of `go build -gcflags=-m`.
			Match(OutputLine(10), Separator),
			Match("can inline (*Point).ManhattanLen"),
			Match("p does not escape"),
		), *flagPrintNotebook)

	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(f.Name()))
}

// TestGoTest tests support for `%test` to run cells with `go test`.
func TestGoTest(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration (nbconvert) test for short tests.")
		return
	}
	f := executeNotebook(t, "gotest")
	err := Check(f,
		Sequence(
			// Trivial Incr function defined.
			Match(
				OutputLine(2),
				Separator,
				"55",
				"2178309",
				"2178309",
				Separator,
			),

			// TestA checks Incr.
			Match(
				OutputLine(3),
				Separator,
				"RUN   TestA",
				"Testing A",
				"PASS: TestA",
				"PASS",
				// There is some output about coverage that follows.
			),

			// Checks TestA declaration is memorized.
			Match(OutputLine(4), Separator),
			Match("TestA"),
			Match(InputLine(5)),

			// If no test is defined in cell, all tests are run (TestA in this case).
			Match(
				OutputLine(5),
				Separator,
				"RUN   TestA",
				"Testing A",
				"PASS: TestA",
				"PASS",
				// There is some output about coverage that follows.
			),

			// If cells are defined in cell, only tests of cell are run, TestA
			// should be excluded.
			Match(
				OutputLine(6),
				Separator,
				"RUN   TestAB",
				"Testing AB",
				"PASS: TestAB",
				"RUN   TestB",
				"Testing B",
				"PASS: TestB",
				"PASS",
				// There is some output about coverage that follows.
			),

			// Passed args to `go test`, so `--test.v` is disabled.
			Match(
				OutputLine(7),
				Separator,
				"Testing A",
				"Testing AB",
				"Testing B",
				"PASS",
				// There is some output about coverage that follows.
			),

			// Check that both benchmarks run.
			Match(OutputLine(8), Separator),
			Match("BenchmarkFibonacciA32"),
			Match("BenchmarkFibonacciB32"),
			Match("PASS"),
			// There is some output about coverage that follows.

		), *flagPrintNotebook)

	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(f.Name()))
}

func TestBashScript(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration (nbconvert) test for short tests.")
		return
	}
	f := executeNotebook(t, "bash_script")
	err := Check(f,
		Sequence(

			// Trivial "echo hello" .
			Match(
				OutputLine(1),
				Separator,
				"hello",
				Separator,
			),

			// Trivial "echo hello" .
			Match(
				OutputLine(2),
				Separator,
				"/gonb_", // gonb_??? directory created in a temporary subdirectory, usually "/tmp".
				Separator,
			),

			// GoNB environment variables:
			Match(
				OutputLine(3),
				Separator,
				"/examples/tests", // subdirectory where it is executed.
				"/gonb_",          // within a temporary directory.
				"/nbtests",        // root directory where jupyter (nbconvert) was executed.
				Separator,
			),
		), *flagPrintNotebook)

	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(f.Name()))
}

// TestWasm checks that the environment variables are created.
//
// Unfortunately, `nbconvert` doesn't work with WASM, so it won't actually verify the wasm part is working.
//
// It does check that the cell is compiled to a `.wasm` file, as well as `wasm_exec.js` is copied from the
// Go directory.
func disabledTestWasm(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration (nbconvert) test for short tests.")
		return
	}
	f := executeNotebook(t, "wasm")
	var wasmPath string
	err := Check(f,
		Sequence(

			// GONB_JUPYTER_ROOT, GONB_WASM_SUBDIR and GONB_WASM_URL
			Match(
				OutputLine(1),
				Separator,
				"/nbtests",
				"/nbtests/jupyter_files/",
				"/files/jupyter_files/",
				Separator,
			),

			Match(OutputLine(2), Separator),
			Capture(&wasmPath),

			// Execution of dummy WASM: we don't expect nbconvert to run anything,
			// but we expect the compiled .wasm file to be generated.
			Match(
				OutputLine(3),
				Separator,
				"",
				Separator,
			),
		), *flagPrintNotebook)

	fmt.Printf(". WASM files path: %s\n", wasmPath)
	require.DirExists(t, wasmPath)
	require.FileExists(t, path.Join(wasmPath, "wasm_exec.js"))
	require.FileExists(t, path.Join(wasmPath, goexec.CompiledWasmName))

	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(f.Name()))
}

// TestGonbui tests that `Gonbui` library is able to reach the kernel.
func TestGonbui(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration (nbconvert) test for short tests.")
		return
	}

	klog.Infof("GOCOVERDIR=%s", os.Getenv("GOCOVERDIR"))

	require.NoError(t, os.Setenv("GONB_GIT_ROOT", rootDir))
	f := executeNotebook(t, "gonbui")
	err := Check(f,
		Sequence(
			// Check GONB_GIT_ROOT was recognized.
			Match(
				OutputLine(2),
				Separator,
				"ok",
				Separator,
			),

			// Check replace rule was created.
			Match(
				OutputLine(3),
				Separator,
				"Added replace rule for module",
				Separator,
			),

			// Check DisplayHTML.
			Match(
				OutputLine(4),
				Separator,
				"html displayed",
				Separator,
			),

			// Check DisplayMarkdown.
			// It doesn't always work on nbconvert (but it works in the Jupyter notebook).
			// Oddly it used to work earlier. Test disabled for now.
			// Issue report:
			// https://github.com/jupyter/nbconvert/issues/2017
			//
			//Match(
			//	OutputLine(5),
			//	Separator,
			//	"markdown displayed",
			//	Separator,
			//),
		), *flagPrintNotebook)

	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(f.Name()))
}
