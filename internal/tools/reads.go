package tools

// Answering a read the turn has already made.
//
// A coding agent re-reads. Often it is a model checking whether it still remembers a file it
// read four rounds ago, and the honest answer to that is one sentence: nothing has changed
// since you read it, at the same range.
//
// But the other re-read is the one that must never be answered that way: a model that just
// wrote a file and is checking that the text it meant is the text that landed. There the
// question is not "is this what I know" but "is this what is in the file", and a shortcut
// would answer a question about the app's memory when the model asked about the disk.
//
// **So the cache is not trusted to notice a change; it is told.** Every tool that writes to
// the folder forgets what it touched — the changed path for a write or an edit, and the whole
// chat's memory for a command, because a command can rewrite anything it likes (`sed -i`,
// `gofmt -w`, a code generator). The size-and-mtime stamp below is only the optimistic case:
// it catches a file somebody changed outside this app. It is deliberately not the guarantee,
// because a filesystem with coarse timestamps — a FAT drive, some network mounts — reports
// the same second for an edit made inside it, and a same-length replacement is invisible to a
// size comparison. A verification read that returned "unchanged" there would be the app
// telling a model its edit had not happened.
//
// The memory is per turn and per chat, so the next turn starts clean, and the map is bounded
// because a turn that reads thousands of files must not grow it without limit.

import (
	"os"
	"strings"
)

// readKey names one read: the chat, the turn, and the file. The turn is part of the key
// rather than a field because two turns of the same chat never share an answer — the second
// one starts with no memory at all, and a key that carried it would keep the first turn's.
type readKey struct {
	chatID string
	turn   string
	path   string
}

// readStamp is what one read of one file, in one turn, looked like.
//
// The modification time is kept as nanoseconds since the epoch rather than as a time.Time:
// this value is compared with ==, and two times that name the same instant are not always ==
// (a monotonic reading, a different location). An int64 has no such subtlety.
type readStamp struct {
	offset int
	limit  int
	size   int64
	mod    int64
}

// maxReadStamps bounds the map. Past it the table is dropped rather than evicted one entry at
// a time: the only thing it saves is a repeated read, so losing it costs a re-read.
const maxReadStamps = 512

// readSeen reports whether this turn already read this exact range of an unchanged file.
func (s *Service) readSeen(key readKey, stamp readStamp) bool {
	s.readsMu.Lock()
	defer s.readsMu.Unlock()
	seen, ok := s.readsSeen[key]
	return ok && seen == stamp
}

// rememberRead records one successful read.
func (s *Service) rememberRead(key readKey, stamp readStamp) {
	s.readsMu.Lock()
	defer s.readsMu.Unlock()
	if s.readsSeen == nil || len(s.readsSeen) >= maxReadStamps {
		s.readsSeen = make(map[readKey]readStamp, maxReadStamps)
	}
	s.readsSeen[key] = stamp
}

// ForgetReads is forgetReads for the app, which reverts a file outside the tools that wrote
// it: the text on disk is no longer what this chat's last read saw.
func (s *Service) ForgetReads(chatID, abs string) { s.forgetReads(chatID, abs) }

// forgetReads drops what this chat remembers about one file, after the app changed it.
//
// Every turn of the chat, not only the current one: a write is a fact about the file, and a
// memory from an earlier turn that happens to name the same path is just as stale.
func (s *Service) forgetReads(chatID, abs string) {
	s.readsMu.Lock()
	defer s.readsMu.Unlock()
	for key := range s.readsSeen {
		if key.chatID == chatID && key.path == abs {
			delete(s.readsSeen, key)
		}
	}
}

// forgetAllReads drops everything this chat remembers, after a command ran.
//
// A command is the one tool whose effect cannot be enumerated: `gofmt -w .` rewrites every
// file under a folder, and the app has no idea which. Keeping any memory after one would be
// guessing that the line happened not to touch the file the model is about to re-read.
func (s *Service) forgetAllReads(chatID string) {
	s.readsMu.Lock()
	defer s.readsMu.Unlock()
	for key := range s.readsSeen {
		if key.chatID == chatID {
			delete(s.readsSeen, key)
		}
	}
}

// statStamp is the identity of a file as the optimistic half of the cache compares it.
func statStamp(info os.FileInfo, offset, limit int) readStamp {
	return readStamp{offset: offset, limit: limit, size: info.Size(), mod: info.ModTime().UnixNano()}
}

// readKeyFor builds the key a read of one file in one turn is remembered under.
//
// A turn outside one — a test, a tool called directly — has no turn id, and no memory either:
// "already read" is a claim about a turn, so without one there is nothing to claim.
func readKeyFor(chatID, turn, abs string) (readKey, bool) {
	if turn == "" || strings.TrimSpace(abs) == "" {
		return readKey{}, false
	}
	return readKey{chatID: chatID, turn: turn, path: abs}, true
}
