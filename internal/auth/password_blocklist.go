package auth

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"sort"
)

const (
	passwordBlocklistMagic      = "GOFERPW1"
	passwordBlocklistHeaderSize = len(passwordBlocklistMagic) + 4
	passwordBlocklistDigestSize = 16
)

//go:embed password_blocklist.bin
var embeddedPasswordBlocklist []byte

var passwordBlocklist = mustLoadPasswordBlocklist(embeddedPasswordBlocklist)

type passwordBlocklistData struct {
	entries []byte
	count   int
}

func mustLoadPasswordBlocklist(data []byte) passwordBlocklistData {
	if len(data) < passwordBlocklistHeaderSize || string(data[:len(passwordBlocklistMagic)]) != passwordBlocklistMagic {
		panic("auth: embedded password blocklist has an invalid header")
	}
	count := int(binary.BigEndian.Uint32(data[len(passwordBlocklistMagic):passwordBlocklistHeaderSize]))
	entries := data[passwordBlocklistHeaderSize:]
	if count <= 0 || len(entries) != count*passwordBlocklistDigestSize {
		panic("auth: embedded password blocklist has an invalid size")
	}
	for index := 1; index < count; index++ {
		previous := entries[(index-1)*passwordBlocklistDigestSize : index*passwordBlocklistDigestSize]
		current := entries[index*passwordBlocklistDigestSize : (index+1)*passwordBlocklistDigestSize]
		if bytes.Compare(previous, current) >= 0 {
			panic("auth: embedded password blocklist is not strictly sorted")
		}
	}
	return passwordBlocklistData{entries: entries, count: count}
}

func commonPasswordBlocklistContains(password string) bool {
	canonical := canonicalPasswordBlocklistValue(password)
	digest := sha256.Sum256([]byte(canonical))
	target := digest[:passwordBlocklistDigestSize]
	index := sort.Search(passwordBlocklist.count, func(index int) bool {
		entry := passwordBlocklist.entries[index*passwordBlocklistDigestSize : (index+1)*passwordBlocklistDigestSize]
		return bytes.Compare(entry, target) >= 0
	})
	if index == passwordBlocklist.count {
		return false
	}
	entry := passwordBlocklist.entries[index*passwordBlocklistDigestSize : (index+1)*passwordBlocklistDigestSize]
	return bytes.Equal(entry, target)
}
