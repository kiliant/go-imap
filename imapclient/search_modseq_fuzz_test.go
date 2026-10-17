package imapclient

import (
	"strings"
	"testing"

	"github.com/kiliant/go-imap/internal/imapwire"
)

func FuzzReadSearchSortNumbers(f *testing.F) {
	for _, seed := range []string{
		"\r\n", " 1 3\r\n", " 4294967295 (MODSEQ 9223372036854775807)\r\n",
		" 1 (modseq 1)\r\n", " (MODSEQ 1)\r\n", " 0 (MODSEQ 1)\r\n",
		" 1 (MODSEQ 0)\r\n", " 1 (MODSEQ 9223372036854775808)\r\n",
		" 1 (MODSEQ 1) 2\r\n", " 1 (MODSEQ 1 2)\r\n", " 1 (MODSEQ 1\r\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, wire string) {
		numbers, modSeq, err := readSearchSortNumbers(imapwire.NewDecoder(strings.NewReader(wire), nil))
		if err != nil {
			return
		}
		if modSeq > MaxModSeq || (modSeq != 0 && len(numbers) == 0) {
			t.Fatalf("invalid aggregate: %d, numbers %v", modSeq, numbers)
		}
		for _, n := range numbers {
			if n == 0 {
				t.Fatal("accepted zero message number")
			}
		}
	})
}
