package lumberjack

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testSidecars returns a checksum and an index sidecar, in sync order.
func testSidecars() []Sidecar {
	return []Sidecar{
		{Suffix: ".sum", Kind: SidecarChecksum},
		{Suffix: ".idx", Kind: SidecarIndex},
	}
}

func checksumLine(b []byte) string {
	return fmt.Sprintf("%x %d\n", sha256.Sum256(b), len(b))
}

func gunzip(t *testing.T, path string) []byte {
	f, err := os.Open(path)
	isNil(err, t)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	isNil(err, t)
	b, err := ioutil.ReadAll(gz)
	isNil(err, t)
	return b
}

func TestSidecarRotation(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestSidecarRotation", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
		Sidecars: testSidecars(),
	}
	defer l.Close()

	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	// the whole current group exists
	existsWithContent(filename, b, t)
	existsWithContent(filename+".sum", []byte(checksumLine(b)), t)
	existsWithContent(filename+".idx", []byte("0 4\n"), t)

	newFakeTime()

	b2 := []byte("foooooo!")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	// the backup group shares one generation timestamp
	backup := backupFile(dir)
	existsWithContent(backup, b, t)
	existsWithContent(backup+".sum", []byte(checksumLine(b)), t)
	existsWithContent(backup+".idx", []byte("0 4\n"), t)

	// the new generation started fresh
	existsWithContent(filename, b2, t)
	existsWithContent(filename+".sum", []byte(checksumLine(b2)), t)
	existsWithContent(filename+".idx", []byte("0 8\n"), t)

	fileCount(dir, 6, t)
}

func TestSidecarChecksumContinuesExistingGeneration(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestSidecarChecksumContinuesExistingGeneration", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	data := []byte("foo!")
	isNil(ioutil.WriteFile(filename, data, 0644), t)

	l := &Logger{
		Filename: filename,
		MaxSize:  100,
		Sidecars: testSidecars(),
	}
	defer l.Close()

	b := []byte("boo!")
	_, err := l.Write(b)
	isNil(err, t)

	// the checksum covers the pre-existing content plus the new write
	existsWithContent(filename+".sum", []byte(checksumLine(append(data, b...))), t)
	// the index offset accounts for the pre-existing content
	existsWithContent(filename+".idx", []byte("4 4\n"), t)
}

func TestSidecarOpaque(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestSidecarOpaque", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
		Sidecars: []Sidecar{{Suffix: ".meta", Kind: SidecarOpaque}},
	}
	defer l.Close()

	b := []byte("boo!")
	_, err := l.Write(b)
	isNil(err, t)

	// lumberjack creates the opaque sidecar but doesn't write to it
	existsWithContent(filename+".meta", []byte{}, t)

	newFakeTime()
	b2 := []byte("foooooo!")
	_, err = l.Write(b2)
	isNil(err, t)

	// the opaque sidecar rotated along with the group
	exists(backupFile(dir)+".meta", t)
	fileCount(dir, 4, t)
}

func TestSidecarInvalidConfig(t *testing.T) {
	currentTime = fakeTime
	dir := makeTempDir("TestSidecarInvalidConfig", t)
	defer os.RemoveAll(dir)

	for _, sidecars := range [][]Sidecar{
		{{Suffix: "", Kind: SidecarChecksum}},
		{{Suffix: ".a"}, {Suffix: ".a"}},
		{{Suffix: ".x", Kind: "bogus"}},
		{{Suffix: "sub/dir.sum"}},
	} {
		l := &Logger{Filename: logFile(dir), Sidecars: sidecars}
		_, err := l.Write([]byte("boo!"))
		notNil(err, t)
		l.Close()
	}
}

func TestWriteContextCancel(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestWriteContextCancel", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
	}
	defer l.Close()

	ctx := context.Background()
	b := []byte("boo!")
	n, err := l.WriteContext(ctx, b)
	isNil(err, t)
	equals(len(b), n, t)

	// cancel the context: new writes are rejected...
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err = l.WriteContext(cancelCtx, []byte("more"))
	equals(0, n, t)
	equals(context.Canceled, err, t)

	// ...and the bytes accepted before cancellation are not truncated
	existsWithContent(filename, b, t)
	fileCount(dir, 1, t)
}

