// Integration tests for vfuse client & server.

package vfuse

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var (
	verbose = flag.Bool("verbose", false, "verbose")
	broken  = flag.Bool("broken", false, "Run known-broken tests")
)

func knownBroken(t *testing.T) {
	if !*broken {
		t.Skip("skipping known-broken test")
	}
}

// This is the list of tests that call getWorld. It's used so we know
// how many tests will ultimately be run and when to do a best-effort
// cleanup on the release of a world, to remove temp files and such.
//
// TODO(bradfitz): consider generating this list automatically by
// using runtime.Stack and finding the goroutine in testing.Main and
// finding the argument with the slice of InternalTest and using some
// strconv and unsafe. That would be gross and awesome.
var worldlyTests []string

func isWorldTest(name string) bool {
	for _, n := range worldlyTests {
		if n == name {
			return true
		}
	}
	return false
}

// addWorldTest registers a test name that might call getWorld.
func addWorldTest(name string) {
	if !strings.HasPrefix(name, "Test") {
		panic("bogus registration of non-Test")
	}
	if isWorldTest(name) {
		panic("duplicate registration of " + name)
	}
	worldlyTests = append(worldlyTests, name)
}

func currentTestName() string {
	pc, _, _, ok := runtime.Caller(2)
	if !ok {
		panic("Caller failed")
	}
	f := runtime.FuncForPC(pc)
	if f == nil {
		panic("can't find FuncForPC of test caller")
	}
	testName := f.Name()
	i := strings.Index(testName, "Test")
	if i < 0 {
		panic("unexpected test name: " + testName)
	}
	return testName[i:]
}

func getWorld(t *testing.T) *world {
	if runtime.GOOS != "linux" {
		t.Skip("test only runs on linux")
	}
	if n := currentTestName(); !isWorldTest(n) {
		t.Fatalf("getWorld called from %v which was not registered with addWorldTest", n)
	}

	currentTest = t
	if w := singleWorld; w != nil {
		w.t = t
		return w
	}
	testsToRun = countTestsToRun()
	singleWorld = newWorld(t)
	singleWorld.t = t
	return singleWorld
}

func countTestsToRun() int {
	f := flag.Lookup("test.run")
	if f == nil || f.Value.String() == "" {
		return len(worldlyTests)
	}
	rx, err := regexp.Compile(f.Value.String())
	if err != nil {
		// Shouldn't get this far anyway.
		return len(worldlyTests)
	}
	n := 0
	for _, name := range worldlyTests {
		if rx.MatchString(name) {
			n++
		}
	}
	return n
}

var (
	testsToRun  int
	worldsEnded int

	currentTest *testing.T
	singleWorld *world
)

type world struct {
	t *testing.T // changed per test. rest is static.

	port      int
	binDir    string
	mountDir  string
	clientDir string

	server      *exec.Cmd
	serverStdin io.WriteCloser

	client *exec.Cmd
}

func newWorld(t *testing.T) *world {
	w := &world{
		binDir:    tempDir(t, "bin"),
		mountDir:  tempDir(t, "mount"),
		clientDir: tempDir(t, "client"),
		port:      7070, // TODO: auto-pick a free one
	}

	vfused := filepath.Join(w.binDir, "vfused")
	out, err := exec.Command("go", "build", "-o", vfused, "github.com/dotcloud/docker/vfuse/vfused").CombinedOutput()
	if err != nil {
		t.Fatalf("vfused build failure: %v, %s", err, out)
	}
	vclient := filepath.Join(w.binDir, "vclient")
	out, err = exec.Command("go", "build", "-o", vclient, "github.com/dotcloud/docker/vfuse/client").CombinedOutput()
	if err != nil {
		t.Fatalf("client build failure: %v, %s", err, out)
	}

	w.server = exec.Command(vfused,
		"--mount="+w.mountDir,
		"--listen="+strconv.Itoa(w.port),
		"--verbose="+strconv.FormatBool(*verbose),
	)
	if *verbose {
		w.server.Stdout = os.Stdout
		w.server.Stderr = os.Stderr
	}
	sin, err := w.server.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	w.serverStdin = sin
	if err := w.server.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		if isMounted(w.mountDir) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !isMounted(w.mountDir) {
		t.Fatal("never saw %s get mounted", w.mountDir)
	}

	w.client = exec.Command(vclient,
		"--addr=localhost:"+strconv.Itoa(w.port),
		"--verbose="+strconv.FormatBool(*verbose),
	)
	w.client.Stdout = os.Stdout
	w.client.Stderr = os.Stderr
	w.client.Dir = w.clientDir
	if err := w.client.Start(); err != nil {
		t.Fatal(err)
	}

	return w
}

