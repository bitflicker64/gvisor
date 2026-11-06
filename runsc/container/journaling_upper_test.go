// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package container

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs/journal"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/sandbox"
	"gvisor.dev/gvisor/runsc/specutils"
)

// journalingUpperTest runs containers that share one journaling rootfs
// upper.
type journalingUpperTest struct {
	t      *testing.T
	prefix string
	// lowerFile is a file in the lower layer (the host root).
	lowerFile string
	// overlayDir is the directory of the root overlay's filestore, without
	// the journaling upper.
	overlayDir string
	// bigSum is the MD5 sum of /journaled/big.
	bigSum string
}

func newJournalingUpperTest(t *testing.T) *journalingUpperTest {
	dir := t.TempDir()
	lowerFile := filepath.Join(dir, "lower-file")
	if err := os.WriteFile(lowerFile, []byte("lower"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return &journalingUpperTest{
		t:          t,
		prefix:     filepath.Join(dir, "upper"),
		lowerFile:  lowerFile,
		overlayDir: t.TempDir(),
	}
}

func (p *journalingUpperTest) conf(imageID string) *config.Config {
	conf := p.plainConf()
	conf.JournalingRootfsUpper = p.prefix
	conf.JournalingRootfsUpperImageID = imageID
	return conf
}

// create creates a container that runs args. The root filesystem is the host
// root, overlaid with the journaling upper.
func (p *journalingUpperTest) create(conf *config.Config, args ...string) (*Container, func(), error) {
	return p.createWith(conf, nil, nil, args...)
}

// createWith is create, with functions that change the spec and the Args.
func (p *journalingUpperTest) createWith(conf *config.Config, mutateSpec func(*specs.Spec), mutateArgs func(*Args), args ...string) (*Container, func(), error) {
	spec := testutil.NewSpecWithArgs(args...)
	spec.Root = &specs.Root{Path: "/", Readonly: false}
	if mutateSpec != nil {
		mutateSpec(spec)
	}
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		p.t.Fatalf("SetupContainer: %v", err)
	}
	cargs := Args{ID: testutil.RandomContainerID(), Spec: spec, BundleDir: bundleDir}
	if mutateArgs != nil {
		mutateArgs(&cargs)
	}
	c, err := New(conf, cargs)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return c, func() {
		if err := c.Destroy(); err != nil {
			p.t.Errorf("Destroy: %v", err)
		}
		cleanup()
		p.waitUnlocked()
	}, nil
}

// start creates and starts a container that sleeps.
func (p *journalingUpperTest) start(conf *config.Config) (*Container, func()) {
	p.t.Helper()
	c, cleanup, err := p.create(conf, "sleep", "1000")
	if err != nil {
		p.t.Fatalf("creating container: %v", err)
	}
	if err := c.Start(conf); err != nil {
		cleanup()
		p.t.Fatalf("starting container: %v", err)
	}
	return c, cleanup
}

func (p *journalingUpperTest) sh(conf *config.Config, c *Container, script string) string {
	p.t.Helper()
	out, err := executeCombinedOutput(conf, c, nil, "/bin/sh", "-c", script)
	if err != nil {
		p.t.Fatalf("script %q failed: %v, output: %s", script, err, out)
	}
	return string(out)
}

// check verifies the files that the first container wrote.
func (p *journalingUpperTest) check(conf *config.Config, c *Container) {
	p.t.Helper()
	got := p.sh(conf, c, fmt.Sprintf(`
cat /journaled/d/f
cat /journaled/d/link
md5sum /journaled/big | cut -d' ' -f1
stat -c '%%a %%s' /journaled/d/f
if test -e %s; then echo lower present; else echo lower deleted; fi
ls /journaled/d
`, p.lowerFile))
	want := "hello\nhello\n" + p.bigSum + "\n600 6\nlower deleted\nf\nlink\n"
	if got != want {
		p.t.Errorf("files after restart:\n%s\nwant:\n%s", got, want)
	}
	if _, err := os.Stat(p.lowerFile); err != nil {
		p.t.Errorf("lower file was modified on the host: %v", err)
	}
}

func TestJournalingRootfsUpper(t *testing.T) {
	p := newJournalingUpperTest(t)
	conf := p.conf("image-1")

	// First sandbox: write files and delete a lower file, then SIGKILL the
	// sandbox.
	c1, cleanup1 := p.start(conf)
	p.bigSum = strings.TrimSpace(p.sh(conf, c1, `
mkdir -p /journaled/d
echo hello > /journaled/d/f
chmod 600 /journaled/d/f
ln -s f /journaled/d/link
dd if=/dev/urandom of=/journaled/big bs=4096 count=300 2>/dev/null
rm `+p.lowerFile+`
md5sum /journaled/big | cut -d' ' -f1
`))

	// A second sandbox can't use the same journaling upper.
	if _, cleanup, err := p.create(p.conf("image-1"), "sleep", "1000"); err == nil {
		cleanup()
		t.Errorf("creating a second sandbox with the same journaling upper succeeded")
	} else if !strings.Contains(err.Error(), "in use by another sandbox") {
		t.Errorf("creating a second sandbox: got error %v, want \"in use by another sandbox\"", err)
	}

	if err := unix.Kill(c1.SandboxPid(), unix.SIGKILL); err != nil {
		t.Fatalf("killing sandbox: %v", err)
	}
	if _, err := c1.Wait(); err != nil {
		t.Logf("Wait after SIGKILL: %v", err)
	}
	cleanup1()
	for _, suffix := range []string{".filestore", ".journal"} {
		if _, err := os.Stat(p.prefix + suffix); err != nil {
			t.Fatalf("journaling upper file deleted after destroy: %v", err)
		}
	}

	// Second sandbox: the files survived the crash. Then stop cleanly.
	c2, cleanup2 := p.start(conf)
	p.check(conf, c2)
	p.sh(conf, c2, "echo clean > /journaled/clean")
	cleanup2()

	// A sandbox whose init exits by itself.
	c3, cleanup3, err := p.create(conf, "/bin/sh", "-c", "echo exited > /journaled/exited")
	if err != nil {
		t.Fatalf("creating container: %v", err)
	}
	if err := c3.Start(conf); err != nil {
		t.Fatalf("starting container: %v", err)
	}
	if ws, err := c3.Wait(); err != nil || ws.ExitStatus() != 0 {
		t.Fatalf("container exited with %v, %v", ws, err)
	}
	cleanup3()

	// Third sandbox: the files survived clean stops.
	c4, cleanup4 := p.start(conf)
	p.check(conf, c4)
	if got, want := p.sh(conf, c4, "cat /journaled/clean /journaled/exited"), "clean\nexited\n"; got != want {
		t.Errorf("files after clean restart: got %q, want %q", got, want)
	}
	cleanup4()

	// A different image ID is refused.
	badConf := p.conf("image-2")
	c5, cleanup5, err := p.create(badConf, "sleep", "1000")
	if err == nil {
		err = c5.Start(badConf)
		cleanup5()
	}
	if err == nil {
		t.Errorf("starting a sandbox with another image ID succeeded")
	} else if !strings.Contains(err.Error(), `journal was created for image "image-1"`) {
		t.Errorf("starting a sandbox with another image ID: got error %v, want image mismatch", err)
	}

	// The refusal did not damage the upper.
	c6, cleanup6 := p.start(conf)
	p.check(conf, c6)
	cleanup6()
}

// kill SIGKILLs the sandbox of c, then calls cleanup.
func (p *journalingUpperTest) kill(c *Container, cleanup func()) {
	p.t.Helper()
	if err := unix.Kill(c.SandboxPid(), unix.SIGKILL); err != nil {
		p.t.Fatalf("killing sandbox: %v", err)
	}
	c.Wait()
	cleanup()
}

// waitUnlocked waits until no sandbox holds the journal's lock. The lock is
// released when the killed sandbox process has fully exited, which can be
// after Destroy returns.
func (p *journalingUpperTest) waitUnlocked() {
	p.t.Helper()
	f, err := os.Open(p.prefix + ".journal")
	if err != nil {
		p.t.Fatalf("opening journal: %v", err)
	}
	defer f.Close()
	start := time.Now()
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			unix.Flock(int(f.Fd()), unix.LOCK_UN)
			if d := time.Since(start); d > time.Second {
				p.t.Logf("journal lock released %v after the sandbox was destroyed", d)
			}
			return
		}
		if time.Since(start) > pollTimeout {
			p.t.Fatalf("journal is still locked %v after the sandbox was destroyed: %v", pollTimeout, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// files returns the contents of the given files, or "-" for missing files.
func (p *journalingUpperTest) files(conf *config.Config, c *Container, names ...string) string {
	p.t.Helper()
	script := ""
	for _, name := range names {
		script += fmt.Sprintf("cat /journaled/%s 2>/dev/null || echo -; ", name)
	}
	return p.sh(conf, c, script)
}

// filestoreSize returns the size of the journaling upper's filestore.
func (p *journalingUpperTest) filestoreSize() int64 {
	p.t.Helper()
	fi, err := os.Stat(p.prefix + ".filestore")
	if err != nil {
		p.t.Fatalf("stat filestore: %v", err)
	}
	return fi.Size()
}

// TestJournalingRootfsUpperShrink checks that runsc shrinks the filestore to
// the journal's filestore end mark before a sandbox starts, and that the
// files survive this.
func TestJournalingRootfsUpperShrink(t *testing.T) {
	p := newJournalingUpperTest(t)
	conf := p.conf("image-1")
	const bigSize = 64 << 20

	c1, cleanup1 := p.start(conf)
	// Write the small files first. The filestore keeps the pages of a file
	// where they are, and a file written right after the deletion can be
	// placed above the deleted file's pages, which the allocator does not
	// reuse at once.
	p.sh(conf, c1, `
mkdir -p /journaled
echo small > /journaled/small
echo after > /journaled/after
dd if=/dev/urandom of=/journaled/big bs=1M count=64 2>/dev/null
rm /journaled/big
`)
	p.kill(c1, cleanup1)
	t.Logf("filestore size after the first sandbox: %d bytes", p.filestoreSize())

	// The journal can refer to the deleted file until it is compacted. Each
	// start compacts the journal after replay.
	start := time.Now()
	c2, cleanup2 := p.start(conf)
	t.Logf("first restart took %v", time.Since(start))
	if got, want := p.files(conf, c2, "small", "big", "after"), "small\n-\nafter\n"; got != want {
		t.Errorf("files after the first restart: got %q, want %q", got, want)
	}
	p.kill(c2, cleanup2)
	before := p.filestoreSize()

	// Shrink the filestore as the next start does.
	f, err := openJournalingRootfsUpperFilestore(p.prefix, false /* isNew */)
	if err != nil {
		t.Fatalf("openJournalingRootfsUpperFilestore: %v", err)
	}
	f.Close()
	after := p.filestoreSize()
	t.Logf("filestore shrank from %d to %d bytes", before, after)
	if after >= bigSize || after%int64(os.Getpagesize()) != 0 {
		t.Errorf("filestore size after shrinking is %d bytes, want a multiple of the page size below %d", after, bigSize)
	}

	start = time.Now()
	c3, cleanup3 := p.start(conf)
	t.Logf("second restart took %v", time.Since(start))
	if got, want := p.files(conf, c3, "small", "big", "after"), "small\n-\nafter\n"; got != want {
		t.Errorf("files after the second restart: got %q, want %q", got, want)
	}
	cleanup3()
}

// plainConf returns a configuration without the journaling upper. Its root
// overlay medium is dir=, which conf keeps, so that checkpoints of a sandbox
// with the journaling upper can restore without it.
func (p *journalingUpperTest) plainConf() *config.Config {
	conf := testutil.TestConfig(p.t)
	if err := conf.Overlay2.Set("root:dir=" + p.overlayDir); err != nil {
		p.t.Fatalf("Overlay2.Set: %v", err)
	}
	return conf
}

// fileState is the size, modification time and contents of a file.
type fileState struct {
	size  int64
	mtime time.Time
	data  []byte
}

// prefixState returns the state of the journaling upper's files.
func (p *journalingUpperTest) prefixState() map[string]fileState {
	p.t.Helper()
	m := make(map[string]fileState)
	for _, suffix := range []string{".filestore", ".journal"} {
		path := p.prefix + suffix
		fi, err := os.Stat(path)
		if err != nil {
			p.t.Fatalf("stat %q: %v", path, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			p.t.Fatalf("read %q: %v", path, err)
		}
		m[suffix] = fileState{size: fi.Size(), mtime: fi.ModTime(), data: data}
	}
	return m
}

// checkPrefixState checks that the journaling upper's files did not change.
func (p *journalingUpperTest) checkPrefixState(what string, want map[string]fileState) {
	p.t.Helper()
	for suffix, got := range p.prefixState() {
		w := want[suffix]
		if got.size != w.size || !got.mtime.Equal(w.mtime) || !bytes.Equal(got.data, w.data) {
			p.t.Errorf("%s: %s changed: size %d -> %d, mtime %v -> %v", what, suffix, w.size, got.size, w.mtime, got.mtime)
		}
	}
}

// TestJournalingRootfsUpperCheckpointPlainRestore checks that a checkpoint
// and a filesystem checkpoint of a sandbox with a journaling upper restore
// without the journaling upper, and that the restores don't touch the
// journaling upper's files.
func TestJournalingRootfsUpperCheckpointPlainRestore(t *testing.T) {
	p := newJournalingUpperTest(t)
	conf := p.conf("image-1")
	dir := t.TempDir()
	fsDir := t.TempDir()

	c1, cleanup1 := p.start(conf)
	p.sh(conf, c1, "mkdir -p /journaled && echo A > /journaled/a && rm "+p.lowerFile)
	if err := c1.FSSave(conf, fsDir, sandbox.FSSaveOpts{Paths: []string{"/"}}); err != nil {
		t.Fatalf("FSSave: %v", err)
	}
	p.sh(conf, c1, "echo B > /journaled/b")
	if err := c1.Checkpoint(conf, dir, sandbox.CheckpointOpts{}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	c1.Wait()
	cleanup1()
	before := p.prefixState()
	script := "cat /journaled/a /journaled/b 2>/dev/null; test -e " + p.lowerFile + " && echo lower present; echo C > /journaled/c"

	// Restore the checkpoint without the journaling upper.
	plain := p.plainConf()
	c2, cleanup2, err := p.createWith(plain, nil, func(args *Args) { args.CheckpointDirPath = dir }, "sleep", "1000")
	if err != nil {
		t.Fatalf("creating container: %v", err)
	}
	if err := c2.Restore(plain, dir, sandbox.DirectIOOff, false /* background */, nil /* networkArgs */); err != nil {
		cleanup2()
		t.Fatalf("Restore: %v", err)
	}
	if got, want := p.sh(plain, c2, script), "A\nB\n"; got != want {
		t.Errorf("after restore without the journaling upper: got %q, want %q", got, want)
	}
	cleanup2()
	p.checkPrefixState("after restore", before)

	// Start from the filesystem checkpoint without the journaling upper.
	c3, cleanup3, err := p.createWith(plain, nil, func(args *Args) { args.FSRestoreImagePaths = []string{fsDir} }, "sleep", "1000")
	if err != nil {
		t.Fatalf("creating container: %v", err)
	}
	if err := c3.Start(plain); err != nil {
		cleanup3()
		t.Fatalf("Start: %v", err)
	}
	if err := c3.WaitFSRestore(); err != nil {
		t.Fatalf("WaitFSRestore: %v", err)
	}
	if got, want := p.sh(plain, c3, script), "A\n"; got != want {
		t.Errorf("after filesystem restore without the journaling upper: got %q, want %q", got, want)
	}
	cleanup3()
	p.checkPrefixState("after filesystem restore", before)

	// The journaling upper still holds the state of the first sandbox.
	c4, cleanup4 := p.start(conf)
	if got, want := p.sh(conf, c4, "cat /journaled/a /journaled/b /journaled/c 2>/dev/null; test -e "+p.lowerFile+" && echo lower present; true"), "A\nB\n"; got != want {
		t.Errorf("after restart with the journaling upper: got %q, want %q", got, want)
	}
	cleanup4()
}

// journalingUpperImages holds images of a sandbox whose upper holds
// /journaled/a with contents "A".
type journalingUpperImages struct {
	checkpoint   string
	fsCheckpoint string
	tar          string
}

// makeImages creates a checkpoint, a filesystem checkpoint and a rootfs upper
// tar of a sandbox with p's journaling upper.
func (p *journalingUpperTest) makeImages() journalingUpperImages {
	p.t.Helper()
	conf := p.conf("image-1")
	imgs := journalingUpperImages{
		checkpoint:   p.t.TempDir(),
		fsCheckpoint: p.t.TempDir(),
		tar:          filepath.Join(p.t.TempDir(), "upper.tar"),
	}
	c, cleanup := p.start(conf)
	p.sh(conf, c, "mkdir -p /journaled && echo A > /journaled/a")
	if err := c.FSSave(conf, imgs.fsCheckpoint, sandbox.FSSaveOpts{Paths: []string{"/"}}); err != nil {
		p.t.Fatalf("FSSave: %v", err)
	}
	tarFile, err := os.Create(imgs.tar)
	if err != nil {
		p.t.Fatalf("Create: %v", err)
	}
	if err := c.TarRootfsUpperLayer(tarFile); err != nil {
		p.t.Fatalf("TarRootfsUpperLayer: %v", err)
	}
	tarFile.Close()
	if err := c.Checkpoint(conf, imgs.checkpoint, sandbox.CheckpointOpts{}); err != nil {
		p.t.Fatalf("Checkpoint: %v", err)
	}
	c.Wait()
	cleanup()
	return imgs
}

// populateMethods are the ways to populate an upper from a restore or tar
// archive.
var populateMethods = []string{"restore", "fs-restore", "tar"}

// populate creates a container that sleeps from imgs with the given method,
// and starts or restores it.
func (p *journalingUpperTest) populate(conf *config.Config, method string, imgs journalingUpperImages) (*Container, func(), error) {
	p.t.Helper()
	conf.AllowRootfsTarAnnotation = true
	var (
		mutateSpec func(*specs.Spec)
		mutateArgs func(*Args)
	)
	switch method {
	case "restore":
		mutateArgs = func(args *Args) { args.CheckpointDirPath = imgs.checkpoint }
	case "fs-restore":
		mutateArgs = func(args *Args) { args.FSRestoreImagePaths = []string{imgs.fsCheckpoint} }
	case "tar":
		mutateSpec = func(spec *specs.Spec) {
			if spec.Annotations == nil {
				spec.Annotations = make(map[string]string)
			}
			spec.Annotations[specutils.AnnotationRootfsUpperTar] = imgs.tar
		}
	default:
		p.t.Fatalf("unknown method %q", method)
	}
	c, cleanup, err := p.createWith(conf, mutateSpec, mutateArgs, "sleep", "1000")
	if err != nil {
		return nil, nil, err
	}
	if method == "restore" {
		err = c.Restore(conf, imgs.checkpoint, sandbox.DirectIOOff, false /* background */, nil /* networkArgs */)
	} else {
		err = c.Start(conf)
	}
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return c, cleanup, nil
}

// waitActive waits until the journal is no longer marked incomplete.
func (p *journalingUpperTest) waitActive() {
	p.t.Helper()
	f, err := os.Open(p.prefix + ".journal")
	if err != nil {
		p.t.Fatalf("opening journal: %v", err)
	}
	defer f.Close()
	deadline := time.Now().Add(pollTimeout)
	for {
		incomplete, err := journal.Incomplete(f)
		if err != nil {
			p.t.Fatalf("journal.Incomplete: %v", err)
		}
		if !incomplete {
			return
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("journal is still incomplete after %v", pollTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// expectStartError checks that a plain start fails with an error that
// contains want.
func (p *journalingUpperTest) expectStartError(conf *config.Config, want string) {
	p.t.Helper()
	c, cleanup, err := p.create(conf, "sleep", "1000")
	if err == nil {
		err = c.Start(conf)
		cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		p.t.Fatalf("starting the sandbox: got error %v, want %q", err, want)
	}
}

// expectPopulateRefusal checks that populating p's upper with method fails
// because its journal already exists, and that the attempt doesn't touch
// PREFIX.*.
func (p *journalingUpperTest) expectPopulateRefusal(method string, imgs journalingUpperImages) {
	p.t.Helper()
	before := p.prefixState()
	_, cleanup, err := p.populate(p.conf("image-1"), method, imgs)
	if err == nil {
		cleanup()
		p.t.Fatalf("%s onto an existing journal succeeded", method)
	}
	if want := "can't restore onto an existing journal at " + p.prefix + ".journal"; !strings.Contains(err.Error(), want) {
		p.t.Errorf("%s onto an existing journal: got error %v, want %q", method, err, want)
	}
	p.checkPrefixState("after the refused "+method, before)
	p.checkNoTempJournal()
}

// checkNoTempJournal checks that no temporary journal file is left.
func (p *journalingUpperTest) checkNoTempJournal() {
	p.t.Helper()
	matches, err := filepath.Glob(p.prefix + ".journal.tmp.*")
	if err != nil {
		p.t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		p.t.Errorf("temporary journal files left: %v", matches)
	}
}

// inode returns the inode number of the file at path.
func inode(t *testing.T, path string) uint64 {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	return stat.Ino
}

// TestJournalingRootfsUpperCreateJournal checks the creation of the journal
// of a new journaling upper that a restore or tar archive populates.
func TestJournalingRootfsUpperCreateJournal(t *testing.T) {
	p := newJournalingUpperTest(t)
	journalPath := p.prefix + ".journal"

	f, err := createJournalingRootfsUpperJournal(p.prefix, journal.Options{ImageID: "image-1"})
	if err != nil {
		t.Fatalf("createJournalingRootfsUpperJournal: %v", err)
	}
	defer f.Close()
	p.checkNoTempJournal()
	if incomplete, err := journal.Incomplete(f); err != nil || !incomplete {
		t.Errorf("journal.Incomplete: %t, %v; want true", incomplete, err)
	}
	// The returned file is PREFIX.journal, and it is locked.
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		t.Fatalf("Fstat: %v", err)
	}
	if ino := inode(t, journalPath); ino != stat.Ino {
		t.Errorf("returned file has inode %d, journal has inode %d", stat.Ino, ino)
	}
	other, err := os.Open(journalPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer other.Close()
	if err := unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != unix.EWOULDBLOCK {
		t.Errorf("Flock of the new journal: got %v, want EWOULDBLOCK", err)
	}

	// An existing journal is refused, whether locked or unlocked.
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	origIno := inode(t, journalPath)
	if _, err := createJournalingRootfsUpperJournal(p.prefix, journal.Options{ImageID: "image-1"}); err == nil || !strings.Contains(err.Error(), "can't restore onto an existing journal") {
		t.Errorf("createJournalingRootfsUpperJournal with a locked journal: got %v, want a refusal", err)
	}
	f.Close()
	if _, err := createJournalingRootfsUpperJournal(p.prefix, journal.Options{ImageID: "image-1"}); err == nil || !strings.Contains(err.Error(), "can't restore onto an existing journal") {
		t.Errorf("createJournalingRootfsUpperJournal with an existing journal: got %v, want a refusal", err)
	}
	p.checkNoTempJournal()
	if after, err := os.ReadFile(journalPath); err != nil || !bytes.Equal(after, before) || inode(t, journalPath) != origIno {
		t.Errorf("existing journal changed: %v", err)
	}
}

// TestJournalingRootfsUpperPopulate checks that a restore, a filesystem
// restore and a rootfs upper tar populate a new journaling upper and start a
// new journal, and that they fail if a journal file already exists.
func TestJournalingRootfsUpperPopulate(t *testing.T) {
	src := newJournalingUpperTest(t)
	imgs := src.makeImages()

	for _, method := range populateMethods {
		t.Run(method, func(t *testing.T) {
			// An existing journal is refused.
			src.t = t
			src.expectPopulateRefusal(method, imgs)

			// A new journaling upper starts from the image.
			p := newJournalingUpperTest(t)
			conf := p.conf("image-1")
			c1, cleanup1, err := p.populate(conf, method, imgs)
			if err != nil {
				t.Fatalf("%s: %v", method, err)
			}
			p.waitActive()
			p.checkNoTempJournal()
			if got, want := p.files(conf, c1, "a"), "A\n"; got != want {
				t.Errorf("after %s: got %q, want %q", method, got, want)
			}
			p.sh(conf, c1, "echo B > /journaled/b")
			// Refused while a sandbox is running with the journal too.
			p.expectPopulateRefusal(method, imgs)
			p.kill(c1, cleanup1)

			c2, cleanup2 := p.start(conf)
			if got, want := p.files(conf, c2, "a", "b"), "A\nB\n"; got != want {
				t.Errorf("after crash and restart: got %q, want %q", got, want)
			}
			cleanup2()

			// Now that its journal exists, the image can't populate it again.
			p.expectPopulateRefusal(method, imgs)
		})
	}

	// A restore into a sandbox that was created without the image is
	// refused, with or without the journaling upper at restore time.
	p := src
	p.t = t
	conf := p.conf("image-1")
	for _, restoreConf := range []*config.Config{conf, p.plainConf()} {
		c, cleanup, err := p.create(conf, "sleep", "1000")
		if err != nil {
			t.Fatalf("creating container: %v", err)
		}
		// Creating the sandbox may shrink the filestore.
		created := p.prefixState()
		if err := c.Restore(restoreConf, imgs.checkpoint, sandbox.DirectIOOff, false /* background */, nil /* networkArgs */); err == nil || !strings.Contains(err.Error(), "can't restore onto an existing journal") {
			t.Errorf("Restore into a sandbox that was created without the image: got error %v, want a refusal", err)
		}
		cleanup()
		p.checkPrefixState("after the refused restore into a sandbox", created)
	}
}

// expectKilled waits for c to exit, and checks that the sandbox was killed.
func (p *journalingUpperTest) expectKilled(c *Container) {
	p.t.Helper()
	ws, err := c.Wait()
	if err != nil {
		p.t.Fatalf("Wait: %v", err)
	}
	if !ws.Signaled() && ws.ExitStatus() == 0 {
		p.t.Fatalf("sandbox exited with status %v, want a failure", ws)
	}
}

// TestJournalingRootfsUpperActivationFailure checks that a failure to
// activate the journal after a population kills the sandbox, that later
// starts and populations fail, and that a population succeeds after the
// journaling upper's files are deleted.
func TestJournalingRootfsUpperActivationFailure(t *testing.T) {
	src := newJournalingUpperTest(t)
	imgs := src.makeImages()

	for _, method := range populateMethods {
		t.Run(method, func(t *testing.T) {
			p := newJournalingUpperTest(t)
			conf := p.conf("image-1")
			failConf := p.conf("image-1")
			failConf.TestOnlyJournalingRootfsUpperFault = "activation"
			c, cleanup, err := p.populate(failConf, method, imgs)
			if method == "tar" {
				// The tar import completes before the container starts,
				// so the activation fails the start.
				if err == nil || !strings.Contains(err.Error(), "journaling rootfs upper") {
					if err == nil {
						cleanup()
					}
					t.Fatalf("tar import with a failing activation: got error %v, want a journaling rootfs upper error", err)
				}
			} else {
				if err != nil {
					t.Fatalf("%s: %v", method, err)
				}
				p.expectKilled(c)
				cleanup()
			}

			p.expectStartError(conf, "journal at "+p.prefix+".journal is incomplete")
			p.expectPopulateRefusal(method, imgs)

			for _, suffix := range []string{".filestore", ".journal"} {
				if err := os.Remove(p.prefix + suffix); err != nil {
					t.Fatalf("Remove: %v", err)
				}
			}
			c2, cleanup2, err := p.populate(conf, method, imgs)
			if err != nil {
				t.Fatalf("%s after deleting the journaling upper: %v", method, err)
			}
			p.waitActive()
			p.checkNoTempJournal()
			if got, want := p.files(conf, c2, "a"), "A\n"; got != want {
				t.Errorf("after %s: got %q, want %q", method, got, want)
			}
			cleanup2()
		})
	}
}

// TestJournalingRootfsUpperSaveResume checks that a checkpoint with resume
// leaves the journal of a journaling upper valid.
func TestJournalingRootfsUpperSaveResume(t *testing.T) {
	p := newJournalingUpperTest(t)
	conf := p.conf("image-1")
	dir := t.TempDir()

	c1, cleanup1 := p.start(conf)
	p.sh(conf, c1, "mkdir -p /journaled && echo A > /journaled/a && echo B > /journaled/b")
	if err := c1.Checkpoint(conf, dir, sandbox.CheckpointOpts{Resume: true}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	p.sh(conf, c1, "echo C > /journaled/c && rm /journaled/b")
	p.kill(c1, cleanup1)

	c2, cleanup2 := p.start(conf)
	if got, want := p.files(conf, c2, "a", "b", "c"), "A\n-\nC\n"; got != want {
		t.Errorf("after checkpoint with resume, crash and restart: got %q, want %q", got, want)
	}
	cleanup2()
}

// TestJournalingRootfsUpperLargeSnapshot restarts a sandbox whose upper's
// snapshot is much larger than the journal's minimum region size.
func TestJournalingRootfsUpperLargeSnapshot(t *testing.T) {
	p := newJournalingUpperTest(t)
	conf := p.conf("image-1")
	conf.TestOnlyJournalingRootfsUpperRegionSize = 16 << 10
	const n = 500

	c1, cleanup1 := p.start(conf)
	p.sh(conf, c1, fmt.Sprintf(`
mkdir -p /journaled/many
i=0
while [ $i -lt %d ]; do echo $i > /journaled/many/file-with-a-long-name-$i; i=$((i+1)); done
`, n))
	p.kill(c1, cleanup1)

	check := func(what string, c *Container) {
		t.Helper()
		got := p.sh(conf, c, "ls /journaled/many | wc -l; cat /journaled/many/file-with-a-long-name-123")
		if want := fmt.Sprintf("%d\n123\n", n); strings.TrimLeft(got, " ") != want {
			t.Errorf("%s: got %q, want %q", what, got, want)
		}
	}
	// Each start compacts the journal after replay.
	c2, cleanup2 := p.start(conf)
	check("after crash and restart", c2)
	p.sh(conf, c2, "echo new > /journaled/new")
	p.kill(c2, cleanup2)

	c3, cleanup3 := p.start(conf)
	check("after a second crash and restart", c3)
	if got, want := p.files(conf, c3, "new"), "new\n"; got != want {
		t.Errorf("after a second crash and restart: got %q, want %q", got, want)
	}
	cleanup3()
	fi, err := os.Stat(p.prefix + ".journal")
	if err != nil {
		t.Fatalf("stat journal: %v", err)
	}
	if fi.Size() <= 4096+2*(16<<10) {
		t.Errorf("journal size is %d bytes, want a relocated region", fi.Size())
	}
}

// multiUpperTest runs sandboxes with a root container and a subcontainer,
// named "root" and "sub", whose rootfs uppers can be journaled.
type multiUpperTest struct {
	t    *testing.T
	conf *config.Config
	// prefixes holds the journaling rootfs upper prefix of each container,
	// or "" if it isn't journaled.
	prefixes [2]string
	// imageIDs holds the image ID of each container.
	imageIDs [2]string
	// rootViaFlag selects the root container's journaling rootfs upper with
	// --journaling-rootfs-upper instead of an annotation.
	rootViaFlag bool
}

var multiUpperNames = [2]string{"root", "sub"}

func newMultiUpperTest(t *testing.T) *multiUpperTest {
	dir := t.TempDir()
	conf := testutil.TestConfig(t)
	if err := conf.Overlay2.Set("root:dir=" + t.TempDir()); err != nil {
		t.Fatalf("Overlay2.Set: %v", err)
	}
	rootDir, cleanup, err := testutil.SetupRootDir()
	if err != nil {
		t.Fatalf("SetupRootDir: %v", err)
	}
	t.Cleanup(cleanup)
	conf.RootDir = rootDir
	return &multiUpperTest{
		t:        t,
		conf:     conf,
		prefixes: [2]string{filepath.Join(dir, "root"), filepath.Join(dir, "sub")},
		imageIDs: [2]string{"image-1", "image-1"},
	}
}

// specs returns the specs and IDs of the two containers.
func (m *multiUpperTest) specs() ([]*specs.Spec, []string, *config.Config) {
	conf := *m.conf
	sps, ids := createSpecs(sleepCmd, sleepCmd)
	for i, spec := range sps {
		spec.Root = &specs.Root{Path: "/", Readonly: false}
		name := multiUpperNames[i]
		spec.Annotations[specutils.ContainerdContainerNameAnnotation] = name
		if m.prefixes[i] == "" {
			continue
		}
		if i == 0 && m.rootViaFlag {
			conf.JournalingRootfsUpper = m.prefixes[i]
			conf.JournalingRootfsUpperImageID = m.imageIDs[i]
			continue
		}
		spec.Annotations[specutils.AnnotationRootfsUpperJournal+"."+name] = m.prefixes[i]
		spec.Annotations[specutils.AnnotationRootfsUpperJournalImageID+"."+name] = m.imageIDs[i]
	}
	return sps, ids, &conf
}

// start starts both containers.
func (m *multiUpperTest) start() ([]*Container, func(), *config.Config) {
	m.t.Helper()
	specs, ids, conf := m.specs()
	cs, cleanup, err := startContainers(conf, specs, ids)
	if err != nil {
		m.t.Fatalf("starting containers: %v", err)
	}
	return cs, func() {
		cleanup()
		m.waitUnlocked()
	}, conf
}

// waitUnlocked waits until no sandbox holds the containers' journal locks.
func (m *multiUpperTest) waitUnlocked() {
	m.t.Helper()
	for _, prefix := range m.prefixes {
		if prefix == "" {
			continue
		}
		if _, err := os.Stat(prefix + ".journal"); err != nil {
			continue
		}
		(&journalingUpperTest{t: m.t, prefix: prefix}).waitUnlocked()
	}
}

// kill SIGKILLs the sandbox of cs, then calls cleanup.
func (m *multiUpperTest) kill(cs []*Container, cleanup func()) {
	m.t.Helper()
	if err := unix.Kill(cs[0].SandboxPid(), unix.SIGKILL); err != nil {
		m.t.Fatalf("killing sandbox: %v", err)
	}
	cs[0].Wait()
	cleanup()
}

func (m *multiUpperTest) sh(conf *config.Config, c *Container, script string) string {
	m.t.Helper()
	out, err := executeCombinedOutput(conf, c, nil, "/bin/sh", "-c", script)
	if err != nil {
		m.t.Fatalf("script %q failed: %v, output: %s", script, err, out)
	}
	return string(out)
}

// files returns the contents of the given files in /journaled, or "-" for
// missing files.
func (m *multiUpperTest) files(conf *config.Config, c *Container, names ...string) string {
	m.t.Helper()
	script := ""
	for _, name := range names {
		script += fmt.Sprintf("cat /journaled/%s 2>/dev/null || echo -; ", name)
	}
	return m.sh(conf, c, script)
}

// TestJournalingRootfsUpperMultiContainer checks that the rootfs uppers of a
// root container and a subcontainer are journaled independently.
func TestJournalingRootfsUpperMultiContainer(t *testing.T) {
	for _, rootViaFlag := range []bool{false, true} {
		t.Run(fmt.Sprintf("rootViaFlag=%t", rootViaFlag), func(t *testing.T) {
			m := newMultiUpperTest(t)
			m.rootViaFlag = rootViaFlag

			cs, cleanup, conf := m.start()
			m.sh(conf, cs[0], "mkdir -p /journaled && echo R > /journaled/root")
			m.sh(conf, cs[1], "mkdir -p /journaled && echo S > /journaled/sub")
			m.kill(cs, cleanup)

			check := func(what string, conf *config.Config, cs []*Container, want0, want1 string) {
				t.Helper()
				if got := m.files(conf, cs[0], "root", "sub", "more"); got != want0 {
					t.Errorf("%s: root container files: got %q, want %q", what, got, want0)
				}
				if got := m.files(conf, cs[1], "root", "sub", "more"); got != want1 {
					t.Errorf("%s: subcontainer files: got %q, want %q", what, got, want1)
				}
			}
			cs, cleanup, conf = m.start()
			check("after crash and restart", conf, cs, "R\n-\n-\n", "-\nS\n-\n")
			m.sh(conf, cs[1], "echo M > /journaled/more")
			cleanup()

			cs, cleanup, conf = m.start()
			check("after clean restart", conf, cs, "R\n-\n-\n", "-\nS\nM\n")
			cleanup()
		})
	}
}

// TestJournalingRootfsUpperSubcontainerNotJournaled checks that a subcontainer
// without the annotation is not journaled, and doesn't touch the root
// container's files.
func TestJournalingRootfsUpperSubcontainerNotJournaled(t *testing.T) {
	m := newMultiUpperTest(t)
	m.prefixes[1] = ""
	specs, ids, conf := m.specs()
	root, cleanupRoot, err := startContainers(conf, specs[:1], ids[:1])
	if err != nil {
		t.Fatalf("starting root container: %v", err)
	}
	m.sh(conf, root[0], "mkdir -p /journaled && echo R > /journaled/root")
	p := &journalingUpperTest{t: t, prefix: m.prefixes[0]}
	before := p.prefixState()
	sub, cleanupSub, err := startContainers(conf, specs[1:], ids[1:])
	if err != nil {
		t.Fatalf("starting subcontainer: %v", err)
	}
	m.sh(conf, sub[0], "mkdir -p /journaled && echo S > /journaled/sub")
	p.checkPrefixState("after the subcontainer wrote", before)
	cleanupSub()
	cleanupRoot()
	m.waitUnlocked()

	cs, cleanup, conf := m.start()
	if got, want := m.files(conf, cs[0], "root", "sub"), "R\n-\n"; got != want {
		t.Errorf("root container files after restart: got %q, want %q", got, want)
	}
	if got, want := m.files(conf, cs[1], "root", "sub"), "-\n-\n"; got != want {
		t.Errorf("subcontainer files after restart: got %q, want %q", got, want)
	}
	cleanup()
}

// TestJournalingRootfsUpperSamePrefix checks that two containers can't use
// the same journaling rootfs upper.
func TestJournalingRootfsUpperSamePrefix(t *testing.T) {
	m := newMultiUpperTest(t)
	m.prefixes[1] = m.prefixes[0]
	specs, ids, conf := m.specs()
	root, cleanupRoot, err := startContainers(conf, specs[:1], ids[:1])
	if err != nil {
		t.Fatalf("starting root container: %v", err)
	}
	defer cleanupRoot()
	m.sh(conf, root[0], "mkdir -p /journaled && echo R > /journaled/root")
	p := &journalingUpperTest{t: t, prefix: m.prefixes[0]}
	before := p.prefixState()
	if _, cleanupSub, err := startContainers(conf, specs[1:], ids[1:]); err == nil {
		cleanupSub()
		t.Fatalf("starting a subcontainer with the root container's prefix succeeded")
	} else if !strings.Contains(err.Error(), "in use by another sandbox") {
		t.Errorf("starting a subcontainer with the root container's prefix: got %v, want an in use error", err)
	}
	p.checkPrefixState("after the refused subcontainer", before)
}

// TestJournalingRootfsUpperSubcontainerImageMismatch checks that a
// subcontainer refuses a journal created for another image.
func TestJournalingRootfsUpperSubcontainerImageMismatch(t *testing.T) {
	m := newMultiUpperTest(t)
	cs, cleanup, conf := m.start()
	m.sh(conf, cs[1], "mkdir -p /journaled && echo S > /journaled/sub")
	cleanup()

	m.imageIDs[1] = "image-2"
	specs, ids, conf := m.specs()
	if _, cleanup, err := startContainers(conf, specs, ids); err == nil {
		cleanup()
		t.Fatalf("starting a subcontainer with another image ID succeeded")
	} else if !strings.Contains(err.Error(), `journal was created for image "image-1"`) {
		t.Errorf("starting a subcontainer with another image ID: got %v, want an image mismatch", err)
	}
	m.waitUnlocked()

	// The refusal did not damage the upper.
	m.imageIDs[1] = "image-1"
	cs, cleanup, conf = m.start()
	if got, want := m.files(conf, cs[1], "sub"), "S\n"; got != want {
		t.Errorf("subcontainer files: got %q, want %q", got, want)
	}
	cleanup()
}

// TestJournalingRootfsUpperMultiContainerCheckpoint checks that a restore of
// a multi-container sandbox populates a new journaling rootfs upper for each
// container, and refuses existing journals.
func TestJournalingRootfsUpperMultiContainerCheckpoint(t *testing.T) {
	src := newMultiUpperTest(t)
	dir := t.TempDir()
	cs, cleanup, conf := src.start()
	src.sh(conf, cs[0], "mkdir -p /journaled && echo R > /journaled/root")
	src.sh(conf, cs[1], "mkdir -p /journaled && echo S > /journaled/sub")
	if err := cs[0].Checkpoint(conf, dir, sandbox.CheckpointOpts{}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	cs[0].Wait()
	cleanup()

	// Restoring onto the existing journals is refused.
	specs, ids, conf := src.specs()
	if _, cleanupRestore, err := restoreContainers(conf, specs, ids, dir); err == nil {
		cleanupRestore()
		t.Fatalf("restoring containers onto existing journals succeeded")
	} else if !strings.Contains(err.Error(), "can't restore onto an existing journal") {
		t.Errorf("restoring containers onto existing journals: got %v, want a refusal", err)
	}

	// Restoring onto new prefixes starts a new journal for each container.
	m := newMultiUpperTest(t)
	specs, ids, conf = m.specs()
	cs, cleanupRestore, err := restoreContainers(conf, specs, ids, dir)
	if err != nil {
		t.Fatalf("restoring containers: %v", err)
	}
	cleanup = func() {
		cleanupRestore()
		m.waitUnlocked()
	}
	for i, prefix := range m.prefixes {
		(&journalingUpperTest{t: t, prefix: prefix}).waitActive()
		if got, want := m.files(conf, cs[i], "root", "sub"), [2]string{"R\n-\n", "-\nS\n"}[i]; got != want {
			t.Errorf("container %d files after restore: got %q, want %q", i, got, want)
		}
	}
	m.sh(conf, cs[0], "echo R2 > /journaled/root2")
	m.sh(conf, cs[1], "echo S2 > /journaled/sub2")
	m.kill(cs, cleanup)

	cs, cleanup, conf = m.start()
	if got, want := m.files(conf, cs[0], "root", "root2", "sub", "sub2"), "R\nR2\n-\n-\n"; got != want {
		t.Errorf("root container files after crash and restart: got %q, want %q", got, want)
	}
	if got, want := m.files(conf, cs[1], "root", "root2", "sub", "sub2"), "-\n-\nS\nS2\n"; got != want {
		t.Errorf("subcontainer files after crash and restart: got %q, want %q", got, want)
	}
	cleanup()
}

// TestJournalingRootfsUpperOverlayMedium checks that journaling works with
// every rootfs overlay medium, which it ignores.
func TestJournalingRootfsUpperOverlayMedium(t *testing.T) {
	for _, medium := range []string{"memory", "self", "dir"} {
		t.Run(medium, func(t *testing.T) {
			p := newJournalingUpperTest(t)
			conf := p.conf("image-1")
			overlay := "root:" + medium
			if medium == "dir" {
				overlay = "root:dir=" + p.overlayDir
			}
			if err := conf.Overlay2.Set(overlay); err != nil {
				t.Fatalf("Overlay2.Set: %v", err)
			}

			c1, cleanup1 := p.start(conf)
			p.sh(conf, c1, "mkdir -p /journaled && echo A > /journaled/a")
			p.kill(c1, cleanup1)

			c2, cleanup2 := p.start(conf)
			if got, want := p.files(conf, c2, "a"), "A\n"; got != want {
				t.Errorf("after crash and restart: got %q, want %q", got, want)
			}
			p.sh(conf, c2, "echo B > /journaled/b")
			cleanup2()

			c3, cleanup3 := p.start(conf)
			if got, want := p.files(conf, c3, "a", "b"), "A\nB\n"; got != want {
				t.Errorf("after clean restart: got %q, want %q", got, want)
			}
			cleanup3()

			// A journaled upper never uses a self-backed filestore in the
			// rootfs, and the overlay directory stays empty.
			if matches, _ := filepath.Glob("/.gvisor.filestore.*"); len(matches) != 0 {
				t.Errorf("self filestores in the rootfs: %v", matches)
			}
			if entries, err := os.ReadDir(p.overlayDir); err != nil || len(entries) != 0 {
				t.Errorf("overlay directory entries: %v, %v", entries, err)
			}
		})
	}
}

// TestJournalingRootfsUpperNoOverlay checks that journaling requires a rootfs
// overlay.
func TestJournalingRootfsUpperNoOverlay(t *testing.T) {
	p := newJournalingUpperTest(t)
	conf := p.conf("image-1")
	if err := conf.Overlay2.Set("none"); err != nil {
		t.Fatalf("Overlay2.Set: %v", err)
	}
	c, cleanup, err := p.create(conf, "sleep", "1000")
	if err == nil {
		err = c.Start(conf)
		cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "--journaling-rootfs-upper requires a rootfs overlay") {
		t.Fatalf("starting without a rootfs overlay: got %v, want a rootfs overlay error", err)
	}
	for _, suffix := range []string{".filestore", ".journal"} {
		if _, err := os.Stat(p.prefix + suffix); err == nil {
			t.Errorf("%s exists after the refused start", suffix)
		}
	}
}

// TestJournalingRootfsUpperFailedSetupCleanup checks that failing before the
// sandbox starts (for example in openJournalingRootfsUpperFilestore or
// sandbox.New) removes a newly created journal without leaking a lock, while
// preserving a pre-existing journal.
func TestJournalingRootfsUpperFailedSetupCleanup(t *testing.T) {
	src := newJournalingUpperTest(t)
	imgs := src.makeImages()

	t.Run("fresh-start-filestore-error", func(t *testing.T) {
		p := newJournalingUpperTest(t)
		conf := p.conf("image-1")
		// Make <prefix>.filestore a directory so openJournalingRootfsUpperFilestore fails.
		if err := os.Mkdir(p.prefix+".filestore", 0700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if c, cleanup, err := p.create(conf, "sleep", "1000"); err == nil {
			cleanup()
			_ = c
			t.Fatalf("creating container with directory filestore succeeded")
		}
		if _, err := os.Stat(p.prefix + ".journal"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("journal exists after failed fresh setup: err=%v", err)
		}
		if err := os.Remove(p.prefix + ".filestore"); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		// A populate on the same prefix must succeed now because no stray
		// journal was left behind.
		c, cleanup, err := p.populate(conf, "tar", imgs)
		if err != nil {
			t.Fatalf("populate after cleanup: %v", err)
		}
		p.waitActive()
		if got, want := p.files(conf, c, "a"), "A\n"; got != want {
			t.Errorf("files after populate: got %q, want %q", got, want)
		}
		cleanup()
	})

	t.Run("populate-filestore-and-early-sandbox-error", func(t *testing.T) {
		p := newJournalingUpperTest(t)
		conf := p.conf("image-1")

		// 1. Fail in openJournalingRootfsUpperFilestore during populate.
		if err := os.Mkdir(p.prefix+".filestore", 0700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if _, cleanup, err := p.populate(conf, "restore", imgs); err == nil {
			cleanup()
			t.Fatalf("populate with directory filestore succeeded")
		}
		if _, err := os.Stat(p.prefix + ".journal"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("journal exists after failed populate filestore setup: err=%v", err)
		}
		if err := os.Remove(p.prefix + ".filestore"); err != nil {
			t.Fatalf("Remove: %v", err)
		}

		// 2. Fail early in sandbox.New (PodInitConfig) before Donate runs:
		// verifies neither the journal file nor the flock/FD is leaked.
		badConf := p.conf("image-1")
		badConf.PodInitConfig = filepath.Join(t.TempDir(), "missing-pod-init.json")
		if _, cleanup, err := p.populate(badConf, "restore", imgs); err == nil {
			cleanup()
			t.Fatalf("populate with missing PodInitConfig succeeded")
		}
		if _, err := os.Stat(p.prefix + ".journal"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("journal exists after failed sandbox.New: err=%v", err)
		}

		// 3. Retry populate on the same prefix; it must succeed cleanly.
		c, cleanup, err := p.populate(conf, "restore", imgs)
		if err != nil {
			t.Fatalf("populate after failed attempts: %v", err)
		}
		p.waitActive()
		if got, want := p.files(conf, c, "a"), "A\n"; got != want {
			t.Errorf("files after restore: got %q, want %q", got, want)
		}
		cleanup()
	})

	t.Run("replay-preserves-existing-journal", func(t *testing.T) {
		p := newJournalingUpperTest(t)
		conf := p.conf("image-1")
		c1, cleanup1 := p.start(conf)
		p.sh(conf, c1, "mkdir -p /journaled && echo KEPT > /journaled/kept")
		cleanup1()

		// Temporarily replace <prefix>.filestore with a symlink so
		// openJournalingRootfsUpperFilestore fails after opening the
		// pre-existing journal.
		savedFilestore := p.prefix + ".filestore.bak"
		if err := os.Rename(p.prefix+".filestore", savedFilestore); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		if err := os.Symlink(savedFilestore, p.prefix+".filestore"); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		before := p.prefixState()
		if _, cleanup, err := p.create(conf, "sleep", "1000"); err == nil {
			cleanup()
			t.Fatalf("creating container with symlink filestore succeeded")
		}
		p.checkPrefixState("after failed replay setup", before)

		if err := os.Remove(p.prefix + ".filestore"); err != nil {
			t.Fatalf("Remove symlink: %v", err)
		}
		if err := os.Rename(savedFilestore, p.prefix+".filestore"); err != nil {
			t.Fatalf("Rename restore: %v", err)
		}
		c2, cleanup2 := p.start(conf)
		if got, want := p.files(conf, c2, "kept"), "KEPT\n"; got != want {
			t.Errorf("files after replay: got %q, want %q", got, want)
		}
		cleanup2()
	})
}