func TestWriteContextCancelBeforeCreate(t *testing.T) {
	currentTime = fakeTime
	dir := makeTempDir("TestWriteContextCancelBeforeCreate", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{Filename: filename}
	defer l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := l.WriteContext(ctx, []byte("boo!"))
	equals(0, n, t)
	equals(context.Canceled, err, t)
	notExist(filename, t)
}

func TestConcurrentWriteSidecars(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestConcurrentWriteSidecars", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10 * 1024 * 1024, // no rotation
		Sidecars: testSidecars(),
	}
	defer l.Close()

	ctx := context.Background()
	var wg sync.WaitGroup
	total := 0
	var mu sync.Mutex
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				b := []byte(fmt.Sprintf("goroutine-%d-write-%d\n", i, j))
				n, err := l.WriteContext(ctx, b)
				if err == nil {
					mu.Lock()
					total += n
					mu.Unlock()
				}
			}
		}(i)
	}
	wg.Wait()
	l.Close()

	// every accepted write landed in full: the main log size matches the
	// accepted byte count, the checksum matches the final content, and the
	// index has exactly one line per write
	content, err := ioutil.ReadFile(filename)
	isNil(err, t)
	equals(total, len(content), t)

	sum, err := ioutil.ReadFile(filename + ".sum")
	isNil(err, t)
	equals(checksumLine(content), string(sum), t)

	idx, err := ioutil.ReadFile(filename + ".idx")
	isNil(err, t)
	equals(100, len(strings.Split(strings.TrimSpace(string(idx)), "\n")), t)
}

func TestRecoveryCompletesInterruptedRotation(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestRecoveryCompletesInterruptedRotation", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	// simulate a crash mid-rotation: every current file was renamed aside,
	// but none reached its backup name
	isNil(ioutil.WriteFile(filename+rotateTmpSuffix, []byte("main"), 0644), t)
	isNil(ioutil.WriteFile(filename+".sum"+rotateTmpSuffix, []byte("sum"), 0644), t)
	isNil(ioutil.WriteFile(filename+".idx"+rotateTmpSuffix, []byte("idx"), 0644), t)

	l := &Logger{
		Filename: filename,
		MaxSize:  10,
		Sidecars: testSidecars(),
	}
	defer l.Close()

	b := []byte("boo!")
	_, err := l.Write(b)
	isNil(err, t)

	// the interrupted rotation was completed: all members share one
	// generation timestamp
	backup := backupFile(dir)
	existsWithContent(backup, []byte("main"), t)
	existsWithContent(backup+".sum", []byte("sum"), t)
	existsWithContent(backup+".idx", []byte("idx"), t)

	// new writes go to a fresh generation, not the old one
	existsWithContent(filename, b, t)
	fileCount(dir, 6, t)
}

func TestRecoveryCompletesWithCurrentMain(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestRecoveryCompletesWithCurrentMain", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	// simulate a crash early in rotation: the sidecars were renamed aside
	// but the main log was not
	isNil(ioutil.WriteFile(filename, []byte("main"), 0644), t)
	isNil(ioutil.WriteFile(filename+".sum"+rotateTmpSuffix, []byte("sum"), 0644), t)
	isNil(ioutil.WriteFile(filename+".idx"+rotateTmpSuffix, []byte("idx"), 0644), t)

	l := &Logger{
		Filename: filename,
		MaxSize:  10,
		Sidecars: testSidecars(),
	}
	defer l.Close()

	b := []byte("boo!")
	_, err := l.Write(b)
	isNil(err, t)

	// the leftover current main joined the same generation as its sidecars
	backup := backupFile(dir)
	existsWithContent(backup, []byte("main"), t)
	existsWithContent(backup+".sum", []byte("sum"), t)
	existsWithContent(backup+".idx", []byte("idx"), t)

	// and the new write did not append to the old generation
	existsWithContent(filename, b, t)
	fileCount(dir, 6, t)
}

func TestRecoveryQuarantinesUnknownTemp(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestRecoveryQuarantinesUnknownTemp", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	// a temp file that looks like ours but matches no configured member
	isNil(ioutil.WriteFile(filename+".bogus"+rotateTmpSuffix, []byte("bogus"), 0644), t)
	// a temp file belonging to a different logger in the same directory
	other := filepath.Join(dir, "other.log"+rotateTmpSuffix)
	isNil(ioutil.WriteFile(other, []byte("other"), 0644), t)

	l := &Logger{
		Filename: filename,
		MaxSize:  10,
		Sidecars: testSidecars(),
	}
	defer l.Close()

	b := []byte("boo!")
	_, err := l.Write(b)
	isNil(err, t)

	// the uncompletable temp was quarantined, not completed or deleted
	existsWithContent(filename+".bogus"+rotateTmpSuffix+quarantineSuffix, []byte("bogus"), t)
	// the other logger's temp was left alone
	existsWithContent(other, []byte("other"), t)

	existsWithContent(filename, b, t)
	// current group (3) + quarantined file + other temp
	fileCount(dir, 5, t)
}