// fpath wraps filepath.Join(w.fuseMountDir, path...).
func (w *world) fpath(path ...string) string { return w.pathJoin(w.mountDir, path) }

// fpath wraps filepath.Join(w.clientDir, path...).
func (w *world) cpath(path ...string) string { return w.pathJoin(w.clientDir, path) }

func (w *world) pathJoin(base string, path []string) string {
	arg := make([]string, 0, len(path)+1)
	arg = append(arg, base)
	arg = append(arg, path...)
	return filepath.Join(arg...)
}

func (w *world) mkdir(path string) {
	if err := os.MkdirAll(path, 0755); err != nil {
		w.t.Fatalf("Error making dir %s: %v", path, err)
	}
}

func (w *world) writeFile(path string, contents string) {
	w.mkdir(filepath.Dir(path))
	if err := ioutil.WriteFile(path, []byte(contents), 0644); err != nil {
		w.t.Fatalf("Error writing %s: %v", path, err)
	}
}

func (w *world) release() {
	worldsEnded++
	if worldsEnded < testsToRun {
		return
	}
	if worldsEnded > testsToRun {
		w.t.Fatalf("unexpected number of releases called on world. forget to register in worldlyTests?")
	}
	w.t.Logf("(end of all tests; shutting down world)")

	w.client.Process.Kill()
	w.serverStdin.Write([]byte("q\n")) // tell FUSE server to close nicely
	w.server.Wait()                    // TODO(bradfitz): in a goroutine racing against a time limit?

	exec.Command("fusermount", "-u", w.mountDir).Run() // just in case

	removeAll(w.binDir)
	removeAll(w.mountDir)
	removeAll(w.clientDir)
}

func isMounted(dir string) bool {
	slurp, _ := ioutil.ReadFile("/proc/mounts")
	return bytes.Contains(slurp, []byte(dir)) // close enough.
}

func tempDir(t *testing.T, name string) string {
	dir, err := ioutil.TempDir("", name)
	if err != nil {
		t.Fatalf("Error making temp dir: %v", err)
	}
	return dir
}

func removeAll(path string) {
	if path == "" {
		panic("removeAll of empty string?")
	}
	os.RemoveAll(path) // best effort: just a tempdir
}

// Stat a regular file.
func init() { addWorldTest("TestStatRegular") }
func TestStatRegular(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	const contents = "Some file contents.\n"
	const file = "stat_reg/file.txt"
	w.writeFile(w.cpath(file), contents)

	fi, err := os.Lstat(w.fpath(file))
	if err != nil {
		t.Fatalf("Lstat = %v; want valid file", err)
	}
	if fi.Size() != int64(len(contents)) {
		t.Errorf("stat size = %d; want %d", fi.Size(), len(contents))
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("file isn't regular")
	}
}

// Stat a non-existant file.
func init() { addWorldTest("TestStatNoExist") }
func TestStatNoExist(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	if _, err := os.Lstat(w.fpath("file-no-exist.txt")); !os.IsNotExist(err) {
		t.Errorf("For non-existant file, want os.IsNotExist; got err = %v", err)
	}
}

