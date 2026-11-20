package lumberjack

import (
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Sidecar kinds selectable via Sidecar.Kind.
const (
	// SidecarChecksum maintains the hex encoded SHA-256 of the current
	// generation's log content and its size in bytes, as "<hash> <size>".
	SidecarChecksum = "checksum"

	// SidecarIndex appends one "<offset> <length>" line for every accepted
	// Write, describing where each write landed in the main log file.
	SidecarIndex = "index"

	// SidecarOpaque leaves the content to the application; lumberjack only
	// creates, syncs, and rotates the file as part of the group.
	SidecarOpaque = ""
)

const (
	// rotateTmpSuffix marks a current group member that has been renamed
	// aside but not yet moved to its backup name.  Files with this suffix
	// only exist while a rotation is in flight (or was interrupted).
	rotateTmpSuffix = ".rotating"

	// compressTmpSuffix marks a compressed group member that has not been
	// committed to its final name yet.
	compressTmpSuffix = ".tmp"

	// quarantineSuffix is appended to leftover temporary files that cannot
	// be completed safely during recovery.
	quarantineSuffix = ".quarantine"
)

// Sidecar declares an auxiliary file that is maintained next to the main
// log file and rotated together with it as one atomic file group.
type Sidecar struct {
	// Suffix is appended to the log filename to form the sidecar's name,
	// e.g. ".sum" turns server.log into server.log.sum.  It must be
	// non-empty and unique within Logger.Sidecars.
	Suffix string `json:"suffix" yaml:"suffix"`

	// Kind selects what lumberjack maintains in the file: SidecarChecksum,
	// SidecarIndex, or SidecarOpaque (the default).
	Kind string `json:"kind" yaml:"kind"`
}

// sidecarFile is the runtime state of one open sidecar.
type sidecarFile struct {
	spec Sidecar
	file *os.File
	hash hash.Hash // non-nil for SidecarChecksum
}

// validateSidecars checks the sidecar configuration for consistency.
func (l *Logger) validateSidecars() error {
	seen := make(map[string]bool, len(l.Sidecars))
	for _, sc := range l.Sidecars {
		if sc.Suffix == "" {
			return errors.New("sidecar suffix must not be empty")
		}
		if strings.ContainsRune(sc.Suffix, '/') || strings.ContainsRune(sc.Suffix, os.PathSeparator) {
			return fmt.Errorf("sidecar suffix %q must not contain a path separator", sc.Suffix)
		}
		if seen[sc.Suffix] {
			return fmt.Errorf("duplicate sidecar suffix %q", sc.Suffix)
		}
		seen[sc.Suffix] = true
		switch sc.Kind {
		case SidecarChecksum, SidecarIndex, SidecarOpaque:
		default:
			return fmt.Errorf("unknown sidecar kind %q", sc.Kind)
		}
	}
	return nil
}

// groupMembers returns the paths of the current file group: the main log
// file first, then each sidecar in configured (sync) order.
func (l *Logger) groupMembers() []string {
	name := l.filename()
	members := make([]string, 0, len(l.Sidecars)+1)
	members = append(members, name)
	for _, sc := range l.Sidecars {
		members = append(members, name+sc.Suffix)
	}
	return members
}

// groupSuffixes returns the suffix identifying each group member within a
// backup name, parallel to groupMembers: "" for the main log file, then the
// configured sidecar suffixes.
func (l *Logger) groupSuffixes() []string {
	suffixes := make([]string, 0, len(l.Sidecars)+1)
	suffixes = append(suffixes, "")
	for _, sc := range l.Sidecars {
		suffixes = append(suffixes, sc.Suffix)
	}
	return suffixes
}

// openSidecars opens the current sidecar files.  When trunc is true the
// files start a fresh generation and are truncated; otherwise they are
// opened to continue the existing generation and checksum state is rebuilt
// from the current main log file.  On failure every file opened so far is
// left for l.close to clean up.
func (l *Logger) openSidecars(mode os.FileMode, trunc bool) error {
	if len(l.Sidecars) == 0 {
		return nil
	}
	if err := l.validateSidecars(); err != nil {
		return err
	}
	name := l.filename()
	l.sidecarFiles = make([]*sidecarFile, len(l.Sidecars))
	for i, spec := range l.Sidecars {
		sc := &sidecarFile{spec: spec}
		l.sidecarFiles[i] = sc

		flag := os.O_CREATE | os.O_WRONLY
		switch spec.Kind {
		case SidecarIndex:
			// index lines are only ever appended
			if trunc {
				flag |= os.O_TRUNC
			} else {
				flag |= os.O_APPEND
			}
		default:
			// checksum files are rewritten from the start on every
			// update; opaque files are never written by us
			if trunc {
				flag |= os.O_TRUNC
			}
		}

		f, err := os.OpenFile(name+spec.Suffix, flag, mode)
		if err != nil {
			return fmt.Errorf("can't open sidecar file: %s", err)
		}
		sc.file = f

		if spec.Kind == SidecarChecksum {
			sc.hash = sha256.New()
			if !trunc {
				if err := l.rebuildChecksum(sc); err != nil {
					return err
				}
			}
			if err := sc.rewriteChecksum(l.size); err != nil {
				return fmt.Errorf("can't update checksum sidecar: %s", err)
			}
		}
	}
	return nil
}

// rebuildChecksum restores the running checksum of a continued generation
// by hashing the existing content of the main log file.
func (l *Logger) rebuildChecksum(sc *sidecarFile) error {
	main, err := os.Open(l.filename())
	if err != nil {
		return fmt.Errorf("can't rebuild checksum sidecar: %s", err)
	}
	defer main.Close()
	if _, err := io.CopyN(sc.hash, main, l.size); err != nil {
		return fmt.Errorf("can't rebuild checksum sidecar: %s", err)
	}
	return nil
}

// rewriteChecksum replaces the checksum sidecar's content with the current
// hash and size of the generation.
func (sc *sidecarFile) rewriteChecksum(size int64) error {
	line := fmt.Sprintf("%x %d\n", sc.hash.Sum(nil), size)
	if _, err := sc.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	n, err := io.WriteString(sc.file, line)
	if err != nil {
		return err
	}
	return sc.file.Truncate(int64(n))
}

// updateSidecars applies an accepted write of len(p) bytes at the given
// offset in the main log file to every sidecar.
func (l *Logger) updateSidecars(p []byte, offset int64) error {
	for _, sc := range l.sidecarFiles {
		if sc.file == nil {
			continue
		}
		switch sc.spec.Kind {
		case SidecarChecksum:
			sc.hash.Write(p)
			if err := sc.rewriteChecksum(l.size); err != nil {
				return fmt.Errorf("can't update checksum sidecar: %s", err)
			}
		case SidecarIndex:
			if _, err := fmt.Fprintf(sc.file, "%d %d\n", offset, len(p)); err != nil {
				return fmt.Errorf("can't update index sidecar: %s", err)
			}
		}
	}
	return nil
}

// renameGroupAside moves every existing member of the current file group
// aside so a fresh generation can be started.  Each member is first renamed
// to a recognizable temporary name and then to its backup name for
// generation gen; if any rename fails, every member is moved back to its
// original name so the group is left in a consistent state.
//
// The rename order makes interrupted rotations recoverable: sidecars are
// renamed aside first (in configured sync order) and the main log last,
// then the main log's backup is committed first.  A leftover temporary
// main file therefore always means the generation had no backups yet.
func (l *Logger) renameGroupAside(gen string) error {
	members := l.groupMembers()
	suffixes := l.groupSuffixes()
	backup := backupNameGen(l.filename(), gen)

	temps := make([]string, len(members))
	var committed []int

	// rollback returns every renamed member to its original current name.
	rollback := func() {
		for _, i := range committed {
			// best effort: move the committed backup back
			_ = osRename(backup+suffixes[i], members[i])
		}
		for i, tmp := range temps {
			if tmp != "" {
				_ = osRename(tmp, members[i])
			}
		}
	}

	// Phase 1: rename current files to temporary names, sidecars first (in
	// configured sync order), the main log last.
	order := make([]int, 0, len(members))
	for i := 1; i < len(members); i++ {
		order = append(order, i)
	}
	order = append(order, 0)

	for _, i := range order {
		if _, err := osStat(members[i]); os.IsNotExist(err) {
			continue
		}
		tmp := members[i] + rotateTmpSuffix
		if err := osRename(members[i], tmp); err != nil {
			rollback()
			return fmt.Errorf("can't rename log file: %s", err)
		}
		temps[i] = tmp
	}

	// Phase 2: commit temporary files to their backup names, main first.
	for _, i := range append([]int{0}, order[:len(order)-1]...) {
		if temps[i] == "" {
			continue
		}
		if err := osRename(temps[i], backup+suffixes[i]); err != nil {
			rollback()
			return fmt.Errorf("can't rename log file: %s", err)
		}
		committed = append(committed, i)
		temps[i] = ""
	}
	return nil
}

// ensureRecovered runs startup recovery exactly once per Logger.
func (l *Logger) ensureRecovered() error {
	l.recoverOnce.Do(func() {
		l.recoverErr = l.recover()
	})
	return l.recoverErr
}

// recover inspects the log directory for leftovers of an interrupted
// previous run and brings the file group back to a consistent state before
// any new write is accepted:
//
//   - partial compression results (*.gz.tmp) are discarded; members that
//     exist both compressed and uncompressed keep the uncompressed source
//     (a stale compressed product is discarded and re-created by the mill).
//   - an interrupted rotation is completed: every leftover temporary and
//     current member of the old generation is moved to backup names sharing
//     one generation timestamp, so subsequent writes start a fresh
//     generation instead of appending to the old one.
//   - temporary files that cannot be completed safely (unknown members of
//     the group, or members conflicting with an existing current file) are
//     quarantined by renaming them with a ".quarantine" suffix.
func (l *Logger) recover() error {
	if err := l.validateSidecars(); err != nil {
		return err
	}
	entries, err := ioutil.ReadDir(l.dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("can't read log file directory: %s", err)
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names[e.Name()] = true
		}
	}

	// Discard partial compression results and compressed/uncompressed
	// duplicates of our backup members.
	for n := range names {
		if strings.HasSuffix(n, compressSuffix+compressTmpSuffix) {
			base := strings.TrimSuffix(strings.TrimSuffix(n, compressTmpSuffix), compressSuffix)
			if _, _, ok := l.parseMemberName(base); ok {
				if err := os.Remove(filepath.Join(l.dir(), n)); err != nil {
					return fmt.Errorf("can't remove partial compressed file: %s", err)
				}
			}
			continue
		}
		if strings.HasSuffix(n, compressSuffix) {
			base := strings.TrimSuffix(n, compressSuffix)
			if _, _, ok := l.parseMemberName(base); ok && names[base] {
				// the uncompressed source is authoritative: a leftover
				// compressed file may be a partial product of an old
				// run, and recompressing the source reproduces a
				// complete one deterministically
				if err := os.Remove(filepath.Join(l.dir(), n)); err != nil {
					return fmt.Errorf("can't remove stale compressed file: %s", err)
				}
			}
		}
	}

	// Collect the temporary rotation files that belong to this group.
	members := l.groupMembers()
	memberIndex := make(map[string]int, len(members))
	for i, m := range members {
		memberIndex[filepath.Base(m)] = i
	}
	mainBase := filepath.Base(l.filename())

	inFlight := false
	for n := range names {
		if !strings.HasSuffix(n, rotateTmpSuffix) {
			continue
		}
		base := strings.TrimSuffix(n, rotateTmpSuffix)
		if _, ok := memberIndex[base]; !ok {
			if strings.HasPrefix(base, mainBase) {
				// looks like a member of this group, but doesn't match
				// the configured members; we can't complete it
				if err := quarantine(filepath.Join(l.dir(), n)); err != nil {
					return err
				}
			}
			continue
		}
		if names[base] {
			// both a current file and a renamed-aside copy exist; we
			// can't tell which one is authoritative, so keep the
			// current file and quarantine the leftover
			if err := quarantine(filepath.Join(l.dir(), n)); err != nil {
				return err
			}
			continue
		}
		inFlight = true
	}
	if !inFlight {
		return nil
	}

	// A rotation was interrupted.  Determine the generation it belongs to:
	// if the main log's backup was already committed, the newest backup
	// generation is the in-flight one; otherwise start a fresh generation
	// that cannot collide with any existing backup.
	gen := ""
	_, mainTempExists := names[mainBase+rotateTmpSuffix]
	if !mainTempExists && !names[mainBase] {
		if groups, err := l.logGroups(); err == nil && len(groups) > 0 {
			gen = groups[0].timestamp.Format(backupTimeFormat)
		}
	}
	if gen == "" {
		gen = l.uniqueGeneration()
	}

	// Complete the rotation: move every leftover temporary and every
	// remaining current member into the generation's backup names.
	backup := backupNameGen(l.filename(), gen)
	suffixes := l.groupSuffixes()
	for i, m := range members {
		target := backup + suffixes[i]
		tmp := m + rotateTmpSuffix
		source := ""
		if _, err := osStat(tmp); err == nil {
			source = tmp
		} else if _, err := osStat(m); err == nil {
			source = m
		}
		if source == "" {
			continue
		}
		if _, err := osStat(target); err == nil {
			// the generation already has this member; we can't
			// complete this file without overwriting data
			if err := quarantine(source); err != nil {
				return err
			}
			continue
		}
		if err := osRename(source, target); err != nil {
			return fmt.Errorf("can't complete interrupted rotation: %s", err)
		}
	}
	return nil
}