func TestRecoveryQuarantinesConflictingTemp(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestRecoveryQuarantinesConflictingTemp", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	// both a current file and a renamed-aside copy of the main log exist;
	// recovery can't tell which is authoritative
	isNil(ioutil.WriteFile(filename, []byte("current"), 0644), t)
	isNil(ioutil.WriteFile(filename+rotateTmpSuffix, []byte("temp"), 0644), t)

	l := &Logger{
		Filename: filename,
		MaxSize:  100,
		Sidecars: testSidecars(),
	}
	defer l.Close()

	b := []byte("boo!")
	_, err := l.Write(b)
	isNil(err, t)

	// the ambiguous temp was quarantined and the current file kept
	existsWithContent(filename+rotateTmpSuffix+quarantineSuffix, []byte("temp"), t)
	existsWithContent(filename, append([]byte("current"), b...), t)
}

func TestRecoveryCompressLeftovers(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestRecoveryCompressLeftovers", t)
	defer os.RemoveAll(dir)

	// a partial compression product next to its complete source
	backup := backupFile(dir)
	isNil(ioutil.WriteFile(backup, []byte("data"), 0644), t)
	isNil(ioutil.WriteFile(backup+compressSuffix+compressTmpSuffix, []byte("partial"), 0644), t)

	// a member that exists both compressed and uncompressed
	newFakeTime()
	backup2 := backupFile(dir)
	isNil(ioutil.WriteFile(backup2, []byte("data2"), 0644), t)
	isNil(ioutil.WriteFile(backup2+compressSuffix, []byte("stale"), 0644), t)

	l := &Logger{
		Filename: logFile(dir),
		MaxSize:  10,
	}
	defer l.Close()

	_, err := l.Write([]byte("boo!"))
	isNil(err, t)

	// the partial temp is gone, the source kept
	notExist(backup+compressSuffix+compressTmpSuffix, t)
	existsWithContent(backup, []byte("data"), t)

	// the stale compressed duplicate is gone, the source kept
	notExist(backup2+compressSuffix, t)
	existsWithContent(backup2, []byte("data2"), t)
}

func TestMaxBackupsGroups(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestMaxBackupsGroups", t)
	defer os.RemoveAll(dir)

	// three generations of complete backup groups
	data := []byte("data")
	var backups []string
	for i := 0; i < 3; i++ {
		backup := backupFile(dir)
		isNil(ioutil.WriteFile(backup, data, 0644), t)
		isNil(ioutil.WriteFile(backup+".sum", data, 0644), t)
		isNil(ioutil.WriteFile(backup+".idx", data, 0644), t)
		backups = append(backups, backup)
		newFakeTime()
	}

	filename := logFile(dir)
	l := &Logger{
		Filename:   filename,
		MaxSize:    10,
		MaxBackups: 1,
		Sidecars:   testSidecars(),
	}
	defer l.Close()

	b2 := []byte("foooooo!")
	_, err := l.Write(b2)
	isNil(err, t)

	<-time.After(10 * time.Millisecond)

	// only the newest generation survives, as a complete group; no orphaned
	// sidecar members of the removed generations are left behind
	fileCount(dir, 6, t) // current group (3) + newest backup group (3)
	for _, gone := range backups[:2] {
		notExist(gone, t)
		notExist(gone+".sum", t)
		notExist(gone+".idx", t)
	}
	exists(backups[2], t)
	exists(backups[2]+".sum", t)
	exists(backups[2]+".idx", t)
}

func TestMaxAgeGroups(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestMaxAgeGroups", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
		MaxAge:   1,
		Sidecars: testSidecars(),
	}
	defer l.Close()

	b := []byte("boo!")
	_, err := l.Write(b)
	isNil(err, t)

	// two days later, the first generation rotates out; its timestamp is
	// the rotation time, so it is still young enough to keep
	newFakeTime()
	b2 := []byte("foooooo!")
	_, err = l.Write(b2)
	isNil(err, t)

	<-time.After(10 * time.Millisecond)
	fileCount(dir, 6, t)

	// two more days later: the first backup generation ages past the
	// cutoff and is removed as a whole, sidecars included
	newFakeTime()
	b3 := []byte("baaaaar!")
	_, err = l.Write(b3)
	isNil(err, t)

	<-time.After(10 * time.Millisecond)

	fileCount(dir, 6, t) // current group (3) + newest backup group (3)
	existsWithContent(filename, b3, t)
	backup := backupFile(dir)
	existsWithContent(backup, b2, t)
	exists(backup+".sum", t)
	exists(backup+".idx", t)
}

