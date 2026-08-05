//go:build ignore

package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	blocklistMagic      = "GOFERPW1"
	blocklistDigestSize = 16
)

func main() {
	inputPath := flag.String("input", "", "UTF-8 newline-delimited password source")
	outputPath := flag.String("output", "password_blocklist.bin", "generated blocklist path")
	flag.Parse()
	if *inputPath == "" {
		fatalf("-input is required")
	}

	input, err := os.Open(*inputPath)
	if err != nil {
		fatalf("open input: %v", err)
	}
	defer input.Close()

	digests := make([][blocklistDigestSize]byte, 0, 100000)
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		value := strings.TrimSuffix(scanner.Text(), "\r")
		if value == "" || !utf8.ValidString(value) {
			continue
		}
		canonical := cases.Fold().String(norm.NFC.String(value))
		sum := sha256.Sum256([]byte(canonical))
		var digest [blocklistDigestSize]byte
		copy(digest[:], sum[:blocklistDigestSize])
		digests = append(digests, digest)
	}
	if err := scanner.Err(); err != nil {
		fatalf("scan input: %v", err)
	}

	sort.Slice(digests, func(left, right int) bool {
		return bytes.Compare(digests[left][:], digests[right][:]) < 0
	})
	unique := digests[:0]
	for _, digest := range digests {
		if len(unique) == 0 || !bytes.Equal(unique[len(unique)-1][:], digest[:]) {
			unique = append(unique, digest)
		}
	}

	var output bytes.Buffer
	output.WriteString(blocklistMagic)
	if err := binary.Write(&output, binary.BigEndian, uint32(len(unique))); err != nil {
		fatalf("write count: %v", err)
	}
	for _, digest := range unique {
		output.Write(digest[:])
	}
	temporaryPath := *outputPath + ".tmp"
	if err := os.WriteFile(temporaryPath, output.Bytes(), 0o644); err != nil {
		fatalf("write output: %v", err)
	}
	if err := os.Rename(temporaryPath, *outputPath); err != nil {
		fatalf("replace output: %v", err)
	}
	fmt.Printf("wrote %d unique password digests to %s\n", len(unique), *outputPath)
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
