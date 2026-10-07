// fatmacho joins single-architecture macOS executables into one universal
// ("fat") file, like Apple's lipo -create, so it can run on Linux CI.
//
//	go run ./tools/fatmacho -o out/McMasterList arm64-binary amd64-binary
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
)

const (
	fatMagic = 0xcafebabe
	align    = 14 // 2^14 = 16 KiB, what lipo uses for arm64
)

func main() {
	out := flag.String("o", "", "output file")
	flag.Parse()
	if *out == "" || flag.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: fatmacho -o OUT BIN1 BIN2...")
		os.Exit(2)
	}
	type slice struct {
		cpu, sub uint32
		data     []byte
		offset   uint32
	}
	var slices []slice
	offset := uint32(1 << align) // header fits easily in the first block
	for _, path := range flag.Args() {
		b, err := os.ReadFile(path)
		if err != nil {
			fail(err)
		}
		if len(b) < 12 || binary.LittleEndian.Uint32(b) != 0xfeedfacf {
			fail(fmt.Errorf("%s is not a 64-bit Mach-O executable", path))
		}
		s := slice{cpu: binary.LittleEndian.Uint32(b[4:]), sub: binary.LittleEndian.Uint32(b[8:]), data: b, offset: offset}
		slices = append(slices, s)
		offset += uint32(len(b))
		offset = (offset + (1 << align) - 1) &^ ((1 << align) - 1)
	}

	buf := make([]byte, slices[len(slices)-1].offset+uint32(len(slices[len(slices)-1].data)))
	binary.BigEndian.PutUint32(buf[0:], fatMagic)
	binary.BigEndian.PutUint32(buf[4:], uint32(len(slices)))
	for i, s := range slices {
		h := buf[8+20*i:]
		binary.BigEndian.PutUint32(h[0:], s.cpu)
		binary.BigEndian.PutUint32(h[4:], s.sub)
		binary.BigEndian.PutUint32(h[8:], s.offset)
		binary.BigEndian.PutUint32(h[12:], uint32(len(s.data)))
		binary.BigEndian.PutUint32(h[16:], align)
		copy(buf[s.offset:], s.data)
	}
	if err := os.WriteFile(*out, buf, 0o755); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "fatmacho:", err)
	os.Exit(1)
}