func TestCompressGroups(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestCompressGroups", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Compress: true,
		Filename: filename,
		MaxSize:  10,
		Sidecars: testSidecars(),
	}
	defer l.Close()

	b := []byte("boo!")
	_, err := l.Write(b)
	isNil(err, t)

	newFakeTime()
	b2 := []byte("foooooo!")
	_, err = l.Write(b2)
	isNil(err, t)

	<-time.After(10 * time.Millisecond)

	// every member of the rotated generation was compressed
	backup := backupFile(dir)
	equals(b, gunzip(t, backup+compressSuffix), t)
	equals([]byte(checksumLine(b)), gunzip(t, backup+".sum"+compressSuffix), t)
	equals([]byte("0 4\n"), gunzip(t, backup+".idx"+compressSuffix), t)

	// no uncompressed members or temp files remain
	notExist(backup, t)
	notExist(backup+".sum", t)
	notExist(backup+".idx", t)
	fileCount(dir, 6, t)
}

func TestCompressGroupFailureRestores(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestCompressGroupFailureRestores", t)
	defer os.RemoveAll(dir)

	// a complete backup generation
	backup := backupFile(dir)
	b := []byte("boo!")
	isNil(ioutil.WriteFile(backup, b, 0644), t)
	isNil(ioutil.WriteFile(backup+".sum", []byte("sum"), 0644), t)
	isNil(ioutil.WriteFile(backup+".idx", []byte("idx"), 0644), t)

	// make one member of the group unreadable so its compression fails
	// mid-group
	isNil(os.Chmod(backup+".idx", 0000), t)
	defer os.Chmod(backup+".idx", 0644)

	l := &Logger{
		Compress: true,
		Filename: logFile(dir),
		Sidecars: testSidecars(),
	}
	defer l.Close()

	err := l.millRunOnce()
	notNil(err, t)

	// the failure restored a consistent state: no compressed products, no
	// temp files, all original members still present
	existsWithContent(backup, b, t)
	exists(backup+".sum", t)
	notExist(backup+compressSuffix, t)
	notExist(backup+".sum"+compressSuffix, t)
	notExist(backup+".idx"+compressSuffix, t)
	notExist(backup+compressSuffix+compressTmpSuffix, t)
	notExist(backup+".sum"+compressSuffix+compressTmpSuffix, t)
}

func TestRotateRenameFailureRollsBack(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestRotateRenameFailureRollsBack", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
		Sidecars: testSidecars(),
	}
	defer l.Close()

	b := []byte("boo!")
	_, err := l.Write(b)
	isNil(err, t)

	// make the index sidecar's temporary name a directory, so renaming it
	// aside fails after the checksum sidecar was already renamed aside and
	// must be rolled back
	newFakeTime()
	isNil(os.Mkdir(filename+".idx"+rotateTmpSuffix, 0755), t)
	_, err = l.Write([]byte("foooooo!"))
	notNil(err, t)
	isNil(os.Remove(filename+".idx"+rotateTmpSuffix), t)

	// the group is back in its original, consistent state: all currents
	// present with their content, no temps, no partial backups
	existsWithContent(filename, b, t)
	existsWithContent(filename+".sum", []byte(checksumLine(b)), t)
	existsWithContent(filename+".idx", []byte("0 4\n"), t)
	fileCount(dir, 3, t)
}

func TestRotateCommitFailureRollsBack(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestRotateCommitFailureRollsBack", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
		Sidecars: testSidecars(),
	}
	defer l.Close()

	b := []byte("boo!")
	_, err := l.Write(b)
	isNil(err, t)

	// make the checksum sidecar's backup name a directory, so committing it
	// fails after the main log's backup was already committed and must be
	// rolled back too
	newFakeTime()
	isNil(os.Mkdir(backupFile(dir)+".sum", 0755), t)
	_, err = l.Write([]byte("foooooo!"))
	notNil(err, t)
	isNil(os.Remove(backupFile(dir)+".sum"), t)

	existsWithContent(filename, b, t)
	existsWithContent(filename+".sum", []byte(checksumLine(b)), t)
	existsWithContent(filename+".idx", []byte("0 4\n"), t)
	fileCount(dir, 3, t)
}

func TestGroupLoggingContinuesAfterFailedRotate(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1

	dir := makeTempDir("TestGroupLoggingContinuesAfterFailedRotate", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
		Sidecars: testSidecars(),
	}
	defer l.Close()

	b := []byte("boo!")
	_, err := l.Write(b)
	isNil(err, t)

	// one rotation fails because the log directory is read-only
	isNil(os.Chmod(dir, 0500), t)
	newFakeTime()
	_, err = l.Write([]byte("foooooo!"))
	notNil(err, t)
	isNil(os.Chmod(dir, 0700), t)

	// the next write recovers: the old generation (still the current files)
	// is rotated out and logging continues on a fresh group
	newFakeTime()
	b3 := []byte("baaaaar!")
	_, err = l.Write(b3)
	isNil(err, t)

	existsWithContent(filename, b3, t)
	existsWithContent(filename+".sum", []byte(checksumLine(b3)), t)
	existsWithContent(backupFile(dir), b, t)
}
