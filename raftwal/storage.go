/*
 * Copyright 2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package raftwal

import (
	"encoding/binary"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/dgraph-io/badger/v2"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/x"
	"github.com/dgraph-io/ristretto/z"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.etcd.io/etcd/raft"
	"go.etcd.io/etcd/raft/raftpb"
	"golang.org/x/net/trace"
	"golang.org/x/sys/unix"
)

// versionKey is hardcoded into the special key used to fetch the maximum version from the DB.
const versionKey = 1

// DiskStorage handles disk access and writing for the RAFT write-ahead log.
// Dir would contain wal.meta file.
// And <start idx zero padded>.ent file.
//
// --- meta.wal wal.meta file ---
// This file should only be 4KB, so it can fit nicely in one Linux page.
// Store the raft ID in the first 8 bytes.
// wal.meta file would have the Snapshot and the HardState. First put hard state, then put Snapshot.
// Leave extra bytes in between to ensure they never overlap.
// Hardstate allocate 1KB. Rest 3KB for Snapshot. So snapshot is always accessible from offset=1024.
// Also checkpoint key goes into meta.
//
// --- <0000i>.ent files ---
// This would contain the raftpb.Entry protos. It contains term, index, type and data. No need to do
// proto.Marshal here.
// Each file can contain 10K entries.
// Term takes 8 bytes, Index takes 8 bytes, Type takes 8 bytes and Data we should store an offset to
// the actual slice, which can be 8 bytes. Total = 32 bytes.
// First 30K entries would consume 960KB.
// Pre-allocate 1MB in each file just for these entries, and zero them out explicitly. Zeroing them
// out would ensure that you'd know when these entries end, in case of a restart. In that case, the
// index would be zero, so you know that's the end.
//
// And the data for these entries are laid out starting offset=1<<20. Those are the offsets you
// store in the Entry for Data field.
// After 30K entries, you rotate the file.
//
// --- clean up ---
// If snapshot idx = Idx_s. Find the first wal.ent whose first Entry is less than Idx_s. This file
// and anything above MUST be kept. All the wal.ent files lower than this file can be deleted.
//
// --- sync ---
// Just do msync calls to sync the mmapped buffer. It would sync that to the disk.
//
// --- crashes ---
// sync would have already flushed the mmap to disk. mmap deals with process crashes just fine. So,
// we're good there. In case of file system crashes or disk crashes, we might need to replace this
// node anyway. The new node would get a new WAL.
//
type DiskStorage struct {
	dir      string
	commitTs uint64
	id       uint64
	gid      uint32
	elog     trace.EventLog

	meta    *metaFile
	entries *entryLog
}

type indexRange struct {
	from, until uint64 // index range for deletion, until index is not deleted.
}

// Constants to use when writing to mmap'ed meta and entry files.
const (
	// metaName is the name of the file used to store metadata (e.g raft ID, checkpoint).
	metaName = "wal.meta"
	// metaFileSize is the size of the wal.meta file (4KB).
	metaFileSize = 4 << 30
	// raftIdOffset is the offset of the raft ID within the wal.meta file.
	raftIdOffset = 0
	// checkpointOffset is the offset of the checkpoint within the wal.meta file.
	checkpointOffset = 8
	//hardStateOffset is the offset of the hard sate within the wal.meta file.
	hardStateOffset = 512
	// snapshotOffest is the offset of the snapshot within the wal.meta file.
	snapshotOffset = 1024
	// maxNumEntries is maximum number of entries before rotating the file.
	maxNumEntries = 30000
	// entryFileOffset
	entryFileOffset = 1 << 20 // 1MB
	// entryFileSize is the initial size of the entry file.
	entryFileSize = 4 * entryFileOffset // 4MB
	// entrySize is the size in bytes of a single entry.
	entrySize = 32
)

var (
	emptyEntry = entry(make([]byte, entrySize))
)

type mmapFile struct {
	data []byte
	fd   *os.File
	// offset int64
}

func (m *mmapFile) sync() error {
	// TODO: Switch this to z.Msync. And probably use MS_SYNC
	return unix.Msync(m.data, unix.MS_SYNC)
}

func (m *mmapFile) slice(offset int) []byte {
	sz := binary.BigEndian.Uint32(m.data[offset:])
	start := offset + 4
	next := start + int(sz)
	res := m.data[start:next]
	return res
}

func (m *mmapFile) allocateSlice(sz, offset int) ([]byte, int) {
	binary.BigEndian.PutUint32(m.data[offset:], uint32(sz))
	return m.data[offset+4 : offset+4+sz], offset + 4 + sz
}

type metaFile struct {
	*mmapFile
}

func zeroOut(buf []byte, start, end int) {
	buf[start] = 0x00
	for i := start + 1; i < end; i *= 2 {
		copy(buf[i:], buf[:i])
	}
}

func newMetaFile(dir string) (*metaFile, error) {
	fname := filepath.Join(dir, metaName)
	mf, err := openMmapFile(fname, os.O_RDWR|os.O_CREATE, metaFileSize)
	if err == errNewFile {
		zeroOut(mf.data, 0, snapshotOffset+4)
	} else if err != nil {
		return nil, errors.Wrapf(err, "unable to open meta file")
	}
	return &metaFile{mmapFile: mf}, nil
}

var errNewFile = errors.New("new file")

func openMmapFile(filename string, flag int, maxSz int) (*mmapFile, error) {
	fd, err := os.OpenFile(filename, flag, 0666)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to open: %s", filename)
	}
	fi, err := fd.Stat()
	if err != nil {
		return nil, errors.Wrapf(err, "cannot stat file: %s", filename)
	}
	fileSize := fi.Size()
	if fileSize > int64(maxSz) {
		return nil, errors.Errorf("file size %d does not match zero or max size %d",
			fileSize, maxSz)
	}
	if err := fd.Truncate(int64(maxSz)); err != nil {
		return nil, errors.Wrapf(err, "error while truncation")
	}
	buf, err := z.Mmap(fd, true, int64(maxSz)) // Mmap up to max size.
	if err != nil {
		return nil, errors.Wrapf(err, "while mmapping %s with size: %d", fd.Name(), maxSz)
	}

	err = nil
	if fileSize == 0 {
		err = errNewFile
	}
	return &mmapFile{
		data: buf,
		fd:   fd,
	}, err
}

func writeSlice(dst []byte, src []byte) {
	binary.BigEndian.PutUint32(dst[:4], uint32(len(src)))
	copy(dst[4:], src)
}

func allocateSlice(dst []byte, sz int) []byte {
	binary.BigEndian.PutUint32(dst[:4], uint32(sz))
	return dst[4 : 4+sz]
}

func sliceSize(dst []byte, offset int) int {
	sz := binary.BigEndian.Uint32(dst[offset:])
	return 4 + int(sz)
}

func readSlice(dst []byte, offset int) []byte {
	b := dst[offset:]
	sz := binary.BigEndian.Uint32(b)
	return b[4 : 4+sz]
}

func (m *metaFile) raftBuf() []byte {
	return m.data[raftIdOffset : raftIdOffset+8]
}

func (m *metaFile) RaftId() uint64 {
	return binary.BigEndian.Uint64(m.raftBuf())
}

func (m *metaFile) StoreRaftId(id uint64) {
	binary.BigEndian.PutUint64(m.raftBuf(), id)
}

func (m *metaFile) UpdateCheckpoint(index uint64) {
	binary.BigEndian.PutUint64(m.data[checkpointOffset:], index)
}

func (m *metaFile) Checkpoint() uint64 {
	return binary.BigEndian.Uint64(m.data[checkpointOffset:])
}

func (m *metaFile) StoreHardState(hs *raftpb.HardState) error {
	if hs == nil || raft.IsEmptyHardState(*hs) {
		return nil
	}
	buf, err := hs.Marshal()
	if err != nil {
		return errors.Wrapf(err, "cannot marshal hard state")
	}
	x.AssertTrue(len(buf) < snapshotOffset-hardStateOffset)
	writeSlice(m.data[hardStateOffset:], buf)
	return nil
}

func (m *metaFile) HardState() (raftpb.HardState, error) {
	val := readSlice(m.data, hardStateOffset)
	var hs raftpb.HardState

	if len(val) == 0 {
		return hs, nil
	}
	if err := hs.Unmarshal(val); err != nil {
		return hs, errors.Wrapf(err, "cannot parse hardState")
	}
	return hs, nil
}

func (m *metaFile) StoreSnapshot(snap *raftpb.Snapshot) error {
	if snap == nil || raft.IsEmptySnap(*snap) {
		return nil
	}
	buf, err := snap.Marshal()
	if err != nil {
		return errors.Wrapf(err, "cannot marshal snapshot")
	}
	if len(m.data)-snapshotOffset < len(buf) {
		return errors.Errorf("Unable to store snapshot of size: %d\n", len(buf))
	}
	writeSlice(m.data[snapshotOffset:], buf)
	return nil
}

func (m *metaFile) Snapshot() (raftpb.Snapshot, error) {
	val := readSlice(m.data, snapshotOffset)

	var snap raftpb.Snapshot
	if len(val) == 0 {
		return snap, nil
	}

	if err := snap.Unmarshal(val); err != nil {
		return snap, errors.Wrapf(err, "cannot parse snapshot")
	}
	return snap, nil
}

type entry []byte

func (e entry) Term() uint64 {
	return binary.BigEndian.Uint64(e)
}
func (e entry) Index() uint64 {
	return binary.BigEndian.Uint64(e[8:])
}
func (e entry) DataOffset() uint64 {
	return binary.BigEndian.Uint64(e[16:])
}
func (e entry) Type() uint64 {
	return binary.BigEndian.Uint64(e[24:])
}

func marshalEntry(b []byte, term, index, do, typ uint64) {
	x.AssertTrue(len(b) == entrySize)

	binary.BigEndian.PutUint64(b, term)
	binary.BigEndian.PutUint64(b[8:], index)
	binary.BigEndian.PutUint64(b[16:], do)
	binary.BigEndian.PutUint64(b[24:], typ)
}

// entryFile represents a single log file.
type entryFile struct {
	*mmapFile
	fid int64
}

func (ef *entryFile) delete() error {
	if err := z.Munmap(ef.data); err != nil {
		glog.Errorf("while munmap file: %s, error: %v\n", ef.fd.Name(), err)
	}
	if err := ef.fd.Truncate(0); err != nil {
		glog.Errorf("while truncate file: %s, error: %v\n", ef.fd.Name(), err)
	}
	return os.Remove(ef.fd.Name())
}

func openEntryFile(path string) (*entryFile, error) {
	mf, err := openMmapFile(path, os.O_RDWR|os.O_CREATE, 16<<30)
	if err == errNewFile {
		zeroOut(mf.data, 0, entryFileOffset)
	} else if err != nil {
		return nil, err
	}
	ef := &entryFile{
		mmapFile: mf,
	}
	return ef, nil
}

func getEntryFiles(dir string) ([]*entryFile, error) {
	entryFiles := x.WalkPathFunc(dir, func(path string, isDir bool) bool {
		if isDir {
			return false
		}
		if strings.HasSuffix(path, ".ent") {
			return true
		}
		return false
	})

	files := make([]*entryFile, 0)
	for _, path := range entryFiles {
		fid, err := strconv.ParseInt(strings.Split(".ent", path)[0], 10, 64)
		if err != nil {
			return nil, errors.Wrapf(err, "while parsing: %s", path)
		}
		f, err := openEntryFile(path)
		if err != nil {
			return nil, err
		}
		f.fid = fid
		files = append(files, f)
	}

	// Sort files by the first index they store.
	sort.Slice(files, func(i, j int) bool {
		return files[i].firstEntry().Index() < files[j].firstEntry().Index()
	})
	return files, nil
}

// get entry from a file.
func (ef *entryFile) getEntry(idx int) entry {
	offset := idx * entrySize
	return entry(ef.data[offset : offset+entrySize])
}

func (ef *entryFile) GetRaftEntry(idx int) raftpb.Entry {
	entry := ef.getEntry(idx)
	re := raftpb.Entry{
		Term:  entry.Term(),
		Index: entry.Index(),
		Type:  raftpb.EntryType(entry.Type()),
	}
	if entry.DataOffset() > 0 {
		re.Data = ef.slice(int(entry.DataOffset()))
	}
	return re
}

func (ef *entryFile) firstEntry() entry {
	return ef.getEntry(0)
}
func (ef *entryFile) firstIndex() uint64 {
	return ef.getEntry(0).Index()
}

func (ef *entryFile) lastEntry() entry {
	for i := maxNumEntries - 1; i >= 0; i-- {
		e := ef.getEntry(i)
		if e.Index() > 0 {
			return e
		}
	}
	return emptyEntry
}

func (ef *entryFile) Term(entryIndex uint64) uint64 {
	offset, found := ef.Offset(entryIndex)
	if !found {
		return 0
	}
	e := ef.getEntry(int(offset))
	return e.Term()
}

func (ef *entryFile) Offset(raftIndex uint64) (int, bool) {
	fi := ef.firstIndex()
	if raftIndex < fi {
		return 0, false
	}
	if diff := int(raftIndex - fi); diff < maxNumEntries && diff >= 0 {
		e := ef.getEntry(diff)
		if e.Index() == raftIndex {
			return diff, true
		}
	}

	// This would find the first entry's index which has entryIndex.
	idx := sort.Search(maxNumEntries, func(i int) bool {
		e := ef.getEntry(i)
		if e.Index() == 0 {
			// We reached too far to the right.
			return true
		}
		return e.Index() >= raftIndex
	})
	if idx == maxNumEntries {
		return 0, false
	}
	if e := ef.getEntry(idx); e.Index() == raftIndex {
		return idx, true
	}
	return 0, false
}

// entryLog represents the entire entry log. It consists of one or more
// entryFile objects.
type entryLog struct {
	// need lock for files and current ?

	// files is the list of all log files ordered in ascending order by the first
	// index in the file. The current file being written should always be accessible
	// by looking at the last element of this slice.
	files   []*entryFile
	current *entryFile
	// nextEntryIdx is the index of the next entry to write to. When this value exceeds
	// maxNumEntries the file will be rotated.
	nextEntryIdx int
	// lastIndex is the value of last index written to the log.
	lastIndex uint64
	// dir is the directory to use to store files.
	dir string
}

func openEntryLog(dir string) (*entryLog, error) {
	e := &entryLog{
		dir: dir,
	}
	files, err := getEntryFiles(dir)
	if err != nil {
		return nil, err
	}
	e.files = files

	var nextFid int64
	for _, ef := range e.files {
		if nextFid < ef.fid {
			nextFid = ef.fid
		}
	}
	nextFid += 1
	ef, err := openEntryFile(path.Join(dir, fmt.Sprintf("%05d.ent", nextFid)))
	if err != nil {
		return nil, errors.Wrapf(err, "while creating a new entry file")
	}

	// Won't append current file to list of files.
	e.current = ef
	return e, nil
}

func (l *entryLog) lastFile() *entryFile {
	return l.files[len(l.files)-1]
}

// getEntry gets the nth entry in the CURRENT log file.
func (l *entryLog) getEntry(n int) (entry, error) {
	if n >= maxNumEntries {
		return nil, errors.Errorf("there cannot be more than %d in a single file",
			maxNumEntries)
	}

	start := n * entrySize
	buf := l.current.data[start : start+entrySize]
	return entry(buf), nil
}

func (l *entryLog) rotate(firstIndex uint64) error {
	var nextFid int64
	for _, ef := range l.files {
		if nextFid < ef.fid {
			nextFid = ef.fid
		}
	}
	nextFid += 1
	ef, err := openEntryFile(path.Join(l.dir, fmt.Sprintf("%05d.ent", nextFid)))
	if err != nil {
		return errors.Wrapf(err, "while creating a new entry file")
	}

	l.files = append(l.files, l.current)
	l.current = ef
	return nil
}

func (l *entryLog) numEntries() int {
	if len(l.files) == 0 {
		return 0
	}
	total := 0
	if len(l.files) >= 1 {
		// all files except the last one.
		total += (len(l.files) - 1) * maxNumEntries
	}
	return total + l.nextEntryIdx
}

func (l *entryLog) AddEntries(entries []raftpb.Entry) error {
	fidx, eidx := l.Offset(entries[0].Index)
	if eidx >= 0 {
		if fidx == -1 {
			zeroOut(l.current.data, entrySize*eidx, entrySize*l.nextEntryIdx)
			l.nextEntryIdx = eidx

		} else {
			x.AssertTrue(fidx != len(l.files))
			l.current = l.files[fidx]
			extra := l.files[fidx+1:]
			for _, ef := range extra {
				if err := ef.delete(); err != nil {
					glog.Errorf("deleting file: %s. error: %v\n", ef.fd.Name(), err)
				}
			}
			zeroOut(l.current.data, entrySize*eidx, entryFileOffset)
			l.nextEntryIdx = eidx

			l.files = l.files[:fidx]
		}
	}
	prev := l.nextEntryIdx - 1
	var offset int
	if prev >= 0 {
		e := l.current.getEntry(prev)
		offset = int(e.DataOffset())
		offset += sliceSize(l.current.data, offset)
	} else {
		offset = entryFileOffset
	}

	for _, re := range entries {
		if l.nextEntryIdx >= maxNumEntries {
			if err := l.rotate(re.Index); err != nil {
				// TODO: see what happens.
				return err
			}
			offset = entryFileOffset
		}

		destBuf, next := l.current.allocateSlice(len(re.Data), offset)
		x.AssertTrue(copy(destBuf, re.Data) == len(re.Data))

		buf, err := l.getEntry(l.nextEntryIdx)
		x.Check(err)
		marshalEntry(buf, re.Term, re.Index, uint64(offset), uint64(re.Type))

		// Update for next entry.
		offset = next
		l.nextEntryIdx++
	}
	return nil
}

func (l *entryLog) DiscardFiles(snapshotIndex uint64) error {
	// TODO: delete all the files below the first file with a first index
	// less than or equal to snapshotIndex.
	return nil
}

func (l *entryLog) FirstIndex() uint64 {
	if l == nil || len(l.files) == 0 {
		return 0
	}
	return l.files[0].firstEntry().Index()
}

func (l *entryLog) LastIndex() uint64 {
	if l.nextEntryIdx-1 >= 0 {
		e := l.current.getEntry(l.nextEntryIdx - 1)
		return e.Index()
	}
	if len(l.files) == 0 {
		return 0
	}
	for i := len(l.files) - 1; i >= 0; i-- {
		ef := l.files[i]
		e := ef.lastEntry()
		if e.Index() > 0 {
			return e.Index()
		}
	}
	return l.lastIndex
}

func (l *entryLog) getEntryFile(fidx int) *entryFile {
	if fidx == -1 {
		return l.current
	}
	if fidx >= len(l.files) {
		return nil
	}
	return l.files[fidx]
}

func (l *entryLog) seekEntry(idx uint64) (entry, error) {
	fidx, off := l.Offset(idx)
	ef := l.getEntryFile(fidx)
	if off == -1 || ef == nil {
		return emptyEntry, errNotFound
	}
	ent := ef.getEntry(off)
	if ent.Index() != idx {
		return emptyEntry, errNotFound
	}
	return ent, nil
}

// Term returns the term of entry i, which must be in the range
// [FirstIndex()-1, LastIndex()]. The term of the entry before
// FirstIndex is retained for matching purposes even though the
// rest of that entry may not be available.
func (l *entryLog) Term(idx uint64) (uint64, error) {
	// Look at the entry files and find the entry file with entry bigger than idx.
	// Read file before that idx.
	ent, err := l.seekEntry(idx)
	if err != nil {
		return 0, err
	}
	t := ent.Term()
	if t == 0 {
		return 0, raft.ErrCompacted
	}
	return t, nil
}

// Offset returns the file index and the offset within that file in which the entry
// with the given index can be found. A value of -1 for the file index means that the
// entry is in the current file.
func (l *entryLog) Offset(idx uint64) (int, int) {
	// Look for the offset in the current file.
	if offset, found := l.current.Offset(idx); found {
		return -1, offset
	}

	// No previous files, therefore we can only go back to the start of the current file.
	if len(l.files) == 0 {
		return -1, -1
	}

	fileIdx := sort.Search(len(l.files), func(i int) bool {
		return l.files[i].firstIndex() >= idx
	})
	if fileIdx >= len(l.files) {
		fileIdx = len(l.files) - 1
	}
	for fileIdx > 0 {
		fi := l.files[fileIdx].firstIndex()
		if fi <= idx {
			break
		}
		fileIdx--
	}
	offset, found := l.files[fileIdx].Offset(idx)
	if !found {
		return -1, -1
	}
	return fileIdx, offset
}

func (l *entryLog) deleteBefore(raftIndex uint64) error {
	fidx, off := l.Offset(raftIndex)
	if off >= 0 && fidx >= 0 && fidx < len(l.files) {
		before := l.files[:fidx]
		l.files = l.files[fidx:]
		for _, ef := range before {
			if err := ef.delete(); err != nil {
				glog.Errorf("while deleting file: %s, err: %v\n", ef.fd.Name(), err)
			}
		}
	}
	return nil
}

func (l *entryLog) reset() error {
	for _, ef := range l.files {
		if err := ef.delete(); err != nil {
			return errors.Wrapf(err, "while deleting %s", ef.fd.Name())
		}
	}
	l.files = l.files[:0]
	zeroOut(l.current.data, 0, entryFileOffset)
	l.nextEntryIdx = 0
	return nil
}

func (l *entryLog) allEntries(lo, hi, maxSize uint64) ([]raftpb.Entry, error) {
	if lo < l.FirstIndex() {
		return nil, raft.ErrCompacted
	}

	entries := make([]raftpb.Entry, 0)
	fileIdx, offset := l.Offset(lo)
	var size uint64

	var currFile *entryFile
	if fileIdx == -1 {
		currFile = l.current
	} else {
		currFile = l.files[fileIdx]
	}

	for {
		if offset >= maxNumEntries {
			if fileIdx == -1 {
				// We are looking at the current file and there are no more entries.
				// Return what we have.
				break
			}

			// Move to the next file.
			fileIdx++
			if fileIdx == len(l.files) {
				currFile = l.current
			} else {
				currFile = l.files[fileIdx]
			}

			// Reset the offset to start reading the next file from the beginning.
			offset = 0
		}

		re := currFile.GetRaftEntry(offset)
		if re.Index >= hi || re.Index == 0 {
			break
		}
		size += uint64(re.Size())
		if len(entries) > 0 && size > maxSize {
			break
		}
		entries = append(entries, re)
		offset++

	}
	return entries, nil
}

// Init initializes returns a properly initialized instance of DiskStorage.
// To gracefully shutdown DiskStorage, store.Closer.SignalAndWait() should be called.
func Init(dir string, id uint64, gid uint32) *DiskStorage {
	w := &DiskStorage{
		dir: dir,
		id:  id,
		gid: gid,
	}

	var err error
	w.meta, err = newMetaFile(dir)
	x.Check(err)

	w.entries, err = openEntryLog(dir)
	x.Check(err)

	if prev := w.meta.RaftId(); prev != id || prev == 0 {
		w.meta.StoreRaftId(id)
	}

	w.elog = trace.NewEventLog("Badger", "RaftStorage")

	snap, err := w.meta.Snapshot()
	x.Check(err)
	if !raft.IsEmptySnap(snap) {
		return w
	}

	first, err := w.FirstIndex()
	if err == errNotFound {
		ents := make([]raftpb.Entry, 1)
		x.Check(w.reset(ents))
	} else {
		x.Check(err)
	}

	// If db is not closed properly, there might be index ranges for which delete entries are not
	// inserted. So insert delete entries for those ranges starting from 0 to (first-1).
	if err := w.entries.deleteBefore(first - 1); err != nil {
		glog.Errorf("while deleting before: %d, err: %v\n", first-1, err)
	}
	return w
}

func (w *DiskStorage) Term(idx uint64) (uint64, error) {
	first, err := w.FirstIndex()
	x.Check(err)
	if idx < first-1 {
		return 0, raft.ErrCompacted
	}

	return w.entries.Term(idx)
}

var idKey = []byte("raftid")

// RaftId reads the given badger store and returns the stored RAFT ID.
func RaftId(db *badger.DB) (uint64, error) {
	var id uint64
	err := db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(idKey)
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			id = binary.BigEndian.Uint64(val)
			return nil
		})
	})
	if err == badger.ErrKeyNotFound {
		return 0, nil
	}
	return id, err
}

// EntryKey returns the key where the entry with the given ID is stored.
func (w *DiskStorage) EntryKey(idx uint64) []byte {
	b := make([]byte, 20)
	binary.BigEndian.PutUint64(b[0:8], w.id)
	binary.BigEndian.PutUint32(b[8:12], w.gid)
	binary.BigEndian.PutUint64(b[12:20], idx)
	return b
}

func (w *DiskStorage) parseIndex(key []byte) uint64 {
	x.AssertTrue(len(key) == 20)
	return binary.BigEndian.Uint64(key[12:20])
}

func (w *DiskStorage) entryPrefix() []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint64(b[0:8], w.id)
	binary.BigEndian.PutUint32(b[8:12], w.gid)
	return b
}

var errNotFound = errors.New("Unable to find raft entry")

func (w *DiskStorage) LastIndex() (uint64, error) {
	return w.entries.LastIndex(), nil
}

// FirstIndex returns the index of the first log entry that is
// possibly available via Entries (older entries have been incorporated
// into the latest Snapshot).
func (w *DiskStorage) FirstIndex() (uint64, error) {
	// We are deleting index ranges in background after taking snapshot, so we should check for last
	// snapshot in WAL(Badger) if it is not found in cache. If no snapshot is found, then we can
	// check firstKey.
	if snap, err := w.Snapshot(); err == nil && !raft.IsEmptySnap(snap) {
		return snap.Metadata.Index + 1, nil
	}
	return w.entries.FirstIndex(), nil

	// index := w.entries.FirstIndex()
	// glog.V(2).Infof("Setting first index: %d", index+1)
	// w.cache.Store(firstKey, index+1)
	// return index + 1, nil
}

// Snapshot returns the most recent snapshot.
// If snapshot is temporarily unavailable, it should return ErrSnapshotTemporarilyUnavailable,
// so raft state machine could know that Storage needs some time to prepare
// snapshot and call Snapshot later.
func (w *DiskStorage) Snapshot() (raftpb.Snapshot, error) {
	return w.meta.Snapshot()
}

// setSnapshot would store the snapshot. We can delete all the entries up until the snapshot
// index. But, keep the raft entry at the snapshot index, to make it easier to build the logic; like
// the dummy entry in MemoryStorage.
func (w *DiskStorage) setSnapshot(s *raftpb.Snapshot) error {
	if s == nil || raft.IsEmptySnap(*s) {
		return nil
	}

	if err := w.meta.StoreSnapshot(s); err != nil {
		return err
	}
	// TODO: Do we need to overwrite the entry?
	// e := raftpb.Entry{Term: s.Metadata.Term, Index: s.Metadata.Index}
	return nil
}

// reset resets the entries. Used for testing.
func (w *DiskStorage) reset(es []raftpb.Entry) error {
	// Clean out the state.
	if err := w.entries.reset(); err != nil {
		return err
	}
	return w.addEntries(es)
}

func (w *DiskStorage) HardState() (raftpb.HardState, error) {
	if w.meta == nil {
		return raftpb.HardState{}, errors.Errorf("uninitialized meta file")
	}
	return w.meta.HardState()
}

func (w *DiskStorage) Checkpoint() (uint64, error) {
	if w.meta == nil {
		return 0, errors.Errorf("uninitialized meta file")
	}
	return w.meta.Checkpoint(), nil
}

func (w *DiskStorage) UpdateCheckpoint(snap *pb.Snapshot) error {
	if w.meta == nil {
		return errors.Errorf("uninitialized meta file")
	}
	w.meta.UpdateCheckpoint(snap.Index)
	return nil
}

// InitialState returns the saved HardState and ConfState information.
func (w *DiskStorage) InitialState() (hs raftpb.HardState, cs raftpb.ConfState, err error) {
	w.elog.Printf("InitialState")
	defer w.elog.Printf("Done")
	hs, err = w.meta.HardState()
	if err != nil {
		return
	}
	var snap raftpb.Snapshot
	snap, err = w.Snapshot()
	if err != nil {
		return
	}
	return hs, snap.Metadata.ConfState, nil
}

func (w *DiskStorage) NumEntries() (int, error) {
	panic("Not implemented")
	// return w.entries.numEntries(), nil
}

// return low to high, excluding the high.
func (w *DiskStorage) allEntries(lo, hi, maxSize uint64) (es []raftpb.Entry, rerr error) {
	// fetch all the entry item from the entryLog

	ents, err := w.entries.allEntries(lo, hi, maxSize)
	if err != nil {
		return nil, err
	}

	return ents, nil
}

// Entries returns a slice of log entries in the range [lo,hi).
// MaxSize limits the total size of the log entries returned, but
// Entries returns at least one entry if any.
func (w *DiskStorage) Entries(lo, hi, maxSize uint64) (es []raftpb.Entry, rerr error) {
	w.elog.Printf("Entries: [%d, %d) maxSize:%d", lo, hi, maxSize)
	defer w.elog.Printf("Done")
	first := w.entries.FirstIndex()
	if lo < first {
		return nil, raft.ErrCompacted
	}

	last := w.entries.LastIndex()
	if hi > last+1 {
		return nil, raft.ErrUnavailable
	}

	return w.allEntries(lo, hi, maxSize)
}

// CreateSnapshot generates a snapshot with the given ConfState and data and writes it to disk.
func (w *DiskStorage) CreateSnapshot(i uint64, cs *raftpb.ConfState, data []byte) error {
	glog.V(2).Infof("CreateSnapshot i=%d, cs=%+v", i, cs)
	first, err := w.FirstIndex()
	if err != nil {
		return err
	}
	if i < first {
		glog.Errorf("i=%d<first=%d, ErrSnapOutOfDate", i, first)
		return raft.ErrSnapOutOfDate
	}

	e, err := w.entries.seekEntry(i)
	if err != nil {
		return err
	}

	var snap raftpb.Snapshot
	snap.Metadata.Index = i
	snap.Metadata.Term = e.Term()
	x.AssertTrue(cs != nil)
	snap.Metadata.ConfState = *cs
	snap.Data = data

	if err := w.setSnapshot(&snap); err != nil {
		return err
	}
	// Now we delete all the files which are below the snapshot index.
	return w.entries.deleteBefore(snap.Metadata.Index)
}

// Save would write Entries, HardState and Snapshot to persistent storage in order, i.e. Entries
// first, then HardState and Snapshot if they are not empty. If persistent storage supports atomic
// writes then all of them can be written together. Note that when writing an Entry with Index i,
// any previously-persisted entries with Index >= i must be discarded.
func (w *DiskStorage) Save(h *raftpb.HardState, es []raftpb.Entry, snap *raftpb.Snapshot) error {
	if err := w.entries.AddEntries(es); err != nil {
		return err
	}
	if err := w.meta.StoreHardState(h); err != nil {
		return err
	}
	if err := w.setSnapshot(snap); err != nil {
		return err
	}
	return nil
}

// Append the new entries to storage.
func (w *DiskStorage) addEntries(entries []raftpb.Entry) error {
	if len(entries) == 0 {
		return nil
	}

	first, err := w.FirstIndex()
	if err != nil {
		return err
	}
	firste := entries[0].Index
	if firste+uint64(len(entries))-1 < first {
		// All of these entries have already been compacted.
		return nil
	}
	if first > firste {
		// Truncate compacted entries
		entries = entries[first-firste:]
	}

	// AddEntries would zero out all the entries starting entries[0].Index before writing.
	if err := w.entries.AddEntries(entries); err != nil {
		return errors.Wrapf(err, "while adding entries")
	}
	return nil
}

// Sync calls the Sync method in the underlying badger instance to write all the contents to disk.
func (w *DiskStorage) Sync() error {
	if err := w.meta.sync(); err != nil {
		return errors.Wrapf(err, "while syncing meta")
	}
	if err := w.entries.current.sync(); err != nil {
		return errors.Wrapf(err, "while syncing current file")
	}
	return nil
}