// Stat a directory.
func init() { addWorldTest("TestStatDir") }
func TestStatDir(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.mkdir(w.cpath("stat_dir"))

	fi, err := os.Lstat(w.fpath("stat_dir"))
	if err != nil {
		t.Fatalf("Lstat = %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("Mode = %v; want Dir", fi.Mode())
	}
}

// Stat a symlink.
func init() { addWorldTest("TestStatSymlink") }
func TestStatSymlink(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.mkdir(w.cpath("stat_symlink"))
	if err := os.Symlink("some-target", w.cpath("stat_symlink/link")); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(w.fpath("stat_symlink/link"))
	if err != nil {
		t.Fatalf("Lstat = %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("Mode = %v; want symlink bit", fi.Mode())
	}
}

// Readlink a symlink.
func init() { addWorldTest("TestReadlink") }
func TestReadlink(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	const target = "some-target"
	w.mkdir(w.cpath("readlink"))
	if err := os.Symlink(target, w.cpath("readlink/link")); err != nil {
		t.Fatal(err)
	}

	got, err := os.Readlink(w.fpath("readlink/link"))
	if err != nil {
		t.Fatalf("Readlink = %v", err)
	}
	if got != target {
		t.Errorf("Readlink = %q; want %q", got, target)
	}
}

// Readdirnames on empty dir
func init() { addWorldTest("TestReaddirnamesEmpty") }
func TestReaddirnamesEmpty(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	const dir = "readdir_empty"
	w.mkdir(w.cpath(dir))

	f, err := os.Open(w.fpath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("Readdirnames = %q; want empty", names)
	}
	if err := f.Close(); err != nil {
		t.Error(err)
	}
}

// Readdirnames on non-empty dir
func init() { addWorldTest("TestReaddirnames") }
func TestReaddirnames(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("dirnames/1.txt"), "file one")
	w.writeFile(w.cpath("dirnames/2.txt"), "file two")

	f, err := os.Open(w.fpath("dirnames"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	want := []string{"1.txt", "2.txt"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("Readdirnames = %q; want %q", names, want)
	}
	if err := f.Close(); err != nil {
		t.Error(err)
	}
}

// summarize walks root and returns a multi-line string describing the walk.
func summarize(root string) (string, error) {
	var buf bytes.Buffer
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(&buf, "%v = %v", filepath.ToSlash(rel), fi.Mode())
		if fi.Mode().IsRegular() {
			fmt.Fprintf(&buf, " (size %d)", fi.Size())
		}
		buf.WriteByte('\n')
		return nil
	})
	return buf.String(), err
}

// Readdirnames on non-empty dir
func init() { addWorldTest("TestReaddirWalk") }
func TestReaddirWalk(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("dirwalk/1.txt"), "one")
	w.writeFile(w.cpath("dirwalk/sub/2.txt"), "and two")

	want, err := summarize(w.cpath("dirwalk"))
	if err != nil {
		t.Fatalf("client Walk error: %v", err)
	}
	got, err := summarize(w.fpath("dirwalk"))
	if err != nil {
		t.Fatalf("fuse walk error: %v", err)
	}
	if got != want {
		t.Errorf("Walk got:\n%s\n\nWant:\n%s", got, want)
	}
}

// Chmod file
func init() { addWorldTest("TestChmod") }
func TestChmod(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("chmod/1.txt"), "test file")

	if err := os.Chmod(w.fpath("chmod/1.txt"), 0777); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(w.cpath("chmod/1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().String() != "-rwxrwxrwx" {
		t.Fatalf("Mode %s, expected %s", st.Mode(), os.FileMode(0777))
	}
}