// uniqueGeneration returns a generation timestamp for the current time
// that does not collide with any existing backup member of the group.
func (l *Logger) uniqueGeneration() string {
	t := currentTime()
	if !l.LocalTime {
		t = t.UTC()
	}
	suffixes := l.groupSuffixes()
	for {
		gen := t.Format(backupTimeFormat)
		clash := false
		backup := backupNameGen(l.filename(), gen)
		for _, sfx := range suffixes {
			for _, cand := range []string{backup + sfx, backup + sfx + compressSuffix} {
				if _, err := osStat(cand); err == nil {
					clash = true
					break
				}
			}
			if clash {
				break
			}
		}
		if !clash {
			return gen
		}
		t = t.Add(time.Millisecond)
	}
}

// quarantine moves a leftover file out of the way so it can neither
// interfere with rotation nor be appended to, without deleting any data.
func quarantine(path string) error {
	target := path + quarantineSuffix
	for i := 1; ; i++ {
		if _, err := osStat(target); os.IsNotExist(err) {
			break
		}
		target = fmt.Sprintf("%s%s.%d", path, quarantineSuffix, i)
	}
	return osRename(path, target)
}

// logGroupFile is one file belonging to a backup generation.
type logGroupFile struct {
	info       os.FileInfo
	compressed bool
}