// Open a file for read.
func init() { addWorldTest("TestOpenRead") }
func TestOpenRead(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	const contents = "Some test file"
	w.writeFile(w.cpath("openread/f.txt"), contents)

	f, err := os.Open(w.fpath("openread/f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() // backup

	all, err := ioutil.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(all) != contents {
		t.Errorf("Contents = %q; want %q", all, contents)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// mkdir directory
func init() { addWorldTest("TestMkdir") }
func TestMkdir(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	fpath := w.fpath("mkdir-new_dir")
	err := os.Mkdir(fpath, 0700)
	if err != nil {
		t.Fatalf("Failed to create directory %q: %v", fpath, err)
	}

	cpath := w.cpath("mkdir-new_dir")
	fi, err := os.Stat(cpath)
	if err != nil {
		t.Fatal("Failed to stat directory %q: %v", cpath, err)
	}
	if !fi.IsDir() {
		t.Fatalf("%q is not a directory", cpath)
	}
	if fi.Mode().String() != "drwx------" {
		t.Fatalf("Mode %s, expected %s", fi.Mode(), os.FileMode(0700))
	}
}

// Rename file
func init() { addWorldTest("TestRename") }
func TestRename(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("rename/before.txt"), "test file")

	base := w.fpath("rename")
	err := os.Rename(filepath.Join(base, "before.txt"), filepath.Join(base, "after.txt"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(w.cpath("rename/after.txt"))
	if err != nil {
		t.Fatal(err)
	}
}

// Rename non-existing file
func init() { addWorldTest("TestRenameNonExisting") }
func TestRenameNonExisting(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	base := w.fpath("rename-non-existing")
	os.Rename(filepath.Join(base, "does-not-exist.txt"), filepath.Join(base, "after.txt"))
	if _, err := os.Lstat(w.cpath("rename-non-existing/after.txt")); !os.IsNotExist(err) {
		t.Errorf("For non-existant file, want os.IsNotExist; got err = %v", err)
	}
}

func init() { addWorldTest("TestRenameTargetExists") }
func TestRenameTargetExists(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("rename-target-exists/1.txt"), "test file")
	w.writeFile(w.cpath("rename-target-exists/2.txt"), "test file 2")

	err := os.Rename(w.fpath("rename-target-exists/1.txt"), w.fpath("rename-target-exists/2.txt"))
	if err != nil {
		t.Fatal("Expected rename to overwrite")
	}
	contents, err := ioutil.ReadFile(w.cpath("rename-target-exists/2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "test file" {
		t.Errorf("Expected %q got %q", "test file", string(contents))
	}
}

func init() { addWorldTest("TestRenameTargetDir") }
func TestRenameTargetDir(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("rename-target-dir/src/1.txt"), "test file")

	err := os.Rename(w.fpath("rename-target-dir/src"), w.fpath("rename-target-dir/dest"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(w.cpath("rename-target-dir/dest/1.txt"))
	if err != nil {
		t.Fatal(err)
	}
}

func init() { addWorldTest("TestRenameTargetDirNonEmpty") }
func TestRenameTargetDirNonEmpty(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("rename-target-dir/src/1.txt"), "test file")
	w.writeFile(w.cpath("rename-target-dir/dest/2.txt"), "test file 2")

	err := os.Rename(w.fpath("rename-target-dir/src"), w.fpath("rename-target-dir/dest"))
	// go fuse doesn't seem to have ENOTEMPTY we get an EIO.
	if err == nil {
		t.Fatal("Expected rename to fail")
	}
}

// rmdir directory
func init() { addWorldTest("TestRmdir") }
func TestRmdir(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.mkdir(w.cpath("rmdir-new_dir"))

	fpath := w.fpath("rmdir-new_dir")
	err := os.Remove(fpath)
	if err != nil {
		t.Fatalf("Failed to rm directory %q: %v", fpath, err)
	}

	cpath := w.cpath("rmdir-new_dir")
	_, err = os.Stat(cpath)
	if err == nil {
		t.Fatal("directory %q still exists", cpath)
	}
	if !os.IsNotExist(err) {
		t.Fatal("directory %q still exists: %v", cpath, err)
	}
}

// Symlink file
func init() { addWorldTest("TestSymlink") }
func TestSymlink(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("symlink/before.txt"), "test file")

	err := os.Symlink("../symlink/before.txt", w.fpath("symlink/after.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(w.cpath("symlink/after.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("File is not symlink")
	}
	content, err := ioutil.ReadFile(w.cpath("symlink/after.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "test file" {
		t.Fatal("Wrong content read from symlink, got %s, must be \"test file\"", content)
	}
}

// Symlink file exists
func init() { addWorldTest("TestSymlinkFileAlreadyExists") }
func TestSymlinkFileAlreadyExists(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("symlink/exists/before.txt"), "test file")
	w.writeFile(w.cpath("symlink/exists/after.txt"), "test file exists")

	err := os.Symlink("../symlink/exists/before.txt", w.fpath("symlink/exists/after.txt"))
	if !os.IsExist(err) {
		t.Fatalf("Error must be 'not exist', got %T:%s", err, err)
	}
}

// Symlink dir
func init() { addWorldTest("TestSymlinkDir") }
func TestSymlinkDir(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("symlink/dir/before/linked.txt"), "test file")

	err := os.Symlink("../dir/before", w.fpath("symlink/dir/after"))
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(w.cpath("symlink/dir/after"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("Directory is not symlink")
	}
	content, err := ioutil.ReadFile(w.cpath("symlink/dir/after/linked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "test file" {
		t.Fatal("Wrong content read from symlinked dir, got %s, must be \"test file\"", content)
	}
}

// Utime file
func init() { addWorldTest("TestUtime") }
func TestUtime(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("utime/1.txt"), "test file")

	atime := time.Unix(5, 4)
	mtime := time.Unix(6, 7)
	if err := os.Chtimes(w.fpath("utime/1.txt"), atime, mtime); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(w.cpath("utime/1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.ModTime() != mtime {
		t.Fatal(err)
	}
	stat := st.Sys().(*syscall.Stat_t)
	natime := time.Unix(int64(stat.Atim.Sec), int64(stat.Atim.Nsec))
	if atime != natime {
		t.Fatalf("Expected atime %v, got %v", atime, natime)
	}
}

// Unlink file
func init() { addWorldTest("TestUnlink") }
func TestUnlink(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("unlink/1.txt"), "test file")

	if err := os.Remove(w.fpath("unlink/1.txt")); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(w.cpath("unlink/1.txt"))
	if !os.IsNotExist(err) {
		t.Fatal("File still alive after unlink")
	}
}

// Unlink not exist file
func init() { addWorldTest("TestUnlinkNotExist") }
func TestUnlinkNotExist(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	if err := os.Remove(w.fpath("unlink/1.txt")); !os.IsNotExist(err) {
		t.Fatalf("Must be 'not exist' error, got %s", err)
	}
}

// Unlink dir
func init() { addWorldTest("TestUnlinkDir") }
func TestUnlinkDir(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.mkdir(w.cpath("unlink/dir"))

	if err := syscall.Unlink(w.fpath("unlink/dir")); err != syscall.EISDIR {
		t.Fatalf("Must be 'is a directory' error, got %s", err)
	}
}

// Truncate file
func init() { addWorldTest("TestTruncateFile") }
func TestTruncateFile(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("truncate/file"), "123456")

	if err := os.Truncate(w.fpath("truncate/file"), 3); err != nil {
		t.Fatal(err)
	}
	content, err := ioutil.ReadFile(w.cpath("truncate/file"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "123" {
		t.Fatalf("Trucated file content %q, but expected \"123\"", content)
	}
}

// Truncate open file
func init() { addWorldTest("TestTruncateOpenFile") }
func TestTruncateOpenFile(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	w.writeFile(w.cpath("truncate/openfile"), "123456")

	f, err := os.OpenFile(w.fpath("truncate/openfile"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(3)
	f.Close()
	if err != nil {
		t.Fatalf("%T, %s", err, err)
	}
	content, err := ioutil.ReadFile(w.cpath("truncate/openfile"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "123" {
		t.Fatalf("Trucated file content %q, but expected \"123\"", content)
	}
}

// Mknod
func init() { addWorldTest("TestMknod") }
func TestMknod(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	mode := uint32(syscall.S_IFSOCK | 0700)
	dev := 0

	w.mkdir(w.cpath("mknod"))

	if err := syscall.Mknod(w.fpath("mknod/dev"), mode, dev); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(w.cpath("mknod/dev"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatal("Expected to be socket; got %v", fi.Mode())
	}
}

// Chown
func init() { addWorldTest("TestChown") }
func TestChown(t *testing.T) {
	w := getWorld(t)
	defer w.release()

	euid := os.Geteuid()
	egid := os.Getegid()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	var gid int = -1
	for _, gid = range groups {
		if gid != egid {
			break
		}
	}
	if gid == -1 {
		t.Fatal("Could not find additional group for %q groups: %v", gid, groups)
	}
	w.writeFile(w.cpath("chown/file"), "123456")

	if err := syscall.Chown(w.fpath("chown/file"), euid, gid); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(w.cpath("chown/file"))
	if err != nil {
		t.Fatal(err)
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("Failed to get Stat info from file")
	}
	if int(sys.Gid) != gid {
		t.Fatal("Expected group id %q not actual group id %q", gid, int(sys.Gid))
	}
}