// logGroup is one backup generation: all files (main log and sidecars,
// compressed or not) that share one rotation timestamp.
type logGroup struct {
	timestamp time.Time
	files     []logGroupFile
}

// logGroups returns the backup generations stored in the log directory,
// sorted newest first.  Files that don't belong to this logger's group
// naming scheme are ignored.
func (l *Logger) logGroups() ([]*logGroup, error) {
	entries, err := ioutil.ReadDir(l.dir())
	if err != nil {
		return nil, fmt.Errorf("can't read log file directory: %s", err)
	}
	var groups []*logGroup
	byGen := make(map[time.Time]*logGroup)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		compressed := strings.HasSuffix(name, compressSuffix)
		base := strings.TrimSuffix(name, compressSuffix)
		ts, _, ok := l.parseMemberName(base)
		if !ok {
			continue
		}
		g := byGen[ts]
		if g == nil {
			g = &logGroup{timestamp: ts}
			byGen[ts] = g
			groups = append(groups, g)
		}
		g.files = append(g.files, logGroupFile{info: e, compressed: compressed})
	}
	sort.Sort(byGenTime(groups))
	return groups, nil
}

// parseMemberName parses a backup file base name (without any compression
// suffix) into its generation timestamp and the suffix of the group member
// it belongs to ("" for the main log file).
func (l *Logger) parseMemberName(base string) (time.Time, string, bool) {
	prefix, ext := l.prefixAndExt()
	if !strings.HasPrefix(base, prefix) {
		return time.Time{}, "", false
	}
	rest := base[len(prefix):]
	// try the longest member suffixes first so overlapping suffixes parse
	// correctly; "" (the main log) is always last
	suffixes := l.groupSuffixes()[1:]
	sort.Slice(suffixes, func(i, j int) bool { return len(suffixes[i]) > len(suffixes[j]) })
	suffixes = append(suffixes, "")
	for _, sfx := range suffixes {
		end := ext + sfx
		if !strings.HasSuffix(rest, end) || len(rest) <= len(end) {
			continue
		}
		if t, err := time.Parse(backupTimeFormat, rest[:len(rest)-len(end)]); err == nil {
			return t, sfx, true
		}
	}
	return time.Time{}, "", false
}

// compressGroup compresses every uncompressed member of a backup
// generation.  Members are compressed to temporary files first and only
// committed once every member succeeded, so a failure anywhere leaves the
// whole generation in its original, consistent (uncompressed) state.
func (l *Logger) compressGroup(g *logGroup) error {
	type member struct {
		src, dst, tmp string
	}
	var members []member
	for _, f := range g.files {
		if f.compressed {
			continue
		}
		src := filepath.Join(l.dir(), f.info.Name())
		dst := src + compressSuffix
		members = append(members, member{src, dst, dst + compressTmpSuffix})
	}
	if len(members) == 0 {
		return nil
	}

	for _, m := range members {
		if err := compressToFile(m.src, m.tmp); err != nil {
			for _, m2 := range members {
				os.Remove(m2.tmp)
			}
			return err
		}
	}

	renamed := 0
	var err error
	for _, m := range members {
		if err = osRename(m.tmp, m.dst); err != nil {
			break
		}
		// the temporary file was chowned before compression; carry the
		// owner over to the final name (a no-op anywhere but linux)
		var fi os.FileInfo
		if fi, err = osStat(m.src); err != nil {
			break
		}
		if err = chownExisting(m.dst, fi); err != nil {
			break
		}
		renamed++
	}
	if err != nil {
		// roll back to a consistent state: every source file is still
		// present, so drop the compressed products
		for i := 0; i < renamed; i++ {
			os.Remove(members[i].dst)
		}
		for _, m := range members {
			os.Remove(m.tmp)
		}
		return fmt.Errorf("can't compress log file group: %s", err)
	}

	for _, m := range members {
		if err := os.Remove(m.src); err != nil {
			return fmt.Errorf("can't remove uncompressed log file: %s", err)
		}
	}
	return nil
}

// compressToFile compresses src into dst (a temporary name), syncing the
// result to disk.  The source file is left untouched; dst is removed again
// if anything fails.
func compressToFile(src, dst string) (err error) {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open log file: %v", err)
	}
	defer f.Close()

	fi, err := osStat(src)
	if err != nil {
		return fmt.Errorf("failed to stat log file: %v", err)
	}

	if err := chown(dst, fi); err != nil {
		return fmt.Errorf("failed to chown compressed log file: %v", err)
	}

	// If this file already exists, we presume it was created by
	// a previous attempt to compress the log file.
	gzf, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode())
	if err != nil {
		return fmt.Errorf("failed to open compressed log file: %v", err)
	}
	defer gzf.Close()

	gz := gzip.NewWriter(gzf)

	defer func() {
		if err != nil {
			os.Remove(dst)
			err = fmt.Errorf("failed to compress log file: %v", err)
		}
	}()

	if _, err := io.Copy(gz, f); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := gzf.Sync(); err != nil {
		return err
	}
	if err := gzf.Close(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	return nil
}

// byGenTime sorts backup generations by newest timestamp first.
type byGenTime []*logGroup

func (b byGenTime) Less(i, j int) bool {
	return b[i].timestamp.After(b[j].timestamp)
}

func (b byGenTime) Swap(i, j int) {
	b[i], b[j] = b[j], b[i]
}

func (b byGenTime) Len() int {
	return len(b)
}
